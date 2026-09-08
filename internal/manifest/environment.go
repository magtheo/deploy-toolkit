package manifest

import (
	"fmt"
	"path"
	"strings"
)

type EnvironmentMetadata struct {
	Name string `yaml:"name" json:"name"`
}

// Automatic-rollback policy values (failurePolicy.autoRollback). Manual and
// promotion-driven recovery are separate explicit authority paths and are
// deliberately not represented here.
const (
	AutoRollbackOff      = "off"
	AutoRollbackSafeOnly = "safe-only"
)

type FailurePolicy struct {
	AutoRollback string `yaml:"autoRollback" json:"autoRollback"`
}

type EnvironmentSpec struct {
	Release       string         `yaml:"release" json:"release"`
	Target        string         `yaml:"target" json:"target"`
	FailurePolicy *FailurePolicy `yaml:"failurePolicy" json:"failurePolicy"`
}

type Environment struct {
	APIVersion string              `yaml:"apiVersion" json:"apiVersion"`
	Kind       string              `yaml:"kind" json:"kind"`
	Metadata   EnvironmentMetadata `yaml:"metadata" json:"metadata"`
	Spec       EnvironmentSpec     `yaml:"spec" json:"spec"`
}

func (e *Environment) AutoRollback() string {
	if e.Spec.FailurePolicy == nil || e.Spec.FailurePolicy.AutoRollback == "" {
		return AutoRollbackSafeOnly
	}
	return e.Spec.FailurePolicy.AutoRollback
}

func (e *Environment) check() error {
	if err := checkReleaseRef(e.Spec.Release); err != nil {
		return fmt.Errorf("spec.release: %w", err)
	}
	// Defense in depth: the schema already restricts the enum, but the
	// policy gates rollback authorization, so the parsed value is
	// validated here too. Anything unrecognized fails validation — never
	// silently treated as a permissive default.
	switch e.AutoRollback() {
	case AutoRollbackOff, AutoRollbackSafeOnly:
	default:
		return fmt.Errorf("spec.failurePolicy.autoRollback %q is not one of [%s, %s]", e.AutoRollback(), AutoRollbackOff, AutoRollbackSafeOnly)
	}
	return nil
}

func LoadEnvironment(path string) (*Environment, error) {
	res, err := loadFile(path, KindEnvironment)
	if err != nil {
		return nil, err
	}
	return res.Environment, nil
}

func checkReleaseRef(ref string) error {
	if path.Clean(ref) != ref {
		return fmt.Errorf("must be a canonical repo-root-relative path, got %q", ref)
	}
	const prefix = ".deploy/releases/"
	if !strings.HasPrefix(ref, prefix) {
		return fmt.Errorf("must resolve within %s, got %q", prefix, ref)
	}
	base := strings.TrimPrefix(ref, prefix)
	if base == "" || strings.Contains(base, "/") {
		return fmt.Errorf("release file must be a flat file name, got %q", ref)
	}
	if strings.Contains(base, "..") {
		return fmt.Errorf("path traversal is not allowed, got %q", ref)
	}
	return nil
}
