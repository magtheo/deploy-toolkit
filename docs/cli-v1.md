# CLI Machine Interface v1 (`deployctl.result/v1`)

> **Status: frozen.** This document is the compatibility promise for the
> `deployctl` machine interface. It follows the same versioning policy as
> [Consumer Contract v1](consumer-contract-v1.md): adding optional fields or
> new enum values is compatible; removing, renaming or retyping anything
> below, or tightening a previously-accepted document, requires `v2`
> (`deployctl.result/v2`), accepted alongside v1 for a deprecation window.

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
  "schema":           "deployctl.result/v1",  // always
  "command":          "deploy",               // enum, below
  "outcome":          "success",              // enum, below
  "project":          "my-app",               // when known
  "environment":      "production",           // when known
  "recoveryRequired": false,                  // always
  "safeToRetry":      true,                   // always
  "message":          "...",                  // NON-CONTRACTUAL, below
  "data":             { }                     // per-command shape, below
}
```

| Field               | Presence        | Meaning                                                                                                                                                       |
| ------------------- | --------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `schema`            | always          | Literally `deployctl.result/v1`.                                                                                                                              |
| `command`           | always          | One of the command enum. Same value on every path of that command — never a display string.                                                                   |
| `outcome`           | always          | One of the outcome enum.                                                                                                                                      |
| `project`           | when known      | Absent only on very early usage errors (before the project could be identified).                                                                              |
| `environment`       | when known      | Same rule.                                                                                                                                                    |
| `recoveryRequired`  | always          | The environment has an unresolved attempt/recovery marker, or a resolution left one. See the semantics section — **`false` never means “safe” by itself**.     |
| `safeToRetry`       | always          | `true` **only** when re-running the same command after the infrastructure problem is fixed is known-safe (no consequential work executed). `false` otherwise.  |
| `message`           | always          | **Explicitly non-contractual.** Human-oriented prose. Never parse it, never branch on it, never assert on it in tests. All machine decisions come from the fields. |
| `data`              | per command     | Command-specific shape. Absent only when there is nothing meaningful to report.                                                                               |

## Command enum

```text
deploy | rollback | status | recovery-resolve
```

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

### `recoveryRequired: false` does not mean “safe”

The field means exactly one thing: *the environment carries an unresolved
attempt or recovery marker, or a resolution left one.* It does not
assert that the observed state is correct, current, or healthy. In
particular `status` `state: "degraded"` (evidence unreadable) claims
**nothing** — even though `recoveryRequired` is `false`. Machine
consumers must reason from the command-specific `data` plus the
envelope, never from one boolean in isolation.

### Durable boundary identity

When a deploy or rollback crosses the durable boundary (its marker
written) but does not commit, the outcome is `uncertain` and `data`
carries the identity needed for recovery: `attemptId` (deploy) or
`recoveryId` + `recoveryStarted` (rollback). Automation must treat
`uncertain` as “find the marker, resolve deliberately” — never as
“rerun and see”.

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

## Per-command `data` shapes

Fields marked *(always)* are always emitted. Others use `omitempty`:
absent means unset/zero, which for booleans means `false`.

### `deploy`

```jsonc
{
  "committed":            false,  // (always) observed state committed
  "alreadyCurrent":       false,  // (always) requested release already current
  "consequentialStarted": false,  // (always) durable attempt marker written
  "attemptId":            "...",  // boundary identity, when written
  "version":              "1.0.0",
  "bundleDigest":         "sha256:...",
  "failureReason":        "...",  // determined failure cause, when any
  "stages":               [ { "name": "preflight", "status": "ok", "exitCode": 0 } ]
}
```

`stages[].status` is one of `ok | failed | skipped | infrastructure-error`.
`name` values come from the consumer's own `project.yaml` hook names —
the *shape* is contract, the values are the consumer's.

### `rollback`

```jsonc
{
  "committed":        false,  // (always) restored state committed
  "alreadyRecovered": false,  // (always) recovery already committed; nothing executed
  "recoveryStarted":  false,  // (always) durable recovery marker written
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
deployctl rollback <env> --to <version> --confirm "rollback <env> to <version>" [flags]
deployctl status <env> [flags]
deployctl recovery resolve <env> [recovery <id>] [attempt <id>] --confirm "..." [flags]
```

- The confirmation sentence is exactly the canonical sentence for the
  scope being authorized. In JSON mode `--confirm` is **required**.
- Selector/confirmation scope mismatches are `refused` (exit 1);
  grammar errors are `usage-error` (exit 2). Grammar is validated
  before any target access.
- Flags and positional selectors may be interleaved; both
  `--flag value` and `--flag=value` forms are accepted for value flags.

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
