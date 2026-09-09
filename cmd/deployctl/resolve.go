package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/magtheo/deploy-toolkit/internal/lifecycle"
	"github.com/magtheo/deploy-toolkit/internal/target"
)

// runRecovery dispatches the recovery group. Today: resolve — the
// explicit, supported end of an unresolved situation.
func runRecovery(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: deployctl recovery resolve <environment> [recovery <id>] [attempt <id>] [--repo-dir .] [--owner identity] [--confirm \"...\"]")
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

// resolveScope is the set of markers the operator is authorizing for
// removal. It is chosen EXPLICITLY — by positional selectors or by the
// typed sentence — never derived from what happens to exist on the
// target: the thing typed by the operator must be exactly the set of
// markers sent to the engine.
type resolveScope struct {
	recoveryID string
	attemptID  string
}

var markerIDPattern = mustCompileHex16()

func mustCompileHex16() *regexp.Regexp {
	return regexp.MustCompile(`^[0-9a-f]{16}$`)
}

// parseResolveScope parses the authorized set from positional selector
// tokens and/or the confirmation sentence. Exactly one of them defines
// the scope; when both are given they must agree.
func parseResolveScope(envName string, tokens []string, confirm string) (sentence string, sc resolveScope, err error) {
	if len(tokens) > 0 {
		sc, err = parseSelectorTokens(tokens)
		if err != nil {
			return "", sc, err
		}
		sentence = canonicalResolveSentence(envName, sc)
		if confirm != "" && confirm != sentence {
			return "", sc, fmt.Errorf("--confirm does not match the selected authorization %q: %w", sentence, errScopeMismatch)
		}
		return sentence, sc, nil
	}
	if confirm != "" {
		sc, err = parseResolveSentence(envName, confirm)
		if err != nil {
			return "", sc, err
		}
		return confirm, sc, nil
	}
	return "", sc, errInteractiveConfirmation
}

// errScopeMismatch marks an authorization disagreement (selector set vs
// --confirm sentence) — a failed authorization, not a usage error.
var errScopeMismatch = errors.New("authorization mismatch")

// parseSelectorTokens parses [recovery <id>] [attempt <id>] in either
// order, each kind at most once, at least one required.
func parseSelectorTokens(tokens []string) (resolveScope, error) {
	sc, _, err := parseScopePairs(tokens)
	return sc, err
}

// parseResolveSentence parses the typed authorization sentence:
//
//	resolve <env> [recovery <id>] [attempt <id>]
//
// in either pair order, at least one pair required.
func parseResolveSentence(envName, sentence string) (resolveScope, error) {
	fields := strings.Fields(sentence)
	if len(fields) < 4 || fields[0] != "resolve" || fields[1] != envName {
		return resolveScope{}, fmt.Errorf("not a valid resolution sentence for %s", envName)
	}
	sc, used, err := parseScopePairs(fields[2:])
	if err != nil {
		return resolveScope{}, err
	}
	if used != len(fields[2:]) {
		return resolveScope{}, fmt.Errorf("unexpected token %q in the resolution sentence", fields[2:][used])
	}
	return sc, nil
}

func parseScopePairs(tokens []string) (resolveScope, int, error) {
	var sc resolveScope
	i := 0
	for i < len(tokens) {
		if i+1 >= len(tokens) {
			return sc, i, fmt.Errorf("selector %q is missing an id", tokens[i])
		}
		kind, id := tokens[i], tokens[i+1]
		if !markerIDPattern.MatchString(id) {
			return sc, i, fmt.Errorf("%q is not a marker id (16 hex digits)", id)
		}
		switch kind {
		case "recovery":
			if sc.recoveryID != "" {
				return sc, i, fmt.Errorf("recovery selected more than once")
			}
			sc.recoveryID = id
		case "attempt":
			if sc.attemptID != "" {
				return sc, i, fmt.Errorf("attempt selected more than once")
			}
			sc.attemptID = id
		default:
			return sc, i, fmt.Errorf("unknown selector %q (want \"recovery\" or \"attempt\")", kind)
		}
		i += 2
	}
	if sc.recoveryID == "" && sc.attemptID == "" {
		return sc, i, fmt.Errorf("a resolution must name at least one marker: recovery <id> and/or attempt <id>")
	}
	return sc, i, nil
}

func canonicalResolveSentence(envName string, sc resolveScope) string {
	parts := []string{"resolve", envName}
	if sc.recoveryID != "" {
		parts = append(parts, "recovery", sc.recoveryID)
	}
	if sc.attemptID != "" {
		parts = append(parts, "attempt", sc.attemptID)
	}
	return strings.Join(parts, " ")
}

// runRecoveryResolve lifts the deployment block after an operator has
// verified the target BY HAND. It executes no hooks and changes nothing
// about what is running: it records who resolved which markers against
// which observed state, then removes exactly the markers the typed
// sentence names. The engine re-reads those markers under the lock and
// refuses on any mismatch.
func runRecoveryResolve(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	jsonMode := wantsJSON(args)
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult(cmdRecoveryResolve, "", "usage: deployctl recovery resolve <environment> [recovery <id>] [attempt <id>]"))
		}
		fmt.Fprintln(stderr, "usage: deployctl recovery resolve <environment> [recovery <id>] [attempt <id>] [--repo-dir .] [--owner identity] [--confirm \"...\"]")
		return exitUsage
	}
	envName := args[0]
	if strings.Contains(envName, "/") {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult(cmdRecoveryResolve, envName, "environment must be a bare name"))
		}
		fmt.Fprintf(stderr, "deployctl recovery resolve: environment must be a bare name, got %q\n", envName)
		return exitUsage
	}
	fs := flag.NewFlagSet("recovery resolve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoDir := fs.String("repo-dir", ".", "checkout containing .deploy/")
	owner := fs.String("owner", "", "identity recorded as resolution evidence (default user@host)")
	confirm := fs.String("confirm", "", "confirmation sentence; omit to be prompted interactively")
	_ = fs.Bool("json", false, "emit a single deployctl.result/v1 JSON document on stdout")
	// Go's flag parsing stops at the first positional token; split flags
	// and positional selectors manually so both orders work. Value flags
	// consume one token; boolean flags (json) consume none.
	flagTokens, selectors := splitFlagsAndValues(args[1:], map[string]bool{"json": true})
	if err := fs.Parse(flagTokens); err != nil {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult(cmdRecoveryResolve, envName, "invalid flags: "+err.Error()))
		}
		return exitUsage
	}

	// Command SYNTAX is validated before any target access — and before
	// the no-markers no-op return: grammar errors are usage errors
	// regardless of target state. (The interactive path's sentence parse
	// necessarily happens later, once the human has typed it.)
	var scope resolveScope
	var sentence string
	var err error
	interactive := len(selectors) == 0 && *confirm == ""

	// JSON mode is explicitly NON-interactive, UNCONDITIONALLY: the
	// confirmation is required in machine mode even when explicit
	// selectors narrow the scope. Selectors choose WHAT would be
	// resolved; --confirm is the act of authorizing it. With --json and
	// no --confirm we emit one structured usage error before any target
	// access — stdin is never read, so automation can never block.
	if jsonMode && *confirm == "" {
		return emitJSON(stdout, usageErrorResult(cmdRecoveryResolve, envName, "--confirm is required in --json mode: selectors choose the scope, the confirmation authorizes it, and the interactive prompt is never read"))
	}
	if !interactive {
		sentence, scope, err = parseResolveScope(envName, selectors, *confirm)
		if err != nil {
			if jsonMode {
				if errors.Is(err, errScopeMismatch) {
					return emitJSON(stdout, &resultEnvelope{Schema: resultSchemaV1, Command: cmdRecoveryResolve, Outcome: outcomeRefused, Environment: envName, Message: err.Error()})
				}
				return emitJSON(stdout, usageErrorResult(cmdRecoveryResolve, envName, err.Error()))
			}
			fmt.Fprintf(stderr, "✗ recovery resolve %s: %v\n", envName, err)
			if errors.Is(err, errScopeMismatch) {
				return exitFailed
			}
			return exitUsage
		}
	}

	dc, err := loadDeploymentContext(*repoDir, envName)
	if err != nil {
		if jsonMode {
			return emitJSON(stdout, usageErrorResult(cmdRecoveryResolve, envName, err.Error()))
		}
		fmt.Fprintf(stderr, "✗ recovery resolve %s: %v\n", envName, err)
		return exitUsage
	}
	tgt, err := connect(ctx, dc.Target)
	if err != nil {
		return reportConnectFailure(err, cmdRecoveryResolve, envName, stderr, jsonMode, stdout)
	}

	project, env := dc.Release.Metadata.Project, dc.Env.Metadata.Name

	// Read the situation exactly as status would: absent / present /
	// unreadable are different facts, and unreadable evidence must be
	// repaired, never resolved.
	st, serr := tgt.ReadState(ctx, project, env)
	attempt, aerr := tgt.ReadAttempt(ctx, project, env)
	recovery, rerr := tgt.ReadRecovery(ctx, project, env)
	// The preflight classifies EXACTLY as the engine does — the CLI
	// must not erase the distinction the sentinels encode:
	//
	//   absent                → normal (nothing to resolve)
	//   target.ErrEvidenceInvalid → refusal (exists, fails validation:
	//                             untrustworthy evidence must be
	//                             repaired, never resolved)
	//   anything else         → infrastructure failure (we could not
	//                             establish the facts at all — a dead
	//                             transport is uncertainty, not policy)
	//
	// Refusal documents carry the same marker facts the engine would
	// report: what is on the target remains on the target. Nothing has
	// been removed at preflight time, so a successful read means the
	// marker remains.
	blockData := func() *resolveResultData {
		return &resolveResultData{RemainingAttemptID: attemptIDOrEmpty(aerr, attempt), RemainingRecoveryID: recoveryIDOrEmpty(rerr, recovery)}
	}
	blocked := aerr == nil || rerr == nil
	refuseJSON := func(format string, args ...any) int {
		if jsonMode {
			return emitJSON(stdout, &resultEnvelope{Schema: resultSchemaV1, Command: cmdRecoveryResolve, Outcome: outcomeRefused, Project: project, Environment: envName, RecoveryRequired: true, Message: fmt.Sprintf(format, args...), Data: blockData()})
		}
		fmt.Fprintf(stderr, "✗ recovery resolve %s: %s\n", envName, fmt.Sprintf(format, args...))
		fmt.Fprintln(stderr, "  Unreadable evidence must be repaired, not resolved. Run `deployctl status`.")
		return exitFailed
	}
	infraJSON := func(format string, args ...any) int {
		if jsonMode {
			return emitJSON(stdout, &resultEnvelope{Schema: resultSchemaV1, Command: cmdRecoveryResolve, Outcome: outcomeInfraFailed, Project: project, Environment: envName, RecoveryRequired: blocked, Message: fmt.Sprintf(format, args...), Data: blockData()})
		}
		fmt.Fprintf(stderr, "✗ recovery resolve %s: %s\n", envName, fmt.Sprintf(format, args...))
		return exitInfra
	}
	if serr != nil && !errors.Is(serr, target.ErrStateAbsent) {
		if errors.Is(serr, target.ErrEvidenceInvalid) {
			return refuseJSON("observed state exists but is invalid: %v", serr)
		}
		return infraJSON("cannot read the observed state: %v", serr)
	}
	if aerr != nil && !errors.Is(aerr, target.ErrAttemptAbsent) {
		if errors.Is(aerr, target.ErrEvidenceInvalid) {
			return refuseJSON("attempt marker exists but is invalid: %v", aerr)
		}
		return infraJSON("cannot read the attempt marker: %v", aerr)
	}
	if rerr != nil && !errors.Is(rerr, target.ErrRecoveryAbsent) {
		if errors.Is(rerr, target.ErrEvidenceInvalid) {
			return refuseJSON("recovery marker exists but is invalid: %v", rerr)
		}
		return infraJSON("cannot read the recovery marker: %v", rerr)
	}
	if aerr != nil && rerr != nil {
		if jsonMode {
			return emitJSON(stdout, &resultEnvelope{Schema: resultSchemaV1, Command: cmdRecoveryResolve, Outcome: outcomeSuccess, Project: project, Environment: envName, SafeToRetry: true, Message: "nothing to resolve; the environment is not blocked", Data: &resolveResultData{NothingToResolve: true}})
		}
		fmt.Fprintf(stdout, "Nothing to resolve: no unresolved attempt or recovery marker on %s.\n", envName)
		return exitOK
	}
	// The lock precheck fails CLOSED: if it cannot establish whether an
	// operation is active, the operator is not asked to authorize
	// anything. (The engine enforces the lock itself; this keeps the
	// human authorization honest.)
	if held, lerr := lockHeld(ctx, tgt, dc); lerr != nil {
		// An unreadable LOCK is not a policy refusal — it is the same
		// infrastructural uncertainty as an unreadable marker: we cannot
		// establish whether an operation is active.
		return infraJSON("cannot establish whether an operation is active: %v", lerr)
	} else if held {
		if jsonMode {
			return emitJSON(stdout, &resultEnvelope{Schema: resultSchemaV1, Command: cmdRecoveryResolve, Outcome: outcomeRefused, Project: project, Environment: envName, RecoveryRequired: blocked, Message: "the environment lock is HELD — an operation may be executing; wait for it to finish first", Data: blockData()})
		}
		fmt.Fprintf(stderr, "✗ recovery resolve %s: the environment lock is HELD — an operation may be executing.\n", envName)
		fmt.Fprintln(stderr, "  Wait for it to finish and verify no deployment is in flight first.")
		return exitFailed
	}

	// The authorized marker set was parsed above from explicit
	// selectors or the typed sentence — never marker presence.
	humanOut := io.Writer(stdout)
	if jsonMode {
		humanOut = io.Discard
	}
	fmt.Fprintf(humanOut, "Environment   %s\n", envName)
	fmt.Fprintf(humanOut, "Observed      %s\n", observedLine(st, serr))
	if rerr == nil {
		fmt.Fprintf(humanOut, "Recovery      %s: %s → %s (id %s, authorization %s)\n", recovery.RecoveryID, recovery.FromRelease, recovery.ToRelease, recovery.RecoveryID, recovery.Authorization)
	}
	if aerr == nil {
		fmt.Fprintf(humanOut, "Attempt       %s: %s → %s (id %s)\n", attempt.AttemptID, orNone(attempt.FromRelease), attempt.ToRelease, attempt.AttemptID)
	}
	fmt.Fprintf(humanOut, "Resolution    records evidence, then removes exactly the markers you authorize.\n")
	fmt.Fprintf(humanOut, "              It runs NO hooks and changes NOTHING about what is running.\n")
	fmt.Fprintf(humanOut, "              Use only after verifying the target by hand (app health,\n")
	fmt.Fprintf(humanOut, "              digest, migrations).\n\n")

	if interactive {
		fmt.Fprintf(humanOut, "Type the sentence for what you authorize:\n")
		fmt.Fprintf(humanOut, "  resolve %s recovery <id>\n", envName)
		fmt.Fprintf(humanOut, "  resolve %s attempt <id>\n", envName)
		fmt.Fprintf(humanOut, "  resolve %s recovery <id> attempt <id>\n> ", envName)
		line, rerr2 := bufio.NewReader(os.Stdin).ReadString('\n')
		if rerr2 != nil && line == "" {
			fmt.Fprintf(stderr, "\n✗ resolve aborted: confirmation could not be read (%v)\n", rerr2)
			return exitFailed
		}
		got := strings.TrimSpace(line)
		scope, err = parseResolveSentence(envName, got)
		if err != nil {
			if jsonMode {
				return emitJSON(stdout, &resultEnvelope{Schema: resultSchemaV1, Command: cmdRecoveryResolve, Outcome: outcomeRefused, Project: project, Environment: envName, Message: err.Error() + " — nothing was changed"})
			}
			fmt.Fprintf(stderr, "✗ resolve aborted: %v — nothing was changed\n", err)
			return exitFailed
		}
		sentence = canonicalResolveSentence(envName, scope)
	} else {
		// Non-interactive confirmation: the sentence is still typed (or
		// passed via --confirm) — it IS the authorization.
		got := *confirm
		if got == "" {
			fmt.Fprintf(humanOut, "Type the sentence to confirm:\n  %s\n> ", sentence)
			line, rerr2 := bufio.NewReader(os.Stdin).ReadString('\n')
			if rerr2 != nil && line == "" {
				fmt.Fprintf(stderr, "\n✗ resolve aborted: confirmation could not be read (%v)\n", rerr2)
				return exitFailed
			}
			got = strings.TrimSpace(line)
		}
		if got != sentence {
			if jsonMode {
				return emitJSON(stdout, &resultEnvelope{Schema: resultSchemaV1, Command: cmdRecoveryResolve, Outcome: outcomeRefused, Project: project, Environment: envName, Message: fmt.Sprintf("confirmation does not match %q — nothing was changed", sentence)})
			}
			fmt.Fprintf(stderr, "✗ resolve aborted: confirmation does not match %q — nothing was changed\n", sentence)
			return exitFailed
		}
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
		ConfirmRecoveryID: scope.recoveryID,
		ConfirmAttemptID:  scope.attemptID,
	})
	if err != nil {
		if jsonMode {
			return emitJSON(stdout, mustEnvelope(resolveResult(rep, err, errors.Is(err, lifecycle.ErrResolveRefused))))
		}
		fmt.Fprintf(stderr, "✗ recovery resolve %s: %v\n", envName, err)
		fmt.Fprintln(stderr, "  The markers were NOT removed — the block is still in place.")
		fmt.Fprintln(stderr, "  Run `deployctl status` and follow its guidance.")
		return exitFailed
	}
	if env, code := resolveResult(rep, nil, false); jsonMode {
		return emitJSON(stdout, env)
	} else {
		_ = code
	}
	if rep.NothingToResolve {
		fmt.Fprintf(stdout, "✓ nothing to resolve — the environment is not blocked\n")
		return exitOK
	}
	fmt.Fprintf(stdout, "✓ authorized resolution recorded (history seq %d); removed", rep.HistorySeq)
	if rep.ResolvedRecoveryID != "" {
		fmt.Fprintf(stdout, " recovery %s", rep.ResolvedRecoveryID)
	}
	if rep.ResolvedAttemptID != "" {
		fmt.Fprintf(stdout, " attempt %s", rep.ResolvedAttemptID)
	}
	fmt.Fprintf(stdout, "\n")
	if rep.LeftRecoveryID != "" || rep.LeftAttemptID != "" {
		fmt.Fprintf(stdout, "  Still present (not named by this authorization):")
		if rep.LeftRecoveryID != "" {
			fmt.Fprintf(stdout, " recovery %s", rep.LeftRecoveryID)
		}
		if rep.LeftAttemptID != "" {
			fmt.Fprintf(stdout, " attempt %s", rep.LeftAttemptID)
		}
		fmt.Fprintf(stdout, "\n  The environment is still blocked — resolve the remaining marker\n  separately once inspected.\n")
		return exitOK
	}
	fmt.Fprintf(stdout, "  The block is lifted. Verify the target still behaves as expected, then\n  deploy or recover normally. Observed state was not modified.\n")
	return exitOK
}

func mustEnvelope(env *resultEnvelope, code int) *resultEnvelope {
	_ = code
	return env
}

func attemptIDOrEmpty(err error, m target.AttemptMarker) string {
	if err == nil {
		return m.AttemptID
	}
	return ""
}

func recoveryIDOrEmpty(err error, m target.RecoveryMarker) string {
	if err == nil {
		return m.RecoveryID
	}
	return ""
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
