package main

// End-to-end tests for the trust split: real artifact bytes cross the
// prepare→deploy boundary, and the deploy side is proven to run with
// NO Git on PATH and NO source checkout.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// withoutGitOnPath restricts PATH to the handful of coreutils the local
// transport legitimately uses — git is deliberately absent. If the
// deploy path tried to touch Git it would fail outright.
func withoutGitOnPath(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	for _, b := range []string{"sh", "mkdir", "rmdir", "cat", "test", "rm"} {
		p, err := exec.LookPath(b)
		if err != nil {
			t.Skipf("coreutil %q not found; cannot build a git-free PATH", b)
		}
		if err := os.Symlink(p, filepath.Join(bin, b)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
}

// prepareArtifact runs deployctl prepare against the fixture and
// returns the artifact directory.
func prepareArtifact(t *testing.T, f *cliFixture, extraArgs ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "prepared")
	args := append([]string{"prepare", "production", "--repo-dir", f.repoDir, "--out", dir}, extraArgs...)
	code, doc := runJSON(t, args...)
	if code != exitOK {
		t.Fatalf("prepare: exit %d: %v", code, doc["message"])
	}
	return dir
}

// The full production path: prepare with repository access, deploy the
// prepared bytes with NO Git on PATH. The report carries the
// invocation=prepared fact.
func TestPrepareDeployPreparedEndToEnd(t *testing.T) {
	f := newCLIFixture(t)
	dir := prepareArtifact(t, f)

	withoutGitOnPath(t)
	code, doc := runJSON(t, "deploy-prepared", "--prepared", dir, "--environment", "production")
	if code != exitOK {
		t.Fatalf("deploy-prepared: exit %d: %v", code, doc["message"])
	}
	mustStr(t, doc, "outcome", "success")
	mustStr(t, doc, "project", "my-app")
	mustStr(t, doc, "environment", "production")
	data := doc["data"].(map[string]any)
	if data["invocation"] != "prepared" {
		t.Errorf("data.invocation = %v, want prepared", data["invocation"])
	}
	if data["committed"] != true {
		t.Errorf("data.committed = %v", data["committed"])
	}
}

// deploy-prepared has no --repo-dir: source access cannot be handed to
// the deploy surface, and unknown flags are usage errors.
func TestDeployPreparedHasNoRepoAccess(t *testing.T) {
	f := newCLIFixture(t)
	dir := prepareArtifact(t, f)
	code, doc := runJSON(t, "deploy-prepared", "--prepared", dir, "--repo-dir", f.repoDir)
	if code != exitUsage {
		t.Fatalf("exit = %d, want usage error: %v", code, doc["message"])
	}
}

// Every tamper on the prepared material fails closed as a refusal
// (exit 1) — and NEVER executes anything (the hook-order marker file is
// only written when a lifecycle hook runs).
func TestDeployPreparedTamperMatrix(t *testing.T) {
	f := newCLIFixture(t)

	mutations := map[string]func(t *testing.T, dir string){
		"flipped release manifest byte": func(t *testing.T, dir string) {
			mutateLastByte(t, dir, "release.yaml")
		},
		"truncated bundle": func(t *testing.T, dir string) {
			p := filepath.Join(dir, "bundle.tar")
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, raw[:len(raw)/2], 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"missing environment manifest": func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "environment.yaml")); err != nil {
				t.Fatal(err)
			}
		},
		"swapped bundle for other bytes": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "bundle.tar"), []byte("hostile bytes"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			dir := prepareArtifact(t, f)
			mutate(t, dir)
			code, doc := runJSON(t, "deploy-prepared", "--prepared", dir, "--environment", "production")
			if code != exitFailed || doc["outcome"] != "refused" {
				t.Fatalf("shape = %v/%d, want refused/%d: %v", doc["outcome"], code, exitFailed, doc["message"])
			}
			if _, err := os.Stat(f.marker); err == nil {
				t.Error("a lifecycle hook ran although the material does not verify")
			}
		})
	}
}

// The --environment expectation must match the artifact: the workflow
// passes its input here, so a swapped artifact cannot deploy silently.
func TestDeployPreparedEnvironmentExpectation(t *testing.T) {
	f := newCLIFixture(t)
	dir := prepareArtifact(t, f)
	code, doc := runJSON(t, "deploy-prepared", "--prepared", dir, "--environment", "some-other-env")
	if code != exitFailed || doc["outcome"] != "refused" {
		t.Fatalf("shape = %v/%d, want refused", doc["outcome"], code)
	}
	if !strings.Contains(doc["message"].(string), "some-other-env") {
		t.Errorf("message = %v, want the environment mismatch", doc["message"])
	}
}

// Rollback under the trust split: two artifacts, zero Git on PATH.
func TestRollbackPreparedEndToEnd(t *testing.T) {
	f := newCLIFixture(t)
	if code, out, _ := runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test"); code != exitOK {
		t.Fatalf("deploy 1.0.0: %d %s", code, out)
	}
	// The fixture already created release 2.0.0 (f.revision2); only the
	// environment pin must move.
	f.writeEnv(t, "2.0.0")
	gitf(t, f.repoDir, "add", "-A")
	gitf(t, f.repoDir, "commit", "-q", "-m", "pin 2.0.0")
	if code, out, _ := runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test"); code != exitOK {
		t.Fatalf("deploy 2.0.0: %d %s", code, out)
	}

	// Prepare both artifacts at the current repository state: the
	// from-artifact (2.0.0, the release the environment pins) and the
	// to-artifact (1.0.0, rollback material via --release).
	from := prepareArtifact(t, f)
	to := prepareArtifact(t, f, "--release", "1.0.0")

	withoutGitOnPath(t)
	code, doc := runJSON(t, "rollback-prepared", "--from", from, "--to", to, "--environment", "production")
	if code != exitOK {
		t.Fatalf("rollback-prepared: exit %d: %v", code, doc["message"])
	}
	mustStr(t, doc, "outcome", "success")
	data := doc["data"].(map[string]any)
	if data["invocation"] != "prepared" {
		t.Errorf("data.invocation = %v, want prepared", data["invocation"])
	}
	if data["fromVersion"] != "2.0.0" || data["toVersion"] != "1.0.0" {
		t.Errorf("transition = %v → %v", data["fromVersion"], data["toVersion"])
	}
}

// The prepared artifact never contains secret material: prepare runs
// with credential values in the environment and the bytes stay clean.
func TestPreparedArtifactContainsNoSecrets(t *testing.T) {
	f := newCLIFixture(t)
	const canary = "super-secret-credential-value-canary"
	t.Setenv("TOOLKIT_TARGET_SSH_KEY_PATH", canary)
	t.Setenv("TOOLKIT_TARGET_HOST_KEY_PATH", canary)
	dir := prepareArtifact(t, f)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Fatalf("artifact has %d members, want exactly 5", len(entries))
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), canary) || strings.Contains(string(raw), "PRIVATE KEY") {
			t.Errorf("member %s leaks secret material", e.Name())
		}
	}
}

// Recovery resolution under the trust split, as the rehearsal will run
// it: a failed deployment leaves an attempt marker; the deploy job —
// still with NO Git on PATH — loads the same artifact, reads the
// boundary identity from the failure report, and authorizes resolution
// with the canonical sentence.
func TestRecoveryResolvePreparedEndToEnd(t *testing.T) {
	f := newCLIFixture(t)
	p := filepath.Join(f.repoDir, "deploy/verify.sh")
	raw, _ := os.ReadFile(p)
	if err := os.WriteFile(p, append(raw, []byte("exit 1\n")...), 0o755); err != nil {
		t.Fatal(err)
	}
	gitf(t, f.repoDir, "add", "-A")
	gitf(t, f.repoDir, "commit", "-q", "-m", "break verify")
	rev := gitf(t, f.repoDir, "rev-parse", "HEAD")
	f.writeRelease(t, "1.0.0", rev)

	dir := prepareArtifact(t, f)

	withoutGitOnPath(t)
	code, doc := runJSON(t, "deploy-prepared", "--prepared", dir, "--environment", "production")
	if code != exitFailed || doc["outcome"] != "failure" {
		t.Fatalf("deploy shape = %v/%d, want failure/%d", doc["outcome"], code, exitFailed)
	}
	data := doc["data"].(map[string]any)
	attemptID, _ := data["attemptId"].(string)
	if attemptID == "" {
		t.Fatalf("no attempt boundary identity in the failure report: %v", data)
	}

	// Resolution is an AUTHORIZATION: canonical sentence, exact scope.
	code, doc = runJSON(t, "recovery", "resolve", "--prepared", dir,
		"attempt", attemptID, "--confirm", "resolve production attempt "+attemptID, "--json")
	if code != exitOK {
		t.Fatalf("resolve --prepared: exit %d: %v", code, doc["message"])
	}
	mustStr(t, doc, "outcome", "success")
	mustStr(t, doc, "environment", "production")
}

func mutateLastByte(t *testing.T, dir, name string) {
	t.Helper()
	p := filepath.Join(dir, name)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-3] ^= 0x01
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}
