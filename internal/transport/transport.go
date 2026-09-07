package transport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Transport interface {
	Put(ctx context.Context, req PutRequest) error
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}

type PutRequest struct {
	Path string

	// Mode holds Unix permission bits (e.g. 0644, 0755). It is not a carrier
	// for other os.FileMode flags; V1 targets are Linux.
	Mode os.FileMode

	Content []byte
}

type RunRequest struct {
	Argv []string

	// Dir is the working directory on the target and must be absolute, just
	// like PutRequest.Path: a relative directory has an ambiguous meaning
	// over a remote transport.
	Dir string

	// Env: nil inherits the login environment; a non-nil map replaces the
	// environment wholesale (exact set, no inheritance).
	Env map[string]string
}

type RunResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

func ValidateAbsolutePath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is empty")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q must be absolute", path)
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path %q contains %q segments", path, "..")
		}
	}
	return filepath.Clean(path), nil
}
