# CLI Machine Interface v1 (`deployctl.result/v1`)

> **Status: frozen.** This document is the compatibility promise for the
> `deployctl` machine interface. The versioning policy is deliberately
> conservative, because consumers build exhaustive switches and state
> machines on the enums below:
>
> | Change                                                         | Compatibility          |
> | -------------------------------------------------------------- | ---------------------- |
> | Adding an optional field                                       | compatible             |
> | Adding a new command                                           | potentially compatible |
> | Adding a value to `outcome`, `status.state`, `lock`, or `stages[].status` | **breaking** — these enums are closed; an unknown value must never enter a v1 automation's switch. Extension requires `v2`. |
> | Removing, renaming, retyping, or tightening accepted documents | **breaking**           |
>
> Breaking changes require `v2` (`deployctl.result/v2`), accepted
> alongside v1 for a deprecation window.

Every operational command — `deploy`, `rollback`, `status`,
`recovery resolve` — speaks two protocols over the same facts:

- **Human mode** (default): progress and guidance on stdout, errors and
  next-step instructions on stderr. *The text is not contract.*
- **Machine mode** (`--json`): exactly one versioned JSON document on
  stdout. *This document is contract.*

## The one-document guarantee

```text
bare --json present
        ↓
EVERY terminal path — success, determined failure, refusal,
uncertainty, infrastructure failure, usage error, unknown flag,
missing flag value, held lock, invalid evidence, dead transport
        ↓
exactly ONE deployctl.result/v1 document on stdout
```

- The document is the last and only thing written to stdout in machine
  mode. Nothing else — no progress, no prose, no blank-line padding.
- Stderr remains free-form diagnostics; it is not contract and may be
  empty or verbose.
- Only the **bare** `--json` flag selects machine mode. `--json=<value>`
  is not the machine interface (Go's boolean parser accepts such tokens,
  but they do not select JSON mode).
- Machine mode is **non-interactive**: a required confirmation must
  arrive via `--confirm`; **stdin is never read**. Selectors choose the
  scope; the confirmation authorizes it — one cannot stand in for the
  other.

## Envelope schema

```jsonc
{
  "schema":              "deployctl.result/v1",  // always
  "command":             "deploy",               // enum, below
  "outcome":             "success",              // enum, below
  "project":             "my-app",               // when known
  "environment":         "production",           // when known
  "recoveryRequired":    false,                  // always
  "safeToRetry":         true,                   // always
  "lockReleaseFailed":   false,                  // (v1 additive; present only when true)
  "stagingLockReleaseFailed": false,             // (v1 additive; present only when true)
  "message":             "...",                  // NON-CONTRACTUAL, below
  "data":                { }                     // per-command shape, below
}
```

| Field               | Presence        | Meaning                                                                                                                                                       |
| ------------------- | --------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `schema`            | always          | Literally `deployctl.result/v1`.                                                                                                                              |
| `command`           | always          | One of the command enum. Same value on every path of that command — never a display string.                                                                   |
| `outcome`           | always          | One of the outcome enum.                                                                                                                                      |
| `project`           | when known      | Absent only on very early usage errors (before the project could be identified).                                                                              |
| `environment`       | when known      | Same rule.                                                                                                                                                    |
| `recoveryRequired`  | always          | The toolkit's **authoritative conclusion** that recovery or resolution is currently required. Raw marker presence is a separate fact, reported in `data.attempt` / `data.recovery` — `false` does **not** imply marker absence, especially while the environment is `locked` or `degraded`. See the semantics section. |
| `safeToRetry`       | always          | `true` **only** when re-running the same command after the infrastructure problem is fixed is known-safe (no consequential work executed). `false` otherwise.  |
| `lockReleaseFailed` | v1 additive; present only when `true` | The invocation could not release its acquired environment lock — the environment stays locked until manual cleanup, **whatever the `outcome`**. Records a fact; it does not change the classification. See `lockRetained` for the deliberate-retention counterpart. |
| `stagingLockReleaseFailed` | v1 additive; present only when `true` | The invocation could not release its staging lock (`.staging/<version>`) — future stages of that release version refuse until manual cleanup, **whatever the `outcome`**. Records a fact; it does not change the classification and does **not** alter `safeToRetry` (retry safety is judged *after* the stated problem — here, the lock removal — is repaired; staging is pre-consequential and idempotent). Distinct from `lockReleaseFailed`: the environment lock is *not* affected. `deploy` and `rollback` only. |
| `message`           | always          | **Explicitly non-contractual.** Human-oriented prose. Never parse it, never branch on it, never assert on it in tests. All machine decisions come from the fields. |
| `data`              | per command     | Command-specific shape. Absent only when there is nothing meaningful to report.                                                                               |

## Command enum

```text
deploy | rollback | status | recovery-resolve | prepare
```

`prepare` (v1 additive) is the artifact-building command of the prepare/
deploy trust split; it never contacts a target.

## Outcome enum and exit codes

```text
success                 → exit 0
failure                 → exit 1
refused                 → exit 1
uncertain               → exit 3
infrastructure-failure  → exit 3
usage-error             → exit 2
```

Exit codes are **broad process categories** — they exist so a process
supervisor can route, not so automation can decide. Decisions are made
from the document, never from the exit code alone.

| Outcome                  | Meaning                                                                                                                                                            |
| ------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `success`                | The requested semantic action completed (including “already current” / “already recovered” / “nothing to resolve”, which are successes with their own data flags).  |
| `failure`                | Determined failure: the lifecycle ran, a stage failed, the outcome is recorded in durable history. `safeToRetry` is `false` unless the failure happened before consequential work. |
| `refused`                | A policy gate declined: unresolved evidence, a genuinely held lock, invalid evidence, identity mismatch, inconsistent coexistence, missing/mismatching confirmation. Nothing was attempted. |
| `uncertain`              | Consequential work **may have executed** and its outcome is not committed. Always `recoveryRequired: true`, `safeToRetry: false`, with the boundary identity (`attemptId` / `recoveryId`) in `data`. |
| `infrastructure-failure` | The toolkit could not establish or complete what it set out to do — connection lost, read failed, pre-execution dispatch failure. Pre-execution failures say `safeToRetry: true`; failures after a committed observation say nothing about retry safety beyond `false`. |
| `usage-error`            | The invocation itself is wrong: unknown flag, missing flag value, bad grammar, missing confirmation, unparseable configuration. Not a target-state fact; `safeToRetry` is `false`. |

## Cross-command semantics

### `recoveryRequired` is a classification, not a presence flag

`recoveryRequired: true` means Deploy Toolkit can **authoritatively
conclude** that recovery or resolution is currently required: an
attempt or recovery marker is unresolved and the toolkit has read it,
or a resolution left one behind. Raw marker presence is reported
separately in `data.attempt` / `data.recovery`.

`recoveryRequired: false` does **not** imply marker absence. Two states
make this deliberate:

- `status` `state: "locked"` — an operation may be executing, and its
  live attempt marker is expected. Starting recovery because a marker
  exists would be exactly wrong; the only safe instruction is to wait.
  `recoveryRequired` stays `false` while the marker fact is reported in
  `data.attempt`.
- `status` `state: "degraded"` — evidence exists but is unreadable; the
  toolkit claims **nothing**, including neither requiring nor clearing
  recovery.

Machine consumers must reason from the command-specific `data` plus the
envelope, never from one boolean in isolation. And in no case does
`recoveryRequired: false` mean “the environment is safe”.

### Durable boundary identity

Two distinct things can happen after the durable boundary is crossed
(the attempt/recovery marker written) and before the outcome is
committed:

- **Determined stage failure** — a lifecycle hook exited nonzero and the
  engine recorded the outcome in durable history. The outcome is
  `failure`, with `recoveryRequired: true` and the boundary identity in
  `data`.
- **Infrastructure failure** — the transport died or the toolkit lost
  the ability to know. The outcome is `uncertain`, always
  `recoveryRequired: true`, `safeToRetry: false`.

In both cases `data` carries the identity needed for recovery:
`attemptId` (deploy) or `recoveryId` + `recoveryStarted` (rollback).
Automation must treat post-boundary `failure` and `uncertain` alike as
“find the marker, resolve deliberately” — never as “rerun and
see”.

### `lockRetained` (v1 additive extension)

Deploy and rollback may emit `data.lockRetained: true`. Per the
versioning policy this is a **compatible** addition (an optional
field); v1 automations that do not know it see it as absent.

```text
lockRetained = true

The invocation deliberately did NOT release its acquired
environment lock because a lifecycle hook's execution fate
could not be established (context cancellation, transport
loss mid-run, or a bounded pipe wait). The hook process may
still be running.
```

It is a **controlled crash**: the environment lock — the mechanism
that already makes a controller crash fail closed — is deliberately
left in place, so no second operation can start while the old hook's
process may still exist. No transport can prove that a process, let
alone a descendant tree, has stopped; the toolkit never claims it.

The remedy is always the same, in this order:

```text
verify the target by hand (nothing still executing)
        ↓
remove the environment lock by hand
        ↓
resolve the attempt/recovery marker if one exists
(recoveryRequired carries that fact)
```

`lockRetained` may accompany either classification of unknown hook
fate:

| Situation                                             | Outcome                  | `recoveryRequired` | `safeToRetry` | `lockRetained` |
| ----------------------------------------------------- | ------------------------ | ------------------ | ------------- | -------------- |
| hook fate unknown before the durable attempt boundary | `infrastructure-failure` | `false`            | `false`       | `true`         |
| hook fate unknown after the boundary                  | `uncertain`              | `true`             | `false`       | `true`         |

`lockRetained` is deliberately **not** set when a lock-release attempt
failed — that is a different situation and surfaces as the envelope
fact `lockReleaseFailed: true` (joined with the operation's own
outcome, never changing its classification). The two facts are
mutually exclusive:

```text
lockRetained      = the lock was deliberately NOT released
                    (a hook's execution fate is unknown)
lockReleaseFailed = the lock release was attempted and failed
                    (manual cleanup required)
```

The staging lock has the same release-failure shape: when
`.staging/<version>` cannot be removed after staging, the envelope
carries `stagingLockReleaseFailed: true` (joined with the operation's
own outcome, never changing its classification). A leftover staging
lock makes every future stage of that version refuse, exactly like a
crashed stager's lock.

A `resolution` whose markers were removed and whose lock release then
failed reports `outcome: infrastructure-failure` with
`lockReleaseFailed: true` and `recoveryRequired: false` — the block is
down; only cleanup failed. When markers remain, `recoveryRequired` is
`true` and `data.remaining*Id` names exactly what survives.

### Evidence taxonomy (recovery resolve preflight)

```text
absent evidence                 → normal (nothing to resolve)
invalid durable evidence        → refused  (exit 1) — exists, fails
                                              validation; repair, don't resolve
cannot read evidence / lock     → infrastructure-failure (exit 3)
genuinely held lock             → refused  (exit 1)
```

The refusal documents carry the same marker facts the engine would
report: what is on the target remains on the target.

## Prepared-material verification refusals

`deploy-prepared`, `rollback-prepared` and `recovery resolve --prepared`
verify the artifact **before any target contact**. Any verification
failure — digest mismatch, unknown schema, identity mismatch, truncated
artifact — is `refused` (exit 1), `safeToRetry: false`: the material is
the problem, nothing was contacted, nothing was executed, and retrying
with the same bytes cannot succeed. Flag/grammar errors on these
commands remain `usage-error` (exit 2); target connection failures keep
the standard classification.

## Per-command `data` shapes

Fields marked *(always)* are always emitted. Others use `omitempty`:
absent means unset/zero, which for booleans means `false`.

### `deploy` and `rollback` — the `invocation` field

*(v1 additive; present only when `"prepared"`)* The operation ran from a
verified prepared artifact (`prepared.deployment/v1`): the deploy side
re-verified every binding before target contact and used **no repository
checkout**. Absent for the direct repository-backed invocation.

### `prepare`

```jsonc
{
  "preparedDir":    "...",           // artifact directory written
  "releasePath":    ".deploy/releases/my-app-1.0.0.yaml",
  "bundleDigest":   "sha256:...",
  "contractDigest": "sha256:...",
  "sourceRevision": "<full 40-hex sha>"
}
```

Prepare failures are `usage-error` (exit 2): the repository state is the
problem — nothing was contacted and nothing was written. Filesystem write
failures are `infrastructure-failure`.

### `deploy`

```jsonc
{
  "committed":            false,  // (always) observed state committed
  "alreadyCurrent":       false,  // (always) requested release already current
  "consequentialStarted": false,  // (always) durable attempt marker written
  "lockRetained":         false,  // (v1 additive; present only when true)
  "attemptId":            "...",  // boundary identity, when written
  "version":              "1.0.0",
  "bundleDigest":         "sha256:...",
  "failureReason":        "...",  // determined failure cause, when any
  "stages":               [ { "name": "preflight", "status": "ok", "exitCode": 0 } ]
}
```

`stages[].status` is one of `ok | failed | skipped | infrastructure-error`.
`stages[].name` is the fixed lifecycle role declared in the consumer's
`project.yaml` (`preflight`, `migrate`, `apply`, `verify`, `rollback`) —
the consumer defines the argv *under* each role, not the role set. Stage
names are non-contract (see below); the `status` enum is not.

### `rollback`

```jsonc
{
  "committed":        false,  // (always) restored state committed
  "alreadyRecovered": false,  // (always) recovery already committed; nothing executed
  "recoveryStarted":  false,  // (always) durable recovery marker written
  "lockRetained":     false,  // (v1 additive; present only when true)
  "recoveryId":       "...",  // boundary identity, when written
  "fromVersion":      "2.0.0",
  "toVersion":        "1.0.0",
  "failureReason":    "...",
  "stages":           [ ]
}
```


### `status`

```jsonc
{
  "state":    "out-of-date",  // enum, below
  "desired":  { "version": "1.0.0", "bundleDigest": "sha256:..." },
  "observed": { "version": "0.9.0", "bundleDigest": "sha256:...",
                "since": "...", "operationId": "..." },   // null when not deployed / unreadable
  "lock":     "free",         // free | held | unreadable
  "attempt":  { "present": false },
  "recovery": { "present": false }
}
```

`state` is a closed enum:

```text
healthy | out-of-date | drift | not-deployed | locked | recovery-required | degraded
```

`degraded` means durable evidence exists but is unreadable; it claims
nothing else. Marker facts (`attempt`, `recovery`) carry `present`,
`unreadable` (reason when unreadable), `id`, `fromRelease`, `toRelease`,
`startedAt`, `authorization` — each optional, each a fact, never an
inference.

### `recovery-resolve`

```jsonc
{
  "nothingToResolve":    false,
  "resolvedRecoveryId":  "...",  // removed by this resolution
  "resolvedAttemptId":   "...",
  "leftRecoveryId":      "...",  // deliberately left by a partial authorization
  "leftAttemptId":       "...",
  "remainingRecoveryId": "...",  // what PHYSICALLY remains after the operation
  "remainingAttemptId":  "...",
  "historySeq":          7,
  "observed":            { "version": "...", "bundleDigest": "...", "operationId": "..." }
}
```

`remaining*Id` is driven by the engine's post-operation presence facts —
never a restatement of read-time marker ids:

- a fully successful resolution exposes **no** `remaining*Id`;
- the attempt-cleared / recovery-clear-failed crash window exposes
  **only** `remainingRecoveryId`;
- a partial authorization shows `left*` (the deliberate leftover) and
  `remaining*` (the same marker, physically) together.

`left*`/`resolved*` name what the operator authorized;
`remaining*` names what is on the target. They can disagree only when
something failed — which is exactly when the distinction matters.

## Invocation grammar

```text
deployctl deploy <env> [flags]
deployctl prepare <env> --out <dir> [--release <version>] [flags]
deployctl deploy-prepared --prepared <dir> [--environment <name>] [flags]
deployctl rollback-prepared --from <dir> --to <dir> [--environment <name>] [flags]
deployctl rollback <env> --to <version> --confirm "rollback <env> to <version>" [flags]
deployctl status <env> [flags]
deployctl recovery resolve <env> [recovery <id>] [attempt <id>] --confirm "..." [flags]
deployctl recovery resolve --prepared <dir> [selectors] --confirm "..." [flags]
```

- The confirmation sentence is exactly the canonical sentence for the
  scope being authorized. In JSON mode `--confirm` is **required**.
- Selector/confirmation scope mismatches are `refused` (exit 1);
  grammar errors are `usage-error` (exit 2). Grammar is validated
  before any target access.
- Flags and positional selectors may be interleaved; both
  `--flag value` and `--flag=value` forms are accepted for value flags.
- One argument lexer owns flag arity **and** machine-mode detection:
  a value flag consumes the following token as its value only when that
  token is not flag-shaped. A flag-shaped token after a value flag is a
  missing value — a `usage-error` that executes nothing, in both
  `--owner --json` and `--json --owner` orders. A bare `--json` can
  therefore never be consumed as a value while simultaneously selecting
  machine mode; audit identities and confirmations can never silently
  become `--json`.

## Target connection contract

For SSH targets, `credentialFrom` and `hostKeyFrom` name **environment
variables** whose values are **file paths** (private key, pinned host
key line). Missing or unparseable material is a configuration error —
`usage-error` (exit 2), nothing executed. An unreachable target is
infrastructure — exit 3, explicitly framed as *did not start*: no
lifecycle operation was executed, `safeToRetry: true`.

Host keys are always pinned; the toolkit never disables strict host
verification.

## What is not contract

- Human-mode output of every kind.
- `message` text, in both modes.
- Stage `name` values (consumer-defined hook names).
- Error prose embedded in any field beyond the structured causes named
  above (`failureReason`, `unreadable`).
- Flag aliases, output ordering, whitespace, and indentation of the
  JSON document.
