package manifest

import "fmt"

const (
	SourceGitHub = "github"
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

type ProjectSource struct {
	Type       string `yaml:"type" json:"type"`
	Repository string `yaml:"repository" json:"repository"`
	Branch     string `yaml:"branch" json:"branch"`
}

type ReleasePolicy struct {
	Source         ProjectSource `yaml:"source" json:"source"`
	RequiredChecks []string      `yaml:"requiredChecks" json:"requiredChecks"`
}

type ProjectMetadata struct {
	Name string `yaml:"name" json:"name"`
}

type Project struct {
	APIVersion string                    `yaml:"apiVersion" json:"apiVersion"`
	Kind       string                    `yaml:"kind" json:"kind"`
	Metadata   ProjectMetadata           `yaml:"metadata" json:"metadata"`
	Release    ReleasePolicy             `yaml:"release" json:"release"`
	Artifacts  map[string]ArtifactSource `yaml:"artifacts" json:"artifacts"`
	Bundle     BundleSpec                `yaml:"bundle" json:"bundle"`
	Lifecycle  Lifecycle                 `yaml:"lifecycle" json:"lifecycle"`
}

func (p *Project) check() error {
	for name, a := range p.Artifacts {
		if err := checkOCIRepository(a.Repository); err != nil {
			return fmt.Errorf("artifact %q: %w", name, err)
		}
	}
	for i, inc := range p.Bundle.Include {
		if err := checkBundleInclude(inc); err != nil {
			return fmt.Errorf("bundle.include[%d]: %w", i, err)
		}
	}
	return nil
}

func LoadProject(path string) (*Project, error) {
	res, err := loadFile(path, KindProject)
	if err != nil {
		return nil, err
	}
	return res.Project, nil
}
