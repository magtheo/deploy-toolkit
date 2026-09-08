package target

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
