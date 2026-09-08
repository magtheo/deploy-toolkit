package target

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// attemptSchemaV1 identifies unresolved-attempt markers.
const attemptSchemaV1 = "toolkit.attempt/v1"

var attemptIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// ErrAttemptAbsent reports that no unresolved attempt marker exists for a
// project/environment pair — the normal, healthy condition.
var ErrAttemptAbsent = errors.New("attempt: no unresolved attempt")

// AttemptMarker is a durable, environment-scoped recovery fact. It is
// written by the lifecycle layer BEFORE the first consequential stage and
// removed only when the attempt has been reconciled to trusted observed
// state: observed state committed (by the attempt itself or by an
// explicit recovery rollback), or explicit recovery resolving it. A
// determined hook failure does NOT remove it — a migration can partially
// apply and then exit 1, so "known failure" is not "safe to repeat
// consequential work". Its presence therefore means:
//
//	a consequential deployment has NOT been reconciled to
//	trusted observed state; its outcome is UNRESOLVED.
//
// This is deliberately a different fact from the environment lock: the
// lock means "someone may be executing now"; the attempt marker means
// "the previous result is unresolved, recovery is required". Normal
// deployments refuse while it exists; explicit recovery (rollback)
// resolves it.
type AttemptMarker struct {
	Schema       string `json:"schema"`
	AttemptID    string `json:"attemptId"` // random 16-hex attempt identity
	Project      string `json:"project"`
	Environment  string `json:"environment"`
	FromRelease  string `json:"fromRelease"` // observed current before the attempt; "" = none
	ToRelease    string `json:"toRelease"`
	BundleDigest string `json:"bundleDigest"`
	StartedAt    string `json:"startedAt"` // RFC3339, UTC
}

func validateAttempt(m AttemptMarker, project, env string) error {
	if m.Schema != attemptSchemaV1 {
		return fmt.Errorf("schema %q, want %q", m.Schema, attemptSchemaV1)
	}
	if !attemptIDPattern.MatchString(m.AttemptID) {
		return fmt.Errorf("attemptId %q is not 16 hex digits", m.AttemptID)
	}
	if m.Project != project || m.Environment != env {
		return fmt.Errorf("records %s/%s, want %s/%s", m.Project, m.Environment, project, env)
	}
	if m.FromRelease != "" {
		if err := CheckVersion(m.FromRelease); err != nil {
			return fmt.Errorf("fromRelease: %w", err)
		}
	}
	if err := CheckVersion(m.ToRelease); err != nil {
		return fmt.Errorf("toRelease: %w", err)
	}
	if !digestPattern.MatchString(m.BundleDigest) {
		return fmt.Errorf("bundleDigest %q is not a sha256 digest", m.BundleDigest)
	}
	if _, err := time.Parse(time.RFC3339, m.StartedAt); err != nil {
		return fmt.Errorf("startedAt %q: %w", m.StartedAt, err)
	}
	return nil
}

// ReadAttempt returns the unresolved attempt marker, or ErrAttemptAbsent.
// The file must parse strictly and validate fully — a corrupt marker is
// not treated as absence.
func (t *Target) ReadAttempt(ctx context.Context, project, env string) (AttemptMarker, error) {
	path, err := t.layout.AttemptPath(project, env)
	if err != nil {
		return AttemptMarker{}, err
	}
	present, err := t.exists(ctx, path)
	if err != nil {
		return AttemptMarker{}, err
	}
	if !present {
		return AttemptMarker{}, fmt.Errorf("%s/%s: %w", project, env, ErrAttemptAbsent)
	}
	raw, err := t.ReadFile(ctx, path)
	if err != nil {
		return AttemptMarker{}, err
	}
	var m AttemptMarker
	if err := decodeStrictJSON(raw, &m); err != nil {
		return AttemptMarker{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := validateAttempt(m, project, env); err != nil {
		return AttemptMarker{}, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// WriteAttempt durably records an unresolved attempt (atomic publication).
func (t *Target) WriteAttempt(ctx context.Context, m AttemptMarker) error {
	path, err := t.layout.AttemptPath(m.Project, m.Environment)
	if err != nil {
		return err
	}
	m.Schema = attemptSchemaV1
	if err := validateAttempt(m, m.Project, m.Environment); err != nil {
		return fmt.Errorf("attempt marker: %w", err)
	}
	raw, err := marshalIndentJSON(m)
	if err != nil {
		return err
	}
	if err := t.tr.Put(ctx, putReq(path, raw, 0o644)); err != nil {
		return fmt.Errorf("writing attempt marker %s: %w", path, err)
	}
	return nil
}

// ClearAttempt removes the attempt marker once the attempt has reached a
// trusted terminal. Removal failure keeps the environment in
// recovery-required — conservative and loud.
func (t *Target) ClearAttempt(ctx context.Context, project, env string) error {
	path, err := t.layout.AttemptPath(project, env)
	if err != nil {
		return err
	}
	res, err := t.tr.Run(ctx, runReq("rm", "-f", path))
	if err != nil {
		return fmt.Errorf("clear attempt marker %s: %w", path, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("clear attempt marker %s: exit %d: %s (manual removal required)", path, res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}
