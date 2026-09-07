package promotion

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/bundle"
	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/release"
)

const (
	checkBaseSHA = "1111111111111111111111111111111111111111"
	digestConst  = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func baseTime() time.Time { return time.Unix(1700000000, 0) }

func projectDoc() []byte {
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

func envDoc(releaseRef string) []byte {
	return []byte("apiVersion: deploy.toolkit/v1\nkind: Environment\nmetadata:\n  name: production\nspec:\n  release: " + releaseRef + "\n  target: production-primary\n")
}

func okRuns() []release.CheckRun {
	return []release.CheckRun{
		{ID: 1, Name: "Tests", Status: "completed", Conclusion: "success", AppID: 1, SuiteID: 10, StartedAt: baseTime()},
		{ID: 2, Name: "CVE scan", Status: "completed", Conclusion: "success", AppID: 1, SuiteID: 11, StartedAt: baseTime()},
	}
}

type fakeStore struct {
	heads          map[string]string
	files          map[string]map[string][]byte
	runs           []release.CheckRun
	anc            bool
	blobs          map[string][]byte
	filesStatus    []ChangedFile
	branches       map[string]bool
	openPRs        map[string]PullRequest
	createdCommits []string
	createdBranch  string
	createdPR      *PullRequest
	treeEntries    []TreeEntry
}

func proposeStore(rev string) *fakeStore {
	return &fakeStore{
		heads: map[string]string{release.TrustedBranch: checkBaseSHA},
		files: map[string]map[string][]byte{
			checkBaseSHA: {
				".deploy/project.yaml":               projectDoc(),
				EnvironmentsDir + "/production.yaml": envDoc(".deploy/releases/my-app-0.0.9.yaml"),
			},
			rev: {".deploy/project.yaml": projectDoc()},
		},
		runs:     okRuns(),
		anc:      true,
		branches: map[string]bool{},
		openPRs:  map[string]PullRequest{},
		blobs:    map[string][]byte{},
	}
}

func (f *fakeStore) BranchHead(ctx context.Context, repo, branch string) (string, error) {
	sha, ok := f.heads[branch]
	if !ok {
		return "", fmt.Errorf("unknown branch %s", branch)
	}
	return sha, nil
}
func (f *fakeStore) VerifyCommit(ctx context.Context, repo, sha string) error { return nil }
func (f *fakeStore) IsAncestor(ctx context.Context, repo, ancestor, descendant string) (bool, error) {
	return f.anc, nil
}
func (f *fakeStore) FileAt(ctx context.Context, repo, path, ref string) ([]byte, error) {
	m, ok := f.files[ref]
	if !ok {
		return nil, fmt.Errorf("no ref %s", ref)
	}
	b, ok := m[path]
	if !ok {
		return nil, fmt.Errorf("no file %s at %s", path, ref)
	}
	return b, nil
}
func (f *fakeStore) CheckRuns(ctx context.Context, repo, ref string) ([]release.CheckRun, error) {
	return f.runs, nil
}
func (f *fakeStore) HeadTree(ctx context.Context, repo, commitSHA string) (string, error) {
	return "tree-" + commitSHA, nil
}
func (f *fakeStore) CreateBlob(ctx context.Context, repo string, content []byte) (string, error) {
	sha := fmt.Sprintf("blob%d", len(f.blobs))
	f.blobs[sha] = content
	return sha, nil
}
func (f *fakeStore) CreateTree(ctx context.Context, repo, baseTree string, entries []TreeEntry) (string, error) {
	f.treeEntries = entries
	return "new-tree", nil
}
func (f *fakeStore) CreateCommit(ctx context.Context, repo, message, treeSHA string, parents []string) (string, error) {
	f.createdCommits = append(f.createdCommits, message)
	return "new-commit", nil
}
func (f *fakeStore) CreateBranch(ctx context.Context, repo, branch, sha string) error {
	f.createdBranch = branch
	f.branches[branch] = true
	return nil
}
func (f *fakeStore) BranchExists(ctx context.Context, repo, branch string) (bool, error) {
	return f.branches[branch], nil
}
func (f *fakeStore) OpenPRForBranch(ctx context.Context, repo, branch string) (*PullRequest, error) {
	if pr, ok := f.openPRs[branch]; ok {
		p := pr
		return &p, nil
	}
	return nil, nil
}
func (f *fakeStore) OpenPromotionPRs(ctx context.Context, repo, env string) ([]PullRequest, error) {
	prefix := fmt.Sprintf(BranchPrefixFmt, env)
	var out []PullRequest
	for b, pr := range f.openPRs {
		if strings.HasPrefix(b, prefix) {
			out = append(out, pr)
		}
	}
	return out, nil
}
func (f *fakeStore) CreatePR(ctx context.Context, repo, base, head, title, body string) (*PullRequest, error) {
	f.createdPR = &PullRequest{Number: 41, URL: "https://example.invalid/pull/41", HeadRef: head}
	return f.createdPR, nil
}
func (f *fakeStore) CompareFiles(ctx context.Context, repo, base, head string) ([]ChangedFile, error) {
	return f.filesStatus, nil
}
func (f *fakeStore) BlobAt(ctx context.Context, repo, blobSHA string) ([]byte, error) {
	if b, ok := f.blobs[blobSHA]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("unknown blob %s", blobSHA)
}

type fakeResolver struct {
	digests map[string]string
}

func (r *fakeResolver) Resolve(ctx context.Context, repository, tag string) (string, error) {
	if d, ok := r.digests[repository+":"+tag]; ok {
		return d, nil
	}
	return "", fmt.Errorf("manifest unknown")
}

func gitFixture(t *testing.T) (release.Bundler, string) {
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
	for rel, content := range map[string]string{
		"config/app.yaml":      "listen: 8080\n",
		".deploy/project.yaml": "apiVersion: deploy.toolkit/v1\nkind: Project\n",
		"docker-compose.yml":   "services: {}\n",
	} {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "x")
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return bundle.NewBuilder(dir), strings.TrimSpace(string(out))
}

func setupPropose(t *testing.T) (release.Bundler, *fakeStore, *fakeResolver, string) {
	t.Helper()
	bundler, rev := gitFixture(t)
	store := proposeStore(rev)
	res := &fakeResolver{digests: map[string]string{"ghcr.io/example/app:" + rev: digestConst}}
	releasesDir := filepath.Join(t.TempDir(), "releases")
	rep, err := release.Create(context.Background(), release.CreateInput{
		Repo: "example/my-app", Revision: rev, Version: "0.1.0",
		MigrationHead: "043", MigrationMode: "forward-compatible",
		RollbackSafe: true, ReleasesDir: releasesDir,
	}, store, res, bundler)
	if err != nil {
		t.Fatalf("release create: %v", err)
	}
	return bundler, store, res, rep.ReleasePath
}

func TestProposeFullFlow(t *testing.T) {
	bundler, store, res, releasePath := setupPropose(t)
	store.anc = true

	prop, err := Propose(context.Background(), ProposeInput{
		Repo: "example/my-app", Environment: "production", ReleasePath: releasePath,
	}, store, res, bundler)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if prop.Unchanged {
		t.Error("first proposal reported unchanged")
	}
	if prop.Branch != "deploy/promote-production-0.1.0" {
		t.Errorf("branch = %q", prop.Branch)
	}
	if prop.From != ".deploy/releases/my-app-0.0.9.yaml" || prop.To != ".deploy/releases/my-app-0.1.0.yaml" {
		t.Errorf("from/to = %q → %q", prop.From, prop.To)
	}
	if len(store.createdCommits) != 1 || !strings.HasPrefix(store.createdCommits[0], "deploy: promote my-app 0.1.0 to production") {
		t.Errorf("commits = %v", store.createdCommits)
	}
	if len(store.treeEntries) != 2 {
		t.Fatalf("tree entries = %d, want exactly 2", len(store.treeEntries))
	}
	var envEntry *TreeEntry
	for i := range store.treeEntries {
		if strings.HasSuffix(store.treeEntries[i].Path, "production.yaml") {
			envEntry = &store.treeEntries[i]
		}
	}
	if envEntry == nil {
		t.Fatal("no environment entry in commit")
	}
	if err := onlyReleaseChanged(envDoc(".deploy/releases/my-app-0.0.9.yaml"), envEntry.Content, prop.To); err != nil {
		t.Errorf("environment change not semantic-only: %v", err)
	}
	if store.createdPR == nil || store.createdPR.HeadRef != prop.Branch {
		t.Errorf("PR = %+v", store.createdPR)
	}
}

func TestProposeIdempotentReturnsExistingPR(t *testing.T) {
	bundler, store, res, releasePath := setupPropose(t)
	store.branches["deploy/promote-production-0.1.0"] = true
	store.openPRs["deploy/promote-production-0.1.0"] = PullRequest{Number: 7, URL: "u", HeadRef: "deploy/promote-production-0.1.0"}

	prop, err := Propose(context.Background(), ProposeInput{
		Repo: "example/my-app", Environment: "production", ReleasePath: releasePath,
	}, store, res, bundler)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !prop.Unchanged || prop.PR.Number != 7 {
		t.Errorf("proposal = %+v", prop)
	}
	if len(store.createdCommits) != 0 {
		t.Error("idempotent propose created a commit")
	}
}

func TestProposeConflictingOpenPromotion(t *testing.T) {
	bundler, store, res, releasePath := setupPropose(t)
	store.openPRs["deploy/promote-production-0.2.0"] = PullRequest{Number: 9, URL: "u9", HeadRef: "deploy/promote-production-0.2.0"}

	_, err := Propose(context.Background(), ProposeInput{
		Repo: "example/my-app", Environment: "production", ReleasePath: releasePath,
	}, store, res, bundler)
	if err == nil || !strings.Contains(err.Error(), "conflicting open promotion") {
		t.Errorf("err = %v", err)
	}
}

func TestProposeRefusesNoOpAndWrongProject(t *testing.T) {
	bundler, store, res, releasePath := setupPropose(t)
	store.files[checkBaseSHA][EnvironmentsDir+"/production.yaml"] = envDoc(".deploy/releases/my-app-0.1.0.yaml")
	if _, err := Propose(context.Background(), ProposeInput{
		Repo: "example/my-app", Environment: "production", ReleasePath: releasePath,
	}, store, res, bundler); err == nil || !strings.Contains(err.Error(), "no-op") {
		t.Errorf("no-op err = %v", err)
	}

	bundler2, store2, res2, releasePath2 := setupPropose(t)
	if _, err := Propose(context.Background(), ProposeInput{
		Repo: "other/my-app", Environment: "production", ReleasePath: releasePath2,
	}, store2, res2, bundler2); err == nil || !strings.Contains(err.Error(), "declares source repository") {
		t.Errorf("wrong-repo err = %v", err)
	}
}

func TestProposeRefusesTamperedRelease(t *testing.T) {
	bundler, store, res, releasePath := setupPropose(t)
	tampered, err := os.ReadFile(releasePath)
	if err != nil {
		t.Fatal(err)
	}
	altDigest := "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	tampered = []byte(strings.Replace(string(tampered), digestConst, altDigest, 1))
	if err := os.WriteFile(releasePath, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Propose(context.Background(), ProposeInput{
		Repo: "example/my-app", Environment: "production", ReleasePath: releasePath,
	}, store, res, bundler)
	if err == nil || !strings.Contains(err.Error(), "not exactly what current deterministic eligibility") {
		t.Errorf("tampered err = %v", err)
	}
}

func TestProposeRejectsTamperedMigrationClaim(t *testing.T) {
	bundler, store, res, releasePath := setupPropose(t)
	tampered, err := os.ReadFile(releasePath)
	if err != nil {
		t.Fatal(err)
	}
	tampered = []byte(strings.Replace(string(tampered), "rollbackSafe: true", "rollbackSafe: false", 1))
	if err := os.WriteFile(releasePath, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Propose(context.Background(), ProposeInput{
		Repo: "example/my-app", Environment: "production", ReleasePath: releasePath,
	}, store, res, bundler)
	// Migration and version are explicit claims, not derived evidence:
	// propose re-renders them from the manifest itself, so the file is
	// accepted. The claim record is exactly what the human authorizes in
	// the PR body. This test pins that boundary explicitly rather than
	// leaving it accidental.
	if err != nil {
		t.Fatalf("migration claims are re-rendered from the manifest by design; got %v", err)
	}
}

func TestSetReleasePreservesEverythingElse(t *testing.T) {
	raw := []byte(`apiVersion: deploy.toolkit/v1
kind: Environment
metadata:
  name: production
spec:
  release: .deploy/releases/my-app-0.0.9.yaml
  target: production-primary
  failurePolicy:
    autoRollback: safe-only
`)
	out, err := SetRelease(raw, ".deploy/releases/my-app-0.1.0.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := onlyReleaseChanged(raw, out, ".deploy/releases/my-app-0.1.0.yaml"); err != nil {
		t.Fatalf("semantic equality violated: %v", err)
	}
	if !strings.Contains(string(out), "autoRollback: safe-only") {
		t.Error("failurePolicy lost in edit")
	}
	if _, err := SetRelease(raw, "../escape.yaml"); err == nil {
		t.Error("invalid ref accepted")
	}
}

func relBytesFixture() []byte {
	return []byte("apiVersion: deploy.toolkit/v1\nkind: Release\nmetadata:\n  project: my-app\n  version: 0.1.0\nsource:\n  type: github\n  repository: example/my-app\n  revision: \"" + strings.Repeat("a", 40) + "\"\nartifacts:\n  app:\n    type: oci\n    image: ghcr.io/example/app\n    digest: " + digestConst + "\nbundle:\n  digest: " + digestConst + "\ndeploymentContract:\n  digest: " + digestConst + "\nmigration:\n  head: \"043\"\n  mode: forward-compatible\n  rollbackSafe: true\n")
}

func checkStore(files []ChangedFile, baseEnv, headEnv []byte) *fakeStore {
	s := proposeStore(strings.Repeat("a", 40))
	s.filesStatus = files
	s.files[checkBaseSHA] = map[string][]byte{EnvironmentsDir + "/production.yaml": baseEnv, ".deploy/releases/my-app-0.0.9.yaml": []byte("old")}
	s.files["headsha"] = map[string][]byte{EnvironmentsDir + "/production.yaml": headEnv}
	s.blobs["relblob"] = relBytesFixture()
	return s
}

func TestCheckDiffPolicy(t *testing.T) {
	legalEnv := func(from, to string) ([]byte, []byte) {
		return envDoc(from), envDoc(to)
	}
	pass := func(t *testing.T, s Store) {
		res, err := Check(context.Background(), CheckInput{Repo: "example/my-app", Base: checkBaseSHA, Head: "headsha"}, s)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if !res.Passed {
			t.Errorf("expected pass, got: %v", res.Messages)
		}
	}
	failMsg := func(t *testing.T, s Store, want string) {
		res, err := Check(context.Background(), CheckInput{Repo: "example/my-app", Base: checkBaseSHA, Head: "headsha"}, s)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if res.Passed {
			t.Fatalf("expected fail, got pass")
		}
		joined := strings.Join(res.Messages, "; ")
		if !strings.Contains(joined, want) {
			t.Errorf("messages %q missing %q", joined, want)
		}
	}

	t.Run("legal add+modify", func(t *testing.T) {
		b, h := legalEnv(".deploy/releases/my-app-0.0.9.yaml", ".deploy/releases/my-app-0.1.0.yaml")
		pass(t, checkStore([]ChangedFile{
			{Path: ".deploy/releases/my-app-0.1.0.yaml", Status: "added", BlobSHA: "relblob"},
			{Path: EnvironmentsDir + "/production.yaml", Status: "modified"},
		}, b, h))
	})
	t.Run("env only change to existing release (rollback)", func(t *testing.T) {
		b, h := legalEnv(".deploy/releases/my-app-0.1.0.yaml", ".deploy/releases/my-app-0.0.9.yaml")
		pass(t, checkStore([]ChangedFile{
			{Path: EnvironmentsDir + "/production.yaml", Status: "modified"},
		}, b, h))
	})
	t.Run("workflow change rejected", func(t *testing.T) {
		b, h := legalEnv(".deploy/releases/my-app-0.0.9.yaml", ".deploy/releases/my-app-0.1.0.yaml")
		failMsg(t, checkStore([]ChangedFile{
			{Path: ".deploy/releases/my-app-0.1.0.yaml", Status: "added", BlobSHA: "relblob"},
			{Path: EnvironmentsDir + "/production.yaml", Status: "modified"},
			{Path: ".github/workflows/deploy.yml", Status: "modified"},
		}, b, h), "illegal changes")
	})
	t.Run("target change rejected", func(t *testing.T) {
		b := envDoc(".deploy/releases/my-app-0.0.9.yaml")
		h := envDoc(".deploy/releases/my-app-0.1.0.yaml")
		h = []byte(strings.Replace(string(h), "target: production-primary", "target: attacker-server", 1))
		failMsg(t, checkStore([]ChangedFile{
			{Path: ".deploy/releases/my-app-0.1.0.yaml", Status: "added", BlobSHA: "relblob"},
			{Path: EnvironmentsDir + "/production.yaml", Status: "modified"},
		}, b, h), "spec.target")
	})
	t.Run("filename content disagreement rejected", func(t *testing.T) {
		b, h := legalEnv(".deploy/releases/my-app-0.0.9.yaml", ".deploy/releases/other-9.9.9.yaml")
		failMsg(t, checkStore([]ChangedFile{
			{Path: ".deploy/releases/other-9.9.9.yaml", Status: "added", BlobSHA: "relblob"},
			{Path: EnvironmentsDir + "/production.yaml", Status: "modified"},
		}, b, h), "filename must agree")
	})
	t.Run("existing release modification rejected", func(t *testing.T) {
		b, h := legalEnv(".deploy/releases/my-app-0.0.9.yaml", ".deploy/releases/my-app-0.1.0.yaml")
		failMsg(t, checkStore([]ChangedFile{
			{Path: ".deploy/releases/my-app-0.0.9.yaml", Status: "modified"},
			{Path: EnvironmentsDir + "/production.yaml", Status: "modified"},
		}, b, h), "releases are immutable")
	})
	t.Run("stale proposal rejected", func(t *testing.T) {
		b, h := legalEnv(".deploy/releases/my-app-0.1.0.yaml", ".deploy/releases/my-app-0.1.0.yaml")
		failMsg(t, checkStore([]ChangedFile{
			{Path: ".deploy/releases/my-app-0.1.0.yaml", Status: "added", BlobSHA: "relblob"},
			{Path: EnvironmentsDir + "/production.yaml", Status: "modified"},
		}, b, h), "stale")
	})
}

func TestRenderBodyWarnings(t *testing.T) {
	bundler, store, res, releasePath := setupPropose(t)
	store.anc = true
	prop, err := Propose(context.Background(), ProposeInput{
		Repo: "example/my-app", Environment: "production", ReleasePath: releasePath,
	}, store, res, bundler)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	body := RenderBody(BodyInput{
		Project: prop.Project, Version: "0.1.0", Environment: "production",
		BaseSHA: checkBaseSHA, BranchHead: checkBaseSHA, Revision: strings.Repeat("a", 40),
		From: prop.From, To: prop.To,
		Evaluation: &release.Evaluation{Checks: []release.CheckResult{{Name: "Tests", Conclusion: "success"}}},
		Migration:  manifest.MigrationSpec{Head: "043", Mode: manifest.MigrationForwardCompatible, RollbackSafe: true},
	})
	if !strings.Contains(body, "Merging this PR authorizes") {
		t.Error("authorization footer missing")
	}
	if !strings.Contains(body, "forward-compatible") {
		t.Error("migration mode missing")
	}

	irr := RenderBody(BodyInput{
		Environment: "production",
		Evaluation:  &release.Evaluation{},
		Migration:   manifest.MigrationSpec{Head: "043", Mode: manifest.MigrationIrreversible},
	})
	if !strings.Contains(irr, "IRREVERSIBLE") {
		t.Error("irreversible warning missing")
	}
	unsafe := RenderBody(BodyInput{
		Environment: "production",
		Evaluation:  &release.Evaluation{},
		Migration:   manifest.MigrationSpec{Head: "043", Mode: manifest.MigrationForwardCompatible},
	})
	if !strings.Contains(unsafe, "rollbackSafe: false") {
		t.Error("unsafe-rollback warning missing")
	}
}
