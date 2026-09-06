package bundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, stderr.String())
	}
	return stdout.String()
}

func fixtureRepo(t *testing.T) (dir, rev string) {
	t.Helper()
	dir = t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "Test")

	mustWrite := func(rel, content string, mode os.FileMode) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("docker-compose.yml", "services: {}\n", 0o644)
	mustWrite("config/app.yaml", "listen: 8080\n", 0o644)
	mustWrite("config/sub/deep.yaml", "deep: true\n", 0o644)
	mustWrite("deploy.sh", "#!/bin/sh\n", 0o755)
	mustWrite(filepath.Join(".deploy", "project.yaml"), "apiVersion: deploy.toolkit/v1\nkind: Project\n", 0o644)
	if err := os.Symlink("config/app.yaml", filepath.Join(dir, "current.yaml")); err != nil {
		t.Fatal(err)
	}

	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "fixture")
	rev = strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))
	return dir, rev
}

func TestBuildDeterministicBundle(t *testing.T) {
	dir, rev := fixtureRepo(t)
	b := NewBuilder(dir)
	ctx := context.Background()

	res1, err := b.Build(ctx, rev, []string{"docker-compose.yml", "config/**"})
	if err != nil {
		t.Fatal(err)
	}
	res2, err := b.Build(ctx, rev, []string{"docker-compose.yml", "config/**"})
	if err != nil {
		t.Fatal(err)
	}

	if res1.Digest != res2.Digest {
		t.Errorf("digest not deterministic: %s vs %s", res1.Digest, res2.Digest)
	}
	want := []string{".deploy/project.yaml", "config/app.yaml", "config/sub/deep.yaml", "docker-compose.yml"}
	if fmt.Sprint(res1.Files) != fmt.Sprint(want) {
		t.Errorf("files = %v, want %v", res1.Files, want)
	}
	if res1.ContractDigest == "" || res1.Digest == "" {
		t.Error("missing digests")
	}
}

func TestContractDigestIsExactBytes(t *testing.T) {
	dir, rev := fixtureRepo(t)
	res, err := NewBuilder(dir).Build(context.Background(), rev, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".deploy", "project.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	want := fmt.Sprintf("sha256:%064x", sum)
	if res.ContractDigest != want {
		t.Errorf("contract digest = %s, want %s", res.ContractDigest, want)
	}
}

func TestBuildExecutableAndSymlinkPreserved(t *testing.T) {
	dir, rev := fixtureRepo(t)
	res, err := NewBuilder(dir).Build(context.Background(), rev, []string{"deploy.sh", "current.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, f := range res.Files {
		found[f] = true
	}
	if !found["deploy.sh"] || !found["current.yaml"] || !found[".deploy/project.yaml"] {
		t.Errorf("expected deploy.sh and current.yaml bundled, got %v", res.Files)
	}
}

func TestBuildRejectsUnmatchedInclude(t *testing.T) {
	dir, rev := fixtureRepo(t)
	if _, err := NewBuilder(dir).Build(context.Background(), rev, []string{"nope/**"}); err == nil {
		t.Error("expected zero-match include to fail")
	}
}

func TestBuildRejectsMissingContract(t *testing.T) {
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "config", "user.email", "t@e.com")
	gitRun(t, dir, "config", "user.name", "T")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644)
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "x")
	rev := gitRun(t, dir, "rev-parse", "HEAD")
	if _, err := NewBuilder(dir).Build(context.Background(), rev, []string{"a.txt"}); err == nil {
		t.Error("expected missing contract to fail")
	}
}

func TestSymlinkTargetValidation(t *testing.T) {
	if err := checkSymlinkTarget("current.yaml", "/etc/passwd"); err == nil {
		t.Error("absolute target accepted")
	}
	if err := checkSymlinkTarget("a/b/link", "../../../outside"); err == nil {
		t.Error("escaping target accepted")
	}
	if err := checkSymlinkTarget("a/b/link", "../../outside"); err != nil {
		t.Errorf("target resolving inside the bundle root rejected: %v", err)
	}
	if err := checkSymlinkTarget("a/link", "../b/file"); err != nil {
		t.Errorf("in-bundle relative target rejected: %v", err)
	}
}

func TestPatternMatching(t *testing.T) {
	cases := []struct {
		path, pattern string
		want          bool
	}{
		{"config/app.yaml", "config/**", true},
		{"config/sub/deep.yaml", "config/**", true},
		{"config", "config/**", true},
		{"docker-compose.yml", "docker-compose.yml", true},
		{"docker-compose.yml", "config/**", false},
		{"a/x/b", "a/**/b", true},
		{"a/b", "a/**/b", true},
		{"a/x/y/b", "a/**/b", true},
		{"a/x/other", "a/*/b", false},
		{"root.yaml", "*.yaml", true},
		{"sub/root.yaml", "*.yaml", false},
		{"sub/root.yaml", "**/root.yaml", true},
	}
	for _, c := range cases {
		if got := matches(c.path, c.pattern); got != c.want {
			t.Errorf("matches(%q, %q) = %v, want %v", c.path, c.pattern, got, c.want)
		}
	}
}
