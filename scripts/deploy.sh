#!/usr/bin/env bash
# Deploys / converges the whole lab. Idempotent: re-run to apply changes.
#
# Variables (env or `make deploy VAR=...`):
#   STORAGE_CLASS   explicit storage class for all PVCs (default: cluster default)
#   SINGLE_NODE=1   single-node sizing (kind/k3d/minikube)
#   SEED_SIZE_GB    total seeded data size, 0 to skip (default: 2)
#   OTLP_ENDPOINT   forward telemetry via OTLP, e.g. coroot.example:4317
#                   (default: telemetry is received and discarded)
#   OTLP_INSECURE   set to 0 if the OTLP endpoint uses TLS (default: 1, plaintext)
#   OTLP_HEADERS    optional exporter headers, e.g. "x-api-key=abc"
#   YES=1           skip the kube-context confirmation prompt
set -euo pipefail
cd "$(dirname "$0")/.."

STORAGE_CLASS="${STORAGE_CLASS:-}"
SINGLE_NODE="${SINGLE_NODE:-}"
SEED_SIZE_GB="${SEED_SIZE_GB:-2}"
OTLP_ENDPOINT="${OTLP_ENDPOINT:-}"
OTLP_INSECURE="${OTLP_INSECURE:-1}"
OTLP_HEADERS="${OTLP_HEADERS:-}"
YES="${YES:-}"

PG_OPERATOR_CHART=3.0.0
PXC_OPERATOR_CHART=1.20.0
PSMDB_OPERATOR_CHART=1.23.0
STRIMZI_CHART=1.1.0
VALKEY_OPERATOR_CHART=0.4.1

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARNING:\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

preflight() {
    command -v kubectl >/dev/null || die "kubectl not found"
    command -v helm >/dev/null || die "helm not found"
    local ctx
    ctx="$(kubectl config current-context 2>/dev/null)" || die "no current kube context"
    if [ -z "$YES" ]; then
        read -r -p "Deploy rca-lab to context '$ctx'? [y/N] " ans
        [ "$ans" = y ] || [ "$ans" = Y ] || die "aborted"
    fi
    if [ -z "$STORAGE_CLASS" ] && ! kubectl get storageclass -o jsonpath='{.items[?(@.metadata.annotations.storageclass\.kubernetes\.io/is-default-class=="true")].metadata.name}' | grep -q .; then
        die "no default StorageClass in this cluster; set STORAGE_CLASS=<name>"
    fi
    local nodes
    nodes="$(kubectl get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    if [ "$nodes" = 1 ] && [ -z "$SINGLE_NODE" ]; then
        warn "cluster has a single node; HA database sizes will not schedule. Consider: make deploy SINGLE_NODE=1"
    fi
}

# install_operator installs a helm release if it is not already deployed.
# Operators are installed once, not upgraded on every run: their controllers
# take ownership of fields in their own Services (e.g. .spec.selector), which
# makes a plain `helm upgrade` fail with server-side-apply conflicts. A failed
# or pending release is torn down and reinstalled. To change an operator
# version, run `make clean` first (or upgrade that release manually).
install_operator() {
    local name=$1 chart=$2 ns=$3 version=$4 values=$5
    local status
    # helm status exits non-zero when the release does not exist; its JSON
    # .info.status is the release state. Works on both Helm v3 and v4.
    status="$(helm status "$name" -n "$ns" -o json 2>/dev/null | grep -o '"status":"[a-z-]*"' | head -1 | cut -d'"' -f4)"
    if [ "$status" = "deployed" ]; then
        info "operator $name already deployed (skipping; run 'make clean' to change versions)"
        return
    fi
    if [ -n "$status" ]; then
        warn "operator $name in state '$status'; reinstalling"
        helm uninstall "$name" -n "$ns" >/dev/null 2>&1 || true
    fi
    helm install "$name" "$chart" -n "$ns" --version "$version" -f "$values" --wait
}

install_operators() {
    info "Installing operators (helm)"
    kubectl apply -f deploy/namespaces.yaml
    helm repo add percona https://percona.github.io/percona-helm-charts/ >/dev/null
    helm repo add strimzi https://strimzi.io/charts/ >/dev/null
    helm repo add valkey https://valkey.io/valkey-helm >/dev/null
    helm repo update percona strimzi valkey >/dev/null
    install_operator pg-operator percona/pg-operator pg-operator "$PG_OPERATOR_CHART" deploy/operators/pg-operator.values.yaml
    install_operator pxc-operator percona/pxc-operator pxc-operator "$PXC_OPERATOR_CHART" deploy/operators/pxc-operator.values.yaml
    install_operator psmdb-operator percona/psmdb-operator psmdb-operator "$PSMDB_OPERATOR_CHART" deploy/operators/psmdb-operator.values.yaml
    install_operator strimzi strimzi/strimzi-kafka-operator strimzi "$STRIMZI_CHART" deploy/operators/strimzi.values.yaml
    install_operator valkey-operator valkey/valkey-operator valkey-operator "$VALKEY_OPERATOR_CHART" deploy/operators/valkey-operator.values.yaml
}

apply_databases() {
    info "Applying database and Kafka resources"
    local base=deploy/overlays/default
    [ -n "$SINGLE_NODE" ] && base=deploy/overlays/single-node
    if [ -n "$STORAGE_CLASS" ]; then
        local tmp
        tmp="$(mktemp -d)"
        trap 'rm -rf "$tmp"' RETURN
        cat > "$tmp/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - $(pwd)/$base
patches:
  - target:
      kind: PerconaPGCluster
      name: pg
    patch: |-
      - op: add
        path: /spec/instances/0/dataVolumeClaimSpec/storageClassName
        value: $STORAGE_CLASS
  - target:
      kind: PerconaXtraDBCluster
      name: mysql
    patch: |-
      - op: add
        path: /spec/pxc/volumeSpec/persistentVolumeClaim/storageClassName
        value: $STORAGE_CLASS
  - target:
      kind: PerconaServerMongoDB
      name: mongodb
    patch: |-
      - op: add
        path: /spec/replsets/0/volumeSpec/persistentVolumeClaim/storageClassName
        value: $STORAGE_CLASS
  - target:
      kind: KafkaNodePool
      name: dual
    patch: |-
      - op: add
        path: /spec/storage/volumes/0/class
        value: $STORAGE_CLASS
  - target:
      kind: ValkeyCluster
      name: valkey
    patch: |-
      - op: add
        path: /spec/persistence/storageClassName
        value: $STORAGE_CLASS
EOF
        kubectl apply -k "$tmp"
    else
        kubectl apply -k "$base"
    fi
}

wait_databases() {
    info "Waiting for databases (first deploy can take ~10 minutes: image pulls + cluster bootstrap)"
    kubectl wait --for=jsonpath='{.status.state}'=ready perconapgcluster/pg -n default --timeout=15m &
    kubectl wait --for=jsonpath='{.status.state}'=ready perconaxtradbcluster/mysql -n default --timeout=15m &
    kubectl wait --for=jsonpath='{.status.state}'=ready perconaservermongodb/mongodb -n default --timeout=15m &
    kubectl wait --for=condition=Ready kafka/kafka -n default --timeout=15m &
    kubectl wait --for=jsonpath='{.status.state}'=Ready valkeycluster/valkey -n default --timeout=15m &
    local pid rc=0
    for pid in $(jobs -p); do
        wait "$pid" || rc=1
    done
    [ "$rc" = 0 ] || die "a database/kafka cluster did not become ready; check 'make status'"
}

mysql_init() {
    info "Creating MySQL databases (orders, payments)"
    kubectl delete job mysql-init -n default --ignore-not-found >/dev/null
    kubectl apply -f deploy/databases/jobs/mysql-init.yaml
    kubectl wait --for=condition=complete job/mysql-init -n default --timeout=5m
}

apply_otel() {
    info "Configuring otel-collector (destination: ${OTLP_ENDPOINT:-discard})"
    local exporters exporter_name
    if [ -n "$OTLP_ENDPOINT" ]; then
        exporter_name=otlp
        exporters="      otlp:
        endpoint: ${OTLP_ENDPOINT}"
        if [ "$OTLP_INSECURE" = 1 ]; then
            exporters="$exporters
        tls:
          insecure: true"
        fi
        if [ -n "$OTLP_HEADERS" ]; then
            local h key val
            exporters="$exporters
        headers:"
            IFS=',' read -ra hs <<< "$OTLP_HEADERS"
            for h in "${hs[@]}"; do
                key="${h%%=*}"; val="${h#*=}"
                exporters="$exporters
          ${key}: \"${val}\""
            done
        fi
    else
        exporter_name=nop
        exporters="      nop: {}"
    fi
    awk -v exporters="$exporters" -v name="$exporter_name" \
        '{gsub(/__EXPORTER_NAME__/, name); if ($0 ~ /^__EXPORTERS__$/) print exporters; else print}' \
        deploy/otel/collector-config.tmpl.yaml | kubectl apply -f -
    kubectl apply -k deploy/otel
    kubectl rollout restart deployment/otel-collector -n default >/dev/null
    kubectl rollout status deployment/otel-collector -n default --timeout=2m
}

apply_apps() {
    if [ ! -f deploy/apps/kustomization.yaml ]; then
        warn "deploy/apps not present yet; skipping application deploy"
        return
    fi
    info "Deploying applications"
    kubectl apply -k deploy/apps
    kubectl wait --for=condition=Available deployment -l part-of=rca-lab -n default --timeout=10m
}

apply_scenarios() {
    if [ ! -f deploy/rca-operator/kustomization.yaml ]; then
        return
    fi
    info "Deploying scenario operator + library"
    kubectl apply -k deploy/rca-operator
    kubectl rollout status deployment/rca-lab-operator -n default --timeout=5m
    [ -f scenarios/kustomization.yaml ] && kubectl apply -k scenarios
}

seed() {
    if [ "$SEED_SIZE_GB" = 0 ]; then
        info "Skipping data seeding (SEED_SIZE_GB=0)"
        return
    fi
    if [ ! -f deploy/seed/seed-job.yaml ]; then
        warn "deploy/seed not present yet; skipping seeding"
        return
    fi
    info "Seeding data (${SEED_SIZE_GB} GB total; idempotent)"
    kubectl delete job data-seeder -n default --ignore-not-found >/dev/null
    sed "s/__SEED_SIZE_GB__/$SEED_SIZE_GB/" deploy/seed/seed-job.yaml | kubectl apply -f -
    kubectl wait --for=condition=complete job/data-seeder -n default --timeout=30m
}

smoke_test() {
    [ -f deploy/apps/kustomization.yaml ] || return 0
    info "Smoke test"
    kubectl run rca-lab-smoke --rm -i --restart=Never --image=curlimages/curl:8.13.0 -n default \
        --command -- curl -fsS -m 10 http://api-gateway:8080/health >/dev/null \
        && info "api-gateway /health OK" \
        || die "smoke test failed: api-gateway /health"
}

summary() {
    cat <<EOF

rca-lab is up.

  Traffic:    load-generator -> api-gateway -> services (continuous)
  Telemetry:  all services -> otel-collector (${OTLP_ENDPOINT:-discarded; set OTLP_ENDPOINT=host:4317 to forward})
  Scenarios:  kubectl get failurescenarios   (web UI: kubectl port-forward svc/rca-lab-operator 8080)
  Status:     make status
EOF
}

preflight
install_operators
apply_databases
wait_databases
mysql_init
apply_otel
apply_apps
apply_scenarios
seed
smoke_test
summary
