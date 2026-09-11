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

	// ProbePath answers exactly one question: what occupies this
	// absolute path on the target? PathAbsent is a POSITIVE fact,
	// returned only when absence has been proven — never as a fallback
	// for "the probe could not tell". Permission failures, broken
	// hierarchies (a non-directory where a directory is required) and
	// transport failures are errors. PathFile means any non-directory
	// object; a stricter special-file policy (devices, sockets, ...)
	// would be separate hardening and is deliberately not attempted
	// here. The probe follows symlinks (v0.1 preserves the historical
	// semantics; a target-tree symlink policy would also be separate
	// hardening).
	ProbePath(ctx context.Context, path string) (PathState, error)
}

// Implementations must return a meaningful PathState only when the
// error is nil; when the error is non-nil, callers must ignore the
// state value entirely (it may be the zero value, PathUnknown).

// PathState is the kind of object found at a probed path. It is
// deliberately not generic filesystem metadata: the lifecycle cares
// about evidence of presence, absence and directory-ness, and nothing
// else. Any state that cannot be positively established is an error,
// not a PathState.
//
// PathUnknown is deliberately the ZERO value, mirroring RunFate: a
// probe result whose state was never set — the classic
// `return transport.PathState{}, nil` wrapper mistake — claims
// NOTHING, never proven absence. Absence is a positive fact and must
// be set explicitly; consumers reject PathUnknown and any value outside
// the enum as the error it is.
type PathState int

const (
	// PathUnknown is the zero value: a probe that cannot speak has
	// claimed nothing, and every consumer must treat it as an error.
	PathUnknown   PathState = iota
	PathAbsent              // proven: no object occupies the path
	PathFile                // proven: a non-directory object occupies the path
	PathDirectory           // proven: a directory occupies the path
)

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
	// Fate is the execution-fate fact carried by EVERY Run return,
	// including error returns. The lifecycle must never infer "may
	// still be running" from arbitrary Go errors — it reads this
	// field. See the RunFate contract.
	Fate RunFate

	// ExitCode is meaningful only when Fate == RunExited.
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// RunFate states what is KNOWN about the execution of a Run request.
// It is the safety input for the lifecycle's lock policy: an
// environment lock must be retained exactly when a started hook's fate
// is RunUnknown, because no transport can prove that a process — let
// alone a descendant tree — has stopped.
//
// RunUnknown is deliberately the ZERO value: a transport that forgets
// to set Fate fails closed (lock retained), never open. The lifecycle
// additionally validates fate/error combinations, so an implementation
// that violates this contract cannot manufacture a releasable outcome.
//
// Every Run return carries a meaningful Fate, even alongside an error:
//
//   - RunNotStarted: no execution could have begun. Request validation
//     failed before anything was attempted, or the start itself
//     definitively failed (local fork/exec error; explicit exec-request
//     rejection over SSH). Nothing is running because of this call.
//     Must accompany a non-nil error.
//   - RunExited: the command reached a determined exit — an exit code
//     was observed (including nonzero, and including SSH 126/127
//     dispatch conventions). The direct command is finished; the
//     synchronous-hook contract (Consumer Contract v1) is what rules
//     out unmanaged descendants, not this fact alone. May accompany an
//     error (SSH 126/127 dispatch StartError).
//   - RunUnknown: execution MAY have begun and its fate cannot be
//     established: context cancellation after start, transport or
//     connection loss mid-run, an ambiguous exec request, or a local
//     Wait bounded by WaitDelay while a pipe-holding process survives.
//     Callers must treat the process as possibly still running: retain
//     the environment lock, never manufacture an exit code. Must
//     accompany a non-nil error.
type RunFate int

const (
	// RunUnknown is the zero value: fate defaults to the conservative
	// state, so an omitted Fate can never authorize lock release.
	RunUnknown RunFate = iota
	RunNotStarted
	RunExited
)

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
