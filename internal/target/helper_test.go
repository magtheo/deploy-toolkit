package target

import (
	"archive/tar"
	"bytes"
	"testing"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/transport/local"
)

func newLocalTarget(t *testing.T) *Target {
	t.Helper()
	tgt, err := New(local.New(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return tgt
}

type testEntry struct {
	path    string
	content string
	mode    uint32 // nil-ish: 0 → 0644
}

// buildBundle produces a canonical-shaped ustar bundle and its digest, the
// way bundle.Builder does (epoch mtimes, no pax).
func buildBundle(t *testing.T, ents []testEntry) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range ents {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{
			Name:     e.path,
			Mode:     int64(mode),
			Size:     int64(len(e.content)),
			ModTime:  time.Unix(0, 0).UTC(),
			Format:   tar.FormatUSTAR,
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), digestOf(buf.Bytes())
}

func testRelease(project, version, bundleDigest string) *manifest.Release {
	return &manifest.Release{
		Metadata: manifest.ReleaseMetadata{Project: project, Version: version},
		Bundle:   manifest.BundleDigest{Digest: bundleDigest},
	}
}

var stageEntries = []testEntry{
	{path: ".deploy/project.yaml", content: "apiVersion: deploy.toolkit/v1\nkind: Project\n"},
	{path: "deploy/run.sh", content: "#!/bin/sh\nexit 0\n", mode: 0o755},
	{path: "config/app.conf", content: "mode=production\n"},
}
