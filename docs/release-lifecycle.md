# Release Lifecycle

From source commit to verified production state.

## The pipeline

```
Developer / agent
       │
       ▼
feature branch → Pull Request
       │
CI (deterministic) + CI Toolkit AI review (advisory)
       │
HUMAN MERGE ──► main
       │
       ▼
build + test + publish  (SHA-tagged artifacts)
        │
        ▼
deployctl release create --revision <sha>
        │  deterministic eligibility
        ▼
deployctl promotion propose <env> --release <version>
        │
promotion PR
        │
HUMAN MERGE = authorize
        │
        ▼
Deploy Toolkit deploys
```

## Release creation and promotion proposal are separate commands

**`deployctl release create --revision abc123`** owns eligibility, artifact
resolution, bundle construction and the release manifest. It never opens PRs
and never changes an environment:

1. revision exists and belongs to permitted `main`;
2. every required check for that exact SHA concluded successfully
   (missing/in-progress/cancelled fail closed);
3. artifact digests resolved from the registry (repository + immutable digest);
4. deployment bundle built and digested per
   [bundle-format-v1.md](bundle-format-v1.md);
5. migration semantics read from explicit inputs — never inferred;
6. release manifest generated and validated.

**`deployctl promotion propose production --release 0.1.17`** owns the
environment change and the human gate:

7. environment file updated to the new release;
8. promotion PR opened.

The split is deliberate vocabulary: *creation* produces an immutable release;
*proposal* is the human-facing authorization act. The semantic version and
migration safety are explicit inputs to `release create`, not inventions of
`deployctl`.

## Eligibility architecture (bounded)

`release create` is decomposed, not one big package — GitHub is the only
candidate-v1 SCM, so there is a clean GitHub client boundary and no generic
adapter registries:

```
internal/
├── github/    client, revision ancestry, required checks
├── oci/       digest resolution via the SHA discovery tag
├── bundle/    builder + digest (bundle-format-v1)
└── release/   eligibility + create
```

The result is structured data, not prose — the promotion PR in the next step
renders this report without reinterpreting what happened:

```go
type EligibilityReport struct {
    SourceRevision string
    BranchHead     string
    Checks         []CheckResult
    Artifacts      map[string]ResolvedArtifact
    BundleDigest   string
    ContractDigest string
}
```

```
$ deployctl release create --revision abc123

✓ source revision exists
✓ revision belongs to main
✓ required CI passed
✓ app artifact found
✓ orchestrator artifact found
✓ workspace artifact found
✓ bundle constructed
✓ manifest validated

Created:
.deploy/releases/platform-core-0.1.17.yaml

$ deployctl promotion propose production --release 0.1.17

Opened:
deploy: promote platform-core 0.1.17 to production
```

## Promotion PR

The human gate. Machine-generated, boring, and read like a changelog:

```
Deploy platform-core 0.1.17

Source              abc123def456
Current production  0.1.16 (89123ab)
Proposed            0.1.17 (abc123d)

Changes since production
  #118 payment retry hardening
  #121 classroom reconnect fix

Artifacts
  app               ✓ sha256:...

Verification
  Typecheck         PASS
  Tests             PASS
  CVE scan          PASS

Database
  migration head    041 → 043
  mode              forward-compatible
  rollback safe     yes
```

Diff policy: only `.deploy/releases/<new>.yaml` and
`.deploy/environments/<env>.yaml` may change — anything else fails the
promotion check (see [trust-model.md](trust-model.md)).

**Merge = authorize production.**

## Deployment state machine

```
VALIDATE → STAGE → PREFLIGHT → MIGRATE → APPLY → VERIFY → COMMIT STATE
```

Staging comes first because the lifecycle contract itself comes from the
staged release: hooks execute inside
`<deployRoot>/<project>/releases/<version>/`, which only exists as a
complete, usable release after `Stage` succeeds. Staging is non-impacting
preparation; preflight decides whether that staged release may begin
consequential execution.

The toolkit owns the state machine; the project's hooks supply the commands
(see [consumer-contract-v1.md](consumer-contract-v1.md)).

Structured stage outcomes are recorded in the deployment history
(`history.jsonl` on the target) and reported as a GitHub Deployment status —
an audit/UI projection, never the source of truth. Raw hook stdout/stderr
is returned to the caller for diagnosis but never persisted into history.

### Uncertain and unreconciled outcomes: the attempt marker

A transport loss during `migrate`/`apply`/`verify` — or a failed
observed-state commit — leaves the attempt's outcome **unknown**: the
service may already run the new release. The lifecycle layer persists an
environment-scoped attempt marker before the first consequential stage;
the marker means *a consequential deployment has not been reconciled to
trusted observed state*. It is removed only by observed-state commit or
explicit recovery — **not** by a hook failure, because a migration can
partially apply and then exit 1: known failure is not safe-to-repeat. A
deployment that ends while the marker exists cannot be retried normally:
the next `Deploy` **refuses** with a recovery-required outcome instead of
re-running migrations or applies into a target whose state is unknown.
Resolution is explicit — the rollback operation, bound to the recorded
attempt's identities — with one self-healing case: committed observed state matching exactly the
requested release and digest proves the attempt finished, and the leftover
marker is cleared. Failures before the marker exists (validate, stage,
contract verification, preflight) involved no consequential work and stay
retryable. See `docs/target-state.md`.

Evidence truthfulness is part of this contract: a stage that produced no
hook outcome is recorded as an infrastructure error without an exit code,
and staging that was never attempted is recorded as `not-attempted` —
history never manufactures facts.

### Failure handling

```
failure (post-consequence: attempt marker exists)
   │
   ├── auto policy permits (env safe-only + failed release rollbackSafe)
   │       → automatic recovery rollback of the failed release
   │       → verify restored release → commit observed state
   │
   └── otherwise
           → STOP + FAIL LOUDLY (environment stays recovery-required)
```

`migration.rollbackSafe: false` or `failurePolicy.autoRollback: off` always
wins: no silent undo of an unsafe state. `migration.mode: irreversible`
refuses rollback for every authorization path. Failures before the attempt
marker exists (validate, stage, contract verification, preflight) did no
consequential work and stay retryable.

## Rollback and recovery

Rollback is a **distinct lifecycle operation**, not a call to normal
Deploy. After a failed `A → B`, observed state may still say `A` while
production partly or fully runs `B`; normal Deploy's idempotency guard
would answer "already-current" and never restore `A`. Rollback forces the
known previous release back and verifies it, holding the same environment
lock for the whole recovery.

Three authorization paths share one engine (recorded as evidence):

- **Recovery** — resolving a recorded unresolved attempt. The failed
  release must equal `attempt.toRelease` (+ pinned bundle digest); the
  restored release must equal `attempt.fromRelease` and committed observed
  state.
- **Auto** — pre-authorized by `failurePolicy.autoRollback: safe-only`
  plus `rollbackSafe: true` on the failed release; also requires an
  unresolved attempt (nothing to undo otherwise). `off` and
  `rollbackSafe: false` always win; `irreversible` is refused for every
  path.
- **Manual/emergency** — explicit operator invocation; may act without an
  unresolved attempt (e.g. production healthy on B but must return to A).
  A recovery marker is created before the first consequential stage, so
  the emergency rollback itself is crash-safe.

Hook ownership follows responsibility: the failed release's staged
contract supplies its optional `rollback` hook (run only when its
migration mode is not `none`) and the rollback-safety claims; the restored
release's staged contract supplies `preflight`, `apply`, and the mandatory
`verify`. The restored release's forward `migrate` is never run as an undo
mechanism.

Every recovery writes its own **recovery marker** before the first
consequential stage — a failed recovery is not blindly retried: the marker
survives any failure (a rollback's own outcome can be unresolved too, and
repeating a rollback hook or an apply is no safer than repeating a failed
migration), and the next ordinary recovery refuses until an operator
resolves it explicitly. Observed-state commits are signed with the
committing operation (`operationId`), so a recovery that committed and
then lost the marker cleanup self-heals on the next invocation — with
proof, and without executing anything. Normal Deploy refuses while a
recovery is unresolved. See `docs/target-state.md` for the marker model.

Auto/manual rollbacks that leave Git desired state pointing at the failed
release produce structured evidence (`rollback.succeeded`/`rollback.failed`
with from/to identities and the recovery id); the reconciliation PR is
step 11 (GitHub wiring), not the lifecycle engine.

**Normal** rollback of a *healthy* deployment remains ordinary promotion
with a reverse diff:

```diff
 spec:
-  release: .deploy/releases/platform-core-0.1.17.yaml
+  release: .deploy/releases/platform-core-0.1.16.yaml
```

PR title: `deploy: roll production back to 0.1.16`. Human merges; the toolkit
applies it. Nothing special.

**Emergency** rollback skips the gate when production is broken:

```
deployctl rollback production --to 0.1.16
```

via an explicit manual workflow that requires typed confirmation. It deploys
the previous known release immediately, leaving Git desired state temporarily
drifted — then automatically opens `deploy: reconcile production after
emergency rollback` to restore the invariant.

## Truth and projections

```
Git desired state          authoritative intent
Server observed state      authoritative observation
GitHub Deployments         audit / UI projection
```

## Implementation sequence

1. ~~Schemas (Project, Release, Environment, Target) + `deployctl validate`~~ **done**
2. ~~Contract hardening: single parse pipeline, source identity, untagged OCI names, semantic invariants, CI~~ **done**
3. ~~Release eligibility + immutable artifact resolution (`release create`)~~ **done**
4. ~~Generated promotion PR flow (`promotion propose` + Promotion Diff Policy)~~ **done**
5. ~~`local` transport (deterministic integration tests)~~ **done**
6. ~~SSH transport (strict host verification)~~ **done**
7. ~~Server-side staging, observed state, history~~ **done**
8. ~~Lifecycle execution + verification~~ **done**
9. ~~Rollback/recovery engine (recovery + auto + manual authorization paths)~~ **done**
10. `platform-core` integration, then `examples/static-site`
11. GitHub wiring: reconcile PRs after auto/emergency rollback, workflow + CLI surfacing
