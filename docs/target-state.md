# Target state: staging, observed state, history

This document specifies the toolkit-owned substrate on a deployment target:
how releases are staged, how observed state is stored, and how history is
recorded. It sits **below lifecycle orchestration** — the state machine that
runs `preflight`/`migrate`/`apply`/`verify` consumes these mechanisms and
owns the transitions between them. Nothing here executes hooks or advances
state on its own.

The root of everything is the Target manifest's `spec.deployRoot` (absolute,
validated). Every path below is a pure function of validated identities —
project, environment, version — never of time, sequence numbers, or
configuration:

    <deployRoot>/<project>/releases/<version>/    staged release trees
    <deployRoot>/<project>/state/<env>.json       observed state snapshot
    <deployRoot>/<project>/history/<env>.jsonl    durable history log
    <deployRoot>/<project>/attempts/<env>.json    unresolved-attempt marker
    <deployRoot>/<project>/.locks/<env>/          environment lock

## Release staging

`Target.Stage` materializes a release's canonical bundle (see
`docs/bundle-format-v1.md`) as `<releases>/<version>/` on the target.
Enforced in this order:

1. **Digest before bytes.** The bundle is hashed client-side and must match
   the release's pinned `bundle.digest` before *anything* is written to the
   target. A mismatch is a broken artifact, not a staging condition — it is
   refused and leaves no trace.

2. **Immutable once staged.** A release directory is complete if and only if
   it contains `.staged.json` (the staged marker). If the directory exists:
   - marker present, same bundle digest → **idempotent** (`already-staged`),
     nothing is written;
   - marker present, different digest → **refused**. The same release
     identity with different bytes must never silently replace a staged
     release — release directories are immutable;
   - marker absent or unreadable → **refused as an interrupted stage**. The
     directory is left untouched; an operator removes it manually. Fail
     closed beats clever cleanup: an interrupted stage can never masquerade
     as a complete release, and the toolkit never silently completes,
     overwrites, or deletes one.

3. **Marker last.** Bundle files are uploaded first (each one atomically,
   via the transport's temp+rename `Put`); the marker — recording schema,
   project, version, bundle digest and staged time — is written last. The
   marker's presence is the atomic commit of the stage. Every reader of a
   release directory checks the marker first.

4. **Defensive extraction.** The bundle is digest-verified, but staging
   still refuses anything a filesystem could misinterpret: absolute entry
   paths, `.`/`..` segments, and any non-regular entry. Bundle Format v1 is
   regular-files-only (the builder refuses symlinks at construction, when
   the release is still mutable), so a non-file entry in a bundle means the
   bytes were tampered with or built by something that ignored the format.

5. **Concurrent stages of the same version** with identical bytes converge
   to the same outcome (idempotent). Stages with conflicting bytes for one
   version are an operator error that step-9's single-flight-per-environment
   execution prevents; the marker discipline above contains the damage.

## Observed state

`state/<env>.json` is an **atomic snapshot of observed state, not desired
state**: what the toolkit last *verified* as running in an environment. It
carries identities, digests and RFC3339 timestamps only — application
secrets never enter state files.

    { "schema": "toolkit.state/v1",
      "project": "...", "environment": "...",
      "current": null | { "release": "...", "bundleDigest": "sha256:...",
                          "since": "RFC3339" },
      "updatedAt": "RFC3339" }

- `current: null` means nothing has been observed deployed (a fresh target).
- Reads of a missing file are a normal condition (`ErrStateAbsent`), not a
  failure. Reads fail closed on schema or identity mismatch — a state file
  moved between environments is refused, not served.
- Writes are validated (identities well-formed, timestamps parse) and
  published atomically via the transport's `Put`.
- **Staging never writes state.** staged is not deployed; deployed is not
  verified. Only the step-9 state machine writes `current`, after lifecycle
  verification succeeds.

## Durable history

`history/<env>.jsonl` is an append-only, **integrity-checked** log of
deployment events. Each line is one record; records form a hash chain:

    { "seq": 1, "time": "RFC3339", "type": "stage.completed",
      "data": { ... }, "prev": "", "hash": "sha256:..." }

- `prev` carries the previous record's `hash` (`""` for the genesis record);
  `hash` covers the record's full content with deterministic JSON encoding
  (map keys sorted — identical inputs produce identical chains).
- **Append is verified:** every append re-reads and re-verifies the whole
  chain (sequence continuity, link integrity, per-record hashes) before
  extending it. The log is republished atomically in full; recorded entries
  are carried over untouched.
- **Reads never trust the file:** a log that fails verification is an
  error — never silently accepted.
- Records are operator-visible evidence: release identities, digests,
  structured stage outcomes, timestamps. Application secrets and raw hook
  output are forbidden in history records.

The chain's guarantee has precise limits, which are part of the contract:

**Detected:** in-place mutation of any record without rehashing; removal of
interior records; reordering; broken links; malformed records; sequence
gaps.

**Not detected:** deletion of a valid suffix (1→2→3→4 truncated to 1→2→3 is
still a valid chain); deliberate whole-chain rewrite with recomputed
hashes. Detecting those requires an external anchor for the expected
terminal `(seq, hash)` — for example a checkpoint in GitHub deployment
evidence. Until such an anchor exists, history is a verifiable local
record, not tamper-proof evidence.

The history log records facts ("stage completed", "deploy started",
"verification failed") — it does not decide anything. Authorization remains
where it has always been: the human merge of the promotion PR.

## Attempt markers: recovery versus concurrency

Two facts are deliberately kept apart:

- the **environment lock** (`.locks/<env>/`) means *someone may be executing
  right now* — active concurrency control;
- the **attempt marker** (`attempts/<env>.json`) means *the previous result
  is unresolved* — a recovery fact.

The lifecycle layer writes the marker before the first consequential stage
(migrate) and removes it only at a trusted terminal: observed state
committed, or a determined hook failure. If a deployment dies after
consequential work but before observed state commits — a transport loss
during migrate/apply/verify, a failed state commit — the marker survives,
and the next normal deployment **refuses** with a recovery-required outcome
instead of re-running migrate/apply into a target whose state is unknown.

Recovery is explicit: an operator (or step 10's rollback/recovery
machinery) resolves the attempt. One self-healing case is recognized: if
committed observed state already matches the desired release and digest,
the attempt demonstrably reached its terminal and the leftover marker is
cleared automatically.

The marker is strict metadata — one JSON value, schema-validated
(`toolkit.attempt/v1`): attempt id, from/to release, bundle digest,
start time. A corrupt marker fails closed, never silently counts as
absent.

## Reused staged material is verified, not trusted

`StageAlreadyStaged` is only returned after the existing release directory
has been proven to still contain exactly the canonical bundle: every
expected file byte-identical, executable semantics intact, marker identity
and digest matching. A `.staged.json` assertion from yesterday is not
evidence that yesterday's bytes are still there — altered staged material
is never executed or reused. `Target.VerifyStage` exposes the same proof
for any consumer of an old release directory (rollback candidates above
all). Detection of *extra* files added after staging is future work;
expected-file integrity is complete.

## Concurrency constraint on the lifecycle layer

This substrate provides no locking. Two actors mutating the same
environment concurrently can interleave destructively: two `AppendHistory`
calls can each read `N` records and each publish their own `N+1` (last
writer silently wins), and two first-time stages of the same version can
both observe the release directory absent and write into it before either
marker exists. Lifecycle execution (the step-9 state machine) therefore
MUST hold a **cross-process, target-scoped environment lock** for the full
validate→stage→migrate→apply→verify→commit sequence. An in-process mutex
is insufficient — correctness must not depend on which CI invocation
happened to run `deployctl`.
