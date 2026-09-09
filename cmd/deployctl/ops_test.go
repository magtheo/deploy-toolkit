package main

import (
	"bytes"
	"context"
	"fmt"
	"github.com/magtheo/deploy-toolkit/internal/bundle"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cliFixture is a full consumer repository: hooks, .deploy manifests and
// two release revisions. Everything the operational commands load or
// build is real — bundles are built from git revisions, manifests go
// through the parse pipeline, and deployments run over the local
// transport against a real deploy root.
type cliFixture struct {
	repoDir   string
	deployDir string // target deploy root
	marker    string // hook execution-order marker (outside the repo)
	revision  string // 1.0.0
	revision2 string // 2.0.0
}

func gitf(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String())
}

func newCLIFixture(t *testing.T) *cliFixture {
	t.Helper()
	repoDir := t.TempDir()
	deployDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "order.txt")

	write := func(rel, content string) {
		p := filepath.Join(repoDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	hook := func(name string) string {
		return fmt.Sprintf("#!/bin/sh\necho %s >> %s\n", name, marker)
	}
	write("deploy/preflight.sh", hook("preflight"))
	write("deploy/migrate.sh", hook("migrate"))
	write("deploy/apply.sh", hook("apply"))
	write("deploy/verify.sh", hook("verify"))
	write("deploy/rollback.sh", hook("rollback"))
	write(".deploy/project.yaml", `apiVersion: deploy.toolkit/v1
kind: Project
metadata:
  name: my-app
release:
  source:
    type: github
    repository: example/my-app
    branch: main
  requiredChecks:
    - ci/build
artifacts:
  app:
    type: oci
    repository: ghcr.io/example/app
bundle:
  include:
    - "deploy/**"
lifecycle:
  preflight:
    argv: ["./deploy/preflight.sh"]
  migrate:
    argv: ["./deploy/migrate.sh"]
  apply:
    argv: ["./deploy/apply.sh"]
  verify:
    argv: ["./deploy/verify.sh"]
  rollback:
    argv: ["./deploy/rollback.sh"]
`)
	gitf(t, repoDir, "init", "-q", "-b", "main")
	gitf(t, repoDir, "config", "user.email", "test@example.com")
	gitf(t, repoDir, "config", "user.name", "Test")
	gitf(t, repoDir, "add", "-A")
	gitf(t, repoDir, "commit", "-q", "-m", "first")
	rev1 := gitf(t, repoDir, "rev-parse", "HEAD")

	f := &cliFixture{repoDir: repoDir, deployDir: deployDir, marker: marker, revision: rev1}
	f.writeRelease(t, "1.0.0", rev1)
	f.writeEnv(t, "1.0.0")
	write(".deploy/targets/local-dev.yaml", fmt.Sprintf(`apiVersion: deploy.toolkit/v1
kind: Target
metadata:
  name: local-dev
spec:
  deployRoot: %s
  transport:
    type: local
`, deployDir))
	gitf(t, repoDir, "add", "-A")
	gitf(t, repoDir, "commit", "-q", "-m", "target")

	// Second genuine release identity: changed hook bytes, new revision.
	write("deploy/apply.sh", hook("apply")+"# release-2.0.0 variant\n")
	gitf(t, repoDir, "add", "-A")
	gitf(t, repoDir, "commit", "-q", "-m", "second")
	f.revision2 = gitf(t, repoDir, "rev-parse", "HEAD")
	f.writeRelease(t, "2.0.0", f.revision2)
	gitf(t, repoDir, "add", "-A")
	gitf(t, repoDir, "commit", "-q", "-m", "release 2.0.0")
	return f
}

// writeRelease generates the release manifest the same way `release
// create` would: digests come from actually building the bundle at the
// revision.
func (f *cliFixture) writeRelease(t *testing.T, version, rev string) {
	t.Helper()
	res, err := bundle.NewBuilder(f.repoDir).BuildFromRevision(context.Background(), rev)
	if err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf(`apiVersion: deploy.toolkit/v1
kind: Release
metadata:
  project: my-app
  version: %s
source:
  type: github
  repository: example/my-app
  revision: "%s"
artifacts:
  app:
    type: oci
    image: ghcr.io/example/app
    digest: sha256:%s
bundle:
  digest: %s
deploymentContract:
  digest: %s
migration:
  head: "001"
  mode: forward-compatible
  rollbackSafe: true
`, version, rev, strings.Repeat("ab", 32), res.Digest, res.ContractDigest)
	p := filepath.Join(f.repoDir, ".deploy", "releases", fmt.Sprintf("my-app-%s.yaml", version))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *cliFixture) writeEnv(t *testing.T, version string) {
	t.Helper()
	p := filepath.Join(f.repoDir, ".deploy", "environments", "production.yaml")
	content := fmt.Sprintf(`apiVersion: deploy.toolkit/v1
kind: Environment
metadata:
  name: production
spec:
  release: .deploy/releases/my-app-%s.yaml
  target: local-dev
  failurePolicy:
    autoRollback: safe-only
`, version)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runCLI(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func orderCLI(t *testing.T, f *cliFixture) []string {
	t.Helper()
	raw, err := os.ReadFile(f.marker)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

func TestDeployCommandEndToEnd(t *testing.T) {
	f := newCLIFixture(t)

	code, out, errOut := runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test")
	if code != exitOK {
		t.Fatalf("deploy exit = %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "production is running 1.0.0") {
		t.Errorf("stdout =\n%s", out)
	}
	if got := strings.Join(orderCLI(t, f), ","); got != "preflight,migrate,apply,verify" {
		t.Errorf("hook order = %v", got)
	}

	// Status reflects the verified deployment, signed with its operation.
	code, out, _ = runCLI("status", "production", "--repo-dir", f.repoDir)
	if code != exitOK {
		t.Fatalf("status exit = %d", code)
	}
	for _, want := range []string{"HEALTHY", "Release     1.0.0", "Operation   deploy:"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}

	// Redeploying is already-current: reported, exit 0, no re-runs.
	before := orderCLI(t, f)
	code, out, _ = runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test")
	if code != exitOK || !strings.Contains(out, "already current") {
		t.Fatalf("redeploy exit = %d, stdout =\n%s", code, out)
	}
	if got := strings.Join(orderCLI(t, f), ","); got != strings.Join(before, ",") {
		t.Errorf("redeploy re-ran hooks: %v → %v", before, got)
	}
}

func TestRollbackCommandTypedConfirmation(t *testing.T) {
	f := newCLIFixture(t)
	if code, out, errOut := runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test"); code != exitOK {
		t.Fatalf("deploy 1.0.0: %d\n%s\n%s", code, out, errOut)
	}
	f.writeEnv(t, "2.0.0")
	if code, out, errOut := runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test"); code != exitOK {
		t.Fatalf("deploy 2.0.0: %d\n%s\n%s", code, out, errOut)
	}
	before := orderCLI(t, f)

	// A near-miss confirmation must abort before anything runs.
	code, _, errOut := runCLI("rollback", "production", "--to", "1.0.0", "--repo-dir", f.repoDir, "--confirm", "rollback production to 1.0.1")
	if code != exitFailed {
		t.Fatalf("wrong confirmation exit = %d, want %d", code, exitFailed)
	}
	if !strings.Contains(errOut, "nothing was executed") {
		t.Errorf("stderr =\n%s", errOut)
	}
	if got := strings.Join(orderCLI(t, f), ","); got != strings.Join(before, ",") {
		t.Errorf("aborted rollback executed hooks: %v → %v", before, got)
	}

	// The exact sentence authorizes the emergency recovery.
	code, out, errOut := runCLI("rollback", "production", "--to", "1.0.0", "--repo-dir", f.repoDir, "--confirm", "rollback production to 1.0.0", "--owner", "test")
	if code != exitOK {
		t.Fatalf("rollback exit = %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "production is running 1.0.0") || !strings.Contains(out, "rollback") {
		t.Errorf("stdout =\n%s", out)
	}
	got := orderCLI(t, f)
	tail := got[len(before):]
	if strings.Join(tail, ",") != "preflight,rollback,apply,verify" {
		t.Errorf("rollback hook execution = %v (from %v), want preflight,rollback,apply,verify", tail, before)
	}

	// Desired (2.0.0, still pinned) now differs from observed (1.0.0):
	// the documented temporary drift until the reconcile PR merges.
	code, out, _ = runCLI("status", "production", "--repo-dir", f.repoDir)
	if code != exitOK || !strings.Contains(out, "OUT OF DATE") {
		t.Fatalf("status after rollback exit = %d, want OUT OF DATE:\n%s", code, out)
	}
}

func TestDeployCommandReportsDeterminedFailure(t *testing.T) {
	f := newCLIFixture(t)
	// Break verify for 1.0.0 only: the hook exits 1 when the version
	// matches.
	p := filepath.Join(f.repoDir, "deploy/verify.sh")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, append(raw, []byte("exit 1\n")...), 0o755); err != nil {
		t.Fatal(err)
	}
	gitf(t, f.repoDir, "add", "-A")
	gitf(t, f.repoDir, "commit", "-q", "-m", "break verify")
	rev := gitf(t, f.repoDir, "rev-parse", "HEAD")
	f.revision = rev
	f.writeRelease(t, "1.0.0", rev)

	code, out, _ := runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test")
	if code != exitFailed {
		t.Fatalf("failing deploy exit = %d, want %d\nstdout:\n%s", code, exitFailed, out)
	}
	if !strings.Contains(out, "verify") || !strings.Contains(out, "deploy failed") {
		t.Errorf("stdout =\n%s", out)
	}

	// A retry is refused with the recovery-required explanation: the
	// attempt marker proves consequential work with an unknown outcome.
	code, out, _ = runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test")
	if code != exitFailed || !strings.Contains(out, "RECOVERY REQUIRED") {
		t.Fatalf("retry exit = %d, want RECOVERY REQUIRED refusal\nstdout:\n%s", code, out)
	}

	code, out, _ = runCLI("status", "production", "--repo-dir", f.repoDir)
	if code != exitOK || !strings.Contains(out, "RECOVERY REQUIRED") {
		t.Fatalf("status exit = %d, want recovery-required rendering:\n%s", code, out)
	}
	if !strings.Contains(out, "deployctl rollback production --to") {
		t.Errorf("status should point at the explicit recovery path:\n%s", out)
	}
}

func TestDeployCommandUsageErrors(t *testing.T) {
	f := newCLIFixture(t)
	if code, _, _ := runCLI("deploy"); code != exitUsage {
		t.Errorf("bare deploy exit = %d, want %d", code, exitUsage)
	}
	if code, _, errOut := runCLI("deploy", "a/b", "--repo-dir", f.repoDir); code != exitUsage {
		t.Errorf("nested environment exit = %d, want %d (%s)", code, exitUsage, errOut)
	}
	if code, _, errOut := runCLI("deploy", "missing", "--repo-dir", f.repoDir); code != exitUsage || !strings.Contains(errOut, "missing") {
		t.Errorf("unknown environment: code = %d, stderr = %s", code, errOut)
	}
	if code, _, errOut := runCLI("rollback", "production", "--repo-dir", f.repoDir, "--confirm", "x"); code != exitUsage || !strings.Contains(errOut, "--to") {
		t.Errorf("rollback without --to: code = %d, stderr = %s", code, errOut)
	}
}

// The CLI is a wrapper: keep the manifest pipeline honest end to end. A
// tampered release manifest (bundle digest that matches nothing) must be
// refused at prepare time, before any connection is made.
func TestDeployCommandRefusesTamperedRelease(t *testing.T) {
	f := newCLIFixture(t)
	p := filepath.Join(f.repoDir, ".deploy/releases/my-app-1.0.0.yaml")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// Flip the first two hex digits of the pinned bundle digest: the
	// manifest stays structurally valid (parse pipeline passes), but the
	// bytes no longer hash to the pin.
	needle := "bundle:\n  digest: sha256:"
	i := strings.Index(string(raw), needle)
	if i < 0 {
		t.Fatal("fixture bug: bundle digest line not found")
	}
	tampered := string(raw)[:i+len(needle)] + "ff" + string(raw)[i+len(needle)+2:]
	if err := os.WriteFile(p, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test")
	if code != exitUsage || !strings.Contains(errOut, "does not contain the release revision") && !strings.Contains(errOut, "hashes to") {
		t.Fatalf("tampered release: code = %d, stderr = %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(f.deployDir, "my-app")); !os.IsNotExist(err) {
		t.Error("a tampered release must not touch the target")
	}
}
