#!/usr/bin/env bash
# Compact, at-a-glance view of the lab.
set -euo pipefail

bold=$'\033[1m'; dim=$'\033[2m'; grn=$'\033[32m'; ylw=$'\033[33m'; red=$'\033[31m'; rst=$'\033[0m'

# ok <value> <good-regex> — colorize a state cell.
colorize() {
    case "$1" in
        ready|Ready|True|Complete) printf '%s%s%s' "$grn" "$1" "$rst" ;;
        ""|"<none>"|null)          printf '%s%s%s' "$dim" "n/a" "$rst" ;;
        *)                         printf '%s%s%s' "$ylw" "$1" "$rst" ;;
    esac
}

row() { printf '  %-16s %s\n' "$1" "$(colorize "$2")"; }

get() { kubectl get "$1" "$2" -n default -o jsonpath="$3" 2>/dev/null || true; }

printf '%sDATABASES%s\n' "$bold" "$rst"
row postgres "$(get perconapgcluster pg '{.status.state}')"
row mysql    "$(get perconaxtradbcluster mysql '{.status.state}')"
row mongodb  "$(get perconaservermongodb mongodb '{.status.state}')"
row valkey   "$(get valkeycluster valkey '{.status.state}')"
row kafka    "$(get kafka kafka '{.status.conditions[?(@.type=="Ready")].status}')"

printf '\n%sAPPLICATIONS%s\n' "$bold" "$rst"
kubectl get deploy -n default -l part-of=rca-lab \
    -o custom-columns='NAME:.metadata.name,READY:.status.readyReplicas,WANT:.spec.replicas' \
    --no-headers 2>/dev/null | awk '{r=($2==$3 && $2!="<none>")?"":"  <"; printf "  %-24s %s/%s%s\n",$1,($2=="<none>"?0:$2),$3,r}'

printf '\n%sSCENARIOS%s\n' "$bold" "$rst"
kubectl get failurescenarios -n default \
    -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,ENABLED:.spec.enabled' \
    --no-headers 2>/dev/null | awk '{printf "  %-32s %-12s %s\n",$1,$2,$3}' \
    || echo "  (scenario operator not installed)"

printf '\n%sUNHEALTHY PODS%s\n' "$bold" "$rst"
unhealthy=$(kubectl get pods -n default --no-headers 2>/dev/null \
    | awk '$3!="Running" && $3!="Completed" {print "  "$1"  "$3"  (restarts: "$4")"}')
if [ -n "$unhealthy" ]; then
    printf '%s%s%s\n' "$red" "$unhealthy" "$rst"
else
    printf '  %snone%s\n' "$grn" "$rst"
fi
