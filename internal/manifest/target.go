package manifest

import (
	"fmt"
	"strings"
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
	Transport  Transport `yaml:"transport" json:"transport"`
	DeployRoot string    `yaml:"deployRoot" json:"deployRoot"`
}

type Target struct {
	APIVersion string         `yaml:"apiVersion" json:"apiVersion"`
	Kind       string         `yaml:"kind" json:"kind"`
	Metadata   TargetMetadata `yaml:"metadata" json:"metadata"`
	Spec       TargetSpec     `yaml:"spec" json:"spec"`
}

func (t *Target) check() error {
	if t.Spec.DeployRoot == "" {
		return fmt.Errorf("spec.deployRoot is required")
	}
	if !strings.HasPrefix(t.Spec.DeployRoot, "/") {
		return fmt.Errorf("spec.deployRoot %q must be an absolute path", t.Spec.DeployRoot)
	}
	for _, seg := range strings.Split(strings.Trim(t.Spec.DeployRoot, "/"), "/") {
		if seg == ".." || seg == "." || seg == "" {
			return fmt.Errorf("spec.deployRoot %q must not contain %q path segments", t.Spec.DeployRoot, seg)
		}
	}
	if t.Spec.Transport.Type == TransportSSH && t.Spec.Transport.HostKeyFrom == "" {
		return fmt.Errorf("ssh transport requires a pinned host key (hostKeyFrom)")
	}
	return nil
}

func LoadTarget(path string) (*Target, error) {
	res, err := loadFile(path, KindTarget)
	if err != nil {
		return nil, err
	}
	return res.Target, nil
}
