# Deploying a Release — Operator Workflow

> A practical, project-neutral runbook for moving a tested commit on `main` to a deployment target with Deploy Toolkit.
>
> This document describes **how to operate Deploy Toolkit**. It does not define how an individual application is built, provisioned, configured, or verified internally. Those details belong to the consuming project and its deployment contract.

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

Deploy Toolkit never treats "run the deploy command" as the authorization act. The authorization is the human-approved promotion.

---

## 2. Before you begin

A consumer repository should already contain:

```text
.deploy/
├── project.yaml
├── environments/
│   └── <environment>.yaml
├── targets/
│   └── <target>.yaml
└── releases/
```

The project must also have a deployment contract declaring its lifecycle hooks, for example:

```yaml
lifecycle:
  preflight:
    argv: ["./deploy/preflight.sh"]
  migrate:
    argv: ["./deploy/migrate.sh"]
  apply:
    argv: ["./deploy/apply.sh"]
  verify:
    argv: ["./deploy/verify.sh"]
  rollback:
    argv: ["./deploy/rollback.sh"]
```

`apply` and `verify` are mandatory. Other hooks are optional according to the consumer contract.

Before the first real deployment, the target must satisfy the generic target prerequisites:

- reachable by the declared transport;
- deployment user exists;
- deploy root is writable;
- SSH host key is pinned when using SSH;
- required runtime/tools for the project's hooks are installed;
- registry authentication is configured if private artifacts must be pulled;
- target-owned application secrets/configuration exist outside immutable release directories.

Deploy Toolkit itself does not provision hosts, install Docker, configure DNS, create databases, or create application secrets.

---

## 3. Step 1 — Wait for trusted `main` to be green

Start from an exact commit on the trusted integration branch.

Record the full source SHA:

```bash
git rev-parse origin/main
```

Use the **full 40-character SHA** throughout release creation.

Do not create a release from a PR head unless that exact revision has become part of trusted `main` and satisfies the project's release policy.

The candidate revision must have all required checks completed successfully. Missing, skipped, cancelled, neutral, in-progress, or failed checks are not eligible.

---

## 4. Step 2 — Confirm releasable artifacts exist

For every OCI artifact declared by the project, CI should publish a temporary discovery tag using the full source SHA:

```text
registry.example.com/project/app:<full-source-sha>
registry.example.com/project/worker:<full-source-sha>
registry.example.com/project/proxy:<full-source-sha>
```

The names are project-specific.

These SHA tags are only used to discover the artifact produced from the source revision. Deploy Toolkit resolves each tag to its immutable registry digest:

```text
registry.example.com/project/app@sha256:...
```

Only the digest is recorded in the release manifest.

If the target must pull private artifacts, verify that the deployment user can authenticate to the registry before the first deployment.

---

## 5. Step 3 — Decide the release metadata

Release creation requires explicit migration semantics. Deploy Toolkit deliberately does not infer them.

Choose:

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

Supported migration modes:

| Mode | Meaning |
|---|---|
| `none` | No schema migration is part of this release |
| `forward-compatible` | New schema remains compatible with the previous application version |
| `maintenance-required` | Deployment requires an explicit maintenance window |
| `irreversible` | Migration cannot safely be undone |

`rollbackSafe: false` disables automatic rollback.

`irreversible` must always be paired with `rollbackSafe: false`.

These values are application claims. The operator or project must decide them deliberately.

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

Release creation performs eligibility and materialization. It should verify, among other things:

```text
source revision exists
source revision belongs to the permitted trusted branch
required CI checks succeeded
OCI artifacts resolve from the source-SHA discovery tags
artifact digests are immutable
deployment bundle can be built from the exact source tree
deployment contract can be validated
migration semantics are explicit
```

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

The promotion proposal should contain only the authorized deployment-state change:

```text
new immutable release manifest
+
environment spec.release pointer change
```

Conceptually:

```diff
 spec:
-  release: .deploy/releases/acme-service-0.3.2.yaml
+  release: .deploy/releases/acme-service-0.4.0.yaml
```

For a first deployment, the environment may instead move from the project's bootstrap sentinel to the first real release.

---

## 8. Step 6 — Verify the promotion PR

Before authorization, the promotion diff should be checked by trusted automation.

The promotion check validates that the PR has not smuggled unrelated changes into the authorization step and, for a newly-created release, re-verifies its eligibility evidence.

Conceptually:

```bash
deployctl promotion check \
  --repo OWNER/REPOSITORY \
  --base <trusted-base-sha> \
  --head <promotion-pr-head-sha> \
  --repo-dir .
```

Prefer running this in a trusted CI workflow rather than relying on a developer's local machine.

The promotion should fail closed if the proposal includes unrelated files or if the release evidence no longer matches policy.

---

## 9. Step 7 — Human reviews and merges the promotion PR

This is the authorization boundary.

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

A consumer repository can invoke Deploy Toolkit through the published reusable workflow.

A typical caller looks like:

```yaml
jobs:
  deploy:
    permissions:
      contents: read
      actions: read

    uses: OWNER/deploy-toolkit/.github/workflows/deploy.yml@<full-toolkit-sha>

    with:
      environment: staging
      repo_dir: .

    secrets:
      target_host: ${{ secrets.TARGET_HOST }}
      target_ssh_key: ${{ secrets.TARGET_SSH_KEY }}
      target_host_key: ${{ secrets.TARGET_HOST_KEY }}
```

Always pin the toolkit workflow by a **full commit SHA**.

For a manual deployment workflow, ensure the caller can run only from the trusted branch or otherwise proves that the invoking commit is the promoted state.

The reusable workflow intentionally splits authority:

```text
prepare job
  repository access
  no target credential
        ↓
immutable prepared deployment artifact
        ↓
deploy job
  target credential
  no consumer source checkout
```

No single job should hold both repository authority and the production target credential.

---

## 11. Step 9 — What Deploy Toolkit does on the target

The deployment engine executes:

```text
VALIDATE
   ↓
STAGE
   ↓
PREFLIGHT
   ↓
MIGRATE
   ↓
APPLY
   ↓
VERIFY
   ↓
COMMIT OBSERVED STATE
```

### VALIDATE

Validates the prepared material and manifest relationships.

### STAGE

Copies the immutable deployment bundle into the release directory on the target.

Typical layout:

```text
<deployRoot>/<project>/
├── releases/
│   └── <version>/
├── state/
├── history/
└── secrets/          # project convention; target-owned, not release-owned
```

### PREFLIGHT

Runs the project's preflight hook, if declared.

Examples of project-specific checks:

```text
required runtime exists
required target-owned config exists
required ports/files/directories are usable
artifact variables are present
application prerequisites are satisfied
```

### MIGRATE

Runs the project's migration hook when declared.

Deploy Toolkit does not interpret the application's database migration system; it executes the declared contract and records the result.

### APPLY

Runs the project's apply hook.

This may mean:

```text
docker compose up
systemctl restart
helm upgrade
copy binaries + restart service
or another project-defined mechanism
```

Deploy Toolkit does not require Docker or any particular runtime.

### VERIFY

Runs the project's mandatory verify hook.

This is the authoritative application-level proof that the deployment succeeded.

The project should verify the state that matters to it, for example:

```text
process/service is running
health endpoint responds
database is reachable
TLS edge is valid
critical dependencies are ready
version/release identity is correct
```

### COMMIT OBSERVED STATE

Only after verification succeeds does Deploy Toolkit record the release as the environment's observed state.

---

## 12. Step 10 — Read the deployment result

The reusable workflow exposes a `deployctl.result/v1` result document.

Do not judge deployment state from the shell exit code alone.

Inspect the structured result fields, especially:

```text
outcome
committed
alreadyCurrent
recoveryRequired
safeToRetry
desired release
observed release
attempt identity
recovery identity
lock state
```

A normal successful deployment should end with observed state matching the promoted release.

You can also query the target state with:

```bash
deployctl status <environment> --repo-dir .
```

or machine-readable output:

```bash
deployctl status <environment> --repo-dir . --json
```

---

## 13. Failure handling

Deploy Toolkit distinguishes failures that happened before consequential execution from failures whose target outcome may be uncertain.

### Safe pre-execution failure

Examples:

```text
invalid manifest
prepared-artifact verification failure
target unreachable before execution
preflight rejection
```

These can often be corrected and retried.

### Consequential failure

Once migration/apply/verification work may have changed target state, Deploy Toolkit records an attempt marker.

If the outcome is not safely known, normal deployment refuses to run again blindly.

The structured result may report:

```text
recoveryRequired: true
safeToRetry: false
```

At that point, do not simply re-run deployment.

Inspect the target and use the documented recovery path.

---

## 14. Automatic rollback

If the environment permits:

```yaml
failurePolicy:
  autoRollback: safe-only
```

and the failed release declares:

```yaml
migration:
  rollbackSafe: true
```

Deploy Toolkit may automatically recover the previous verified release.

`rollbackSafe: false` disables this.

An `irreversible` migration cannot be automatically or manually represented as safely reversible.

---

## 15. Normal rollback of a healthy deployment

A normal rollback is not an emergency command.

Promote the previous release again:

```diff
 spec:
-  release: .deploy/releases/acme-service-0.4.0.yaml
+  release: .deploy/releases/acme-service-0.3.2.yaml
```

Then follow the same promotion review and deployment path.

This preserves the ordinary authorization model.

---

## 16. Emergency recovery

Use the explicit rollback/recovery paths only when the currently-running environment must be restored outside the normal promotion flow.

Example:

```bash
deployctl rollback <environment> --to <version>
```

Emergency rollback requires explicit confirmation and records its authority in deployment history.

If a deployment or recovery has an unresolved marker, follow the recovery procedure rather than repeatedly invoking deploy/rollback.

The toolkit deliberately refuses ambiguous retry behavior.

---

## 17. First deployment checklist

The first deployment usually requires more preparation than later releases.

Before promoting the first real release, confirm:

- target exists and is reachable;
- deployment user and deploy root are ready;
- SSH host key is pinned;
- caller workflow has the required GitHub secrets;
- target-owned application configuration exists;
- registry login exists if artifacts are private;
- source-SHA artifacts are published;
- release migration semantics are known;
- project verify hook can prove the first deployment;
- environment currently points at its documented bootstrap state/sentinel.

Project-specific bootstrap tasks belong in the consuming repository's documentation.

Examples include:

```text
creating an identity-provider tenant
provisioning DNS credentials
creating a database
configuring email
creating application users
loading TLS/DNS provider credentials
bootstrapping object storage
```

Deploy Toolkit should not absorb these project-specific concerns.

---

## 18. Routine release flow after bootstrap

Once the first rehearsal is complete, the normal path should be short:

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

Inspect target state:

```bash
deployctl status <environment> --repo-dir .
```

Machine-readable status:

```bash
deployctl status <environment> --repo-dir . --json
```

Emergency rollback:

```bash
deployctl rollback <environment> --to <version>
```

---

## Related documentation

Use this document as the practical operator path. For exact guarantees and deeper semantics, see:

```text
README.md
docs/consumer-contract-v1.md
docs/release-lifecycle.md
docs/promotion-diff-policy.md
docs/prepared-artifact-v1.md
docs/target-prerequisites.md
docs/target-state.md
docs/cli-v1.md
docs/trust-model.md
```

The operator workflow should stay short and project-neutral; project-specific deployment instructions belong in the consuming repository.
