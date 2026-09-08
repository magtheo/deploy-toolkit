package target

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestVerifyStageRejectsDifferentTarBytes pins the digest-first contract:
// a tar that extracts to the SAME files and modes but is NOT the canonical
// byte stream (different entry order → different bytes → different digest)
// must be rejected even though a file-by-file comparison alone would pass.
// "This staged tree matches" must mean "this is the exact immutable
// Release bundle", not "some tar with the same contents".
func TestVerifyStageRejectsDifferentTarBytes(t *testing.T) {
	tgt := newLocalTarget(t)
	reordered := append(append([]testEntry{}, stageEntries[1:]...), stageEntries[0])
	bundleA, digestA := buildBundle(t, stageEntries) // canonical order
	bundleB, digestB := buildBundle(t, reordered)    // same files/modes, different bytes
	if digestA == digestB {
		t.Fatal("test setup: reordered tar must hash differently")
	}

	rel := testRelease("my-app", "1.0.0", digestA)
	if _, err := tgt.Stage(t.Context(), rel, bundleA, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}

	// The staged tree genuinely contains bundleB's files — a naive
	// file-by-file diff would pass.
	if err := tgt.VerifyStage(t.Context(), rel, bundleA); err != nil {
		t.Fatalf("canonical bundle must verify: %v", err)
	}
	err := tgt.VerifyStage(t.Context(), rel, bundleB)
	if err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("VerifyStage with different tar bytes err = %v, want digest rejection", err)
	}

	// And the stage itself never accepted the wrong bytes in the first
	// place: staging bundleB against digestA is refused before writes.
	bad := testRelease("my-app", "1.0.0", digestA)
	if _, err := tgt.Stage(t.Context(), bad, bundleB, time.Unix(1700000100, 0)); err == nil {
		t.Fatal("Stage accepted bundle bytes that do not match the release pin")
	}
	marker, err := osReadMarker(t, tgt, "my-app", "1.0.0")
	if err != nil || marker.BundleDigest != digestA {
		t.Errorf("staged marker changed: %+v, %v", marker, err)
	}
}

func osReadMarker(t *testing.T, tgt *Target, project, version string) (StagedMarker, error) {
	t.Helper()
	dir, err := tgt.layout.ReleaseDir(project, version)
	if err != nil {
		return StagedMarker{}, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, stagedMarkerName))
	if err != nil {
		return StagedMarker{}, err
	}
	var m StagedMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		return StagedMarker{}, err
	}
	return m, nil
}
