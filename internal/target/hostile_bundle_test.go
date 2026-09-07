package target

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hostileEntry builds a one-entry tar with an attacker-chosen header.
func hostileEntry(t *testing.T, hdr *tar.Header, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr.ModTime = time.Unix(0, 0).UTC()
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if hdr.Typeflag == tar.TypeReg {
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestStageRefusesTraversalEntry(t *testing.T) {
	tgt := newLocalTarget(t)
	bundle := hostileEntry(t, &tar.Header{Name: "../../etc/evil.conf", Typeflag: tar.TypeReg, Size: 4}, []byte("pwn\n"))
	rel := testRelease("my-app", "1.0.0", digestOf(bundle))
	_, err := tgt.Stage(t.Context(), rel, bundle, time.Now())
	if err == nil || !strings.Contains(err.Error(), "refusing entry path") {
		t.Fatalf("traversal entry must be refused: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(tgt.layout.Root(), "my-app")); !os.IsNotExist(statErr) {
		t.Errorf("nothing may be written for a hostile bundle (stat err = %v)", statErr)
	}
}

func TestStageRefusesAbsoluteEntry(t *testing.T) {
	tgt := newLocalTarget(t)
	bundle := hostileEntry(t, &tar.Header{Name: "/etc/evil.conf", Typeflag: tar.TypeReg, Size: 4}, []byte("pwn\n"))
	rel := testRelease("my-app", "1.0.0", digestOf(bundle))
	if _, err := tgt.Stage(t.Context(), rel, bundle, time.Now()); err == nil {
		t.Fatal("absolute entry path must be refused")
	}
}

func TestStageRefusesSymlinkEntry(t *testing.T) {
	tgt := newLocalTarget(t)
	bundle := hostileEntry(t, &tar.Header{Name: "deploy/current", Typeflag: tar.TypeSymlink, Linkname: "../../elsewhere"}, nil)
	rel := testRelease("my-app", "1.0.0", digestOf(bundle))
	_, err := tgt.Stage(t.Context(), rel, bundle, time.Now())
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink entry must be refused explicitly: %v", err)
	}
}

func TestStageRefusesEmptyBundle(t *testing.T) {
	tgt := newLocalTarget(t)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	bundle := buf.Bytes()
	rel := testRelease("my-app", "1.0.0", digestOf(bundle))
	if _, err := tgt.Stage(t.Context(), rel, bundle, time.Now()); err == nil {
		t.Fatal("empty bundle must be refused")
	}
}
