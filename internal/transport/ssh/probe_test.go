package ssh

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/magtheo/deploy-toolkit/internal/transport"
)

// The SFTP protocol collapses ENOENT and ENOTDIR onto one status code —
// these tests pin the walk's proof that a NO_SUCH_FILE beneath
// positively-verified directories is genuine absence, while a broken
// hierarchy is an error with NO not-exist semantics.
func TestProbePathOverSFTP(t *testing.T) {
	signer := clientSigner(t)
	ts := startTestServer(t, signer.PublicKey())

	tr, err := dialTransport(t, ts, signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	root := t.TempDir()
	file := filepath.Join(root, "evidence.json")
	if err := os.WriteFile(file, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Intermediate is a FILE: OpenSSH maps the resulting ENOTDIR to the
	// same SSH_FX_NO_SUCH_FILE as a genuine ENOENT — the walk must see
	// through that.
	blockedParent := filepath.Join(root, "blocker")
	if err := os.WriteFile(blockedParent, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	behindBlocker := filepath.Join(blockedParent, "child", "leaf.json")

	tests := []struct {
		name string
		path string
		want transport.PathState
	}{
		{"present file", file, transport.PathFile},
		{"present deploy root", root, transport.PathDirectory},
		{"proven absent leaf", filepath.Join(root, "missing.json"), transport.PathAbsent},
		{"absent because an ancestor is absent (fresh deploy root)", filepath.Join(root, "state", "production.json"), transport.PathAbsent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tr.ProbePath(t.Context(), tc.path)
			if err != nil || got != tc.want {
				t.Fatalf("got (%v, %v), want %v", got, err, tc.want)
			}
		})
	}

	t.Run("broken hierarchy is an error, never absence", func(t *testing.T) {
		_, err := tr.ProbePath(t.Context(), behindBlocker)
		if err == nil {
			t.Fatal("want an error")
		}
		if errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the error must not carry not-exist semantics: %v", err)
		}
	})

	t.Run("relative paths are refused", func(t *testing.T) {
		if _, err := tr.ProbePath(t.Context(), "relative/path"); err == nil {
			t.Fatal("want a validation error")
		}
	})
}

// Permission failure over SFTP must arrive as an error with
// permission — not absence — semantics: this pins the library
// translation (SSH_FX_PERMISSION_DENIED → os.ErrPermission) the
// outside-in walk relies on to never map "cannot establish" to absence.
// Skipped as root, since root ignores permission bits.
func TestProbePathPermissionDeniedIsNeverAbsent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits")
	}
	signer := clientSigner(t)
	ts := startTestServer(t, signer.PublicKey())
	tr, err := dialTransport(t, ts, signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	root := t.TempDir()
	sealed := filepath.Join(root, "sealed")
	if err := os.MkdirAll(sealed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sealed, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sealed, 0o755) })

	_, err = tr.ProbePath(t.Context(), filepath.Join(sealed, "production.json"))
	if err == nil {
		t.Fatal("want an error")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("permission failure must not carry not-exist semantics: %v", err)
	}
}
