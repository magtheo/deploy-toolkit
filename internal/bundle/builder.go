package bundle

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
)

const contractPath = ".deploy/project.yaml"

type entry struct {
	path  string
	mode  string
	otype string
	oid   string
}

type Builder struct {
	repoDir string
}

func NewBuilder(repoDir string) *Builder {
	return &Builder{repoDir: repoDir}
}

func (b *Builder) git(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = b.repoDir
	cmd.Env = append(os.Environ(), "GIT_NO_REPLACE_OBJECTS=1", "GIT_CONFIG_NOSYSTEM=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (b *Builder) tree(ctx context.Context, revision string) ([]entry, error) {
	out, err := b.git(ctx, "ls-tree", "-r", "-z", revision)
	if err != nil {
		return nil, fmt.Errorf("enumerate tree at %s (is the revision present in the local checkout?): %w", revision, err)
	}
	var entries []entry
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, p, ok := bytes.Cut(rec, []byte{'\t'})
		if !ok {
			return nil, fmt.Errorf("malformed ls-tree record %q", rec)
		}
		parts := strings.SplitN(string(meta), " ", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("malformed ls-tree meta %q", meta)
		}
		entries = append(entries, entry{path: string(p), mode: parts[0], otype: parts[1], oid: parts[2]})
	}
	return entries, nil
}

func (b *Builder) blob(ctx context.Context, oid string) ([]byte, error) {
	return b.git(ctx, "cat-file", "blob", oid)
}

func (b *Builder) byPath(entries []entry) (map[string]entry, error) {
	m := make(map[string]entry, len(entries))
	for _, e := range entries {
		if _, dup := m[e.path]; dup {
			return nil, fmt.Errorf("duplicate tree entry %q", e.path)
		}
		if e.otype == "commit" {
			return nil, fmt.Errorf("submodule %q cannot be bundled; vendor its content instead", e.path)
		}
		m[e.path] = e
	}
	return m, nil
}

func matchSegments(pathSegs, patternSegs []string) bool {
	if len(patternSegs) == 0 {
		return len(pathSegs) == 0
	}
	if patternSegs[0] == "**" {
		for i := 0; i <= len(pathSegs); i++ {
			if matchSegments(pathSegs[i:], patternSegs[1:]) {
				return true
			}
		}
		return false
	}
	if len(pathSegs) == 0 {
		return false
	}
	ok, err := path.Match(patternSegs[0], pathSegs[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pathSegs[1:], patternSegs[1:])
}

func matches(path string, pattern string) bool {
	return matchSegments(strings.Split(path, "/"), strings.Split(pattern, "/"))
}
