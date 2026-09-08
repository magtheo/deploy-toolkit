package target

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validRecovery() RecoveryMarker {
	return RecoveryMarker{
		RecoveryID:       "0123456789abcdef",
		SourceAttemptID:  "fedcba9876543210",
		Project:          "my-app",
		Environment:      "production",
		FromRelease:      "2.0.0",
		FromBundleDigest: "sha256:" + strings.Repeat("bb", 32),
		ToRelease:        "1.0.0",
		ToBundleDigest:   "sha256:" + strings.Repeat("aa", 32),
		Authorization:    "recovery",
		StartedAt:        "2026-09-08T00:00:00Z",
	}
}

func TestRecoveryMarkerRoundtripAndValidation(t *testing.T) {
	tgt := newLocalTarget(t)
	ctx := t.Context()

	if _, err := tgt.ReadRecovery(ctx, "my-app", "production"); !errors.Is(err, ErrRecoveryAbsent) {
		t.Fatalf("absent recovery err = %v, want ErrRecoveryAbsent", err)
	}

	valid := validRecovery()
	if err := tgt.WriteRecovery(ctx, valid); err != nil {
		t.Fatal(err)
	}
	got, err := tgt.ReadRecovery(ctx, "my-app", "production")
	if err != nil || got.RecoveryID != valid.RecoveryID || got.SourceAttemptID != valid.SourceAttemptID ||
		got.FromRelease != "2.0.0" || got.ToRelease != "1.0.0" || got.Authorization != "recovery" {
		t.Fatalf("roundtrip = %+v, %v", got, err)
	}

	bad := []func(*RecoveryMarker){
		func(m *RecoveryMarker) { m.RecoveryID = "xyz" },
		func(m *RecoveryMarker) { m.SourceAttemptID = "attempt-1" },
		func(m *RecoveryMarker) { m.FromRelease = "not-semver" },
		func(m *RecoveryMarker) { m.ToRelease = "../evil" },
		func(m *RecoveryMarker) { m.FromBundleDigest = "md5:00" },
		func(m *RecoveryMarker) { m.ToBundleDigest = "garbage" },
		func(m *RecoveryMarker) { m.Authorization = "always" },
		func(m *RecoveryMarker) { m.Authorization = "" },
		func(m *RecoveryMarker) { m.StartedAt = "yesterday" },
	}
	for i, mutate := range bad {
		m := valid
		mutate(&m)
		if err := tgt.WriteRecovery(ctx, m); err == nil {
			t.Errorf("case %d: invalid recovery marker accepted", i)
		}
	}

	// A corrupt on-disk marker is an error, not absence.
	p, _ := tgt.layout.RecoveryPath("my-app", "production")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tgt.ReadRecovery(ctx, "my-app", "production"); err == nil || errors.Is(err, ErrRecoveryAbsent) {
		t.Errorf("corrupt marker err = %v, want fail-closed error", err)
	}

	if err := tgt.ClearRecovery(ctx, "my-app", "production"); err != nil {
		t.Fatal(err)
	}
	if _, err := tgt.ReadRecovery(ctx, "my-app", "production"); !errors.Is(err, ErrRecoveryAbsent) {
		t.Fatalf("cleared recovery err = %v, want ErrRecoveryAbsent", err)
	}
}

func TestStateOperationIDValidation(t *testing.T) {
	tgt := newLocalTarget(t)
	ctx := t.Context()

	base := State{
		Project:     "my-app",
		Environment: "production",
		Current: &CurrentDeployment{
			Release:      "1.0.0",
			BundleDigest: "sha256:" + strings.Repeat("aa", 32),
			Since:        "2026-09-08T00:00:00Z",
		},
		UpdatedAt: "2026-09-08T00:00:00Z",
	}

	// Absent operationId (state written before the contract existed) and
	// well-formed ids are accepted.
	for _, op := range []string{"", "deploy:0123456789abcdef", "recovery:fedcba9876543210"} {
		st := base
		st.Current.OperationID = op
		if err := tgt.WriteState(ctx, st); err != nil {
			t.Fatalf("operationId %q rejected: %v", op, err)
		}
		got, err := tgt.ReadState(ctx, "my-app", "production")
		if err != nil || got.Current.OperationID != op {
			t.Fatalf("roundtrip operationId = %q, %v", got.Current.OperationID, err)
		}
	}

	// Malformed ids fail closed on write — and on read.
	for _, op := range []string{"deploy", "deploy:xyz", "rollback:0123456789abcdef", "recovery:0123456789abcdef0"} {
		st := base
		st.Current.OperationID = op
		if err := tgt.WriteState(ctx, st); err == nil {
			t.Errorf("operationId %q accepted", op)
		}
	}
	p, _ := tgt.layout.StatePath("my-app", "production")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), `"operationId": "recovery:fedcba9876543210"`, `"operationId": "recovery:short"`, 1)
	if err := os.WriteFile(p, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tgt.ReadState(ctx, "my-app", "production"); err == nil || !strings.Contains(err.Error(), "operationId") {
		t.Errorf("tampered operationId err = %v, want rejection", err)
	}
}
