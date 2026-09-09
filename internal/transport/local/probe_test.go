package local

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/magtheo/deploy-toolkit/internal/transport"
)

// ProbePath must return PROVEN absence only — a broken hierarchy or an
// inaccessible path is an error, never PathAbsent.
func TestProbePathClassifies(t *testing.T) {
	tr := &Transport{}
	root := t.TempDir()

	// Present file and directory.
	file := filepath.Join(root, "evidence.json")
	if err := os.WriteFile(file, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "subdir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Broken hierarchy: an INTERMEDIATE component is a regular file.
	blockedParent := filepath.Join(root, "blocker")
	if err := os.WriteFile(blockedParent, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	behindBlocker := filepath.Join(blockedParent, "child", "leaf.json")

	// Dangling final symlink follows the historical follow-symlink
	// semantics: the namespace genuinely lacks a file there → absent.
	dangling := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(root, "does-not-exist"), dangling); err != nil {
		t.Fatal(err)
	}
	// Symlink to an existing file → present.
	linked := filepath.Join(root, "linked")
	if err := os.Symlink(file, linked); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want transport.PathState
	}{
		{"present file", file, transport.PathFile},
		{"present directory", dir, transport.PathDirectory},
		{"proven absent leaf", filepath.Join(root, "missing.json"), transport.PathAbsent},
		{"proven absent deep", filepath.Join(root, "a", "b", "c.json"), transport.PathAbsent},
		{"dangling final symlink", dangling, transport.PathAbsent},
		{"symlink to existing file", linked, transport.PathFile},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tr.ProbePath(t.Context(), tc.path)
			if err != nil || got != tc.want {
				t.Fatalf("got (%v, %v), want %v", got, err, tc.want)
			}
		})
	}

	t.Run("intermediate is a file: error, never absence", func(t *testing.T) {
		_, err := tr.ProbePath(t.Context(), behindBlocker)
		if err == nil {
			t.Fatal("want an error")
		}
		// The state value is meaningless when err != nil (zero value).
		// The load-bearing property: the error carries NO not-exist
		// semantics, so no caller can mistake a broken hierarchy for a
		// fresh target.
		if errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the error must not carry not-exist semantics: %v", err)
		}
	})
}
