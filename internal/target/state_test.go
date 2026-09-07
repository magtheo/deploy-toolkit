package target

import (
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
