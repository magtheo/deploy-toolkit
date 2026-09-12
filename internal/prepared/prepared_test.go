package prepared

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The manifests below are minimal valid v1 manifests; the bundle bytes
// are opaque to this package (it only verifies the digest web), so a
// pinned digest of arbitrary bytes is enough to exercise every rule.

const envYAML = `apiVersion: deploy.toolkit/v1
kind: Environment
metadata:
  name: production
spec:
  release: .deploy/releases/my-app-1.0.0.yaml
  target: local-dev
  failurePolicy:
    autoRollback: safe-only
`

const targetYAML = `apiVersion: deploy.toolkit/v1
kind: Target
metadata:
  name: local-dev
spec:
  deployRoot: /srv/my-app
  transport:
    type: local
`

func releaseYAML(bundleDigest, contractDigest string) string {
	return `apiVersion: deploy.toolkit/v1
kind: Release
metadata:
  project: my-app
  version: 1.0.0
source:
  type: github
  repository: example/my-app
  revision: "0123456789abcdef0123456789abcdef01234567"
artifacts:
  app:
    type: oci
    image: ghcr.io/example/app
    digest: sha256:` + strings.Repeat("ab", 32) + `
bundle:
  digest: ` + bundleDigest + `
deploymentContract:
  digest: ` + contractDigest + `
migration:
  head: "001"
  mode: forward-compatible
  rollbackSafe: true
`
}

func sha256Of(b []byte) string {
	return "sha256:" + strings.ToLower(fmt.Sprintf("%x", sha256.Sum256(b)))
}

func fixture() (releaseBytes, envBytes, targetBytes, bundle []byte) {
	bundle = []byte("canonical bundle bytes")
	return []byte(releaseYAML(sha256Of(bundle), "sha256:"+strings.Repeat("cd", 32))),
		[]byte(envYAML), []byte(targetYAML), bundle
}

func TestPrepareLoadRoundtrip(t *testing.T) {
	releaseBytes, envBytes, targetBytes, bundleBytes := fixture()
	dir := filepath.Join(t.TempDir(), "prepared")

	m, err := Prepare(dir, releaseBytes, envBytes, targetBytes, bundleBytes, true)
	if err != nil {
		t.Fatal(err)
	}
	if m.Project != "my-app" || m.Version != "1.0.0" || m.Environment != "production" || m.Target != "local-dev" {
		t.Errorf("manifest identity = %s@%s %s/%s", m.Project, m.Version, m.Environment, m.Target)
	}
	if m.BundleDigest != sha256Of(bundleBytes) {
		t.Errorf("manifest bundle digest = %s", m.BundleDigest)
	}

	art, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(art.Bundle) != string(bundleBytes) {
		t.Error("bundle bytes did not survive the roundtrip")
	}
	if art.Release.Metadata.Version != "1.0.0" || art.Environment.Metadata.Name != "production" || art.Target.Metadata.Name != "local-dev" {
		t.Errorf("parsed identity mismatch: %v/%v/%v", art.Release.Metadata, art.Environment.Metadata, art.Target.Metadata)
	}

	// LoadForDeploy accepts material whose environment pins this release.
	if _, err := LoadForDeploy(dir); err != nil {
		t.Errorf("LoadForDeploy refused deploy material: %v", err)
	}

	// Deterministic: re-preparing identical material is byte-identical.
	dir2 := filepath.Join(t.TempDir(), "prepared")
	if _, err := Prepare(dir2, releaseBytes, envBytes, targetBytes, bundleBytes, true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{FileManifest, FileRelease, FileEnvironment, FileTarget, FileBundle} {
		a, errA := os.ReadFile(filepath.Join(dir, name))
		b, errB := os.ReadFile(filepath.Join(dir2, name))
		if errA != nil || errB != nil || string(a) != string(b) {
			t.Errorf("member %s is not deterministic across prepares", name)
		}
	}
}

// Re-preparing identical material is idempotent; re-preparing different
// material over an existing artifact is refused — prepared material is
// immutable.
func TestPrepareImmutability(t *testing.T) {
	releaseBytes, envBytes, targetBytes, bundleBytes := fixture()
	dir := filepath.Join(t.TempDir(), "prepared")
	if _, err := Prepare(dir, releaseBytes, envBytes, targetBytes, bundleBytes, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(dir, releaseBytes, envBytes, targetBytes, bundleBytes, true); err != nil {
		t.Errorf("identical re-prepare refused: %v", err)
	}
	_, _, _, otherBundle := fixture()
	otherBundle = []byte("different bundle bytes")
	relOther := []byte(releaseYAML(sha256Of(otherBundle), "sha256:"+strings.Repeat("cd", 32)))
	if _, err := Prepare(dir, relOther, envBytes, targetBytes, otherBundle, true); err == nil {
		t.Error("overwriting an existing artifact with different material succeeded")
	}
}

// Every member is digest-bound: flipping one byte anywhere fails closed.
func TestTamperedMemberRefused(t *testing.T) {
	releaseBytes, envBytes, targetBytes, bundleBytes := fixture()
	for name, mutate := range map[string]func([]byte) []byte{
		FileRelease:     func(b []byte) []byte { return append([]byte{}, b[:10]...) },
		FileEnvironment: func(b []byte) []byte { c := append([]byte{}, b...); c[5] ^= 1; return c },
		FileTarget:      func(b []byte) []byte { c := append([]byte{}, b...); c[5] ^= 1; return c },
		FileBundle:      func(b []byte) []byte { return append(b, 'x') },
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "prepared")
			if _, err := Prepare(dir, releaseBytes, envBytes, targetBytes, bundleBytes, true); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(dir, name)
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, mutate(raw), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(dir); !errors.Is(err, ErrNotVerifiable) {
				t.Errorf("tampered %s: err = %v, want ErrNotVerifiable", name, err)
			}
		})
	}
}

// A truncated artifact (missing member) fails closed.
func TestTruncatedArtifactRefused(t *testing.T) {
	releaseBytes, envBytes, targetBytes, bundleBytes := fixture()
	dir := filepath.Join(t.TempDir(), "prepared")
	if _, err := Prepare(dir, releaseBytes, envBytes, targetBytes, bundleBytes, true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{FileManifest, FileRelease, FileEnvironment, FileTarget, FileBundle} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); !errors.Is(err, ErrNotVerifiable) {
			t.Errorf("missing %s: err = %v, want ErrNotVerifiable", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(releaseYAML("sha256:x", "sha256:y")), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// An unknown schema version fails closed.
func TestUnknownSchemaRefused(t *testing.T) {
	releaseBytes, envBytes, targetBytes, bundleBytes := fixture()
	dir := filepath.Join(t.TempDir(), "prepared")
	if _, err := Prepare(dir, releaseBytes, envBytes, targetBytes, bundleBytes, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, FileManifest))
	if err != nil {
		t.Fatal(err)
	}
	mutated := strings.Replace(string(raw), SchemaV1, "prepared.deployment/v2", 1)
	if err := os.WriteFile(filepath.Join(dir, FileManifest), []byte(mutated), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); !errors.Is(err, ErrNotVerifiable) {
		t.Errorf("unknown schema: err = %v, want ErrNotVerifiable", err)
	}
}

// The environment must point at the target the artifact carries.
func TestCrossTargetMismatchRefused(t *testing.T) {
	releaseBytes, envBytes, _, bundleBytes := fixture()
	otherTarget := strings.Replace(targetYAML, "local-dev", "other-box", 1)
	dir := filepath.Join(t.TempDir(), "prepared")
	if _, err := Prepare(dir, releaseBytes, envBytes, []byte(otherTarget), bundleBytes, true); err == nil {
		t.Fatal("prepare accepted an environment pointing at a different target")
	}
}

// Strict pin: deploy material must carry the release the environment
// pins. Rollback material (non-strict) may not, but LoadForDeploy then
// refuses it — the deploy side can never mistake it for deploy material.
func TestStrictReleasePin(t *testing.T) {
	releaseBytes, envBytes, targetBytes, bundleBytes := fixture()
	// A release the environment does NOT pin: same project, other version.
	otherRelease := strings.Replace(string(releaseBytes), "version: 1.0.0", "version: 0.9.0", 1)

	dir := filepath.Join(t.TempDir(), "deploy")
	if _, err := Prepare(dir, releaseBytes, envBytes, targetBytes, bundleBytes, true); err != nil {
		t.Fatalf("strict prepare of pinned release refused: %v", err)
	}

	dirRollback := filepath.Join(t.TempDir(), "rollback")
	if _, err := Prepare(dirRollback, []byte(otherRelease), envBytes, targetBytes, bundleBytes, false); err != nil {
		t.Fatalf("non-strict prepare of rollback material refused: %v", err)
	}
	// Load is pair-level (rollback may legitimately differ)…
	if _, err := Load(dirRollback); err != nil {
		t.Errorf("Load refused rollback material: %v", err)
	}
	// …but the deploy entry point refuses it.
	if _, err := LoadForDeploy(dirRollback); !errors.Is(err, ErrNotVerifiable) {
		t.Errorf("LoadForDeploy accepted rollback material: err = %v", err)
	}
	// And prepare refuses to write deploy material against an unpinned release.
	dirStrict := filepath.Join(t.TempDir(), "strict")
	if _, err := Prepare(dirStrict, []byte(otherRelease), envBytes, targetBytes, bundleBytes, true); !errors.Is(err, ErrNotVerifiable) {
		t.Errorf("strict prepare accepted an unpinned release: err = %v", err)
	}
}

// Prepared artifacts are non-secret by construction: private-key
// material in any member is refused before anything is written.
func TestSecretMaterialRefused(t *testing.T) {
	releaseBytes, envBytes, _, bundleBytes := fixture()
	// The PEM marker hides in a YAML comment: schema-valid, decodes
	// strictly — only the secret scan can catch it.
	hostile := targetYAML + "  # -----BEGIN OPENSSH PRIVATE KEY-----b3BlbnNzaC1rZXk\n"
	dir := filepath.Join(t.TempDir(), "prepared")
	if _, err := Prepare(dir, releaseBytes, envBytes, []byte(hostile), bundleBytes, true); !errors.Is(err, ErrNotVerifiable) {
		t.Fatalf("hostile target manifest: err = %v, want ErrNotVerifiable", err)
	}
	entries, err := os.ReadDir(dir)
	if err == nil && len(entries) > 0 {
		t.Errorf("secret-bearing artifact wrote %d files before refusing", len(entries))
	}
}
