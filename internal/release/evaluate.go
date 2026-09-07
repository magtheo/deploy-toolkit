package release

import (
	"context"
	"fmt"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/magtheo/deploy-toolkit/internal/bundle"
	"github.com/magtheo/deploy-toolkit/internal/manifest"
)

type Bundler interface {
	Build(ctx context.Context, revision string, include []string) (bundle.Result, error)
}

type EvalInput struct {
	Repo     string
	Revision string
}

type Evaluation struct {
	Repo           string
	Revision       string
	BranchHead     string
	Policy         *manifest.Project
	Material       *manifest.Project
	Checks         []CheckResult
	Artifacts      map[string]ResolvedArtifact
	BundleDigest   string
	ContractDigest string
	BundleFiles    []string
}

func Evaluate(ctx context.Context, in EvalInput, src Source, resolver Resolver, bundler Bundler) (*Evaluation, error) {
	policy, material, branchHead, err := loadProjects(ctx, in.Repo, in.Revision, src)
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
	return &Evaluation{
		Repo: in.Repo, Revision: in.Revision, BranchHead: branchHead,
		Policy: policy, Material: material,
		Checks: checks, Artifacts: artifacts,
		BundleDigest: bres.Digest, ContractDigest: bres.ContractDigest, BundleFiles: bres.Files,
	}, nil
}

func ReleaseFilePath(dir, project, version string) string {
	return filepath.Join(dir, fmt.Sprintf("%s-%s.yaml", project, version))
}

func ReleaseFileName(project, version string) string {
	return fmt.Sprintf("%s-%s.yaml", project, version)
}

func RenderManifest(rel *manifest.Release) ([]byte, error) {
	data, err := yaml.Marshal(rel)
	if err != nil {
		return nil, fmt.Errorf("render release manifest: %w", err)
	}
	if _, err := manifest.Parse(data, manifest.KindRelease); err != nil {
		return nil, fmt.Errorf("generated release manifest is not valid: %w", err)
	}
	return data, nil
}

func RenderReleaseFor(ev *Evaluation, version string, migration manifest.MigrationSpec) ([]byte, error) {
	rel := &manifest.Release{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.KindRelease,
		Metadata: manifest.ReleaseMetadata{
			Project: ev.Material.Metadata.Name,
			Version: version,
		},
		Source: manifest.ReleaseSource{
			Type:       manifest.SourceGitHub,
			Repository: ev.Material.Release.Source.Repository,
			Revision:   ev.Revision,
		},
		Artifacts: make(map[string]manifest.BuiltArtifact, len(ev.Artifacts)),
		Bundle: manifest.BundleDigest{
			Digest: ev.BundleDigest,
		},
		DeploymentContract: manifest.ContractDigest{
			Digest: ev.ContractDigest,
		},
		Migration: migration,
	}
	for name, a := range ev.Artifacts {
		rel.Artifacts[name] = manifest.BuiltArtifact{Type: "oci", Image: a.Repository, Digest: a.Digest}
	}
	return RenderManifest(rel)
}
