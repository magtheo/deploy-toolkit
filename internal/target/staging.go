package target

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	// StageNotAttempted: no stage was attempted (e.g. the attempt was
	// refused before staging). Explicit so evidence never renders a
	// refusal as "new".
	StageNotAttempted StageStatus = iota
	// StageNew: the release was not staged before and is now staged.
	StageNew
	// StageAlreadyStaged: the exact release (same bundle digest) was
	// already staged AND the staged material still verifies against the
	// canonical bundle; nothing was written. Staging the same release
	// twice is idempotent.
	StageAlreadyStaged
)

func (s StageStatus) String() string {
	switch s {
	case StageNotAttempted:
		return "not-attempted"
	case StageNew:
		return "new"
	case StageAlreadyStaged:
		return "already-staged"
	default:
		return "unknown"
	}
}

// StagingCleanupTimeout bounds staging-lock release attempts so a
// stuck target cannot hang staging finalization forever. Var for test
// overriding (exported because the lifecycle tests exercise it through
// the public Deploy path).
var StagingCleanupTimeout = 30 * time.Second

// ErrStageLockHeld marks the staging lock being held: another
// environment of the same project is staging THIS release version right
// now. Like the environment lock, it is a REFUSAL-class fact (another
// operation may be executing), and a crashed stager leaves the lock for
// manual removal — never silently broken.
var ErrStageLockHeld = errors.New("staging lock is held")

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
//  5. Staging is serialized ACROSS environments by a project-scoped
//     staging lock (.staging/<version>, atomic mkdir). Releases are
//     environment-independent, so two environments deploying the same
//     version share one staging sequence; without the lock, one would
//     see the other's mid-stage directory (files present, marker not
//     yet written) and misread it as an interrupted stage. Under the
//     lock that shape can only mean a crashed stager — refused, lock
//     left for manual removal, exactly like the environment lock.
//
// Stage returns NAMED values: the deferred staging-lock release joins a
// release failure into retErr — unnamed returns would drop it.
func (t *Target) Stage(ctx context.Context, rel *manifest.Release, bundle []byte, stagedAt time.Time) (status StageStatus, retErr error) {
	releaseDir, err := t.layout.ReleaseDir(rel.Metadata.Project, rel.Metadata.Version)
	if err != nil {
		return StageNew, err
	}
	markerPath := releaseDir + "/" + stagedMarkerName

	if rel.Bundle.Digest == "" {
		return StageNotAttempted, fmt.Errorf("release %s %s pins no bundle digest", rel.Metadata.Project, rel.Metadata.Version)
	}
	actual := digestOf(bundle)
	if actual != rel.Bundle.Digest {
		return StageNotAttempted, fmt.Errorf("refusing to stage %s %s: bundle bytes hash to %s but the release pins %s", rel.Metadata.Project, rel.Metadata.Version, actual, rel.Bundle.Digest)
	}

	files, err := readBundleFiles(bundle)
	if err != nil {
		return StageNotAttempted, err
	}
	// Serialize the marker-check + write sequence across environments.
	// Everything below — probe, existing-stage verification, file
	// writes, marker — runs under this lock.
	lockDir, err := t.layout.StagingLockDir(rel.Metadata.Project, rel.Metadata.Version)
	if err != nil {
		return StageNotAttempted, err
	}
	parent := lockDir[:strings.LastIndex(lockDir, "/")]
	if res, err := t.tr.Run(ctx, transport.RunRequest{Argv: []string{"mkdir", "-p", parent}, Dir: "/"}); err != nil {
		return StageNotAttempted, fmt.Errorf("staging lock %s: create parent: %w", lockDir, err)
	} else if res.ExitCode != 0 {
		return StageNotAttempted, fmt.Errorf("staging lock %s: create parent exit %d: %s", lockDir, res.ExitCode, firstLine(res.Stderr))
	}
	// mkdir can fail EEXIST for a lock the owner removes a moment
	// later (winner finished staging between our mkdir and our probe).
	// That exact handoff is retried a bounded number of times; a lock
	// that keeps existing is the genuine held shape: refusal.
	var acquired bool
	for attempt := 0; attempt < 3; attempt++ {
		res, err := t.tr.Run(ctx, transport.RunRequest{Argv: []string{"mkdir", lockDir}, Dir: "/"})
		if err != nil {
			return StageNotAttempted, fmt.Errorf("staging lock %s: %w", lockDir, err)
		}
		if res.ExitCode == 0 {
			acquired = true
			break
		}
		pst, perr := t.probePath(ctx, lockDir)
		if perr == nil && pst == transport.PathDirectory {
			return StageNotAttempted, fmt.Errorf("%w: %s is being staged by another operation (a crashed stager leaves the lock; remove it manually after verifying no stage is in flight)", ErrStageLockHeld, lockDir)
		}
		if perr == nil && pst == transport.PathAbsent {
			continue // the winner released it between mkdir and probe: try again
		}
		return StageNotAttempted, fmt.Errorf("staging lock %s: mkdir exit %d: %s", lockDir, res.ExitCode, firstLine(res.Stderr))
	}
	if !acquired {
		return StageNotAttempted, fmt.Errorf("%w: %s could not be acquired", ErrStageLockHeld, lockDir)
	}
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), StagingCleanupTimeout)
		defer ccancel()
		rres, rerr := t.tr.Run(cctx, transport.RunRequest{Argv: []string{"rmdir", lockDir}, Dir: "/"})
		if rerr == nil && rres.ExitCode != 0 {
			rerr = fmt.Errorf("rmdir exit %d: %s", rres.ExitCode, firstLine(rres.Stderr))
		}
		if rerr != nil {
			// A leftover staging lock blocks future stages of this
			// version; the error must never be swallowed.
			if retErr == nil {
				retErr = fmt.Errorf("staging lock %s could not be released (manual cleanup required): %w", lockDir, rerr)
			}
		}
	}()

	st, err := t.probePath(ctx, releaseDir)
	if err != nil {
		return StageNotAttempted, err
	}
	if st == transport.PathFile {
		return StageNotAttempted, fmt.Errorf("release directory %s is not a directory (broken target hierarchy)", releaseDir)
	}
	if st == transport.PathDirectory {
		m, err := t.readMarker(ctx, markerPath)
		if err != nil {
			return StageNotAttempted, fmt.Errorf("release directory %s exists but is not a complete stage (interrupted stages must be removed manually): %w", releaseDir, err)
		}
		if m.Project != rel.Metadata.Project || m.Version != rel.Metadata.Version {
			return StageNotAttempted, fmt.Errorf("staged marker %s records %s %s, refusing to treat it as %s %s", markerPath, m.Project, m.Version, rel.Metadata.Project, rel.Metadata.Version)
		}
		if m.BundleDigest != rel.Bundle.Digest {
			return StageNotAttempted, fmt.Errorf("refusing to stage %s %s: already staged with bundle digest %s, this bundle is %s — release directories are immutable", rel.Metadata.Project, rel.Metadata.Version, m.BundleDigest, rel.Bundle.Digest)
		}
		// The marker's word is not proof: verify that the directory still
		// contains exactly the canonical bundle before declaring the
		// stage reusable. A yesterday's assertion does not protect
		// against altered bytes on the target.
		if verr := t.verifyStagedFiles(ctx, releaseDir, bundle); verr != nil {
			return StageNotAttempted, fmt.Errorf("staged release %s no longer matches its marker (%w) — altered staged material is never executed or reused", releaseDir, verr)
		}
		return StageAlreadyStaged, nil
	}
	for _, f := range files {
		if err := t.tr.Put(ctx, transport.PutRequest{
			Path:    releaseDir + "/" + f.relPath,
			Content: f.content,
			Mode:    f.mode,
		}); err != nil {
			return StageNotAttempted, fmt.Errorf("staging %s/%s: %w", rel.Metadata.Version, f.relPath, err)
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
		return StageNotAttempted, err
	}
	markerJSON = append(markerJSON, '\n')
	// The marker goes last: before this Put the directory is an incomplete
	// stage that every reader refuses; after it the release is complete.
	if err := t.tr.Put(ctx, transport.PutRequest{Path: markerPath, Content: markerJSON, Mode: 0o644}); err != nil {
		return StageNotAttempted, fmt.Errorf("writing staged marker %s: %w", markerPath, err)
	}
	return StageNew, nil
}

// VerifyStage proves that a staged release directory may be trusted as
// the release it claims to be: the supplied bytes must hash to the
// release's pinned bundle digest, the marker must record that same
// identity and digest, all canonical files must be present with
// byte-identical content, and executable / non-executable semantics must
// be intact. Rollback and any reuse of staged material must pass here
// before executing anything from the directory. Scope: this proves all
// canonical files are intact — it is not full tree equality, because
// files ADDED after staging are not yet detected; lifecycle hooks must
// not depend on undeclared release-directory files.
func (t *Target) VerifyStage(ctx context.Context, rel *manifest.Release, bundle []byte) error {
	if rel.Bundle.Digest == "" {
		return fmt.Errorf("release %s %s pins no bundle digest", rel.Metadata.Project, rel.Metadata.Version)
	}
	if actual := digestOf(bundle); actual != rel.Bundle.Digest {
		// The supplied bytes are not the pinned bundle — even if every
		// extracted file happened to match the staged tree, this is not
		// the immutable Release the digest vouches for.
		return fmt.Errorf("supplied bundle hashes to %s but the release pins %s", actual, rel.Bundle.Digest)
	}
	releaseDir, err := t.layout.ReleaseDir(rel.Metadata.Project, rel.Metadata.Version)
	if err != nil {
		return err
	}
	m, err := t.readMarker(ctx, releaseDir+"/"+stagedMarkerName)
	if err != nil {
		return fmt.Errorf("not a complete stage: %w", err)
	}
	if m.Project != rel.Metadata.Project || m.Version != rel.Metadata.Version {
		return fmt.Errorf("marker records %s %s, want %s %s", m.Project, m.Version, rel.Metadata.Project, rel.Metadata.Version)
	}
	if m.BundleDigest != rel.Bundle.Digest {
		return fmt.Errorf("marker digest %s, want %s", m.BundleDigest, rel.Bundle.Digest)
	}
	return t.verifyStagedFiles(ctx, releaseDir, bundle)
}

func (t *Target) verifyStagedFiles(ctx context.Context, releaseDir string, bundle []byte) error {
	files, err := readBundleFiles(bundle)
	if err != nil {
		return err
	}
	for _, f := range files {
		p := releaseDir + "/" + f.relPath
		res, err := t.tr.Run(ctx, transport.RunRequest{Argv: []string{"cat", p}, Dir: "/"})
		if err != nil {
			return fmt.Errorf("%s: %w", f.relPath, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("%s: missing or unreadable on target (exit %d)", f.relPath, res.ExitCode)
		}
		if !bytes.Equal(res.Stdout, f.content) {
			return fmt.Errorf("%s: content differs from the canonical bundle", f.relPath)
		}
		execWant := f.mode&0o100 != 0
		res, err = t.tr.Run(ctx, transport.RunRequest{Argv: []string{"test", "-x", p}, Dir: "/"})
		if err != nil {
			return fmt.Errorf("%s: %w", f.relPath, err)
		}
		execGot := res.ExitCode == 0
		if execWant != execGot {
			return fmt.Errorf("%s: executable semantics differ (bundle mode %o)", f.relPath, f.mode)
		}
	}
	return nil
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
