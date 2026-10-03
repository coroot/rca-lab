#!/usr/bin/env bash
# Tears the lab down. Order matters: scenario CRs first (the operator reverts
# any active failure), then apps, then database CRs while their operators are
# still running (finalizers must execute), then the operators themselves.
#
# Variables:
#   KEEP_DATA=1   keep PVCs (redeploying later reuses the data)
#   YES=1         skip the confirmation prompt
#   EXTERNAL_DBS  databases that were provided externally and must be left
#                 alone (default: what deploy.sh recorded in ConfigMap rca-lab)
set -euo pipefail
cd "$(dirname "$0")/.."
. scripts/lib.sh

KEEP_DATA="${KEEP_DATA:-}"
YES="${YES:-}"

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

ctx="$(kubectl config current-context 2>/dev/null)" || die "no current kube context"
if [ -z "$YES" ]; then
    read -r -p "Remove rca-lab from context '$ctx'? [y/N] " ans
    [ "$ans" = y ] || [ "$ans" = Y ] || die "aborted"
fi

[ -n "$EXTERNAL_DBS" ] || EXTERNAL_DBS="$(recorded_external_dbs)"
if is_external mysql; then
    info "MySQL is external (EXTERNAL_DBS=mysql): leaving its cluster, volumes and operator untouched"
fi

if kubectl get crd maintenancejobs.maintenance.platform.dev >/dev/null 2>&1; then
    info "Deleting failure scenarios (waits for reverts)"
    kubectl delete maintenancejobs --all -n default --timeout=10m || true
fi

if [ -f deploy/rca-operator/kustomization.yaml ]; then
    info "Removing scenario operator"
    kubectl delete -k deploy/rca-operator --ignore-not-found || true
fi

if [ -f deploy/apps/kustomization.yaml ]; then
    info "Removing applications"
    kubectl delete -k deploy/apps --ignore-not-found || true
fi
kubectl delete job data-seeder mysql-init -n default --ignore-not-found

info "Removing otel-collector"
kubectl delete -k deploy/otel --ignore-not-found || true
kubectl delete configmap otel-collector-config -n default --ignore-not-found

info "Deleting database and Kafka clusters (finalizers run while operators are still installed)"
kubectl kustomize deploy/overlays/default | drop_external_clusters \
    | kubectl delete -f - --ignore-not-found --timeout=15m || true
clusters="perconapgcluster/pg perconaservermongodb/mongodb kafka/kafka"
is_external mysql || clusters="$clusters perconaxtradbcluster/mysql"
# shellcheck disable=SC2086
kubectl wait --for=delete $clusters -n default --timeout=15m 2>/dev/null || true

# Sweep leftover Job pods the operators don't always GC (e.g. completed
# pgBackRest backup pods, mysql-init/seed jobs).
info "Removing leftover job pods"
kubectl delete pods -n default --field-selector=status.phase=Succeeded --ignore-not-found >/dev/null 2>&1 || true

if [ -z "$KEEP_DATA" ]; then
    info "Deleting PVCs"
    kubectl delete pvc -n default -l postgres-operator.crunchydata.com/cluster=pg --ignore-not-found
    is_external mysql || kubectl delete pvc -n default -l app.kubernetes.io/instance=mysql --ignore-not-found
    kubectl delete pvc -n default -l app.kubernetes.io/instance=mongodb --ignore-not-found
    kubectl delete pvc -n default -l strimzi.io/cluster=kafka --ignore-not-found
    kubectl delete pvc -n default -l app.kubernetes.io/managed-by=valkey-operator --ignore-not-found
fi

info "Uninstalling operators"
helm uninstall pg-operator -n pg-operator 2>/dev/null || true
is_external mysql || helm uninstall pxc-operator -n pxc-operator 2>/dev/null || true
helm uninstall psmdb-operator -n psmdb-operator 2>/dev/null || true
helm uninstall strimzi -n strimzi 2>/dev/null || true
helm uninstall valkey-operator -n valkey-operator 2>/dev/null || true
helm uninstall chaos-mesh -n chaos-mesh 2>/dev/null || true
kubectl delete -f deploy/namespaces.yaml --ignore-not-found
kubectl delete configmap rca-lab -n default --ignore-not-found

info "Done"
