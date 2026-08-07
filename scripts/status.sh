#!/usr/bin/env bash
# Shows the lab's state at a glance.
set -euo pipefail

echo "=== Databases ==="
kubectl get perconapgcluster,perconaxtradbcluster,perconaservermongodb -n default 2>/dev/null || true
kubectl get kafka -n default 2>/dev/null || true
kubectl get statefulset valkey -n default 2>/dev/null || true
echo
echo "=== Applications ==="
kubectl get deployments -n default -l part-of=rca-lab 2>/dev/null || true
echo
echo "=== Failure scenarios ==="
kubectl get failurescenarios -n default 2>/dev/null || echo "(scenario operator not installed)"
echo
echo "=== Not-ready pods (default) ==="
kubectl get pods -n default --field-selector=status.phase!=Running,status.phase!=Succeeded 2>/dev/null | tail -n +1 || true
