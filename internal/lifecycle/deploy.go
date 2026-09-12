package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/target"
)

// StageResult is one lifecycle hook's outcome. Stdout/Stderr are returned
// to the caller for CI-console diagnosis; they are deliberately NOT part
// of anything persisted on the target — raw hook output can contain
// application secrets, and history must not.
//
// A non-zero exit code is a hook failure (the command ran and reported
// failure). A transport failure — unreachable target, StartError,
// cancellation — is NOT a hook outcome and never becomes one; it is
// returned as an infrastructure error by Deploy.
type StageResult struct {
	Name     string
	Skipped  bool
	ExitCode int
	Failed   bool
	// InfraError marks a stage that produced NO hook outcome because the
	// transport failed: the command may or may not have executed on the
	// target, and no exit code exists. Evidence renders this as an
	// infrastructure error, never as an exit code.
	InfraError bool
	// Unknown marks a stage whose execution fate is RunUnknown: the
	// process may still be running and no exit code will ever arrive.
	// This — not arbitrary transport trouble — is the fact that forces
	// the environment lock to be retained.
	Unknown bool
	Stdout  []byte
	Stderr  []byte
}

// Report describes one deployment attempt. A completed attempt with a
// failed hook or a refused condition is reported, not returned as an
// error: the outcome is a fact, and it is in the history log.
// Infrastructure failures are returned as errors.
type Report struct {
	Project          string
	Environment      string
	Version          string
	LockDir          string
	Staged           target.StageStatus
	BundleDigest     string
	ContractVerified bool
	Stages           []StageResult
	Committed        bool // observed state advanced (verify succeeded)
	AlreadyCurrent   bool // desired release+digest already observed; nothing consequential ran
	RecoveryRequired bool // an unresolved attempt marker exists; explicit recovery is needed
	// ConsequentialStarted is the durable-boundary fact: the attempt
	// marker was successfully persisted, so migrate/apply/verify may
	// have begun executing. An infrastructure error with this flag set
	// leaves the outcome UNKNOWN — it must be classified as uncertain,
	// never as safe-to-rerun. It is set immediately after the marker
	// write succeeds, so callers never reconstruct the boundary by
	// re-reading the target over a transport that may itself be dead.
	ConsequentialStarted bool
	// AttemptID is the identity of the attempt marker this invocation
	// wrote ("" until the boundary is crossed).
	AttemptID     string
	HistorySeq    int64
	FailureReason string
	// LockRetained records that the invocation deliberately did NOT
	// release its acquired environment lock because a lifecycle hook's
	// execution fate was RunUnknown — the hook process may still be
	// running. This is a controlled crash: the environment stays locked
	// pending human verification of the target, exactly as after a
	// controller crash. It is never set for a lock-release ATTEMPT that
	// failed (that surfaces as a joined error, a different situation).
	LockRetained bool
}

// DeployInput carries the desired state (parsed through the manifest
// pipeline by the caller) and the substrate bindings.
//
// The bundle arrives as prepared canonical bytes — the product of the
// PREPARE side, which owns Git and the source checkout. This is the
// prepare/deploy trust split: the deployment operation holds only the
// target credential, never the source repository. Stage verifies the
// bytes against Release.bundle.digest before anything is written.
type DeployInput struct {
	Target         *target.Target
	TargetManifest *manifest.Target // required: cross-checked against the Environment
	Environment    *manifest.Environment
	Release        *manifest.Release
	Bundle         []byte // prepared canonical bundle bytes (prepare side)
	Owner          string // lock owner identity, e.g. "runner:x/y@id"
	Now            func() time.Time
}

// Deploy executes the full sequence. The returned error covers
// infrastructure failures (incomplete input, lock, transport, cancellation);
// deployment outcomes — failed hooks, contract mismatches, drift refusals —
// come back as a Report with FailureReason set and Committed false.
func Deploy(ctx context.Context, in DeployInput) (rep *Report, err error) {
	// Fail closed on incomplete input instead of dereferencing nils.
	switch {
	case in.Target == nil:
		return nil, fmt.Errorf("DeployInput.Target is required")
	case in.TargetManifest == nil:
		return nil, fmt.Errorf("DeployInput.TargetManifest is required")
	case in.Environment == nil:
		return nil, fmt.Errorf("DeployInput.Environment is required")
	case in.Release == nil:
		return nil, fmt.Errorf("DeployInput.Release is required")
	case len(in.Bundle) == 0:
		return nil, fmt.Errorf("DeployInput.Bundle is required (prepared canonical bundle bytes from the prepare side)")
	}
	now := in.Now
	if now == nil {
		now = time.Now
	}
	rel := in.Release
	env := in.Environment
	rep = &Report{
		Project:      rel.Metadata.Project,
		Environment:  env.Metadata.Name,
		Version:      rel.Metadata.Version,
		BundleDigest: rel.Bundle.Digest,
	}
	if err := validateDesired(in); err != nil {
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
		Release:     rep.Version,
	})
	if err != nil {
		return rep, err
	}
	// releaseLock is lifecycle-local retain state: the lock is released
	// on every return path EXCEPT when a hook's execution fate was
	// RunUnknown. EnvLock.Release stays literal — "attempt to remove the
	// lock" — and is simply never called for a deliberate retention.
	releaseLock := true
	defer func() {
		if !releaseLock {
			return
		}
		// Cleanup must survive a cancelled deploy context but must not run
		// unbounded: a fresh, bounded context. Lock-release failure is a
		// hard failure — the environment is blocked until an operator
		// cleans up — so it is joined with any existing error, never
		// swallowed by it.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), lockCleanupTimeout)
		defer cancel()
		if relErr := lock.Release(cleanupCtx); relErr != nil {
			// Carry the sentinel so rendering can surface the compound
			// state instead of a single classification.
			err = errors.Join(err, fmt.Errorf("environment lock %s could not be released (manual cleanup required): %w: %w", lockDir, ErrLockReleaseFailed, relErr))
		}
	}()

	failDeployment := func(reason string) (*Report, error) {
		rep.FailureReason = reason
		// The attempt marker is deliberately NOT cleared here. Once it
		// exists, a failure — even a determined hook exit — means a
		// consequential deployment has not been reconciled to trusted
		// observed state: a migration can partially apply and then exit 1,
		// so "known failure" is not "safe to repeat consequential work".
		// The marker survives until explicit recovery (the rollback
		// operation) resolves it, or until committed observed state proves
		// the attempt finished (the self-heal below). Failures before the marker is
		// written — validate, stage, contract, preflight — leave no
		// marker and remain retryable.
		//
		// Consistency rule: a determined failure AFTER the durable
		// boundary leaves the marker, so the environment is now
		// recovery-required. The report, history and CLI output must
		// say so on the FIRST failure — not only after a later status
		// or refused retry reveals it.
		if rep.ConsequentialStarted && !rep.Committed {
			rep.RecoveryRequired = true
		}
		if herr := recordOutcome(ctx, in, now, rep, "deploy.failed"); herr != nil {
			return rep, errors.Join(fmt.Errorf("%s", reason), fmt.Errorf("history outcome not recorded: %w", herr))
		}
		return rep, nil
	}
	failInfra := func(what string, cause error) (*Report, error) {
		rep.FailureReason = what + " (infrastructure failure)"
		if herr := recordOutcome(ctx, in, now, rep, "deploy.failed"); herr != nil {
			cause = errors.Join(cause, fmt.Errorf("history outcome not recorded: %w", herr))
		}
		return rep, fmt.Errorf("%s: %w", what, cause)
	}

	// Observed state: a fresh target has none — that is normal, not fatal.
	observed, err := in.Target.ReadState(ctx, rep.Project, rep.Environment)
	if err != nil && !errors.Is(err, target.ErrStateAbsent) {
		return rep, fmt.Errorf("read observed state: %w", err)
	}
	if errors.Is(err, target.ErrStateAbsent) {
		observed = target.State{Project: rep.Project, Environment: rep.Environment}
	}

	// Unresolved-attempt check: a marker means the previous deployment may
	// have performed consequential work with an unknown outcome. Two
	// trusted facts resolve it: committed observed state for this exact
	// release (the attempt did finish — heal the leftover marker), or
	// nothing — in which case normal retry is refused until explicit
	// recovery (the rollback operation) resolves the attempt.
	attempt, aerr := in.Target.ReadAttempt(ctx, rep.Project, rep.Environment)
	if aerr != nil && !errors.Is(aerr, target.ErrAttemptAbsent) {
		return rep, fmt.Errorf("read attempt marker: %w", aerr)
	}

	// An unresolved RECOVERY marker is a harder stop: a previous rollback
	// may have performed consequential work with an unknown outcome, and
	// production may be between the two releases. Deploying anything —
	// even the release the recovery was restoring — would repeat
	// consequential work into an unknown target. Resolution is the
	// rollback operation itself (which self-heals a committed-but-unclean
	// recovery via the observed state's operationId) or explicit operator
	// cleanup after verifying the target.
	if _, rerr := in.Target.ReadRecovery(ctx, rep.Project, rep.Environment); rerr == nil {
		rep.RecoveryRequired = true
		return failDeployment("unresolved recovery marker: a previous rollback has not been reconciled to trusted observed state — run the recovery to resolution or resolve it explicitly before deploying")
	} else if !errors.Is(rerr, target.ErrRecoveryAbsent) {
		return rep, fmt.Errorf("read recovery marker: %w", rerr)
	}
	if aerr == nil {
		// Self-heal requires the FULL identity match — the same triple
		// standard the rollback self-heal demands (operationId proves
		// WHICH operation committed, release identity alone proves
		// nothing): the marker must describe an attempt to exactly the
		// requested release with exactly the requested bundle digest,
		// AND committed observed state must show that same release with
		// that same digest SIGNED by this attempt's id. Only then does
		// observed state prove that THIS attempt reached its trusted
		// terminal. A marker for a different target release (e.g. an
		// unresolved 2.0.0 attempt while 1.0.0 is observed) is never
		// erased by deploying the currently observed release — and a
		// hand-forged marker naming the currently observed release is
		// never erased without the commit signature either.
		if observed.Current != nil &&
			attempt.ToRelease == rep.Version && attempt.BundleDigest == rel.Bundle.Digest &&
			observed.Current.Release == rep.Version && observed.Current.BundleDigest == rel.Bundle.Digest &&
			observed.Current.OperationID == "deploy:"+attempt.AttemptID {
			// The attempt reached its trusted terminal; the marker is a
			// leftover from a crash between commit and cleanup.
			if cerr := in.Target.ClearAttempt(ctx, rep.Project, rep.Environment); cerr != nil {
				return rep, fmt.Errorf("clear stale attempt marker: %w", cerr)
			}
		} else {
			rep.RecoveryRequired = true
			cur := "none"
			if observed.Current != nil {
				cur = observed.Current.Release + "@" + observed.Current.BundleDigest
			}
			return failDeployment(fmt.Sprintf(
				"unresolved deployment attempt %s (%s → %s, started %s) with observed state %s: consequential work may have executed and the outcome is unknown — explicit recovery is required before retrying",
				attempt.AttemptID, attempt.FromRelease, attempt.ToRelease, attempt.StartedAt, cur))
		}
	}

	// Idempotency guard: observed state is a decision input, not a log.
	if observed.Current != nil {
		switch {
		case observed.Current.Release == rep.Version && observed.Current.BundleDigest == rel.Bundle.Digest:
			// Already current: the environment is running exactly the
			// desired release. Re-running migrate/apply is not idempotent
			// in general — and a retry after a lost history write must
			// never repeat consequential work. Record the fact and stop.
			staged, serr := in.Target.Stage(ctx, rel, in.Bundle, now())
			if serr != nil {
				return rep, fmt.Errorf("stage release: %w", serr)
			}
			rep.Staged = staged
			rep.AlreadyCurrent = true
			if herr := recordOutcome(ctx, in, now, rep, "deploy.already-current"); herr != nil {
				return rep, fmt.Errorf("record history outcome: %w", herr)
			}
			return rep, nil
		case observed.Current.Release == rep.Version:
			// Same release identity, different observed digest: the state
			// and the release have drifted apart. Refuse — this is not a
			// deployment condition anyone asked for.
			return failDeployment(fmt.Sprintf(
				"state drift: observed release %s carries bundle digest %s, but the release pins %s — refusing to redeploy over drifted state",
				rep.Version, observed.Current.BundleDigest, rel.Bundle.Digest))
		}
		// A different release is current: proceed with the upgrade.
	}

	// Stage: digest-verified, marker-last, idempotent. Nothing here can
	// affect the running service.
	staged, err := in.Target.Stage(ctx, rel, in.Bundle, now())
	if err != nil {
		return rep, fmt.Errorf("stage release: %w", err)
	}
	rep.Staged = staged

	// The lifecycle contract comes from the STAGED release, byte for byte:
	// read what actually landed on the target, hash it, match it against
	// the release's pinned digest, and parse THAT manifest for hooks.
	stagedProject, err := verifyStagedContract(ctx, in.Target, rel)
	if err != nil {
		if errors.Is(err, errContractRefused) {
			return failDeployment(err.Error())
		}
		return failInfra("staged deployment contract unreadable", err)
	}
	rep.ContractVerified = true
	if stagedProject.Lifecycle.Verify == nil {
		// Defense in depth: the schema already requires verify. State
		// must never advance on a skipped verification.
		return failDeployment("staged deployment contract declares no verify hook; verify is mandatory")
	}
	releaseDir, err := layout.ReleaseDir(rep.Project, rep.Version)
	if err != nil {
		return rep, err
	}

	henv, err := hookEnv(rel, rep.Environment)
	if err != nil {
		return rep, fmt.Errorf("hook environment: %w", err)
	}

	// Exit codes are hook outcomes; Run errors are infrastructure errors.
	// A RunUnknown fate additionally retains the environment lock — for
	// EVERY hook, including preflight: the hook-execution boundary and
	// the durable attempt boundary are different, and a possibly-still-
	// running preflight process is exactly what the lock exists to guard.
	runStage := func(name string, step *manifest.LifecycleStep) (StageResult, error) {
		sr, err := runStageStep(ctx, in.Target.Transport(), name, releaseDir, henv, step)
		if sr.Unknown {
			releaseLock = false
			rep.LockRetained = true
		}
		return sr, err
	}

	// Staging is non-impacting; preflight decides whether the staged
	// release may begin consequential execution.
	sr, ierr := runStage("preflight", stagedProject.Lifecycle.Preflight)
	rep.Stages = append(rep.Stages, sr)
	if ierr != nil {
		return failInfra("preflight could not be executed", ierr)
	}
	if sr.Failed {
		return failDeployment("preflight failed")
	}

	// Durable attempt marker BEFORE the first consequential stage. From
	// here until a trusted terminal, an interrupted attempt leaves this
	// marker behind — and the next deployment refuses rather than
	// re-running migrate/apply into an unknown target state.
	attemptID, err := randomHexID()
	if err != nil {
		return rep, fmt.Errorf("generate attempt id: %w", err)
	}
	fromRelease := ""
	if observed.Current != nil {
		fromRelease = observed.Current.Release
	}
	rep.AttemptID = attemptID
	if err := in.Target.WriteAttempt(ctx, target.AttemptMarker{
		AttemptID:    attemptID,
		Project:      rep.Project,
		Environment:  rep.Environment,
		FromRelease:  fromRelease,
		ToRelease:    rep.Version,
		BundleDigest: rel.Bundle.Digest,
		StartedAt:    now().UTC().Format(time.RFC3339),
	}); err != nil {
		return failInfra("attempt marker could not be persisted; refusing consequential work", err)
	}
	rep.ConsequentialStarted = true

	// Migration hooks follow the release's declared migration semantics:
	// mode none means no migration runs even if a hook is declared.
	var migrateStep *manifest.LifecycleStep
	if rel.Migration.Mode != manifest.MigrationNone {
		migrateStep = stagedProject.Lifecycle.Migrate
	}
	sr, ierr = runStage("migrate", migrateStep)
	rep.Stages = append(rep.Stages, sr)
	if ierr != nil {
		if sr.Unknown {
			// The attempt marker exists (written before this stage), so
			// recovery is required as a matter of fact, not of caution.
			rep.RecoveryRequired = true
		}
		return failInfra("migrate could not be executed", ierr)
	}
	if sr.Failed {
		return failDeployment("migrate failed")
	}

	sr, ierr = runStage("apply", stagedProject.Lifecycle.Apply)
	rep.Stages = append(rep.Stages, sr)
	if ierr != nil {
		if sr.Unknown {
			rep.RecoveryRequired = true
		}
		return failInfra("apply could not be executed", ierr)
	}
	if sr.Failed {
		return failDeployment("apply failed")
	}

	sr, ierr = runStage("verify", stagedProject.Lifecycle.Verify)
	rep.Stages = append(rep.Stages, sr)
	if ierr != nil {
		if sr.Unknown {
			rep.RecoveryRequired = true
		}
		return failInfra("verify could not be executed", ierr)
	}
	if sr.Failed {
		return failDeployment("verify failed")
	}

	// Verify succeeded — and only verify success — lets observed state
	// advance. staged ≠ deployed; apply success ≠ verified. The commit is
	// signed with this attempt's id: it is the durable proof of WHICH
	// operation produced this observation.
	observed.Current = &target.CurrentDeployment{
		Release:      rep.Version,
		BundleDigest: rel.Bundle.Digest,
		Since:        now().UTC().Format(time.RFC3339),
		OperationID:  "deploy:" + attemptID,
	}
	observed.UpdatedAt = now().UTC().Format(time.RFC3339)
	if err := in.Target.WriteState(ctx, observed); err != nil {
		// The state commit is the trusted terminal; failing here leaves
		// the outcome uncertain (the service may be on the new release
		// while observed state says otherwise). The attempt marker stays,
		// so a retry is refused until recovery — never re-run blindly.
		// RecoveryRequired is set HERE, not only in failDeployment: the
		// marker exists, so recovery is a fact of this report regardless
		// of which failure shape carries it.
		rep.RecoveryRequired = true
		return failInfra("observed state commit failed", err)
	}
	rep.Committed = true

	// Trusted terminal reached: clear the attempt marker. A failure here
	// is loud but self-healing — the next deployment sees committed state
	// matching desired and clears the leftover.
	if cerr := in.Target.ClearAttempt(ctx, rep.Project, rep.Environment); cerr != nil {
		err = errors.Join(fmt.Errorf("observed state committed, but the attempt marker could not be cleared: %w", cerr))
		if herr := recordOutcome(ctx, in, now, rep, "deploy.succeeded"); herr != nil {
			return rep, errors.Join(err, fmt.Errorf("history outcome not recorded: %w", herr))
		}
		return rep, err
	}
	if err := recordOutcome(ctx, in, now, rep, "deploy.succeeded"); err != nil {
		return rep, fmt.Errorf("record history outcome: %w", err)
	}
	return rep, nil
}

// randomHexID generates a 16-hex-digit operation id, used for attempt and
// recovery markers.
func randomHexID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func recordOutcome(ctx context.Context, in DeployInput, now func() time.Time, rep *Report, kind string) error {
	// Evidence finalization deliberately ignores the caller's context: a
	// cancelled deadline must not cost the operator the record of what
	// happened. See evidenceCtx for the strict boundary.
	ctx, cancel := evidenceCtx()
	defer cancel()
	data := outcomeData(rep, in)
	if kind == "deploy.succeeded" {
		// The state commit is part of the success fact.
		data["committed"] = true
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

// validateDesired cross-checks the desired manifests against each other
// before anything touches the target: the Environment must point at this
// Target and at exactly this Release.
func validateDesired(in DeployInput) error {
	env := in.Environment
	rel := in.Release
	if env.Spec.Target == "" {
		return fmt.Errorf("environment %q declares no target", env.Metadata.Name)
	}
	if env.Spec.Target != in.TargetManifest.Metadata.Name {
		return fmt.Errorf("environment %q targets %q, but the deployment targets %q", env.Metadata.Name, env.Spec.Target, in.TargetManifest.Metadata.Name)
	}
	wantRef := fmt.Sprintf(".deploy/releases/%s-%s.yaml", rel.Metadata.Project, rel.Metadata.Version)
	if env.Spec.Release != wantRef {
		return fmt.Errorf("environment %q pins release %q, but the deployment is for %q", env.Metadata.Name, env.Spec.Release, wantRef)
	}
	if err := target.CheckVersion(rel.Metadata.Version); err != nil {
		return fmt.Errorf("release version: %w", err)
	}
	return nil
}

// outcomeData is the structured, secret-free history payload: identities,
// digests, per-stage exit codes, timestamps. Raw hook output never enters.
func outcomeData(rep *Report, in DeployInput) map[string]any {
	stages := make(map[string]any, len(rep.Stages))
	for _, s := range rep.Stages {
		switch {
		case s.Skipped:
			stages[s.Name] = "skipped"
		case s.InfraError:
			// No exit code exists — never manufacture one.
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
		"release":          rep.Version,
		"bundleDigest":     rep.BundleDigest,
		"target":           in.TargetManifest.Metadata.Name,
		"staged":           rep.Staged.String(),
		"contractVerified": rep.ContractVerified,
		"stages":           stages,
		"actor":            in.Owner,
	}
	if rep.AlreadyCurrent {
		data["alreadyCurrent"] = true
	}
	if rep.RecoveryRequired {
		data["recoveryRequired"] = true
	}
	if rep.Committed {
		data["committed"] = true
	}
	if rep.FailureReason != "" {
		data["reason"] = rep.FailureReason
	}
	return data
}
