#!/usr/bin/env bash
# Tears the lab down. Order matters: scenario CRs first (the operator reverts
# any active failure), then apps, then database CRs while their operators are
# still running (finalizers must execute), then the operators themselves.
#
# Variables:
#   KEEP_DATA=1   keep PVCs (redeploying later reuses the data)
#   YES=1         skip the confirmation prompt
set -euo pipefail
cd "$(dirname "$0")/.."

KEEP_DATA="${KEEP_DATA:-}"
YES="${YES:-}"

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

ctx="$(kubectl config current-context 2>/dev/null)" || die "no current kube context"
if [ -z "$YES" ]; then
    read -r -p "Remove rca-lab from context '$ctx'? [y/N] " ans
    [ "$ans" = y ] || [ "$ans" = Y ] || die "aborted"
fi

if kubectl get crd failurescenarios.rcalab.dev >/dev/null 2>&1; then
    info "Deleting failure scenarios (waits for reverts)"
    kubectl delete failurescenarios --all -n default --timeout=10m || true
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
kubectl delete -k deploy/overlays/default --ignore-not-found --timeout=15m || true
kubectl wait --for=delete perconapgcluster/pg perconaxtradbcluster/mysql perconaservermongodb/mongodb kafka/kafka -n default --timeout=15m 2>/dev/null || true

if [ -z "$KEEP_DATA" ]; then
    info "Deleting PVCs"
    kubectl delete pvc -n default -l app=valkey --ignore-not-found
    kubectl delete pvc -n default -l postgres-operator.crunchydata.com/cluster=pg --ignore-not-found
    kubectl delete pvc -n default -l app.kubernetes.io/instance=mysql --ignore-not-found
    kubectl delete pvc -n default -l app.kubernetes.io/instance=mongodb --ignore-not-found
    kubectl delete pvc -n default -l strimzi.io/cluster=kafka --ignore-not-found
fi

info "Uninstalling operators"
helm uninstall pg-operator -n pg-operator 2>/dev/null || true
helm uninstall pxc-operator -n pxc-operator 2>/dev/null || true
helm uninstall psmdb-operator -n psmdb-operator 2>/dev/null || true
helm uninstall strimzi -n strimzi 2>/dev/null || true
kubectl delete -f deploy/namespaces.yaml --ignore-not-found

info "Done"
