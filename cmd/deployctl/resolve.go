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

// runRecovery dispatches the recovery group. Today: resolve — the
// explicit, supported end of an unresolved situation.
func runRecovery(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: deployctl recovery resolve <environment> [--repo-dir .] [--owner identity] [--confirm \"...\"]")
		return exitUsage
	}
	switch args[0] {
	case "resolve":
		return runRecoveryResolve(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "deployctl recovery: unknown subcommand %q\n", args[0])
		return exitUsage
	}
}

// runRecoveryResolve lifts the deployment block after an operator has
// verified the target BY HAND. It executes no hooks and changes nothing
// about what is running: it records who resolved what against which
// observed state, then removes the markers. The typed confirmation names
// the exact marker ids being resolved — the engine re-reads them under
// the lock and refuses on any mismatch.
func runRecoveryResolve(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "usage: deployctl recovery resolve <environment> [--repo-dir .] [--owner identity] [--confirm \"...\"]")
		return exitUsage
	}
	envName := args[0]
	if strings.Contains(envName, "/") {
		fmt.Fprintf(stderr, "deployctl recovery resolve: environment must be a bare name, got %q\n", envName)
		return exitUsage
	}
	fs := flag.NewFlagSet("recovery resolve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoDir := fs.String("repo-dir", ".", "checkout containing .deploy/")
	owner := fs.String("owner", "", "identity recorded as resolution evidence (default user@host)")
	confirm := fs.String("confirm", "", "confirmation sentence; omit to be prompted interactively")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "deployctl recovery resolve: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}

	dc, err := loadDeploymentContext(*repoDir, envName)
	if err != nil {
		fmt.Fprintf(stderr, "✗ recovery resolve %s: %v\n", envName, err)
		return exitUsage
	}
	tgt, err := connect(ctx, dc.Target)
	if err != nil {
		return reportConnectFailure(err, "recovery resolve", envName, stderr)
	}

	project, env := dc.Release.Metadata.Project, dc.Env.Metadata.Name

	// Read the situation exactly as status would: absent / present /
	// unreadable are different facts, and unreadable evidence must be
	// repaired, never resolved.
	st, serr := tgt.ReadState(ctx, project, env)
	attempt, aerr := tgt.ReadAttempt(ctx, project, env)
	recovery, rerr := tgt.ReadRecovery(ctx, project, env)
	if serr != nil && !errors.Is(serr, target.ErrStateAbsent) {
		fmt.Fprintf(stderr, "✗ recovery resolve %s: observed state is unreadable: %v\n", envName, serr)
		fmt.Fprintln(stderr, "  Unreadable evidence must be repaired, not resolved. Run `deployctl status`.")
		return exitFailed
	}
	if aerr != nil && !errors.Is(aerr, target.ErrAttemptAbsent) {
		fmt.Fprintf(stderr, "✗ recovery resolve %s: attempt marker is unreadable: %v\n", envName, aerr)
		fmt.Fprintln(stderr, "  Unreadable evidence must be repaired, not resolved. Run `deployctl status`.")
		return exitFailed
	}
	if rerr != nil && !errors.Is(rerr, target.ErrRecoveryAbsent) {
		fmt.Fprintf(stderr, "✗ recovery resolve %s: recovery marker is unreadable: %v\n", envName, rerr)
		fmt.Fprintln(stderr, "  Unreadable evidence must be repaired, not resolved. Run `deployctl status`.")
		return exitFailed
	}
	if aerr != nil && rerr != nil {
		fmt.Fprintf(stdout, "Nothing to resolve: no unresolved attempt or recovery marker on %s.\n", envName)
		return exitOK
	}
	// A held lock means an operation may be executing right now — the
	// engine refuses under a held lock; surface it as a determined
	// refusal before asking the human to confirm anything.
	if held, lerr := lockHeld(ctx, tgt, dc); lerr == nil && held {
		fmt.Fprintf(stderr, "✗ recovery resolve %s: the environment lock is HELD — an operation may be executing.\n", envName)
		fmt.Fprintln(stderr, "  Wait for it to finish and verify no deployment is in flight first.")
		return exitFailed
	}

	fmt.Fprintf(stdout, "Environment   %s\n", envName)
	fmt.Fprintf(stdout, "Observed      %s\n", observedLine(st, serr))
	if rerr == nil {
		fmt.Fprintf(stdout, "Recovery      %s: %s → %s (id %s, authorization %s)\n", recovery.RecoveryID, recovery.FromRelease, recovery.ToRelease, recovery.RecoveryID, recovery.Authorization)
	}
	if aerr == nil {
		fmt.Fprintf(stdout, "Attempt       %s: %s → %s (id %s)\n", attempt.AttemptID, orNone(attempt.FromRelease), attempt.ToRelease, attempt.AttemptID)
	}
	fmt.Fprintf(stdout, "Resolution    records evidence, then removes the markers. It runs NO hooks\n")
	fmt.Fprintf(stdout, "              and changes NOTHING about what is running. Use only after\n")
	fmt.Fprintf(stdout, "              verifying the target by hand (app health, digest, migrations).\n\n")

	// The typed sentence names the exact markers being resolved.
	var sentence string
	switch {
	case rerr == nil && aerr == nil:
		sentence = fmt.Sprintf("resolve %s recovery %s", envName, recovery.RecoveryID)
	case rerr == nil:
		sentence = fmt.Sprintf("resolve %s recovery %s", envName, recovery.RecoveryID)
	default:
		sentence = fmt.Sprintf("resolve %s attempt %s", envName, attempt.AttemptID)
	}
	got := *confirm
	if got == "" {
		fmt.Fprintf(stdout, "Type the sentence to confirm:\n  %s\n> ", sentence)
		line, rerr2 := bufio.NewReader(os.Stdin).ReadString('\n')
		if rerr2 != nil && line == "" {
			fmt.Fprintf(stderr, "\n✗ resolve aborted: confirmation could not be read (%v)\n", rerr2)
			return exitFailed
		}
		got = strings.TrimSpace(line)
	}
	if got != sentence {
		fmt.Fprintf(stderr, "✗ resolve aborted: confirmation does not match %q — nothing was changed\n", sentence)
		return exitFailed
	}

	ownerID := *owner
	if ownerID == "" {
		ownerID = defaultOwner()
	}
	rep, err := lifecycle.Resolve(ctx, lifecycle.ResolveInput{
		Target:            tgt,
		TargetManifest:    dc.Target,
		Project:           project,
		Environment:       dc.Env,
		Owner:             ownerID,
		ConfirmRecoveryID: confirmIDOrEmpty(rerr == nil, recovery.RecoveryID),
		ConfirmAttemptID:  confirmIDOrEmpty(aerr == nil, attempt.AttemptID),
	})
	if err != nil {
		fmt.Fprintf(stderr, "✗ recovery resolve %s: %v\n", envName, err)
		fmt.Fprintln(stderr, "  The markers were NOT removed — the block is still in place.")
		fmt.Fprintln(stderr, "  Run `deployctl status` and follow its guidance.")
		return exitFailed
	}
	if rep.NothingToResolve {
		fmt.Fprintf(stdout, "✓ nothing to resolve — the environment is not blocked\n")
		return exitOK
	}
	fmt.Fprintf(stdout, "✓ resolved: recorded evidence (history seq %d) and removed", rep.HistorySeq)
	if rep.ResolvedRecoveryID != "" {
		fmt.Fprintf(stdout, " recovery %s", rep.ResolvedRecoveryID)
	}
	if rep.ResolvedAttemptID != "" {
		fmt.Fprintf(stdout, " attempt %s", rep.ResolvedAttemptID)
	}
	fmt.Fprintf(stdout, "\n  The block is lifted. Verify the target still behaves as expected, then\n  deploy or recover normally. Observed state was not modified.\n")
	return exitOK
}

// confirmIDOrEmpty passes the marker id only when the marker is present;
// the empty string tells the engine this kind is not being resolved.
func confirmIDOrEmpty(present bool, id string) string {
	if !present {
		return ""
	}
	return id
}

func observedLine(st target.State, stateErr error) string {
	if stateErr != nil {
		return "none (never deployed)"
	}
	if st.Current == nil {
		return "none recorded (valid empty state)"
	}
	return fmt.Sprintf("%s@%s (operation %s)", st.Current.Release, shortDigest(st.Current.BundleDigest), operationDisplay(st.Current.OperationID))
}
