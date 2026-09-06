package manifest

import (
	"fmt"
	"os"
)

type ReleaseMetadata struct {
	Project string `yaml:"project" json:"project"`
	Version string `yaml:"version" json:"version"`
}

type ReleaseSource struct {
	Repository string `yaml:"repository" json:"repository"`
	Revision   string `yaml:"revision" json:"revision"`
}

type BuiltArtifact struct {
	Type   string `yaml:"type" json:"type"`
	Image  string `yaml:"image" json:"image"`
	Digest string `yaml:"digest" json:"digest"`
}

type BundleDigest struct {
	Digest string `yaml:"digest" json:"digest"`
}

type ContractDigest struct {
	Digest string `yaml:"digest" json:"digest"`
}

type MigrationSpec struct {
	Head         string `yaml:"head" json:"head"`
	Mode         string `yaml:"mode" json:"mode"`
	RollbackSafe bool   `yaml:"rollbackSafe" json:"rollbackSafe"`
}

type Release struct {
	APIVersion         string                   `yaml:"apiVersion" json:"apiVersion"`
	Kind               string                   `yaml:"kind" json:"kind"`
	Metadata           ReleaseMetadata          `yaml:"metadata" json:"metadata"`
	Source             ReleaseSource            `yaml:"source" json:"source"`
	Artifacts          map[string]BuiltArtifact `yaml:"artifacts" json:"artifacts"`
	Bundle             BundleDigest             `yaml:"bundle" json:"bundle"`
	DeploymentContract ContractDigest           `yaml:"deploymentContract" json:"deploymentContract"`
	Migration          MigrationSpec            `yaml:"migration" json:"migration"`
}

func LoadRelease(path string) (*Release, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if _, err := Validate(data, KindRelease); err != nil {
		return nil, err
	}
	var r Release
	if err := decodeStrict(data, &r); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	return &r, nil
}
