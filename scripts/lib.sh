# Shared by deploy.sh / clean.sh / status.sh (sourced, not executed).

# EXTERNAL_DBS: comma-separated databases the lab must not install because
# they are provided from outside (see docs/external-databases.md). Supported:
# mysql. deploy.sh records the value in ConfigMap rca-lab (namespace default)
# so `make clean` and `make status` pick it up without being told again.
EXTERNAL_DBS="${EXTERNAL_DBS:-}"
SUPPORTED_EXTERNAL_DBS="mysql"

recorded_external_dbs() {
    kubectl get configmap rca-lab -n default -o jsonpath='{.data.external-dbs}' 2>/dev/null || true
}

is_external() {
    case ",$EXTERNAL_DBS," in
        *,"$1",*) return 0 ;;
    esac
    return 1
}

# Drops every top-level document of the given kind from a multi-document YAML
# stream (kubectl kustomize output). Only column-0 `kind:` lines count, so
# nested kinds (patch targets, ownerReferences) never match.
drop_kind() {
    awk -v kind="$1" '
        function flush() { if (buf != "" && !skip) printf "%s", buf; buf = ""; skip = 0 }
        /^---$/ { flush(); print; next }
        $0 == "kind: " kind { skip = 1 }
        { buf = buf $0 "\n" }
        END { flush() }'
}

# Removes the cluster CRs of external databases from a rendered manifest
# stream, so the lab neither creates nor deletes them.
drop_external_clusters() {
    if is_external mysql; then
        drop_kind PerconaXtraDBCluster
    else
        cat
    fi
}
