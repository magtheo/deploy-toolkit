package bundle

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"path"
	"sort"
	"time"
)

type Result struct {
	Digest         string
	ContractDigest string
	Files          []string
}

func (b *Builder) Build(ctx context.Context, revision string, include []string) (Result, error) {
	entries, err := b.tree(ctx, revision)
	if err != nil {
		return Result{}, err
	}
	lookup, err := b.byPath(entries)
	if err != nil {
		return Result{}, err
	}
	contract, ok := lookup[contractPath]
	if !ok {
		return Result{}, fmt.Errorf("deployment contract %s is not tracked at revision %s", contractPath, revision)
	}

	selected := map[string]bool{contractPath: true}
	matchCount := make([]int, len(include))
	for p := range lookup {
		for i, pattern := range include {
			if matches(p, pattern) {
				selected[p] = true
				matchCount[i]++
			}
		}
	}
	for i, pattern := range include {
		if matchCount[i] == 0 {
			return Result{}, fmt.Errorf("bundle.include %q matches no tracked file at revision %s", pattern, revision)
		}
	}

	files := make([]string, 0, len(selected))
	for p := range selected {
		files = append(files, p)
	}
	sort.Strings(files)

	contractBytes, err := b.blob(ctx, contract.oid)
	if err != nil {
		return Result{}, err
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, p := range files {
		e := lookup[p]
		content, err := b.blob(ctx, e.oid)
		if err != nil {
			return Result{}, err
		}
		hdr := &tar.Header{
			Name:    p,
			ModTime: time.Unix(0, 0).UTC(),
			Format:  tar.FormatUSTAR,
		}
		switch e.mode {
		case "100644", "100755":
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(content))
			if e.mode == "100755" {
				hdr.Mode = 0o755
			} else {
				hdr.Mode = 0o644
			}
		case "120000":
			if err := checkSymlinkTarget(p, string(content)); err != nil {
				return Result{}, err
			}
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = string(content)
			hdr.Mode = 0o755
		default:
			return Result{}, fmt.Errorf("unsupported git mode %s for %q", e.mode, p)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return Result{}, fmt.Errorf("cannot represent %q in ustar: %w", p, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write(content); err != nil {
				return Result{}, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return Result{}, err
	}

	sum := sha256.Sum256(buf.Bytes())
	contractSum := sha256.Sum256(contractBytes)
	return Result{
		Digest:         fmt.Sprintf("sha256:%064x", sum),
		ContractDigest: fmt.Sprintf("sha256:%064x", contractSum),
		Files:          files,
	}, nil
}

func checkSymlinkTarget(entryPath, target string) error {
	if path.IsAbs(target) {
		return fmt.Errorf("symlink %q has absolute target %q; escaping symlinks are rejected", entryPath, target)
	}
	resolved := path.Clean(path.Join(path.Dir(entryPath), target))
	if resolved == ".." || hasTraversal(resolved) {
		return fmt.Errorf("symlink %q target %q resolves outside the bundle root", entryPath, target)
	}
	return nil
}

func hasTraversal(cleaned string) bool {
	return cleaned == ".." || len(cleaned) >= 3 && cleaned[:3] == "../"
}
