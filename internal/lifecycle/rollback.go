package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/target"
)

// RollbackAuthorization states who authorized a rollback. The execution
// mechanics are shared; the authority is not, and it is recorded as
// evidence in history.
//
//	RollbackRecovery — resolving an unresolved deployment attempt
//	                   (an attempt marker must exist and match).
//	RollbackManual   — explicit operator/emergency authority. May resolve
//	                   an unresolved attempt, or roll back a healthy
//	                   deployment (a recovery marker is then created).
//	RollbackAuto     — pre-authorized by the environment's failure policy:
//	                   requires an unresolved attempt marker plus
//	                   migration.rollbackSafe on the failed release.
type RollbackAuthorization string

const (
	RollbackRecovery RollbackAuthorization = "recovery"
	RollbackManual   RollbackAuthorization = "manual"
	RollbackAuto     RollbackAuthorization = "auto"
)

// RollbackInput describes the recovery transition From → To. Both
// releases are immutable Release manifests, and both bundles arrive as
// prepared canonical bytes from the prepare side — verified against their
// pinned digests before anything executes.
//
// Hook ownership follows semantic responsibility: the From (failed)
// release's staged contract supplies its optional rollback hook — B knows
// how to undo B-specific consequences such as its own migration. The To
// (previous) release's staged contract supplies preflight, apply and the
// mandatory verify — A knows how to install and verify A. The To release's
// forward migrate hook is deliberately NEVER run as a rollback mechanism.
type RollbackInput struct {
	Target         *target.Target
	TargetManifest *manifest.Target
	Environment    *manifest.Environment
	// FromRelease is the release being rolled back — the failed or
	// undesired one. Its migration semantics are the ones being undone.
	FromRelease *manifest.Release
	FromBundle  []byte
	// ToRelease is the release being restored — the known previous one.
	ToRelease *manifest.Release
	ToBundle  []byte
	// Authorization is the authority path (see RollbackAuthorization).
	Authorization RollbackAuthorization
	Owner         string // lock owner identity, e.g. "operator:alice"
	Now           func() time.Time
}

// RollbackReport describes one recovery attempt. Like Deploy, a completed
// attempt with a failed hook or a refused condition is reported, not
// returned as an error; infrastructure failures are returned as errors.
type RollbackReport struct {
	Project          string
	Environment      string
	FromVersion      string
	FromBundleDigest string
	ToVersion        string
	ToBundleDigest   string
	LockDir          string
	StagedFrom       target.StageStatus
	StagedTo         target.StageStatus
	Stages           []StageResult
	Committed        bool // observed state advanced to the To release
	// RecoveryID is the id of the recovery this invocation reconciled:
	// its own marker's id on the emergency path, or the pre-existing
	// marker's id on the proven-committed (self-heal) path — not
	// necessarily empty there. It is "" only when nothing was
	// reconciled.
	RecoveryID string
	// RecoveryResolved: the deployment attempt marker being recovered was
	// cleared at the trusted terminal (or by the self-heal path).
	RecoveryResolved bool
	// RecoveryMarkerCreated: the rollback created its own recovery marker
	// (the emergency path — no unresolved recovery existed beforehand).
	RecoveryMarkerCreated bool
	// AlreadyRecovered: a leftover recovery marker was PROVEN committed
	// (observed state carries its operationId), so this invocation only
	// performed safe cleanup and executed no hooks.
	AlreadyRecovered bool
	// RecoveryRequired: the invocation was refused because an unresolved
	// recovery marker exists and repeating consequential rollback work is
	// not known-safe.
	RecoveryRequired bool
	// RecoveryStarted is the durable-boundary fact: the recovery marker
	// was successfully persisted, so the rollback hook and the restored
	// release's apply/verify may have begun. An infrastructure error
	// with this flag set leaves the outcome UNKNOWN — uncertain, never
	// safe-to-rerun — regardless of whether a later target read can
	// still see the marker. Set immediately after the marker write
	// succeeds.
	RecoveryStarted bool
	HistorySeq      int64
	FailureReason   string
	// LockRetained records that the invocation deliberately did NOT
	// release its acquired environment lock because a lifecycle hook's
	// execution fate was RunUnknown — the hook process may still be
	// running. A controlled crash, not a release failure (which surfaces
	// as a joined error instead).
	LockRetained bool
}

// Rollback is the explicit recovery operation. It is deliberately NOT a
// call to Deploy: after a failed A→B attempt, observed state may still say
// A while production may partly or fully run B — normal Deploy's
// idempotency guard would answer "already-current" and never restore A.
// Rollback forces the known previous release back into place and verifies
// it.
//
// The sequence:
//
//	ACQUIRE the same environment lock
//	  → READ observed state, deployment attempt marker, recovery marker
//	  → RESOLVE any leftover recovery marker (self-heal ONLY with
//	    operationId proof; otherwise refuse — never replay)
//	  → VALIDATE the recovery transition (exact identity bindings)
//	  → VERIFY rollback policy (authorization path + migration semantics)
//	  → STAGE + verify staged contracts for BOTH releases
//	  → To-release PREFLIGHT (non-impacting)
//	  → WRITE the recovery marker (from/to, authorization, recoveryId)
//	  → From-release ROLLBACK hook (if its migration ran and hook declared)
//	  → To-release APPLY → VERIFY (mandatory)
//	  → COMMIT observed state = To release, signed recovery:<recoveryId>
//	  → CLEAR attempt marker, RECORD the rollback outcome, CLEAR the
//	    recovery marker (evidence survives the last breadcrumb removed;
//	    trusted terminal)
//	  → RELEASE lock
//
// Failure semantics mirror Deploy's, one notch stricter: everything before
// the recovery marker write (stage, contracts, To-preflight) is retryable;
// once the marker exists, ANY failure — hook, transport, state commit —
// keeps BOTH markers, and the next ordinary recovery REFUSES rather than
// repeating B's rollback hook or A's apply. v0.1 assumes no hook
// idempotency contract; resolution is explicit (a fresh recovery after an
// operator has verified the target, removing the markers). The one
// automatic path is the self-heal: a leftover recovery marker whose
// operationId is recorded in observed state is PROVEN committed, so its
// cleanup may run without executing anything.
func Rollback(ctx context.Context, in RollbackInput) (rep *RollbackReport, err error) {
	switch {
	case in.Target == nil:
		return nil, fmt.Errorf("RollbackInput.Target is required")
	case in.TargetManifest == nil:
		return nil, fmt.Errorf("RollbackInput.TargetManifest is required")
	case in.Environment == nil:
		return nil, fmt.Errorf("RollbackInput.Environment is required")
	case in.FromRelease == nil:
		return nil, fmt.Errorf("RollbackInput.FromRelease is required")
	case len(in.FromBundle) == 0:
		return nil, fmt.Errorf("RollbackInput.FromBundle is required (prepared canonical bundle bytes)")
	case in.ToRelease == nil:
		return nil, fmt.Errorf("RollbackInput.ToRelease is required")
	case len(in.ToBundle) == 0:
		return nil, fmt.Errorf("RollbackInput.ToBundle is required (prepared canonical bundle bytes)")
	}
	switch in.Authorization {
	case RollbackRecovery, RollbackManual, RollbackAuto:
	default:
		return nil, fmt.Errorf("RollbackInput.Authorization must be one of recovery, manual, auto (got %q)", in.Authorization)
	}
	now := in.Now
	if now == nil {
		now = time.Now
	}
	from, to, env := in.FromRelease, in.ToRelease, in.Environment
	rep = &RollbackReport{
		Project:          from.Metadata.Project,
		Environment:      env.Metadata.Name,
		FromVersion:      from.Metadata.Version,
		FromBundleDigest: from.Bundle.Digest,
		ToVersion:        to.Metadata.Version,
		ToBundleDigest:   to.Bundle.Digest,
	}
	if err := validateRollbackTransition(in); err != nil {
		return rep, err
	}

	layout := in.Target.Layout()
	lockDir, err := layout.LockPath(rep.Project, rep.Environment)
	if err != nil {
		return rep, err
	}
	rep.LockDir = lockDir
	lock, err := AcquireEnvLock(ctx, in.Target.Transport(), lockDir, LockOwner{
		User:        in.Owner,
		Project:     rep.Project,
		Environment: rep.Environment,
		Release:     rep.FromVersion + "→" + rep.ToVersion,
	})
	if err != nil {
		return rep, err
	}
	// Lifecycle-local retain state (mirrors Deploy): Release stays
	// literal and is skipped entirely on a deliberate retention.
	releaseLock := true
	defer func() {
		if !releaseLock {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), lockCleanupTimeout)
		defer cancel()
		if relErr := lock.Release(cleanupCtx); relErr != nil {
			err = errors.Join(err, fmt.Errorf("environment lock %s could not be released (manual cleanup required): %w", lockDir, relErr))
		}
	}()

	failRollback := func(reason string) (*RollbackReport, error) {
		rep.FailureReason = reason
		// Same rule as Deploy, one notch stricter: the marker is never
		// cleared on failure. A rollback that died after consequential
		// work has an unknown outcome, and even a determined hook exit
		// may have half-undone the failed release. The environment stays
		// recovery-required; a later rollback (whose bindings still
		// hold) or operator action resolves it.
		//
		// Consistency rule, mirroring Deploy: once the durable boundary
		// is crossed (RecoveryStarted) and the recovery did not commit,
		// the recovery marker survives — so the report, history and CLI
		// output must declare recovery-required on the FIRST failure.
		if rep.RecoveryStarted && !rep.Committed {
			rep.RecoveryRequired = true
		}
		if herr := recordRollbackOutcome(ctx, in, now, rep, "rollback.failed", string(in.Authorization)); herr != nil {
			return rep, errors.Join(fmt.Errorf("%s", reason), fmt.Errorf("history outcome not recorded: %w", herr))
		}
		return rep, nil
	}
	failInfra := func(what string, cause error) (*RollbackReport, error) {
		rep.FailureReason = what + " (infrastructure failure)"
		if herr := recordRollbackOutcome(ctx, in, now, rep, "rollback.failed", string(in.Authorization)); herr != nil {
			cause = errors.Join(cause, fmt.Errorf("history outcome not recorded: %w", herr))
		}
		return rep, fmt.Errorf("%s: %w", what, cause)
	}

	// Observed state: what the target last VERIFIED as running.
	observed, oerr := in.Target.ReadState(ctx, rep.Project, rep.Environment)
	if oerr != nil && !errors.Is(oerr, target.ErrStateAbsent) {
		return rep, fmt.Errorf("read observed state: %w", oerr)
	}
	if errors.Is(oerr, target.ErrStateAbsent) {
		observed = target.State{Project: rep.Project, Environment: rep.Environment}
	}

	// The unresolved attempt marker decides which recovery situation this
	// is. It is read, never rewritten here, and never cleared before the
	// trusted terminal.
	attempt, aerr := in.Target.ReadAttempt(ctx, rep.Project, rep.Environment)
	if aerr != nil && !errors.Is(aerr, target.ErrAttemptAbsent) {
		return rep, fmt.Errorf("read attempt marker: %w", aerr)
	}
	haveMarker := aerr == nil

	// The recovery marker is the OTHER durable fact: WHAT recovery is
	// already in flight (the attempt marker says WHY recovery is needed).
	recovery, rerr := in.Target.ReadRecovery(ctx, rep.Project, rep.Environment)
	if rerr != nil && !errors.Is(rerr, target.ErrRecoveryAbsent) {
		return rep, fmt.Errorf("read recovery marker: %w", rerr)
	}
	haveRecovery := rerr == nil

	// A leftover recovery marker admits exactly two resolutions:
	//
	//  1. PROVEN committed — observed state carries this recovery's
	//     operationId AND the release/digest it names. The operationId
	//     binds the observation to this recovery; the release and digest
	//     cross-check binds it to this recovery's transition, so a
	//     structurally valid but semantically inconsistent state snapshot
	//     can never talk the toolkit into deleting recovery evidence.
	//     (The cross-check matters because observed state may have said
	//     the To release before the rollback ever ran — release identity
	//     alone proves nothing.) Then this invocation runs NO hooks and
	//     only completes the cleanup; the request must describe the same
	//     transition.
	//  2. Anything else — an UNRESOLVED recovery: a previous rollback has
	//     executed consequential work (a rollback hook, an apply) with an
	//     unknown outcome. Repeating it is no safer than repeating a
	//     failed migration, so ordinary recovery refuses, no matter which
	//     authorization asks. Resolution is explicit: an operator
	//     verifies the target, removes the markers, and starts a fresh
	//     recovery.
	if haveRecovery {
		committed := observed.Current != nil &&
			observed.Current.OperationID == "recovery:"+recovery.RecoveryID &&
			observed.Current.Release == recovery.ToRelease &&
			observed.Current.BundleDigest == recovery.ToBundleDigest
		sameTransition := recovery.FromRelease == rep.FromVersion && recovery.FromBundleDigest == rep.FromBundleDigest &&
			recovery.ToRelease == rep.ToVersion && recovery.ToBundleDigest == rep.ToBundleDigest
		if committed && sameTransition {
			return resolveCommittedRecovery(ctx, in, now, rep, attempt, haveMarker, recovery)
		}
		rep.RecoveryRequired = true
		if committed {
			return failRollback(fmt.Sprintf(
				"recovery marker %s (%s → %s) is proven committed by observed state but this request describes a different transition (%s → %s): rerun that exact recovery to complete its cleanup, or resolve it explicitly",
				recovery.RecoveryID, recovery.FromRelease, recovery.ToRelease, rep.FromVersion, rep.ToVersion))
		}
		return failRollback(fmt.Sprintf(
			"unresolved recovery marker %s (%s → %s, started %s): a previous rollback has executed consequential work with an unknown outcome — repeating a rollback hook or an apply is not known-safe. Verify the target, resolve the markers explicitly, then start a fresh recovery",
			recovery.RecoveryID, recovery.FromRelease, recovery.ToRelease, recovery.StartedAt))
	}

	// VALIDATE the recovery transition: bind both releases to durable
	// facts, not to the caller's word.
	switch {
	case haveMarker:
		// Recovering the recorded attempt: the From release must be
		// EXACTLY what the attempt was deploying, and the To release
		// must be EXACTLY the release the attempt started from.
		if attempt.ToRelease != rep.FromVersion || attempt.BundleDigest != rep.FromBundleDigest {
			return failRollback(fmt.Sprintf(
				"attempt marker records %s → %s@%s; this rollback restores %s → %s@%s — a recovery must resolve the recorded attempt, not a different transition",
				attempt.FromRelease, attempt.ToRelease, attempt.BundleDigest, rep.FromVersion, rep.ToVersion, rep.ToBundleDigest))
		}
		if attempt.FromRelease == "" {
			return failRollback("the unresolved attempt started from no deployed release; there is no previous release to restore")
		}
		if attempt.FromRelease != rep.ToVersion {
			return failRollback(fmt.Sprintf(
				"attempt marker started from %s, but this rollback would restore %s — refusing to restore a release the attempt did not replace",
				attempt.FromRelease, rep.ToVersion))
		}
		if observed.Current == nil ||
			observed.Current.Release != rep.ToVersion || observed.Current.BundleDigest != rep.ToBundleDigest {
			cur := "none"
			if observed.Current != nil {
				cur = observed.Current.Release + "@" + observed.Current.BundleDigest
			}
			return failRollback(fmt.Sprintf(
				"observed state is %s, but the rollback target is %s@%s — restoring anything but the recorded previous release would be a guess",
				cur, rep.ToVersion, rep.ToBundleDigest))
		}
	case observed.Current != nil &&
		observed.Current.Release == rep.FromVersion && observed.Current.BundleDigest == rep.FromBundleDigest:
		// Emergency rollback of a resolved, healthy deployment: the
		// observed release is exactly the one being undone. The recovery
		// marker is written below, before the first consequential stage.
		if in.Authorization != RollbackManual {
			return failRollback(fmt.Sprintf(
				"no unresolved attempt marker exists and observed state is %s@%s: automatic and recovery rollbacks require an unresolved attempt — use explicit manual authorization for an emergency rollback",
				rep.FromVersion, rep.FromBundleDigest))
		}
	default:
		cur := "none"
		if observed.Current != nil {
			cur = observed.Current.Release + "@" + observed.Current.BundleDigest
		}
		return failRollback(fmt.Sprintf(
			"rollback must start from the release that is actually deployed: observed state is %s, not %s@%s, and no unresolved attempt marker binds %s",
			cur, rep.FromVersion, rep.FromBundleDigest, rep.FromVersion))
	}

	// VERIFY rollback policy. Migration semantics are claims by the
	// failed release; the toolkit never overrides them — for any
	// authorization path.
	if from.Migration.Mode == manifest.MigrationIrreversible {
		return failRollback(fmt.Sprintf(
			"release %s declares migration mode %q: it cannot be undone, and the toolkit will not pretend a rollback restores the pre-migration state",
			rep.FromVersion, from.Migration.Mode))
	}
	if in.Authorization == RollbackAuto {
		if ok, why := AutoRollbackPermitted(env, from); !ok {
			return failRollback("automatic rollback not permitted: " + why)
		}
	}

	// STAGE both releases. Staging is digest-verified and non-impacting;
	// AlreadyStaged implies the staged material still verifies.
	stagedFrom, serr := in.Target.Stage(ctx, from, in.FromBundle, now())
	if serr != nil {
		return rep, fmt.Errorf("stage release %s: %w", rep.FromVersion, serr)
	}
	rep.StagedFrom = stagedFrom
	stagedTo, serr := in.Target.Stage(ctx, to, in.ToBundle, now())
	if serr != nil {
		return rep, fmt.Errorf("stage release %s: %w", rep.ToVersion, serr)
	}
	rep.StagedTo = stagedTo

	// Both staged contracts are verified against their release pins
	// before anything executes from either directory.
	fromContract, err := verifyStagedContract(ctx, in.Target, from)
	if err != nil {
		if errors.Is(err, errContractRefused) {
			return failRollback(err.Error())
		}
		return failInfra(fmt.Sprintf("staged deployment contract for %s unreadable", rep.FromVersion), err)
	}
	toContract, err := verifyStagedContract(ctx, in.Target, to)
	if err != nil {
		if errors.Is(err, errContractRefused) {
			return failRollback(err.Error())
		}
		return failInfra(fmt.Sprintf("staged deployment contract for %s unreadable", rep.ToVersion), err)
	}
	if toContract.Lifecycle.Verify == nil {
		// Defense in depth: the schema already requires verify. Observed
		// state must never advance on a skipped verification.
		return failRollback(fmt.Sprintf("staged contract for %s declares no verify hook; verify is mandatory", rep.ToVersion))
	}

	toDir, err := layout.ReleaseDir(rep.Project, rep.ToVersion)
	if err != nil {
		return rep, err
	}
	fromDir, err := layout.ReleaseDir(rep.Project, rep.FromVersion)
	if err != nil {
		return rep, err
	}
	toHenv, err := hookEnv(to, rep.Environment)
	if err != nil {
		return rep, fmt.Errorf("hook environment: %w", err)
	}
	fromHenv, err := hookEnv(from, rep.Environment)
	if err != nil {
		return rep, fmt.Errorf("hook environment: %w", err)
	}
	// RunUnknown fate retains the lock for every hook, preflight
	// included (mirrors Deploy: the hook-execution boundary and the
	// durable recovery boundary are different).
	runToStage := func(name string, step *manifest.LifecycleStep) (StageResult, error) {
		sr, err := runStageStep(ctx, in.Target.Transport(), name, toDir, toHenv, step)
		if sr.Unknown {
			releaseLock = false
			rep.LockRetained = true
		}
		return sr, err
	}

	// The To release's preflight decides whether the restore may begin.
	sr, ierr := runToStage("preflight", toContract.Lifecycle.Preflight)
	rep.Stages = append(rep.Stages, sr)
	if ierr != nil {
		return failInfra("preflight could not be executed", ierr)
	}
	if sr.Failed {
		return failRollback("preflight failed")
	}

	// Durable recovery marker BEFORE the first consequential stage. The
	// deployment attempt marker says WHY recovery is needed and is never
	// rewritten; this marker says WHAT recovery was started: which
	// transition, which authorization, which attempt it resolves. From
	// here until a trusted terminal, ANY failure keeps it — and the next
	// ordinary recovery refuses rather than replaying hooks.
	recoveryID, err := randomHexID()
	if err != nil {
		return rep, fmt.Errorf("generate recovery id: %w", err)
	}
	sourceAttempt := ""
	if haveMarker {
		sourceAttempt = attempt.AttemptID
	}
	if werr := in.Target.WriteRecovery(ctx, target.RecoveryMarker{
		RecoveryID:       recoveryID,
		SourceAttemptID:  sourceAttempt,
		Project:          rep.Project,
		Environment:      rep.Environment,
		FromRelease:      rep.FromVersion,
		FromBundleDigest: rep.FromBundleDigest,
		ToRelease:        rep.ToVersion,
		ToBundleDigest:   rep.ToBundleDigest,
		Authorization:    string(in.Authorization),
		StartedAt:        now().UTC().Format(time.RFC3339),
	}); werr != nil {
		return failInfra("recovery marker could not be persisted; refusing consequential work", werr)
	}
	rep.RecoveryID = recoveryID
	rep.RecoveryMarkerCreated = true
	rep.RecoveryStarted = true

	// From-release ROLLBACK hook: undoes From-specific consequences —
	// above all its own migration. It runs only if the failed release's
	// migration actually ran (mode ≠ none) and its staged contract
	// declares the hook. The To release's forward migrate hook is
	// deliberately not run: its migration metadata describes deploying To
	// forward, not reversing From.
	if from.Migration.Mode != manifest.MigrationNone {
		sr, ierr = runStageStep(ctx, in.Target.Transport(), "rollback", fromDir, fromHenv, fromContract.Lifecycle.Rollback)
		rep.Stages = append(rep.Stages, sr)
		if sr.Unknown {
			releaseLock = false
			rep.LockRetained = true
		}
		if ierr != nil {
			if sr.Unknown {
				// The recovery marker exists (written before this
				// stage), so recovery is required as a fact.
				rep.RecoveryRequired = true
			}
			return failInfra("rollback could not be executed", ierr)
		}
		if sr.Failed {
			return failRollback("rollback hook failed")
		}
	}

	sr, ierr = runToStage("apply", toContract.Lifecycle.Apply)
	rep.Stages = append(rep.Stages, sr)
	if ierr != nil {
		if sr.Unknown {
			rep.RecoveryRequired = true
		}
		return failInfra("apply could not be executed", ierr)
	}
	if sr.Failed {
		return failRollback("apply failed")
	}

	sr, ierr = runToStage("verify", toContract.Lifecycle.Verify)
	rep.Stages = append(rep.Stages, sr)
	if ierr != nil {
		if sr.Unknown {
			rep.RecoveryRequired = true
		}
		return failInfra("verify could not be executed", ierr)
	}
	if sr.Failed {
		return failRollback("verify failed")
	}

	// Verified — and only verified — lets observed state move back. The
	// commit is signed with this recovery's id: the durable proof of
	// WHICH operation produced this observation (release identity alone
	// cannot distinguish "rollback done" from "rollback never started",
	// because observed state may have said the To release all along).
	observed.Current = &target.CurrentDeployment{
		Release:      rep.ToVersion,
		BundleDigest: rep.ToBundleDigest,
		Since:        now().UTC().Format(time.RFC3339),
		OperationID:  "recovery:" + recoveryID,
	}
	observed.UpdatedAt = now().UTC().Format(time.RFC3339)
	if werr := in.Target.WriteState(ctx, observed); werr != nil {
		return failInfra("observed state commit failed", werr)
	}
	rep.Committed = true

	// Trusted terminal: the restored release verified and the state
	// committed — signed recovery:<recoveryId> in observed state.
	// Clearing order matters, and so does evidence order. The ATTEMPT
	// marker goes first: a leftover attempt marker alone (recovery
	// already cleared) would read as "recovery still needed" and invite
	// a replay over committed state. The success evidence is persisted
	// WHILE the recovery marker still exists, because that marker is the
	// breadcrumb the proven-committed cleanup path needs: if the history
	// write or the recovery-marker removal fails, the next invocation
	// self-heals without executing anything. The recovery marker is the
	// LAST recovery fact removed.
	if cerr := in.Target.ClearAttempt(ctx, rep.Project, rep.Environment); cerr != nil {
		// RecoveryResolved is not claimed — the attempt marker is still
		// there. Evidence is still recorded while the recovery marker
		// exists, so the next invocation completes the cleanup.
		if herr := recordRollbackOutcome(ctx, in, now, rep, "rollback.succeeded", string(in.Authorization)); herr != nil {
			return rep, errors.Join(fmt.Errorf("observed state committed, but the attempt marker could not be cleared: %w", cerr), fmt.Errorf("history outcome not recorded: %w", herr))
		}
		return rep, fmt.Errorf("observed state committed, but the attempt marker could not be cleared: %w", cerr)
	}
	rep.RecoveryResolved = haveMarker
	if herr := recordRollbackOutcome(ctx, in, now, rep, "rollback.succeeded", string(in.Authorization)); herr != nil {
		// The attempt marker is cleared, but the recovery marker REMAINS:
		// the next invocation for this transition enters the
		// proven-committed cleanup path instead of rerunning hooks.
		return rep, fmt.Errorf("history outcome not recorded: %w", herr)
	}
	if cerr := in.Target.ClearRecovery(ctx, rep.Project, rep.Environment); cerr != nil {
		return rep, fmt.Errorf("observed state committed and rollback.succeeded recorded, but the recovery marker could not be cleared: %w", cerr)
	}
	return rep, nil
}

// resolveCommittedRecovery completes the bookkeeping of a recovery that
// has demonstrably committed: observed state carries the recovery marker's
// operationId and its exact to-release/digest, so no hook may run — only
// marker cleanup. The historical identity and authority are the marker's,
// not the cleanup invocation's: this path finishes bookkeeping, it must
// not rewrite who authorized the operation that actually changed
// production (Step 11 reconciliation decisions depend on that authority).
func resolveCommittedRecovery(ctx context.Context, in RollbackInput, now func() time.Time, rep *RollbackReport, attempt target.AttemptMarker, haveAttempt bool, recovery target.RecoveryMarker) (*RollbackReport, error) {
	rep.RecoveryID = recovery.RecoveryID
	clearedAttempt := false
	if haveAttempt && recovery.SourceAttemptID != "" && attempt.AttemptID == recovery.SourceAttemptID {
		if cerr := in.Target.ClearAttempt(ctx, rep.Project, rep.Environment); cerr != nil {
			return rep, fmt.Errorf("observed state is committed (recovery %s), but the attempt marker could not be cleared: %w", recovery.RecoveryID, cerr)
		}
		clearedAttempt = true
	}
	rep.AlreadyRecovered = true
	rep.RecoveryResolved = clearedAttempt
	// Evidence before the last breadcrumb: the recovery marker stays
	// until rollback.already-recovered is durably recorded.
	if herr := recordRollbackOutcome(ctx, in, now, rep, "rollback.already-recovered", recovery.Authorization); herr != nil {
		return rep, fmt.Errorf("history outcome not recorded: %w", herr)
	}
	if cerr := in.Target.ClearRecovery(ctx, rep.Project, rep.Environment); cerr != nil {
		return rep, fmt.Errorf("rollback.already-recovered recorded, but the recovery marker could not be cleared: %w", cerr)
	}
	return rep, nil
}

// AutoRollbackPermitted evaluates the environment's pre-authorized
// failure policy against the FAILED release's migration semantics. It is
// the single policy gate for RollbackAuto; callers (step 11 wiring) use it
// to decide whether to invoke Rollback after a failed deploy. Manual and
// promotion-driven recovery are separate explicit authority paths and do
// not consult this policy.
func AutoRollbackPermitted(env *manifest.Environment, failed *manifest.Release) (bool, string) {
	switch env.AutoRollback() {
	case manifest.AutoRollbackOff:
		return false, "failurePolicy.autoRollback: off"
	case manifest.AutoRollbackSafeOnly:
		if failed.Migration.Mode == manifest.MigrationIrreversible {
			return false, "migration mode irreversible cannot be undone"
		}
		if !failed.Migration.RollbackSafe {
			return false, "migration.rollbackSafe: false disables automatic rollback"
		}
		return true, ""
	default:
		return false, fmt.Sprintf("failurePolicy.autoRollback %q is not a recognized policy; refusing (treated as off)", env.AutoRollback())
	}
}

// validateRollbackTransition checks input identities before anything
// touches the target: the environment must point at this target and at one
// of the two involved releases (Git desired state may still pin the failed
// release during auto/emergency rollback, or already pin the restored one
// for promotion-driven recovery), and the transition must actually move.
func validateRollbackTransition(in RollbackInput) error {
	from, to, env := in.FromRelease, in.ToRelease, in.Environment
	if env.Spec.Target == "" {
		return fmt.Errorf("environment %q declares no target", env.Metadata.Name)
	}
	if env.Spec.Target != in.TargetManifest.Metadata.Name {
		return fmt.Errorf("environment %q targets %q, but the rollback targets %q", env.Metadata.Name, env.Spec.Target, in.TargetManifest.Metadata.Name)
	}
	ref := func(rel *manifest.Release) string {
		return fmt.Sprintf(".deploy/releases/%s-%s.yaml", rel.Metadata.Project, rel.Metadata.Version)
	}
	if env.Spec.Release != ref(from) && env.Spec.Release != ref(to) {
		return fmt.Errorf("environment %q pins release %q, which is neither the rollback source %q nor the restore target %q", env.Metadata.Name, env.Spec.Release, ref(from), ref(to))
	}
	if from.Metadata.Project != to.Metadata.Project {
		return fmt.Errorf("rollback across projects: %q → %q", from.Metadata.Project, to.Metadata.Project)
	}
	if err := target.CheckVersion(from.Metadata.Version); err != nil {
		return fmt.Errorf("rollback source version: %w", err)
	}
	if err := target.CheckVersion(to.Metadata.Version); err != nil {
		return fmt.Errorf("restore target version: %w", err)
	}
	if from.Metadata.Version == to.Metadata.Version {
		return fmt.Errorf("rollback source and restore target are the same release (%s); there is nothing to restore", from.Metadata.Version)
	}
	return nil
}

// recordRollbackOutcome appends the structured, secret-free history record
// for a rollback outcome. The authorization is an explicit parameter: a
// cleanup-only invocation records the ORIGINAL recovery's authorization —
// the durable marker, not the invocation finishing the bookkeeping, is
// authoritative for who changed production.
func recordRollbackOutcome(ctx context.Context, in RollbackInput, now func() time.Time, rep *RollbackReport, kind, authorization string) error {
	// Mirror deploy: evidence finalization is context-independent.
	ctx, cancel := evidenceCtx()
	defer cancel()
	stages := make(map[string]any, len(rep.Stages))
	for _, s := range rep.Stages {
		switch {
		case s.Skipped:
			stages[s.Name] = "skipped"
		case s.InfraError:
			stages[s.Name] = map[string]any{"infrastructureError": true}
		default:
			entry := map[string]any{"exit": s.ExitCode}
			if s.Failed {
				entry["failed"] = true
			}
			stages[s.Name] = entry
		}
	}
	data := map[string]any{
		"project":          rep.Project,
		"environment":      rep.Environment,
		"fromRelease":      rep.FromVersion,
		"fromBundleDigest": rep.FromBundleDigest,
		"toRelease":        rep.ToVersion,
		"toBundleDigest":   rep.ToBundleDigest,
		"target":           in.TargetManifest.Metadata.Name,
		"authorization":    authorization,
		"stagedFrom":       rep.StagedFrom.String(),
		"stagedTo":         rep.StagedTo.String(),
		"stages":           stages,
		"actor":            in.Owner,
	}
	if rep.RecoveryResolved {
		data["recoveryResolved"] = true
	}
	if rep.RecoveryMarkerCreated {
		data["recoveryMarkerCreated"] = true
	}
	if rep.AlreadyRecovered {
		data["alreadyRecovered"] = true
	}
	if rep.RecoveryRequired {
		data["recoveryRequired"] = true
	}
	if rep.RecoveryID != "" {
		data["recoveryId"] = rep.RecoveryID
	}
	if rep.Committed {
		data["committed"] = true
	}
	if rep.FailureReason != "" {
		data["reason"] = rep.FailureReason
	}
	seq, err := in.Target.AppendHistory(ctx, rep.Project, rep.Environment, target.Entry{
		Time: now().UTC().Format(time.RFC3339),
		Type: kind,
		Data: data,
	})
	if err != nil {
		return err
	}
	rep.HistorySeq = seq
	return nil
}
