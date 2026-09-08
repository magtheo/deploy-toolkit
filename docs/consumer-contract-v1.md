# Consumer Contract v1

> **Status: candidate.** v1 is not frozen yet. It freezes only after (1) this
> contract-hardening pass and (2) a real `platform-core` deployment has
> exercised release generation end to end. Freezing before the reference
> consumer has used the contract would be backwards.

The configuration surface of Deploy Toolkit is an **API**. This document is the
compatibility promise for `deploy.toolkit/v1` and the policy for changing it.

## What is contract

1. The JSON Schemas in [`schemas/`](../schemas/) — Project, Release,
   Environment, Target. Any manifest passing `deployctl validate` today must
   keep passing (same or newer toolkit) under v1.
2. The semantic invariants enforced on top of the schemas (`internal/manifest`
   checks): untagged OCI repository names, `irreversible ⇒ rollbackSafe: false`,
   release-reference and bundle-include path safety.
3. The lifecycle hook protocol — argv semantics, exit codes, working directory
   and environment (below).
4. The GitHub workflow interface — inputs, permissions, and required secrets of
   the published reusable workflows.
5. Consumer pinning — consumers pin full SHAs; that pinning model itself is
   part of the contract.

Everything else (CLI UX, internal Go APIs, server-side layout details) is
implementation and may change.

## Versioning policy

- **Adding** an optional field, a new enum value, or a new lifecycle step is a
  compatible change: bump nothing, document it.
- **Removing/renaming a field, changing a type or pattern, or tightening a
  previously-accepted document** is breaking: it requires `v2`
  (`deploy.toolkit/v2`), accepted alongside v1 for a deprecation window.
- Schema and Go types (`internal/manifest`) move in lockstep; a schema change
  without a type change (or vice versa) is a bug.
- Released toolkit versions are published as readable release notes above an
  immutable full SHA; consumers still pin the SHA.

## Schemas

| Manifest    | File                                        | Written by    |
| ----------- | ------------------------------------------- | ------------- |
| Project     | `.deploy/project.yaml`                      | human         |
| Release     | `.deploy/releases/<project>-<version>.yaml` | `deployctl`   |
| Environment | `.deploy/environments/<env>.yaml`           | human + PR    |
| Target      | `.deploy/targets/<name>.yaml`               | human         |

Start from [`templates/`](../templates/). Never hand-edit a Release manifest.

## Source identity

Source identity is **SCM identity**, never a registry. For v1 the source
adapter is explicitly **GitHub**:

```yaml
release:
  source:
    type: github
    repository: example/my-app    # owner/repo — where the revision lives
    branch: main                  # permitted promotion branch
```

Generated releases carry the same shape:

```yaml
source:
  type: github
  repository: example/my-app
  revision: "4ecd4114647f7dda41d98bc17e50ec027f28fac9"
```

This does not make deployment provider-specific — source provider (GitHub) and
deployment target (generic SSH/local) are separate planes. Later source
adapters (`gitlab`, `forgejo`, …) are earned, not pretended.

## Artifact discovery convention

Eligibility must answer: *which OCI artifact corresponds to source SHA X?*
Candidate v1 uses one simple rule:

> **Every releasable OCI artifact MUST be published with the full source Git
> SHA as a temporary discovery tag.**

Resolution therefore reads `ghcr.io/example/app:<full-source-sha>`, captures
the registry digest, and records only:

```yaml
image: ghcr.io/example/app
digest: sha256:...
```

The SHA tag is an **input to resolution**, never part of the immutable
Release manifest. Once the digest is pinned, the tag is disposable and may be
garbage-collected:

```
mutable/discovery identity    repo:<git-sha>
        ↓ registry resolution
immutable identity            repo@sha256:...
```

## Policy authority: current main vs candidate

For an old commit on `main` there are two versions of `.deploy/project.yaml`:
the candidate's and current main's. They answer different questions:

| Authority              | Source revision      | Decides                                            |
| ---------------------- | -------------------- | -------------------------------------------------- |
| **Eligibility policy** | current trusted main | source repository, permitted branch, requiredChecks |
| **Deployment material**| candidate revision   | artifact repositories, bundle definition, lifecycle, deployment contract |

`release create` reads policy from current `main` and deployment material from
the candidate. Consequence: new security policy (e.g. an added
`requiredChecks` entry) applies to old candidates, while an old application is
never paired with new deployment scripts.

Candidate v1 defines `requiredChecks` as exact **GitHub check-run names** for
the candidate SHA. Any state other than `success` — missing, queued,
in_progress, failure, cancelled, timed_out, skipped, neutral,
action_required — fails eligibility. No "probably fine."

## Path safety rules

- `Environment.spec.release` is a **canonical repo-root-relative** path that
  must resolve within `.deploy/releases/` as a flat file name
  (`.deploy/releases/<project>-<version>.yaml`). Absolute paths, `..`
  traversal, nested directories and non-canonical forms are rejected. There is
  exactly one resolution rule; no per-file relative bases.
- `bundle.include` entries are repo-root-relative globs matched against the
  Git-tracked tree at the release revision — see
  [bundle-format-v1.md](bundle-format-v1.md). Leading `/`, backslashes, `.`
  and `..` segments are rejected; `*`/`**` are allowed only as whole segments.
- OCI `repository`/`image` fields are **untagged repository names**; tags and
  floating references (`latest`) are structurally impossible, releases pin the
  digest separately.

## Lifecycle hook protocol

A Project declares hooks as **argv vectors** (or scripts invoked via argv):

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

Rules (v1 promise):

- Hooks are executed with deterministic argument boundaries; YAML strings are
  **never** evaluated as shell programs.
- `apply` is the only mandatory hook; every other step is optional and skipped
  when absent.
- Hooks run on the target, inside the staged release directory
  (`<deployRoot>/<project>/releases/<version>/` — releases are
  environment-independent; state and history are environment-scoped). See
  `docs/target-state.md`.
- Exit code `0` = success; any other code = failure of the current lifecycle
  stage.
- The hook environment is **exact and deterministic** — ambient runner or
  login environment is never inherited:
  - `PATH` — a fixed toolkit default
    (`/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`);
  - `DEPLOY_PROJECT`, `DEPLOY_ENVIRONMENT`, `DEPLOY_RELEASE_VERSION`,
    `DEPLOY_SOURCE_REVISION` (the pinned full SHA);
  - `DEPLOY_ARTIFACT_<NAME>` — one per release artifact, `<NAME>` being the
    artifact name uppercased with `-`→`_`, value `<image>@<digest>`. Hooks
    are pinned to the exact Release artifacts without copying the Release
    manifest into the bundle;
  - for `rollback` (step 10): the release being rolled back to.
- Lifecycle execution holds a **cross-process, target-scoped environment
  lock** for the whole sequence (atomic `mkdir` on the target; a held lock
  fails closed; no stale-lock breaking — a crashed runner leaves the lock
  for explicit operator removal).
- History records **structured stage outcomes** (stage name, exit code,
  release identity, digests, timestamps) — never raw hook stdout/stderr,
  which may contain application secrets. Raw output is not persisted by
  v0.1; a separate attempt-log mechanism may be added later.

Deploy Toolkit owns the state machine; the project supplies the commands.

```
VALIDATE → STAGE → PREFLIGHT → MIGRATE → APPLY → VERIFY → COMMIT STATE
```

Staging precedes preflight because hooks execute from the staged release
directory — the deployment contract that runs is the one staged, verified
byte-for-byte against `deploymentContract.digest`. On failure: rollback if
safe and permitted (see below), else stop and fail loudly.

## Migration contract

Declared by the release, never inferred:

```yaml
migration:
  head: "043"
  mode: forward-compatible
  rollbackSafe: true
```

| Mode                  | Meaning                                                     |
| --------------------- | ----------------------------------------------------------- |
| `none`                | No schema changes in this release                           |
| `forward-compatible`  | Old code works against new schema; rolling forward is safe  |
| `maintenance-required`| Requires downtime; surfaced prominently in the promotion PR |
| `irreversible`        | Cannot be undone; auto-rollback disabled                    |

`rollbackSafe: false` disables auto rollback regardless of mode.
**`irreversible ⇒ rollbackSafe: false`** is enforced structurally (schema) and
semantically (Go checks) — the combination `mode: irreversible,
rollbackSafe: true` cannot validate.

Migration semantics are declared by the project/release, never inferred.
`forward-compatible` vs `irreversible` is a semantic claim about the
application's database; deployctl must not guess it.

## Target contract

A Target declares only what the toolkit needs: a trusted connection, the
credential environment-variable names, and the absolute root of the subtree
the toolkit owns on that target.

```yaml
spec:
  deployRoot: /srv/deploy       # toolkit-owned subtree root (absolute)
  transport:
    type: ssh
    hostFrom: DEPLOY_HOST        # env var holding the hostname
    port: 22
    user: deploy
    hostKeyFrom: DEPLOY_HOST_KEY # pinned host key — verification is mandatory
    credentialFrom: DEPLOY_SSH_KEY
```

V1 transports: `ssh` and `local`. No provider fields — infrastructure identity
belongs to Pulumi, which may emit a Target descriptor as an output.

`deployRoot` must be absolute and free of traversal segments. Everything the
toolkit stores on the target lives inside it; see `docs/target-state.md` for
the derived layout and the staging/state/history disciplines.

### Target-side utilities (both transports)

The substrate uses only POSIX utilities resolved via the account's PATH:
`test`, `cat` (state reads and staged-marker reads). Nothing else is
required of an SSH or local target in V1.

### SSH target prerequisites (V1)

An SSH target must provide, and the toolkit may rely on:

- a **POSIX-compatible command shell** at the account's login shell;
- an **SFTP subsystem**;
- support for the **`posix-rename@openssh.com`** extension (atomic
  publication of staged files).

The **`fsync@openssh.com`** extension is optional: if the target declares it
unsupported, publication remains atomic and only the durability guarantee is
weakened; any other sync error fails closed.

### Start-failure semantics (both transports)

A target-side *start failure* — the requested program or working directory
cannot be used — is a **transport error** (`transport.StartError`), not a
command result. Local transports observe start failures directly. Over SSH,
exit codes **126** (not usable / failed `cd`) and **127** (program not found)
follow the POSIX dispatch convention and are reported as start failures; a
target command that deliberately exits 126/127 is therefore
indistinguishable from a start failure — a documented protocol limit.

## GitHub workflow interface (planned for consumers)

```yaml
jobs:
  deploy:
    uses: magtheo/deploy-toolkit/.github/workflows/deploy.yml@<full-sha>
    with:
      toolkit_ref: <full-sha>
      environment: production
```

Consumers pin **full SHAs**, never tags or branches. Workflow inputs,
permissions and secret names are contract items; changing them follows the
versioning policy above.
