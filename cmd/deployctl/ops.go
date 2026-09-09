package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/magtheo/deploy-toolkit/internal/bundle"
	"github.com/magtheo/deploy-toolkit/internal/lifecycle"
	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/target"
	"github.com/magtheo/deploy-toolkit/internal/transport"
	"github.com/magtheo/deploy-toolkit/internal/transport/local"
	"github.com/magtheo/deploy-toolkit/internal/transport/ssh"
)

// Exit codes for the operational commands. 0 = success (including
// already-current / already-recovered), 1 = a reported outcome failure
// (determined: the history log records what happened), 2 = usage or
// configuration error, 3 = infrastructure failure.
//
// Exit 3 alone must never be read as "outcome uncertain": the command's
// REPORT distinguishes pre-execution failures (no lifecycle operation
// ran), bookkeeping failures after a committed state, and uncertain
// outcomes (an attempt/recovery marker is unresolved — RECOVERY
// REQUIRED). Automation keys on that reported recovery state, never on
// the exit code alone.
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
	exitInfra  = 3
)

// errTargetConfig marks a connect() failure that is local configuration —
// an unset variable, an unreadable or unparseable credential file — rather
// than a target-infrastructure problem. No network contact was attempted.
var errTargetConfig = errors.New("target configuration")

// deploymentContext is everything the operational commands load from the
// consumer repository: the environment (desired state), the release it
// pins and the target it runs on.
type deploymentContext struct {
	RepoDir string
	Env     *manifest.Environment
	Release *manifest.Release
	Target  *manifest.Target
}

func loadDeploymentContext(repoDir, envName string) (*deploymentContext, error) {
	envPath := filepath.Join(repoDir, ".deploy", "environments", envName+".yaml")
	env, err := manifest.LoadEnvironment(envPath)
	if err != nil {
		return nil, err
	}
	rel, err := manifest.LoadRelease(filepath.Join(repoDir, env.Spec.Release))
	if err != nil {
		return nil, fmt.Errorf("load pinned release: %w", err)
	}
	tgt, err := manifest.LoadTarget(filepath.Join(repoDir, ".deploy", "targets", env.Spec.Target+".yaml"))
	if err != nil {
		return nil, fmt.Errorf("load target %q: %w", env.Spec.Target, err)
	}
	return &deploymentContext{RepoDir: repoDir, Env: env, Release: rel, Target: tgt}, nil
}

// loadRelease loads an additional release manifest (rollback targets),
// pinned by version.
func (dc *deploymentContext) loadRelease(version string) (*manifest.Release, error) {
	ref := fmt.Sprintf(".deploy/releases/%s-%s.yaml", dc.Release.Metadata.Project, version)
	return manifest.LoadRelease(filepath.Join(dc.RepoDir, ref))
}

// connect builds the transport the Target manifest declares and returns
// the target handle.
//
// SSH environment-variable contract: the manifest names variables; the
// environment holds values.
//
//	hostFrom:       value is the hostname.
//	credentialFrom: value is a PATH to the SSH private-key file.
//	hostKeyFrom:    value is a PATH to a file holding the pinned host key
//	                (authorized_keys format). The target pins its host key;
//	                host-key prompts and StrictHostKeyChecking bypasses do
//	                not exist here.
//
// Credential material itself never lives in the manifest or in Git.
// Failures to assemble this configuration return an error wrapping
// errTargetConfig; only dial/handshake failures are infrastructure.
func connect(ctx context.Context, mt *manifest.Target) (*target.Target, error) {
	spec := mt.Spec.Transport
	var tr transport.Transport
	switch spec.Type {
	case manifest.TransportLocal:
		tr = local.New()
	case manifest.TransportSSH:
		host := os.Getenv(spec.HostFrom)
		if host == "" {
			return nil, fmt.Errorf("ssh: %s is not set (the target manifest names the variable, the environment holds the value): %w", spec.HostFrom, errTargetConfig)
		}
		keyPath := os.Getenv(spec.CredentialFrom)
		if keyPath == "" {
			return nil, fmt.Errorf("ssh: %s is not set (names the private-key file path): %w", spec.CredentialFrom, errTargetConfig)
		}
		keyData, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("ssh: read credential file (%s, from %s): %w (%w)", keyPath, spec.CredentialFrom, err, errTargetConfig)
		}
		signer, err := ssh.ParsePrivateKey(keyData)
		if err != nil {
			return nil, fmt.Errorf("ssh: %s does not contain a parseable private key: %w (%w)", keyPath, err, errTargetConfig)
		}
		hostKeyPath := os.Getenv(spec.HostKeyFrom)
		if hostKeyPath == "" {
			return nil, fmt.Errorf("ssh: %s is not set (names the pinned host-key file path): %w", spec.HostKeyFrom, errTargetConfig)
		}
		hostKeyData, err := os.ReadFile(hostKeyPath)
		if err != nil {
			return nil, fmt.Errorf("ssh: read pinned host key file (%s, from %s): %w (%w)", hostKeyPath, spec.HostKeyFrom, err, errTargetConfig)
		}
		hostKey, err := ssh.ParseHostKey(string(hostKeyData))
		if err != nil {
			return nil, fmt.Errorf("ssh: %s does not contain a parseable host key: %w (%w)", hostKeyPath, err, errTargetConfig)
		}
		tr, err = ssh.New(ctx, ssh.Config{
			Host: host, Port: spec.EffectivePort(), User: spec.User,
			Signer: signer, HostKey: hostKey,
		})
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("target %q declares unsupported transport type %q: %w", mt.Metadata.Name, spec.Type, errTargetConfig)
	}
	return target.FromManifest(tr, mt)
}

// reportConnectFailure renders a failure that happened before any
// lifecycle operation: nothing was executed and nothing is unresolved.
func reportConnectFailure(err error, cmd, envName string, stderr io.Writer, jsonMode bool, stdout io.Writer) int {
	display := strings.Replace(cmd, "-", " ", 1)
	if jsonMode {
		outcome := outcomeInfraFailed
		if errors.Is(err, errTargetConfig) {
			outcome = outcomeUsageError
		}
		env := &resultEnvelope{
			Schema: resultSchemaV1, Command: cmd, Outcome: outcome,
			Environment: envName, SafeToRetry: !errors.Is(err, errTargetConfig),
			Message: "the operation did not start: " + err.Error(),
		}
		return emitJSON(stdout, env)
	}
	if errors.Is(err, errTargetConfig) {
		fmt.Fprintf(stderr, "✗ %s %s: configuration error: %v\n", display, envName, err)
		fmt.Fprintln(stderr, "  Nothing was started; no lifecycle operation was executed.")
		return exitUsage
	}
	fmt.Fprintf(stderr, "✗ %s %s: infrastructure failure: %v\n", display, envName, err)
	fmt.Fprintln(stderr, "  The operation did not start: the target could not be reached.")
	fmt.Fprintln(stderr, "  No lifecycle operation was executed; nothing is unresolved.")
	return exitInfra
}

// hasUnresolvedMarkers has been removed deliberately: after an
// infrastructure failure the markers must be read over the same
// transport that just failed (or with the same context that was
// cancelled), and a read error is not absence. The engine reports the
// durable boundary itself — Report.ConsequentialStarted /
// RollbackReport.RecoveryStarted — set immediately after the marker
// write succeeds. Never infer the boundary by probing the target after
// the fact.

// prepareBundle is the prepare side at the command line: build the
// canonical bundle for a release's pinned revision from a local checkout
// and refuse early if the bytes do not hash to the release's pin (the
// engine re-verifies — this is a friendlier error at the layer that can
// fix it: a stale checkout).
func prepareBundle(ctx context.Context, repoDir string, rel *manifest.Release) ([]byte, error) {
	res, err := bundle.NewBuilder(repoDir).BuildFromRevision(ctx, rel.Source.Revision)
	if err != nil {
		return nil, fmt.Errorf("build bundle for %s from %s (is the revision present in --repo-dir?): %w", rel.Metadata.Version, shortSHA(rel.Source.Revision), err)
	}
	if res.Digest != rel.Bundle.Digest {
		return nil, fmt.Errorf("locally built bundle for %s hashes to %s but the release pins %s — the checkout does not contain the release revision", rel.Metadata.Version, res.Digest, rel.Bundle.Digest)
	}
	return res.Bytes, nil
}

func defaultOwner() string {
	user := os.Getenv("USER")
	host, _ := os.Hostname()
	switch {
	case user != "" && host != "":
		return user + "@" + host
	case user != "":
		return user
	default:
		return host
	}
}

// renderStage prints one lifecycle stage outcome the way history records
// it: exit codes for hook outcomes, no manufactured code for
// infrastructure errors.
func renderStage(stdout io.Writer, s lifecycle.StageResult) {
	switch {
	case s.Skipped:
		fmt.Fprintf(stdout, "  %-10s — skipped\n", s.Name)
	case s.InfraError:
		fmt.Fprintf(stdout, "  %-10s ⚠ infrastructure error (no exit code — the command may have executed)\n", s.Name)
	case s.Failed:
		fmt.Fprintf(stdout, "  %-10s ✗ exit %d\n", s.Name, s.ExitCode)
	default:
		fmt.Fprintf(stdout, "  %-10s ✓\n", s.Name)
	}
}

func renderStageDetails(stdout io.Writer, s lifecycle.StageResult) {
	if s.InfraError || (!s.Failed && !s.Skipped) {
		return
	}
	if len(strings.TrimSpace(string(s.Stderr))) > 0 {
		fmt.Fprintf(stdout, "    stderr: %s\n", firstLines(string(s.Stderr), 8))
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " | ")
}
