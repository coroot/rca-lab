# The FailureScenario operator

The `rca-lab-operator` (in `operator/`) turns declarative `FailureScenario`
custom resources into real, reversible failures. A scenario is the source of
truth; the operator reconciles the world to match it and — crucially —
**restores normal state durably**, even across its own crashes.

## Triggering a scenario

Scenarios live in `scenarios/` and are `FailureScenario` CRs in the `default`
namespace. Start/stop them with the web UI (`kubectl port-forward
svc/rca-lab-operator 8080`) or with kubectl:

```bash
kubectl get failurescenarios
kubectl patch failurescenario <name> --type=merge -p '{"spec":{"enabled":true}}'   # start
kubectl patch failurescenario <name> --type=merge -p '{"spec":{"enabled":false}}'  # stop
```

`enabled: true` holds the failure until you disable it (or its `duration`
expires). A one-shot timed run instead sets `spec.trigger.runID` (a fresh id)
and `spec.trigger.duration` — the UI's "run" button. To stop a *triggered* run
you must clear `spec.trigger` (the UI's Stop does this), not just `enabled`.

## Action types

An action is one failure-injection primitive; a scenario composes an ordered
list of them (each with an optional `delay` offset).

| Type | What it does | Revert |
|------|--------------|--------|
| `Workload` | Runs a Job that looks like a real workload — a `dbtool` query-loop (heavy analytics SQL), or a `cpuBurn` CPU hog optionally `colocateWithApp` a victim (noisy neighbor) | Delete the Job (ownerRef + `activeDeadlineSeconds` dead-man too) |
| `Scale` | Scales a Deployment (e.g. load-generator ×5) | Restore previous replica count; `held-by` lock prevents two scenarios fighting over one target |
| `DeployImage` | Rolls out a different image for one container — a genuine bad deploy | Roll back to the known-good image (recorded in the revert token, a Deployment annotation, and optionally spec) |
| `ChaosMesh` | Creates a Chaos Mesh CR (`NetworkChaos`, `StressChaos`, …) verbatim, with a dead-man `spec.duration` | Delete the chaos object (Chaos Mesh's own finalizer runs recovery) |

## Durability model (why revert is crash-safe)

Every action follows **plan → persist token → mutate**:

1. `Plan` observes live state and returns a fully self-contained revert token.
2. The controller persists that token to the scenario `status.activeActions`
   **before** any mutation.
3. `Revert` restores normal state **from the token alone** — it never reads the
   spec, is idempotent, and tolerates never-applied / vanished / double calls.

On top of that:

- A **finalizer** guarantees a deleted CR reverts before it disappears.
- A **startup sweeper** deletes any object labeled `rcalab.dev/scenario` whose
  owning scenario is gone or idle (a backstop for force-deleted CRs).
- **Dead-man switches** (`activeDeadlineSeconds` on Jobs, `spec.duration` on
  Chaos Mesh objects) self-recover the blast radius even if the operator is
  down for a long time.

These guarantees are exercised by `test/e2e/durability.sh` (run against a live
cluster with the lab deployed):

```bash
test/e2e/durability.sh
# DRILL 1 — delete a CR mid-run: the finalizer reverts first
# DRILL 2 — kill the operator mid-run: it resumes from status and still reverts
# DRILL 3 — orphaned labeled workload: the startup sweeper cleans it up
```

## Adding a scenario

1. Write a `FailureScenario` YAML under `scenarios/<category>/` and add it to
   `scenarios/kustomization.yaml`. Give it accurate `expectedSymptoms` — they
   are the grading rubric for RCA tooling.
2. For a **bad-deploy** scenario, add a variant under
   `services/<svc>/variants/<name>/` (a real code regression) with a
   `variant.yaml` declaring a plausible next-semver `tag`; CI builds it and
   `scripts/gen-variant-registry.py` records it in `variants/registry.yaml`.
   `scripts/check-scenario-images.py` (CI) fails if a scenario references an
   image tag that isn't a base tag in `versions.yaml` or a registry variant.
3. A `DeployImage` action should omit `knownGoodImage`: the operator records
   the image the target is actually running at activation, so the rollback
   target never drifts.
