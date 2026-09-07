package release

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
)

const (
	ProjectPath = ".deploy/project.yaml"

	TrustedBranch = "main"
)

var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

type Source interface {
	BranchHead(ctx context.Context, repo, branch string) (string, error)
	VerifyCommit(ctx context.Context, repo, sha string) error
	IsAncestor(ctx context.Context, repo, ancestor, descendant string) (bool, error)
	FileAt(ctx context.Context, repo, path, ref string) ([]byte, error)
	CheckRuns(ctx context.Context, repo, ref string) ([]CheckRun, error)
}

type Resolver interface {
	Resolve(ctx context.Context, repository, tag string) (string, error)
}

type CheckRun struct {
	ID         int64
	Name       string
	Status     string
	Conclusion string
	AppID      int64
	SuiteID    int64
	StartedAt  time.Time
}

type CheckResult struct {
	Name       string
	Conclusion string
}

type ResolvedArtifact struct {
	Repository string
	Digest     string
}

type Report struct {
	Project        string
	SourceRevision string
	BranchHead     string
	Checks         []CheckResult
	Artifacts      map[string]ResolvedArtifact
	BundleDigest   string
	ContractDigest string
	BundleFiles    []string
	ReleasePath    string
	Unchanged      bool
}

func laterRun(a, b CheckRun) bool {
	if !a.StartedAt.Equal(b.StartedAt) {
		return a.StartedAt.After(b.StartedAt)
	}
	return a.ID > b.ID
}

func checkEligibility(required []string, runs []CheckRun) ([]CheckResult, error) {
	byName := make(map[string][]CheckRun)
	for _, r := range runs {
		byName[r.Name] = append(byName[r.Name], r)
	}
	results := make([]CheckResult, 0, len(required))
	for _, name := range required {
		rs := byName[name]
		if len(rs) == 0 {
			return nil, fmt.Errorf("required check %q not found on candidate (missing fails closed)", name)
		}
		current := make(map[[2]int64]CheckRun)
		for _, r := range rs {
			key := [2]int64{r.AppID, r.SuiteID}
			prev, ok := current[key]
			if !ok || laterRun(r, prev) {
				current[key] = r
			}
		}
		if len(current) > 1 {
			return nil, fmt.Errorf("required check %q is ambiguous: %d different producers currently report a check with this name", name, len(current))
		}
		for _, r := range current {
			if r.Status != "completed" {
				return nil, fmt.Errorf("required check %q has not concluded (status %q)", name, r.Status)
			}
			if r.Conclusion != "success" {
				return nil, fmt.Errorf("required check %q concluded %q (only success is eligible)", name, r.Conclusion)
			}
			results = append(results, CheckResult{Name: name, Conclusion: "success"})
		}
	}
	return results, nil
}

func loadProjects(ctx context.Context, repo, revision string, src Source) (policy, material *manifest.Project, branchHead string, err error) {
	branchHead, err = src.BranchHead(ctx, repo, TrustedBranch)
	if err != nil {
		return nil, nil, "", err
	}
	if err := src.VerifyCommit(ctx, repo, revision); err != nil {
		return nil, nil, "", err
	}
	ancestor, err := src.IsAncestor(ctx, repo, revision, branchHead)
	if err != nil {
		return nil, nil, "", err
	}
	if !ancestor {
		return nil, nil, "", fmt.Errorf("revision %s is not reachable from %s@%s (head %s)", revision, repo, TrustedBranch, branchHead)
	}
	policyBytes, err := src.FileAt(ctx, repo, ProjectPath, branchHead)
	if err != nil {
		return nil, nil, "", fmt.Errorf("read release policy: %w", err)
	}
	policyRes, err := manifest.Parse(policyBytes, manifest.KindProject)
	if err != nil {
		return nil, nil, "", fmt.Errorf("release policy (%s @ %s): %w", ProjectPath, branchHead, err)
	}
	if policyRes.Project.Release.Source.Repository != repo {
		return nil, nil, "", fmt.Errorf("release policy at %s declares source repository %q, but %s is being evaluated", branchHead, policyRes.Project.Release.Source.Repository, repo)
	}
	if policyRes.Project.Release.Source.Branch != TrustedBranch {
		return nil, nil, "", fmt.Errorf("release policy at %s declares permitted branch %q, but %s is the trusted integration branch", branchHead, policyRes.Project.Release.Source.Branch, TrustedBranch)
	}
	materialBytes, err := src.FileAt(ctx, repo, ProjectPath, revision)
	if err != nil {
		return nil, nil, "", fmt.Errorf("read deployment material: %w", err)
	}
	materialRes, err := manifest.Parse(materialBytes, manifest.KindProject)
	if err != nil {
		return nil, nil, "", fmt.Errorf("deployment material (%s @ %s): %w", ProjectPath, revision, err)
	}
	if policyRes.Project.Metadata.Name != materialRes.Project.Metadata.Name {
		return nil, nil, "", fmt.Errorf("project identity mismatch: policy at %s declares %q, candidate %s declares %q", branchHead, policyRes.Project.Metadata.Name, revision, materialRes.Project.Metadata.Name)
	}
	if policyRes.Project.Release.Source.Repository != materialRes.Project.Release.Source.Repository {
		return nil, nil, "", fmt.Errorf("source repository mismatch between policy (%q) and candidate (%q)", policyRes.Project.Release.Source.Repository, materialRes.Project.Release.Source.Repository)
	}
	return policyRes.Project, materialRes.Project, branchHead, nil
}
