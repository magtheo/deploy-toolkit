package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/magtheo/deploy-toolkit/internal/promotion"
	"github.com/magtheo/deploy-toolkit/internal/release"
)

// Minimal promotion.Store fake: just enough state for the classify CLI
// exit-code contract (no evidence path — the PROMOTION case is a rollback
// to an existing release, which requires no registry).
type classifyStore struct {
	heads   map[string]string
	trees   map[string]map[string]string
	files   map[string]map[string][]byte
	blobs   map[string][]byte
	parents map[string][]string
	anc     bool
	headErr error
}

func (f *classifyStore) BranchHead(ctx context.Context, repo, branch string) (string, error) {
	if f.headErr != nil {
		return "", f.headErr
	}
	sha, ok := f.heads[branch]
	if !ok {
		return "", fmt.Errorf("unknown branch %s", branch)
	}
	return sha, nil
}
func (f *classifyStore) VerifyCommit(ctx context.Context, repo, sha string) error { return nil }
func (f *classifyStore) IsAncestor(ctx context.Context, repo, ancestor, descendant string) (bool, error) {
	return f.anc, nil
}
func (f *classifyStore) FileAt(ctx context.Context, repo, path, ref string) ([]byte, error) {
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
func (f *classifyStore) FileAtOptional(ctx context.Context, repo, path, ref string) ([]byte, bool, error) {
	m, ok := f.files[ref]
	if !ok {
		return nil, false, nil
	}
	b, ok := m[path]
	if !ok {
		return nil, false, nil
	}
	return b, true, nil
}
func (f *classifyStore) CheckRuns(ctx context.Context, repo, ref string) ([]release.CheckRun, error) {
	return nil, nil
}
func (f *classifyStore) CommitParents(ctx context.Context, repo, sha string) ([]string, error) {
	return f.parents[sha], nil
}
func (f *classifyStore) CommitTreeLeaves(ctx context.Context, repo, sha string) (map[string]promotion.TreeLeaf, error) {
	t, ok := f.trees[sha]
	if !ok {
		return nil, fmt.Errorf("no tree for %s", sha)
	}
	out := make(map[string]promotion.TreeLeaf, len(t))
	for p, oid := range t {
		out[p] = promotion.TreeLeaf{OID: oid, Mode: "100644", Type: "blob"}
	}
	return out, nil
}
func (f *classifyStore) BlobAt(ctx context.Context, repo, blobSHA string) ([]byte, error) {
	b, ok := f.blobs[blobSHA]
	if !ok {
		return nil, fmt.Errorf("unknown blob %s", blobSHA)
	}
	return b, nil
}
func (f *classifyStore) HeadTree(ctx context.Context, repo, commitSHA string) (string, error) {
	return "tree-" + commitSHA, nil
}
func (f *classifyStore) CreateBlob(ctx context.Context, repo string, content []byte) (string, error) {
	return "", fmt.Errorf("not implemented")
}
func (f *classifyStore) CreateTree(ctx context.Context, repo, baseTree string, entries []promotion.TreeEntry) (string, error) {
	return "", fmt.Errorf("not implemented")
}
func (f *classifyStore) CreateCommit(ctx context.Context, repo, message, treeSHA string, parents []string) (string, error) {
	return "", fmt.Errorf("not implemented")
}
func (f *classifyStore) CreateBranch(ctx context.Context, repo, branch, sha string) error {
	return fmt.Errorf("not implemented")
}
func (f *classifyStore) BranchExists(ctx context.Context, repo, branch string) (bool, error) {
	return false, nil
}
func (f *classifyStore) OpenPRForBranch(ctx context.Context, repo, branch string) (*promotion.PullRequest, error) {
	return nil, nil
}
func (f *classifyStore) OpenPromotionPRs(ctx context.Context, repo, env string) ([]promotion.PullRequest, error) {
	return nil, nil
}
func (f *classifyStore) CreatePR(ctx context.Context, repo, base, head, title, body string) (*promotion.PullRequest, error) {
	return nil, fmt.Errorf("not implemented")
}

const (
	cliBase = "1111111111111111111111111111111111111111"
	cliHead = "2222222222222222222222222222222222222222"
	cliEnv  = ".deploy/environments/production.yaml"
	cliRel  = ".deploy/releases/my-app-0.1.0.yaml"
	cliOld  = ".deploy/releases/my-app-0.0.9.yaml"
)

func cliProjectDoc() []byte {
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
  verify:
    argv: ["./deploy/verify.sh"]
`)
}

func cliEnvDoc(releaseRef string) []byte {
	return []byte("apiVersion: deploy.toolkit/v1\nkind: Environment\nmetadata:\n  name: production\nspec:\n  release: " + releaseRef + "\n  target: production-primary\n")
}

func cliOldRelease() []byte {
	return []byte("apiVersion: deploy.toolkit/v1\nkind: Release\nmetadata:\n  project: my-app\n  version: 0.0.9\nsource:\n  type: github\n  repository: example/my-app\n  revision: \"" + strings.Repeat("b", 40) + "\"\nartifacts:\n  app:\n    type: oci\n    image: ghcr.io/example/app\n    digest: sha256:" + strings.Repeat("f", 64) + "\nbundle:\n  digest: sha256:" + strings.Repeat("f", 64) + "\ndeploymentContract:\n  digest: sha256:" + strings.Repeat("f", 64) + "\nmigration:\n  head: \"040\"\n  mode: none\n  rollbackSafe: true\n")
}

// cliRollbackStore wires a push-mode rollback: the environment flips from
// the new release back to an existing older one. No evidence (checkout,
// registry) is involved.
func cliRollbackStore() *classifyStore {
	doc := cliProjectDoc()
	s := &classifyStore{
		heads: map[string]string{"main": cliHead},
		anc:   true,
		parents: map[string][]string{
			cliHead: {cliBase},
		},
		trees: map[string]map[string]string{
			cliBase: {".deploy/project.yaml": "pblob", cliEnv: "envbase", cliRel: "relblob", cliOld: "oldblob"},
			cliHead: {".deploy/project.yaml": "pblob", cliEnv: "envhead", cliRel: "relblob", cliOld: "oldblob"},
		},
		files: map[string]map[string][]byte{
			cliBase: {".deploy/project.yaml": doc, cliEnv: cliEnvDoc(cliRel)},
			cliHead: {".deploy/project.yaml": doc, cliEnv: cliEnvDoc(cliOld)},
		},
		blobs: map[string][]byte{
			"pblob":   doc,
			"envbase": cliEnvDoc(cliRel),
			"envhead": cliEnvDoc(cliOld),
			"oldblob": cliOldRelease(),
		},
	}
	return s
}

func runClassifyCLI(t *testing.T, s *classifyStore, in *promotion.ClassifyInput, jsonMode bool) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runPromotionClassifyWith(context.Background(), in, jsonMode, s, nil, nil, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestPromotionClassifyExitCodes pins the CLI exit mapping: 0 PROMOTION,
// 1 determined no (ORDINARY and INVALID), 2 usage, 3 infrastructure.
// Consumers must branch on the classification value, never the code alone.
func TestPromotionClassifyExitCodes(t *testing.T) {
	pushIn := func() *promotion.ClassifyInput {
		return &promotion.ClassifyInput{Repo: "example/my-app", Mode: promotion.ModePush, Base: cliBase, Head: cliHead}
	}

	t.Run("PROMOTION exits 0", func(t *testing.T) {
		code, out, errOut := runClassifyCLI(t, cliRollbackStore(), pushIn(), false)
		if code != 0 || !strings.Contains(out, "classification: PROMOTION") || errOut != "" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
		}
	})
	t.Run("ORDINARY exits 1", func(t *testing.T) {
		s := cliRollbackStore()
		s.trees[cliHead][cliEnv] = "envbase" // same env content: no pointer change
		s.files[cliHead][cliEnv] = cliEnvDoc(cliRel)
		s.trees[cliHead]["docs/readme.md"] = "r"
		s.blobs["r"] = []byte("hi\n")
		code, out, _ := runClassifyCLI(t, s, pushIn(), false)
		if code != 1 || !strings.Contains(out, "classification: ORDINARY") {
			t.Fatalf("code=%d stdout=%q", code, out)
		}
	})
	t.Run("INVALID exits 1", func(t *testing.T) {
		s := cliRollbackStore()
		s.trees[cliHead][".github/workflows/deploy.yml"] = "wf"
		s.blobs["wf"] = []byte("on: push\n")
		code, out, _ := runClassifyCLI(t, s, pushIn(), false)
		if code != 1 || !strings.Contains(out, "classification: INVALID") {
			t.Fatalf("code=%d stdout=%q", code, out)
		}
	})
	t.Run("ERROR exits 3", func(t *testing.T) {
		s := cliRollbackStore()
		s.headErr = fmt.Errorf("502 bad gateway")
		code, out, _ := runClassifyCLI(t, s, pushIn(), false)
		if code != 3 || !strings.Contains(out, "classification: ERROR") {
			t.Fatalf("code=%d stdout=%q", code, out)
		}
	})
	t.Run("usage error exits 2", func(t *testing.T) {
		code, _, errOut := runClassifyCLI(t, cliRollbackStore(), &promotion.ClassifyInput{Repo: "example/my-app", Mode: "bogus", Base: cliBase, Head: cliHead}, false)
		if code != 2 || errOut == "" {
			t.Fatalf("code=%d stderr=%q", code, errOut)
		}
	})
}

// TestPromotionClassifyJSONShape pins the --json envelope: exactly the
// classification, promotionOnly and reason keys, serialized with
// encoding/json.
func TestPromotionClassifyJSONShape(t *testing.T) {
	code, out, _ := runClassifyCLI(t, cliRollbackStore(), &promotion.ClassifyInput{Repo: "example/my-app", Mode: promotion.ModePush, Base: cliBase, Head: cliHead}, true)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("output is not a single JSON object: %v: %q", err, out)
	}
	if len(obj) != 3 {
		t.Fatalf("keys = %v, want exactly classification, promotionOnly, reason", obj)
	}
	for _, k := range []string{"classification", "promotionOnly", "reason"} {
		if _, ok := obj[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	var parsed classifyJSON
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Classification != "PROMOTION" || !parsed.PromotionOnly || parsed.Reason == "" {
		t.Fatalf("parsed = %+v", parsed)
	}
}
