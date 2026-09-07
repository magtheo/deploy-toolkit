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

// StartError marks a target-side start failure: the requested program or
// working directory could not be used. Local transports observe this
// directly; over SSH, exit codes 126 (found but not usable, or a failed cd
// into Dir) and 127 (program not found) follow the POSIX dispatch
// convention. A target command that deliberately exits 126/127 is
// indistinguishable from a start failure over SSH — a protocol limit that is
// pinned here, not discovered later by the history layer.
type StartError struct {
	ExitCode int
	Err      error
}

func (e *StartError) Error() string {
	if e.ExitCode != 0 {
		return fmt.Sprintf("target could not start command (exit %d): %v", e.ExitCode, e.Err)
	}
	return fmt.Sprintf("target could not start command: %v", e.Err)
}

func (e *StartError) Unwrap() error { return e.Err }

func ValidateRunRequest(req RunRequest) error {
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		return fmt.Errorf("argv must name a program")
	}
	for i, a := range req.Argv {
		if strings.IndexByte(a, 0) >= 0 {
			return fmt.Errorf("argv[%d] contains NUL", i)
		}
	}
	for k, v := range req.Env {
		if k == "" {
			return fmt.Errorf("environment key must not be empty")
		}
		if strings.IndexByte(k, '=') >= 0 {
			return fmt.Errorf("environment key %q contains '='", k)
		}
		if strings.IndexByte(k, 0) >= 0 {
			return fmt.Errorf("environment key %q contains NUL", k)
		}
		if strings.IndexByte(v, 0) >= 0 {
			return fmt.Errorf("environment value for %q contains NUL", k)
		}
	}
	return nil
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
