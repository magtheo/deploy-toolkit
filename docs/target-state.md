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
   paths, `.`/`..` segments, and entry types the transport cannot
   materialize. V1 has no symlink primitive, so symlink entries are refused
   explicitly rather than approximated by regular files with different
   semantics.

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

`history/<env>.jsonl` is an append-only, tamper-evident log of deployment
events. Each line is one record; records form a **hash chain**:

    { "seq": 1, "time": "RFC3339", "type": "stage.completed",
      "data": { ... }, "prev": "", "hash": "sha256:..." }

- `prev` carries the previous record's `hash` (`""` for the genesis record);
  `hash` covers the record's full content with deterministic JSON encoding
  (map keys sorted — identical inputs produce identical chains).
- **Append is verified:** every append re-reads and re-verifies the whole
  chain (sequence continuity, link integrity, per-record hashes) before
  extending it. The log is republished atomically in full; recorded entries
  are carried over untouched.
- **Reads never trust the file:** a tampered, truncated, or reordered log
  fails verification and is an error — never silently accepted.
- Records are operator-visible evidence: release identities, digests,
  decision outcomes, timestamps. Application secrets are forbidden in
  history records.

The history log records facts ("stage completed", "deploy started",
"verification failed") — it does not decide anything. Authorization remains
where it has always been: the human merge of the promotion PR.
