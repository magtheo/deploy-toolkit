package release

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/magtheo/deploy-toolkit/internal/bundle"
)

const (
	testRevision = "0123456789abcdef0123456789abcdef01234567"
	testDigest   = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func policyDoc() []byte {
	return []byte(`apiVersion: deploy.toolkit/v1
kind: Project
metadata:
  name: my-app
release:
  source:
    type: github
    repository: example/my-app
    branch: main
  requiredChecks:
    - Tests
    - CVE scan
artifacts:
  app:
    type: oci
    repository: ghcr.io/example/app
bundle:
  include:
    - config/**
lifecycle:
  apply:
    argv: ["./deploy/apply.sh"]
`)
}

func fakeSource(rev, branchHead string, policy, material []byte, runs []CheckRun, ancestor bool) Source {
	return &stubSource{
		head:     branchHead,
		files:    map[string][]byte{branchHead: policy, rev: material},
		runs:     runs,
		ancestor: ancestor,
		existRev: rev,
	}
}

type stubSource struct {
	head     string
	files    map[string][]byte
	runs     []CheckRun
	ancestor bool
	existRev string
}

func (s *stubSource) BranchHead(ctx context.Context, repo, branch string) (string, error) {
	return s.head, nil
}
func (s *stubSource) VerifyCommit(ctx context.Context, repo, sha string) error {
	if sha != s.existRev {
		return fmt.Errorf("not found")
	}
	return nil
}
func (s *stubSource) IsAncestor(ctx context.Context, repo, ancestor, descendant string) (bool, error) {
	return s.ancestor, nil
}
func (s *stubSource) FileAt(ctx context.Context, repo, path, ref string) ([]byte, error) {
	b, ok := s.files[ref]
	if !ok {
		return nil, fmt.Errorf("no file at %s", ref)
	}
	return b, nil
}
func (s *stubSource) CheckRuns(ctx context.Context, repo, ref string) ([]CheckRun, error) {
	return s.runs, nil
}

type stubResolver struct {
	digests map[string]string
}

func (r *stubResolver) Resolve(ctx context.Context, repository, tag string) (string, error) {
	if d, ok := r.digests[repository+":"+tag]; ok {
		return d, nil
	}
	return "", fmt.Errorf("manifest unknown")
}

func gitFixture(t *testing.T) (bundler Bundler, rev string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@e.com")
	run("config", "user.name", "T")
	p := filepath.Join(dir, "config", "app.yaml")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("listen: 8080\n"), 0o644)
	p2 := filepath.Join(dir, ".deploy", "project.yaml")
	os.MkdirAll(filepath.Dir(p2), 0o755)
	os.WriteFile(p2, []byte("apiVersion: deploy.toolkit/v1\nkind: Project\n"), 0o644)
	run("add", "-A")
	run("commit", "-q", "-m", "x")
	rev = gitOut(t, dir, "rev-parse", "HEAD")
	return bundle.NewBuilder(dir), rev
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func TestCreateHappyPath(t *testing.T) {
	bundler, rev := gitFixture(t)
	src := fakeSource(rev, "9999"+rev[4:], policyDoc(), policyDoc(), []CheckRun{
		{Name: "Tests", Status: "completed", Conclusion: "success", AppID: 1, SuiteID: 10},
		{Name: "CVE scan", Status: "completed", Conclusion: "success", AppID: 1, SuiteID: 11},
	}, true)
	res := &stubResolver{digests: map[string]string{"ghcr.io/example/app:" + rev: testDigest}}
	releasesDir := filepath.Join(t.TempDir(), "releases")

	in := CreateInput{
		Repo: "example/my-app", Revision: rev,
		Version: "0.1.0", MigrationHead: "043", MigrationMode: "forward-compatible",
		RollbackSafe: true, ReleasesDir: releasesDir,
	}
	rep, err := Create(context.Background(), in, src, res, bundler)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rep.Unchanged {
		t.Error("first create reported unchanged")
	}
	if rep.Project != "my-app" || rep.BundleDigest == "" || rep.ContractDigest == "" {
		t.Errorf("incomplete report: %+v", rep)
	}
	if len(rep.Checks) != 2 {
		t.Errorf("checks = %+v", rep.Checks)
	}
	a := rep.Artifacts["app"]
	if a.Digest != testDigest || a.Repository != "ghcr.io/example/app" {
		t.Errorf("artifact = %+v", a)
	}

	data, err := os.ReadFile(rep.ReleasePath)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, string(data), "revision: "+rev+"\n")
	assertContains(t, string(data), "digest: "+testDigest+"\n")
	assertNotContains(t, string(data), rev+":")

	rep2, err := Create(context.Background(), in, src, res, bundler)
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if !rep2.Unchanged {
		t.Error("idempotent rerun reported changed")
	}
}

func TestCreateRefusesOverwrite(t *testing.T) {
	bundler, rev := gitFixture(t)
	src := fakeSource(rev, "9999"+rev[4:], policyDoc(), policyDoc(), []CheckRun{
		{Name: "Tests", Status: "completed", Conclusion: "success", AppID: 1, SuiteID: 10},
		{Name: "CVE scan", Status: "completed", Conclusion: "success", AppID: 1, SuiteID: 11},
	}, true)
	res := &stubResolver{digests: map[string]string{"ghcr.io/example/app:" + rev: testDigest}}
	releasesDir := filepath.Join(t.TempDir(), "releases")
	in := CreateInput{Repo: "example/my-app", Revision: rev, Version: "0.1.0", MigrationHead: "043", MigrationMode: "none", ReleasesDir: releasesDir}
	if _, err := Create(context.Background(), in, src, res, bundler); err != nil {
		t.Fatal(err)
	}

	res2 := &stubResolver{digests: map[string]string{"ghcr.io/example/app:" + rev: "sha256:" + strings.Repeat("ab", 32)}}
	if _, err := Create(context.Background(), in, src, res2, bundler); err == nil {
		t.Fatal("expected overwrite refusal")
	} else if !strings.Contains(err.Error(), "different release") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestCreateRejections(t *testing.T) {
	okRuns := []CheckRun{
		{Name: "Tests", Status: "completed", Conclusion: "success", AppID: 1, SuiteID: 10},
		{Name: "CVE scan", Status: "completed", Conclusion: "success", AppID: 1, SuiteID: 11},
	}
	try := func(t *testing.T, runs []CheckRun, ancestor bool, mutatePolicy, mutateMaterial func() []byte, in CreateInput) error {
		t.Helper()
		bundler, rev := gitFixture(t)
		policy, material := policyDoc(), policyDoc()
		if mutatePolicy != nil {
			policy = mutatePolicy()
		}
		if mutateMaterial != nil {
			material = mutateMaterial()
		}
		src := fakeSource(rev, "9999"+rev[4:], policy, material, runs, ancestor)
		_, err := Create(context.Background(), in, src, &stubResolver{digests: map[string]string{"ghcr.io/example/app:" + rev: testDigest}}, bundler)
		return err
	}
	releasesDir := filepath.Join(t.TempDir(), "releases")
	std := CreateInput{Repo: "example/my-app", Version: "0.1.0", MigrationHead: "1", MigrationMode: "none", ReleasesDir: releasesDir}

	t.Run("not ancestor", func(t *testing.T) {
		in := std
		_, rev := gitFixture(t)
		in.Revision = rev
		err := try(t, okRuns, false, nil, nil, in)
		if err == nil || !strings.Contains(err.Error(), "not reachable") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("missing check", func(t *testing.T) {
		in := std
		_, rev := gitFixture(t)
		in.Revision = rev
		err := try(t, okRuns[:1], true, nil, nil, in)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("failed check", func(t *testing.T) {
		in := std
		_, rev := gitFixture(t)
		in.Revision = rev
		err := try(t, []CheckRun{
			{Name: "Tests", Status: "completed", Conclusion: "failure", AppID: 1, SuiteID: 10},
			{Name: "CVE scan", Status: "completed", Conclusion: "success", AppID: 1, SuiteID: 11},
		}, true, nil, nil, in)
		if err == nil || !strings.Contains(err.Error(), "failure") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("ambiguous check", func(t *testing.T) {
		in := std
		_, rev := gitFixture(t)
		in.Revision = rev
		err := try(t, []CheckRun{
			{Name: "Tests", Status: "completed", Conclusion: "success", AppID: 1, SuiteID: 10},
			{Name: "Tests", Status: "completed", Conclusion: "success", AppID: 1, SuiteID: 99},
			{Name: "CVE scan", Status: "completed", Conclusion: "success", AppID: 1, SuiteID: 11},
		}, true, nil, nil, in)
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("identity mismatch", func(t *testing.T) {
		in := std
		_, rev := gitFixture(t)
		in.Revision = rev
		err := try(t, okRuns, true, nil, func() []byte {
			return []byte(strings.Replace(string(policyDoc()), "name: my-app", "name: other-app", 1))
		}, in)
		if err == nil || !strings.Contains(err.Error(), "identity mismatch") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("policy declares foreign repository", func(t *testing.T) {
		in := std
		_, rev := gitFixture(t)
		in.Revision = rev
		in.Repo = "other/my-app"
		err := try(t, okRuns, true, nil, nil, in)
		if err == nil || !strings.Contains(err.Error(), "declares source repository") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("policy declares non-trusted branch", func(t *testing.T) {
		in := std
		_, rev := gitFixture(t)
		in.Revision = rev
		err := try(t, okRuns, true, func() []byte {
			return []byte(strings.Replace(string(policyDoc()), "branch: main", "branch: develop", 1))
		}, nil, in)
		if err == nil || !strings.Contains(err.Error(), "trusted integration branch") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("irreversible with rollback safe input", func(t *testing.T) {
		in := std
		_, rev := gitFixture(t)
		in.Revision = rev
		in.MigrationMode = "irreversible"
		in.RollbackSafe = true
		err := try(t, okRuns, true, nil, nil, in)
		if err == nil || !strings.Contains(err.Error(), "rollback-safe") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("short revision input", func(t *testing.T) {
		in := std
		in.Revision = "abc"
		err := try(t, okRuns, true, nil, nil, in)
		if err == nil || !strings.Contains(err.Error(), "full commit SHA") {
			t.Errorf("err = %v", err)
		}
	})
}

func assertContains(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Errorf("%q does not contain %q", s, sub)
	}
}

func assertNotContains(t *testing.T, s, sub string) {
	t.Helper()
	if strings.Contains(s, sub) {
		t.Errorf("%q unexpectedly contains %q", s, sub)
	}
}
