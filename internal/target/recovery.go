package target

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// recoverySchemaV1 identifies unresolved-recovery markers.
const recoverySchemaV1 = "toolkit.recovery/v1"

// ErrRecoveryAbsent reports that no unresolved recovery marker exists for
// a project/environment pair — the normal, healthy condition.
var ErrRecoveryAbsent = errors.New("recovery: no unresolved recovery")

// authorization values recorded in recovery markers. Keep in lockstep
// with lifecycle.RollbackAuthorization, which is the only writer.
var recoveryAuthorizations = map[string]bool{
	"recovery": true,
	"manual":   true,
	"auto":     true,
}

// RecoveryMarker is the durable fact that a RECOVERY (rollback) operation
// was started and has not been reconciled. It is deliberately separate
// from AttemptMarker:
//
//	AttemptMarker   = WHY recovery is needed (a deployment A→B is unresolved)
//	RecoveryMarker  = WHAT recovery was started (a rollback B→A is unresolved)
//
// It is written before the first consequential rollback stage and removed
// only when the recovery has been reconciled to trusted observed state —
// proven by the observed state's operationId carrying THIS marker's
// recoveryId. Any failure after it exists keeps it: repeating a rollback
// hook or an apply is no safer than repeating a failed migration, and
// v0.1 assumes no hook idempotency contract. It is removed by exactly
// one of two authorities: the proven-committed self-heal (observed state
// carries this marker's recoveryId as its operationId, proven by the
// whole committed identity), or explicit operator resolution
// (lifecycle.Resolve) after the operator has verified the target by
// hand.
type RecoveryMarker struct {
	Schema           string `json:"schema"`
	RecoveryID       string `json:"recoveryId"`      // random 16-hex recovery identity
	SourceAttemptID  string `json:"sourceAttemptId"` // AttemptMarker.AttemptID being resolved; "" = emergency rollback
	Project          string `json:"project"`
	Environment      string `json:"environment"`
	FromRelease      string `json:"fromRelease"` // release being undone
	FromBundleDigest string `json:"fromBundleDigest"`
	ToRelease        string `json:"toRelease"` // release being restored
	ToBundleDigest   string `json:"toBundleDigest"`
	Authorization    string `json:"authorization"` // recovery | manual | auto
	StartedAt        string `json:"startedAt"`     // RFC3339, UTC
}

func validateRecovery(m RecoveryMarker, project, env string) error {
	if m.Schema != recoverySchemaV1 {
		return fmt.Errorf("schema %q, want %q", m.Schema, recoverySchemaV1)
	}
	if !attemptIDPattern.MatchString(m.RecoveryID) {
		return fmt.Errorf("recoveryId %q is not 16 hex digits", m.RecoveryID)
	}
	if m.SourceAttemptID != "" && !attemptIDPattern.MatchString(m.SourceAttemptID) {
		return fmt.Errorf("sourceAttemptId %q is not 16 hex digits", m.SourceAttemptID)
	}
	if m.Project != project || m.Environment != env {
		return fmt.Errorf("records %s/%s, want %s/%s", m.Project, m.Environment, project, env)
	}
	if err := CheckVersion(m.FromRelease); err != nil {
		return fmt.Errorf("fromRelease: %w", err)
	}
	if !digestPattern.MatchString(m.FromBundleDigest) {
		return fmt.Errorf("fromBundleDigest %q is not a sha256 digest", m.FromBundleDigest)
	}
	if err := CheckVersion(m.ToRelease); err != nil {
		return fmt.Errorf("toRelease: %w", err)
	}
	if !digestPattern.MatchString(m.ToBundleDigest) {
		return fmt.Errorf("toBundleDigest %q is not a sha256 digest", m.ToBundleDigest)
	}
	if !recoveryAuthorizations[m.Authorization] {
		return fmt.Errorf("authorization %q is not one of [recovery, manual, auto]", m.Authorization)
	}
	if _, err := time.Parse(time.RFC3339, m.StartedAt); err != nil {
		return fmt.Errorf("startedAt %q: %w", m.StartedAt, err)
	}
	return nil
}

// ReadRecovery returns the unresolved recovery marker, or ErrRecoveryAbsent.
// The file must parse strictly and validate fully — a corrupt marker is
// not treated as absence.
func (t *Target) ReadRecovery(ctx context.Context, project, env string) (RecoveryMarker, error) {
	path, err := t.layout.RecoveryPath(project, env)
	if err != nil {
		return RecoveryMarker{}, err
	}
	present, err := t.exists(ctx, path)
	if err != nil {
		return RecoveryMarker{}, err
	}
	if !present {
		return RecoveryMarker{}, fmt.Errorf("%s/%s: %w", project, env, ErrRecoveryAbsent)
	}
	raw, err := t.ReadFile(ctx, path)
	if err != nil {
		return RecoveryMarker{}, err
	}
	var m RecoveryMarker
	if err := decodeStrictJSON(raw, &m); err != nil {
		return RecoveryMarker{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := validateRecovery(m, project, env); err != nil {
		return RecoveryMarker{}, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// WriteRecovery durably records an unresolved recovery attempt (atomic
// publication).
func (t *Target) WriteRecovery(ctx context.Context, m RecoveryMarker) error {
	path, err := t.layout.RecoveryPath(m.Project, m.Environment)
	if err != nil {
		return err
	}
	m.Schema = recoverySchemaV1
	if err := validateRecovery(m, m.Project, m.Environment); err != nil {
		return fmt.Errorf("recovery marker: %w", err)
	}
	raw, err := marshalIndentJSON(m)
	if err != nil {
		return err
	}
	if err := t.tr.Put(ctx, putReq(path, raw, 0o644)); err != nil {
		return fmt.Errorf("writing recovery marker %s: %w", path, err)
	}
	return nil
}

// ClearRecovery removes the recovery marker once the recovery has been
// reconciled to trusted observed state. Removal failure keeps the
// environment blocked — conservative and loud; the marker is healable
// through the observed state's operationId, so a leftover is safe.
func (t *Target) ClearRecovery(ctx context.Context, project, env string) error {
	path, err := t.layout.RecoveryPath(project, env)
	if err != nil {
		return err
	}
	res, err := t.tr.Run(ctx, runReq("rm", "-f", path))
	if err != nil {
		return fmt.Errorf("clear recovery marker %s: %w", path, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("clear recovery marker %s: exit %d: %s (manual removal required)", path, res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}
