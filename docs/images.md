# Image versions & deploying changes

Container image tags in rca-lab have a **single source of truth**, so that
changing a service, building it, and deploying it is a predictable flow with no
mutable-tag surprises.

## Sources of truth

- **`versions.yaml`** — the tag for every service's *base* image
  (`ghcr.io/coroot/rca-lab/<service>:<tag>`). This is the version that runs in
  the lab.
- **`services/<svc>/variants/<name>/variant.yaml`** — the tag for each
  *bad-deploy variant* image (a real code regression used by a deploy
  scenario). `variants/registry.yaml` is generated from these by
  `scripts/gen-variant-registry.py`.

## The workflow: change a service

1. Edit the service under `services/<svc>/`.
2. **Bump that service's tag in `versions.yaml`** (patch for a fix, minor for a
   feature). This is the step that makes the change deployable — it is how the
   deploy learns there is something new to roll out.
3. Commit and push. CI (`.github/workflows/build.yml`) rebuilds the changed
   service and pushes `:<new-tag>` (plus `:sha-<commit>` and `:latest`).
4. `make deploy` resolves each service's tag from `versions.yaml` and applies
   it. Only the service whose tag changed is rolled out; untouched services
   keep their tag and are left running.

Because tags are immutable in practice (a new build means a new tag), a plain
`kubectl apply` sees a real change and performs a normal rollout — no
`rollout restart`, no `imagePullPolicy: Always` tricks required. Forgetting
step 2 means the same tag is rebuilt and the deploy is a no-op — that is the
signal to bump the version.

## How the deploy resolves tags

`scripts/deploy.sh` reads `versions.yaml` and applies a kustomize `images:`
override to the app, operator, and seed manifests. The tags hardcoded in
`deploy/**` manifests are only a readable fallback for `kubectl apply -k`
without the script; `make deploy` is authoritative.

## Failure-scenario images

Deploy scenarios (e.g. `scenarios/deploy/order-service-gc-regression.yaml`)
reference two kinds of image:

- The **regression image** they roll out — a variant, whose tag comes from the
  variant's `variant.yaml` and appears in `variants/registry.yaml`.
- The **rollback target** — *not* pinned in the scenario. The operator records
  the image the target Deployment is actually running when the scenario
  activates and rolls back to exactly that, so the rollback target never drifts
  from `versions.yaml`.

`scripts/check-scenario-images.py` (run in CI) fails the build if a scenario
references an image tag that is neither a base tag in `versions.yaml` nor a
variant in `variants/registry.yaml` — catching drift before it reaches a
cluster.
