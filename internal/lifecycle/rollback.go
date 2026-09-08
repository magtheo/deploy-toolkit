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
	// RecoveryResolved: a pre-existing unresolved attempt marker was
	// cleared at the trusted terminal.
	RecoveryResolved bool
	// MarkerCreated: the rollback created its own recovery marker (the
	// emergency path — no unresolved attempt existed beforehand).
	MarkerCreated bool
	HistorySeq    int64
	FailureReason string
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
//	  → READ observed state + unresolved attempt marker
//	  → VALIDATE the recovery transition (exact identity bindings)
//	  → VERIFY rollback policy (authorization path + migration semantics)
//	  → STAGE + verify staged contracts for BOTH releases
//	  → To-release PREFLIGHT
//	  → marker: reuse, or create before the first consequential stage
//	  → From-release ROLLBACK hook (if its migration ran and hook declared)
//	  → To-release APPLY → VERIFY (mandatory)
//	  → COMMIT observed state = To release
//	  → CLEAR the attempt marker (trusted terminal)
//	  → APPEND structured rollback outcome
//	  → RELEASE lock
//
// Failure semantics mirror Deploy's, one notch stricter: everything before
// the marker write (stage, contracts, To-preflight) is retryable; once the
// marker exists, ANY failure — hook, transport, state commit — keeps the
// environment recovery-required. The marker is cleared only after the To
// release verified and observed state committed.
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
	defer func() {
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
		if herr := recordRollbackOutcome(ctx, in, now, rep, "rollback.failed"); herr != nil {
			return rep, errors.Join(fmt.Errorf("%s", reason), fmt.Errorf("history outcome not recorded: %w", herr))
		}
		return rep, nil
	}
	failInfra := func(what string, cause error) (*RollbackReport, error) {
		rep.FailureReason = what + " (infrastructure failure)"
		if herr := recordRollbackOutcome(ctx, in, now, rep, "rollback.failed"); herr != nil {
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
		// observed release is exactly the one being undone. A recovery
		// marker is created below, before the first consequential stage.
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
	runToStage := func(name string, step *manifest.LifecycleStep) (StageResult, error) {
		return runStageStep(ctx, in.Target.Transport(), name, toDir, toHenv, step)
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

	// Durable marker BEFORE the first consequential stage. On the
	// emergency path no marker exists yet; the recovery marker describes
	// the transition being performed (From = what runs now, To = what is
	// restored). An existing marker is reused untouched.
	if !haveMarker {
		attemptID, err := randomAttemptID()
		if err != nil {
			return rep, fmt.Errorf("generate attempt id: %w", err)
		}
		if werr := in.Target.WriteAttempt(ctx, target.AttemptMarker{
			AttemptID:    attemptID,
			Project:      rep.Project,
			Environment:  rep.Environment,
			FromRelease:  rep.FromVersion,
			ToRelease:    rep.ToVersion,
			BundleDigest: rep.ToBundleDigest,
			StartedAt:    now().UTC().Format(time.RFC3339),
		}); werr != nil {
			return failInfra("recovery marker could not be persisted; refusing consequential work", werr)
		}
		rep.MarkerCreated = true
	}

	// From-release ROLLBACK hook: undoes From-specific consequences —
	// above all its own migration. It runs only if the failed release's
	// migration actually ran (mode ≠ none) and its staged contract
	// declares the hook. The To release's forward migrate hook is
	// deliberately not run: its migration metadata describes deploying To
	// forward, not reversing From.
	if from.Migration.Mode != manifest.MigrationNone {
		sr, ierr = runStageStep(ctx, in.Target.Transport(), "rollback", fromDir, fromHenv, fromContract.Lifecycle.Rollback)
		rep.Stages = append(rep.Stages, sr)
		if ierr != nil {
			return failInfra("rollback could not be executed", ierr)
		}
		if sr.Failed {
			return failRollback("rollback hook failed")
		}
	}

	sr, ierr = runToStage("apply", toContract.Lifecycle.Apply)
	rep.Stages = append(rep.Stages, sr)
	if ierr != nil {
		return failInfra("apply could not be executed", ierr)
	}
	if sr.Failed {
		return failRollback("apply failed")
	}

	sr, ierr = runToStage("verify", toContract.Lifecycle.Verify)
	rep.Stages = append(rep.Stages, sr)
	if ierr != nil {
		return failInfra("verify could not be executed", ierr)
	}
	if sr.Failed {
		return failRollback("verify failed")
	}

	// Verified — and only verified — lets observed state move back.
	observed.Current = &target.CurrentDeployment{
		Release:      rep.ToVersion,
		BundleDigest: rep.ToBundleDigest,
		Since:        now().UTC().Format(time.RFC3339),
	}
	observed.UpdatedAt = now().UTC().Format(time.RFC3339)
	if werr := in.Target.WriteState(ctx, observed); werr != nil {
		return failInfra("observed state commit failed", werr)
	}
	rep.Committed = true

	// Trusted terminal: the restored release verified and the state
	// committed — the attempt (original or created here) is resolved.
	rep.RecoveryResolved = haveMarker
	if cerr := in.Target.ClearAttempt(ctx, rep.Project, rep.Environment); cerr != nil {
		err = errors.Join(fmt.Errorf("observed state committed, but the attempt marker could not be cleared: %w", cerr))
		if herr := recordRollbackOutcome(ctx, in, now, rep, "rollback.succeeded"); herr != nil {
			return rep, errors.Join(err, fmt.Errorf("history outcome not recorded: %w", herr))
		}
		return rep, err
	}
	if err := recordRollbackOutcome(ctx, in, now, rep, "rollback.succeeded"); err != nil {
		return rep, fmt.Errorf("record history outcome: %w", err)
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
// for a rollback outcome.
func recordRollbackOutcome(ctx context.Context, in RollbackInput, now func() time.Time, rep *RollbackReport, kind string) error {
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
		"authorization":    string(in.Authorization),
		"stagedFrom":       rep.StagedFrom.String(),
		"stagedTo":         rep.StagedTo.String(),
		"stages":           stages,
		"actor":            in.Owner,
	}
	if rep.RecoveryResolved {
		data["recoveryResolved"] = true
	}
	if rep.MarkerCreated {
		data["markerCreated"] = true
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
