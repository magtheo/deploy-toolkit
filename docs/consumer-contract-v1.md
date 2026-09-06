# Consumer Contract v1

The configuration surface of Deploy Toolkit is an **API**. This document is the
compatibility promise for `deploy.toolkit/v1` and the policy for changing it.

## What is contract

1. The JSON Schemas in [`schemas/`](../schemas/) — Project, Release,
   Environment, Target. Any manifest passing `deployctl validate` today must
   keep passing (same or newer toolkit) under v1.
2. The lifecycle hook protocol — argv semantics, exit codes, working directory
   and environment (below).
3. The GitHub workflow interface — inputs, permissions, and required secrets of
   the published reusable workflows.
4. Consumer pinning — consumers pin full SHAs; that pinning model itself is
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
  (`/opt/deploy-toolkit/<project>/<environment>/releases/<version>/`).
- Exit code `0` = success; any other code = failure of the current lifecycle
  stage.
- The toolkit sets, at minimum: `DEPLOY_PROJECT`, `DEPLOY_ENVIRONMENT`,
  `DEPLOY_RELEASE_VERSION`, `DEPLOY_SOURCE_REVISION`, and for `rollback`, the
  release being rolled back to.
- Stdout/stderr is captured into the deployment history record.

Deploy Toolkit owns the state machine; the project supplies the commands.

```
VALIDATE → PREFLIGHT → STAGE → MIGRATE → APPLY → VERIFY → COMMIT STATE
```

On failure: rollback if safe and permitted (see below), else stop and fail
loudly.

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

## Target contract

A Target declares only what the toolkit needs: a trusted connection and the
credential environment-variable names.

```yaml
spec:
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
