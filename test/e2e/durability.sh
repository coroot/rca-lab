#!/usr/bin/env bash
# End-to-end durability drills for the FailureScenario operator.
#
# Proves the operator's crash-safety guarantees against a live cluster with the
# lab deployed (make deploy). Each drill deliberately interrupts the operator or
# the resource lifecycle and asserts that normal state is still restored.
#
#   1. Delete a CR mid-run          -> the finalizer reverts before the CR goes.
#   2. Kill the operator mid-run    -> it resumes from status and still reverts.
#   3. Orphaned labeled workload    -> the startup sweeper cleans it up.
#
# Usage: test/e2e/durability.sh   (uses the current kube context)
set -euo pipefail
cd "$(dirname "$0")/../.."

ns=default
pass=0; fail=0
ok()   { printf '\033[32m  PASS\033[0m %s\n' "$*"; pass=$((pass+1)); }
bad()  { printf '\033[31m  FAIL\033[0m %s\n' "$*"; fail=$((fail+1)); }
info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

# wait_for <timeout-s> <shell-expr> — re-eval the expression each poll until true.
# (The expression is a string so command substitutions inside it run every
# iteration, not once at call time.)
wait_for() { local t=$1 expr=$2 n=0; until eval "$expr"; do n=$((n+2)); [ "$n" -ge "$t" ] && return 1; sleep 2; done; }

phase()   { kubectl get failurescenario "$1" -n $ns -o jsonpath='{.status.phase}' 2>/dev/null; }
replicas(){ kubectl get deploy "$1" -n $ns -o jsonpath='{.spec.replicas}' 2>/dev/null; }
jobcount(){ kubectl get job "$1" -n $ns --no-headers 2>/dev/null | wc -l | tr -d ' '; }

# ---------------------------------------------------------------------------
info "DRILL 1 — delete a CR mid-run: the finalizer must revert first"
kubectl patch failurescenario pg-analytics-queries -n $ns --type=merge -p '{"spec":{"enabled":true}}' >/dev/null
if wait_for 90 '[ "$(phase pg-analytics-queries)" = Active ] && [ "$(jobcount analytics-reporting)" = 1 ]'; then
    kubectl delete failurescenario pg-analytics-queries -n $ns --timeout=120s >/dev/null
    if [ "$(jobcount analytics-reporting)" = 0 ] && [ -z "$(kubectl get failurescenario pg-analytics-queries -n $ns --no-headers 2>/dev/null)" ]; then
        ok "CR deleted cleanly and the workload was reverted by the finalizer"
    else
        bad "workload survived the CR deletion or the CR is stuck terminating"
    fi
    kubectl apply -k scenarios >/dev/null   # restore the CR for later runs
else
    bad "scenario never reached Active"
fi

# ---------------------------------------------------------------------------
info "DRILL 2 — kill the operator mid-run, request stop while it's down"
kubectl patch failurescenario traffic-spike -n $ns --type=merge -p '{"spec":{"enabled":true}}' >/dev/null
if wait_for 60 '[ "$(replicas load-generator)" = 5 ]'; then
    kubectl delete pod -n $ns -l app=rca-lab-operator --wait=false >/dev/null
    kubectl patch failurescenario traffic-spike -n $ns --type=merge -p '{"spec":{"enabled":false}}' >/dev/null
    if wait_for 120 '[ "$(replicas load-generator)" = 1 ] && [ "$(phase traffic-spike)" = Idle ]'; then
        ok "operator restarted, resumed from status, and reverted (load-generator -> 1)"
    else
        bad "load-generator was not scaled back after the operator restart"
    fi
else
    bad "traffic-spike never scaled load-generator to 5"
fi

# ---------------------------------------------------------------------------
info "DRILL 3 — startup sweeper cleans an orphaned labeled workload"
kubectl apply -f - >/dev/null <<'EOF'
apiVersion: batch/v1
kind: Job
metadata:
  name: ghost-workload
  namespace: default
  labels: { rcalab.dev/scenario: ghost-scenario, app: ghost-workload }
spec:
  ttlSecondsAfterFinished: 3600
  template:
    spec:
      restartPolicy: Never
      terminationGracePeriodSeconds: 2   # drain fast once the sweeper deletes it
      containers:
        - { name: ghost, image: busybox:1.36, command: ["sh","-c","sleep 3600"] }
EOF
kubectl rollout restart deployment/rca-lab-operator -n $ns >/dev/null
kubectl rollout status deployment/rca-lab-operator -n $ns --timeout=120s >/dev/null
# Generous window: after restart the sweeper runs once cache-synced + leader-elected.
if wait_for 180 '[ "$(jobcount ghost-workload)" = 0 ]'; then
    ok "sweeper deleted the orphan (owning scenario does not exist)"
else
    bad "orphaned workload survived the operator restart"
    kubectl delete job ghost-workload -n $ns --ignore-not-found >/dev/null
fi

# ---------------------------------------------------------------------------
printf '\n\033[1m%d passed, %d failed\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
