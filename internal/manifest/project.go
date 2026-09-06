package manifest

import (
	"fmt"
	"os"
)

type ArtifactSource struct {
	Type       string `yaml:"type" json:"type"`
	Repository string `yaml:"repository" json:"repository"`
}

type BundleSpec struct {
	Include []string `yaml:"include" json:"include"`
}

type LifecycleStep struct {
	Argv []string `yaml:"argv" json:"argv"`
}

type Lifecycle struct {
	Preflight *LifecycleStep `yaml:"preflight" json:"preflight"`
	Migrate   *LifecycleStep `yaml:"migrate" json:"migrate"`
	Apply     *LifecycleStep `yaml:"apply" json:"apply"`
	Verify    *LifecycleStep `yaml:"verify" json:"verify"`
	Rollback  *LifecycleStep `yaml:"rollback" json:"rollback"`
}

type ReleasePolicy struct {
	Source         ProjectSource `yaml:"source" json:"source"`
	RequiredChecks []string      `yaml:"requiredChecks" json:"requiredChecks"`
}

type ProjectSource struct {
	Branch string `yaml:"branch" json:"branch"`
}

type ProjectMetadata struct {
	Name string `yaml:"name" json:"name"`
}

type Project struct {
	APIVersion string                    `yaml:"apiVersion" json:"apiVersion"`
	Kind       string                    `yaml:"kind" json:"kind"`
	Metadata   ProjectMetadata           `yaml:"metadata" json:"metadata"`
	Release    *ReleasePolicy            `yaml:"release" json:"release"`
	Artifacts  map[string]ArtifactSource `yaml:"artifacts" json:"artifacts"`
	Bundle     *BundleSpec               `yaml:"bundle" json:"bundle"`
	Lifecycle  Lifecycle                 `yaml:"lifecycle" json:"lifecycle"`
}

func LoadProject(path string) (*Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if _, err := Validate(data, KindProject); err != nil {
		return nil, err
	}
	var p Project
	if err := decodeStrict(data, &p); err != nil {
		return nil, fmt.Errorf("decode project: %w", err)
	}
	return &p, nil
}
