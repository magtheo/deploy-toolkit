package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
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

type Parsed struct {
	Header      Header
	Project     *Project
	Release     *Release
	Environment *Environment
	Target      *Target
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

func schemaValidate(kind string, data []byte) error {
	all, err := schemasForKinds()
	if err != nil {
		return err
	}
	instance, err := yamlToInstance(data)
	if err != nil {
		return err
	}
	if err := all[schemaFile(kind)].Validate(instance); err != nil {
		return fmt.Errorf("schema validation failed for %s: %w", kind, err)
	}
	return nil
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

func decodeStrict(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return dec.Decode(out)
}

// ensureSingleDocument enforces the exact-one-document discipline every
// other layer of the toolkit already has: unknown fields are refused,
// trailing JSON after a history record is refused, extra argv is
// refused. A second YAML document after `---` would otherwise be
// silently ignored by every reader here — outside schema validation,
// strict decoding and semantic checks entirely — while a human or a
// merge diff believes it is part of the manifest. Only the first
// document is the manifest; anything else is refused.
func ensureSingleDocument(data []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var first any
	if err := dec.Decode(&first); err != nil {
		return fmt.Errorf("manifest must contain exactly one YAML document: %w", err)
	}
	var extra any
	switch err := dec.Decode(&extra); {
	case errors.Is(err, io.EOF):
		return nil
	case err != nil:
		return fmt.Errorf("manifest must contain exactly one YAML document: %w", err)
	default:
		return fmt.Errorf("manifest contains more than one YAML document; exactly one is required")
	}
}

func Parse(data []byte, wantKind string) (*Parsed, error) {
	if err := ensureSingleDocument(data); err != nil {
		return nil, err
	}
	h, err := parseHeader(data)
	if err != nil {
		return nil, err
	}
	if wantKind != "" && h.Kind != wantKind {
		return nil, fmt.Errorf("expected kind %q, found %q", wantKind, h.Kind)
	}
	if err := schemaValidate(h.Kind, data); err != nil {
		return nil, err
	}
	res := &Parsed{Header: h}
	switch h.Kind {
	case KindProject:
		var m Project
		if err := decodeStrict(data, &m); err != nil {
			return nil, fmt.Errorf("decode project: %w", err)
		}
		if err := m.check(); err != nil {
			return nil, fmt.Errorf("invalid project %q: %w", m.Metadata.Name, err)
		}
		res.Project = &m
	case KindRelease:
		var m Release
		if err := decodeStrict(data, &m); err != nil {
			return nil, fmt.Errorf("decode release: %w", err)
		}
		if err := m.check(); err != nil {
			return nil, fmt.Errorf("invalid release %s@%s: %w", m.Metadata.Project, m.Metadata.Version, err)
		}
		res.Release = &m
	case KindEnvironment:
		var m Environment
		if err := decodeStrict(data, &m); err != nil {
			return nil, fmt.Errorf("decode environment: %w", err)
		}
		if err := m.check(); err != nil {
			return nil, fmt.Errorf("invalid environment %q: %w", m.Metadata.Name, err)
		}
		res.Environment = &m
	case KindTarget:
		var m Target
		if err := decodeStrict(data, &m); err != nil {
			return nil, fmt.Errorf("decode target: %w", err)
		}
		if err := m.check(); err != nil {
			return nil, fmt.Errorf("invalid target %q: %w", m.Metadata.Name, err)
		}
		res.Target = &m
	}
	return res, nil
}

func loadFile(path string, wantKind string) (*Parsed, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data, wantKind)
}
