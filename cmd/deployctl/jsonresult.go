package main

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/magtheo/deploy-toolkit/internal/lifecycle"
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
	Committed            bool           `json:"committed"`
	AlreadyCurrent       bool           `json:"alreadyCurrent"`
	ConsequentialStarted bool           `json:"consequentialStarted"`
	AttemptID            string         `json:"attemptId,omitempty"`
	Version              string         `json:"version,omitempty"`
	BundleDigest         string         `json:"bundleDigest,omitempty"`
	FailureReason        string         `json:"failureReason,omitempty"`
	Stages               []stageOutcome `json:"stages,omitempty"`
}

func deployResult(rep *lifecycle.Report, err error) (*resultEnvelope, int) {
	env := &resultEnvelope{Schema: resultSchemaV1, Command: "deploy"}
	data := &deployResultData{}
	env.Data = data
	failureStages := false
	if rep != nil {
		env.Project, env.Environment = rep.Project, rep.Environment
		*data = deployResultData{
			Committed:            rep.Committed,
			AlreadyCurrent:       rep.AlreadyCurrent,
			ConsequentialStarted: rep.ConsequentialStarted,
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
		case rep != nil && rep.Committed:
			env.Outcome = outcomeInfraFailed
			env.Message = "the observed state is committed; post-commit bookkeeping failed"
		case rep != nil && rep.ConsequentialStarted:
			env.Outcome = outcomeUncertain
			env.RecoveryRequired = true
			env.SafeToRetry = false
			env.Message = "outcome uncertain: consequential deployment work may have executed (attempt " + orNone(rep.AttemptID) + " unresolved)"
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
	return env, exitByOutcome(env.Outcome)
}

// ---- rollback ----------------------------------------------------------

type rollbackResultData struct {
	Committed            bool           `json:"committed"`
	AlreadyRecovered     bool           `json:"alreadyRecovered"`
	ConsequentialStarted bool           `json:"recoveryStarted"`
	RecoveryID           string         `json:"recoveryId,omitempty"`
	FromVersion          string         `json:"fromVersion,omitempty"`
	ToVersion            string         `json:"toVersion,omitempty"`
	FailureReason        string         `json:"failureReason,omitempty"`
	Stages               []stageOutcome `json:"stages,omitempty"`
}

func rollbackResult(rep *lifecycle.RollbackReport, err error) (*resultEnvelope, int) {
	env := &resultEnvelope{Schema: resultSchemaV1, Command: "rollback"}
	data := &rollbackResultData{}
	env.Data = data
	failureStages := false
	if rep != nil {
		env.Project, env.Environment = rep.Project, rep.Environment
		*data = rollbackResultData{
			Committed:            rep.Committed,
			AlreadyRecovered:     rep.AlreadyRecovered,
			ConsequentialStarted: rep.RecoveryStarted,
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
		case rep != nil && rep.Committed:
			env.Outcome = outcomeInfraFailed
			env.Message = "the observed state is committed; post-commit bookkeeping failed"
		case rep != nil && rep.AlreadyRecovered:
			env.Outcome = outcomeInfraFailed
			env.Message = "the recovery is already committed; post-commit bookkeeping failed"
		case rep != nil && rep.RecoveryStarted:
			env.Outcome = outcomeUncertain
			env.RecoveryRequired = true
			env.Message = "outcome uncertain: consequential recovery work may have executed (recovery " + orNone(rep.RecoveryID) + " unresolved)"
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
		Command:     "status",
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

type resolveResultData struct {
	NothingToResolve   bool        `json:"nothingToResolve"`
	ResolvedRecoveryID string      `json:"resolvedRecoveryId,omitempty"`
	ResolvedAttemptID  string      `json:"resolvedAttemptId,omitempty"`
	LeftRecoveryID     string      `json:"leftRecoveryId,omitempty"`
	LeftAttemptID      string      `json:"leftAttemptId,omitempty"`
	HistorySeq         int64       `json:"historySeq,omitempty"`
	Observed           *releaseRef `json:"observed,omitempty"`
}

func resolveResult(rep *lifecycle.ResolveReport, err error, refused bool) (*resultEnvelope, int) {
	env := &resultEnvelope{Schema: resultSchemaV1, Command: "recovery-resolve"}
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
		}
		if rep.LeftRecoveryID != "" || rep.LeftAttemptID != "" {
			env.RecoveryRequired = true
		}
	}
	switch {
	case err != nil && refused:
		env.Outcome = outcomeRefused
		env.Message = "refused: " + err.Error()
	case err != nil:
		env.Outcome = outcomeInfraFailed
		env.Message = "infrastructure failure; the markers were NOT removed: " + err.Error()
	case rep.NothingToResolve:
		env.Outcome = outcomeSuccess
		env.SafeToRetry = true
		env.Message = "nothing to resolve; the environment is not blocked"
	default:
		env.Outcome = outcomeSuccess
		env.Message = "resolution recorded and authorized markers removed"
	}
	return env, exitByOutcome(env.Outcome)
}

// ---- argument normalization -------------------------------------------

// splitFlagsAndValues partitions tokens into flag tokens (with their
// values) and positional tokens, preserving order. Value flags consume
// the following token; boolean flags consume nothing; "--flag=value" is
// self-contained. Unknown flags are assumed to take a value ONLY when
// they don't — the FlagSet rejects them either way, as a usage error.
func splitFlagsAndValues(tokens []string, booleanFlags map[string]bool) (flags, positional []string) {
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if !strings.HasPrefix(tok, "-") || tok == "-" {
			positional = append(positional, tok)
			continue
		}
		name := strings.TrimLeft(tok, "-")
		if strings.Contains(name, "=") {
			flags = append(flags, tok)
			continue
		}
		flags = append(flags, tok)
		if !booleanFlags[name] && i+1 < len(tokens) {
			i++
			flags = append(flags, tokens[i])
		}
	}
	return flags, positional
}

var errInteractiveConfirmation = errors.New("interactive confirmation required")

// wantsJSON reports whether --json was requested, even before flag
// parsing: the earliest usage errors must still honor the machine mode.
func wantsJSON(args []string) bool {
	for _, a := range args {
		if a == "--json" || strings.HasPrefix(a, "--json=") {
			return true
		}
	}
	return false
}
