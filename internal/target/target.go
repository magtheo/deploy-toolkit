// Package target is the server-side substrate the deployment state machine
// consumes: deterministic target paths, crash-aware release staging, the
// observed-state snapshot and the durable history log.
//
// It sits below lifecycle orchestration: nothing here runs hooks or decides
// that state may advance. Staging a release never changes observed state —
// staged is not deployed.
package target

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/transport"
)

type Target struct {
	tr     transport.Transport
	layout Layout
}

// New binds a transport to the toolkit-owned subtree of a target.
// deployRoot comes from the Target manifest's spec.deployRoot and is
// re-validated here; manifest validation never travels with the bytes.
func New(tr transport.Transport, deployRoot string) (*Target, error) {
	layout, err := NewLayout(deployRoot)
	if err != nil {
		return nil, err
	}
	return &Target{tr: tr, layout: layout}, nil
}

// probePath classifies a toolkit-owned target path. The transport probe
// returns proven absence, proven presence (with kind), or an error —
// "could not establish" is never mapped to absence. The wrapped error
// is deliberately opaque about cause: callers classify ERRORS as
// infrastructure/unreadable and only PathAbsent as evidence absence.
func (t *Target) probePath(ctx context.Context, path string) (transport.PathState, error) {
	st, err := t.tr.ProbePath(ctx, path)
	if err != nil {
		return transport.PathUnknown, fmt.Errorf("target: probe %s: %w", path, err)
	}
	// Validate the enum instead of trusting implementations: the zero
	// value (PathUnknown) and any value outside this version's enum
	// mean the transport claimed NOTHING — that is an error, never
	// absence. Mirrors the RunFate validation in the lifecycle.
	switch st {
	case transport.PathAbsent, transport.PathFile, transport.PathDirectory:
		return st, nil
	default:
		return transport.PathUnknown, fmt.Errorf("target: probe %s: transport returned unproven state %d (contract violation, treated as error)", path, int(st))
	}
}

// absent is the one shared decision for evidence paths: only a
// transport-PROVEN absence may become an Err*Absent sentinel. A
// directory where evidence is expected, a permission failure, a broken
// hierarchy and a dead transport are all errors.
func (t *Target) absent(ctx context.Context, path string) (bool, error) {
	st, err := t.probePath(ctx, path)
	switch {
	case err != nil:
		return false, err
	case st == transport.PathAbsent:
		return true, nil
	case st == transport.PathDirectory:
		return false, fmt.Errorf("%s is a directory where evidence was expected (broken target hierarchy)", path)
	default:
		return false, nil
	}
}

// Exists reports whether ANY object occupies a toolkit-owned path.
// Its contract is strict: true, nil = positively present; false, nil =
// positively absent; a non-nil error = could not establish. Type-aware
// callers (the lock) use ProbePath instead, because "something occupies
// this pathname" is not evidence that an environment is locked.
func (t *Target) Exists(ctx context.Context, path string) (bool, error) {
	st, err := t.probePath(ctx, path)
	if err != nil {
		return false, err
	}
	return st != transport.PathAbsent, nil
}

// ProbePath exposes the transport's proven presence/absence/kind probe
// for type-aware substrate consumers (the status lock classification).
func (t *Target) ProbePath(ctx context.Context, path string) (transport.PathState, error) {
	return t.probePath(ctx, path)
}

// ReadFile reads a toolkit-owned file from the target — staged contract
// bytes, state snapshots, history logs. Callers must gate it behind the
// path/evidence probe (probePath / absent) when absence is meaningful:
// cat reports both absence and unreadability as a non-zero exit and the
// two cannot be distinguished through the transport contract.
func (t *Target) ReadFile(ctx context.Context, path string) ([]byte, error) {
	res, err := t.tr.Run(ctx, transport.RunRequest{Argv: []string{"cat", path}, Dir: "/"})
	if err != nil {
		return nil, fmt.Errorf("target: read %s: %w", path, err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("target: read %s: exit %d: %s", path, res.ExitCode, firstLine(res.Stderr))
	}
	return res.Stdout, nil
}

// Layout exposes the derived path layout (lock paths, release dirs) for
// the lifecycle layer.
func (t *Target) Layout() Layout { return t.layout }

// Transport exposes the underlying transport for lifecycle operations
// (environment lock acquisition, hook execution) that are outside the
// substrate's own responsibilities.
func (t *Target) Transport() transport.Transport { return t.tr }

func marshalIndentJSON(v any) ([]byte, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func putReq(path string, content []byte, mode os.FileMode) transport.PutRequest {
	return transport.PutRequest{Path: path, Content: content, Mode: mode}
}

func runReq(argv ...string) transport.RunRequest {
	return transport.RunRequest{Argv: argv, Dir: "/"}
}

func firstLine(b []byte) string {
	for i, c := range b {
		if c == '\n' {
			return string(b[:i])
		}
	}
	return string(b)
}

// FromManifest binds a transport to a parsed Target manifest.
func FromManifest(tr transport.Transport, mt *manifest.Target) (*Target, error) {
	return New(tr, mt.Spec.DeployRoot)
}
