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
func runStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "usage: deployctl status <environment> [--repo-dir .]")
		return exitUsage
	}
	envName := args[0]
	if strings.Contains(envName, "/") {
		fmt.Fprintf(stderr, "deployctl status: environment must be a bare name, got %q\n", envName)
		return exitUsage
	}
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoDir := fs.String("repo-dir", ".", "checkout containing .deploy/")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "deployctl status: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}

	dc, err := loadDeploymentContext(*repoDir, envName)
	if err != nil {
		fmt.Fprintf(stderr, "✗ status %s: %v\n", envName, err)
		return exitUsage
	}
	tgt, err := connect(ctx, dc.Target)
	if err != nil {
		fmt.Fprintf(stderr, "✗ status %s: %v\n", envName, err)
		return exitInfra
	}

	fmt.Fprintf(stdout, "Project       %s\n", dc.Release.Metadata.Project)
	fmt.Fprintf(stdout, "Environment   %s\n", dc.Env.Metadata.Name)
	fmt.Fprintf(stdout, "Target        %s\n", dc.Target.Metadata.Name)
	fmt.Fprintf(stdout, "Desired\n  Release     %s\n", dc.Release.Metadata.Version)
	fmt.Fprintf(stdout, "  Digest      %s\n", shortDigest(dc.Release.Bundle.Digest))

	st, err := tgt.ReadState(ctx, dc.Release.Metadata.Project, dc.Env.Metadata.Name)
	switch {
	case err == nil:
		fmt.Fprintf(stdout, "Observed\n  Release     %s\n", st.Current.Release)
		fmt.Fprintf(stdout, "  Digest      %s\n", shortDigest(st.Current.BundleDigest))
		fmt.Fprintf(stdout, "  Verified    %s\n", st.Current.Since)
		fmt.Fprintf(stdout, "  Operation   %s\n", operationDisplay(st.Current.OperationID))
	case errors.Is(err, target.ErrStateAbsent):
		fmt.Fprintf(stdout, "Observed\n  none — this environment has never been deployed to\n")
	default:
		fmt.Fprintf(stdout, "Observed\n  unreadable: %v\n", err)
	}

	lockHeld, err := lockHeld(ctx, tgt, dc)
	if err != nil {
		fmt.Fprintf(stdout, "Lock          unreadable: %v\n", err)
	} else if lockHeld {
		fmt.Fprintf(stdout, "Lock          HELD — a deployment or recovery may be executing right now\n")
	} else {
		fmt.Fprintf(stdout, "Lock          none\n")
	}

	attempt, aerr := tgt.ReadAttempt(ctx, dc.Release.Metadata.Project, dc.Env.Metadata.Name)
	recovery, rerr := tgt.ReadRecovery(ctx, dc.Release.Metadata.Project, dc.Env.Metadata.Name)

	printAttempt := func() {
		fmt.Fprintf(stdout, "Deployment attempt  UNRESOLVED\n")
		fmt.Fprintf(stdout, "  Transition  %s → %s\n", orNone(attempt.FromRelease), attempt.ToRelease)
		fmt.Fprintf(stdout, "  Digest      %s\n", shortDigest(attempt.BundleDigest))
		fmt.Fprintf(stdout, "  Started     %s (id %s)\n", attempt.StartedAt, attempt.AttemptID)
		fmt.Fprintf(stdout, "  Meaning     consequential deployment work may have executed; the outcome is unknown\n")
	}
	printRecovery := func() {
		fmt.Fprintf(stdout, "Recovery      UNRESOLVED\n")
		fmt.Fprintf(stdout, "  Transition  %s → %s\n", recovery.FromRelease, recovery.ToRelease)
		fmt.Fprintf(stdout, "  Started     %s (id %s, authorization %s)\n", recovery.StartedAt, recovery.RecoveryID, recovery.Authorization)
		fmt.Fprintf(stdout, "  Meaning     consequential recovery work may have executed; the outcome is unknown\n")
	}

	fmt.Fprintf(stdout, "\nState         %s\n", classifyState(dc, st, err, lockHeld, aerr == nil, rerr == nil))
	if aerr == nil {
		printAttempt()
	}
	if rerr == nil {
		printRecovery()
	}
	printGuidance(stdout, lockHeld, aerr == nil, rerr == nil, st, err, dc)
	return exitOK
}

func classifyState(dc *deploymentContext, st target.State, stateErr error, lock, attempt, recovery bool) string {
	switch {
	case recovery:
		return "RECOVERY REQUIRED — unresolved recovery marker blocks everything"
	case attempt:
		return "RECOVERY REQUIRED — unresolved deployment attempt blocks normal deployment"
	case lock:
		return "LOCKED — a deployment or recovery may be executing right now"
	case stateErr != nil:
		if errors.Is(stateErr, target.ErrStateAbsent) {
			return "NOT DEPLOYED"
		}
		return "DEGRADED EVIDENCE — observed state failed validation; investigate before deploying"
	case st.Current.Release != dc.Release.Metadata.Version:
		return "OUT OF DATE — desired release differs from observed"
	case st.Current.BundleDigest != dc.Release.Bundle.Digest:
		return "DRIFT — observed release carries a different bundle digest than the release pins"
	default:
		return "HEALTHY — desired and observed agree"
	}
}

func printGuidance(stdout io.Writer, lock, attempt, recovery bool, st target.State, stateErr error, dc *deploymentContext) {
	switch {
	case recovery:
		fmt.Fprintln(stdout, "\nNormal deployment AND recovery are blocked. An earlier recovery has an")
		fmt.Fprintln(stdout, "unknown outcome: repeating it is not known-safe.")
		fmt.Fprintln(stdout, "  1. Verify the target's actual state (app health, running digest, migrations).")
		fmt.Fprintln(stdout, "  2. If the recovery demonstrably committed, rerun it — the proven-committed")
		fmt.Fprintln(stdout, "     path cleans up the markers without executing hooks.")
		fmt.Fprintln(stdout, "  3. Otherwise resolve the markers explicitly after verifying, then start a")
		fmt.Fprintln(stdout, "     fresh recovery.")
	case attempt:
		fmt.Fprintln(stdout, "\nNormal deployment is blocked. Do NOT retry the deployment and do NOT edit")
		fmt.Fprintln(stdout, "observed state by hand.")
		fmt.Fprintln(stdout, "  If production is healthy on the restored release, an explicit recovery")
		fmt.Fprintln(stdout, "  resolves the attempt: deployctl rollback "+dc.Env.Metadata.Name+" --to "+orNone(attemptTargetRelease(dc, st, stateErr)))
	case lock:
		fmt.Fprintln(stdout, "\nWait for the active operation to finish. A crashed runner leaves the lock;")
		fmt.Fprintln(stdout, "remove it manually ONLY after verifying no deployment is in flight.")
	}
}

// attemptTargetRelease names the release a blocked environment would
// restore to: the attempt's origin, when readable from state.
func attemptTargetRelease(dc *deploymentContext, st target.State, stateErr error) string {
	if stateErr == nil && st.Current != nil {
		return st.Current.Release
	}
	return dc.Release.Metadata.Version
}

func lockHeld(ctx context.Context, tgt *target.Target, dc *deploymentContext) (bool, error) {
	path, err := tgt.Layout().LockPath(dc.Release.Metadata.Project, dc.Env.Metadata.Name)
	if err != nil {
		return false, err
	}
	return tgt.Exists(ctx, path)
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
