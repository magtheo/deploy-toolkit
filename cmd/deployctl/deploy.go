package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/magtheo/deploy-toolkit/internal/lifecycle"
)

// runDeploy executes a deployment of the release the environment pins.
// It is a thin wrapper: every decision — locks, staging, contract
// verification, marker discipline, state transitions — belongs to the
// engine; this layer only loads manifests, prepares the bundle, connects,
// and renders the report.
//
// Exit codes: 0 success / already-current, 1 reported deployment failure,
// 2 usage or configuration error, 3 infrastructure failure (uncertain —
// never blindly retry; run deployctl status).
func runDeploy(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "usage: deployctl deploy <environment> [--repo-dir .] [--owner identity]")
		return exitUsage
	}
	envName := args[0]
	if strings.Contains(envName, "/") {
		fmt.Fprintf(stderr, "deployctl deploy: environment must be a bare name, got %q\n", envName)
		return exitUsage
	}
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoDir := fs.String("repo-dir", ".", "local checkout containing the release revision (for the bundle)")
	owner := fs.String("owner", "", "identity recorded in the lock and history (default user@host)")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "deployctl deploy: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}

	dc, err := loadDeploymentContext(*repoDir, envName)
	if err != nil {
		fmt.Fprintf(stderr, "✗ deploy %s: %v\n", envName, err)
		return exitUsage
	}
	bundleBytes, err := prepareBundle(ctx, dc.RepoDir, dc.Release)
	if err != nil {
		fmt.Fprintf(stderr, "✗ deploy %s: %v\n", envName, err)
		return exitUsage
	}
	tgt, err := connect(ctx, dc.Target)
	if err != nil {
		fmt.Fprintf(stderr, "✗ deploy %s: %v\n", envName, err)
		return exitInfra
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
	return reportDeploy(rep, err, stdout, stderr)
}

func reportDeploy(rep *lifecycle.Report, err error, stdout, stderr io.Writer) int {
	if err != nil {
		fmt.Fprintf(stderr, "✗ deploy %s: infrastructure failure: %v\n", rep.Project+"/"+rep.Environment, err)
		fmt.Fprintln(stderr, "  The outcome is UNCERTAIN — the deployment may have executed consequential work.")
		fmt.Fprintln(stderr, "  Do not simply retry: run `deployctl status` and follow the recovery guidance.")
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
