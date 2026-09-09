package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/magtheo/deploy-toolkit/internal/lifecycle"
	"github.com/magtheo/deploy-toolkit/internal/target"
)

// runRollback is the explicit manual/emergency recovery path: restore a
// known previous release immediately, with typed confirmation. It is a
// thin wrapper over the engine's Rollback operation; the confirmation
// gate lives here because the authority is the human typing the
// sentence, and the CLI is where the human is.
//
// Authorization is always "manual": normal rollback of a healthy
// deployment is an ordinary promotion (reverse diff + merge), and
// automatic rollback after a failed deploy is Step 11 workflow wiring
// that invokes the same engine with its own authorization.
func runRollback(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "usage: deployctl rollback <environment> --to <version> [--from <version>] [--repo-dir .] [--confirm \"...\"]")
		return exitUsage
	}
	envName := args[0]
	if strings.Contains(envName, "/") {
		fmt.Fprintf(stderr, "deployctl rollback: environment must be a bare name, got %q\n", envName)
		return exitUsage
	}
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	to := fs.String("to", "", "version to restore (required)")
	from := fs.String("from", "", "version being undone (default: the release the environment pins)")
	repoDir := fs.String("repo-dir", ".", "local checkout containing both release revisions")
	confirm := fs.String("confirm", "", "confirmation sentence; omit to be prompted interactively")
	owner := fs.String("owner", "", "identity recorded in the lock and history (default user@host)")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "deployctl rollback: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if *to == "" {
		fmt.Fprintln(stderr, "deployctl rollback: --to <version> is required")
		return exitUsage
	}
	// Versions become release paths: validate them as strict SemVer
	// before they feed into filesystem path construction.
	if err := target.CheckVersion(*to); err != nil {
		fmt.Fprintf(stderr, "deployctl rollback: --to: %v\n", err)
		return exitUsage
	}
	if *from != "" {
		if err := target.CheckVersion(*from); err != nil {
			fmt.Fprintf(stderr, "deployctl rollback: --from: %v\n", err)
			return exitUsage
		}
	}

	dc, err := loadDeploymentContext(*repoDir, envName)
	if err != nil {
		fmt.Fprintf(stderr, "✗ rollback %s: %v\n", envName, err)
		return exitUsage
	}
	fromRel := dc.Release
	if *from != "" && *from != fromRel.Metadata.Version {
		fromRel, err = dc.loadRelease(*from)
		if err != nil {
			fmt.Fprintf(stderr, "✗ rollback %s: load --from release: %v\n", envName, err)
			return exitUsage
		}
	}
	toRel, err := dc.loadRelease(*to)
	if err != nil {
		fmt.Fprintf(stderr, "✗ rollback %s: load --to release: %v\n", envName, err)
		return exitUsage
	}

	// Prepare BOTH bundles up front: a rollback that cannot prove its
	// material must fail before any confirmation, not after.
	fromBundle, err := prepareBundle(ctx, dc.RepoDir, fromRel)
	if err != nil {
		fmt.Fprintf(stderr, "✗ rollback %s: %v\n", envName, err)
		return exitUsage
	}
	toBundle, err := prepareBundle(ctx, dc.RepoDir, toRel)
	if err != nil {
		fmt.Fprintf(stderr, "✗ rollback %s: %v\n", envName, err)
		return exitUsage
	}

	// Typed confirmation BEFORE the target is contacted: the human
	// authorization boundary is also the first production contact.
	// Nothing — not even an authenticated connection — happens before
	// the sentence matches. The sentence names environment and
	// transition; near-misses are refused. This is deliberate friction
	// by design.
	sentence := fmt.Sprintf("rollback %s to %s", envName, toRel.Metadata.Version)
	got := *confirm
	if got == "" {
		fmt.Fprintf(stdout, "Environment   %s\n", envName)
		fmt.Fprintf(stdout, "Undoes        %s (%s)\n", fromRel.Metadata.Version, shortDigest(fromRel.Bundle.Digest))
		fmt.Fprintf(stdout, "Restores      %s (%s)\n", toRel.Metadata.Version, shortDigest(toRel.Bundle.Digest))
		fmt.Fprintf(stdout, "Authorization manual (emergency)\n\n")
		fmt.Fprintf(stdout, "A recovery may run the failed release's rollback hook and the restored\nrelease's apply/verify on production. Type the sentence to confirm:\n  %s\n> ", sentence)
		reader := bufio.NewReader(os.Stdin)
		line, rerr := reader.ReadString('\n')
		if rerr != nil && line == "" {
			fmt.Fprintf(stderr, "\n✗ rollback aborted: confirmation could not be read (%v)\n", rerr)
			return exitFailed
		}
		got = strings.TrimSpace(line)
	}
	if got != sentence {
		fmt.Fprintf(stderr, "✗ rollback aborted: confirmation does not match %q — nothing was executed, the target was not contacted\n", sentence)
		return exitFailed
	}

	tgt, err := connect(ctx, dc.Target)
	if err != nil {
		return reportConnectFailure(err, "rollback", envName, stderr)
	}

	ownerID := *owner
	if ownerID == "" {
		ownerID = defaultOwner()
	}
	rep, err := lifecycle.Rollback(ctx, lifecycle.RollbackInput{
		Target:         tgt,
		TargetManifest: dc.Target,
		Environment:    dc.Env,
		FromRelease:    fromRel,
		FromBundle:     fromBundle,
		ToRelease:      toRel,
		ToBundle:       toBundle,
		Authorization:  lifecycle.RollbackManual,
		Owner:          ownerID,
	})
	return reportRollback(ctx, rep, err, tgt, envName, stdout, stderr)
}

func reportRollback(ctx context.Context, rep *lifecycle.RollbackReport, err error, tgt *target.Target, envName string, stdout, stderr io.Writer) int {
	if err != nil {
		fmt.Fprintf(stderr, "✗ rollback %s: infrastructure failure: %v\n", envName, err)
		switch {
		case rep != nil && rep.Committed:
			fmt.Fprintln(stderr, "  The observed state IS committed — the recovery itself succeeded.")
			fmt.Fprintln(stderr, "  What failed is bookkeeping (marker/history cleanup): the leftover")
			fmt.Fprintln(stderr, "  marker still blocks normal operation. Run `deployctl status` and")
			fmt.Fprintln(stderr, "  follow its guidance.")
		case rep != nil && rep.AlreadyRecovered:
			fmt.Fprintln(stderr, "  The recovery IS already committed (observed state carries its")
			fmt.Fprintln(stderr, "  operationId); what failed is marker cleanup, which still blocks")
			fmt.Fprintln(stderr, "  normal operation. Run `deployctl status` and follow its guidance.")
		case rep != nil && hasUnresolvedMarkers(ctx, tgt, rep.Project, rep.Environment):
			fmt.Fprintln(stderr, "  The outcome is UNCERTAIN: consequential recovery work may have")
			fmt.Fprintln(stderr, "  executed (an attempt or recovery marker is unresolved).")
			fmt.Fprintln(stderr, "  Do not retry. Run `deployctl status` and follow the recovery guidance.")
		default:
			fmt.Fprintln(stderr, "  No consequential work was executed — nothing was applied to the target.")
			fmt.Fprintln(stderr, "  After the infrastructure problem is fixed, the recovery can simply")
			fmt.Fprintln(stderr, "  be rerun.")
		}
		return exitInfra
	}
	fmt.Fprintf(stdout, "Recovery %s: %s → %s\n", rep.Project+"/"+rep.Environment, rep.FromVersion, rep.ToVersion)
	for i := range rep.Stages {
		renderStage(stdout, rep.Stages[i])
		renderStageDetails(stdout, rep.Stages[i])
	}
	if rep.AlreadyRecovered {
		fmt.Fprintf(stdout, "✓ recovery %s is already committed (observed state carries its operationId); markers cleaned up, nothing was executed\n", rep.RecoveryID)
		return exitOK
	}
	if rep.Committed {
		fmt.Fprintf(stdout, "✓ production is running %s@%s (verified, state committed, recovery %s resolved)\n", rep.ToVersion, rep.ToBundleDigest, rep.RecoveryID)
		if rep.RecoveryMarkerCreated && !rep.RecoveryResolved {
			fmt.Fprintf(stdout, "  Note: Git desired state still pins the rolled-back release — open the reconcile PR.\n")
		}
		return exitOK
	}
	fmt.Fprintf(stdout, "✗ recovery failed: %s\n", rep.FailureReason)
	if rep.RecoveryRequired {
		fmt.Fprintln(stdout, "\nRECOVERY REQUIRED")
		fmt.Fprintln(stdout, "  The recovery's own outcome is unresolved. Its marker blocks every")
		fmt.Fprintln(stdout, "  further deployment and recovery until an operator has verified the")
		fmt.Fprintln(stdout, "  target and resolved the situation explicitly.")
		fmt.Fprintln(stdout, "  Inspect: deployctl status "+envName)
	}
	return exitFailed
}

func shortDigest(d string) string {
	if len(d) > 19 {
		return d[:19] + "…"
	}
	return d
}
