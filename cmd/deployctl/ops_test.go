package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"

	"github.com/magtheo/deploy-toolkit/internal/bundle"
	"github.com/magtheo/deploy-toolkit/internal/lifecycle"
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
	// The failed operation was the FIRST deployment (nothing verified
	// existed before it): the marker's origin is empty, so status must
	// NOT suggest rolling back to the release that failed.
	if strings.Contains(out, "deployctl rollback production --to") {
		t.Errorf("status suggested a rollback target for a first deployment:\n%s", out)
	}
	if !strings.Contains(out, "FIRST deployment") {
		t.Errorf("status should state that no restore target exists:\n%s", out)
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
	// Flip the first hex digit of the pinned bundle digest to a
	// GUARANTEED-different digit (a fixed replacement would be a no-op
	// when the genuine digest already starts with it): the manifest
	// stays structurally valid (parse pipeline passes), but the bytes
	// no longer hash to the pin.
	needle := "bundle:\n  digest: sha256:"
	i := strings.Index(string(raw), needle)
	if i < 0 {
		t.Fatal("fixture bug: bundle digest line not found")
	}
	repl := "0"
	if string(raw)[i+len(needle)] == '0' {
		repl = "1"
	}
	tampered := string(raw)[:i+len(needle)] + repl + string(raw)[i+len(needle)+1:]
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

// ---- fail-closed status evidence -------------------------------------

func (f *cliFixture) statePath() string {
	return filepath.Join(f.deployDir, "my-app", "state", "production.json")
}

func (f *cliFixture) attemptPath() string {
	return filepath.Join(f.deployDir, "my-app", "attempts", "production.json")
}

func (f *cliFixture) recoveryPath() string {
	return filepath.Join(f.deployDir, "my-app", "recoveries", "production.json")
}

func (f *cliFixture) lockPath() string {
	return filepath.Join(f.deployDir, "my-app", ".locks", "production")
}

func writeFileCLIF(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const fakeDigest = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// A valid observed-state document may carry "current": null — "nothing
// observed deployed". Status must render that, not panic.
func TestStatusHandlesEmptyCurrentState(t *testing.T) {
	f := newCLIFixture(t)
	writeFileCLIF(t, f.statePath(), `{"schema":"toolkit.state/v1","project":"my-app","environment":"production","current":null,"updatedAt":"2026-09-09T00:00:00Z"}`)
	code, out, errOut := runCLI("status", "production", "--repo-dir", f.repoDir)
	if code != exitOK {
		t.Fatalf("status exit = %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "NOT DEPLOYED") || !strings.Contains(out, "records no current deployment") {
		t.Errorf("stdout =\n%s", out)
	}
}

// Corrupt evidence must degrade the whole report: a status that says
// HEALTHY while the attempt marker is unreadable is exactly backwards.
func TestStatusFailsClosedOnCorruptMarkers(t *testing.T) {
	f := newCLIFixture(t)
	if code, out, errOut := runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test"); code != exitOK {
		t.Fatalf("deploy: %d\n%s\n%s", code, out, errOut)
	}

	for _, tc := range []struct{ name, path, content string }{
		{"attempt", f.attemptPath(), "{not json"},
		{"recovery", f.recoveryPath(), `{"schema":"wrong`},
	} {
		writeFileCLIF(t, tc.path, tc.content)
		code, out, _ := runCLI("status", "production", "--repo-dir", f.repoDir)
		if code != exitOK {
			t.Fatalf("%s: status exit = %d", tc.name, code)
		}
		if !strings.Contains(out, "DEGRADED EVIDENCE") {
			t.Errorf("%s marker corrupt: stdout =\n%s", tc.name, out)
		}
		if strings.Contains(out, "State         HEALTHY") {
			t.Errorf("%s marker corrupt: state line claims HEALTHY:\n%s", tc.name, out)
		}
		if !strings.Contains(out, "UNREADABLE") {
			t.Errorf("%s marker corrupt: unreadable fact not named:\n%s", tc.name, out)
		}
		os.Remove(tc.path)
	}
}

// A held lock dominates marker guidance: during an active rollback the
// lock AND the recovery marker legitimately coexist, and the only safe
// instruction is "wait, touch nothing".
func TestStatusLockDominatesMarkers(t *testing.T) {
	f := newCLIFixture(t)
	if code, out, errOut := runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test"); code != exitOK {
		t.Fatalf("deploy: %d\n%s\n%s", code, out, errOut)
	}
	writeFileCLIF(t, f.recoveryPath(), fmt.Sprintf(`{"schema":"toolkit.recovery/v1","recoveryId":"0123456789abcdef","sourceAttemptId":"","project":"my-app","environment":"production","fromRelease":"1.0.0","fromBundleDigest":%q,"toRelease":"2.0.0","toBundleDigest":%q,"authorization":"manual","startedAt":"2026-09-09T00:00:00Z"}`, fakeDigest, fakeDigest))
	writeFileCLIF(t, f.lockPath(), "")
	code, out, _ := runCLI("status", "production", "--repo-dir", f.repoDir)
	if code != exitOK {
		t.Fatalf("status exit = %d", code)
	}
	if !strings.Contains(out, "State         LOCKED") {
		t.Errorf("state line =\n%s", out)
	}
	if strings.Contains(out, "RECOVERY REQUIRED") {
		t.Errorf("an active operation was reported as needing recovery:\n%s", out)
	}
	if !strings.Contains(out, "Wait for the active operation") {
		t.Errorf("missing wait guidance:\n%s", out)
	}
}

// Rollback guidance comes from the attempt marker's own origin. An empty
// origin is a first deployment: there is nothing to restore, and the
// guidance must say so instead of pointing at the failed release.
func TestStatusFirstDeploymentAttemptHasNoRestoreTarget(t *testing.T) {
	f := newCLIFixture(t)
	writeFileCLIF(t, f.attemptPath(), fmt.Sprintf(`{"schema":"toolkit.attempt/v1","attemptId":"0123456789abcdef","project":"my-app","environment":"production","fromRelease":"","toRelease":"1.0.0","bundleDigest":%q,"startedAt":"2026-09-09T00:00:00Z"}`, fakeDigest))
	code, out, _ := runCLI("status", "production", "--repo-dir", f.repoDir)
	if code != exitOK {
		t.Fatalf("status exit = %d", code)
	}
	if !strings.Contains(out, "RECOVERY REQUIRED") || !strings.Contains(out, "FIRST deployment") {
		t.Errorf("stdout =\n%s", out)
	}
	if strings.Contains(out, "deployctl rollback") {
		t.Errorf("status suggested rollback for a first deployment:\n%s", out)
	}

	// With a real origin, the suggestion names the attempt's origin, not
	// the desired release.
	writeFileCLIF(t, f.attemptPath(), fmt.Sprintf(`{"schema":"toolkit.attempt/v1","attemptId":"0123456789abcdef","project":"my-app","environment":"production","fromRelease":"0.9.0","toRelease":"1.0.0","bundleDigest":%q,"startedAt":"2026-09-09T00:00:00Z"}`, fakeDigest))
	code, out, _ = runCLI("status", "production", "--repo-dir", f.repoDir)
	if code != exitOK || !strings.Contains(out, "--to 0.9.0") {
		t.Errorf("origin-derived guidance missing (--to 0.9.0):\n%s", out)
	}
}

// ---- SSH configuration and connection classification ------------------

// useSSHTransport repoints the environment's target at an SSH transport:
// host 127.0.0.1 port 1 (connection refused), with real generated key
// material unless overridden. Missing variables are simulated with an
// empty value.
func (f *cliFixture) useSSHTransport(t *testing.T, keyPEM, hostKeyLine string) {
	t.Helper()
	dir := t.TempDir()
	writeTarget := func() {
		writeFileCLIF(t, filepath.Join(f.repoDir, ".deploy", "targets", "local-dev.yaml"), fmt.Sprintf(`apiVersion: deploy.toolkit/v1
kind: Target
metadata:
  name: local-dev
spec:
  deployRoot: %s
  transport:
    type: ssh
    hostFrom: DEPLOY_TEST_HOST
    port: 1
    user: deploy
    hostKeyFrom: DEPLOY_TEST_HOSTKEY
    credentialFrom: DEPLOY_TEST_KEY
`, f.deployDir))
	}
	writeTarget()
	keyPath := filepath.Join(dir, "key.pem")
	hostKeyPath := filepath.Join(dir, "hostkey")
	if keyPEM != "" {
		writeFileCLIF(t, keyPath, keyPEM)
	}
	if hostKeyLine != "" {
		writeFileCLIF(t, hostKeyPath, hostKeyLine)
	}
	t.Setenv("DEPLOY_TEST_HOST", "127.0.0.1")
	t.Setenv("DEPLOY_TEST_KEY", keyPath)
	t.Setenv("DEPLOY_TEST_HOSTKEY", hostKeyPath)
}

func validSSHMaterial(t *testing.T) (string, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := gossh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	pub, err := gossh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block)), string(gossh.MarshalAuthorizedKey(pub))
}

// Missing or unparseable credential configuration is a configuration
// error (exit 2), never an infrastructure failure, and nothing runs.
func TestDeploySSHConfigurationErrors(t *testing.T) {
	validKey, validHostKey := validSSHMaterial(t)
	tests := []struct {
		name       string
		key        string
		hostKey    string
		wantStderr string
	}{
		{"missing host variable", "", "", ""},
		{"missing key file", "", validHostKey, "read credential file"},
		{"malformed private key", "not a key", validHostKey, "parseable private key"},
		{"malformed host key", validKey, "nope", "parseable host key"},
	}
	// The first case needs an UNSET host variable; t.Setenv cannot
	// unset, so it is handled inside the loop via a dedicated fixture.
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIFixture(t)
			f.useSSHTransport(t, tc.key, tc.hostKey)
			if i == 0 {
				os.Setenv("DEPLOY_TEST_HOST", "")
			}
			markerBefore := len(orderCLI(t, f))
			code, _, errOut := runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test")
			if code != exitUsage {
				t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, exitUsage, errOut)
			}
			if tc.wantStderr != "" && !strings.Contains(errOut, tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, errOut)
			}
			if i == 0 && !strings.Contains(errOut, "DEPLOY_TEST_HOST is not set") {
				t.Errorf("stderr =\n%s", errOut)
			}
			if !strings.Contains(errOut, "Nothing was started") {
				t.Errorf("pre-execution framing missing:\n%s", errOut)
			}
			if len(orderCLI(t, f)) != markerBefore {
				t.Error("hooks ran despite configuration failure")
			}
		})
	}
}

// An unreachable target is an infrastructure failure (exit 3) with
// explicit pre-execution framing: no lifecycle operation was executed.
func TestDeploySSHUnreachableTarget(t *testing.T) {
	f := newCLIFixture(t)
	keyPEM, hostKey := validSSHMaterial(t)
	f.useSSHTransport(t, keyPEM, hostKey)
	markerBefore := len(orderCLI(t, f))
	code, _, errOut := runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test")
	if code != exitInfra {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, exitInfra, errOut)
	}
	if !strings.Contains(errOut, "did not start") || !strings.Contains(errOut, "No lifecycle operation was executed") {
		t.Errorf("stderr =\n%s", errOut)
	}
	if len(orderCLI(t, f)) != markerBefore {
		t.Error("hooks ran despite connection failure")
	}
}

// The confirmation gate precedes target contact: with the target
// unreachable, a WRONG confirmation still aborts as an authorization
// failure (exit 1) — proving the connection was never attempted.
func TestRollbackConfirmationPrecedesConnection(t *testing.T) {
	f := newCLIFixture(t)
	keyPEM, hostKey := validSSHMaterial(t)
	f.useSSHTransport(t, keyPEM, hostKey)
	code, _, errOut := runCLI("rollback", "production", "--to", "1.0.0", "--repo-dir", f.repoDir, "--confirm", "rollback production to 9.9.9")
	if code != exitFailed {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, exitFailed, errOut)
	}
	if !strings.Contains(errOut, "nothing was executed, the target was not contacted") {
		t.Errorf("stderr =\n%s", errOut)
	}
}

// ---- infrastructure classification comes from the engine's own
// durable-boundary fact, never from re-reading the target ---------------

func reportDeployOut(rep *lifecycle.Report) string {
	var buf bytes.Buffer
	reportDeploy(rep, errors.New("ssh connection died"), &buf, &buf)
	return buf.String()
}

func TestDeployInfraClassification(t *testing.T) {
	tests := []struct {
		name    string
		rep     *lifecycle.Report
		want    []string
		notWant []string
	}{
		{
			name:    "boundary crossed — uncertain, never safe to rerun",
			rep:     &lifecycle.Report{Project: "my-app", Environment: "production", ConsequentialStarted: true, AttemptID: "0123456789abcdef"},
			want:    []string{"UNCERTAIN", "0123456789abcdef", "Do not retry"},
			notWant: []string{"can simply", "No consequential work"},
		},
		{
			name:    "pre-boundary — rerun after repair is safe",
			rep:     &lifecycle.Report{Project: "my-app", Environment: "production"},
			want:    []string{"No consequential work was executed"},
			notWant: []string{"UNCERTAIN"},
		},
		{
			name:    "committed — bookkeeping, not uncertain",
			rep:     &lifecycle.Report{Project: "my-app", Environment: "production", Committed: true},
			want:    []string{"Post-commit bookkeeping failed"},
			notWant: []string{"UNCERTAIN"},
		},
		{
			name:    "nil report — nothing started",
			rep:     nil,
			want:    []string{"No consequential work was executed"},
			notWant: []string{"UNCERTAIN"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := reportDeployOut(tc.rep)
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q:\n%s", w, out)
				}
			}
			for _, n := range tc.notWant {
				if strings.Contains(out, n) {
					t.Errorf("output must not contain %q:\n%s", n, out)
				}
			}
		})
	}
}

func TestRollbackInfraClassification(t *testing.T) {
	var buf bytes.Buffer
	reportRollback(&lifecycle.RollbackReport{Project: "my-app", Environment: "production", RecoveryStarted: true, RecoveryID: "0123456789abcdef"}, errors.New("ssh connection died"), "production", &buf, &buf)
	out := buf.String()
	for _, w := range []string{"UNCERTAIN", "0123456789abcdef", "Do not retry"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "can simply") {
		t.Errorf("uncertain outcome must not suggest rerunning:\n%s", out)
	}

	buf.Reset()
	reportRollback(&lifecycle.RollbackReport{AlreadyRecovered: true}, errors.New("cleanup failed"), "production", &buf, &buf)
	if out = buf.String(); !strings.Contains(out, "bookkeeping failed") || strings.Contains(out, "UNCERTAIN") {
		t.Errorf("already-recovered cleanup failure misclassified:\n%s", out)
	}
}

// ---- status guidance must obey the classification precedence ----------

func writeAttemptMarker(t *testing.T, f *cliFixture, from string) {
	t.Helper()
	writeFileCLIF(t, f.attemptPath(), fmt.Sprintf(`{"schema":"toolkit.attempt/v1","attemptId":"0123456789abcdef","project":"my-app","environment":"production","fromRelease":%q,"toRelease":"2.0.0","bundleDigest":%q,"startedAt":"2026-09-09T00:00:00Z"}`, from, fakeDigest))
}

func writeRecoveryMarkerCLIF(t *testing.T, f *cliFixture) {
	t.Helper()
	writeFileCLIF(t, f.recoveryPath(), fmt.Sprintf(`{"schema":"toolkit.recovery/v1","recoveryId":"0123456789abcdef","sourceAttemptId":"","project":"my-app","environment":"production","fromRelease":"2.0.0","fromBundleDigest":%q,"toRelease":"1.0.0","toBundleDigest":%q,"authorization":"manual","startedAt":"2026-09-09T00:00:00Z"}`, fakeDigest, fakeDigest))
}

// In every combination where a higher-priority fact outranks the attempt
// marker, no rollback command may be printed: an operator following the
// FIRST instruction status emits must never touch recovery state.
func TestStatusGuidanceObeyesClassificationPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, f *cliFixture)
		want    string
		notWant string
	}{
		{
			name: "lock + attempt",
			setup: func(t *testing.T, f *cliFixture) {
				writeAttemptMarker(t, f, "1.0.0")
				writeFileCLIF(t, f.lockPath(), "")
			},
			want:    "Wait for the active operation",
			notWant: "deployctl rollback",
		},
		{
			name: "recovery + attempt",
			setup: func(t *testing.T, f *cliFixture) {
				writeAttemptMarker(t, f, "1.0.0")
				writeRecoveryMarkerCLIF(t, f)
			},
			want:    "Normal deployment AND recovery are blocked",
			notWant: "deployctl rollback",
		},
		{
			name: "degraded + attempt",
			setup: func(t *testing.T, f *cliFixture) {
				writeAttemptMarker(t, f, "1.0.0")
				writeFileCLIF(t, f.recoveryPath(), "{corrupt")
			},
			want:    "EVIDENCE UNREADABLE",
			notWant: "deployctl rollback",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIFixture(t)
			tc.setup(t, f)
			code, out, _ := runCLI("status", "production", "--repo-dir", f.repoDir)
			if code != exitOK {
				t.Fatalf("status exit = %d", code)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output missing %q:\n%s", tc.want, out)
			}
			if strings.Contains(out, tc.notWant) {
				t.Errorf("output must not contain %q:\n%s", tc.notWant, out)
			}
		})
	}
}
