package target

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/transport"
)

// stagedMarkerName is written into a release directory only after every
// bundle file is in place. Its presence is the atomic commit of a stage:
// a directory without it is an interrupted stage, never a release.
const stagedMarkerName = ".staged.json"

const stagedSchemaV1 = "toolkit.staged/v1"

type StageStatus int

const (
	// StageNew: the release was not staged before and is now staged.
	StageNew StageStatus = iota
	// StageAlreadyStaged: the exact release (same bundle digest) was
	// already staged; nothing was written. Staging the same release
	// twice is idempotent.
	StageAlreadyStaged
)

func (s StageStatus) String() string {
	switch s {
	case StageNew:
		return "new"
	case StageAlreadyStaged:
		return "already-staged"
	default:
		return "unknown"
	}
}

// StagedMarker records what a staged release directory contains. It is an
// observed fact about bytes on the target, not desired state.
type StagedMarker struct {
	Schema       string `json:"schema"`
	Project      string `json:"project"`
	Version      string `json:"version"`
	BundleDigest string `json:"bundleDigest"`
	StagedAt     string `json:"stagedAt"` // RFC3339, UTC
}

// Stage materializes the release's canonical bundle on the target as
// <deployRoot>/<project>/releases/<version>/ and marks it staged.
//
// Invariants, enforced in this order:
//
//  1. The bundle bytes are verified against the release's pinned bundle
//     digest BEFORE anything is written. A digest mismatch is refused
//     outright — it is a broken release artifact, not a staging problem.
//  2. A staged release directory is immutable. If it exists with the same
//     bundle digest, Stage is idempotent (StageAlreadyStaged). If it exists
//     with a different digest, staging is refused: same release identity
//     with different bytes must never silently replace a release.
//  3. Staging is crash-aware. The marker is written last, so an interrupted
//     stage leaves a directory without a marker. Such a directory is
//     refused (never silently completed or overwritten) — the operator
//     removes it manually. Fail closed beats clever cleanup.
//  4. Staging never touches observed state. staged is not deployed.
func (t *Target) Stage(ctx context.Context, rel *manifest.Release, bundle []byte, stagedAt time.Time) (StageStatus, error) {
	releaseDir, err := t.layout.ReleaseDir(rel.Metadata.Project, rel.Metadata.Version)
	if err != nil {
		return StageNew, err
	}
	markerPath := releaseDir + "/" + stagedMarkerName

	if rel.Bundle.Digest == "" {
		return StageNew, fmt.Errorf("release %s %s pins no bundle digest", rel.Metadata.Project, rel.Metadata.Version)
	}
	actual := digestOf(bundle)
	if actual != rel.Bundle.Digest {
		return StageNew, fmt.Errorf("refusing to stage %s %s: bundle bytes hash to %s but the release pins %s", rel.Metadata.Project, rel.Metadata.Version, actual, rel.Bundle.Digest)
	}

	present, err := t.exists(ctx, releaseDir)
	if err != nil {
		return StageNew, err
	}
	if present {
		m, err := t.readMarker(ctx, markerPath)
		if err != nil {
			return StageNew, fmt.Errorf("release directory %s exists but is not a complete stage (interrupted stages must be removed manually): %w", releaseDir, err)
		}
		if m.Project != rel.Metadata.Project || m.Version != rel.Metadata.Version {
			return StageNew, fmt.Errorf("staged marker %s records %s %s, refusing to treat it as %s %s", markerPath, m.Project, m.Version, rel.Metadata.Project, rel.Metadata.Version)
		}
		if m.BundleDigest != rel.Bundle.Digest {
			return StageNew, fmt.Errorf("refusing to stage %s %s: already staged with bundle digest %s, this bundle is %s — release directories are immutable", rel.Metadata.Project, rel.Metadata.Version, m.BundleDigest, rel.Bundle.Digest)
		}
		return StageAlreadyStaged, nil
	}

	files, err := readBundleFiles(bundle)
	if err != nil {
		return StageNew, err
	}
	for _, f := range files {
		if err := t.tr.Put(ctx, transport.PutRequest{
			Path:    releaseDir + "/" + f.relPath,
			Content: f.content,
			Mode:    f.mode,
		}); err != nil {
			return StageNew, fmt.Errorf("staging %s/%s: %w", rel.Metadata.Version, f.relPath, err)
		}
	}

	marker := StagedMarker{
		Schema:       stagedSchemaV1,
		Project:      rel.Metadata.Project,
		Version:      rel.Metadata.Version,
		BundleDigest: actual,
		StagedAt:     stagedAt.UTC().Format(time.RFC3339),
	}
	markerJSON, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return StageNew, err
	}
	markerJSON = append(markerJSON, '\n')
	// The marker goes last: before this Put the directory is an incomplete
	// stage that every reader refuses; after it the release is complete.
	if err := t.tr.Put(ctx, transport.PutRequest{Path: markerPath, Content: markerJSON, Mode: 0o644}); err != nil {
		return StageNew, fmt.Errorf("writing staged marker %s: %w", markerPath, err)
	}
	return StageNew, nil
}

// readMarker reads and parses a staged marker. All toolkit-owned target
// metadata follows one parsing discipline: exactly one JSON value, unknown
// fields rejected. Any failure — missing, unreadable, malformed, extra
// fields, unparseable timestamp — is an error; callers treat the directory
// as an incomplete stage and fail closed.
func (t *Target) readMarker(ctx context.Context, markerPath string) (StagedMarker, error) {
	var m StagedMarker
	raw, err := t.ReadFile(ctx, markerPath)
	if err != nil {
		return m, err
	}
	if err := decodeStrictJSON(raw, &m); err != nil {
		return m, fmt.Errorf("%s: %w", markerPath, err)
	}
	if m.Schema != stagedSchemaV1 {
		return m, fmt.Errorf("%s: schema %q, want %q", markerPath, m.Schema, stagedSchemaV1)
	}
	if _, err := time.Parse(time.RFC3339, m.StagedAt); err != nil {
		return m, fmt.Errorf("%s: stagedAt %q: %w", markerPath, m.StagedAt, err)
	}
	return m, nil
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("sha256:%064x", sum)
}
