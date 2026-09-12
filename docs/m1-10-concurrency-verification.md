# M1-10 verification note: concurrency & crash-safety qualification

Status: complete. This note is the register update for M1-10
("the concurrency story has zero proof today").

## Race matrix (real goroutines, real local transports, FIFO/sentinel
handshakes — no sleeps as synchronization)

| Case | Held-at | Proof |
| --- | --- | --- |
| deploy vs deploy, same env | apply (in-flight, post-boundary) | loser: `ErrEnvLockHeld`, zero hooks (ledger unchanged), zero durable state; winner commits; no lock survives |
| deploy vs rollback, same env | apply | same refusal facts; rollback performed nothing |
| deploy vs recovery resolve | apply (attempt marker present on disk) | resolve refused on the lock; the in-flight attempt marker untouched |
| rollback vs recovery resolve | rollback hook | resolve refused; in-flight recovery marker untouched |
| deploy vs deploy, different envs | both parked in apply simultaneously | independent locks, independent progress, both commit, both locks cleaned |

Churn: 40 sequential full deploys; 6 environments × 8 rounds in
parallel against one target (`TestLockChurn*`).

## Boundary matrix

Cancellation / transport-loss coverage per boundary — proven by
(existing) unless noted:

- preflight: existing (fate-unknown refusal, pre-boundary) + contract-violation table
- attempt-marker write: NEW — cancellation mid-write → no marker, boundary never crossed, lock cleanly released
- migrate/apply/rollback: existing (unknown-fate retention tests for each stage + rollback hook)
- verify: existing (M1-6 evidence tests cancel/kill at verify/apply)
- observed-state commit: NEW — cancellation mid-commit → uncertain, marker survives, lock released (verify's fate was determined); also fixed: the engine report now sets `RecoveryRequired` on this path (the fact was CLI-compensated before)
- evidence/history finalization: existing (M1-6 suite)
- marker cleanup: NEW — `rm` of the attempt marker failing at the trusted terminal → commit stands, marker survives, retry self-heals
- lock cleanup: existing (compound `ErrLockReleaseFailed` tests + retention tests)

## Global invariants

- **A. No unexplained surviving lock** — asserted throughout:
  `TestNoUnexplainedSurvivingLock` (retention), the compound-failure
  tests (`ErrLockReleaseFailed`), and post-run lock probes in every
  race/churn test.
- **B. No lost recovery evidence** — every uncertain-path test asserts
  the marker survives; the state-commit boundary now carries the fact
  in the engine report itself.
- **C. No unsafe repeated consequential work** — scoped to
  retry/recovery of the same unresolved operation: refusal tests
  (held-lock, recovery-required) plus the marker-cleanup self-heal
  proof. (Normal later deployments legitimately run hooks again.)

## Refinements the stress runs forced (each narrow, fail-closed preserved)

- Staging-lock release is bounded by `target.StagingCleanupTimeout`
  (overridable) — a stuck target cannot hang staging finalization any
  more than it can hang lock cleanup.
- All client-side validation (bundle digest, entry-path refusals)
  happens BEFORE the staging lock is acquired: a hostile bundle still
  writes literally nothing to the target.
- The mkdir/probe handoff (winner releases the staging lock between the
  loser's failed mkdir and its probe) gets a bounded 3-attempt
  re-check — a precise, demonstrated race, not broad retry. A lock
  that keeps existing is still refusal-class.
- Existing tests updated where their fault injection became broader
  than the scenario they prove (the join test now injects rmdir failure
  into the environment lock only; the bounded-cleanup test bounds both
  cleanup timeouts).

## Stress

- `go test -race -shuffle=on -count=3 ./internal/lifecycle/ ./cmd/deployctl/ ./internal/target/ ./internal/transport/...` — clean
- `go test -race -count=20` on the churn/race/sentinel set — clean
- full repository gate (gofmt, vet, all packages, darwin cross-build, template validation) — clean

## Bugs found and fixed (each with adversarial regression)

1. **Cross-environment staging race (real production bug).** Releases
   are environment-independent, so two environments deploying the same
   version raced one staging sequence; the loser saw a mid-stage
   directory (no marker yet) and failed "interrupted stage" — a
   false interrupted-stage claim and a violation of environment
   independence. Found by `TestLockChurnParallelEnvironments` within
   minutes of existing. Two distinct ordering defects had to be fixed:
   (a) the losers' directory probe ran BEFORE the staging lock, so a
   concurrent environment's mid-stage directory was still misread —
   CI reproduced this consistently while local `-race` runs passed, a
   genuinely scheduling-sensitive hazard; (b) the lock's release defer
   was registered after the already-staged probe block, so every
   `already-staged` stage leaked the lock and permanently blocked
   future stages of that version — caught by the sequential churn
   immediately after. Both orderings now have adversarial coverage:
   only pure client-side validation precedes the lock, and the defer
   is registered immediately after acquisition. The churn tests fail
   deterministically on either defect. Fix: project-scoped staging lock
   (`.staging/<version>/`, atomic mkdir, crash leaves it for manual
   removal, refusal-class sentinel `target.ErrStageLockHeld`, bounded
   mkdir/probe handoff for the winner-releases-lock window; CLI renders
   the refusal). This also explains the PR #1 CI failure being
   environment-sensitive (timing), though that symptom was a different
   surface.
2. **State-commit failure did not set `RecoveryRequired` in the engine
   report** (CLI compensated). Now set at the boundary.
3. **Resolve dropped deferred release failures** — caught during the
   M1-6 amendment review (unnamed returns); included here in the
   register for completeness.

4. **Staging-lock release failure could be silently lost** (review
   amendment to this phase). The staging lock's release defer recorded
   the rmdir failure only when no primary staging error existed,
   contradicting its own "never swallowed" comment. Now:
   `target.ErrStageLockReleaseFailed` is always `errors.Join`ed — both
   facts survive. The CLI surfaces it as the structured
   `stagingLockReleaseFailed` envelope fact (distinct from the
   environment lock's `lockReleaseFailed`, implying `safeToRetry:
   false`), and the held-staging-lock refusal now classifies
   identically on human and JSON surfaces (refused, exit 1).

## ENOTEMPTY flake verdict (PR #1 CI, `Release` rmdir)

Not reproduced: 40-deploy sequential churn, 6×8 parallel-environment
churn, and `TestRecoveryResolvePerMarkerScopes` under
`-race -count=20` all pass locally. `Release` is strictly sequential
(`rm -f owner.json` → `rmdir`, same goroutine, kernel-ordered), and no
code path writes into the lock directory between those calls. The
one-time CI failure (never since, including this phase's heavier
stress) remains consistent with a runner/overlayfs dentry anomaly.
**Verdict: watch-item.** No retry added — a retry would be justified
only by a demonstrated transient condition, which we do not have. If
it recurs, M1-10's stress harness is the reproduction vehicle.

## Conclusion

For a single environment, concurrent deploy, rollback, and resolution
are serialized by the environment lock; no losing actor performs
consequential work; failures and cancellation cannot silently lose the
lock/marker facts needed for recovery; retries do not repeat work
whose safety is unknown; and separate environments remain
independently operable (serialized only where they genuinely share
mutable state — the staging sequence of the same release version).
