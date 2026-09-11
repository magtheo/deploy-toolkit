package main

import (
	"bufio"
	"context"
	"errors"
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
	lexFlags, positional, jsonMode, missingValue := lexArgs(args, map[string]bool{"repo-dir": true, "owner": true, "to": true, "from": true, "confirm": true})
	if missingValue != "" {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult(cmdRollback, envNameOr(positional), "--"+missingValue+" requires a value"))
		}
		fmt.Fprintf(stderr, "deployctl rollback: --%s requires a value\n", missingValue)
		return exitUsage
	}
	if len(positional) == 0 {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult(cmdRollback, "", "usage: deployctl rollback <environment> --to <version>"))
		}
		fmt.Fprintln(stderr, "usage: deployctl rollback <environment> --to <version> [--from <version>] [--repo-dir .] [--confirm \"...\"]")
		return exitUsage
	}
	if len(positional) > 1 {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult(cmdRollback, positional[0], fmt.Sprintf("unexpected argument %q", positional[1])))
		}
		fmt.Fprintf(stderr, "deployctl rollback: unexpected argument %q\n", positional[1])
		return exitUsage
	}
	envName := positional[0]
	if strings.Contains(envName, "/") {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult("rollback", envName, "environment must be a bare name"))
		}
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
	_ = fs.Bool("json", false, "emit a single deployctl.result/v1 JSON document on stdout")
	if err := fs.Parse(lexFlags); err != nil {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult(cmdRollback, envName, "invalid flags: "+err.Error()))
		}
		fmt.Fprintf(stderr, "deployctl rollback: %v\n", err)
		return exitUsage
	}
	// JSON mode is explicitly NON-interactive: CI must never block on a
	// terminal. A required confirmation without --confirm is a usage
	// error, emitted before anything else happens.
	if jsonMode && *confirm == "" {
		return emitJSON(stdout, usageErrorResult(cmdRollback, envName, "--confirm is required in --json mode; the interactive prompt is never read"))
	}
	humanOut := io.Writer(stdout)
	if jsonMode {
		humanOut = io.Discard
	}
	if *to == "" {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult(cmdRollback, envName, "--to <version> is required"))
		}
		fmt.Fprintln(stderr, "deployctl rollback: --to <version> is required")
		return exitUsage
	}
	// Versions become release paths: validate them as strict SemVer
	// before they feed into filesystem path construction.
	if err := target.CheckVersion(*to); err != nil {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult(cmdRollback, envName, "--to: "+err.Error()))
		}
		fmt.Fprintf(stderr, "deployctl rollback: --to: %v\n", err)
		return exitUsage
	}
	if *from != "" {
		if err := target.CheckVersion(*from); err != nil {
			if jsonMode {
				return emitJSON(stdout, usageErrorResult(cmdRollback, envName, "--from: "+err.Error()))
			}
			fmt.Fprintf(stderr, "deployctl rollback: --from: %v\n", err)
			return exitUsage
		}
	}

	dc, err := loadDeploymentContext(*repoDir, envName)
	if err != nil {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult("rollback", envName, err.Error()))
		}
		fmt.Fprintf(stderr, "✗ rollback %s: %v\n", envName, err)
		return exitUsage
	}
	fromRel := dc.Release
	if *from != "" && *from != fromRel.Metadata.Version {
		fromRel, err = dc.loadRelease(*from)
		if err != nil {
			if jsonMode {
				return emitJSON(stdout, usageErrorResult("rollback", envName, "load --from release: "+err.Error()))
			}
			fmt.Fprintf(stderr, "✗ rollback %s: load --from release: %v\n", envName, err)
			return exitUsage
		}
	}
	toRel, err := dc.loadRelease(*to)
	if err != nil {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult("rollback", envName, "load --to release: "+err.Error()))
		}
		fmt.Fprintf(stderr, "✗ rollback %s: load --to release: %v\n", envName, err)
		return exitUsage
	}

	// Prepare BOTH bundles up front: a rollback that cannot prove its
	// material must fail before any confirmation, not after.
	fromBundle, err := prepareBundle(ctx, dc.RepoDir, fromRel)
	if err != nil {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult("rollback", envName, err.Error()))
		}
		fmt.Fprintf(stderr, "✗ rollback %s: %v\n", envName, err)
		return exitUsage
	}
	toBundle, err := prepareBundle(ctx, dc.RepoDir, toRel)
	if err != nil {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult("rollback", envName, err.Error()))
		}
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
		fmt.Fprintf(humanOut, "Environment   %s\n", envName)
		fmt.Fprintf(humanOut, "Undoes        %s (%s)\n", fromRel.Metadata.Version, shortDigest(fromRel.Bundle.Digest))
		fmt.Fprintf(humanOut, "Restores      %s (%s)\n", toRel.Metadata.Version, shortDigest(toRel.Bundle.Digest))
		fmt.Fprintf(humanOut, "Authorization manual (emergency)\n\n")
		fmt.Fprintf(humanOut, "A recovery may run the failed release's rollback hook and the restored\nrelease's apply/verify on production. Type the sentence to confirm:\n  %s\n> ", sentence)
		reader := bufio.NewReader(os.Stdin)
		line, rerr := reader.ReadString('\n')
		if rerr != nil && line == "" {
			fmt.Fprintf(stderr, "\n✗ rollback aborted: confirmation could not be read (%v)\n", rerr)
			return exitFailed
		}
		got = strings.TrimSpace(line)
	}
	if got != sentence {
		if jsonMode {
			return emitJSON(stdout, &resultEnvelope{Schema: resultSchemaV1, Command: "rollback", Outcome: outcomeRefused, Environment: envName, Message: fmt.Sprintf("confirmation does not match %q — nothing was executed, the target was not contacted", sentence)})
		}
		fmt.Fprintf(stderr, "✗ rollback aborted: confirmation does not match %q — nothing was executed, the target was not contacted\n", sentence)
		return exitFailed
	}

	tgt, err := connect(ctx, dc.Target)
	if err != nil {
		return reportConnectFailure(err, "rollback", envName, stderr, jsonMode, stdout)
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
	if jsonMode {
		env, _ := rollbackResult(rep, err)
		return emitJSON(stdout, env)
	}
	return reportRollback(rep, err, envName, humanOut, stderr)
}

func reportRollback(rep *lifecycle.RollbackReport, err error, envName string, stdout, stderr io.Writer) int {
	if err != nil {
		if errors.Is(err, lifecycle.ErrEnvLockHeld) {
			fmt.Fprintf(stderr, "✗ rollback %s: refused: the environment lock is held — another\noperation may currently be executing.\n", envName)
			warnLockReleaseFailed(stderr, err)
			fmt.Fprintln(stderr, "  Do not rerun mechanically. Run `deployctl status`; only remove a stale")
			fmt.Fprintln(stderr, "  lock after verifying no recovery is in flight.")
			return exitFailed
		}
		if errors.Is(err, target.ErrEvidenceInvalid) {
			fmt.Fprintf(stderr, "✗ rollback %s: refused: durable evidence on the target exists but is invalid.\n", envName)
			warnLockReleaseFailed(stderr, err)
			fmt.Fprintln(stderr, "  Rerunning cannot help while the evidence is invalid. Run `deployctl status`, inspect")
			fmt.Fprintln(stderr, "  the affected evidence and verify the target's actual state before following the")
			fmt.Fprintln(stderr, "  recovery procedure. Do NOT edit observed state merely to make this proceed.")
			return exitFailed
		}
		fmt.Fprintf(stderr, "✗ rollback %s: infrastructure failure: %v\n", envName, err)
		warnLockReleaseFailed(stderr, err)
		switch {
		case rep != nil && rep.LockRetained:
			fmt.Fprintln(stderr, "  A lifecycle hook's execution fate could not be established: the hook")
			fmt.Fprintln(stderr, "  process may still be running. The environment lock was deliberately")
			fmt.Fprintln(stderr, "  retained. Verify the target — nothing may still be executing — and")
			fmt.Fprintln(stderr, "  only then remove the lock by hand and proceed deliberately.")
			if rep.RecoveryStarted {
				fmt.Fprintln(stderr, "  A recovery marker is unresolved: run the recovery after cleanup.")
			}
			return exitInfra
		case rep != nil && rep.Committed:
			fmt.Fprintln(stderr, "  The observed state is committed; the recovery itself succeeded.")
			fmt.Fprintln(stderr, "  Post-commit bookkeeping failed. Run `deployctl status` before")
			fmt.Fprintln(stderr, "  taking another action.")
		case rep != nil && rep.AlreadyRecovered:
			fmt.Fprintln(stderr, "  The recovery is already committed (observed state carries its")
			fmt.Fprintln(stderr, "  operationId); post-commit bookkeeping failed. Run")
			fmt.Fprintln(stderr, "  `deployctl status` before taking another action.")
		case rep != nil && rep.RecoveryStarted:
			fmt.Fprintf(stderr, "  The outcome is UNCERTAIN: consequential recovery work may have\n  executed (recovery %s is unresolved).\n", orUnknown(rep.RecoveryID))
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
