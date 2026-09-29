# Deploying a Release — Operator Workflow

> A practical, project-neutral runbook for moving an eligible revision on trusted `main` to a deployment target with Deploy Toolkit.
>
> This guide assembles the existing contracts into one operator path; it does not redefine them. Exact semantics belong to the owner documents linked throughout and listed at the end.

---

## 1. The deployment model

Deploy Toolkit separates four different decisions:

```text
code change
   ↓
CI decides whether the change is acceptable
   ↓
human merges to trusted main
   ↓
release creation decides exactly what bytes make up a release
   ↓
promotion PR asks whether an environment should use that release
   ↓
human merge authorizes the environment change
   ↓
deployment makes the target match the promoted state
```

The important distinction is:

```text
Release ≠ Promotion ≠ Deployment
```

- A **release** is an immutable description of source, artifacts, deployment bundle, and migration semantics.
- A **promotion** changes an environment's desired release.
- A **deployment** applies that already-authorized desired state to the target.

Deploy Toolkit never treats "run the deploy command" as the authorization act. The authorization is the human-approved promotion merge.

---

## 2. Before you begin

A consumer repository should already contain:

```text
.deploy/
├── project.yaml          # the deployment contract: lifecycle hooks, artifacts, bundle
├── environments/
│   └── <environment>.yaml
├── targets/
│   └── <target>.yaml
└── releases/             # generated, immutable
```

The deployment contract — including the mandatory `apply` and `verify` hooks — is defined by [consumer-contract-v1.md](consumer-contract-v1.md).

The target must already satisfy the generic target contract. For SSH targets, [bootstrap-ssh-target.md](bootstrap-ssh-target.md) is the operational guide for preparing one; [target-prerequisites.md](target-prerequisites.md) is the normative reference. This guide does not repeat target preparation.

Deploy Toolkit does not provision hosts, install runtimes, configure DNS, create databases, or create application secrets.

---

## 3. Step 1 — Choose the eligible revision

Choose the exact eligible revision you want to release. In the common case of releasing the current trusted `main`, obtain it with:

```bash
git rev-parse origin/main
```

Use the **full 40-character SHA** throughout release creation.

Eligibility is decided by the actual policy at release creation: the revision must exist, belong to the permitted trusted branch, and have every required check concluded successfully for that exact SHA — missing, skipped, cancelled, in-progress, or failed checks are not eligible ([release-lifecycle.md](release-lifecycle.md)).

Do not create a release from a PR head unless that exact revision has become part of trusted `main`.

---

## 4. Step 2 — Confirm releasable artifacts are published

Ensure the artifact-discovery requirements in [consumer-contract-v1.md](consumer-contract-v1.md) are satisfied: every releasable OCI artifact **MUST** be published with the full source Git SHA as a temporary discovery tag:

```text
registry.example.com/project/app:<full-source-sha>
registry.example.com/project/worker:<full-source-sha>
```

Release creation resolves each discovery tag to its immutable registry digest and records only the digest in the release manifest; the tag itself is disposable.

If the target must pull private artifacts, verify the deployment identity's registry authentication before the first deployment (see [bootstrap-ssh-target.md](bootstrap-ssh-target.md)).

---

## 5. Step 3 — Decide the release metadata

Release creation requires explicit migration semantics; Deploy Toolkit deliberately does not infer them. Choose:

```text
version
migration head
migration mode
rollback safety
```

Example:

```text
version:        0.4.0
migration head: 052
mode:           forward-compatible
rollback safe:  true
```

The meanings of the migration modes and their rules (`irreversible` requires `rollbackSafe: false`, and so on) are owned by [consumer-contract-v1.md](consumer-contract-v1.md). These values are application claims — the operator or project must decide them deliberately.

---

## 6. Step 4 — Create the immutable release

From a checkout containing the candidate revision:

```bash
deployctl release create \
  --repo OWNER/REPOSITORY \
  --revision <full-source-sha> \
  --version <version> \
  --migration-head "<migration-head>" \
  --migration-mode <mode> \
  --repo-dir .
```

If rollback is explicitly safe, add:

```bash
--rollback-safe
```

Example:

```bash
deployctl release create \
  --repo example/acme-service \
  --revision 0123456789abcdef0123456789abcdef01234567 \
  --version 0.4.0 \
  --migration-head "052" \
  --migration-mode forward-compatible \
  --rollback-safe \
  --repo-dir .
```

Release creation runs the deterministic eligibility pipeline — ancestry, required checks, artifact digest resolution, bundle and contract digests from the exact Git tree — described in [release-lifecycle.md](release-lifecycle.md).

The result is an immutable release manifest:

```text
.deploy/releases/<project>-<version>.yaml
```

Do not hand-edit generated release manifests.

Release creation does **not** change an environment and does **not** open a deployment authorization PR.

---

## 7. Step 5 — Propose the release to an environment

Create the promotion proposal:

```bash
deployctl promotion propose <environment> \
  --release .deploy/releases/<project>-<version>.yaml \
  --repo OWNER/REPOSITORY \
  --repo-dir .
```

Example:

```bash
deployctl promotion propose staging \
  --release .deploy/releases/acme-service-0.4.0.yaml \
  --repo example/acme-service \
  --repo-dir .
```

The proposal is one atomic commit containing only the authorized deployment-state change:

```diff
 spec:
-  release: .deploy/releases/acme-service-0.3.2.yaml
+  release: .deploy/releases/acme-service-0.4.0.yaml
```

For a first deployment, the environment instead moves from the project's bootstrap sentinel to the first real release.

---

## 8. Step 6 — Verify the promotion PR

Before authorization, the promotion diff is checked by trusted automation:

```bash
deployctl promotion check \
  --repo OWNER/REPOSITORY \
  --base <trusted-base-sha> \
  --head <promotion-pr-head-sha> \
  --repo-dir .
```

The check enforces the Promotion Diff Policy — exact allowed diff shape, compare-and-swap freshness against the live trusted head, and re-verification of release evidence — and fails closed on anything else ([promotion-diff-policy.md](promotion-diff-policy.md)).

Prefer running this in trusted CI rather than relying on a developer's local machine.

---

## 9. Step 7 — Human reviews and merges the promotion PR

This is the authorization boundary:

```text
promotion PR merge = authorization to change the environment
```

The human reviewer should confirm at least:

```text
correct environment
correct release version
correct source revision
expected artifact set
expected migration semantics
expected rollback-safety declaration
no unrelated changes
promotion check passed
```

Do not treat CI success by itself as authorization.

Do not let an agent merge unless a human has explicitly delegated that authority.

---

## 10. Step 8 — Trigger the deployment workflow

A consumer repository invokes Deploy Toolkit through the published reusable workflow:

```yaml
jobs:
  deploy:
    permissions:
      contents: read   # prepare: repository checkout
      actions: read    # deploy: run-artifact download

    uses: magtheo/deploy-toolkit/.github/workflows/deploy.yml@<full-toolkit-sha>

    with:
      environment: staging        # required
      # repo_dir: .               # optional (monorepos)
      # owner: ""                 # optional audit identity

    secrets:
      target_host: ${{ secrets.TARGET_HOST }}
      target_ssh_key: ${{ secrets.TARGET_SSH_KEY }}
      target_host_key: ${{ secrets.TARGET_HOST_KEY }}
```

Always pin the toolkit workflow by a **full commit SHA** — that pin is the machinery trust anchor ([consumer-contract-v1.md](consumer-contract-v1.md)).

The caller is responsible for invoking deployment only from the trusted, human-promoted branch state — for example, a manual workflow restricted to the trusted branch.

The workflow splits authority deliberately ([prepared-artifact-v1.md](prepared-artifact-v1.md), [trust-model.md](trust-model.md)):

```text
prepare job: repository access, no target credential
        ↓ immutable prepared deployment artifact
deploy job: target credential, no consumer source checkout
```

No single job holds both repository authority and the production target credential.

---

## 11. Step 9 — What Deploy Toolkit does on the target

The lifecycle engine executes one fixed ordering, owned by [release-lifecycle.md](release-lifecycle.md) and the hook protocol in [consumer-contract-v1.md](consumer-contract-v1.md):

```text
VALIDATE → STAGE → PREFLIGHT → MIGRATE → APPLY → VERIFY → COMMIT OBSERVED STATE
```

- **VALIDATE** — the prepared material and manifest relationships verify.
- **STAGE** — the immutable bundle is staged into the release directory on the target; hooks execute from there.
- **PREFLIGHT** — the project's optional preflight hook decides whether the staged release may begin consequential execution.
- **MIGRATE** — the project's optional migrate hook runs; the toolkit records its result without interpreting it.
- **APPLY** — the mandatory apply hook makes the target run the release, however the project defines that.
- **VERIFY** — the mandatory verify hook is the authoritative application-level proof.
- **COMMIT OBSERVED STATE** — only after verification succeeds is the release recorded as observed state.

Deploy Toolkit owns the ordering and the evidence; the project's hooks own everything application-specific.

---

## 12. Step 10 — Read the deployment result

The deployment exposes a single `deployctl.result/v1` document. The document — not the process exit code — is the decision surface: its envelope carries `outcome`, `recoveryRequired`, and `safeToRetry`, with per-command facts (e.g. `committed`, `alreadyCurrent`, `attemptId`) in `data`. The result contract, outcome enum, and exit-code categories are owned by [cli-v1.md](cli-v1.md).

A normal successful deployment ends with observed state matching the promoted release.

Classify the target with:

```bash
deployctl status <environment> --repo-dir .
```

or, machine-readable:

```bash
deployctl status <environment> --repo-dir . --json
```

If the result was not a success, continue with §13 — do not start by inspecting the target by hand.

---

## 13. When a deployment fails

Failure handling has a deliberate order of operations:

```text
the toolkit classifies  →  the operator gathers application-specific evidence
                           where the toolkit cannot know  →  the toolkit
                           performs recovery/resolution
```

Keeping that order is what prevents an incident from improvising a second, worse deployment control plane.

> **Operator rule**
>
> Start deployment failure investigation with `deployctl.result/v1` and `deployctl status <environment> --json`.
>
> Treat direct target inspection as **application-specific evidence gathering, not state management**.
>
> If consequential work may have executed, do not blindly retry the deployment and do not delete toolkit-owned evidence. Establish a target condition compatible with the next action, then use the documented recovery/resolution path.

### 13.1 Start with the deployment result

The complete `deployctl.result/v1` document — never the process exit code alone — is the first failure evidence surface. Exit codes are broad process categories so a supervisor can route; decisions come from the document ([cli-v1.md](cli-v1.md)).

Read at least:

```text
outcome                 success | failure | refused | uncertain |
                        infrastructure-failure | usage-error
safeToRetry             whether re-running the SAME operation can succeed as-is
recoveryRequired        the toolkit's authoritative conclusion that recovery
                        or resolution is currently required
data.consequentialStarted              the durable attempt marker was written
data.attemptId / data.recoveryId       the boundary identity needed for recovery
stages[]                per-lifecycle-stage outcomes
lockReleaseFailed / stagingLockReleaseFailed / data.lockRetained
                        lock facts, when present
```

The result answers the first question:

```text
did the operation fail BEFORE consequential execution,
or is there target-state uncertainty that requires recovery/resolution?
```

Note that `recoveryRequired` is a classification, not a marker-presence flag: `false` does not imply marker absence — a `locked` or `degraded` target can report marker facts without requiring recovery ([cli-v1.md](cli-v1.md)).

### 13.2 Classify with `deployctl status` before reconstructing anything by hand

The normal next command is:

```bash
deployctl status <environment> --repo-dir . --json
```

Its structured result is the **deployment-control-plane classification** of the target — desired release, observed release, observed bundle digest, environment lock state, unresolved deployment attempt, unresolved recovery, unreadable/degraded evidence:

```text
healthy | out-of-date | drift | not-deployed | locked | recovery-required | degraded
```

Do not normally reconstruct these relationships by manually reading files under `<deployRoot>` — that is what `status` is for. In particular, `degraded` means durable evidence exists but is unreadable and claims nothing else; a healthier state must never be reconstructed by inference from partial reads.

### 13.3 Branch on the classification

```text
deployment fails
      |
      v
inspect deployctl.result/v1
      |
      v
deployctl status <environment> --repo-dir . --json
      |
      +-- state == degraded
      |       repair access/evidence first
      |       do not infer a safe state manually
      |
      +-- state == locked
      |       do not recover, do not remove markers
      |       an operation may still be executing — establish whether it is
      |       (see 13.5 for the exceptional retained-lock remedy)
      |
      +-- recoveryRequired == true
      |       STOP normal deploy/retry
      |       gather application-specific evidence (13.4)
      |       establish a target condition compatible with the next action
      |       use documented rollback / recovery resolve (13.6)
      |       run deployctl status again (13.7)
      |
      +-- safeToRetry == true
              repair the stated pre-execution / infrastructure (or cleanup)
              problem, confirm status reports no higher-priority state,
              then rerun the operation
```

**Precedence.** The blocking states are listed first deliberately: `degraded` and `locked` status, and explicit lock-release failure facts (`lockReleaseFailed` / `stagingLockReleaseFailed`), take precedence over acting on `safeToRetry`. Those facts do not change the classification — but they gate acting on it. `safeToRetry: true` is orthogonal to cleanup facts: it means the same operation may be repeated only **after the stated infrastructure/cleanup problem has been repaired** and `deployctl status` no longer reports a higher-priority blocking state (`degraded` → `locked` → `recovery-required` → the normal desired/observed classification). A rerun into a genuinely held lock is refused, not queued.

The exact precedence between these states is owned by [cli-v1.md](cli-v1.md) and [target-state.md](target-state.md); this section summarizes that contract, it does not redefine it. The two underlying questions remain distinct:

```text
consequential work started?  → target-state risk (attempt/recovery marker; recoveryRequired)
safeToRetry                  → whether re-running the SAME operation can succeed as-is
```

- **Nothing consequential ran** (for example, the target was unreachable before execution): `safeToRetry: true` — fix the cause and re-run.
- **Refused before target contact** — policy gates, invalid evidence, prepared-material verification failures: `outcome: refused`, `safeToRetry: false`. Nothing was contacted or executed, but re-running the same bytes and inputs cannot succeed; fix the material or the state, then re-run.
- **Consequential work started and the outcome is not safely known**: an attempt marker exists, `recoveryRequired: true`, `safeToRetry: false`. This includes *determined* hook failures after the boundary — a migration can partially mutate state and then exit 1, so a known failure is not a safe repeat. Normal deployment refuses to re-run into an unknown target state; never infer retry safety merely from "the target looks unchanged."

### 13.4 Direct target inspection is evidence gathering, not state management

Direct target inspection is appropriate when Deploy Toolkit requires the operator to establish application-specific reality that the toolkit cannot know — for example application health, the artifact/digest actually running, or the state migrations reached.

These checks are **evidence gathering**. They do not replace `deployctl status`, and they must not be used to mutate toolkit-owned deployment state.

The concrete procedures are consumer-specific — container/process inspection, systemd units, application logs, database migration queries, HTTP health checks, reverse-proxy/TLS state, application-specific configuration — and belong in the consuming repository's deployment/recovery documentation, not here. Deploy Toolkit describes **when and why** such checks are needed, never their application-specific implementation.

### 13.5 Do not hand-edit toolkit-owned state

Toolkit-owned deployment evidence is **not** a normal manual recovery interface:

```text
<deployRoot>/<project>/state/<env>.json
<deployRoot>/<project>/attempts/<env>.json
<deployRoot>/<project>/recoveries/<env>.json
<deployRoot>/<project>/history/<env>.jsonl
```

In particular, this is **not** a recovery pattern:

```text
deployment failed
→ SSH to target
→ delete attempt marker
→ retry deployment
```

The supported shape is:

```text
deployment failed
→ inspect result/status
→ establish actual target condition
→ use the supported recovery/resolution mechanism
→ inspect status again
→ begin a fresh operation
```

Manual filesystem intervention remains appropriate **only** in the exceptional cases the contracts themselves define — and these stay distinct from attempt/recovery resolution:

```text
deliberately retained environment lock (data.lockRetained: true)
    → verify by hand that nothing is still executing
    → remove the environment lock by hand
    → resolve the attempt/recovery marker if one exists
    (docs/cli-v1.md, "lockRetained")

a lock whose release failed (lockReleaseFailed: true)
    → the environment stays locked until manual cleanup
    (docs/cli-v1.md)

crashed staging lock (.staging/<version>) or an interrupted stage
directory without its .staged.json marker
    → remove by hand; the fail-closed refusal keeps it from
    masquerading as a complete release
    (docs/target-state.md, docs/cli-v1.md)
```

Everything else waits for the supported mechanisms.

### 13.6 Recovery resolution — the supported boundary

When the classification is `recovery-required`, resolution is explicit:

```bash
deployctl recovery resolve <environment> [recovery <id>] [attempt <id>] \
  --confirm "resolve <environment> [recovery <id>] [attempt <id>]"
```

`recovery resolve` does **not** run hooks, repair the application, prove that the failed deployment completed, or make an unknown target state trustworthy. It records that the operator has **independently established** a target condition compatible with the next action and explicitly authorizes removal of the named blocking evidence ([target-state.md](target-state.md)). "The app responds" is not sufficient inspection: the target must be understood to be in a condition compatible with the next action — typically manually restored to, and verified against, the known observed release.

```text
recovery-required
      |
      v
inspect application-specific reality (13.4)
      |
      v
establish a compatible/safe target condition
      |
      v
deployctl recovery resolve ...
      |
      v
deployctl status <environment> --repo-dir . --json
      |
      v
fresh deployment/recovery operation
```

Resolution itself refuses on a genuinely held lock, unreadable evidence, or identity mismatch with the markers on the target; the result document reports what was authorized and what physically remains (`resolved*` / `left*` / `remaining*` — [cli-v1.md](cli-v1.md)).

### 13.7 Close the loop with `status`

After recovery, marker resolution, or an exceptional manual lock cleanup, rerun:

```bash
deployctl status <environment> --repo-dir . --json
```

before starting the next deployment operation. The consistent cycle is:

```text
toolkit classification
        ↓
application-specific evidence gathering
        ↓
toolkit recovery/resolution
        ↓
toolkit classification
        ↓
fresh operation
```

---

## 14. Automatic rollback

If the environment declares `failurePolicy.autoRollback: safe-only` and the failed release declares `migration.rollbackSafe: true`, Deploy Toolkit may automatically recover the previous verified release. `off` and `rollbackSafe: false` always win; `irreversible` refuses rollback on every authorization path. The policy is owned by [consumer-contract-v1.md](consumer-contract-v1.md) and [release-lifecycle.md](release-lifecycle.md).

---

## 15. Normal rollback of a healthy deployment

A normal rollback is not an emergency command. Promote the previous release again:

```diff
 spec:
-  release: .deploy/releases/acme-service-0.4.0.yaml
+  release: .deploy/releases/acme-service-0.3.2.yaml
```

Then follow the same promotion review and deployment path. This preserves the ordinary authorization model.

---

## 16. Emergency recovery

Use the explicit rollback/recovery paths only when the currently-running environment must be restored outside the normal promotion flow:

```bash
deployctl rollback <environment> --to <version> \
  --confirm "rollback <environment> to <version>"
```

(`--confirm` may be omitted in interactive mode; the same sentence is then typed at the prompt. It is checked before any target contact, and it is required in `--json` mode.)

Emergency rollback records its authority in deployment history and leaves Git desired state drifted until reconciled ([release-lifecycle.md](release-lifecycle.md)).

If a deployment or recovery has an unresolved marker, follow the failure-investigation workflow (§13) — including `deployctl recovery resolve` ([cli-v1.md](cli-v1.md)) — rather than repeatedly invoking deploy/rollback. The toolkit deliberately refuses ambiguous retries.

---

## 17. First deployment checklist

The first deployment usually requires more preparation than later releases. Before promoting the first real release, confirm:

- the target is bootstrapped and reachable — the full checklist is in [bootstrap-ssh-target.md](bootstrap-ssh-target.md);
- target-owned application configuration and secrets exist outside immutable release trees;
- the caller workflow has the required GitHub secrets and permission floor;
- source-SHA discovery-tagged artifacts are published;
- release migration semantics are decided;
- the project's `verify` hook can prove the first deployment;
- the environment points at its documented bootstrap sentinel.

Project-specific bootstrap tasks — creating identity tenants, DNS records, databases, application users, provider credentials, and similar — belong in the consuming repository's documentation. Deploy Toolkit does not absorb them.

---

## 18. Routine release flow after bootstrap

Once the first rehearsal is complete, the normal path is short:

```text
1. merge application change to trusted main
2. wait for required CI and SHA-tagged artifact publication
3. create immutable release
4. propose release to environment
5. promotion check passes
6. human merges promotion PR
7. trigger deployment workflow
8. inspect deployctl.result/v1
9. confirm desired == observed
```

That is the routine operating model.

---

## 19. Responsibility boundary

A useful rule is:

> Deploy Toolkit owns **release identity, authorization, transfer, lifecycle ordering, observed state, rollback/recovery discipline, and deployment evidence**.

The consumer owns:

```text
how the application is built
what artifacts exist
how the application starts
how migrations work
how the application proves health
what secrets it needs
what network/DNS/TLS configuration it needs
what runtime is installed
how infrastructure is provisioned
```

The consumer's documentation also owns the **application-specific failure-evidence procedures** — how to identify the running artifact and verify its digest/version, check application health, establish migration state, and inspect service/process/container logs, proxy/TLS state, and application-specific configuration. Deploy Toolkit's failure workflow (§13) treats these as evidence gathering; it deliberately does not prescribe them.

This keeps Deploy Toolkit usable across unrelated projects.

---

## 20. Quick command reference

Create a release:

```bash
deployctl release create \
  --repo OWNER/REPOSITORY \
  --revision <full-sha> \
  --version <version> \
  --migration-head "<head>" \
  --migration-mode <mode> \
  [--rollback-safe] \
  --repo-dir .
```

Propose it:

```bash
deployctl promotion propose <environment> \
  --release .deploy/releases/<project>-<version>.yaml \
  --repo OWNER/REPOSITORY \
  --repo-dir .
```

Check a promotion:

```bash
deployctl promotion check \
  --repo OWNER/REPOSITORY \
  --base <trusted-base-sha> \
  --head <promotion-head-sha> \
  --repo-dir .
```

Deploy directly from a repository checkout (the reusable workflow instead runs the prepare/deploy-prepared trust split):

```bash
deployctl deploy <environment> --repo-dir .
```

Inspect target state:

```bash
deployctl status <environment> --repo-dir . [--json]
```

Emergency rollback (typed confirmation required):

```bash
deployctl rollback <environment> --to <version> \
  --confirm "rollback <environment> to <version>"
```

---

## Related documentation

Use this document as the practical operator path. Exact semantics are owned by:

```text
docs/consumer-contract-v1.md
docs/cli-v1.md
docs/release-lifecycle.md
docs/promotion-diff-policy.md
docs/prepared-artifact-v1.md
docs/target-state.md
docs/target-prerequisites.md
docs/bootstrap-ssh-target.md
docs/trust-model.md
```

Project-specific deployment instructions belong in the consuming repository.
