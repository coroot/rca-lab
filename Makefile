# rca-lab — failure reproduction lab for RCA tooling.
#
# Usage:
#   make deploy                          deploy/converge everything (idempotent)
#   make deploy SINGLE_NODE=1            single-node sizing (kind/k3d/minikube)
#   make deploy STORAGE_CLASS=fast-ssd   explicit storage class (default: cluster default)
#   make deploy SEED_SIZE_GB=0           skip data seeding
#   make deploy OTLP_ENDPOINT=host:4317  send telemetry somewhere (default: discard)
#   make otel   OTLP_ENDPOINT=... \       reconfigure just the collector, e.g. Coroot:
#     OTLP_HEADERS=x-api-key=<key> \         auth header (see scripts/deploy.sh for
#     OTLP_SIGNALS=traces                     per-vendor examples)
#   make clean                           tear everything down
#   make clean KEEP_DATA=1               tear down but keep PVCs
#   make status                          show lab state
#
# Telemetry export knobs (see scripts/deploy.sh header for details):
#   OTLP_ENDPOINT  destination (unset = receive and discard)
#   OTLP_HEADERS   auth headers, comma-separated key=value (e.g. x-api-key=...)
#   OTLP_SIGNALS   signals to forward: subset of traces,metrics,logs (default all)
#   OTLP_INSECURE  0 if the endpoint uses TLS (default 1, plaintext)

.PHONY: deploy clean status otel

deploy:
	@./scripts/deploy.sh

otel:
	@./scripts/deploy.sh otel

clean:
	@./scripts/clean.sh

status:
	@./scripts/status.sh
