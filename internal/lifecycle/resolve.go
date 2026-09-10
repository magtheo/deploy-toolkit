package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/target"
)

// ResolveInput asks for explicit resolution of unresolved markers: the
// operator has verified the target by hand and determined that the block
// may be lifted.
type ResolveInput struct {
	Target         *target.Target
	TargetManifest *manifest.Target
	// Project is the release project whose markers are being resolved —
	// the same identity the deployments used (release metadata), not the
	// target manifest's name.
	Project     string
	Environment *manifest.Environment
	Owner       string // actor identity recorded as evidence

	// ConfirmRecoveryID / ConfirmAttemptID are the marker identities the
	// operator is resolving. They are identity bindings, not decoration:
	// the engine re-reads the markers under the lock and refuses unless
	// the durable facts still carry exactly these ids — the operator
	// confirmed what they saw, and what they saw is what gets removed.
	ConfirmRecoveryID string // "" = the operator is not resolving a recovery
	ConfirmAttemptID  string // "" = the operator is not resolving an attempt

	// Now pins the clock (tests); nil means time.Now.
	Now func() time.Time
}

// ResolveReport is the outcome of an explicit resolution.
type ResolveReport struct {
	Project     string
	Environment string
	// ResolvedRecoveryID / ResolvedAttemptID name the markers actually
	// removed ("" when none of that kind was unresolved).
	ResolvedRecoveryID string
	ResolvedAttemptID  string
	// NothingToResolve: no marker was unresolved; the invocation is an
	// idempotent no-op.
	NothingToResolve bool
	// Left*ID names present markers the confirmation did NOT name: a
	// partial authorization leaves them (and the block) in place by
	// design.
	LeftRecoveryID string
	LeftAttemptID  string
	// AttemptPresentAfter / RecoveryPresentAfter are the engine-owned
	// blocked-state facts, set as soon as the under-lock reads land and
	// updated after each successful clear — so EVERY return path,
	// including history-write and marker-clear failures, truthfully
	// says what still lies on the target. AttemptMarkerID /
	// RecoveryMarkerID name what was present at read time ("" if
	// absent) so a failed resolution can name the remaining block.
	AttemptPresentAfter  bool
	RecoveryPresentAfter bool
	AttemptMarkerID      string
	RecoveryMarkerID     string
	// RecoveryRequired: after this invocation, does ANY marker still
	// block normal operation? Maintained on every return path.
	RecoveryRequired bool
	// Observed records what the target claimed at resolution time — the
	// state the operator verified against, preserved as evidence.
	ObservedRelease      string
	ObservedBundleDigest string
	ObservedOperationID  string
	HistorySeq           int64
}

// ErrResolveRefused marks refusals: the operation was not allowed
// (lock held, unreadable evidence, identity mismatch, inconsistent
// coexistence) — nothing was changed and the invocation can be corrected.
// It is distinct from infrastructure failures, which leave the outcome to
// be retried after repair.
var ErrResolveRefused = errors.New("resolve refused")

func refuseResolve(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrResolveRefused)
}

// Resolve is the explicit, supported end of an unresolved situation — the
// operation the runbook names after an operator has verified the target
// by hand. It executes NO hooks and changes NOTHING about what is
// running: it records who resolved what, against which observed state,
// and then removes the markers that block normal operation.
//
// It fails closed:
//
//   - the environment lock must be free — a held lock means an operation
//     may be executing right now, and resolving under it could delete
//     the live operation's own markers;
//   - observed state must be readable and valid — HEALTHY cannot be
//     restored to an environment whose state cannot be trusted;
//   - every marker present must match the confirmed identity — a
//     mismatch means the target changed since the operator looked.
//
// Evidence is recorded BEFORE the markers are removed (attempt first,
// recovery last), so a failed history write leaves the block in place
// and the invocation can simply be retried.
func Resolve(ctx context.Context, in ResolveInput) (*ResolveReport, error) {
	switch {
	case in.Target == nil:
		return nil, fmt.Errorf("ResolveInput.Target is required")
	case in.TargetManifest == nil:
		return nil, fmt.Errorf("ResolveInput.TargetManifest is required")
	case in.Project == "":
		return nil, fmt.Errorf("ResolveInput.Project is required")
	case in.Environment == nil:
		return nil, fmt.Errorf("ResolveInput.Environment is required")
	case in.Environment.Spec.Target == "":
		return nil, fmt.Errorf("environment %q declares no target", in.Environment.Metadata.Name)
	case in.Environment.Spec.Target != in.TargetManifest.Metadata.Name:
		return nil, fmt.Errorf("environment %q targets %q, but the resolution targets %q", in.Environment.Metadata.Name, in.Environment.Spec.Target, in.TargetManifest.Metadata.Name)
	}

	now := in.Now
	if now == nil {
		now = time.Now
	}
	rep := &ResolveReport{
		Project:     in.Project,
		Environment: in.Environment.Metadata.Name,
	}
	layout := in.Target.Layout()
	lockDir, err := layout.LockPath(rep.Project, rep.Environment)
	if err != nil {
		return rep, err
	}
	lock, err := AcquireEnvLock(ctx, in.Target.Transport(), lockDir, LockOwner{
		User:        in.Owner,
		Project:     rep.Project,
		Environment: rep.Environment,
		Release:     "resolve",
	})
	if err != nil {
		if errors.Is(err, ErrEnvLockHeld) {
			// Refusal classification, but keep BOTH sentinels
			// reachable: the held-lock fact must survive for callers
			// that classify by errors.Is.
			return rep, fmt.Errorf("acquire environment lock: %w: %w", err, ErrResolveRefused)
		}
		return rep, fmt.Errorf("acquire environment lock: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), lockCleanupTimeout)
		defer cancel()
		if relErr := lock.Release(cleanupCtx); relErr != nil {
			err = errors.Join(err, fmt.Errorf("environment lock %s could not be released (manual cleanup required): %w", lockDir, relErr))
		}
	}()

	// Observed state must be trustworthy before anything is resolved.
	// Absent is a normal, resolvable situation — the failed FIRST deploy
	// leaves no state at all. INVALID evidence (exists, fails strict
	// validation) is a refusal: nothing may be authorized against facts
	// that cannot be trusted. A transport failure is infrastructure.
	st, err := in.Target.ReadState(ctx, rep.Project, rep.Environment)
	if err != nil && !errors.Is(err, target.ErrStateAbsent) {
		if errors.Is(err, target.ErrEvidenceInvalid) {
			return rep, refuseResolve("observed state is invalid: %v", err)
		}
		return rep, fmt.Errorf("read observed state: %w", err)
	}
	if st.Current != nil {
		rep.ObservedRelease = st.Current.Release
		rep.ObservedBundleDigest = st.Current.BundleDigest
		rep.ObservedOperationID = st.Current.OperationID
	}

	attempt, aerr := in.Target.ReadAttempt(ctx, rep.Project, rep.Environment)
	attemptPresent := aerr == nil
	if aerr != nil && !errors.Is(aerr, target.ErrAttemptAbsent) {
		if errors.Is(aerr, target.ErrEvidenceInvalid) {
			return rep, refuseResolve("attempt marker is invalid: %v", aerr)
		}
		return rep, fmt.Errorf("read attempt marker: %w", aerr)
	}
	recovery, rerr := in.Target.ReadRecovery(ctx, rep.Project, rep.Environment)
	recoveryPresent := rerr == nil
	if rerr != nil && !errors.Is(rerr, target.ErrRecoveryAbsent) {
		if errors.Is(rerr, target.ErrEvidenceInvalid) {
			return rep, refuseResolve("recovery marker is invalid: %v", rerr)
		}
		return rep, fmt.Errorf("read recovery marker: %w", rerr)
	}

	// The blocked-state facts exist from the moment the reads land; the
	// clears below update them, and every failure path leaves them
	// truthful. RecoveryRequired is finalized on every return.
	rep.AttemptPresentAfter = attemptPresent
	rep.RecoveryPresentAfter = recoveryPresent
	if attemptPresent {
		rep.AttemptMarkerID = attempt.AttemptID
	}
	if recoveryPresent {
		rep.RecoveryMarkerID = recovery.RecoveryID
	}
	defer func() { rep.RecoveryRequired = rep.AttemptPresentAfter || rep.RecoveryPresentAfter }()

	if !attemptPresent && !recoveryPresent {
		rep.NothingToResolve = true
		return rep, nil
	}

	// Identity bindings: what the operator confirmed is what must lie on
	// the target. A mismatch means the situation changed since they
	// looked — refuse and let them look again.
	if in.ConfirmRecoveryID != "" {
		if !recoveryPresent || recovery.RecoveryID != in.ConfirmRecoveryID {
			return rep, refuseResolve("the confirmation names recovery %s, but the target does not carry that marker — re-run `status` and confirm what is actually there", in.ConfirmRecoveryID)
		}
	}
	if in.ConfirmAttemptID != "" {
		if !attemptPresent || attempt.AttemptID != in.ConfirmAttemptID {
			return rep, refuseResolve("the confirmation names attempt %s, but the target does not carry that marker — re-run `status` and confirm what is actually there", in.ConfirmAttemptID)
		}
	}
	if in.ConfirmRecoveryID == "" && in.ConfirmAttemptID == "" {
		return rep, refuseResolve("a resolution must name at least one marker id — refusing to clear unnamed facts")
	}
	if recoveryPresent && attemptPresent && in.ConfirmRecoveryID != "" && in.ConfirmAttemptID != "" {
		// Clearing BOTH together is only consistent as one story: this
		// recovery is resolving THIS attempt. An emergency rollback
		// carries sourceAttemptId "" (no source attempt) — a coexisting
		// attempt marker under it is evidence inconsistency, and must
		// never be erased in one authorization. Resolving ONE marker at
		// a time (leaving the other to keep the block) stays available:
		// that is the inspect-first path.
		if recovery.SourceAttemptID == "" || recovery.SourceAttemptID != attempt.AttemptID {
			return rep, refuseResolve("inconsistent evidence: the recovery marker (sourceAttemptId %q) coexists with attempt marker %s — inspect the target and resolve them one at a time", orEmpty(recovery.SourceAttemptID), attempt.AttemptID)
		}
	}

	// The pre-removal record is an AUTHORIZATION fact, deliberately not
	// named "resolved": it says the operator authorized clearing these
	// exact facts — it cannot promise the removal succeeded. If a
	// subsequent marker removal fails, durable evidence says
	// resolution-authorized while status still says RECOVERY REQUIRED,
	// and those agree: the block is still up. Only ResolveReport.Resolved*
	// (and the absence of the markers) claim completion. The event is
	// neutral because the operation may authorize either marker or both:
	// the confirmed ids carry the exact scope.
	kind := "resolution.authorized"
	data := map[string]any{
		"authorization":       "manual",
		"actor":               in.Owner,
		"target":              in.TargetManifest.Metadata.Name,
		"confirmedRecoveryId": in.ConfirmRecoveryID,
		"confirmedAttemptId":  in.ConfirmAttemptID,
		"observed": map[string]any{
			"release":     rep.ObservedRelease,
			"digest":      rep.ObservedBundleDigest,
			"operationId": rep.ObservedOperationID,
		},
	}
	seq, herr := in.Target.AppendHistory(ctx, rep.Project, rep.Environment, target.Entry{
		Time: now().UTC().Format(time.RFC3339),
		Type: kind,
		Data: data,
	})
	if herr != nil {
		// Evidence before removal: with the resolution unrecorded the
		// markers stay, the block stays, and the invocation can be
		// retried.
		return rep, fmt.Errorf("record resolution evidence: %w", herr)
	}
	rep.HistorySeq = seq

	// Attempt first, recovery last: a crash between the two removals
	// leaves the recovery marker — the more informative fact — in place.
	if in.ConfirmAttemptID != "" {
		if cerr := in.Target.ClearAttempt(ctx, rep.Project, rep.Environment); cerr != nil {
			return rep, fmt.Errorf("clear attempt marker %s: %w", attempt.AttemptID, cerr)
		}
		rep.ResolvedAttemptID = attempt.AttemptID
		rep.AttemptPresentAfter = false
	}
	if in.ConfirmRecoveryID != "" {
		if cerr := in.Target.ClearRecovery(ctx, rep.Project, rep.Environment); cerr != nil {
			return rep, fmt.Errorf("clear recovery marker %s: %w", recovery.RecoveryID, cerr)
		}
		rep.ResolvedRecoveryID = recovery.RecoveryID
		rep.RecoveryPresentAfter = false
	}
	// Facts confirmed for removal but left by a partial authorization
	// keep blocking; name them so the operator sees the block is not
	// fully lifted.
	if recoveryPresent && in.ConfirmRecoveryID == "" {
		rep.LeftRecoveryID = recovery.RecoveryID
	}
	if attemptPresent && in.ConfirmAttemptID == "" {
		rep.LeftAttemptID = attempt.AttemptID
	}
	return rep, nil
}

func orEmpty(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
