// Package prepared implements the prepared deployment artifact — the
// immutable, verifiable, secret-free material boundary between the
// prepare stage (repository authority, zero target credential) and the
// deploy stage (target credential, zero source access).
//
// The deploy side reconstructs nothing from Git: Load re-parses every
// member through the one manifest validation pipeline and verifies the
// full integrity web in docs/prepared-artifact-v1.md before the caller
// is allowed to contact a target. Every failure is fail-closed.
package prepared

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
)

// SchemaV1 is the schema identifier of the v1 integrity manifest.
const SchemaV1 = "prepared.deployment/v1"

// The five members of a prepared artifact directory.
const (
	FileManifest    = "prepared.json"
	FileRelease     = "release.yaml"
	FileEnvironment = "environment.yaml"
	FileTarget      = "target.yaml"
	FileBundle      = "bundle.tar"
)

// ErrNotVerifiable marks prepared material that failed verification:
// the artifact exists but its facts cannot be trusted, so nothing may
// be contacted or executed. It is a refusal, not an infrastructure
// error — retrying cannot repair it.
var ErrNotVerifiable = errors.New("prepared material does not verify")

// Digests binds the exact bytes of every artifact member. Each digest
// is "sha256:<hex>".
type Digests struct {
	Release     string `json:"release"`
	Environment string `json:"environment"`
	Target      string `json:"target"`
	Bundle      string `json:"bundle"`
}

// Manifest is the integrity manifest (prepared.json): identity plus the
// closed digest web over the other four members. Deterministic — fixed
// key order, no timestamps.
type Manifest struct {
	Schema         string  `json:"schema"`
	Project        string  `json:"project"`
	Version        string  `json:"version"`
	Environment    string  `json:"environment"`
	Target         string  `json:"target"`
	SourceRevision string  `json:"sourceRevision"`
	BundleDigest   string  `json:"bundleDigest"`
	ContractDigest string  `json:"contractDigest"`
	Digests        Digests `json:"digests"`
}

// Artifact is a fully verified prepared deployment. The parsed manifests
// and bundle bytes are everything lifecycle.Deploy / Rollback / Resolve
// consume; the caller never touches Git.
type Artifact struct {
	Dir         string
	Manifest    Manifest
	Release     *manifest.Release
	Environment *manifest.Environment
	Target      *manifest.Target
	Bundle      []byte
}

func sha256Hex(b []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(b))
}

// verifyFailure renders a refusal-class verification failure.
func verifyFailure(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotVerifiable, fmt.Sprintf(format, args...))
}

// crossCheck validates identity and cross-manifest invariants from
// PARSED manifests plus the raw member bytes. Used by both Prepare
// (before writing) and Load (before returning) so the two sides agree
// on exactly what "verified" means.
//
// strictReleasePin additionally requires the environment to pin exactly
// THIS release. Deploy artifacts are strict. Rollback material is not:
// the environment pins the failed release, so the artifact of the
// release being restored legitimately carries an environment that pins
// a different (transition-endpoint) release — the engine validates the
// transition endpoints itself.
func crossCheck(m Manifest, release *manifest.Release, env *manifest.Environment, tgt *manifest.Target, bundle []byte, strictReleasePin bool) error {
	if m.Schema != SchemaV1 {
		return verifyFailure("unknown schema %q, want %q", m.Schema, SchemaV1)
	}
	if m.Project != release.Metadata.Project || m.Version != release.Metadata.Version {
		return verifyFailure("manifest identity %s@%s does not match release manifest %s@%s",
			m.Project, m.Version, release.Metadata.Project, release.Metadata.Version)
	}
	if m.Environment != env.Metadata.Name {
		return verifyFailure("manifest environment %q does not match environment manifest %q", m.Environment, env.Metadata.Name)
	}
	if m.Target != tgt.Metadata.Name {
		return verifyFailure("manifest target %q does not match target manifest %q", m.Target, tgt.Metadata.Name)
	}
	if m.SourceRevision != release.Source.Revision {
		return verifyFailure("manifest source revision does not match the release manifest pin")
	}
	// The environment must point at exactly this target and this
	// release — the same cross-checks the engine performs, repeated
	// here so they hold BEFORE target contact.
	if env.Spec.Target != tgt.Metadata.Name {
		return verifyFailure("environment %q targets %q, but the artifact carries target %q",
			env.Metadata.Name, env.Spec.Target, tgt.Metadata.Name)
	}
	wantRef := fmt.Sprintf(".deploy/releases/%s-%s.yaml", release.Metadata.Project, release.Metadata.Version)
	if strictReleasePin && env.Spec.Release != wantRef {
		return verifyFailure("environment %q pins release %q, but the artifact carries %q",
			env.Metadata.Name, env.Spec.Release, wantRef)
	}
	if m.BundleDigest != release.Bundle.Digest {
		return verifyFailure("manifest bundle digest %s does not match the release pin %s", m.BundleDigest, release.Bundle.Digest)
	}
	if m.ContractDigest != release.DeploymentContract.Digest {
		return verifyFailure("manifest contract digest does not match the release pin")
	}
	if got := sha256Hex(bundle); got != release.Bundle.Digest {
		return verifyFailure("bundle bytes hash to %s but the release pins %s", got, release.Bundle.Digest)
	}
	return nil
}

// secretMarkers are byte patterns that must never appear in any member.
// The target manifest schema already excludes credential material; this
// scan is defense in depth so a hostile or misassembled artifact is
// refused before it can travel.
var secretMarkers = []string{
	"BEGIN OPENSSH PRIVATE KEY",
	"BEGIN RSA PRIVATE KEY",
	"BEGIN EC PRIVATE KEY",
	"BEGIN DSA PRIVATE KEY",
	"BEGIN PRIVATE KEY",
}

func checkNoSecrets(members map[string][]byte) error {
	for _, name := range []string{FileRelease, FileEnvironment, FileTarget, FileBundle} {
		b := members[name]
		for _, marker := range secretMarkers {
			if strings.Contains(string(b), marker) {
				return verifyFailure("%s contains private-key material — prepared artifacts are non-secret by construction", name)
			}
		}
	}
	return nil
}

// buildManifest parses the three manifests from their exact bytes,
// cross-checks everything and returns the integrity manifest plus the
// parsed manifests. No files are read or written — the pure heart
// shared by Prepare and Load.
func buildManifest(releaseBytes, envBytes, targetBytes, bundle []byte, strictReleasePin bool) (Manifest, *manifest.Release, *manifest.Environment, *manifest.Target, error) {
	rel, err := manifest.Parse(releaseBytes, manifest.KindRelease)
	if err != nil {
		return Manifest{}, nil, nil, nil, verifyFailure("release manifest: %v", err)
	}
	env, err := manifest.Parse(envBytes, manifest.KindEnvironment)
	if err != nil {
		return Manifest{}, nil, nil, nil, verifyFailure("environment manifest: %v", err)
	}
	tgt, err := manifest.Parse(targetBytes, manifest.KindTarget)
	if err != nil {
		return Manifest{}, nil, nil, nil, verifyFailure("target manifest: %v", err)
	}
	m := Manifest{
		Schema:         SchemaV1,
		Project:        rel.Release.Metadata.Project,
		Version:        rel.Release.Metadata.Version,
		Environment:    env.Environment.Metadata.Name,
		Target:         tgt.Target.Metadata.Name,
		SourceRevision: rel.Release.Source.Revision,
		BundleDigest:   rel.Release.Bundle.Digest,
		ContractDigest: rel.Release.DeploymentContract.Digest,
		Digests: Digests{
			Release:     sha256Hex(releaseBytes),
			Environment: sha256Hex(envBytes),
			Target:      sha256Hex(targetBytes),
			Bundle:      sha256Hex(bundle),
		},
	}
	if err := crossCheck(m, rel.Release, env.Environment, tgt.Target, bundle, strictReleasePin); err != nil {
		return Manifest{}, nil, nil, nil, err
	}
	return m, rel.Release, env.Environment, tgt.Target, nil
}

// Prepare validates prepared inputs and writes the artifact directory.
// It touches no target and requires no credential: the caller passes the
// exact manifest bytes from the promoted repository state and the exact
// canonical bundle bytes (whose digest must equal the release pin).
//
// Refuses to overwrite an existing artifact whose content differs —
// prepared material is immutable; an identical rewrite is allowed so a
// retried prepare step stays idempotent.
func Prepare(dir string, releaseBytes, envBytes, targetBytes, bundle []byte, strictReleasePin bool) (*Manifest, error) {
	m, _, _, _, err := buildManifest(releaseBytes, envBytes, targetBytes, bundle, strictReleasePin)
	if err != nil {
		return nil, err
	}
	members := map[string][]byte{
		FileRelease:     releaseBytes,
		FileEnvironment: envBytes,
		FileTarget:      targetBytes,
		FileBundle:      bundle,
	}
	if err := checkNoSecrets(members); err != nil {
		return nil, err
	}
	manifestJSON, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	manifestJSON = append(manifestJSON, '\n')

	if prev, err := os.ReadFile(filepath.Join(dir, FileManifest)); err == nil {
		prevMembers, rerr := readMembers(dir)
		if rerr != nil ||
			!sameBytes(prev, manifestJSON) ||
			!sameBytes(prevMembers[FileRelease], releaseBytes) ||
			!sameBytes(prevMembers[FileEnvironment], envBytes) ||
			!sameBytes(prevMembers[FileTarget], targetBytes) ||
			!sameBytes(prevMembers[FileBundle], bundle) {
			return nil, fmt.Errorf("refusing to overwrite existing prepared artifact %s with different material", dir)
		}
		return &m, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// The integrity manifest goes LAST: a partially written directory
	// is never a valid artifact.
	ordered := []string{FileRelease, FileEnvironment, FileTarget, FileBundle, FileManifest}
	writes := map[string][]byte{
		FileRelease:     releaseBytes,
		FileEnvironment: envBytes,
		FileTarget:      targetBytes,
		FileBundle:      bundle,
		FileManifest:    manifestJSON,
	}
	for _, name := range ordered {
		if err := os.WriteFile(filepath.Join(dir, name), writes[name], 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", name, err)
		}
	}
	return &m, nil
}

func sameBytes(a, b []byte) bool {
	return len(a) == len(b) && string(a) == string(b)
}

func readMember(dir, name string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, verifyFailure("member %s is missing (truncated artifact?)", name)
		}
		return nil, err
	}
	return b, nil
}

func readMembers(dir string) (map[string][]byte, error) {
	members := make(map[string][]byte, 4)
	for _, name := range []string{FileRelease, FileEnvironment, FileTarget, FileBundle} {
		b, err := readMember(dir, name)
		if err != nil {
			return nil, err
		}
		members[name] = b
	}
	return members, nil
}

// Load reads and fully verifies a prepared artifact. Every step fails
// closed; on success the returned Artifact is everything the lifecycle
// engine needs — the caller needs no Git and no repository checkout.
//
// The environment's release pin is NOT required to name this artifact's
// release (rollback material legitimately differs); deployments must
// additionally call LoadForDeploy.
func Load(dir string) (*Artifact, error) {
	manifestBytes, err := readMember(dir, FileManifest)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(manifestBytes)))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, verifyFailure("%s: %v", FileManifest, err)
	}
	if dec.More() {
		return nil, verifyFailure("%s: trailing data", FileManifest)
	}
	members, err := readMembers(dir)
	if err != nil {
		return nil, err
	}
	if m.Digests.Release != sha256Hex(members[FileRelease]) {
		return nil, verifyFailure("release manifest digest mismatch — the artifact was altered or corrupted")
	}
	if m.Digests.Environment != sha256Hex(members[FileEnvironment]) {
		return nil, verifyFailure("environment manifest digest mismatch — the artifact was altered or corrupted")
	}
	if m.Digests.Target != sha256Hex(members[FileTarget]) {
		return nil, verifyFailure("target manifest digest mismatch — the artifact was altered or corrupted")
	}
	if m.Digests.Bundle != sha256Hex(members[FileBundle]) {
		return nil, verifyFailure("bundle digest mismatch — the artifact was altered or corrupted")
	}
	rel, err := manifest.Parse(members[FileRelease], manifest.KindRelease)
	if err != nil {
		return nil, verifyFailure("release manifest: %v", err)
	}
	env, err := manifest.Parse(members[FileEnvironment], manifest.KindEnvironment)
	if err != nil {
		return nil, verifyFailure("environment manifest: %v", err)
	}
	tgt, err := manifest.Parse(members[FileTarget], manifest.KindTarget)
	if err != nil {
		return nil, verifyFailure("target manifest: %v", err)
	}
	if err := crossCheck(m, rel.Release, env.Environment, tgt.Target, members[FileBundle], false); err != nil {
		return nil, err
	}
	return &Artifact{
		Dir:         dir,
		Manifest:    m,
		Release:     rel.Release,
		Environment: env.Environment,
		Target:      tgt.Target,
		Bundle:      members[FileBundle],
	}, nil
}

// LoadForDeploy is Load plus the deployment-strict check: the
// environment must pin exactly this artifact's release. This is the
// only entry point deploy-prepared may use.
func LoadForDeploy(dir string) (*Artifact, error) {
	art, err := Load(dir)
	if err != nil {
		return nil, err
	}
	wantRef := fmt.Sprintf(".deploy/releases/%s-%s.yaml", art.Release.Metadata.Project, art.Release.Metadata.Version)
	if art.Environment.Spec.Release != wantRef {
		return nil, verifyFailure("environment %q pins release %q, but the artifact carries %q",
			art.Environment.Metadata.Name, art.Environment.Spec.Release, wantRef)
	}
	return art, nil
}
