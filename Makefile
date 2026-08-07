# rca-lab — failure reproduction lab for RCA tooling.
#
# Usage:
#   make deploy                          deploy/converge everything (idempotent)
#   make deploy SINGLE_NODE=1            single-node sizing (kind/k3d/minikube)
#   make deploy STORAGE_CLASS=fast-ssd   explicit storage class (default: cluster default)
#   make deploy SEED_SIZE_GB=0           skip data seeding
#   make deploy OTLP_ENDPOINT=host:4317  send telemetry somewhere (default: discard)
#   make clean                           tear everything down
#   make clean KEEP_DATA=1               tear down but keep PVCs
#   make status                          show lab state

.PHONY: deploy clean status

deploy:
	@./scripts/deploy.sh

clean:
	@./scripts/clean.sh

status:
	@./scripts/status.sh
