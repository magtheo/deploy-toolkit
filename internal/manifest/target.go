package manifest

import (
	"fmt"
	"os"
)

type TargetMetadata struct {
	Name string `yaml:"name" json:"name"`
}

type Transport struct {
	Type           string `yaml:"type" json:"type"`
	HostFrom       string `yaml:"hostFrom" json:"hostFrom"`
	Port           int    `yaml:"port" json:"port"`
	User           string `yaml:"user" json:"user"`
	HostKeyFrom    string `yaml:"hostKeyFrom" json:"hostKeyFrom"`
	CredentialFrom string `yaml:"credentialFrom" json:"credentialFrom"`
}

func (t Transport) EffectivePort() int {
	if t.Port == 0 {
		return 22
	}
	return t.Port
}

type TargetSpec struct {
	Transport Transport `yaml:"transport" json:"transport"`
}

type Target struct {
	APIVersion string         `yaml:"apiVersion" json:"apiVersion"`
	Kind       string         `yaml:"kind" json:"kind"`
	Metadata   TargetMetadata `yaml:"metadata" json:"metadata"`
	Spec       TargetSpec     `yaml:"spec" json:"spec"`
}

func LoadTarget(path string) (*Target, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if _, err := Validate(data, KindTarget); err != nil {
		return nil, err
	}
	var t Target
	if err := decodeStrict(data, &t); err != nil {
		return nil, fmt.Errorf("decode target: %w", err)
	}
	return &t, nil
}
