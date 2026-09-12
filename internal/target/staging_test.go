package target

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/transport"
	"github.com/magtheo/deploy-toolkit/internal/transport/local"
)

func TestStageNew(t *testing.T) {
	tgt := newLocalTarget(t)
	bundle, digest := buildBundle(t, stageEntries)
	root := tgt.layout.Root()

	status, err := tgt.Stage(t.Context(), testRelease("my-app", "1.0.0", digest), bundle, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if status != StageNew {
		t.Fatalf("status = %s, want new", status)
	}

	// Bundle files materialized with their modes.
	got, err := os.ReadFile(filepath.Join(root, "my-app/releases/1.0.0/deploy/run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "#!/bin/sh\nexit 0\n" {
		t.Errorf("run.sh content = %q", got)
	}
	fi, err := os.Stat(filepath.Join(root, "my-app/releases/1.0.0/deploy/run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("run.sh mode = %o, want 755", fi.Mode().Perm())
	}
	got, err = os.ReadFile(filepath.Join(root, "my-app/releases/1.0.0/config/app.conf"))
	if err != nil || string(got) != "mode=production\n" {
		t.Errorf("app.conf = %q, %v", got, err)
	}

	// Marker records what was staged — an observed fact.
	raw, err := os.ReadFile(filepath.Join(root, "my-app/releases/1.0.0/.staged.json"))
	if err != nil {
		t.Fatal(err)
	}
	var marker StagedMarker
	if err := json.Unmarshal(raw, &marker); err != nil {
		t.Fatal(err)
	}
	if marker.Schema != stagedSchemaV1 || marker.Project != "my-app" || marker.Version != "1.0.0" || marker.BundleDigest != digest {
		t.Errorf("marker = %+v", marker)
	}
	if marker.StagedAt != "2023-11-14T22:13:20Z" {
		t.Errorf("stagedAt = %q", marker.StagedAt)
	}
}

func TestStageIdempotent(t *testing.T) {
	tgt := newLocalTarget(t)
	bundle, digest := buildBundle(t, stageEntries)

	if _, err := tgt.Stage(t.Context(), testRelease("my-app", "1.0.0", digest), bundle, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(tgt.layout.Root(), "my-app/releases/1.0.0/.staged.json")
	before, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}

	status, err := tgt.Stage(t.Context(), testRelease("my-app", "1.0.0", digest), bundle, time.Unix(1800000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if status != StageAlreadyStaged {
		t.Fatalf("re-stage status = %s, want already-staged", status)
	}
	after, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("idempotent re-stage rewrote the marker")
	}
}

func TestStageDigestCollisionRefused(t *testing.T) {
	tgt := newLocalTarget(t)
	bundleA, digestA := buildBundle(t, stageEntries)
	bundleB, digestB := buildBundle(t, []testEntry{{path: "deploy/other.sh", content: "#!/bin/sh\n"}})
	if digestA == digestB {
		t.Fatal("test bundles must differ")
	}
	if _, err := tgt.Stage(t.Context(), testRelease("my-app", "1.0.0", digestA), bundleA, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}

	// Same release identity, new bytes: a release manifest for the same
	// version claiming a different bundle digest must be refused via the
	// staged marker, leaving the original release untouched.
	_, err := tgt.Stage(t.Context(), testRelease("my-app", "1.0.0", digestB), bundleB, time.Now())
	if err == nil {
		t.Fatal("staging different bytes for a pinned release digest must fail")
	}
	if !strings.Contains(err.Error(), "immutable") {
		t.Errorf("error should invoke immutability: %v", err)
	}
}

func TestStageIdentityCollisionRefused(t *testing.T) {
	tgt := newLocalTarget(t)
	bundle, digest := buildBundle(t, stageEntries)
	if _, err := tgt.Stage(t.Context(), testRelease("my-app", "1.0.0", digest), bundle, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	// Same version identity, different bundle digest claimed by the release.
	_, err := tgt.Stage(t.Context(), testRelease("my-app", "1.0.0", digestOf([]byte("other-bytes"))), bundle, time.Now())
	if err == nil || !strings.Contains(err.Error(), "hash to") {
		t.Fatalf("bundle/release digest mismatch must be refused before writing: %v", err)
	}
}

func TestStageRefusedBeforeAnyWrite(t *testing.T) {
	tgt := newLocalTarget(t)
	bundle, _ := buildBundle(t, stageEntries)
	bad := testRelease("my-app", "1.0.0", "sha256:"+strings.Repeat("ab", 32))
	if _, err := tgt.Stage(t.Context(), bad, bundle, time.Now()); err == nil {
		t.Fatal("mismatched bundle digest must fail")
	}
	if _, err := os.Stat(filepath.Join(tgt.layout.Root(), "my-app/releases/1.0.0")); !os.IsNotExist(err) {
		t.Errorf("release directory must not exist after refusal (err = %v)", err)
	}
}

func TestStageUnmarkedDirectoryRefused(t *testing.T) {
	tgt := newLocalTarget(t)
	dir := filepath.Join(tgt.layout.Root(), "my-app/releases/1.0.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy/partial.bin"), []byte("half-written"), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle, digest := buildBundle(t, stageEntries)
	_, err := tgt.Stage(t.Context(), testRelease("my-app", "1.0.0", digest), bundle, time.Now())
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("unmarked release directory must be refused as interrupted stage: %v", err)
	}
	// The partial file is untouched — no silent completion.
	got, readErr := os.ReadFile(filepath.Join(dir, "deploy/partial.bin"))
	if readErr != nil || string(got) != "half-written" {
		t.Errorf("interrupted stage was modified: %q, %v", got, readErr)
	}
}

func TestStageCorruptMarkerRefused(t *testing.T) {
	tgt := newLocalTarget(t)
	dir := filepath.Join(tgt.layout.Root(), "my-app/releases/1.0.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bundle, digest := buildBundle(t, stageEntries)
	rel := testRelease("my-app", "1.0.0", digest)

	valid := `{"schema":"toolkit.staged/v1","project":"my-app","version":"1.0.0","bundleDigest":"` + digest + `","stagedAt":"2023-11-14T22:13:20Z"}`
	hostile := []struct {
		name   string
		marker string
	}{
		{"not json", "{not json"},
		{"wrong schema", strings.Replace(valid, "toolkit.staged/v1", "toolkit.staged/v2", 1)},
		{"bad timestamp", strings.Replace(valid, "2023-11-14T22:13:20Z", "yesterday", 1)},
		{"unknown field", strings.Replace(valid, "}", `,"extra":1}`, 1)},
		{"trailing json", valid + "\n{\"schema\":\"toolkit.staged/v1\"}"},
	}
	for _, c := range hostile {
		if err := os.WriteFile(filepath.Join(dir, stagedMarkerName), []byte(c.marker), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := tgt.Stage(t.Context(), rel, bundle, time.Now()); err == nil {
			t.Errorf("%s: hostile marker must fail closed", c.name)
		}
	}
}

// failingTransport forwards to a real local transport but fails the
// configured Run argv head and Put path prefix — used to force staging
// and staging-lock-release failures deterministically.
type failingTransport struct {
	transport.Transport
	killRun string // argv[0] to fail (e.g. "rmdir")
	killPut string // Put path prefix to fail
}

func (f *failingTransport) Run(ctx context.Context, req transport.RunRequest) (transport.RunResult, error) {
	if f.killRun != "" && len(req.Argv) > 0 && req.Argv[0] == f.killRun {
		return transport.RunResult{ExitCode: 1, Stderr: []byte("forced failure")}, nil
	}
	return f.Transport.Run(ctx, req)
}

func (f *failingTransport) Put(ctx context.Context, req transport.PutRequest) error {
	if f.killPut != "" && strings.HasPrefix(req.Path, f.killPut) {
		return errors.New("forced put failure")
	}
	return f.Transport.Put(ctx, req)
}

// A successful stage whose staging lock cannot be released must still
// fail: the leftover lock blocks every future stage of the version.
func TestStageSuccessWithReleaseFailureJoins(t *testing.T) {
	root := t.TempDir()
	tgt, err := New(&failingTransport{Transport: local.New(), killRun: "rmdir"}, root)
	if err != nil {
		t.Fatal(err)
	}
	bundle, digest := buildBundle(t, stageEntries)

	status, err := tgt.Stage(t.Context(), testRelease("my-app", "1.0.0", digest), bundle, time.Unix(1700000000, 0))
	if status != StageNew {
		t.Errorf("status = %s, want new (the stage itself succeeded)", status)
	}
	if !errors.Is(err, ErrStageLockReleaseFailed) {
		t.Fatalf("err = %v, want ErrStageLockReleaseFailed in chain", err)
	}
	if _, serr := os.Stat(filepath.Join(root, "my-app/.staging/1.0.0")); serr != nil {
		t.Errorf("staging lock did not survive: %v", serr)
	}
}

// A primary staging failure must NOT swallow a simultaneous staging
// lock release failure: both facts survive in the joined error.
func TestStageFailurePreservesReleaseFailure(t *testing.T) {
	root := t.TempDir()
	tgt, err := New(&failingTransport{
		Transport: local.New(),
		killRun:   "rmdir",
		killPut:   filepath.Join(root, "my-app/releases"),
	}, root)
	if err != nil {
		t.Fatal(err)
	}
	bundle, digest := buildBundle(t, stageEntries)

	_, err = tgt.Stage(t.Context(), testRelease("my-app", "1.0.0", digest), bundle, time.Unix(1700000000, 0))
	if err == nil {
		t.Fatal("stage with forced put failure succeeded")
	}
	if errors.Is(err, ErrStageLockHeld) {
		t.Fatalf("primary staging failure misread as held lock: %v", err)
	}
	if !strings.Contains(err.Error(), "forced put failure") {
		t.Errorf("primary staging failure lost: %v", err)
	}
	if !errors.Is(err, ErrStageLockReleaseFailed) {
		t.Errorf("release failure swallowed by primary staging failure: %v", err)
	}
}
