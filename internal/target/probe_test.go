package target

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/transport"
	"github.com/magtheo/deploy-toolkit/internal/transport/local"
)

// opaqueProbe fails ProbePath for matching paths — the "transport died
// mid-probe" shape. The state value must never leak into evidence
// classification.
type opaqueProbe struct {
	inner    transport.Transport
	failWhen func(path string) bool
}

func (t *opaqueProbe) Put(ctx context.Context, req transport.PutRequest) error {
	return t.inner.Put(ctx, req)
}

func (t *opaqueProbe) Run(ctx context.Context, req transport.RunRequest) (transport.RunResult, error) {
	return t.inner.Run(ctx, req)
}

func (t *opaqueProbe) ProbePath(ctx context.Context, path string) (transport.PathState, error) {
	if t.failWhen != nil && t.failWhen(path) {
		return 0, errors.New("probe channel died")
	}
	return t.inner.ProbePath(ctx, path)
}

// No read class may interpret UNKNOWN (transport failure, broken
// hierarchy, permission, a directory where evidence was expected) as
// absence. Absence is proven absence, everywhere.
func TestUnknownIsNeverAbsence(t *testing.T) {
	root := t.TempDir()
	if _, err := New(&opaqueProbe{inner: local.New()}, root); err != nil {
		t.Fatal(err)
	}

	t.Run("transport failure during probe is an error, not a sentinel", func(t *testing.T) {
		tr, err := New(&opaqueProbe{inner: local.New(), failWhen: func(string) bool { return true }}, root)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tr.ReadState(t.Context(), "my-app", "production"); err == nil || errors.Is(err, ErrStateAbsent) {
			t.Errorf("ReadState: err = %v, must not be ErrStateAbsent", err)
		}
		if _, err := tr.ReadAttempt(t.Context(), "my-app", "production"); err == nil || errors.Is(err, ErrAttemptAbsent) {
			t.Errorf("ReadAttempt: err = %v, must not be ErrAttemptAbsent", err)
		}
		if _, err := tr.ReadRecovery(t.Context(), "my-app", "production"); err == nil || errors.Is(err, ErrRecoveryAbsent) {
			t.Errorf("ReadRecovery: err = %v, must not be ErrRecoveryAbsent", err)
		}
		if _, err := tr.ReadHistory(t.Context(), "my-app", "production"); err == nil || errors.Is(err, ErrHistoryAbsent) {
			t.Errorf("ReadHistory: err = %v, must not be ErrHistoryAbsent", err)
		}
	})

	t.Run("directory where evidence was expected is an error", func(t *testing.T) {
		tgt := newLocalTarget(t)
		p, err := tgt.Layout().StatePath("my-app", "production")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err = tgt.ReadState(t.Context(), "my-app", "production")
		if err == nil || errors.Is(err, ErrStateAbsent) {
			t.Fatalf("err = %v, must not be ErrStateAbsent", err)
		}
	})

	t.Run("deploy root is a file: error, never absence", func(t *testing.T) {
		blocked := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		tgt, err := New(local.New(), filepath.Join(blocked, "deploy"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tgt.ReadState(t.Context(), "my-app", "production"); err == nil || errors.Is(err, ErrStateAbsent) {
			t.Fatalf("err = %v, must not be ErrStateAbsent", err)
		}
	})

	t.Run("unreadable parent is an error (skipped as root)", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores permission bits")
		}
		tgt := newLocalTarget(t)
		p, err := tgt.Layout().StatePath("my-app", "production")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Dir(p), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(filepath.Dir(p), 0o755) })
		if _, err := tgt.ReadState(t.Context(), "my-app", "production"); err == nil || errors.Is(err, ErrStateAbsent) {
			t.Fatalf("err = %v, must not be ErrStateAbsent", err)
		}
	})
}

// Staging: a FILE squatting on the release directory path is broken
// evidence, never a reusable stage.
func TestStageRefusesFileAtReleaseDir(t *testing.T) {
	tgt := newLocalTarget(t)
	bundle, digest := buildBundle(t, stageEntries)
	rel := testRelease("my-app", "1.0.0", digest)
	dir, err := tgt.Layout().ReleaseDir(rel.Metadata.Project, rel.Metadata.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = tgt.Stage(t.Context(), rel, bundle, time.Unix(1700000000, 0))
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("err = %v, want a not-a-directory refusal", err)
	}
}
