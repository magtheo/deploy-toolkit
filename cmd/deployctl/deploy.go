package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/magtheo/deploy-toolkit/internal/lifecycle"
	"github.com/magtheo/deploy-toolkit/internal/target"
)

// runDeploy executes a deployment of the release the environment pins.
// It is a thin wrapper: every decision — locks, staging, contract
// verification, marker discipline, state transitions — belongs to the
// engine; this layer only loads manifests, prepares the bundle, connects,
// and renders the report.
//
// Exit codes: 0 success / already-current, 1 reported deployment failure,
// 2 usage or configuration error, 3 infrastructure failure. The failure
// REPORT — not the exit code — says whether the outcome is uncertain
// (unresolved attempt/recovery marker) or nothing was executed.
func runDeploy(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	lexFlags, positional, jsonMode, missingValue := lexArgs(args, map[string]bool{"repo-dir": true, "owner": true})
	if missingValue != "" {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult(cmdDeploy, envNameOr(positional), "--"+missingValue+" requires a value"))
		}
		fmt.Fprintf(stderr, "deployctl deploy: --%s requires a value\n", missingValue)
		return exitUsage
	}
	if len(positional) == 0 {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult(cmdDeploy, "", "usage: deployctl deploy <environment> [--repo-dir .] [--owner identity]"))
		}
		fmt.Fprintln(stderr, "usage: deployctl deploy <environment> [--repo-dir .] [--owner identity]")
		return exitUsage
	}
	if len(positional) > 1 {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult(cmdDeploy, positional[0], fmt.Sprintf("unexpected argument %q", positional[1])))
		}
		fmt.Fprintf(stderr, "deployctl deploy: unexpected argument %q\n", positional[1])
		return exitUsage
	}
	envName := positional[0]
	if strings.Contains(envName, "/") {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult("deploy", envName, "environment must be a bare name"))
		}
		fmt.Fprintf(stderr, "deployctl deploy: environment must be a bare name, got %q\n", envName)
		return exitUsage
	}
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoDir := fs.String("repo-dir", ".", "local checkout containing the release revision (for the bundle)")
	owner := fs.String("owner", "", "identity recorded in the lock and history (default user@host)")
	_ = fs.Bool("json", false, "emit a single deployctl.result/v1 JSON document on stdout")
	if err := fs.Parse(lexFlags); err != nil {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult(cmdDeploy, envName, "invalid flags: "+err.Error()))
		}
		fmt.Fprintf(stderr, "deployctl deploy: %v\n", err)
		return exitUsage
	}
	humanOut := io.Writer(stdout)
	if jsonMode {
		humanOut = io.Discard
	}

	dc, err := loadDeploymentContext(*repoDir, envName)
	if err != nil {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult("deploy", envName, err.Error()))
		}
		fmt.Fprintf(stderr, "✗ deploy %s: %v\n", envName, err)
		return exitUsage
	}
	bundleBytes, err := prepareBundle(ctx, dc.RepoDir, dc.Release)
	if err != nil {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult("deploy", envName, err.Error()))
		}
		fmt.Fprintf(stderr, "✗ deploy %s: %v\n", envName, err)
		return exitUsage
	}
	tgt, err := connect(ctx, dc.Target)
	if err != nil {
		return reportConnectFailure(err, "deploy", envName, stderr, jsonMode, stdout)
	}
	ownerID := *owner
	if ownerID == "" {
		ownerID = defaultOwner()
	}

	rep, err := lifecycle.Deploy(ctx, lifecycle.DeployInput{
		Target:         tgt,
		TargetManifest: dc.Target,
		Environment:    dc.Env,
		Release:        dc.Release,
		Bundle:         bundleBytes,
		Owner:          ownerID,
	})
	if jsonMode {
		env, _ := deployResult(rep, err)
		return emitJSON(stdout, env)
	}
	return reportDeploy(rep, err, humanOut, stderr)
}

func reportDeploy(rep *lifecycle.Report, err error, stdout, stderr io.Writer) int {
	if err != nil {
		env := "?"
		if rep != nil {
			env = rep.Project + "/" + rep.Environment
		}
		if errors.Is(err, lifecycle.ErrEnvLockHeld) {
			fmt.Fprintf(stderr, "✗ deploy %s: refused: the environment lock is held — another\noperation may currently be executing.\n", env)
			fmt.Fprintln(stderr, "  Do not rerun mechanically. Run `deployctl status`; only remove a stale")
			fmt.Fprintln(stderr, "  lock after verifying no deployment is in flight.")
			return exitFailed
		}
		if errors.Is(err, target.ErrEvidenceInvalid) {
			fmt.Fprintf(stderr, "✗ deploy %s: refused: durable evidence on the target exists but is invalid.\n", env)
			fmt.Fprintln(stderr, "  Rerunning cannot help while the evidence is invalid. Run `deployctl status`, inspect")
			fmt.Fprintln(stderr, "  the affected evidence and verify the target's actual state before following the")
			fmt.Fprintln(stderr, "  recovery procedure. Do NOT edit observed state merely to make this proceed.")
			return exitFailed
		}
		fmt.Fprintf(stderr, "✗ deploy %s: infrastructure failure: %v\n", env, err)
		switch {
		case rep != nil && rep.Committed:
			fmt.Fprintln(stderr, "  The observed state is committed; the deployment itself succeeded.")
			fmt.Fprintln(stderr, "  Post-commit bookkeeping failed. Run `deployctl status` before")
			fmt.Fprintln(stderr, "  taking another action.")
		case rep != nil && rep.ConsequentialStarted:
			id := rep.AttemptID
			if id == "" {
				id = "(id unknown)"
			}
			fmt.Fprintf(stderr, "  The outcome is UNCERTAIN: consequential deployment work may have\n  executed (attempt %s is unresolved).\n", id)
			fmt.Fprintln(stderr, "  Do not retry. Run `deployctl status` and follow the recovery guidance.")
		default:
			fmt.Fprintln(stderr, "  No consequential work was executed — nothing was applied to the target.")
			fmt.Fprintln(stderr, "  After the infrastructure problem is fixed, the deployment can simply")
			fmt.Fprintln(stderr, "  be rerun.")
		}
		return exitInfra
	}
	fmt.Fprintf(stdout, "Deploy %s: release %s\n", rep.Project+"/"+rep.Environment, rep.Version)
	for i := range rep.Stages {
		renderStage(stdout, rep.Stages[i])
		renderStageDetails(stdout, rep.Stages[i])
	}
	if rep.AlreadyCurrent {
		fmt.Fprintf(stdout, "✓ already current — %s@%s is verified running; no consequential work was repeated\n", rep.Version, rep.BundleDigest)
		return exitOK
	}
	if rep.Committed {
		fmt.Fprintf(stdout, "✓ production is running %s@%s (verified, state committed)\n", rep.Version, rep.BundleDigest)
		return exitOK
	}
	fmt.Fprintf(stdout, "✗ deploy failed: %s\n", rep.FailureReason)
	if rep.RecoveryRequired {
		fmt.Fprintln(stdout, "\nRECOVERY REQUIRED")
		fmt.Fprintln(stdout, "  A deployment attempt is unresolved: consequential work may have")
		fmt.Fprintln(stdout, "  executed and the outcome is unknown. Normal deployments are BLOCKED")
		fmt.Fprintln(stdout, "  until the situation is resolved.")
		fmt.Fprintln(stdout, "  Inspect: deployctl status "+rep.Environment)
	}
	return exitFailed
}
