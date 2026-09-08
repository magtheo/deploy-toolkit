package lifecycle

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
)

// hookDefaultPATH is the complete PATH hooks receive. The hook environment
// is deterministic by construction: exactly the variables below, never the
// runner's or login user's ambient environment. Hooks that need more must
// declare it themselves or read configuration from the release.
const hookDefaultPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// hookEnv builds the exact environment a lifecycle hook runs with:
//
//	PATH                        fixed default (see hookDefaultPATH)
//	DEPLOY_PROJECT              release's project
//	DEPLOY_ENVIRONMENT          environment name
//	DEPLOY_RELEASE_VERSION      release version
//	DEPLOY_SOURCE_REVISION      pinned source revision (full SHA)
//	DEPLOY_ARTIFACT_<NAME>      <image>@<digest> per declared artifact
//
// The artifact mapping is injective: artifact names are [a-z0-9-] slugs,
// and uppercasing with '-'→'_' cannot collide. Hooks are pinned to the
// exact Release artifacts without copying the Release manifest into the
// bundle.
func hookEnv(rel *manifest.Release, environment string) (map[string]string, error) {
	env := map[string]string{
		"PATH":                   hookDefaultPATH,
		"DEPLOY_PROJECT":         rel.Metadata.Project,
		"DEPLOY_ENVIRONMENT":     environment,
		"DEPLOY_RELEASE_VERSION": rel.Metadata.Version,
		"DEPLOY_SOURCE_REVISION": rel.Source.Revision,
	}
	for name, a := range rel.Artifacts {
		key, err := artifactEnvName(name)
		if err != nil {
			return nil, err
		}
		env[key] = a.Image + "@" + a.Digest
	}
	return env, nil
}

func artifactEnvName(name string) (string, error) {
	// Names arrive schema-validated, but this path derives environment
	// variable syntax from them — validate locally too.
	if name == "" {
		return "", fmt.Errorf("artifact name must not be empty")
	}
	var b strings.Builder
	b.WriteString("DEPLOY_ARTIFACT_")
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r - ('a' - 'A'))
		case r == '-':
			b.WriteByte('_')
		default:
			return "", fmt.Errorf("artifact name %q: only [a-z0-9-] map to hook environment variables", name)
		}
	}
	return b.String(), nil
}

func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("sha256:%064x", sum)
}

func firstLine(b []byte) string {
	for i, c := range b {
		if c == '\n' {
			return string(b[:i])
		}
	}
	return string(b)
}
