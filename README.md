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

## Scenario library

Every scenario uses a genuine real-world mechanism — never a synthetic fault
flag inside the app — and reverts durably. Each carries an `expectedSymptoms`
list that doubles as documentation and a grading rubric for RCA tools.

| Scenario | Category | Mechanism | What an RCA tool should find |
|----------|----------|-----------|------------------------------|
| `pg-analytics-queries` | database | An `analytics-reporting` workload runs heavy multi-join/aggregation queries (full scans of the ~10 GB products table) against the production PostgreSQL, through the same pgBouncer pool as the apps. | Elevated `product-catalog`/`inventory-service` latency; PostgreSQL CPU/IO saturation; new full-scan query fingerprints in `pg_stat_statements` attributable to the `analytics-reporting` workload. |
| `mysql-analytics-queries` | database | The same `analytics-reporting` actor runs large join/aggregation queries (filesort, temp tables) against the production MySQL `orders` database via HAProxy. | Elevated `order-service`/checkout latency and errors; PXC CPU/IO saturation; heavy statements in the slow query log attributable to the workload. |
| `order-service-gc-regression` | deploy | A genuine bad deploy: `order-service` rolls out `1.1.0`, a real code regression that deep-copies every order read into an ineffective cache. GC pressure builds; revert rolls back to the known-good image. | p99 rises after the rollout while p50 stays flat; JVM allocation rate and GC time climb; heap sawtooth trends toward the limit; onset correlates exactly with the deployment event. |
| `traffic-spike` | infra | The `load-generator` Deployment is scaled to 5 replicas — real extra traffic across the whole stack. | Uniform RPS increase everywhere; saturation (latency/errors) appears only at the weakest component, testing cause-vs-consequence reasoning. |

More scenarios (bad migrations, connection-pool leaks, Kafka consumer lag,
network partitions, cache eviction pressure, and others) are on the roadmap;
each will follow the same real-mechanism, durable-revert rule.

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
