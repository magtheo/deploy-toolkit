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
	altDigest    = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	headSHA      = "headsha"
	envPathConst = EnvironmentsDir + "/production.yaml"
	relPathConst = ".deploy/releases/my-app-0.1.0.yaml"
	oldRelConst  = ".deploy/releases/my-app-0.0.9.yaml"
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
	trees          map[string]map[string]string
	parents        map[string][]string
	runs           []release.CheckRun
	anc            bool
	blobs          map[string][]byte
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
				release.ProjectPath: projectDoc(),
				envPathConst:        envDoc(oldRelConst),
			},
			rev: {release.ProjectPath: projectDoc()},
		},
		trees:    map[string]map[string]string{},
		parents:  map[string][]string{},
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
func (f *fakeStore) CommitParents(ctx context.Context, repo, sha string) ([]string, error) {
	if p, ok := f.parents[sha]; ok {
		return p, nil
	}
	return []string{checkBaseSHA}, nil
}
func (f *fakeStore) CommitTreePaths(ctx context.Context, repo, sha string) (map[string]string, error) {
	t, ok := f.trees[sha]
	if !ok {
		return nil, fmt.Errorf("no tree for %s", sha)
	}
	return t, nil
}
func (f *fakeStore) BlobAt(ctx context.Context, repo, blobSHA string) ([]byte, error) {
	if b, ok := f.blobs[blobSHA]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("unknown blob %s", blobSHA)
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
	f.createdPR = &PullRequest{Number: 41, URL: "https://example.invalid/pull/41", HeadRef: head, BaseRef: base}
	return f.createdPR, nil
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

func gitFixture(t *testing.T) (release.Bundler, string, string) {
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
	return bundle.NewBuilder(dir), strings.TrimSpace(string(out)), dir
}

func setupPropose(t *testing.T) (release.Bundler, *fakeStore, *fakeResolver, string) {
	t.Helper()
	bundler, rev, _ := gitFixture(t)
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

	prop, err := Propose(context.Background(), ProposeInput{
		Repo: "example/my-app", Environment: "production", ReleasePath: releasePath, RepoDir: ".",
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
	if prop.CommitSHA != "new-commit" {
		t.Errorf("commit = %q", prop.CommitSHA)
	}
	if prop.From != oldRelConst || prop.To != relPathConst {
		t.Errorf("from/to = %q → %q", prop.From, prop.To)
	}
	if !prop.IsNewRelease {
		t.Error("expected IsNewRelease")
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
	if err := onlyReleaseChanged(envDoc(oldRelConst), envEntry.Content, prop.To); err != nil {
		t.Errorf("environment change not semantic-only: %v", err)
	}
	if store.createdPR == nil || store.createdPR.BaseRef != release.TrustedBranch {
		t.Errorf("PR = %+v", store.createdPR)
	}
}

func TestProposeExistingReleaseNeedsNoRegistry(t *testing.T) {
	bundler, store, res, releasePath := setupPropose(t)
	relBytes, err := os.ReadFile(releasePath)
	if err != nil {
		t.Fatal(err)
	}
	store.files[checkBaseSHA][relPathConst] = relBytes
	res.digests = map[string]string{}

	prop, err := Propose(context.Background(), ProposeInput{
		Repo: "example/my-app", Environment: "production", ReleasePath: releasePath, RepoDir: ".",
	}, store, res, bundler)
	if err != nil {
		t.Fatalf("Propose existing release: %v", err)
	}
	if prop.IsNewRelease {
		t.Error("existing release classified as new")
	}
	if len(store.treeEntries) != 1 || store.treeEntries[0].Path != envPathConst {
		t.Fatalf("entries = %+v, want env-only change", store.treeEntries)
	}
}

func validOldRelease() []byte {
	return []byte("apiVersion: deploy.toolkit/v1\nkind: Release\nmetadata:\n  project: my-app\n  version: 0.0.9\nsource:\n  type: github\n  repository: example/my-app\n  revision: \"" + strings.Repeat("b", 40) + "\"\nartifacts:\n  app:\n    type: oci\n    image: ghcr.io/example/app\n    digest: " + altDigest + "\nbundle:\n  digest: " + altDigest + "\ndeploymentContract:\n  digest: " + altDigest + "\nmigration:\n  head: \"040\"\n  mode: none\n  rollbackSafe: true\n")
}

func wireVerifiedProposal(store *fakeStore, relBytes []byte) {
	store.files[headSHA] = map[string][]byte{envPathConst: envDoc(relPathConst)}
	store.trees[checkBaseSHA] = map[string]string{
		release.ProjectPath: "pblob",
		envPathConst:        "envblob-base",
		oldRelConst:         "oldrelblob",
	}
	store.trees[headSHA] = map[string]string{
		release.ProjectPath: "pblob",
		envPathConst:        "envblob-head",
		oldRelConst:         "oldrelblob",
		relPathConst:        "relblob",
	}
	store.blobs["pblob"] = projectDoc()
	store.blobs["envblob-base"] = envDoc(oldRelConst)
	store.blobs["envblob-head"] = envDoc(relPathConst)
	store.blobs["oldrelblob"] = validOldRelease()
	store.blobs["relblob"] = relBytes
	store.parents[headSHA] = []string{checkBaseSHA}
	store.heads["deploy/promote-production-0.1.0"] = headSHA
	store.branches["deploy/promote-production-0.1.0"] = true
	store.openPRs["deploy/promote-production-0.1.0"] = PullRequest{
		Number: 7, URL: "u", HeadRef: "deploy/promote-production-0.1.0", BaseRef: release.TrustedBranch,
	}
}

func TestProposeIdempotentReturnsVerifiedExistingPR(t *testing.T) {
	bundler, store, res, releasePath := setupPropose(t)
	relBytes, err := os.ReadFile(releasePath)
	if err != nil {
		t.Fatal(err)
	}
	wireVerifiedProposal(store, relBytes)

	prop, err := Propose(context.Background(), ProposeInput{
		Repo: "example/my-app", Environment: "production", ReleasePath: releasePath, RepoDir: ".",
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

func TestProposeIdempotentRefusesModifiedProposal(t *testing.T) {
	bundler, store, res, releasePath := setupPropose(t)
	relBytes, err := os.ReadFile(releasePath)
	if err != nil {
		t.Fatal(err)
	}
	wireVerifiedProposal(store, relBytes)
	store.blobs["envblob-head"] = envDoc(".deploy/releases/my-app-9.9.9.yaml")
	store.files[headSHA][envPathConst] = envDoc(".deploy/releases/my-app-9.9.9.yaml")

	_, err = Propose(context.Background(), ProposeInput{
		Repo: "example/my-app", Environment: "production", ReleasePath: releasePath, RepoDir: ".",
	}, store, res, bundler)
	if err == nil || !strings.Contains(err.Error(), "stale or modified") {
		t.Errorf("err = %v", err)
	}
}

func TestProposeIdempotentRefusesForeignBaseBranch(t *testing.T) {
	bundler, store, res, releasePath := setupPropose(t)
	store.branches["deploy/promote-production-0.1.0"] = true
	store.openPRs["deploy/promote-production-0.1.0"] = PullRequest{
		Number: 7, URL: "u", HeadRef: "deploy/promote-production-0.1.0", BaseRef: "feature-x",
	}

	_, err := Propose(context.Background(), ProposeInput{
		Repo: "example/my-app", Environment: "production", ReleasePath: releasePath, RepoDir: ".",
	}, store, res, bundler)
	if err == nil || !strings.Contains(err.Error(), "no longer targets") {
		t.Errorf("err = %v", err)
	}
}

func TestProposeConflictingOpenPromotion(t *testing.T) {
	bundler, store, res, releasePath := setupPropose(t)
	store.openPRs["deploy/promote-production-0.2.0"] = PullRequest{Number: 9, URL: "u9", HeadRef: "deploy/promote-production-0.2.0"}

	_, err := Propose(context.Background(), ProposeInput{
		Repo: "example/my-app", Environment: "production", ReleasePath: releasePath, RepoDir: ".",
	}, store, res, bundler)
	if err == nil || !strings.Contains(err.Error(), "conflicting open promotion") {
		t.Errorf("err = %v", err)
	}
}

func TestProposeRefusesNoOpAndWrongProject(t *testing.T) {
	bundler, store, res, releasePath := setupPropose(t)
	store.files[checkBaseSHA][envPathConst] = envDoc(relPathConst)
	if _, err := Propose(context.Background(), ProposeInput{
		Repo: "example/my-app", Environment: "production", ReleasePath: releasePath, RepoDir: ".",
	}, store, res, bundler); err == nil || !strings.Contains(err.Error(), "no-op") {
		t.Errorf("no-op err = %v", err)
	}

	bundler2, store2, res2, releasePath2 := setupPropose(t)
	if _, err := Propose(context.Background(), ProposeInput{
		Repo: "other/my-app", Environment: "production", ReleasePath: releasePath2, RepoDir: ".",
	}, store2, res2, bundler2); err == nil || !strings.Contains(err.Error(), "declares source repository") {
		t.Errorf("wrong-repo err = %v", err)
	}
}

func TestProposeRefusesTamperedEvidence(t *testing.T) {
	bundler, store, res, releasePath := setupPropose(t)
	tampered, err := os.ReadFile(releasePath)
	if err != nil {
		t.Fatal(err)
	}
	tampered = []byte(strings.Replace(string(tampered), digestConst, altDigest, 1))
	if err := os.WriteFile(releasePath, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Propose(context.Background(), ProposeInput{
		Repo: "example/my-app", Environment: "production", ReleasePath: releasePath, RepoDir: ".",
	}, store, res, bundler)
	if err == nil || !strings.Contains(err.Error(), "not exactly what current deterministic eligibility") {
		t.Errorf("tampered err = %v", err)
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
	out, err := SetRelease(raw, relPathConst)
	if err != nil {
		t.Fatal(err)
	}
	if err := onlyReleaseChanged(raw, out, relPathConst); err != nil {
		t.Fatalf("semantic equality violated: %v", err)
	}
	if !strings.Contains(string(out), "autoRollback: safe-only") {
		t.Error("failurePolicy lost in edit")
	}
	if _, err := SetRelease(raw, "../escape.yaml"); err == nil {
		t.Error("invalid ref accepted")
	}
}

func checkFixture(t *testing.T) (release.Bundler, *fakeStore, *fakeResolver, string) {
	t.Helper()
	bundler, rev, dir := gitFixture(t)
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
	relBytes, err := os.ReadFile(rep.ReleasePath)
	if err != nil {
		t.Fatal(err)
	}
	store.trees[checkBaseSHA] = map[string]string{
		release.ProjectPath: "pblob",
		envPathConst:        "envblob-base",
		oldRelConst:         "oldrelblob",
	}
	store.trees[headSHA] = map[string]string{
		release.ProjectPath: "pblob",
		envPathConst:        "envblob-head",
		oldRelConst:         "oldrelblob",
		relPathConst:        "relblob",
	}
	store.files[headSHA] = map[string][]byte{envPathConst: envDoc(relPathConst)}
	store.parents[headSHA] = []string{checkBaseSHA}
	store.blobs["pblob"] = projectDoc()
	store.blobs["envblob-base"] = envDoc(oldRelConst)
	store.blobs["envblob-head"] = envDoc(relPathConst)
	store.blobs["oldrelblob"] = validOldRelease()
	store.blobs["relblob"] = relBytes
	return bundler, store, res, dir
}

func TestCheckDiffPolicy(t *testing.T) {
	t.Run("stale base rejected", func(t *testing.T) {
		bundler, store, res, dir := checkFixture(t)
		store.heads[release.TrustedBranch] = "moved-on"
		outcome, err := Check(context.Background(), CheckInput{Repo: "example/my-app", Base: checkBaseSHA, Head: headSHA, RepoDir: dir}, store, res, bundler)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Passed || !strings.Contains(strings.Join(outcome.Messages, "; "), "stale") {
			t.Errorf("outcome = %+v", outcome)
		}
	})
	t.Run("multi-parent head rejected", func(t *testing.T) {
		bundler, store, res, dir := checkFixture(t)
		store.parents[headSHA] = []string{checkBaseSHA, "other"}
		outcome, err := Check(context.Background(), CheckInput{Repo: "example/my-app", Base: checkBaseSHA, Head: headSHA, RepoDir: dir}, store, res, bundler)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Passed || !strings.Contains(strings.Join(outcome.Messages, "; "), "exactly one commit") {
			t.Errorf("outcome = %+v", outcome)
		}
	})
	t.Run("legal new release diff passes", func(t *testing.T) {
		bundler, store, res, dir := checkFixture(t)
		outcome, err := Check(context.Background(), CheckInput{Repo: "example/my-app", Base: checkBaseSHA, Head: headSHA, RepoDir: dir}, store, res, bundler)
		if err != nil {
			t.Fatal(err)
		}
		if !outcome.Passed {
			t.Errorf("expected pass, got: %v", outcome.Messages)
		}
	})
	t.Run("added release without checkout fails closed", func(t *testing.T) {
		_, store, res, _ := checkFixture(t)
		outcome, err := Check(context.Background(), CheckInput{Repo: "example/my-app", Base: checkBaseSHA, Head: headSHA}, store, res, nil)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Passed || !strings.Contains(strings.Join(outcome.Messages, "; "), "without a checkout") {
			t.Errorf("outcome = %+v", outcome)
		}
	})
	t.Run("tampered evidence rejected", func(t *testing.T) {
		bundler, store, res, dir := checkFixture(t)
		store.blobs["relblob"] = []byte(strings.Replace(string(store.blobs["relblob"]), digestConst, altDigest, 1))
		outcome, err := Check(context.Background(), CheckInput{Repo: "example/my-app", Base: checkBaseSHA, Head: headSHA, RepoDir: dir}, store, res, bundler)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Passed || !strings.Contains(strings.Join(outcome.Messages, "; "), "differs from what current deterministic eligibility") {
			t.Errorf("outcome = %+v", outcome)
		}
	})
	t.Run("foreign repository release rejected", func(t *testing.T) {
		bundler, store, res, dir := checkFixture(t)
		store.blobs["relblob"] = []byte(strings.Replace(string(store.blobs["relblob"]), "repository: example/my-app", "repository: evil/other", 1))
		outcome, err := Check(context.Background(), CheckInput{Repo: "example/my-app", Base: checkBaseSHA, Head: headSHA, RepoDir: dir}, store, res, bundler)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Passed || !strings.Contains(strings.Join(outcome.Messages, "; "), "not bound to trusted project") {
			t.Errorf("outcome = %+v", outcome)
		}
	})
	t.Run("workflow change rejected", func(t *testing.T) {
		bundler, store, res, dir := checkFixture(t)
		store.trees[headSHA][".github/workflows/deploy.yml"] = "wfblob"
		store.blobs["wfblob"] = []byte("on: push")
		outcome, err := Check(context.Background(), CheckInput{Repo: "example/my-app", Base: checkBaseSHA, Head: headSHA, RepoDir: dir}, store, res, bundler)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Passed || !strings.Contains(strings.Join(outcome.Messages, "; "), "illegal changes") {
			t.Errorf("outcome = %+v", outcome)
		}
	})
	t.Run("target swap rejected", func(t *testing.T) {
		bundler, store, res, dir := checkFixture(t)
		swapped := []byte(strings.Replace(string(envDoc(relPathConst)), "target: production-primary", "target: attacker-server", 1))
		store.blobs["envblob-head"] = swapped
		store.files[headSHA][envPathConst] = swapped
		outcome, err := Check(context.Background(), CheckInput{Repo: "example/my-app", Base: checkBaseSHA, Head: headSHA, RepoDir: dir}, store, res, bundler)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Passed || !strings.Contains(strings.Join(outcome.Messages, "; "), "spec.target") {
			t.Errorf("outcome = %+v", outcome)
		}
	})
	t.Run("existing release modification rejected", func(t *testing.T) {
		bundler, store, res, dir := checkFixture(t)
		delete(store.trees[headSHA], oldRelConst)
		outcome, err := Check(context.Background(), CheckInput{Repo: "example/my-app", Base: checkBaseSHA, Head: headSHA, RepoDir: dir}, store, res, bundler)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Passed || !strings.Contains(strings.Join(outcome.Messages, "; "), "releases are immutable") {
			t.Errorf("outcome = %+v", outcome)
		}
	})
	t.Run("rollback to existing release passes without registry", func(t *testing.T) {
		_, store, _, _ := checkFixture(t)
		store.trees[checkBaseSHA] = map[string]string{
			release.ProjectPath: "pblob",
			envPathConst:        "envblob-base",
			relPathConst:        "relblob",
			oldRelConst:         "oldrelblob",
		}
		store.trees[headSHA] = map[string]string{
			release.ProjectPath: "pblob",
			envPathConst:        "envblob-head",
			relPathConst:        "relblob",
			oldRelConst:         "oldrelblob",
		}
		store.files[checkBaseSHA][envPathConst] = envDoc(relPathConst)
		store.files[headSHA][envPathConst] = envDoc(oldRelConst)
		store.blobs["envblob-base"] = envDoc(relPathConst)
		store.blobs["envblob-head"] = envDoc(oldRelConst)
		outcome, err := Check(context.Background(), CheckInput{Repo: "example/my-app", Base: checkBaseSHA, Head: headSHA}, store, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !outcome.Passed {
			t.Errorf("expected pass, got: %v", outcome.Messages)
		}
	})
	t.Run("env only pointing at foreign project rejected", func(t *testing.T) {
		_, store, _, _ := checkFixture(t)
		store.trees[headSHA] = map[string]string{
			release.ProjectPath: "pblob",
			envPathConst:        "envblob-head",
			relPathConst:        "relblob",
		}
		store.files[headSHA][envPathConst] = envDoc(".deploy/releases/other-9.9.9.yaml")
		store.blobs["envblob-head"] = envDoc(".deploy/releases/other-9.9.9.yaml")
		outcome, err := Check(context.Background(), CheckInput{Repo: "example/my-app", Base: checkBaseSHA, Head: headSHA}, store, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Passed || !strings.Contains(strings.Join(outcome.Messages, "; "), "belongs to project") {
			t.Errorf("outcome = %+v", outcome)
		}
	})
}

func TestRenderBodyWarnings(t *testing.T) {
	bundler, store, res, releasePath := setupPropose(t)
	prop, err := Propose(context.Background(), ProposeInput{
		Repo: "example/my-app", Environment: "production", ReleasePath: releasePath, RepoDir: ".",
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
