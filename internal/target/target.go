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
	"fmt"

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

// exists reports whether path exists on the target. test is a POSIX
// utility: exit 0 = true, 1 = false, anything else is a target error.
func (t *Target) exists(ctx context.Context, path string) (bool, error) {
	res, err := t.tr.Run(ctx, transport.RunRequest{Argv: []string{"test", "-e", path}, Dir: "/"})
	if err != nil {
		return false, fmt.Errorf("target: test -e %s: %w", path, err)
	}
	switch res.ExitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("target: test -e %s: exit %d: %s", path, res.ExitCode, firstLine(res.Stderr))
	}
}

// readFile reads a toolkit-owned file from the target. Callers must gate it
// behind exists() when absence is meaningful: cat reports both absence and
// unreadability as a non-zero exit and the two cannot be distinguished
// through the transport contract.
func (t *Target) readFile(ctx context.Context, path string) ([]byte, error) {
	res, err := t.tr.Run(ctx, transport.RunRequest{Argv: []string{"cat", path}, Dir: "/"})
	if err != nil {
		return nil, fmt.Errorf("target: read %s: %w", path, err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("target: read %s: exit %d: %s", path, res.ExitCode, firstLine(res.Stderr))
	}
	return res.Stdout, nil
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
