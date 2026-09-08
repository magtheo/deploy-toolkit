package bundle

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
)

type Result struct {
	Digest         string
	ContractDigest string
	Files          []string
	// Bytes carries the canonical tar itself so the deploy stage can
	// stage exactly what was digested — no rebuild between verification
	// and upload.
	Bytes []byte
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
		default:
			// Symlinks (120000) are not part of Bundle Format v1: the
			// staging substrate cannot materialize them over the transport
			// contract, and a bundle the toolkit can build but never stage
			// is a cross-layer contradiction. Fail at construction, where
			// the release is still mutable — not at staging, where it is
			// already authorized. Vendor the link's content instead.
			return Result{}, fmt.Errorf("%q is tracked as %s; Bundle Format v1 carries regular files only — vendor the content instead of linking", p, e.mode)
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
		Bytes:          buf.Bytes(),
	}, nil
}

// BuildFromRevision builds the bundle for a release revision the way the
// deployment stage must: the bundle.include list is read from the
// revision's OWN .deploy/project.yaml — never from the current state of
// main, which may have drifted since the release was promoted. This is the
// deploy-time half of "the deployment contract comes from the promoted
// release".
func (b *Builder) BuildFromRevision(ctx context.Context, revision string) (Result, error) {
	out, err := b.git(ctx, "cat-file", "blob", revision+":"+contractPath)
	if err != nil {
		return Result{}, fmt.Errorf("read deployment contract at %s: %w", revision, err)
	}
	parsed, err := manifest.Parse(out, manifest.KindProject)
	if err != nil {
		return Result{}, fmt.Errorf("deployment contract at %s is not a valid project manifest: %w", revision, err)
	}
	return b.Build(ctx, revision, parsed.Project.Bundle.Include)
}
