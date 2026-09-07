package target

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
)

type bundleFile struct {
	relPath string
	mode    os.FileMode
	content []byte
}

// readBundleFiles parses a canonical bundle tar defensively. The bundle is
// digest-verified upstream, but staging refuses anything a filesystem could
// misinterpret: absolute or traversing entry paths, and entry types the
// transport cannot materialize (the v1 transport contract has no symlink
// primitive, so symlink entries are refused rather than approximated by
// regular files with different semantics).
func readBundleFiles(bundle []byte) ([]bundleFile, error) {
	tr := tar.NewReader(bytes.NewReader(bundle))
	var files []bundleFile
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("bundle: %w", err)
		}
		name := hdr.Name
		if name == "" || name[0] == '/' {
			return nil, fmt.Errorf("bundle: refusing entry path %q", name)
		}
		for _, seg := range splitPath(name) {
			if seg == "" || seg == "." || seg == ".." {
				return nil, fmt.Errorf("bundle: refusing entry path %q", name)
			}
		}
		switch hdr.Typeflag {
		case tar.TypeReg:
			content, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("bundle: read %q: %w", name, err)
			}
			mode := os.FileMode(hdr.Mode) & 0o777
			if mode == 0 {
				mode = 0o644
			}
			files = append(files, bundleFile{relPath: name, mode: mode, content: content})
		case tar.TypeSymlink:
			return nil, fmt.Errorf("bundle: entry %q is a symlink; the v1 staging substrate cannot materialize symlinks and refuses instead of approximating it", name)
		default:
			return nil, fmt.Errorf("bundle: entry %q has unsupported type %q", name, string(hdr.Typeflag))
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("bundle: no entries")
	}
	return files, nil
}

func splitPath(p string) []string {
	var segs []string
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			segs = append(segs, p[start:i])
			start = i + 1
		}
	}
	return append(segs, p[start:])
}
