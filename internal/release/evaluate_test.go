package release

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/bundle"
)

const evalRev = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type evalSource struct {
	head    string
	headErr error
	anc     bool
	files   map[string]map[string][]byte
	fileErr error
	runs    []CheckRun
	runsErr error
}

func (s *evalSource) BranchHead(ctx context.Context, repo, branch string) (string, error) {
	if s.headErr != nil {
		return "", s.headErr
	}
	return s.head, nil
}

func (s *evalSource) VerifyCommit(ctx context.Context, repo, sha string) error { return nil }

func (s *evalSource) IsAncestor(ctx context.Context, repo, ancestor, descendant string) (bool, error) {
	return s.anc, nil
}

func (s *evalSource) FileAt(ctx context.Context, repo, path, ref string) ([]byte, error) {
	if s.fileErr != nil {
		return nil, s.fileErr
	}
	m, ok := s.files[ref]
	if !ok {
		return nil, fmt.Errorf("no ref %s", ref)
	}
	b, ok := m[path]
	if !ok {
		return nil, fmt.Errorf("no file %s at %s", path, ref)
	}
	return b, nil
}

func (s *evalSource) CheckRuns(ctx context.Context, repo, ref string) ([]CheckRun, error) {
	if s.runsErr != nil {
		return nil, s.runsErr
	}
	return s.runs, nil
}

type evalResolver struct{ err error }

func (r *evalResolver) Resolve(ctx context.Context, repository, tag string) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	return "sha256:" + strings.Repeat("1", 64), nil
}

type evalBundler struct{ err error }

func (b *evalBundler) Build(ctx context.Context, revision string, include []string) (bundle.Result, error) {
	if b.err != nil {
		return bundle.Result{}, b.err
	}
	return bundle.Result{Digest: "sha256:" + strings.Repeat("2", 64), ContractDigest: "sha256:" + strings.Repeat("3", 64)}, nil
}

func evalProjectDoc() []byte {
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

func evalSourceOK() *evalSource {
	doc := evalProjectDoc()
	return &evalSource{
		head: "mainhead",
		anc:  true,
		files: map[string]map[string][]byte{
			"mainhead": {ProjectPath: doc},
			evalRev:    {ProjectPath: doc},
		},
		runs: []CheckRun{
			{ID: 1, Name: "Tests", Status: "completed", Conclusion: "success", AppID: 1, SuiteID: 10, StartedAt: time.Unix(1700000000, 0)},
		},
	}
}

// TestEvaluateSeparatesInfraFromPolicy pins the structural ERROR/INVALID
// boundary: failures in dependency calls (source reads, check-runs API, OCI
// resolution, bundle construction) are *InfraError; verdicts about the
// candidate (required checks, reachability) are plain errors. Consumers
// branch on the type via errors.As, never on message text.
func TestEvaluateSeparatesInfraFromPolicy(t *testing.T) {
	in := EvalInput{Repo: "example/my-app", Revision: evalRev}
	infra := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected error")
		}
		var ie *InfraError
		if !errors.As(err, &ie) {
			t.Fatalf("error %q is not *InfraError", err)
		}
	}
	verdict := func(t *testing.T, err error, msg string) {
		t.Helper()
		if err == nil {
			t.Fatal("expected error")
		}
		var ie *InfraError
		if errors.As(err, &ie) {
			t.Fatalf("error %q must not be *InfraError", err)
		}
		if !strings.Contains(err.Error(), msg) {
			t.Fatalf("error %q does not contain %q", err, msg)
		}
	}

	t.Run("check-runs API failure is infra", func(t *testing.T) {
		src := evalSourceOK()
		src.runsErr = errors.New("502 bad gateway")
		_, err := Evaluate(context.Background(), in, src, &evalResolver{}, &evalBundler{})
		infra(t, err)
	})
	t.Run("branch head read failure is infra", func(t *testing.T) {
		src := evalSourceOK()
		src.headErr = errors.New("connection refused")
		_, err := Evaluate(context.Background(), in, src, &evalResolver{}, &evalBundler{})
		infra(t, err)
	})
	t.Run("file read failure is infra", func(t *testing.T) {
		src := evalSourceOK()
		src.fileErr = errors.New("500 internal error")
		_, err := Evaluate(context.Background(), in, src, &evalResolver{}, &evalBundler{})
		infra(t, err)
	})
	t.Run("OCI resolver failure is infra", func(t *testing.T) {
		_, err := Evaluate(context.Background(), in, evalSourceOK(), &evalResolver{err: errors.New("manifest unknown")}, &evalBundler{})
		infra(t, err)
	})
	t.Run("bundle build failure is infra", func(t *testing.T) {
		_, err := Evaluate(context.Background(), in, evalSourceOK(), &evalResolver{}, &evalBundler{err: errors.New("git object not found")})
		infra(t, err)
	})
	t.Run("successful evaluation is not an error", func(t *testing.T) {
		ev, err := Evaluate(context.Background(), in, evalSourceOK(), &evalResolver{}, &evalBundler{})
		if err != nil {
			t.Fatal(err)
		}
		if ev.BranchHead != "mainhead" || len(ev.Checks) != 1 {
			t.Fatalf("evaluation = %+v", ev)
		}
	})
	t.Run("failed required check is a policy verdict", func(t *testing.T) {
		src := evalSourceOK()
		src.runs = []CheckRun{
			{ID: 2, Name: "Tests", Status: "completed", Conclusion: "failure", AppID: 1, SuiteID: 10, StartedAt: time.Unix(1700000000, 0)},
		}
		_, err := Evaluate(context.Background(), in, src, &evalResolver{}, &evalBundler{})
		verdict(t, err, "required check")
	})
	t.Run("unreachable revision is a policy verdict", func(t *testing.T) {
		src := evalSourceOK()
		src.anc = false
		_, err := Evaluate(context.Background(), in, src, &evalResolver{}, &evalBundler{})
		verdict(t, err, "not reachable")
	})
}
