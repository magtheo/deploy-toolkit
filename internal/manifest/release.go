package manifest

import (
	"fmt"
)

const (
	MigrationNone                = "none"
	MigrationForwardCompatible   = "forward-compatible"
	MigrationMaintenanceRequired = "maintenance-required"
	MigrationIrreversible        = "irreversible"
)

type ReleaseMetadata struct {
	Project string `yaml:"project" json:"project"`
	Version string `yaml:"version" json:"version"`
}

type ReleaseSource struct {
	Type       string `yaml:"type" json:"type"`
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

func (r *Release) check() error {
	if r.Migration.Mode == MigrationIrreversible && r.Migration.RollbackSafe {
		return fmt.Errorf("migration mode %q cannot have rollbackSafe: true", MigrationIrreversible)
	}
	for name, a := range r.Artifacts {
		if err := checkOCIRepository(a.Image); err != nil {
			return fmt.Errorf("artifact %q: %w", name, err)
		}
	}
	return nil
}

func LoadRelease(path string) (*Release, error) {
	res, err := loadFile(path, KindRelease)
	if err != nil {
		return nil, err
	}
	return res.Release, nil
}
