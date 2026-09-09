package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/magtheo/deploy-toolkit/internal/target"
)

// runStatus is the read-only human window onto the target: desired vs
// observed, and every durable recovery fact — lock, deployment attempt,
// recovery marker — translated from internal machinery into plain
// operational language. It reads with the deploy credential; it decides
// nothing.
//
// Status is the operator's decision surface, so it fails closed: an
// unreadable or invalid recovery fact (lock, attempt marker, recovery
// marker, observed state) is DEGRADED EVIDENCE — HEALTHY is never claimed
// while any evidence is unreadable. A held lock dominates attempt and
// recovery markers: during an active deployment or rollback both exist by
// design, and the only safe instruction then is "wait, touch nothing".
func runStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	jsonMode := wantsJSON(args)
	rawStdout := stdout
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult("status", "", "usage: deployctl status <environment> [--repo-dir .]"))
		}
		fmt.Fprintln(stderr, "usage: deployctl status <environment> [--repo-dir .]")
		return exitUsage
	}
	envName := args[0]
	if strings.Contains(envName, "/") {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult("status", envName, "environment must be a bare name"))
		}
		fmt.Fprintf(stderr, "deployctl status: environment must be a bare name, got %q\n", envName)
		return exitUsage
	}
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoDir := fs.String("repo-dir", ".", "checkout containing .deploy/")
	_ = fs.Bool("json", false, "emit a single deployctl.result/v1 JSON document on stdout")
	if err := fs.Parse(args[1:]); err != nil {
		if jsonMode {
			return emitJSON(rawStdout, usageErrorResult(cmdStatus, envName, "invalid flags"))
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		if jsonMode {
			return emitJSON(rawStdout, usageErrorResult(cmdStatus, envName, fmt.Sprintf("unexpected argument %q", fs.Arg(0))))
		}
		fmt.Fprintf(stderr, "deployctl status: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}

	dc, err := loadDeploymentContext(*repoDir, envName)
	if err != nil {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult("status", envName, err.Error()))
		}
		fmt.Fprintf(stderr, "✗ status %s: %v\n", envName, err)
		return exitUsage
	}
	tgt, err := connect(ctx, dc.Target)
	if err != nil {
		return reportConnectFailure(err, "status", envName, stderr, jsonMode, stdout)
	}
	humanOut := io.Writer(stdout)
	if jsonMode {
		humanOut = io.Discard
	}
	stdout = humanOut

	fmt.Fprintf(stdout, "Project       %s\n", dc.Release.Metadata.Project)
	fmt.Fprintf(stdout, "Environment   %s\n", dc.Env.Metadata.Name)
	fmt.Fprintf(stdout, "Target        %s\n", dc.Target.Metadata.Name)
	fmt.Fprintf(stdout, "Desired\n  Release     %s\n", dc.Release.Metadata.Version)
	fmt.Fprintf(stdout, "  Digest      %s\n", shortDigest(dc.Release.Bundle.Digest))

	project, env := dc.Release.Metadata.Project, dc.Env.Metadata.Name

	// Gather every recovery fact FIRST, letting each one speak for
	// itself: nil / absent / present / unreadable are different facts.
	st, stateErr := tgt.ReadState(ctx, project, env)
	lock, lockErr := lockHeld(ctx, tgt, dc)
	attempt, attemptErr := tgt.ReadAttempt(ctx, project, env)
	recovery, recoveryErr := tgt.ReadRecovery(ctx, project, env)

	attemptPresent := attemptErr == nil
	attemptUnreadable := attemptErr != nil && !errors.Is(attemptErr, target.ErrAttemptAbsent)
	recoveryPresent := recoveryErr == nil
	recoveryUnreadable := recoveryErr != nil && !errors.Is(recoveryErr, target.ErrRecoveryAbsent)
	stateAbsent := errors.Is(stateErr, target.ErrStateAbsent) || (stateErr == nil && st.Current == nil)
	stateUnreadable := stateErr != nil && !stateAbsent

	fmt.Fprintf(stdout, "Observed\n")
	switch {
	case stateErr == nil && st.Current != nil:
		fmt.Fprintf(stdout, "  Release     %s\n", st.Current.Release)
		fmt.Fprintf(stdout, "  Digest      %s\n", shortDigest(st.Current.BundleDigest))
		fmt.Fprintf(stdout, "  Verified    %s\n", st.Current.Since)
		fmt.Fprintf(stdout, "  Operation   %s\n", operationDisplay(st.Current.OperationID))
	case stateErr == nil:
		fmt.Fprintf(stdout, "  none recorded — the state file exists but records no current deployment\n")
	case stateAbsent:
		fmt.Fprintf(stdout, "  none — this environment has never been deployed to\n")
	default:
		fmt.Fprintf(stdout, "  unreadable: %v\n", stateErr)
	}

	switch {
	case lockErr != nil:
		fmt.Fprintf(stdout, "Lock          unreadable: %v\n", lockErr)
	case lock:
		fmt.Fprintf(stdout, "Lock          HELD — a deployment or recovery may be executing right now\n")
	default:
		fmt.Fprintf(stdout, "Lock          none\n")
	}

	switch {
	case attemptPresent:
		fmt.Fprintf(stdout, "Deployment attempt  UNRESOLVED\n")
		fmt.Fprintf(stdout, "  Transition  %s → %s\n", orNone(attempt.FromRelease), attempt.ToRelease)
		fmt.Fprintf(stdout, "  Digest      %s\n", shortDigest(attempt.BundleDigest))
		fmt.Fprintf(stdout, "  Started     %s (id %s)\n", attempt.StartedAt, attempt.AttemptID)
		fmt.Fprintf(stdout, "  Meaning     consequential deployment work may have executed; the outcome is unknown\n")
	case attemptUnreadable:
		fmt.Fprintf(stdout, "Deployment attempt  UNREADABLE: %v\n", attemptErr)
	default:
		fmt.Fprintf(stdout, "Deployment attempt  none\n")
	}

	switch {
	case recoveryPresent:
		fmt.Fprintf(stdout, "Recovery      UNRESOLVED\n")
		fmt.Fprintf(stdout, "  Transition  %s → %s\n", recovery.FromRelease, recovery.ToRelease)
		fmt.Fprintf(stdout, "  Started     %s (id %s, authorization %s)\n", recovery.StartedAt, recovery.RecoveryID, recovery.Authorization)
		fmt.Fprintf(stdout, "  Meaning     consequential recovery work may have executed; the outcome is unknown\n")
	case recoveryUnreadable:
		fmt.Fprintf(stdout, "Recovery      UNREADABLE: %v\n", recoveryErr)
	default:
		fmt.Fprintf(stdout, "Recovery      none\n")
	}

	degraded := attemptUnreadable || recoveryUnreadable || lockErr != nil || stateUnreadable
	if jsonMode {
		return emitJSON(rawStdout, buildStatusJSON(dc, st, stateErr, stateAbsent, lock, lockErr, attempt, attemptErr, attemptUnreadable, recovery, recoveryErr, recoveryUnreadable))
	}
	fmt.Fprintf(stdout, "\nState         %s\n", classifyState(stateClassification{
		degraded:      degraded,
		lock:          lock,
		recovery:      recoveryPresent,
		attempt:       attemptPresent,
		desired:       dc.Release.Metadata.Version,
		desiredDigest: dc.Release.Bundle.Digest,
		observed:      st.Current,
		stateAbsent:   stateAbsent,
	}))
	printGuidance(stdout, guidanceFacts{
		degraded: degraded,
		lock:     lock,
		recovery: recoveryPresent,
		attempt:  attemptPresent,
		from:     attempt.FromRelease,
		env:      env,
	})
	return exitOK
}

type stateClassification struct {
	degraded      bool
	lock          bool
	recovery      bool
	attempt       bool
	desired       string
	desiredDigest string
	observed      *target.CurrentDeployment
	stateAbsent   bool
}

// classifyState precedence: unreadable evidence — nothing else may be
// claimed while a recovery fact cannot be read; lock — an operation in
// flight outranks every marker it may have created; recovery marker;
// attempt marker; desired/observed comparison.
func classifyState(c stateClassification) string {
	human, _ := c.classify()
	return human
}

// classify returns the human line and the machine enum from ONE
// precedence switch, so the two views cannot diverge.
func (c stateClassification) classify() (human, machine string) {
	switch {
	case c.degraded:
		return "DEGRADED EVIDENCE — a recovery fact is unreadable or invalid; claim nothing, repair access first", "degraded"
	case c.lock:
		return "LOCKED — an operation may be executing; wait, do not start recovery or deployment", "locked"
	case c.recovery:
		return "RECOVERY REQUIRED — unresolved recovery marker blocks everything", "recovery-required"
	case c.attempt:
		return "RECOVERY REQUIRED — unresolved deployment attempt blocks normal deployment", "recovery-required"
	case c.stateAbsent:
		return "NOT DEPLOYED", "not-deployed"
	case c.observed.Release != c.desired:
		return "OUT OF DATE — desired release differs from observed", "out-of-date"
	case c.observed.BundleDigest != c.desiredDigest:
		return "DRIFT — observed release carries a different bundle digest than the release pins", "drift"
	default:
		return "HEALTHY — desired and observed agree", "healthy"
	}
}

type guidanceFacts struct {
	degraded bool
	lock     bool
	recovery bool
	attempt  bool
	from     string
	env      string
}

// printGuidance owns exactly the same precedence as classifyState —
// degraded, lock, recovery, attempt — so classification and instruction
// cannot drift apart. Attempt rollback guidance appears ONLY when no
// higher-priority fact says otherwise: suggesting a rollback while an
// operation is in flight, while a recovery is unresolved, or while
// evidence is unreadable would tell the operator to do exactly the
// wrong thing.
func printGuidance(stdout io.Writer, f guidanceFacts) {
	switch {
	case f.degraded:
		fmt.Fprintln(stdout, "\nEVIDENCE UNREADABLE — deploy nothing and resolve nothing by hand.")
		fmt.Fprintln(stdout, "  At least one recovery fact (lock, attempt marker, recovery marker,")
		fmt.Fprintln(stdout, "  observed state) could not be read or failed validation. HEALTHY")
		fmt.Fprintln(stdout, "  cannot be claimed while evidence is unreadable. Repair access to")
		fmt.Fprintln(stdout, "  the target and rerun `deployctl status`.")
	case f.lock:
		fmt.Fprintln(stdout, "\nWait for the active operation to finish. Do not attempt recovery and")
		fmt.Fprintln(stdout, "do not remove anything: a marker that appears while the lock is held")
		fmt.Fprintln(stdout, "may be the live operation's own. A crashed runner leaves the lock;")
		fmt.Fprintln(stdout, "remove it manually ONLY after verifying no deployment is in flight.")
	case f.recovery:
		fmt.Fprintln(stdout, "\nNormal deployment AND recovery are blocked. An earlier recovery has an")
		fmt.Fprintln(stdout, "unknown outcome: repeating it is not known-safe.")
		fmt.Fprintln(stdout, "  1. Verify the target's actual state (app health, running digest, migrations).")
		fmt.Fprintln(stdout, "  2. If the recovery demonstrably committed, rerun it — the proven-committed")
		fmt.Fprintln(stdout, "     path cleans up the markers without executing hooks.")
		fmt.Fprintln(stdout, "  3. Otherwise resolve the markers explicitly after verifying, then start a")
		fmt.Fprintln(stdout, "     fresh recovery.")
	case f.attempt:
		printAttemptGuidance(f.from, f.env, stdout)
	}
}

// printAttemptGuidance derives the restore target from the attempt
// marker's own origin. An empty origin means the failed operation was the
// FIRST deployment: there is no previously verified release to restore,
// and suggesting a rollback to the release that failed would be exactly
// wrong.
func printAttemptGuidance(fromRelease, env string, stdout io.Writer) {
	fmt.Fprintln(stdout, "\nNormal deployment is blocked. Do NOT retry the deployment and do NOT edit")
	fmt.Fprintln(stdout, "observed state by hand.")
	if fromRelease == "" {
		fmt.Fprintln(stdout, "  This was the FIRST deployment for this environment: no previously")
		fmt.Fprintln(stdout, "  verified release exists, so rollback cannot infer a restore target.")
		fmt.Fprintln(stdout, "  Verify the target by hand (app health, running digest, migrations),")
		fmt.Fprintln(stdout, "  then resolve the situation explicitly per the recovery runbook.")
		return
	}
	fmt.Fprintf(stdout, "  If production is healthy on %s, an explicit recovery restores it:\n", fromRelease)
	fmt.Fprintf(stdout, "    deployctl rollback %s --to %s --confirm \"rollback %s to %s\"\n", env, fromRelease, env, fromRelease)
}

func lockHeld(ctx context.Context, tgt *target.Target, dc *deploymentContext) (bool, error) {
	path, err := tgt.Layout().LockPath(dc.Release.Metadata.Project, dc.Env.Metadata.Name)
	if err != nil {
		return false, err
	}
	return tgt.Exists(ctx, path)
}

// buildStatusJSON assembles the machine document from the same facts the
// human rendering consumes — one read, two views, no divergence.
func buildStatusJSON(dc *deploymentContext, st target.State, stateErr error, stateAbsent bool, lock bool, lockErr error, attempt target.AttemptMarker, attemptErr error, attemptUnreadable bool, recovery target.RecoveryMarker, recoveryErr error, recoveryUnreadable bool) *resultEnvelope {
	desired := &releaseRef{Version: dc.Release.Metadata.Version, BundleDigest: dc.Release.Bundle.Digest}
	var observed *releaseRef
	if stateErr == nil && st.Current != nil {
		observed = &releaseRef{Version: st.Current.Release, BundleDigest: st.Current.BundleDigest, Since: st.Current.Since, OperationID: st.Current.OperationID}
	}
	lockStr := "free"
	switch {
	case lockErr != nil:
		lockStr = "unreadable"
	case lock:
		lockStr = "held"
	}
	attemptFact := &markerFact{}
	switch {
	case attemptErr == nil:
		attemptFact.Present = true
		attemptFact.ID = attempt.AttemptID
		attemptFact.FromRelease = attempt.FromRelease
		attemptFact.ToRelease = attempt.ToRelease
		attemptFact.StartedAt = attempt.StartedAt
	case attemptUnreadable:
		attemptFact.Unreadable = attemptErr.Error()
	}
	recoveryFact := &markerFact{}
	switch {
	case recoveryErr == nil:
		recoveryFact.Present = true
		recoveryFact.ID = recovery.RecoveryID
		recoveryFact.FromRelease = recovery.FromRelease
		recoveryFact.ToRelease = recovery.ToRelease
		recoveryFact.StartedAt = recovery.StartedAt
		recoveryFact.Authorization = recovery.Authorization
	case recoveryUnreadable:
		recoveryFact.Unreadable = recoveryErr.Error()
	}
	c := stateClassification{
		degraded:      attemptUnreadable || recoveryUnreadable || lockErr != nil || (stateErr != nil && !stateAbsent),
		lock:          lock,
		recovery:      recoveryErr == nil,
		attempt:       attemptErr == nil,
		desired:       dc.Release.Metadata.Version,
		desiredDigest: dc.Release.Bundle.Digest,
		observed:      st.Current,
		stateAbsent:   stateAbsent,
	}
	_, machine := c.classify()
	return statusResult(dc.Release.Metadata.Project, dc.Env.Metadata.Name, desired, observed, lockStr, machine, attemptFact, recoveryFact)
}

func orUnknown(s string) string {
	if s == "" {
		return "(id unknown)"
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func operationDisplay(op string) string {
	if op == "" {
		return "(pre-operationId state)"
	}
	return op
}
