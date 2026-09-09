# Trust Model

Deploy Toolkit's trust boundaries are the product. Every mechanism below exists
because authorization and execution must not be able to corrupt each other.

## The three-stage discipline

Inherited from CI Toolkit's layered model (deterministic gates → advisory AI →
human judgment):

```
1. deterministic eligibility
       ↓
2. human promotion
       ↓
3. deterministic deployment
```

**AI never decides that production changes.** AI may explain a release,
summarize changes, or highlight risk — advisory only, never merge authority,
never deployment authority.

## Promotion Diff Policy

A promotion PR may only change two things — and only in semantically
constrained ways (path-level allowlisting alone would still permit a target
swap inside an allowed file):

- one **added** immutable release file (`.deploy/releases/<project>-<version>.yaml`,
  filename agreeing with its contents), or none if promoting an existing release;
- one environment file in which **only `spec.release` may differ** from the
  trusted base — never `target`, `failurePolicy` or identity.

Everything else must be byte-for-byte unchanged, including all existing
release files. The change is a compare-and-swap on the environment's current
release: if the trusted base has moved, the proposal is stale, fails the
check, and must be regenerated.

Full rules: [promotion-diff-policy.md](promotion-diff-policy.md). The check
executes from trusted code — the pinned toolkit workflow — never from
PR-controlled code, and requires no production secrets. Rationale: the
authorization PR must not be able to modify the machinery executing the
deployment.

## The deployment contract comes from the promoted release

Deploying revision `abc123` uses:

```
.deploy/project.yaml @ abc123
deploy/**            @ abc123
config/**            @ abc123
```

Never `main`'s current state. If it did, an attacker (or an accident) could
pair an old application with a new deployment procedure — a version-skew bug
class. Application and its deployment semantics are one immutable unit.

## Two trust stages in CI

```
prepare — no production secrets
  verify release, checkout promoted SHA, build bundle, verify checksums

deploy  — only the target connection credential
  download bundle, verify digest, SSH, stage/apply/verify, record
```

The deploy job never checks out application source. Application secrets never
enter GitHub; they live on the target, outside the release.

## Strict host verification

The target pins its SSH host key (`hostKeyFrom` in the Target manifest). Deploy
Toolkit verifies:

- **GitHub → server** authentication (the SSH credential), and
- **server → expected identity** (the pinned host key).

`StrictHostKeyChecking=no` and equivalents must never appear — an attacker who
can intercept DNS/network traffic must not be able to receive deployment
credentials.

## Pinned consumption

Consumers will reference this repository's workflows and binary by **full
commit SHA**. The reusable workflows (including `deploy.yml`) are planned, not
published yet — until they exist, consumers run `deployctl` directly and own
the wiring themselves:

```
uses: <org>/<toolkit>/.github/workflows/deploy.yml@<full-sha>   # planned
```

Readable release tags (`v0.4.0 → 12ab...`) are documentation above an
immutable SHA, never the pin itself. A mutable ref would let a compromised
toolkit repository push code into every consumer's secret-bearing jobs.

## Release eligibility is deterministic

Before a promotion PR can be generated, the release must pass deterministic
eligibility:

- revision exists and is an **ancestor of** the configured branch head — mere
  existence is not enough, a commit on an unmerged feature branch also exists;
- every required GitHub check for **that exact SHA** concluded successfully;
  missing/queued/in-progress/failure/cancelled/timed_out/skipped all fail
  closed;
- all declared artifacts resolved via the SHA discovery tag and pinned to
  immutable digests;
- bundle built and digested from the exact Git tree at the revision;
- manifest validates against the contract schemas.

CI Toolkit's AI review, where present, remains advisory evidence — it gates
nothing here.

## Policy authority: current main vs candidate

Two versions of `.deploy/project.yaml` can exist for an old candidate: the
candidate's and current main's. Authority is split:

- **current trusted main** owns release *eligibility policy* — source
  repository, permitted branch, `requiredChecks`;
- **the candidate revision** owns *deployment material* — artifact
  repositories, bundle definition, lifecycle, deployment contract.

Hardened security policy therefore applies to old candidates (you cannot
promote your way around a newly added required check), while the old
application is never paired with new deployment scripts.

## Rollback policy

- Normal rollback: the same promotion machinery, reverse diff, human merge.
- Emergency rollback: explicit manual workflow, immediate deploy, then an
  automatic reconcile PR restoring Git desired state.
- Auto rollback after a failed deploy only when `migration.rollbackSafe` is
  true and the environment's `failurePolicy.autoRollback` permits it;
  otherwise stop and fail loudly.
