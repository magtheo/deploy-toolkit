package main

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/magtheo/deploy-toolkit/internal/lifecycle"
	"github.com/magtheo/deploy-toolkit/internal/target"
)

// The public machine interface. --json emits exactly one versioned
// document on stdout — no human prose — whose SEMANTIC fields are the
// automation contract: outcome, committed, recoveryRequired, safeToRetry,
// boundary identities, desired/observed state, lock and marker facts.
// Exit codes remain broad process categories; decisions are made from
// fields, never from the exit code alone.
const resultSchemaV1 = "deployctl.result/v1"

// Outcome vocabulary (deployctl.result/v1):
//
//	success                the command did what was asked, including
//	                       deliberate no-ops (already-current,
//	                       nothing-to-resolve) and partial resolutions
//	failure                determined outcome failure; history records it
//	refused                the invocation was not authorized/allowed;
//	                       nothing consequential was attempted
//	uncertain              consequential work MAY have executed and the
//	                       outcome is unknown; recoveryRequired is true
//	infrastructure-failure infrastructure broke; the data says whether the
//	                       consequential boundary was crossed
//	usage-error            command syntax/configuration; nothing ran
const (
	outcomeSuccess     = "success"
	outcomeFailure     = "failure"
	outcomeRefused     = "refused"
	outcomeUncertain   = "uncertain"
	outcomeInfraFailed = "infrastructure-failure"
	outcomeUsageError  = "usage-error"
)

type resultEnvelope struct {
	Schema      string `json:"schema"`
	Command     string `json:"command"`
	Outcome     string `json:"outcome"`
	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`
	// LockReleaseFailed (compatible v1 envelope extension): the
	// invocation's environment lock could not be released — the
	// environment stays locked until manual cleanup, whatever the
	// outcome classification. See cli-v1.md.
	LockReleaseFailed bool `json:"lockReleaseFailed,omitempty"`
	// StagingLockReleaseFailed (compatible v1 envelope extension): the
	// invocation's staging lock (.staging/<version>) could not be
	// released — future stages of that release version refuse until
	// manual cleanup, whatever the outcome classification. Distinct
	// from lockReleaseFailed, which means the environment lock.
	StagingLockReleaseFailed bool `json:"stagingLockReleaseFailed,omitempty"`
	// RecoveryRequired: an unresolved attempt/recovery marker (or a
	// partial resolution leaving one) blocks normal operation.
	RecoveryRequired bool `json:"recoveryRequired"`
	// SafeToRetry: re-running the same command after the infrastructure
	// problem is fixed is known-safe (no consequential work executed).
	// False for every other outcome.
	SafeToRetry bool   `json:"safeToRetry"`
	Message     string `json:"message"`
	Data        any    `json:"data,omitempty"`
}

type stageOutcome struct {
	Name     string `json:"name"`
	Status   string `json:"status"` // ok | failed | skipped | infrastructure-error
	ExitCode *int   `json:"exitCode,omitempty"`
}

func stageOutcomes(stages []lifecycle.StageResult) []stageOutcome {
	out := make([]stageOutcome, 0, len(stages))
	for _, s := range stages {
		so := stageOutcome{Name: s.Name}
		switch {
		case s.Skipped:
			so.Status = "skipped"
		case s.InfraError:
			so.Status = "infrastructure-error"
		case s.Failed:
			so.Status = "failed"
			code := s.ExitCode
			so.ExitCode = &code
		default:
			so.Status = "ok"
			code := s.ExitCode
			so.ExitCode = &code
		}
		out = append(out, so)
	}
	return out
}

func emitJSON(stdout io.Writer, env *resultEnvelope) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		return exitInfra
	}
	return exitByOutcome(env.Outcome)
}

func exitByOutcome(outcome string) int {
	switch outcome {
	case outcomeSuccess:
		return exitOK
	case outcomeFailure, outcomeRefused:
		return exitFailed
	case outcomeUsageError:
		return exitUsage
	default:
		return exitInfra
	}
}

func usageErrorResult(command, envName, message string) *resultEnvelope {
	return &resultEnvelope{
		Schema:      resultSchemaV1,
		Command:     command,
		Outcome:     outcomeUsageError,
		Environment: envName,
		Message:     message,
	}
}

// ---- deploy ------------------------------------------------------------

type deployResultData struct {
	Committed            bool `json:"committed"`
	AlreadyCurrent       bool `json:"alreadyCurrent"`
	ConsequentialStarted bool `json:"consequentialStarted"`
	// LockRetained (compatible v1 extension): the invocation
	// deliberately did not release its acquired environment lock
	// because a lifecycle hook's execution fate could not be
	// established. Verify the target, remove the lock by hand, then
	// resolve any marker; see cli-v1.md.
	LockRetained  bool           `json:"lockRetained,omitempty"`
	AttemptID     string         `json:"attemptId,omitempty"`
	Version       string         `json:"version,omitempty"`
	BundleDigest  string         `json:"bundleDigest,omitempty"`
	FailureReason string         `json:"failureReason,omitempty"`
	Stages        []stageOutcome `json:"stages,omitempty"`
}

func deployResult(rep *lifecycle.Report, err error) (*resultEnvelope, int) {
	env := &resultEnvelope{Schema: resultSchemaV1, Command: cmdDeploy}
	data := &deployResultData{}
	env.Data = data
	failureStages := false
	if rep != nil {
		env.Project, env.Environment = rep.Project, rep.Environment
		*data = deployResultData{
			Committed:            rep.Committed,
			AlreadyCurrent:       rep.AlreadyCurrent,
			ConsequentialStarted: rep.ConsequentialStarted,
			LockRetained:         rep.LockRetained,
			AttemptID:            rep.AttemptID,
			Version:              rep.Version,
			BundleDigest:         rep.BundleDigest,
			FailureReason:        rep.FailureReason,
			Stages:               stageOutcomes(rep.Stages),
		}
		for _, s := range rep.Stages {
			if s.Failed {
				failureStages = true
			}
		}
		env.RecoveryRequired = rep.RecoveryRequired
	}
	switch {
	case err != nil:
		switch {
		case errors.Is(err, lifecycle.ErrEnvLockHeld):
			// A held lock is a REFUSAL per the frozen cli-v1
			// contract: another operation may currently be executing.
			// It is never safe-to-retry infrastructure.
			env.Outcome = outcomeRefused
			env.Message = "refused: the environment lock is held — another operation may currently be executing"
		case errors.Is(err, target.ErrEvidenceInvalid):
			// Invalid durable evidence is a REFUSAL, mirroring the
			// resolve preflight: the evidence exists but fails strict
			// validation, so the facts cannot be trusted and nothing
			// may be decided. Retrying is pointless until the evidence
			// is repaired; it is infrastructure-shaped only in the
			// transport sense, never in the safe-to-retry sense.
			env.Outcome = outcomeRefused
			env.Message = "refused: durable evidence on the target exists but is invalid — run deployctl status, inspect the evidence and verify the target before recovery; do not rerun"
		case errors.Is(err, target.ErrStageLockHeld):
			// The release version is being staged by another
			// environment's operation right now: refusal, same class
			// as a held environment lock.
			env.Outcome = outcomeRefused
			env.Message = "refused: this release version is being staged by another operation — wait for it to finish, or remove a crashed stager's lock after verifying"
		case rep != nil && rep.Committed:
			env.Outcome = outcomeInfraFailed
			env.Message = "the observed state is committed; post-commit bookkeeping failed"
		case rep != nil && rep.ConsequentialStarted:
			env.Outcome = outcomeUncertain
			env.RecoveryRequired = true
			env.SafeToRetry = false
			env.Message = "outcome uncertain: consequential deployment work may have executed (attempt " + orNone(rep.AttemptID) + " unresolved)"
			if rep.LockRetained {
				env.Message += " — the environment lock was deliberately retained because a hook's execution fate could not be established; verify the target, remove the lock, then resolve the attempt"
			}
		case rep != nil && rep.LockRetained:
			// Pre-boundary unknown fate: still possibly-running work,
			// so the lock survives and retrying is NOT safe — but no
			// attempt marker exists, so this is infrastructure-failure
			// shape, not uncertain.
			env.Outcome = outcomeInfraFailed
			env.SafeToRetry = false
			env.Message = "infrastructure failure: a lifecycle hook's execution fate could not be established — the environment lock was deliberately retained; verify no hook is still executing, remove the lock, then retry deliberately: " + err.Error()
		default:
			env.Outcome = outcomeInfraFailed
			env.SafeToRetry = true
			env.Message = "infrastructure failure before consequential work: " + err.Error()
		}
	case rep.AlreadyCurrent:
		env.Outcome = outcomeSuccess
		env.Message = "already current; no consequential work was repeated"
	case rep.Committed:
		env.Outcome = outcomeSuccess
		env.Message = "deployment verified and committed"
	case !rep.ConsequentialStarted && !failureStages && rep.FailureReason != "":
		env.Outcome = outcomeRefused
		env.Message = "refused: " + rep.FailureReason
	default:
		env.Outcome = outcomeFailure
		env.Message = "deploy failed: " + rep.FailureReason
	}
	noteLockReleaseFailed(env, err)
	noteStagingLockReleaseFailed(env, err)
	return env, exitByOutcome(env.Outcome)
}

// noteLockReleaseFailed surfaces the COMPOUND state: whatever the
// classification (refusal, failure, uncertain, infrastructure), a
// lock-release failure is an additional fact that must never hide
// behind it — the environment stays locked until manual cleanup.
func noteLockReleaseFailed(env *resultEnvelope, err error) {
	if err == nil || !errors.Is(err, lifecycle.ErrLockReleaseFailed) {
		return
	}
	// The structured fact is the contract; the prose is a courtesy.
	env.LockReleaseFailed = true
	env.Message += " — THE ENVIRONMENT LOCK COULD NOT BE RELEASED: manual cleanup is required; no other operation may start until it is removed"
}

// noteStagingLockReleaseFailed mirrors noteLockReleaseFailed for the
// staging lock: a release-failure of .staging/<version> blocks every
// future stage of that version, so it must surface as a structured
// fact on any outcome — deploy and rollback both stage.
func noteStagingLockReleaseFailed(env *resultEnvelope, err error) {
	if err == nil || !errors.Is(err, target.ErrStageLockReleaseFailed) {
		return
	}
	env.StagingLockReleaseFailed = true
	// A leftover staging lock refuses every future stage of this
	// version until an operator removes it, so a rerun is NOT known-
	// safe — the same reasoning as a retained environment lock.
	env.SafeToRetry = false
	env.Message += " — THE STAGING LOCK COULD NOT BE RELEASED: manual cleanup is required; future stages of this release version refuse until it is removed"
}

// ---- rollback ----------------------------------------------------------

type rollbackResultData struct {
	Committed            bool `json:"committed"`
	AlreadyRecovered     bool `json:"alreadyRecovered"`
	ConsequentialStarted bool `json:"recoveryStarted"`
	// LockRetained (compatible v1 extension): mirror of deploy.
	LockRetained  bool           `json:"lockRetained,omitempty"`
	RecoveryID    string         `json:"recoveryId,omitempty"`
	FromVersion   string         `json:"fromVersion,omitempty"`
	ToVersion     string         `json:"toVersion,omitempty"`
	FailureReason string         `json:"failureReason,omitempty"`
	Stages        []stageOutcome `json:"stages,omitempty"`
}

func rollbackResult(rep *lifecycle.RollbackReport, err error) (*resultEnvelope, int) {
	env := &resultEnvelope{Schema: resultSchemaV1, Command: cmdRollback}
	data := &rollbackResultData{}
	env.Data = data
	failureStages := false
	if rep != nil {
		env.Project, env.Environment = rep.Project, rep.Environment
		*data = rollbackResultData{
			Committed:            rep.Committed,
			AlreadyRecovered:     rep.AlreadyRecovered,
			ConsequentialStarted: rep.RecoveryStarted,
			LockRetained:         rep.LockRetained,
			RecoveryID:           rep.RecoveryID,
			FromVersion:          rep.FromVersion,
			ToVersion:            rep.ToVersion,
			FailureReason:        rep.FailureReason,
			Stages:               stageOutcomes(rep.Stages),
		}
		for _, s := range rep.Stages {
			if s.Failed {
				failureStages = true
			}
		}
		env.RecoveryRequired = rep.RecoveryRequired
	}
	switch {
	case err != nil:
		switch {
		case errors.Is(err, lifecycle.ErrEnvLockHeld):
			// Mirror deploy: a held lock is a refusal, never
			// safe-to-retry infrastructure.
			env.Outcome = outcomeRefused
			env.Message = "refused: the environment lock is held — another operation may currently be executing"
		case errors.Is(err, target.ErrEvidenceInvalid):
			// Mirror deploy: invalid durable evidence is a refusal.
			env.Outcome = outcomeRefused
			env.Message = "refused: durable evidence on the target exists but is invalid — run deployctl status, inspect the evidence and verify the target before recovery; do not rerun"
		case errors.Is(err, target.ErrStageLockHeld):
			// The release version is being staged by another
			// environment's operation right now: refusal, same class
			// as a held environment lock.
			env.Outcome = outcomeRefused
			env.Message = "refused: this release version is being staged by another operation — wait for it to finish, or remove a crashed stager's lock after verifying"
		case rep != nil && rep.Committed:
			env.Outcome = outcomeInfraFailed
			env.Message = "the observed state is committed; post-commit bookkeeping failed"
		case rep != nil && rep.AlreadyRecovered:
			env.Outcome = outcomeInfraFailed
			env.Message = "the recovery is already committed; post-commit bookkeeping failed"
		case rep != nil && rep.RecoveryStarted:
			env.Outcome = outcomeUncertain
			env.RecoveryRequired = true
			env.SafeToRetry = false
			env.Message = "outcome uncertain: consequential recovery work may have executed (recovery " + orNone(rep.RecoveryID) + " unresolved)"
			if rep.LockRetained {
				env.Message += " — the environment lock was deliberately retained because a hook's execution fate could not be established; verify the target, remove the lock, then resolve the recovery"
			}
		case rep != nil && rep.LockRetained:
			// Pre-boundary unknown fate (mirror of deploy).
			env.Outcome = outcomeInfraFailed
			env.SafeToRetry = false
			env.Message = "infrastructure failure: a lifecycle hook's execution fate could not be established — the environment lock was deliberately retained; verify no hook is still executing, remove the lock, then retry deliberately: " + err.Error()
		default:
			env.Outcome = outcomeInfraFailed
			env.SafeToRetry = true
			env.Message = "infrastructure failure before consequential work: " + err.Error()
		}
	case rep.AlreadyRecovered:
		env.Outcome = outcomeSuccess
		env.Message = "recovery already committed; markers cleaned up, nothing executed"
	case rep.Committed:
		env.Outcome = outcomeSuccess
		env.Message = "restored release verified and committed"
	case !rep.RecoveryStarted && !failureStages && rep.FailureReason != "":
		env.Outcome = outcomeRefused
		env.Message = "refused: " + rep.FailureReason
	default:
		env.Outcome = outcomeFailure
		env.Message = "rollback failed: " + rep.FailureReason
	}
	noteLockReleaseFailed(env, err)
	noteStagingLockReleaseFailed(env, err)
	return env, exitByOutcome(env.Outcome)
}

// ---- status ------------------------------------------------------------

type releaseRef struct {
	Version      string `json:"version"`
	BundleDigest string `json:"bundleDigest"`
	Since        string `json:"since,omitempty"`
	OperationID  string `json:"operationId,omitempty"`
}

type markerFact struct {
	Present       bool   `json:"present"`
	Unreadable    string `json:"unreadable,omitempty"`
	ID            string `json:"id,omitempty"`
	FromRelease   string `json:"fromRelease,omitempty"`
	ToRelease     string `json:"toRelease,omitempty"`
	StartedAt     string `json:"startedAt,omitempty"`
	Authorization string `json:"authorization,omitempty"`
}

type statusResultData struct {
	// State machine enum: healthy | out-of-date | drift | not-deployed |
	// locked | recovery-required | degraded.
	State    string      `json:"state"`
	Desired  *releaseRef `json:"desired"`
	Observed *releaseRef `json:"observed"`
	Lock     string      `json:"lock"` // free | held | unreadable
	Attempt  *markerFact `json:"attempt"`
	Recovery *markerFact `json:"recovery"`
}

func statusResult(project, envName string, desired, observed *releaseRef, lock, stateMachine string, attempt, recovery *markerFact) *resultEnvelope {
	env := &resultEnvelope{
		Schema:      resultSchemaV1,
		Command:     cmdStatus,
		Outcome:     outcomeSuccess,
		Message:     "state: " + stateMachine,
		SafeToRetry: true,
		Project:     project,
		Environment: envName,
		Data: &statusResultData{
			State:    stateMachine,
			Desired:  desired,
			Observed: observed,
			Lock:     lock,
			Attempt:  attempt,
			Recovery: recovery,
		},
	}
	env.RecoveryRequired = stateMachine == "recovery-required"
	if stateMachine == "degraded" {
		env.Message = "evidence unreadable; claim nothing"
	}
	return env
}

// ---- recovery resolve --------------------------------------------------

// Machine command-name enum: the Command field is one of these constants
// on every path, never an arbitrary display string.
const (
	cmdDeploy          = "deploy"
	cmdRollback        = "rollback"
	cmdStatus          = "status"
	cmdRecoveryResolve = "recovery-resolve"
)

type resolveResultData struct {
	NothingToResolve    bool        `json:"nothingToResolve"`
	ResolvedRecoveryID  string      `json:"resolvedRecoveryId,omitempty"`
	ResolvedAttemptID   string      `json:"resolvedAttemptId,omitempty"`
	LeftRecoveryID      string      `json:"leftRecoveryId,omitempty"`
	LeftAttemptID       string      `json:"leftAttemptId,omitempty"`
	RemainingRecoveryID string      `json:"remainingRecoveryId,omitempty"`
	RemainingAttemptID  string      `json:"remainingAttemptId,omitempty"`
	HistorySeq          int64       `json:"historySeq,omitempty"`
	Observed            *releaseRef `json:"observed,omitempty"`
}

// observedRef projects what observed state the operator resolved against.
func observedRef(rep *lifecycle.ResolveReport) *releaseRef {
	if rep.ObservedRelease == "" && rep.ObservedOperationID == "" {
		return nil
	}
	return &releaseRef{Version: rep.ObservedRelease, BundleDigest: rep.ObservedBundleDigest, OperationID: rep.ObservedOperationID}
}

func resolveResult(rep *lifecycle.ResolveReport, err error, refused bool) (*resultEnvelope, int) {
	env := &resultEnvelope{Schema: resultSchemaV1, Command: cmdRecoveryResolve}
	data := &resolveResultData{}
	env.Data = data
	if rep != nil {
		env.Project, env.Environment = rep.Project, rep.Environment
		*data = resolveResultData{
			NothingToResolve:   rep.NothingToResolve,
			ResolvedRecoveryID: rep.ResolvedRecoveryID,
			ResolvedAttemptID:  rep.ResolvedAttemptID,
			LeftRecoveryID:     rep.LeftRecoveryID,
			LeftAttemptID:      rep.LeftAttemptID,
			HistorySeq:         rep.HistorySeq,
			Observed:           observedRef(rep),
		}
		// What REMAINS is an engine-owned fact (the *PresentAfter
		// flags), not a restatement of the read-time marker ids: a
		// successful resolution must expose no remaining ids, and a
		// half-cleared target must expose only the survivor. The
		// read-time ids name WHAT the remaining marker is; Left* names
		// what was deliberately left by a partial authorization.
		if rep.AttemptPresentAfter {
			data.RemainingAttemptID = rep.AttemptMarkerID
		}
		if rep.RecoveryPresentAfter {
			data.RemainingRecoveryID = rep.RecoveryMarkerID
		}
		// The engine owns the blocked-state fact on EVERY path —
		// including history-write and marker-clear failures.
		env.RecoveryRequired = rep.RecoveryRequired
	}
	switch {
	case err != nil && refused:
		env.Outcome = outcomeRefused
		env.RecoveryRequired = rep.RecoveryRequired
		env.Message = "refused: " + err.Error()
	case err != nil:
		// Fact-driven, not coarse: RecoveryRequired is the engine's
		// post-run fact. A cleanup-window release failure after
		// successful removals must not claim markers remain, and a
		// partial clear must not claim ALL markers remain — the
		// remaining*Id fields name exactly what survives.
		env.Outcome = outcomeInfraFailed
		if rep.RecoveryRequired {
			env.Message = "infrastructure failure; the block remains: unresolved marker(s) are still on the target: " + err.Error()
		} else {
			env.Message = "marker resolution completed, but environment-lock cleanup failed: " + err.Error()
		}
	case rep.NothingToResolve:
		env.Outcome = outcomeSuccess
		env.SafeToRetry = true
		env.Message = "nothing to resolve; the environment is not blocked"
	default:
		env.Outcome = outcomeSuccess
		env.Message = "resolution recorded and authorized markers removed"
	}
	noteLockReleaseFailed(env, err)
	return env, exitByOutcome(env.Outcome)
}

// ---- argument normalization -------------------------------------------

// booleanCLI is the set of flags that take no value, across all
// operational commands.
var booleanCLI = map[string]bool{"json": true}

func isFlagToken(tok string) bool {
	return strings.HasPrefix(tok, "-") && tok != "-"
}

// lexArgs is the SINGLE authority on flag arity and machine-mode
// detection, so the two interpretations can never diverge. It
// partitions tokens into flag tokens (with their values) and positional
// tokens, preserving order, and reports whether the bare --json flag
// selected machine mode.
//
// A value flag (anything in valueFlags) consumes the following token as
// its value ONLY when that token is not flag-shaped. A flag-shaped
// token after a value flag is a MISSING VALUE — reported as
// missingValue, which every command turns into a usage error — never a
// quoted value, and in particular --json can never be consumed as a
// value while simultaneously selecting machine mode. "--flag=value" is
// self-contained. Unknown flags are passed through for the FlagSet to
// reject as undefined, consuming a following non-flag token as their
// prospective value.
func lexArgs(tokens []string, valueFlags map[string]bool) (flags, positional []string, jsonMode bool, missingValue string) {
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if !isFlagToken(tok) {
			positional = append(positional, tok)
			continue
		}
		name := strings.TrimLeft(tok, "-")
		if tok == "--json" {
			jsonMode = true
		}
		if strings.Contains(name, "=") {
			flags = append(flags, tok)
			continue
		}
		flags = append(flags, tok)
		if booleanCLI[name] {
			continue
		}
		if valueFlags[name] {
			if i+1 >= len(tokens) || isFlagToken(tokens[i+1]) {
				// Missing value: record it and keep lexing — the
				// remaining tokens still decide machine mode (a
				// trailing --json must still select it), and the
				// caller turns this into a usage error without
				// ever parsing the flags.
				if missingValue == "" {
					missingValue = name
				}
				continue
			}
			i++
			flags = append(flags, tokens[i])
			continue
		}
		// Unknown flag: let the FlagSet reject it, but do not let it
		// swallow a flag-shaped token on the way.
		if i+1 < len(tokens) && !isFlagToken(tokens[i+1]) {
			i++
			flags = append(flags, tokens[i])
		}
	}
	return flags, positional, jsonMode, missingValue
}

var errInteractiveConfirmation = errors.New("interactive confirmation required")

func envNameOr(positional []string) string {
	if len(positional) > 0 {
		return positional[0]
	}
	return ""
}
