package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
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
	// The first failure already declares recovery required: the attempt
	// marker survives a post-consequential determined failure.
	if !strings.Contains(out, "RECOVERY REQUIRED") {
		t.Errorf("first failure must declare RECOVERY REQUIRED:\n%s", out)
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
	os.MkdirAll(f.lockPath(), 0o755)
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
	f.useSSHTransportAt(t, keyPEM, hostKeyLine, "127.0.0.1", 1)
}

// useSSHTransportAt is useSSHTransport with an explicit host and port,
// for tests that stand up a live in-process SSH server.
func (f *cliFixture) useSSHTransportAt(t *testing.T, keyPEM, hostKeyLine, host string, port int) {
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
    port: %d
    user: deploy
    hostKeyFrom: DEPLOY_TEST_HOSTKEY
    credentialFrom: DEPLOY_TEST_KEY
`, f.deployDir, port))
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
	t.Setenv("DEPLOY_TEST_HOST", host)
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
				os.MkdirAll(f.lockPath(), 0o755)
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

// A determined rollback-hook failure keeps the recovery marker: the CLI
// must declare RECOVERY REQUIRED on the first failure, and status must
// classify the environment accordingly.
func TestRollbackCommandDeterminedFailureRequiresRecovery(t *testing.T) {
	f := newCLIFixture(t)
	if code, out, errOut := runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test"); code != exitOK {
		t.Fatalf("deploy 1.0.0: %d\n%s\n%s", code, out, errOut)
	}
	// Break the rollback hook BEFORE creating the 2.0.0 release, so the
	// failing hook is part of 2.0.0's immutable contract.
	p := filepath.Join(f.repoDir, "deploy/rollback.sh")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, append(raw, []byte("exit 1\n")...), 0o755); err != nil {
		t.Fatal(err)
	}
	gitf(t, f.repoDir, "add", "-A")
	gitf(t, f.repoDir, "commit", "-q", "-m", "break rollback hook")
	rev := gitf(t, f.repoDir, "rev-parse", "HEAD")
	f.revision2 = rev
	f.writeRelease(t, "2.0.0", rev)
	f.writeEnv(t, "2.0.0")
	if code, out, errOut := runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test"); code != exitOK {
		t.Fatalf("deploy 2.0.0: %d\n%s\n%s", code, out, errOut)
	}

	code, out, _ := runCLI("rollback", "production", "--to", "1.0.0", "--repo-dir", f.repoDir, "--confirm", "rollback production to 1.0.0", "--owner", "test")
	if code != exitFailed {
		t.Fatalf("rollback exit = %d, want %d\nstdout:\n%s", code, exitFailed, out)
	}
	if !strings.Contains(out, "rollback") || !strings.Contains(out, "failed") {
		t.Errorf("stdout =\n%s", out)
	}
	if !strings.Contains(out, "RECOVERY REQUIRED") {
		t.Errorf("first rollback failure must declare RECOVERY REQUIRED:\n%s", out)
	}

	code, out, _ = runCLI("status", "production", "--repo-dir", f.repoDir)
	if code != exitOK {
		t.Fatalf("status exit = %d", code)
	}
	if !strings.Contains(out, "RECOVERY REQUIRED") || !strings.Contains(out, "Recovery      UNRESOLVED") {
		t.Errorf("status must classify the failed recovery:\n%s", out)
	}
}

// The full recovery-resolve story at the CLI: a failed deploy blocks the
// environment; the operator resolves it with a typed confirmation naming
// the exact marker id; the block lifts; normal deployment resumes.
func TestRecoveryResolveCommand(t *testing.T) {
	f := newCLIFixture(t)
	// Make verify fail, regenerate the release at the broken revision.
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
	f.writeRelease(t, "1.0.0", rev)
	if code, out, _ := runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test"); code != exitFailed {
		t.Fatalf("failing deploy exit = %d\n%s", code, out)
	}
	if _, err := os.Stat(f.attemptPath()); err != nil {
		t.Fatal("fixture bug: expected the attempt marker")
	}

	// Unreadable observed state must be repaired, not resolved.
	writeFileCLIF(t, f.statePath(), "{corrupt")
	code, _, errOut := runCLI("recovery", "resolve", "production", "--repo-dir", f.repoDir, "--confirm", "resolve production attempt deadbeefdeadbeef")
	if code != exitFailed || !strings.Contains(errOut, "repaired, not resolved") {
		t.Fatalf("corrupt state: exit = %d, stderr =\n%s", code, errOut)
	}
	os.Remove(f.statePath())

	// A sentence naming a marker that is not there: refused by the
	// engine's identity binding, nothing changed.
	code, _, errOut = runCLI("recovery", "resolve", "production", "--repo-dir", f.repoDir, "--confirm", "resolve production attempt deadbeefdeadbeef")
	if code != exitFailed || !strings.Contains(errOut, "NOT removed") {
		t.Fatalf("absent-marker confirmation: exit = %d, stderr =\n%s", code, errOut)
	}
	if _, err := os.Stat(f.attemptPath()); err != nil {
		t.Fatal("a refused resolution must not remove the marker")
	}

	// The real sentence names the marker id that is actually there.
	markerRaw, err := os.ReadFile(f.attemptPath())
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		AttemptID string `json:"attemptId"`
	}
	if err := json.Unmarshal(markerRaw, &m); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI("recovery", "resolve", "production", "--repo-dir", f.repoDir,
		"attempt", m.AttemptID, "--confirm", "resolve production attempt "+m.AttemptID, "--owner", "operator")
	if code != exitOK {
		t.Fatalf("resolve exit = %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.Contains(out, m.AttemptID) || !strings.Contains(out, "block is lifted") {
		t.Errorf("stdout =\n%s", out)
	}
	if _, err := os.Stat(f.attemptPath()); !os.IsNotExist(err) {
		t.Error("the marker must be gone after resolution")
	}

	// Resolving again immediately is a no-op.
	code, out, _ = runCLI("recovery", "resolve", "production", "--repo-dir", f.repoDir,
		"attempt", m.AttemptID)
	if code != exitOK || !strings.Contains(out, "Nothing to resolve") {
		t.Errorf("second resolve should be a no-op: exit = %d, stdout =\n%s", code, out)
	}

	// Normal operation resumes: the retry deploys again and fails at
	// verify — as a normal determined failure, not a refusal.
	code, out, _ = runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test")
	if code != exitFailed || !strings.Contains(out, "RECOVERY REQUIRED") {
		t.Fatalf("post-resolve deploy exit = %d, stdout =\n%s", code, out)
	}
}

// The authorized marker set must come from what the operator SELECTED,
// not from what happens to exist: attempt-only, recovery-only and joint
// scopes are all reachable from the CLI, and a partial authorization
// leaves the other marker blocking — visibly.
func TestRecoveryResolvePerMarkerScopes(t *testing.T) {
	f := newCLIFixture(t)
	writeAttemptMarker(t, f, "1.0.0")
	// A recovery LINKED to that attempt (the consistent pair).
	writeFileCLIF(t, f.recoveryPath(), fmt.Sprintf(`{"schema":"toolkit.recovery/v1","recoveryId":"0123456789abcdef","sourceAttemptId":"fedcba9876543210","project":"my-app","environment":"production","fromRelease":"2.0.0","fromBundleDigest":%q,"toRelease":"1.0.0","toBundleDigest":%q,"authorization":"manual","startedAt":"2026-09-09T00:00:00Z"}`, fakeDigest, fakeDigest))
	// Make the attempt id match the linked pair.
	writeAttemptMarker(t, f, "1.0.0")
	raw, err := os.ReadFile(f.attemptPath())
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"attemptId":"0123456789abcdef"`), []byte(`"attemptId":"fedcba9876543210"`), 1)
	writeFileCLIF(t, f.attemptPath(), string(raw))
	attID, recID := "fedcba9876543210", "0123456789abcdef"

	// Grammar errors are usage errors.
	code, _, errOut := runCLI("recovery", "resolve", "production", "--repo-dir", f.repoDir, "recovery")
	if code != exitUsage {
		t.Fatalf("dangling selector: exit = %d, stderr = %s", code, errOut)
	}
	code, _, errOut = runCLI("recovery", "resolve", "production", "--repo-dir", f.repoDir, "recovery", recID, "--confirm", "resolve production recovery deadbeefdeadbeef")
	if code != exitFailed || !strings.Contains(errOut, "does not match the selected authorization") {
		t.Fatalf("confirm/selector mismatch: exit = %d, stderr = %s", code, errOut)
	}

	// Attempt-only: the recovery marker stays and still blocks — said
	// out loud, not silently.
	code, out, errOut := runCLI("recovery", "resolve", "production", "--repo-dir", f.repoDir,
		"attempt", attID, "--confirm", "resolve production attempt "+attID, "--owner", "operator")
	if code != exitOK {
		t.Fatalf("attempt-only resolve exit = %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "Still present") || !strings.Contains(out, recID) {
		t.Errorf("partial authorization must report what it left:\\n%s", out)
	}
	if _, err := os.Stat(f.recoveryPath()); err != nil {
		t.Fatal("the recovery marker must survive an attempt-only authorization")
	}
	if _, err := os.Stat(f.attemptPath()); !os.IsNotExist(err) {
		t.Error("the attempt marker must be gone")
	}

	// Recovery-only completes the job.
	code, out, _ = runCLI("recovery", "resolve", "production", "--repo-dir", f.repoDir,
		"recovery", recID, "--confirm", "resolve production recovery "+recID, "--owner", "operator")
	if code != exitOK || !strings.Contains(out, "block is lifted") {
		t.Fatalf("recovery-only resolve: exit = %d, stdout =\\n%s", code, out)
	}
	if _, err := os.Stat(f.recoveryPath()); !os.IsNotExist(err) {
		t.Error("the recovery marker must be gone")
	}

	// Joint scope on a fresh inconsistent pair is refused by the engine.
	f2 := newCLIFixture(t)
	writeFileCLIF(t, f2.recoveryPath(), fmt.Sprintf(`{"schema":"toolkit.recovery/v1","recoveryId":"0123456789abcdef","sourceAttemptId":"","project":"my-app","environment":"production","fromRelease":"2.0.0","fromBundleDigest":%q,"toRelease":"1.0.0","toBundleDigest":%q,"authorization":"manual","startedAt":"2026-09-09T00:00:00Z"}`, fakeDigest, fakeDigest))
	writeAttemptMarker(t, f2, "1.0.0")
	code, _, errOut = runCLI("recovery", "resolve", "production", "--repo-dir", f2.repoDir,
		"recovery", recID, "attempt", "0123456789abcdef",
		"--confirm", "resolve production recovery "+recID+" attempt 0123456789abcdef")
	if code != exitFailed || !strings.Contains(errOut, "inconsistent evidence") {
		t.Fatalf("joint inconsistent authorization: exit = %d, stderr =\\n%s", code, errOut)
	}
}

// ---- the machine contract: deployctl.result/v1 ------------------------

func runJSON(t *testing.T, args ...string) (int, map[string]any) {
	t.Helper()
	args = append(args, "--json")
	code, out, errOut := runCLI(args...)
	if !json.Valid([]byte(out)) {
		t.Fatalf("--json stdout is not a single JSON document: exit=%d out=%q stderr=%q", code, out, errOut)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["schema"] != "deployctl.result/v1" {
		t.Errorf("schema = %v", doc["schema"])
	}
	return code, doc
}

func mustStr(t *testing.T, doc map[string]any, key, want string) {
	t.Helper()
	if got, _ := doc[key].(string); got != want {
		t.Errorf("%s = %v, want %q", key, doc[key], want)
	}
}

func TestJSONDeploySuccess(t *testing.T) {
	f := newCLIFixture(t)
	code, doc := runJSON(t, "deploy", "production", "--repo-dir", f.repoDir, "--owner", "test")
	if code != exitOK {
		t.Fatalf("exit = %d: %v", code, doc["message"])
	}
	mustStr(t, doc, "outcome", "success")
	mustStr(t, doc, "project", "my-app")
	mustStr(t, doc, "environment", "production")
	if doc["recoveryRequired"] != false || doc["safeToRetry"] != false {
		t.Errorf("flags = %v/%v", doc["recoveryRequired"], doc["safeToRetry"])
	}
	data := doc["data"].(map[string]any)
	if data["committed"] != true || data["consequentialStarted"] != true {
		t.Errorf("data = %v", data)
	}
	if stages, _ := data["stages"].([]any); len(stages) != 4 {
		t.Errorf("stages = %v", data["stages"])
	}

	// Already-current is a deliberate no-op with outcome success.
	code, doc = runJSON(t, "deploy", "production", "--repo-dir", f.repoDir, "--owner", "test")
	if code != exitOK || doc["outcome"] != "success" {
		t.Fatalf("redeploy: %d %v", code, doc["outcome"])
	}
	if doc["data"].(map[string]any)["alreadyCurrent"] != true {
		t.Errorf("data = %v", doc["data"])
	}
}

func TestJSONDeployDeterminedFailureIsRecoveryRequired(t *testing.T) {
	f := newCLIFixture(t)
	p := filepath.Join(f.repoDir, "deploy/verify.sh")
	raw, _ := os.ReadFile(p)
	os.WriteFile(p, append(raw, []byte("exit 1\n")...), 0o755)
	gitf(t, f.repoDir, "add", "-A")
	gitf(t, f.repoDir, "commit", "-q", "-m", "break verify")
	rev := gitf(t, f.repoDir, "rev-parse", "HEAD")
	f.writeRelease(t, "1.0.0", rev)

	code, doc := runJSON(t, "deploy", "production", "--repo-dir", f.repoDir, "--owner", "test")
	if code != exitFailed {
		t.Fatalf("exit = %d", code)
	}
	mustStr(t, doc, "outcome", "failure")
	if doc["recoveryRequired"] != true || doc["safeToRetry"] != false {
		t.Errorf("flags = %v/%v", doc["recoveryRequired"], doc["safeToRetry"])
	}
	data := doc["data"].(map[string]any)
	if data["consequentialStarted"] != true || data["attemptId"] == "" {
		t.Errorf("data = %v", data)
	}
}

func TestJSONStatusShapes(t *testing.T) {
	f := newCLIFixture(t)
	if code, out, _ := runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test"); code != exitOK {
		t.Fatalf("deploy: %d %s", code, out)
	}
	code, doc := runJSON(t, "status", "production", "--repo-dir", f.repoDir)
	if code != exitOK || doc["outcome"] != "success" {
		t.Fatalf("status: %d %v", code, doc)
	}
	data := doc["data"].(map[string]any)
	if data["state"] != "healthy" || data["lock"] != "free" {
		t.Errorf("data = %v", data)
	}
	if doc["recoveryRequired"] != false {
		t.Errorf("recoveryRequired = %v", doc["recoveryRequired"])
	}
	obs := data["observed"].(map[string]any)
	if obs["version"] != "1.0.0" || obs["operationId"] == "" {
		t.Errorf("observed = %v", obs)
	}

	// Corrupt evidence → degraded, with the unreadable fact named.
	writeFileCLIF(t, f.attemptPath(), "{corrupt")
	_, doc = runJSON(t, "status", "production", "--repo-dir", f.repoDir)
	data = doc["data"].(map[string]any)
	if data["state"] != "degraded" || doc["recoveryRequired"] != false {
		t.Errorf("data = %v", data)
	}
	if doc["outcome"] != "success" {
		t.Errorf("status reporting degraded evidence is still a successful report: %v", doc["outcome"])
	}
	os.Remove(f.attemptPath())

	// Unresolved attempt → recovery-required.
	writeAttemptMarker(t, f, "1.0.0")
	_, doc = runJSON(t, "status", "production", "--repo-dir", f.repoDir)
	data = doc["data"].(map[string]any)
	if data["state"] != "recovery-required" || doc["recoveryRequired"] != true {
		t.Errorf("data = %v recoveryRequired = %v", data, doc["recoveryRequired"])
	}
	if m := data["attempt"].(map[string]any); m["present"] != true {
		t.Errorf("attempt = %v", m)
	}
}

func TestJSONRollbackRefusalAndSuccess(t *testing.T) {
	f := newCLIFixture(t)
	if code, out, errOut := runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test"); code != exitOK {
		t.Fatalf("deploy: %d\n%s\n%s", code, out, errOut)
	}
	f.writeEnv(t, "2.0.0")
	if code, out, errOut := runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test"); code != exitOK {
		t.Fatalf("deploy 2.0.0: %d\n%s\n%s", code, out, errOut)
	}

	// A near-miss confirmation is a refused outcome, exit 1.
	code, doc := runJSON(t, "rollback", "production", "--to", "1.0.0", "--repo-dir", f.repoDir, "--confirm", "rollback production to 9.9.9", "--owner", "test")
	if code != exitFailed {
		t.Fatalf("exit = %d", code)
	}
	mustStr(t, doc, "outcome", "refused")

	code, doc = runJSON(t, "rollback", "production", "--to", "1.0.0", "--repo-dir", f.repoDir, "--confirm", "rollback production to 1.0.0", "--owner", "test")
	if code != exitOK || doc["outcome"] != "success" {
		t.Fatalf("rollback: %d %v", code, doc["outcome"])
	}
	data := doc["data"].(map[string]any)
	if data["committed"] != true || data["fromVersion"] != "2.0.0" || data["toVersion"] != "1.0.0" || data["recoveryStarted"] != true {
		t.Errorf("data = %v", data)
	}
}

func TestJSONResolveScopes(t *testing.T) {
	f := newCLIFixture(t)
	// Blocked by a handcrafted pair.
	writeAttemptMarker(t, f, "1.0.0")
	writeFileCLIF(t, f.attemptPath(), strings.Replace(string(mustRead(t, f.attemptPath())), "0123456789abcdef", "fedcba9876543210", 1))
	raw := fmt.Sprintf(`{"schema":"toolkit.recovery/v1","recoveryId":"0123456789abcdef","sourceAttemptId":"fedcba9876543210","project":"my-app","environment":"production","fromRelease":"2.0.0","fromBundleDigest":%q,"toRelease":"1.0.0","toBundleDigest":%q,"authorization":"manual","startedAt":"2026-09-09T00:00:00Z"}`, fakeDigest, fakeDigest)
	writeFileCLIF(t, f.recoveryPath(), raw)

	// Partial: attempt-only, the recovery remains and still blocks.
	code, doc := runJSON(t, "recovery", "resolve", "production", "--repo-dir", f.repoDir,
		"attempt", "fedcba9876543210", "--confirm", "resolve production attempt fedcba9876543210", "--owner", "operator")
	if code != exitOK || doc["outcome"] != "success" {
		t.Fatalf("partial: %d %v", code, doc["outcome"])
	}
	if doc["recoveryRequired"] != true {
		t.Errorf("a partial resolution leaves the environment blocked: %v", doc["recoveryRequired"])
	}
	data := doc["data"].(map[string]any)
	if data["resolvedAttemptId"] != "fedcba9876543210" || data["leftRecoveryId"] != "0123456789abcdef" {
		t.Errorf("data = %v", data)
	}

	// Complete the resolution.
	code, doc = runJSON(t, "recovery", "resolve", "production", "--repo-dir", f.repoDir,
		"recovery", "0123456789abcdef", "--confirm", "resolve production recovery 0123456789abcdef", "--owner", "operator")
	if code != exitOK || doc["recoveryRequired"] != false {
		t.Fatalf("complete: %d %v %v", code, doc["outcome"], doc["recoveryRequired"])
	}

	// Nothing left: idempotent no-op.
	code, doc = runJSON(t, "recovery", "resolve", "production", "--repo-dir", f.repoDir,
		"recovery", "0123456789abcdef", "--confirm", "resolve production recovery 0123456789abcdef")
	if code != exitOK || doc["outcome"] != "success" || doc["data"].(map[string]any)["nothingToResolve"] != true {
		t.Fatalf("no-op: %d %v", code, doc)
	}
}

func TestJSONUsageErrorIsJSONToo(t *testing.T) {
	f := newCLIFixture(t)
	code, doc := runJSON(t, "deploy")
	if code != exitUsage || doc["outcome"] != "usage-error" {
		t.Fatalf("exit = %d outcome = %v", code, doc["outcome"])
	}
	// Grammar errors are usage errors even when the target is unblocked.
	code, doc = runJSON(t, "recovery", "resolve", "production", "--repo-dir", f.repoDir, "nonsense", "garbage")
	if code != exitUsage || doc["outcome"] != "usage-error" {
		t.Fatalf("grammar-before-target: exit = %d outcome = %v", code, doc["outcome"])
	}
}

func TestJSONUnreachableTargetIsPreExecutionInfra(t *testing.T) {
	f := newCLIFixture(t)
	keyPEM, hostKey := validSSHMaterial(t)
	f.useSSHTransport(t, keyPEM, hostKey)
	code, doc := runJSON(t, "deploy", "production", "--repo-dir", f.repoDir, "--owner", "test")
	if code != exitInfra {
		t.Fatalf("exit = %d", code)
	}
	mustStr(t, doc, "outcome", "infrastructure-failure")
	if doc["safeToRetry"] != true {
		t.Errorf("a pre-execution failure is safe to retry after repair: %v", doc["safeToRetry"])
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Negative JSON contract: every error path emits exactly one valid
// deployctl.result/v1 document — usage errors, confirmation refusals,
// held locks, corrupt evidence — never prose on stdout.
func TestJSONErrorPathsAreAlwaysJSON(t *testing.T) {
	f := newCLIFixture(t)

	tests := []struct {
		name    string
		args    []string
		code    int
		outcome string
	}{
		{"deploy unknown flag", []string{"deploy", "production", "--repo-dir", f.repoDir, "--bogus"}, exitUsage, "usage-error"},
		{"rollback missing --to", []string{"rollback", "production", "--repo-dir", f.repoDir}, exitUsage, "usage-error"},
		{"rollback bad semver", []string{"rollback", "production", "--to", "NOT.A.VERSION", "--repo-dir", f.repoDir, "--confirm", "x"}, exitUsage, "usage-error"},
		{"rollback json is non-interactive", []string{"rollback", "production", "--to", "1.0.0", "--repo-dir", f.repoDir}, exitUsage, "usage-error"},
		{"resolve json is non-interactive", []string{"recovery", "resolve", "production", "--repo-dir", f.repoDir}, exitUsage, "usage-error"},
		{"resolve selectors do not replace confirmation", []string{"recovery", "resolve", "production", "--repo-dir", f.repoDir, "attempt", "0123456789abcdef"}, exitUsage, "usage-error"},
		{"resolve unknown flag after confirm", []string{"recovery", "resolve", "production", "--repo-dir", f.repoDir, "attempt", "0123456789abcdef", "--confirm", "resolve production attempt 0123456789abcdef", "--bogus"}, exitUsage, "usage-error"},

		{"resolve grammar before target", []string{"recovery", "resolve", "production", "--repo-dir", f.repoDir, "nonsense", "garbage"}, exitUsage, "usage-error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, doc := runJSON(t, tc.args...)
			if code != tc.code || doc["outcome"] != tc.outcome {
				t.Fatalf("exit = %d outcome = %v, want %d/%s", code, doc["outcome"], tc.code, tc.outcome)
			}
		})
	}

	// A value flag with NO value is a different fs.Parse failure
	// ("flag needs an argument"). It must be the LAST token, so this
	// case drives runCLI directly — runJSON appends --json, which a
	// trailing value flag would otherwise legally consume as its value.
	code, out, _ := runCLI("recovery", "resolve", "production", "--repo-dir", f.repoDir,
		"attempt", "0123456789abcdef",
		"--confirm", "resolve production attempt 0123456789abcdef",
		"--json", "--owner")
	if code != exitUsage {
		t.Errorf("missing flag value: exit = %d, want %d", code, exitUsage)
	}
	if !json.Valid([]byte(out)) {
		t.Errorf("missing flag value: stdout is not one JSON document: %q", out)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["outcome"] != "usage-error" || doc["command"] != "recovery-resolve" {
		t.Errorf("missing flag value: outcome = %v command = %v", doc["outcome"], doc["command"])
	}

	// Corrupt evidence: refused, and the block is stated.
	writeFileCLIF(t, f.statePath(), "{corrupt")
	code, doc = runJSON(t, "recovery", "resolve", "production", "--repo-dir", f.repoDir,
		"attempt", "0123456789abcdef", "--confirm", "resolve production attempt 0123456789abcdef")
	if code != exitFailed || doc["outcome"] != "refused" {
		t.Fatalf("corrupt state: exit = %d outcome = %v", code, doc["outcome"])
	}

	// Held lock: refused, environment named.
	os.Remove(f.statePath())
	writeAttemptMarker(t, f, "1.0.0")
	os.MkdirAll(f.lockPath(), 0o755)
	code, doc = runJSON(t, "recovery", "resolve", "production", "--repo-dir", f.repoDir,
		"attempt", "0123456789abcdef", "--confirm", "resolve production attempt 0123456789abcdef")
	if code != exitFailed || doc["outcome"] != "refused" {
		t.Fatalf("held lock: exit = %d outcome = %v", code, doc["outcome"])
	}
	data := doc["data"].(map[string]any)
	if data["remainingAttemptId"] != "0123456789abcdef" {
		t.Errorf("held-lock refusal must name the remaining block: %v", data)
	}
}

// NOTE: dead-transport classification is covered by
// TestJSONResolveDeadProbeIsInfrastructure (CLI end-to-end: a probe that
// cannot run is exit 3 infrastructure-failure, never refusal or absence)
// and by TestUnknownIsNeverAbsence in internal/target (every read class
// refuses to interpret unknown as absence). The substrate no longer
// shells out for existence, so per-command kill-the-exec tests cannot
// target individual reads — and no longer need to: all read classes
// share one proven-absence primitive and one failure classification.

// The blocked-state facts in resolve JSON come from the engine's
// post-operation presence flags — not from the read-time marker ids:
//
//	full success            → NO remaining ids, recoveryRequired false
//	half-cleared crash      → ONLY the surviving recovery id
//	partial authorization   → remaining names the deliberate leftover
//
// These assert the exact JSON fields, so a stale id cannot survive CI.
func TestResolveResultJSONBlockedFacts(t *testing.T) {
	assertData := func(t *testing.T, env *resultEnvelope, want map[string]any) {
		t.Helper()
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		data, _ := doc["data"].(map[string]any)
		for k, want := range want {
			got, ok := data[k]
			if want == nil {
				if ok && got != "" && got != false && got != float64(0) {
					t.Errorf("data.%s = %v, want absent", k, got)
				}
				continue
			}
			if got != want {
				t.Errorf("data.%s = %v, want %v", k, got, want)
			}
		}
	}

	t.Run("full success exposes no remaining ids", func(t *testing.T) {
		env, code := resolveResult(&lifecycle.ResolveReport{
			Project: "my-app", Environment: "production",
			ResolvedAttemptID: "0123456789abcdef", HistorySeq: 3,
			AttemptPresentAfter: false, RecoveryPresentAfter: false,
			AttemptMarkerID: "0123456789abcdef", RecoveryMarkerID: "fedcba9876543210",
		}, nil, false)
		if code != exitOK || env.Outcome != outcomeSuccess || env.RecoveryRequired {
			t.Fatalf("outcome = %s recoveryRequired = %v code = %d", env.Outcome, env.RecoveryRequired, code)
		}
		assertData(t, env, map[string]any{
			"remainingAttemptId": nil, "remainingRecoveryId": nil,
			"resolvedAttemptId": "0123456789abcdef",
		})
	})

	t.Run("half-cleared crash exposes only the surviving recovery id", func(t *testing.T) {
		env, code := resolveResult(&lifecycle.ResolveReport{
			Project: "my-app", Environment: "production",
			ResolvedAttemptID:   "0123456789abcdef",
			AttemptPresentAfter: false, RecoveryPresentAfter: true,
			AttemptMarkerID: "0123456789abcdef", RecoveryMarkerID: "fedcba9876543210",
			RecoveryRequired: true,
		}, errors.New("clear recovery marker: transport died"), false)
		if code != exitInfra || env.Outcome != outcomeInfraFailed || !env.RecoveryRequired {
			t.Fatalf("outcome = %s recoveryRequired = %v code = %d", env.Outcome, env.RecoveryRequired, code)
		}
		assertData(t, env, map[string]any{
			"remainingRecoveryId": "fedcba9876543210",
			"remainingAttemptId":  nil,
			"resolvedAttemptId":   "0123456789abcdef",
		})
	})

	t.Run("partial authorization names the deliberate leftover", func(t *testing.T) {
		env, code := resolveResult(&lifecycle.ResolveReport{
			Project: "my-app", Environment: "production",
			ResolvedRecoveryID: "fedcba9876543210", LeftAttemptID: "0123456789abcdef",
			AttemptPresentAfter: true, RecoveryPresentAfter: false,
			AttemptMarkerID: "0123456789abcdef", RecoveryMarkerID: "fedcba9876543210",
		}, nil, false)
		if code != exitOK || env.Outcome != outcomeSuccess {
			t.Fatalf("outcome = %s code = %d", env.Outcome, code)
		}
		assertData(t, env, map[string]any{
			"remainingAttemptId":  "0123456789abcdef",
			"leftAttemptId":       "0123456789abcdef",
			"remainingRecoveryId": nil,
		})
	})
}

// A bare --json can never be consumed as a value while simultaneously
// selecting machine mode. The shared lexer owns both flag arity and
// JSON detection: a flag-shaped token after a value flag is a missing
// value (usage-error, exit 2), in BOTH orders — and nothing is
// executed, so the audit identity is never written as "--json".
func TestJSONNeverConsumableAsValue(t *testing.T) {
	t.Run("deploy --owner --json executes nothing", func(t *testing.T) {
		f := newCLIFixture(t)
		before := len(orderCLI(t, f))
		code, doc := runJSON(t, "deploy", "production", "--repo-dir", f.repoDir, "--owner", "--json")
		if code != exitUsage || doc["outcome"] != "usage-error" {
			t.Fatalf("exit = %d outcome = %v, want 2/usage-error", code, doc["outcome"])
		}
		if doc["command"] != "deploy" || doc["environment"] != "production" {
			t.Errorf("command = %v environment = %v", doc["command"], doc["environment"])
		}
		if len(orderCLI(t, f)) != before {
			t.Error("hooks ran despite the missing flag value")
		}
		if f.attemptExists() {
			t.Error("an attempt marker was written despite the missing flag value")
		}
	})

	t.Run("resolve --owner --json authorizes nothing", func(t *testing.T) {
		f := newCLIFixture(t)
		writeAttemptMarker(t, f, "1.0.0")
		code, doc := runJSON(t, "recovery", "resolve", "production", "--repo-dir", f.repoDir,
			"attempt", "0123456789abcdef",
			"--confirm", "resolve production attempt 0123456789abcdef",
			"--owner", "--json")
		if code != exitUsage || doc["outcome"] != "usage-error" {
			t.Fatalf("exit = %d outcome = %v, want 2/usage-error", code, doc["outcome"])
		}
		if !f.attemptExists() {
			t.Error("the attempt marker was resolved despite the missing flag value")
		}
	})

	t.Run("--json --owner is the same missing value", func(t *testing.T) {
		f := newCLIFixture(t)
		code, doc := runJSON(t, "deploy", "production", "--repo-dir", f.repoDir, "--json", "--owner")
		if code != exitUsage || doc["outcome"] != "usage-error" {
			t.Fatalf("exit = %d outcome = %v, want 2/usage-error", code, doc["outcome"])
		}
	})
}

func (f *cliFixture) attemptExists() bool {
	_, err := os.Stat(f.attemptPath())
	return err == nil
}

// The recovery-resolve prologue must lex the ENTIRE argv exactly once —
// the same single-pass invariant as the other operational commands. An
// empty argv must be a usage error (never a panic on args[1:]), and
// --json in the first position must still select machine mode.
func TestResolveSinglePassLexing(t *testing.T) {
	t.Run("bare invocation is a usage error, not a panic", func(t *testing.T) {
		f := newCLIFixture(t)
		code, _, errOut := runCLI("recovery", "resolve")
		if code != exitUsage {
			t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, exitUsage, errOut)
		}
		if strings.Contains(errOut, "panic") {
			t.Errorf("panicked:\n%s", errOut)
		}
		if len(orderCLI(t, f)) != 0 || f.attemptExists() {
			t.Error("something executed despite the usage error")
		}
	})

	t.Run("--json in first position still selects machine mode", func(t *testing.T) {
		newCLIFixture(t)
		code, out, errOut := runCLI("recovery", "resolve", "--json")
		if code != exitUsage {
			t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitUsage, out, errOut)
		}
		if !json.Valid([]byte(out)) {
			t.Fatalf("--json stdout is not a single JSON document: out=%q stderr=%q", out, errOut)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatal(err)
		}
		if doc["schema"] != "deployctl.result/v1" || doc["command"] != "recovery-resolve" || doc["outcome"] != "usage-error" {
			t.Errorf("document = %v", doc)
		}
	})

	t.Run("machine mode before the environment preserves interleaving", func(t *testing.T) {
		f := newCLIFixture(t)
		writeAttemptMarker(t, f, "1.0.0")
		code, doc := runJSON(t, "recovery", "resolve", "--json", "production", "attempt", "0123456789abcdef",
			"--repo-dir", f.repoDir, "--confirm", "resolve production attempt 0123456789abcdef")
		if code != exitOK || doc["outcome"] != "success" {
			t.Fatalf("exit = %d outcome = %v (%s), want interleaved parse to resolve", code, doc["outcome"], doc["message"])
		}
		if f.attemptExists() {
			t.Error("the marker was not resolved")
		}
	})
}

// A genuinely held environment lock is a REFUSAL per the frozen
// deployctl.result/v1 contract — never safe-to-retry infrastructure.
// These regressions drive the engine sentinel through both commands and
// pin the JSON shape and the human classification.
func TestHeldLockIsRefusalNotInfra(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"deploy", []string{"deploy", "production", "--repo-dir", "%DIR%", "--owner", "test"}},
		{"rollback", []string{"rollback", "production", "--to", "2.0.0", "--repo-dir", "%DIR%", "--confirm", "rollback production to 2.0.0", "--owner", "test"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIFixture(t)
			if err := os.MkdirAll(f.lockPath(), 0o755); err != nil {
				t.Fatal(err)
			}
			before := len(orderCLI(t, f))

			args := make([]string, len(tc.args))
			for i, a := range tc.args {
				args[i] = strings.ReplaceAll(a, "%DIR%", f.repoDir)
			}
			code, doc := runJSON(t, args...)
			if code != exitFailed {
				t.Fatalf("exit = %d, want %d (%s)", code, exitFailed, doc["message"])
			}
			if doc["outcome"] != "refused" || doc["recoveryRequired"] != false || doc["safeToRetry"] != false {
				t.Errorf("shape = %v %v %v, want refused/false/false", doc["outcome"], doc["recoveryRequired"], doc["safeToRetry"])
			}
			if doc["command"] != tc.name {
				t.Errorf("command = %v", doc["command"])
			}
			if len(orderCLI(t, f)) != before {
				t.Error("hooks ran despite a held lock")
			}
			if f.attemptExists() {
				t.Error("an attempt marker was written despite a held lock")
			}

			// Human mode: refused framing, never "simply rerun".
			hArgs := append([]string{}, args...)
			code, _, errOut := runCLI(hArgs...)
			if code != exitFailed {
				t.Fatalf("human exit = %d, want %d", code, exitFailed)
			}
			if !strings.Contains(errOut, "refused") || !strings.Contains(errOut, "may currently be executing") {
				t.Errorf("human stderr lacks refused framing:\n%s", errOut)
			}
			if strings.Contains(errOut, "simply") || strings.Contains(errOut, "infrastructure failure") {
				t.Errorf("human stderr misclassifies a refusal:\n%s", errOut)
			}
		})
	}
}

// Transport death during the substrate probe is infrastructure, never
// absence: the CLI must report infrastructure-failure (exit 3) — a dead
// probe must never let a mid-recovery environment present itself as a
// fresh target.
func TestJSONResolveDeadProbeIsInfrastructure(t *testing.T) {
	f := newCLIFixture(t)
	writeAttemptMarker(t, f, "1.0.0")
	keyPEM, hostKeyLine := validSSHMaterial(t)
	key, _, _, _, err := gossh.ParseAuthorizedKey([]byte(hostKeyLine))
	if err != nil {
		t.Fatal(err)
	}
	srv := startFakeSSHOpts(t, key, "", true) // sftp rejected: probes cannot run
	host, port := srv.hostPort()
	f.useSSHTransportAt(t, keyPEM, srv.hostKey(), host, port)
	code, doc := runJSON(t, "recovery", "resolve", "production", "--repo-dir", f.repoDir,
		"attempt", "0123456789abcdef", "--confirm", "resolve production attempt 0123456789abcdef")
	if code != exitInfra || doc["outcome"] != "infrastructure-failure" {
		t.Fatalf("exit = %d outcome = %v (%s), want 3/infrastructure-failure", code, doc["outcome"], doc["message"])
	}
	if !f.attemptExists() {
		t.Error("the marker must survive a dead probe")
	}
}

// status may emit not-deployed only from PROVEN absence; a broken
// hierarchy renders as degraded evidence claiming nothing.
func TestStatusBrokenHierarchyIsDegradedNotNotDeployed(t *testing.T) {
	f := newCLIFixture(t)
	// "state" exists as a regular FILE where the hierarchy needs a
	// directory — the exact shape the old `test -e` misreported as a
	// fresh target.
	// The INTERMEDIATE "state" component is a regular file where the
	// hierarchy requires a directory — the exact shape `test -e`
	// misreported as a fresh target (its exit 1 conflated ENOTDIR with
	// ENOENT). The probe must classify it as unknown, and status must
	// degrade rather than claim not-deployed.
	intermediate := filepath.Dir(f.statePath()) // .../my-app/state
	if err := os.MkdirAll(filepath.Dir(intermediate), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(intermediate, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, doc := runJSON(t, "status", "production", "--repo-dir", f.repoDir)
	if code != exitOK {
		t.Fatalf("a degraded status report is still a successful report: exit = %d", code)
	}
	data, _ := doc["data"].(map[string]any)
	if data["state"] != "degraded" {
		t.Fatalf("state = %v, want degraded", data["state"])
	}
	if data["state"] == "not-deployed" {
		t.Errorf("a broken hierarchy must never present as a fresh target")
	}
	if data["observed"] != nil {
		t.Errorf("degraded status must claim no observed state: %v", data["observed"])
	}
}

// A regular FILE at the lock path is broken evidence: lock "unreadable",
// overall state "degraded" — never "free" (which would green-light
// operations) and never "held" (only a real lock DIRECTORY is held,
// matching AcquireEnvLock).
func TestLockPathFileIsUnreadableNotFreeOrHeld(t *testing.T) {
	f := newCLIFixture(t)
	writeFileCLIF(t, f.lockPath(), "not a lock")
	code, doc := runJSON(t, "status", "production", "--repo-dir", f.repoDir)
	if code != exitOK {
		t.Fatalf("exit = %d, want a successful degraded report", code)
	}
	data, _ := doc["data"].(map[string]any)
	lock, _ := data["lock"].(string)
	if lock != "unreadable" {
		t.Errorf("lock = %q, want unreadable", lock)
	}
	if lock == "free" || lock == "held" {
		t.Errorf("a file at the lock path was classified %q", lock)
	}
	if data["state"] != "degraded" {
		t.Errorf("state = %v, want degraded", data["state"])
	}
}

// Invalid durable evidence is a REFUSAL in every operational command:
// the facts cannot be trusted, so nothing may be decided and rerunning
// cannot help. Deploy and rollback previously rendered this as
// safe-to-retry infrastructure — the exact opposite of the truth.
func TestCorruptEvidenceIsRefusalNotInfra(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"deploy over corrupt state", []string{"deploy", "production", "--repo-dir", "%DIR%", "--owner", "test"}},
		{"deploy over corrupt attempt marker", []string{"deploy", "production", "--repo-dir", "%DIR%", "--owner", "test"}},
		{"rollback over corrupt state", []string{"rollback", "production", "--to", "2.0.0", "--repo-dir", "%DIR%", "--confirm", "rollback production to 2.0.0", "--owner", "test"}},
		{"deploy over corrupt recovery marker", []string{"deploy", "production", "--repo-dir", "%DIR%", "--owner", "test"}},
	}
	damage := []func(t *testing.T, f *cliFixture){
		func(t *testing.T, f *cliFixture) { writeFileCLIF(t, f.statePath(), "{corrupt") },
		func(t *testing.T, f *cliFixture) { writeFileCLIF(t, f.attemptPath(), "{corrupt") },
		func(t *testing.T, f *cliFixture) { writeFileCLIF(t, f.statePath(), "{corrupt") },
		// Distinct path: ReadRecovery → ErrEvidenceInvalid must
		// survive deploy's wrap and still classify as refusal.
		func(t *testing.T, f *cliFixture) { writeFileCLIF(t, f.recoveryPath(), "{corrupt") },
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIFixture(t)
			damage[i](t, f)

			args := make([]string, len(tc.args))
			for j, a := range tc.args {
				args[j] = strings.ReplaceAll(a, "%DIR%", f.repoDir)
			}
			code, doc := runJSON(t, args...)
			if code != exitFailed {
				t.Fatalf("exit = %d, want %d (%s)", code, exitFailed, doc["message"])
			}
			if doc["outcome"] != "refused" || doc["safeToRetry"] != false {
				t.Errorf("shape = %v safeToRetry=%v, want refused/false", doc["outcome"], doc["safeToRetry"])
			}
			if doc["recoveryRequired"] != false {
				t.Errorf("recoveryRequired = %v: untrustworthy state does not prove markers", doc["recoveryRequired"])
			}
			if len(orderCLI(t, f)) != 0 {
				t.Error("hooks ran despite corrupt evidence")
			}

			code, _, errOut := runCLI(args...)
			if code != exitFailed {
				t.Fatalf("human exit = %d", code)
			}
			if !strings.Contains(errOut, "invalid") || !strings.Contains(errOut, "verify") {
				t.Errorf("human stderr lacks inspect-and-verify guidance:\n%s", errOut)
			}
			if !strings.Contains(errOut, "Do NOT edit") {
				t.Errorf("human stderr must forbid hand-editing observed state:\n%s", errOut)
			}
			if strings.Contains(errOut, "repair the damaged file by hand") || strings.Contains(errOut, "simply") || strings.Contains(errOut, "infrastructure failure") {
				t.Errorf("human stderr misclassifies a refusal or invites hand-editing:\n%s", errOut)
			}
		})
	}
}
