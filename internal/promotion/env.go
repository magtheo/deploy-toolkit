package promotion

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
)

func SetRelease(raw []byte, newRef string) ([]byte, error) {
	if _, err := manifest.Parse(raw, manifest.KindEnvironment); err != nil {
		return nil, fmt.Errorf("environment document is not valid: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("environment root is not a mapping")
	}
	spec := mappingValue(root, "spec")
	if spec == nil {
		return nil, fmt.Errorf("environment has no spec mapping")
	}
	rel := mappingValue(spec, "release")
	if rel == nil {
		return nil, fmt.Errorf("environment spec has no release key")
	}
	if rel.Kind != yaml.ScalarNode {
		return nil, fmt.Errorf("spec.release is not a scalar")
	}
	rel.Tag = "!!str"
	rel.Value = newRef
	rel.Style = 0

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if err := onlyReleaseChanged(raw, out, newRef); err != nil {
		return nil, err
	}
	return out, nil
}

func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func onlyReleaseChanged(before, after []byte, want string) error {
	a, err := manifest.Parse(before, manifest.KindEnvironment)
	if err != nil {
		return err
	}
	b, err := manifest.Parse(after, manifest.KindEnvironment)
	if err != nil {
		return fmt.Errorf("edited environment is not valid: %w", err)
	}
	ae, be := a.Environment, b.Environment
	switch {
	case ae.Metadata != be.Metadata:
		return fmt.Errorf("environment metadata must not change during promotion")
	case ae.Spec.Target != be.Spec.Target:
		return fmt.Errorf("environment spec.target must not change during promotion")
	case ae.AutoRollback() != be.AutoRollback():
		return fmt.Errorf("environment failurePolicy must not change during promotion")
	case be.Spec.Release != want:
		return fmt.Errorf("edited environment does not point at %q", want)
	}
	return nil
}
