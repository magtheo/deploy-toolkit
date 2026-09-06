package manifest

import (
	"fmt"
	"os"
)

type EnvironmentMetadata struct {
	Name string `yaml:"name" json:"name"`
}

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
		return "safe-only"
	}
	return e.Spec.FailurePolicy.AutoRollback
}

func LoadEnvironment(path string) (*Environment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if _, err := Validate(data, KindEnvironment); err != nil {
		return nil, err
	}
	var env Environment
	if err := decodeStrict(data, &env); err != nil {
		return nil, fmt.Errorf("decode environment: %w", err)
	}
	return &env, nil
}
