package main

import (
	"context"
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
// configuration error, 3 = infrastructure failure (UNCERTAIN: the command
// may have executed consequential work; never blindly retry — run status).
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
	exitInfra  = 3
)

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
// the target handle. Credentials and host keys come from the environment
// variables the manifest names — never from Git.
func connect(ctx context.Context, mt *manifest.Target) (*target.Target, error) {
	spec := mt.Spec.Transport
	var tr transport.Transport
	switch spec.Type {
	case manifest.TransportLocal:
		tr = local.New()
	case manifest.TransportSSH:
		host := os.Getenv(spec.HostFrom)
		if host == "" {
			return nil, fmt.Errorf("ssh: %s is not set (the target manifest names the variable, the environment holds the value)", spec.HostFrom)
		}
		keyData, err := os.ReadFile(os.Getenv(spec.CredentialFrom))
		if err != nil {
			return nil, fmt.Errorf("ssh: read credential %s: %w", spec.CredentialFrom, err)
		}
		signer, err := ssh.ParsePrivateKey(keyData)
		if err != nil {
			return nil, fmt.Errorf("ssh: parse credential: %w", err)
		}
		hostKeyData, err := os.ReadFile(os.Getenv(spec.HostKeyFrom))
		if err != nil {
			return nil, fmt.Errorf("ssh: read pinned host key %s: %w", spec.HostKeyFrom, err)
		}
		hostKey, err := ssh.ParseHostKey(string(hostKeyData))
		if err != nil {
			return nil, fmt.Errorf("ssh: parse pinned host key: %w", err)
		}
		tr, err = ssh.New(ctx, ssh.Config{
			Host: host, Port: spec.EffectivePort(), User: spec.User,
			Signer: signer, HostKey: hostKey,
		})
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("target %q declares unsupported transport type %q", mt.Metadata.Name, spec.Type)
	}
	return target.FromManifest(tr, mt)
}

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
