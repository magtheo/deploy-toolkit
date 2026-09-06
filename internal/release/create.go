package release

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/magtheo/deploy-toolkit/internal/bundle"
	"github.com/magtheo/deploy-toolkit/internal/manifest"
)

type Bundler interface {
	Build(ctx context.Context, revision string, include []string) (bundle.Result, error)
}

type CreateInput struct {
	Repo          string
	Branch        string
	Revision      string
	RepoDir       string
	Version       string
	MigrationHead string
	MigrationMode string
	RollbackSafe  bool
	ReleasesDir   string
}

func (in *CreateInput) validate() error {
	if in.Repo == "" || in.Branch == "" {
		return fmt.Errorf("--repo owner/name and --branch are required")
	}
	if !revisionPattern.MatchString(in.Revision) {
		return fmt.Errorf("--revision must be a full commit SHA, got %q", in.Revision)
	}
	if in.Version == "" {
		return fmt.Errorf("--version is required")
	}
	if in.MigrationHead == "" {
		return fmt.Errorf("--migration-head is required (migration semantics are explicit inputs, never inferred)")
	}
	switch in.MigrationMode {
	case manifest.MigrationNone, manifest.MigrationForwardCompatible, manifest.MigrationMaintenanceRequired, manifest.MigrationIrreversible:
	default:
		return fmt.Errorf("--migration-mode must be one of none, forward-compatible, maintenance-required, irreversible (got %q)", in.MigrationMode)
	}
	if in.MigrationMode == manifest.MigrationIrreversible && in.RollbackSafe {
		return fmt.Errorf("--rollback-safe cannot be combined with migration mode %q", in.MigrationMode)
	}
	if in.ReleasesDir == "" {
		in.ReleasesDir = filepath.Join(".deploy", "releases")
	}
	return nil
}

func Create(ctx context.Context, in CreateInput, src Source, resolver Resolver, bundler Bundler) (*Report, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	policy, material, branchHead, err := loadProjects(ctx, in.Repo, in.Branch, in.Revision, src)
	if err != nil {
		return nil, err
	}
	runs, err := src.CheckRuns(ctx, in.Repo, in.Revision)
	if err != nil {
		return nil, err
	}
	checks, err := checkEligibility(policy.Release.RequiredChecks, runs)
	if err != nil {
		return nil, err
	}

	artifacts := make(map[string]ResolvedArtifact, len(material.Artifacts))
	for name, a := range material.Artifacts {
		digest, err := resolver.Resolve(ctx, a.Repository, in.Revision)
		if err != nil {
			return nil, fmt.Errorf("artifact %q: %w", name, err)
		}
		artifacts[name] = ResolvedArtifact{Repository: a.Repository, Digest: digest}
	}

	bres, err := bundler.Build(ctx, in.Revision, material.Bundle.Include)
	if err != nil {
		return nil, fmt.Errorf("build bundle: %w", err)
	}

	rel := &manifest.Release{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.KindRelease,
		Metadata: manifest.ReleaseMetadata{
			Project: material.Metadata.Name,
			Version: in.Version,
		},
		Source: manifest.ReleaseSource{
			Type:       manifest.SourceGitHub,
			Repository: material.Release.Source.Repository,
			Revision:   in.Revision,
		},
		Artifacts: make(map[string]manifest.BuiltArtifact, len(artifacts)),
		Bundle: manifest.BundleDigest{
			Digest: bres.Digest,
		},
		DeploymentContract: manifest.ContractDigest{
			Digest: bres.ContractDigest,
		},
		Migration: manifest.MigrationSpec{
			Head:         in.MigrationHead,
			Mode:         in.MigrationMode,
			RollbackSafe: in.RollbackSafe,
		},
	}
	for name, a := range artifacts {
		rel.Artifacts[name] = manifest.BuiltArtifact{Type: "oci", Image: a.Repository, Digest: a.Digest}
	}

	data, err := yaml.Marshal(rel)
	if err != nil {
		return nil, fmt.Errorf("render release manifest: %w", err)
	}
	if _, err := manifest.Parse(data, manifest.KindRelease); err != nil {
		return nil, fmt.Errorf("generated release manifest is not valid: %w", err)
	}

	path := filepath.Join(in.ReleasesDir, fmt.Sprintf("%s-%s.yaml", material.Metadata.Name, in.Version))
	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
		if !bytes.Equal(existing, data) {
			return nil, fmt.Errorf("release %s-%s already exists and names a different release; refusing to overwrite", material.Metadata.Name, in.Version)
		}
		return &Report{
			Project: material.Metadata.Name, SourceRevision: in.Revision, BranchHead: branchHead,
			Checks: checks, Artifacts: artifacts,
			BundleDigest: bres.Digest, ContractDigest: bres.ContractDigest, BundleFiles: bres.Files,
			ReleasePath: path, Unchanged: true,
		}, nil
	case os.IsNotExist(err):
	default:
		return nil, err
	}
	if err := os.MkdirAll(in.ReleasesDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return nil, err
	}
	return &Report{
		Project: material.Metadata.Name, SourceRevision: in.Revision, BranchHead: branchHead,
		Checks: checks, Artifacts: artifacts,
		BundleDigest: bres.Digest, ContractDigest: bres.ContractDigest, BundleFiles: bres.Files,
		ReleasePath: path,
	}, nil
}
