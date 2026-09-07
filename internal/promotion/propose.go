package promotion

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/release"
)

const (
	ReleasesDir     = ".deploy/releases"
	EnvironmentsDir = ".deploy/environments"
	BranchPrefixFmt = "deploy/promote-%s-"
)

type TreeEntry struct {
	Path    string
	Content []byte
}

type ChangedFile struct {
	Path    string
	Status  string
	BlobSHA string
}

type PullRequest struct {
	Number  int
	URL     string
	HeadRef string
}

type Store interface {
	BranchHead(ctx context.Context, repo, branch string) (string, error)
	VerifyCommit(ctx context.Context, repo, sha string) error
	IsAncestor(ctx context.Context, repo, ancestor, descendant string) (bool, error)
	FileAt(ctx context.Context, repo, path, ref string) ([]byte, error)
	CheckRuns(ctx context.Context, repo, ref string) ([]release.CheckRun, error)

	HeadTree(ctx context.Context, repo, commitSHA string) (string, error)
	CreateBlob(ctx context.Context, repo string, content []byte) (string, error)
	CreateTree(ctx context.Context, repo, baseTree string, entries []TreeEntry) (string, error)
	CreateCommit(ctx context.Context, repo, message, treeSHA string, parents []string) (string, error)
	CreateBranch(ctx context.Context, repo, branch, sha string) error
	BranchExists(ctx context.Context, repo, branch string) (bool, error)
	OpenPRForBranch(ctx context.Context, repo, branch string) (*PullRequest, error)
	OpenPromotionPRs(ctx context.Context, repo, env string) ([]PullRequest, error)
	CreatePR(ctx context.Context, repo, base, head, title, body string) (*PullRequest, error)

	CompareFiles(ctx context.Context, repo, base, head string) ([]ChangedFile, error)
	BlobAt(ctx context.Context, repo, blobSHA string) ([]byte, error)
}

type ProposeInput struct {
	Repo        string
	Environment string
	ReleasePath string
	RepoDir     string
}

type Proposal struct {
	Project     string
	Environment string
	From        string
	To          string
	BaseSHA     string
	Branch      string
	PR          *PullRequest
	Unchanged   bool
	Report      *release.Report
}

func Propose(ctx context.Context, in ProposeInput, src Store, resolver release.Resolver, bundler release.Bundler) (*Proposal, error) {
	if in.Repo == "" || in.Environment == "" || in.ReleasePath == "" {
		return nil, fmt.Errorf("--repo, <environment> and --release are required")
	}
	relPath := filepath.ToSlash(filepath.Clean(in.ReleasePath))
	relBytes, err := os.ReadFile(relPath)
	if err != nil {
		return nil, fmt.Errorf("read release manifest: %w", err)
	}
	relRes, err := manifest.Parse(relBytes, manifest.KindRelease)
	if err != nil {
		return nil, err
	}
	rel := relRes.Release
	if rel.Source.Repository != in.Repo {
		return nil, fmt.Errorf("release %s declares source repository %q, but %s is being promoted", relPath, rel.Source.Repository, in.Repo)
	}
	project := rel.Metadata.Project
	version := rel.Metadata.Version
	wantName := release.ReleaseFileName(project, version)
	if filepath.Base(relPath) != wantName {
		return nil, fmt.Errorf("release file %s must be named %s (project and version must agree with the manifest)", relPath, wantName)
	}
	toRef := release.ReleaseFilePath(ReleasesDir, project, version)

	envPath := fmt.Sprintf("%s/%s.yaml", EnvironmentsDir, in.Environment)
	baseSHA, err := src.BranchHead(ctx, in.Repo, release.TrustedBranch)
	if err != nil {
		return nil, err
	}
	envBytes, err := src.FileAt(ctx, in.Repo, envPath, baseSHA)
	if err != nil {
		return nil, fmt.Errorf("read environment %s from %s@%s: %w", envPath, in.Repo, baseSHA, err)
	}
	envRes, err := manifest.Parse(envBytes, manifest.KindEnvironment)
	if err != nil {
		return nil, fmt.Errorf("environment at %s: %w", baseSHA, err)
	}
	env := envRes.Environment
	if env.Metadata.Name != in.Environment {
		return nil, fmt.Errorf("environment file declares name %q, promoting %q", env.Metadata.Name, in.Environment)
	}
	if env.Spec.Release == toRef {
		return nil, fmt.Errorf("environment %s already points at %s; proposal would be a no-op", in.Environment, toRef)
	}
	fromRef := env.Spec.Release

	ev, err := release.Evaluate(ctx, release.EvalInput{Repo: in.Repo, Revision: rel.Source.Revision}, src, resolver, bundler)
	if err != nil {
		return nil, fmt.Errorf("release %s is not eligible under current policy: %w", version, err)
	}
	expected, err := release.RenderReleaseFor(ev, version, rel.Migration)
	if err != nil {
		return nil, err
	}
	if string(expected) != string(relBytes) {
		return nil, fmt.Errorf("release file %s is not exactly what current deterministic eligibility would generate; refusing to promote a hand-tampered manifest", relPath)
	}

	branch := fmt.Sprintf(BranchPrefixFmt+"%s", in.Environment, version)
	exists, err := src.BranchExists(ctx, in.Repo, branch)
	if err != nil {
		return nil, err
	}
	if exists {
		pr, err := src.OpenPRForBranch(ctx, in.Repo, branch)
		if err != nil {
			return nil, err
		}
		if pr != nil {
			return &Proposal{Project: project, Environment: in.Environment, From: fromRef, To: toRef, BaseSHA: baseSHA, Branch: branch, PR: pr, Unchanged: true}, nil
		}
		for i := 2; ; i++ {
			candidate := fmt.Sprintf("%s-%d", branch, i)
			free, err := src.BranchExists(ctx, in.Repo, candidate)
			if err != nil {
				return nil, err
			}
			if !free {
				branch = candidate
				break
			}
		}
	}
	conflicts, err := src.OpenPromotionPRs(ctx, in.Repo, in.Environment)
	if err != nil {
		return nil, err
	}
	for _, pr := range conflicts {
		if pr.HeadRef != branch {
			return nil, fmt.Errorf("conflicting open promotion for environment %s: %s (#%d) must be resolved first", in.Environment, pr.URL, pr.Number)
		}
	}

	newEnv, err := SetRelease(envBytes, toRef)
	if err != nil {
		return nil, err
	}
	oldRel := oldReleaseSummary(ctx, src, in.Repo, baseSHA, fromRef)

	entries := []TreeEntry{
		{Path: toRef, Content: relBytes},
		{Path: envPath, Content: newEnv},
	}
	baseTree, err := src.HeadTree(ctx, in.Repo, baseSHA)
	if err != nil {
		return nil, err
	}
	treeSHA, err := src.CreateTree(ctx, in.Repo, baseTree, entries)
	if err != nil {
		return nil, err
	}
	title := fmt.Sprintf("deploy: promote %s %s to %s", project, version, in.Environment)
	body := RenderBody(BodyInput{
		Project: project, Version: version, Environment: in.Environment,
		BaseSHA: baseSHA, BranchHead: ev.BranchHead, Revision: rel.Source.Revision,
		From: fromRef, To: toRef, OldRelease: oldRel,
		Evaluation: ev, Migration: rel.Migration,
	})
	commitSHA, err := src.CreateCommit(ctx, in.Repo, title, treeSHA, []string{baseSHA})
	if err != nil {
		return nil, err
	}
	if err := src.CreateBranch(ctx, in.Repo, branch, commitSHA); err != nil {
		return nil, err
	}
	pr, err := src.CreatePR(ctx, in.Repo, release.TrustedBranch, branch, title, body)
	if err != nil {
		return nil, err
	}
	return &Proposal{
		Project: project, Environment: in.Environment, From: fromRef, To: toRef,
		BaseSHA: baseSHA, Branch: branch, PR: pr,
		Report: &release.Report{
			Project: project, SourceRevision: rel.Source.Revision, BranchHead: ev.BranchHead,
			Checks: ev.Checks, Artifacts: ev.Artifacts,
			BundleDigest: ev.BundleDigest, ContractDigest: ev.ContractDigest, BundleFiles: ev.BundleFiles,
		},
	}, nil
}

func oldReleaseSummary(ctx context.Context, src Store, repo, ref, releaseRef string) *manifest.Release {
	if releaseRef == "" {
		return nil
	}
	data, err := src.FileAt(ctx, repo, releaseRef, ref)
	if err != nil {
		return nil
	}
	res, err := manifest.Parse(data, manifest.KindRelease)
	if err != nil {
		return nil
	}
	return res.Release
}
