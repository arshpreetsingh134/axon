# Plan: release and deploy preflight in axon

Status: Phases 0 (pass 1), 1, 2 and 3 built and verified, uncommitted (see §9). Phases 4 and 5 not started.
Scope: let axon answer "will this release deploy cleanly to an official environment?" locally, in seconds or minutes,
without starting a pipeline run, and automate the hand-copied steps between a build and a deploy. The pipeline stays
the system of record for real releases and deploys. axon becomes the fast pre-check and the clerk.

## 1. Why

On an official (pre-provisioned, image-backed, app-core 0.2) environment, mycel-gh's release-and-deploy path has
three gates that are only checked inside pipeline runs. One release on 2026-11-24 failed four times before it
deployed. Each failure cost a full pipeline run to discover:

| # | Gate | What failed | Could have been caught |
|---|---|---|---|
| 1 | Protected-main check on the source lock | The GitHub compare of a pin against `main` was 1.2 MB; the tool failed to read it ("unexpected EOF") | Locally, with `git merge-base --is-ancestor`, before starting the run |
| 2 | Required secret keys | A merged change added two `${VAR}` references to an app's env; the environment's pre-provisioned secret lacked them | At PR time, by diffing the `${VAR}` set against the environment's secret key names |
| 3 | Concurrent runs | A real deploy was queued behind a teammate's dry run without anyone noticing | By checking for queued and in-progress runs before starting one |
| 4 | Manual steps | Version numbers, image digests (copied from build logs), release records (downloaded from run artifacts) and environment selection were all written by hand across five PRs | By generating them |

axon already has most of the building blocks: `release lock/check/checkout/create`, `env check` (the `${VAR}`
findings), `env plan` (a server-side dry run of axon's own rendered manifests) and `ci status/watch/logs`. What it
lacks is any notion of an **official environment** in the mycel-gh layout: image-backed members, pre-provisioned
namespaces, the `github/` lock and workflow files, and schema-3 release records.

## 2. Clean-room rules (same as SPEC.md §0)

mycel-gh is Apache-2.0 and its NOTICE reserves the "mycel" trade names, exactly like mycel. So:

1. **Phase 0 writes a spec addendum** (`SPEC.md` Appendix C, "Official environments") from black-box sources only:
   - `mycel-gh` `--help` output;
   - the generated workflows and the `github/*.toml` files;
   - pipeline run logs;
   - committed release records;
   - live objects read back from a cluster after a mycel-gh deploy (`kubectl get -o yaml`).
2. Implementation works from that addendum only. Decompilation is a last resort, used only for a specific behavioural
   question that black-box sources can't answer. Results are recorded as behaviour, never as code, per §0.
3. New verbs, flags and files use axon names. Compat mode *reads* the mycel-gh layout; axon never writes "mycel"
   branding.
4. Test fixtures are synthetic. No real organization's hosts, IDs or repository names go into this public repo.

## 2a. Phase 0 findings (SPEC.md Appendix C, pass 1)

These change the plan as follows:

- **The reference tool can already dry-run as the developer.** `env deploy <env> <release> --check` does every apply
  and delete as a server-side dry run *as the calling identity*. With D1 this gives a local **oracle**: T1.1's
  conformance test compares axon's object set with the reference tool's check run on the same kube context.
- **The required-key rule is exact.** The required keys are the `[app.env]` values in the cluster descriptor that are
  exactly `"${NAME}"`. Image-backed member files contribute none. This was checked against two descriptor revisions.
  The reference tool's own `env check` doesn't run this gate, so T0.3 fills a real gap. A `${NAME}` inside a comment
  doesn't count: a real deploy passed with one present.
- **Deploys read the descriptor from the default-branch head,** not from the release commit. A descriptor fix
  merged after the release took effect without a new release. T1.3 therefore renders "release images + current
  descriptor".
- **Foreign record digests don't match axon's encoding** (checked on two records). T0.1 checks structure only.
  Verifying them waits on pass 2 (C.9).
- **Concurrency:** deploy runs queue silently, and run inputs can't be read through the API. T2.1 must refuse,
  rather than queue, by default.
- **Pass 2 (decompilation through REA) is blocked by configuration.** The session's REA server has no Ghidra
  settings. It's needed only for the items in C.9 (the digest encoding and the renderer's edge cases). Phases 1 to
  3 don't depend on it.

## 3. What we build

Three tiers. Each is useful on its own, and later tiers build on earlier ones.

### Tier 0: offline checks (no cluster, no pipeline run)

**T0.1 Read the official layout (compat).** `internal/descriptor` and `internal/release` learn:
- `github/workflow.toml` (registry, environments, pinned tool version);
- `github/sources.lock.toml` (in addition to axon's `release/sources.lock.toml`);
- `github/materializations/<member>/mycelfile` (image-backed members: `image@sha256`, token, db);
- release records with `schema_version = 3` (modules, sources, platform images, evidence). These are read-only: axon
  verifies their structure but doesn't recompute a foreign digest, the same as `MadeByAxon` today;
- environment files with `tier`, `namespace`, `domain` and `release`, where a non-dev tier means
  **pre-provisioned**: never create a namespace or install fabric.

**T0.2 `axon release lock --check`: are the pins on protected main?**
- For each locked source, confirm the pinned commit is an ancestor of the source's protected branch.
- Primary method: a local mirror (`git fetch` into an axon cache, then `git merge-base --is-ancestor`). This has no
  response-size limit, which is gate 1's failure.
- Fallback: the GitHub compare API, reading only `status`, `ahead_by` and `behind_by` with a streaming decoder and no
  body cap.
- Output names each source and says "on main", "N commits behind main" or "not on main (develop-only)", with a hint:
  "move the pin to main's head with `axon release lock --source <app> --to main`".

**T0.3 `axon env check <env>`: required keys versus the environment's inventory.**
- Today `env check` resolves `${VAR}` against the local `.env` and secret store. Extend it to official environments:
  collect every `${VAR}` that the cluster descriptor's `[app.env]` sections and the image-backed members need.
- Compare against the environment's **key inventory**: key names only, never values. It's read live (T1.2) or from
  axon's local state, `~/.config/axon/inventory/<cluster>/<env>.keys` (honouring `XDG_CONFIG_HOME`), which
  `env check --live` refreshes. The inventory is never written into a repository, not even an ignored file inside
  one (decision D2).
- A new key that isn't in the inventory is a blocking finding that names the file and line that introduced it.
  Gate 2 would have been caught at PR time.

**T0.4 `axon release diff <a> <b>`: what changed between two releases.**
- Per module: old digest → new digest. Per source: old commit → new commit. Per platform image: old → new.
- Attribute every change to the commit (and PR, when `gh` is available) that changed the pin. The output is ready to
  paste into a release PR.
- Flag images whose digest changed with no pin change (a rebuild by the release itself), so a reviewer can tell
  expected rebuilds from surprises.

**T0.5 `axon release check --pins`: are the pins consistent with each other?**
Each image digest must agree across:
- the materialization files;
- the build workflow revisions;
- the smoke workflow;
- the release record.

Every pinned image must also exist in the registry for each release platform (`docker buildx imagetools inspect`,
which needs `read:packages`).

### Tier 1: live cluster checks (read-only plus a server-side dry run)

**T1.1 Render the 0.2 topology.** This is the largest piece of work.
- Add `gen.K8sOfficial` to render what an official deploy applies for image-backed app-core 0.2 members: deployments,
  services, ingresses, delegation-target config maps, service accounts and roles, the worker deployments, the
  Temporal search-attribute job, and env wiring (`${VAR}` → `secretKeyRef` into the app env secret).
- The spec comes from T0's black-box sources and from live objects.
- Conformance test: render the release that is currently deployed, `kubectl diff --server-side` it against the live
  cluster, and expect no differences other than the fields Kubernetes or the controller manage (status, managed
  fields, generated annotations). Iterate on the renderer until this is clean. That's the acceptance bar for T1.1.

**T1.2 Live key inventory.**
- Read the **key names** of the pre-provisioned app env secret (`kubectl get secret -o json | jq '.data|keys'`).
  Never read the values.
- Feed the names to T0.3. `axon env check <env> --live` refreshes the local inventory file (T0.3, D2).

**T1.3 `axon env plan <env> <release>` for official environments.**
- Run T0.2, T0.3 (live), T0.5 and then T1.1. Then `kubectl apply --server-side --dry-run=server` and
  `kubectl diff --server-side` for every rendered object.
- Report:
  - per object: unchanged, changed (with a field-level diff summary), created or deleted;
  - per app: the image it moves from and to;
  - "would retire" items, such as objects left behind by renames.
- Never create a namespace, never touch fabric on a pre-provisioned environment, and never print secret values.

**T1.4 RBAC preflight.**
- `kubectl auth can-i` for every verb and resource the plan would use, as the current identity, before the dry run.
- Kubernetes doesn't distinguish dry-run permission from write permission: a server-side dry run of `apply` needs the
  same `create`/`patch` rights as the real apply. So a missing grant should say exactly which role binding to ask
  for, not fail halfway through.

### Tier 2: pipeline orchestration (makes the official path faster and safer)

**T2.1 `axon ci deploy <env> <release> [--check]`: start a deploy run with guards.**
- Refuse if a deploy run for the same environment is queued or in progress, unless `--queue` is given. Show who
  started it and when. This is gate 3.
- Run T1.3 locally first when cluster access exists (`--no-local` skips it).
- Start the run, watch it, and print the summary line, or the first failing line with its step.

**T2.2 `axon release pin --from-run <run-id>...`: write image digests from build runs.**
- Read each build run's logs and take the image index (manifest-list) digest per image. Logs are used so this works
  without `read:packages`.
- Rewrite the materialization files and the smoke workflow, then run T0.5. `--pr` opens the PR (branch, commit,
  body listing runs, sources and digests).
- `--only <token>` pins a subset, for a rebuild of one image.

**T2.3 `axon release record --from-run <run-id> --select <env>`: commit a release record.**
- Download the record artifact into an isolated directory, check its structure, and run T0.4 against the
  environment's current release.
- Write `releases/<v>.toml`, set the environment's `release`, and optionally open the PR with the T0.4 diff as the
  body.

**T2.4 `axon release next`: choose the next version.** `release.Next` already exists. Expose it for the official
layout (series from the policy or the latest record) and use it as the default `version` input when starting a
release run.

## 4. Order of work

| Phase | Deliverable | Size | Depends on |
|---|---|---|---|
| 0 | SPEC.md Appendix C: official-environment behaviour from black-box sources | M | Live cluster read access for object snapshots |
| 1 | T0.1 layout reading, T0.2 lock check, T0.5 pin consistency | M | Phase 0 |
| 2 | T0.3 key inventory (committed list), T0.4 release diff | S | Phase 1 |
| 3 | T2.1 guarded run start, T2.2 digest pinning, T2.3 record, T2.4 next version | M | Phase 1 |
| 4 | T1.2 live key names, T1.4 RBAC preflight | S | Cluster credentials |
| 5 | T1.1 renderer and conformance test, T1.3 official `env plan` | L | Phases 0 and 4 |

Phases 1 to 3 remove all four failures from §1 without the renderer. Phase 5 is what makes the local dry run a real
substitute for the pipeline's check-mode run. It's also the riskiest part: the renderer must track the reference
tool's output as both evolve, so the conformance test (T1.1) must run on every change to the renderer.

## 5. Code map

| Area | Files |
|---|---|
| Layout reading | `internal/descriptor/load.go`, `types.go` (image-backed members, official env fields); `internal/layout` (compat paths for `github/`) |
| Lock check | `internal/commands/release.go` (`runReleaseLock` gains `--check`, `--source`, `--to`); new `internal/gitmirror` |
| Records | `internal/release/release.go` (schema-3 reader, `Diff`); `internal/commands/release.go` (`diff`, `pin`, `record`, `next`) |
| Key inventory | `internal/commands/env.go` (`envFindings`, `liveFindings`); `internal/deploy/secrets.go` |
| Renderer | new `internal/gen/k8s_official.go`; `internal/deploy/k8s.go` (`planK8s` official branch: no namespace, no fabric) |
| Pipelines | `internal/ci/github.go`, `internal/ci/logs.go` (run guard, digest extraction, artifact download) |

## 6. Tests and acceptance

- Unit tests with synthetic fixtures: an image-backed member, a schema-3 record, a lock whose pin is far behind main
  (a large compare), and a descriptor change that adds a `${VAR}`.
- Golden tests for `release diff` and `release pin` output.
- Acceptance, run privately against the real QA environment. Results stay out of this repo.
  1. `release lock --check` on a lock that pins a commit well behind main reports "N behind, on main", where the
     reference tool failed with "unexpected EOF".
  2. `env check <env> --live` on a descriptor that references keys the environment's secret lacks names exactly
     those keys and the file that introduced them.
  3. `env plan <env> <current release>` immediately after a pipeline deploy shows zero changes.
  4. `env plan <env> <next release>` shows the same objects as the pipeline's check-mode run, apart from the
     documented differences.
  5. `ci deploy --check` refuses to start while another deploy run for that environment is queued or running.

## 7. Prerequisites and constraints

- **Cluster access (D1, the developer's own identity):** the Azure CLI, `kubelogin`, and an Azure RBAC role for the
  developer on the environment's namespace with the verbs from T1.4. Tier 1 needs write-level rights even for a dry
  run. Tiers 0 and 2 need none.
- **GitHub:** a `gh` token that can read the cluster and source repositories. Add `read:packages` for the T0.5
  registry check; T2.2 works without it.
- **Org repositories stay as they are.** This tool runs locally. It doesn't add jobs, files or names to an
  organization's repositories; their pipelines remain generated by the reference tool. A PR-time gate there (gate
  2) would be a separate, tool-neutral proposal to that repository's owners.

## 8. Decisions (2026-11-24)

- **D1. Tier 1 runs as the developer's own identity.** It uses the developer's kube context and Azure login, never
  the pipeline's deploy identity and never impersonation. T1.4 tells the developer which grants they lack.
- **D2. The key inventory lives only in axon's local state** (`~/.config/axon/inventory/…`), never in any
  repository.
- **D3. The renderer is pinned to the reference tool version** that the cluster's workflow file pins
  (`runner_version`). axon records which version its renderer conforms to, and warns, without blocking, when the
  workflow moves to a version axon hasn't been checked against.

## 9. Status (2026-11-24)

Built (uncommitted). All Go tests and 507 black-box tests pass.

| Verb | Plan item | Verified against the real cluster repository (read-only) |
|---|---|---|
| `release lock --check` | T0.2 | At the commit whose pin broke the pipeline gate ("unexpected EOF"), it reports "on main, 2 commit(s) behind" in about 7 s (mirrors cached). It checks `main`, not the default branch. |
| `release check --pins` | T0.5 | Clean on main (41 pins). It catches a tampered digest and a tampered build revision. |
| `env check <env>` (official) | T0.3 | Before the descriptor fix it names exactly the two keys the deploy stopped on, with file, line and app; after the fix it passes. |
| `env keys <env>` | T0.3, T1.2 | `--add` and `--remove` work; the inventory is `0600` under `~/.config/axon/inventory/`. `--live` is **not yet run against a cluster** (no credentials on this machine). |
| `release diff <a> <b>` | T0.4 | 0.1.17 → 0.1.18: every change is traced to its commit and PR, matching a manual review. Rebuilds and direct pushes are told apart. |
| `release next` | T2.4 | Gives 0.1.19, counting records on origin/main. |
| `release pin --from-run` | T2.2 | Replaying the three build runs before the digest PR produces a result **byte-identical** to the hand-written PR (19 pins, 12 files). |
| `release record --from-run` | T2.3 | Replaying the release run produces a record and environment file **identical** to the hand-written record PR. It refuses a failed run and an existing record. |
| `ci deploy <env> <release>` | T2.1 | The guard and preflight pass with `--dry-run`, and it blocks a release not on main. **Starting a run and watching it have not been exercised**, since that starts a pipeline run on a shared environment. |

Found by the new checks: a Forms build workflow builds its source at a commit that is **not on main** (only on `develop`
and feature branches). The pipeline's gate doesn't check build-workflow revisions, so this went unnoticed.

Next:
- **Phase 4:** run `env keys --live` and add the RBAC preflight once the developer has a kube context for the cluster
  (D1).
- **Phase 5:** the renderer.
- **Pass 2 (C.9):** needs the REA server configured with Ghidra in the configuration this session reads.

### Phase 4 and 5 progress (2026-11-24, later)

- **The developer's identity on the QA cluster (D1):** the developer can read workloads, ConfigMaps, Ingresses and
  NetworkPolicies, but **cannot read secrets and cannot write**. So `env keys --live` and a server-side dry run aren't
  possible with this role. The renderer's plan compares with the live objects' *last-applied* manifests instead,
  which needs read access only. To read key names, the role needs `get` on the app env secret.
- **`axon env plan <env> [<release>] --context <ctx>`** for official environments renders the deploy objects and compares
  them with the live last-applied manifests. On QA, release 0.1.19 gives **1,017 of 1,017 rendered objects
  unchanged**. That's about 79% of what a deploy applies. Covered:
  - every switcher app's ServiceAccount, Service, Ingress, delegation ConfigMap and Deployment;
  - all their NetworkPolicies, from one edge list mirrored into ingress rules;
  - the namespace-wide policies.
- **Not rendered yet:**
  - the IAM and console Deployments, which need runtime inputs: the IAM configuration and first admin, and the
    console topology;
  - the two workers;
  - the fabric (traefik, temporal, keycloak, grafana, tempo, helix, clickhouse), with its jobs, ConfigMaps and
    policies.
