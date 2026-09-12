package main

// The trust-split commands: prepare (repository authority, zero target
// credential) and deploy-prepared / rollback-prepared (target
// credential, zero source access). The artifact between them is
// specified in docs/prepared-artifact-v1.md and implemented in
// internal/prepared; the lifecycle engine consumes it unchanged.
//
// deploy-prepared has NO --repo-dir input and never touches Git: if the
// prepared material does not verify, the command refuses (exit 1)
// before any target contact.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/magtheo/deploy-toolkit/internal/lifecycle"
	"github.com/magtheo/deploy-toolkit/internal/prepared"
	"github.com/magtheo/deploy-toolkit/internal/target"
)

// invocationPrepared marks an invocation that ran from a verified
// prepared artifact rather than a repository checkout.
const invocationPrepared = "prepared"

// prepareResultData is the machine shape of a successful prepare. It
// names exactly what was bound into the artifact so the prepare job's
// log is auditable without parsing prose.
type prepareResultData struct {
	PreparedDir    string `json:"preparedDir"`
	ReleasePath    string `json:"releasePath"`
	BundleDigest   string `json:"bundleDigest,omitempty"`
	ContractDigest string `json:"contractDigest,omitempty"`
	SourceRevision string `json:"sourceRevision,omitempty"`
}

// prepareResult renders the prepare envelope. Prepare failures are
// usage/configuration (exit 2): the repository state, not the target,
// is the problem — nothing was contacted and nothing was written
// (filesystem write failures are infrastructure).
func prepareResult(m *prepared.Manifest, dir, releasePath string, err error) (*resultEnvelope, int) {
	env := &resultEnvelope{Schema: resultSchemaV1, Command: cmdPrepare}
	data := &prepareResultData{PreparedDir: dir, ReleasePath: releasePath}
	env.Data = data
	switch {
	case err != nil:
		if errors.Is(err, prepared.ErrNotVerifiable) || isRepoStateError(err) {
			env.Outcome = outcomeUsageError
			env.SafeToRetry = false
			env.Message = "prepare refused: " + err.Error()
			return env, exitUsage
		}
		env.Outcome = outcomeInfraFailed
		env.SafeToRetry = true
		env.Message = "prepare failed: " + err.Error()
		return env, exitInfra
	default:
		env.Project = m.Project
		env.Environment = m.Environment
		env.Outcome = outcomeSuccess
		env.Message = "prepared deployment artifact written and verified"
		data.BundleDigest = m.BundleDigest
		data.ContractDigest = m.ContractDigest
		data.SourceRevision = m.SourceRevision
		return env, exitOK
	}
}

// isRepoStateError reports whether err stems from the repository/
// checkout side (manifest loading, bundle building) rather than the
// filesystem.
func isRepoStateError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "load pinned release") ||
		strings.Contains(msg, "does not contain the release revision") ||
		strings.Contains(msg, "build bundle for") ||
		strings.Contains(msg, "manifest") ||
		strings.Contains(msg, "prepared material does not verify")
}

func runPrepare(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	lexFlags, positional, jsonMode, missingValue := lexArgs(args, map[string]bool{"repo-dir": true, "out": true})
	if missingValue != "" {
		return usageFailure(jsonMode, stdout, stderr, cmdPrepare, envNameOr(positional), "--"+missingValue+" requires a value")
	}
	if len(positional) == 0 {
		return usageFailure(jsonMode, stdout, stderr, cmdPrepare, "", "usage: deployctl prepare <environment> --repo-dir . --out <dir>")
	}
	if len(positional) > 1 {
		return usageFailure(jsonMode, stdout, stderr, cmdPrepare, positional[0], fmt.Sprintf("unexpected argument %q", positional[1]))
	}
	envName := positional[0]
	if strings.Contains(envName, "/") {
		return usageFailure(jsonMode, stdout, stderr, cmdPrepare, envName, "environment must be a bare name")
	}
	fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoDir := fs.String("repo-dir", ".", "local checkout containing the promoted release state")
	out := fs.String("out", "", "directory to write the prepared artifact into (required)")
	release := fs.String("release", "", "prepare this version instead of the release the environment pins (rollback material; the engine still validates the transition)")
	_ = fs.Bool("json", false, "emit a single deployctl.result/v1 JSON document on stdout")
	if err := fs.Parse(lexFlags); err != nil {
		return usageFailure(jsonMode, stdout, stderr, cmdPrepare, envName, "invalid flags: "+err.Error())
	}
	if *out == "" {
		return usageFailure(jsonMode, stdout, stderr, cmdPrepare, envName, "--out <dir> is required")
	}

	dc, err := loadDeploymentContext(*repoDir, envName)
	if err != nil {
		return emitPrepare(jsonMode, stdout, stderr, nil, *out, "", err)
	}
	// Strict pin by default: a deploy artifact must carry the release
	// the environment pins. --release relaxes ONLY this cross-check to
	// build rollback material; the rollback engine still validates the
	// transition endpoints itself.
	strictPin := *release == ""
	rel := dc.Release
	relPath := dc.Env.Spec.Release
	if !strictPin {
		if err := target.CheckVersion(*release); err != nil {
			return emitPrepare(jsonMode, stdout, stderr, nil, *out, "", fmt.Errorf("--release: %w", err))
		}
		if rel, err = dc.loadRelease(*release); err != nil {
			return emitPrepare(jsonMode, stdout, stderr, nil, *out, "", fmt.Errorf("load --release %s: %w", *release, err))
		}
		relPath = fmt.Sprintf(".deploy/releases/%s-%s.yaml", rel.Metadata.Project, rel.Metadata.Version)
	}
	bundleBytes, err := prepareBundle(ctx, dc.RepoDir, rel)
	if err != nil {
		return emitPrepare(jsonMode, stdout, stderr, nil, *out, relPath, err)
	}
	releaseBytes, err := os.ReadFile(filepath.Join(*repoDir, filepath.FromSlash(relPath)))
	if err != nil {
		return emitPrepare(jsonMode, stdout, stderr, nil, *out, relPath, fmt.Errorf("re-read release manifest bytes: %w", err))
	}
	envBytes, err := os.ReadFile(filepath.Join(*repoDir, ".deploy", "environments", envName+".yaml"))
	if err != nil {
		return emitPrepare(jsonMode, stdout, stderr, nil, *out, relPath, fmt.Errorf("re-read environment manifest bytes: %w", err))
	}
	targetBytes, err := os.ReadFile(filepath.Join(*repoDir, ".deploy", "targets", dc.Env.Spec.Target+".yaml"))
	if err != nil {
		return emitPrepare(jsonMode, stdout, stderr, nil, *out, relPath, fmt.Errorf("re-read target manifest bytes: %w", err))
	}
	m, err := prepared.Prepare(*out, releaseBytes, envBytes, targetBytes, bundleBytes, strictPin)
	return emitPrepare(jsonMode, stdout, stderr, m, *out, relPath, err)
}

func emitPrepare(jsonMode bool, stdout, stderr io.Writer, m *prepared.Manifest, dir, relPath string, err error) int {
	env, code := prepareResult(m, dir, relPath, err)
	if jsonMode {
		return emitJSON(stdout, env)
	}
	if err != nil {
		fmt.Fprintf(stderr, "✗ prepare: %v\n", err)
		return code
	}
	fmt.Fprintf(stdout, "✓ prepared %s@%s for %s/%s → %s\n", m.Project, m.Version, m.Environment, m.Target, dir)
	fmt.Fprintf(stdout, "  bundle %s\n", m.BundleDigest)
	return code
}

func runDeployPrepared(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	lexFlags, positional, jsonMode, missingValue := lexArgs(args, map[string]bool{"prepared": true, "environment": true, "owner": true})
	if missingValue != "" {
		return usageFailure(jsonMode, stdout, stderr, cmdDeploy, "", "--"+missingValue+" requires a value")
	}
	if len(positional) != 0 {
		return usageFailure(jsonMode, stdout, stderr, cmdDeploy, positional[0], fmt.Sprintf("unexpected argument %q (deploy-prepared takes no positional environment; the artifact names it)", positional[0]))
	}
	fs := flag.NewFlagSet("deploy-prepared", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("prepared", "", "prepared artifact directory (required)")
	expectEnv := fs.String("environment", "", "refuse unless the artifact was prepared for this environment")
	owner := fs.String("owner", "", "identity recorded in the lock and history (default user@host)")
	_ = fs.Bool("json", false, "emit a single deployctl.result/v1 JSON document on stdout")
	if err := fs.Parse(lexFlags); err != nil {
		return usageFailure(jsonMode, stdout, stderr, cmdDeploy, "", "invalid flags: "+err.Error())
	}
	if *dir == "" {
		return usageFailure(jsonMode, stdout, stderr, cmdDeploy, "", "--prepared <dir> is required")
	}

	// Verification happens BEFORE any target contact: a refusal here
	// never dialed anything, never staged anything, never executed
	// anything.
	art, err := prepared.LoadForDeploy(*dir)
	if err != nil {
		return reportVerificationFailure(jsonMode, stdout, stderr, cmdDeploy, *expectEnv, err)
	}
	if *expectEnv != "" && art.Manifest.Environment != *expectEnv {
		err := fmt.Errorf("artifact was prepared for environment %q, but the invocation expects %q", art.Manifest.Environment, *expectEnv)
		return reportVerificationFailure(jsonMode, stdout, stderr, cmdDeploy, *expectEnv, err)
	}
	tgt, err := connect(ctx, art.Target)
	if err != nil {
		return reportConnectFailure(err, cmdDeploy, art.Manifest.Environment, stderr, jsonMode, stdout)
	}
	ownerID := *owner
	if ownerID == "" {
		ownerID = defaultOwner()
	}
	rep, derr := lifecycle.Deploy(ctx, lifecycle.DeployInput{
		Target:         tgt,
		TargetManifest: art.Target,
		Environment:    art.Environment,
		Release:        art.Release,
		Bundle:         art.Bundle,
		Owner:          ownerID,
	})
	env, _ := deployResult(rep, derr)
	env.Data.(*deployResultData).Invocation = invocationPrepared
	if jsonMode {
		return emitJSON(stdout, env)
	}
	return reportDeploy(rep, derr, stdout, stderr)
}

func runRollbackPrepared(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	lexFlags, positional, jsonMode, missingValue := lexArgs(args, map[string]bool{"from": true, "to": true, "environment": true, "owner": true})
	if missingValue != "" {
		return usageFailure(jsonMode, stdout, stderr, cmdRollback, "", "--"+missingValue+" requires a value")
	}
	if len(positional) != 0 {
		return usageFailure(jsonMode, stdout, stderr, cmdRollback, positional[0], fmt.Sprintf("unexpected argument %q (rollback-prepared takes no positional environment; the artifacts name it)", positional[0]))
	}
	fs := flag.NewFlagSet("rollback-prepared", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "", "prepared artifact of the currently deployed (failed) release (required)")
	to := fs.String("to", "", "prepared artifact of the release being restored (required)")
	expectEnv := fs.String("environment", "", "refuse unless the artifacts were prepared for this environment")
	owner := fs.String("owner", "", "identity recorded in the lock and history (default user@host)")
	_ = fs.Bool("json", false, "emit a single deployctl.result/v1 JSON document on stdout")
	if err := fs.Parse(lexFlags); err != nil {
		return usageFailure(jsonMode, stdout, stderr, cmdRollback, "", "invalid flags: "+err.Error())
	}
	if *from == "" || *to == "" {
		return usageFailure(jsonMode, stdout, stderr, cmdRollback, "", "--from <dir> and --to <dir> are both required")
	}

	fromArt, err := prepared.Load(*from)
	if err != nil {
		return reportVerificationFailure(jsonMode, stdout, stderr, cmdRollback, *expectEnv, fmt.Errorf("--from: %w", err))
	}
	toArt, err := prepared.Load(*to)
	if err != nil {
		return reportVerificationFailure(jsonMode, stdout, stderr, cmdRollback, *expectEnv, fmt.Errorf("--to: %w", err))
	}
	// Both artifacts must describe the SAME environment on the SAME
	// target configuration; anything else is ambiguous material. (The
	// to-artifact legitimately carries an environment that pins the
	// failed release — that is what rollback material is.)
	if fromArt.Manifest.Environment != toArt.Manifest.Environment {
		err := fmt.Errorf("--from is for environment %q but --to is for %q", fromArt.Manifest.Environment, toArt.Manifest.Environment)
		return reportVerificationFailure(jsonMode, stdout, stderr, cmdRollback, *expectEnv, err)
	}
	if fromArt.Manifest.Digests.Target != toArt.Manifest.Digests.Target {
		err := errors.New("--from and --to carry different target configurations; the target descriptor is ambiguous")
		return reportVerificationFailure(jsonMode, stdout, stderr, cmdRollback, *expectEnv, err)
	}
	if *expectEnv != "" && fromArt.Manifest.Environment != *expectEnv {
		err := fmt.Errorf("artifacts were prepared for environment %q, but the invocation expects %q", fromArt.Manifest.Environment, *expectEnv)
		return reportVerificationFailure(jsonMode, stdout, stderr, cmdRollback, *expectEnv, err)
	}
	tgt, err := connect(ctx, fromArt.Target)
	if err != nil {
		return reportConnectFailure(err, cmdRollback, fromArt.Manifest.Environment, stderr, jsonMode, stdout)
	}
	ownerID := *owner
	if ownerID == "" {
		ownerID = defaultOwner()
	}
	rep, rerr := lifecycle.Rollback(ctx, lifecycle.RollbackInput{
		Target:         tgt,
		TargetManifest: fromArt.Target,
		Environment:    fromArt.Environment,
		FromRelease:    fromArt.Release,
		FromBundle:     fromArt.Bundle,
		ToRelease:      toArt.Release,
		ToBundle:       toArt.Bundle,
		Authorization:  lifecycle.RollbackManual,
		Owner:          ownerID,
	})
	env, _ := rollbackResult(rep, rerr)
	env.Data.(*rollbackResultData).Invocation = invocationPrepared
	if jsonMode {
		return emitJSON(stdout, env)
	}
	return reportRollback(rep, rerr, fromArt.Manifest.Environment, stdout, stderr)
}

// reportVerificationFailure renders the refusal for material that does
// not verify: refused, exit 1, nothing contacted.
func reportVerificationFailure(jsonMode bool, stdout, stderr io.Writer, cmd, envName string, err error) int {
	if jsonMode {
		env := &resultEnvelope{
			Schema:      resultSchemaV1,
			Command:     cmd,
			Outcome:     outcomeRefused,
			Environment: envName,
			Message:     "refused: " + err.Error(),
		}
		return emitJSON(stdout, env)
	}
	fmt.Fprintf(stderr, "✗ %s: refused: %v\n", cmd, err)
	fmt.Fprintln(stderr, "  The prepared material does not verify. Nothing was contacted and nothing")
	fmt.Fprintln(stderr, "  was executed. Do not retry with the same material; regenerate it on the")
	fmt.Fprintln(stderr, "  prepare side.")
	return exitFailed
}

// usageFailure renders a usage error on both surfaces.
func usageFailure(jsonMode bool, stdout, stderr io.Writer, cmd, envName, msg string) int {
	if jsonMode {
		return emitJSON(stdout, usageErrorResult(cmd, envName, msg))
	}
	fmt.Fprintf(stderr, "deployctl %s: %s\n", cmd, msg)
	return exitUsage
}
