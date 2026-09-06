package manifest

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/magtheo/deploy-toolkit/schemas"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

const (
	APIVersion = "deploy.toolkit/v1"

	KindProject     = "Project"
	KindRelease     = "Release"
	KindEnvironment = "Environment"
	KindTarget      = "Target"
)

type Header struct {
	APIVersion string `yaml:"apiVersion" json:"apiVersion"`
	Kind       string `yaml:"kind"       json:"kind"`
}

func schemaFile(kind string) string {
	switch kind {
	case KindProject:
		return "project.schema.json"
	case KindRelease:
		return "release.schema.json"
	case KindEnvironment:
		return "environment.schema.json"
	case KindTarget:
		return "target.schema.json"
	default:
		return ""
	}
}

var (
	compileOnce sync.Once
	compiled    map[string]*jsonschema.Schema
	compileErr  error
)

func schemasForKinds() (map[string]*jsonschema.Schema, error) {
	compileOnce.Do(func() {
		raw, err := schemas.Raw()
		if err != nil {
			compileErr = err
			return
		}
		compiled = make(map[string]*jsonschema.Schema, len(raw))
		for name, data := range raw {
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
			if err != nil {
				compileErr = fmt.Errorf("schema %s: %w", name, err)
				return
			}
			compiler := jsonschema.NewCompiler()
			compiler.AddResource(name, doc)
			sch, err := compiler.Compile(name)
			if err != nil {
				compileErr = fmt.Errorf("schema %s: %w", name, err)
				return
			}
			compiled[name] = sch
		}
	})
	return compiled, compileErr
}

func parseHeader(data []byte) (Header, error) {
	var h Header
	if err := yaml.Unmarshal(data, &h); err != nil {
		return h, fmt.Errorf("parse header: %w", err)
	}
	if h.APIVersion != APIVersion {
		return h, fmt.Errorf("unsupported apiVersion %q (want %q)", h.APIVersion, APIVersion)
	}
	if schemaFile(h.Kind) == "" {
		return h, fmt.Errorf("unknown kind %q", h.Kind)
	}
	return h, nil
}

func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = normalize(e)
		}
		return t
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, e := range t {
			m[fmt.Sprint(k)] = normalize(e)
		}
		return m
	case []any:
		for i, e := range t {
			t[i] = normalize(e)
		}
		return t
	default:
		return v
	}
}

func yamlToInstance(data []byte) (any, error) {
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	return normalize(v), nil
}

func Validate(data []byte, wantKind string) (Header, error) {
	h, err := parseHeader(data)
	if err != nil {
		return h, err
	}
	if wantKind != "" && h.Kind != wantKind {
		return h, fmt.Errorf("expected kind %q, found %q", wantKind, h.Kind)
	}
	all, err := schemasForKinds()
	if err != nil {
		return h, err
	}
	sch := all[schemaFile(h.Kind)]
	instance, err := yamlToInstance(data)
	if err != nil {
		return h, err
	}
	if err := sch.Validate(instance); err != nil {
		return h, fmt.Errorf("schema validation failed for %s: %w", h.Kind, err)
	}
	return h, nil
}

func decodeStrict(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return dec.Decode(out)
}
