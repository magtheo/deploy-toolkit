package target

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStateAbsent(t *testing.T) {
	tgt := newLocalTarget(t)
	_, err := tgt.ReadState(t.Context(), "my-app", "production")
	if !errors.Is(err, ErrStateAbsent) {
		t.Fatalf("err = %v, want ErrStateAbsent", err)
	}
}

func TestStateWriteReadRoundtrip(t *testing.T) {
	tgt := newLocalTarget(t)
	st := State{
		Project:     "my-app",
		Environment: "production",
		Current: &CurrentDeployment{
			Release:      "1.0.0",
			BundleDigest: "sha256:" + strings.Repeat("cd", 32),
			Since:        time.Unix(1700000000, 0).UTC().Format(time.RFC3339),
		},
		UpdatedAt: time.Unix(1700000100, 0).UTC().Format(time.RFC3339),
	}
	if err := tgt.WriteState(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	got, err := tgt.ReadState(t.Context(), "my-app", "production")
	if err != nil {
		t.Fatal(err)
	}
	if got.Current == nil || got.Current.Release != "1.0.0" || got.Current.BundleDigest != st.Current.BundleDigest {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	if got.UpdatedAt != st.UpdatedAt || got.Environment != "production" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

func TestStateNilCurrentSurvivesRoundtrip(t *testing.T) {
	tgt := newLocalTarget(t)
	st := State{
		Project:     "my-app",
		Environment: "staging",
		UpdatedAt:   time.Unix(1700000100, 0).UTC().Format(time.RFC3339),
	}
	if err := tgt.WriteState(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	got, err := tgt.ReadState(t.Context(), "my-app", "staging")
	if err != nil {
		t.Fatal(err)
	}
	if got.Current != nil {
		t.Errorf("current = %+v, want nil", got.Current)
	}
}

func TestStateIdentitySwapDetected(t *testing.T) {
	tgt := newLocalTarget(t)
	// Write a state file, then move it to another environment's path and
	// check that reading it as that environment fails closed.
	if err := tgt.WriteState(t.Context(), State{
		Project:     "my-app",
		Environment: "staging",
		UpdatedAt:   time.Unix(1700000100, 0).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	root := tgt.layout.Root()
	if err := os.Rename(filepath.Join(root, "my-app/state/staging.json"), filepath.Join(root, "my-app/state/production.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := tgt.ReadState(t.Context(), "my-app", "production"); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("mismatched state identity must be refused: %v", err)
	}
}

func TestReadStateRejectsFieldsWriteWouldReject(t *testing.T) {
	tgt := newLocalTarget(t)
	if err := tgt.WriteState(t.Context(), State{
		Project:     "my-app",
		Environment: "production",
		Current: &CurrentDeployment{
			Release:      "1.0.0",
			BundleDigest: "sha256:" + strings.Repeat("ab", 32),
			Since:        fixedTime(1700000000),
		},
		UpdatedAt: fixedTime(1700000100),
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tgt.layout.Root(), "my-app/state/production.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	hostile := []struct {
		name    string
		mutate  func(m map[string]any)
		wantErr string
	}{
		{"traversal release", func(m map[string]any) {
			m["current"].(map[string]any)["release"] = "../../nonsense"
		}, "SemVer"},
		{"garbage digest", func(m map[string]any) {
			m["current"].(map[string]any)["bundleDigest"] = "garbage"
		}, "sha256"},
		{"broken since", func(m map[string]any) {
			m["current"].(map[string]any)["since"] = "yesterday"
		}, "since"},
		{"broken updatedAt", func(m map[string]any) {
			m["updatedAt"] = "broken"
		}, "updatedAt"},
		{"wrong schema", func(m map[string]any) {
			m["schema"] = "toolkit.state/v2"
		}, "schema"},
	}
	for _, c := range hostile {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		c.mutate(m)
		mutated, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, mutated, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := tgt.ReadState(t.Context(), "my-app", "production"); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: err = %v, want it to mention %q", c.name, err, c.wantErr)
		}
		// restore for the next case
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadStateRejectsUnknownFields(t *testing.T) {
	tgt := newLocalTarget(t)
	if err := tgt.WriteState(t.Context(), State{
		Project:     "my-app",
		Environment: "production",
		UpdatedAt:   fixedTime(1700000100),
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tgt.layout.Root(), "my-app/state/production.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["desired"] = map[string]any{"release": "2.0.0"} // desired state must never masquerade as observed
	mutated, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, mutated, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tgt.ReadState(t.Context(), "my-app", "production"); err == nil {
		t.Fatal("unknown fields must be rejected, not silently dropped")
	}
}

func TestReadStateRejectsTrailingJSON(t *testing.T) {
	tgt := newLocalTarget(t)
	if err := tgt.WriteState(t.Context(), State{
		Project:     "my-app",
		Environment: "production",
		UpdatedAt:   fixedTime(1700000100),
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tgt.layout.Root(), "my-app/state/production.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A second, complete JSON value after the valid state object — the
	// first Decode succeeds and must not mask it.
	trailing := string(raw) + "\n{\"desired\":{\"release\":\"evil\"}}\n"
	if err := os.WriteFile(path, []byte(trailing), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tgt.ReadState(t.Context(), "my-app", "production"); err == nil {
		t.Fatal("trailing JSON after the state object must be rejected")
	}
}

func TestWriteStateValidation(t *testing.T) {
	tgt := newLocalTarget(t)
	cases := []State{
		{Project: "My-App", Environment: "prod", UpdatedAt: "2023-11-14T22:13:20Z"},
		{Project: "my-app", Environment: "prod", UpdatedAt: "not-a-time"},
		{Project: "my-app", Environment: "prod", UpdatedAt: "2023-11-14T22:13:20Z",
			Current: &CurrentDeployment{Release: "", BundleDigest: "sha256:x", Since: "2023-11-14T22:13:20Z"}},
		{Project: "my-app", Environment: "prod", UpdatedAt: "2023-11-14T22:13:20Z",
			Current: &CurrentDeployment{Release: "1.0.0", BundleDigest: "", Since: "2023-11-14T22:13:20Z"}},
		{Project: "my-app", Environment: "prod", UpdatedAt: "2023-11-14T22:13:20Z",
			Current: &CurrentDeployment{Release: "1.0.0", BundleDigest: "sha256:x", Since: "yesterday"}},
	}
	for i, st := range cases {
		if err := tgt.WriteState(t.Context(), st); err == nil {
			t.Errorf("case %d: invalid state accepted", i)
		}
	}
}
