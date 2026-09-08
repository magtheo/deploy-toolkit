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

// readBundleFiles parses a canonical bundle tar defensively. Bundle Format
// v1 carries regular files only — the builder refuses everything else — so
// anything but regular entries here means the bytes were tampered with or
// built by something that ignored the format: staging refuses rather than
// misinterpret. Path checks (absolute, traversal segments) are defense in
// depth for the same reason.
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
			return nil, fmt.Errorf("bundle: entry %q is a symlink; Bundle Format v1 carries regular files only", name)
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
