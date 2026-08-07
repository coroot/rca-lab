# rca-lab

A realistic, reproducible failure lab for evaluating root-cause-analysis (RCA)
tooling — human or AI — on a live Kubernetes cluster.

Most RCA benchmarks replay canned telemetry from toy environments with
synthetic faults toggled by feature flags. rca-lab takes the opposite approach:

- **A real polyglot microservice stack** (Python, Go, Java, Node.js, Rust, PHP)
  behind an API gateway, with continuous generated load.
- **Real databases under production-grade operators**: PostgreSQL, MySQL and
  MongoDB via Percona operators, replicated Valkey, Kafka via Strimzi — with
  seeded data volumes.
- **Real failure mechanisms only.** No chaos flags inside the apps. A GC
  pressure incident is a genuine allocation regression shipped as a new image
  version and rolled back later; a database incident is an analytics workload
  running heavy queries against the production database; a traffic spike is
  actually more traffic.
- **Durable revert.** Every failure scenario is a `FailureScenario` custom
  resource driven by an operator that restores the normal state when the
  scenario ends, is disabled, or is deleted — even across operator restarts.
- **Rich telemetry, bring your own backend.** Every service is instrumented
  with OpenTelemetry SDKs: traces, SDK-emitted metrics (JVM/runtime/HTTP), and
  logs (to stdout *and* OTLP, trace-correlated). Everything flows to a bundled
  otel-collector that **discards data by default** — point it at any OTLP
  backend with one variable.

## Quick start

Requirements: `kubectl` + `helm` pointed at a cluster (any distribution;
a default StorageClass, ~8 CPU / 16 GiB across nodes for the full-size lab).

```bash
git clone https://github.com/coroot/rca-lab && cd rca-lab
make deploy                 # everything: operators → databases → Kafka → apps → seed
```

Single-node cluster (kind/k3d/minikube):

```bash
make deploy SINGLE_NODE=1
```

Send telemetry somewhere (e.g. Coroot, or any OTLP endpoint):

```bash
make deploy OTLP_ENDPOINT=my-backend:4317
```

Other variables: `STORAGE_CLASS=<name>`, `SEED_SIZE_GB=<n>` (0 skips seeding),
`OTLP_HEADERS=k=v`, `YES=1` (no confirmation prompt). Re-running `make deploy`
converges idempotently — it is also how you change any of these settings.

Teardown:

```bash
make clean                  # KEEP_DATA=1 keeps the database volumes
```

## Triggering failures

Scenarios are Kubernetes custom resources:

```bash
kubectl get failurescenarios
kubectl patch failurescenario pg-analytics-queries --type=merge -p '{"spec":{"enabled":true}}'
```

or use the web UI:

```bash
kubectl port-forward svc/rca-lab-operator 8080
```

Each scenario documents its mechanism and the telemetry symptoms an RCA tool
should be able to observe. Scenarios can also run on a cron schedule with a
fixed duration — see `scenarios/`.

> **Never install rca-lab on a shared or production cluster.** The scenario
> operator deliberately has the power to degrade workloads in its namespace.

## Architecture

```
load-generator → api-gateway ─┬→ product-catalog ─→ PostgreSQL (products) ─┐
                              │        └─→ recommendation-service (gRPC)   │ Percona PG
                              ├→ inventory-service → PostgreSQL (inventory)┘
                              ├→ cart-service ─→ Valkey (replicated)
                              │        └─→ order-service ─→ MySQL (orders) ─┐
                              ├→ order-service ─→ payment-service           │ Percona PXC
                              │        │              └→ MySQL (payments) ──┘
                              │        └─⇢ Kafka (order-events) ⇢ fulfillment-service
                              └→ review-service ─→ MongoDB (reviews, PSMDB)
```

All services export OTLP (traces, metrics, logs) to `otel-collector` in the
`default` namespace. Operators live in their own namespaces (`pg-operator`,
`pxc-operator`, `psmdb-operator`, `strimzi`).

## Repository layout

- `services/` — application sources, one directory per service; `variants/`
  subdirectories hold **bad-deploy variants**: real code regressions built into
  plausibly-versioned images for deploy/rollback scenarios.
- `deploy/` — Kubernetes manifests (databases, Kafka, otel, apps) and helm
  values for the operators.
- `scenarios/` — the failure scenario library.
- `operator/` — the `FailureScenario` operator, its embedded web UI, and the
  `dbtool` used by database scenario workloads.
- `scripts/` — `deploy.sh` / `clean.sh` / `status.sh` driven by the Makefile.
