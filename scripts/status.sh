#!/usr/bin/env bash
# Compact, at-a-glance view of the lab.
set -euo pipefail

bold=$'\033[1m'; dim=$'\033[2m'; grn=$'\033[32m'; ylw=$'\033[33m'; red=$'\033[31m'; rst=$'\033[0m'

get() { kubectl get "$1" "$2" -n default -o jsonpath="$3" 2>/dev/null || true; }

# Normalize the various "healthy" values (ready / Ready / True) to one word and
# color it; anything else is shown as-is in yellow.
db_row() {
    local name=$1 raw=$2 out color
    case "$raw" in
        ready|Ready|True) out=ready; color=$grn ;;
        "")               out="not deployed"; color=$dim ;;
        *)                out="$raw"; color=$ylw ;;
    esac
    printf '  %-10s %s%s%s\n' "$name" "$color" "$out" "$rst"
}

printf '%sDATABASES%s\n' "$bold" "$rst"
db_row postgres "$(get perconapgcluster pg '{.status.state}')"
db_row mysql    "$(get perconaxtradbcluster mysql '{.status.state}')"
db_row mongodb  "$(get perconaservermongodb mongodb '{.status.state}')"
db_row valkey   "$(get valkeycluster valkey '{.status.state}')"
db_row kafka    "$(get kafka kafka '{.status.conditions[?(@.type=="Ready")].status}')"

printf '\n%sAPPLICATIONS%s\n' "$bold" "$rst"
kubectl get deploy -n default -l part-of=rca-lab \
    -o custom-columns='NAME:.metadata.name,READY:.status.readyReplicas,WANT:.spec.replicas' \
    --no-headers 2>/dev/null | awk -v g="$grn" -v y="$ylw" -v r="$rst" '
    {ready=($2=="<none>"?0:$2); ok=(ready==$3); c=(ok?g:y);
     printf "  %-24s %s%s/%s%s\n",$1,c,ready,$3,r}'

printf '\n%sSCENARIOS%s %s(idle = normal; enable with: kubectl patch failurescenario <name> --type=merge -p '"'"'{\"spec\":{\"enabled\":true}}'"'"')%s\n' "$bold" "$rst" "$dim" "$rst"
scen=$(kubectl get failurescenarios -n default \
    -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,CAT:.spec.category' \
    --no-headers 2>/dev/null || true)
if [ -n "$scen" ]; then
    echo "$scen" | awk -v g="$grn" -v y="$ylw" -v d="$dim" -v r="$rst" '
        {phase=($2=="<none>"?"Idle":$2);
         if (phase=="Idle") {label="idle"; c=d} else {label=toupper(phase); c=y}
         printf "  %-32s %s%-10s%s %s%s%s\n",$1,c,label,r,d,$3,r}'
else
    printf '  %s(scenario operator not installed)%s\n' "$dim" "$rst"
fi

printf '\n%sUNHEALTHY PODS%s\n' "$bold" "$rst"
unhealthy=$(kubectl get pods -n default --no-headers 2>/dev/null \
    | awk '$3!="Running" && $3!="Completed" {print "  "$1"  "$3"  (restarts: "$4")"}')
if [ -n "$unhealthy" ]; then
    printf '%s%s%s\n' "$red" "$unhealthy" "$rst"
else
    printf '  %snone%s\n' "$grn" "$rst"
fi
