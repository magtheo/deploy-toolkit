package target

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/transport"
)

// stateSchemaV1 identifies observed-state snapshots.
const stateSchemaV1 = "toolkit.state/v1"

// digestPattern pins the observed bundle digest shape: sha256, lowercase
// hex, 64 digits — the same representation the bundle builder emits.
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ErrStateAbsent reports that no deployment has ever been observed for a
// project/environment pair. It is a normal condition for a fresh target,
// not a failure.
var ErrStateAbsent = errors.New("observed state: no deployment recorded")

// ErrEvidenceInvalid marks strict-validation failures on durable target
// files (state snapshot, attempt/recovery markers): the file EXISTS and
// was read, but does not decode or validate. This is structurally
// different from absence (normal) and corruption-by-transport (an error
// mid-read surfaces as a transport error). Invalid evidence fails
// closed: every operational command (deploy, rollback, recovery
// resolve) classifies it as a refusal to act — the facts cannot be
// trusted, so nothing may be decided and rerunning cannot help until a
// human has inspected the evidence and verified the target's actual
// state. It must never encourage editing observed state merely to make
// an operation proceed.
var ErrEvidenceInvalid = errors.New("evidence failed strict validation")

// operationIDPattern pins the operationId shape: which kind of lifecycle
// operation committed this observation, and that operation's durable ID
// (attempt or recovery marker id). Empty means "committed before the
// operationId contract existed" — readable, never fabricated.
var operationIDPattern = regexp.MustCompile(`^(deploy|recovery):[0-9a-f]{16}$`)

// CurrentDeployment is the observed fact "this environment is (believed to
// be) running this release". It is written by the deployment state machine
// after lifecycle verification — never by staging.
type CurrentDeployment struct {
	Release      string `json:"release"`
	BundleDigest string `json:"bundleDigest"`
	Since        string `json:"since"` // RFC3339, UTC — when the observation was recorded
	// OperationID names the lifecycle operation that committed this
	// observation: "deploy:<attemptId>" or "recovery:<recoveryId>". It is
	// the proof that resolves the crash window where a committed state is
	// observationally identical to the pre-operation state (e.g. a
	// rollback to A looks the same before and after, because observed
	// already said A): only when the operationId matches the unresolved
	// marker's id has THIS operation demonstrably committed.
	OperationID string `json:"operationId,omitempty"`
}

// State is an atomic snapshot of OBSERVED state, not desired state. It
// records what the toolkit last verified on the target; it makes no claim
// about what should be running. It carries digests and timestamps only —
// application secrets never enter state files.
type State struct {
	Schema      string             `json:"schema"`
	Project     string             `json:"project"`
	Environment string             `json:"environment"`
	Current     *CurrentDeployment `json:"current"`   // nil = nothing observed deployed
	UpdatedAt   string             `json:"updatedAt"` // RFC3339, UTC
}

// validateState is the single strict validator for observed-state
// snapshots, applied identically to reads and writes. Step 9 makes
// decisions from observed state, so a read must never trust fields the
// write path would have rejected: schema, exact identity, strict SemVer
// for the deployed release, well-formed digest, parseable timestamps.
// Anything else fails closed.
func validateState(st State, project, env string) error {
	if st.Schema != stateSchemaV1 {
		return fmt.Errorf("schema %q, want %q", st.Schema, stateSchemaV1)
	}
	if st.Project != project || st.Environment != env {
		return fmt.Errorf("records %s/%s, refusing to serve it as %s/%s", st.Project, st.Environment, project, env)
	}
	if _, err := time.Parse(time.RFC3339, st.UpdatedAt); err != nil {
		return fmt.Errorf("updatedAt %q: %w", st.UpdatedAt, err)
	}
	if st.Current == nil {
		return nil
	}
	if err := CheckVersion(st.Current.Release); err != nil {
		return fmt.Errorf("current.release: %w", err)
	}
	if !digestPattern.MatchString(st.Current.BundleDigest) {
		return fmt.Errorf("current.bundleDigest %q is not a sha256 digest", st.Current.BundleDigest)
	}
	if _, err := time.Parse(time.RFC3339, st.Current.Since); err != nil {
		return fmt.Errorf("current.since %q: %w", st.Current.Since, err)
	}
	if st.Current.OperationID != "" && !operationIDPattern.MatchString(st.Current.OperationID) {
		return fmt.Errorf("current.operationId %q is not an operation-qualified id", st.Current.OperationID)
	}
	return nil
}

// decodeStrictJSON decodes exactly one JSON value into v, rejecting
// unknown fields and any trailing content after the first value. A state
// file of "{valid}{smuggled}" must not pass as "{valid}".
func decodeStrictJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("trailing JSON after the first value")
	}
	return nil
}

// ReadState returns the observed state snapshot for project/env. The file
// must pass the same strict validation as a fresh write — unknown fields
// and trailing content are rejected, not silently dropped — because the
// snapshot is input to lifecycle decisions.
func (t *Target) ReadState(ctx context.Context, project, env string) (State, error) {
	path, err := t.layout.StatePath(project, env)
	if err != nil {
		return State{}, err
	}
	absent, err := t.absent(ctx, path)
	if err != nil {
		return State{}, err
	}
	if absent {
		return State{}, fmt.Errorf("%s/%s: %w", project, env, ErrStateAbsent)
	}
	raw, err := t.ReadFile(ctx, path)
	if err != nil {
		return State{}, err
	}
	var st State
	if err := decodeStrictJSON(raw, &st); err != nil {
		return State{}, fmt.Errorf("%s: %w: %w", path, err, ErrEvidenceInvalid)
	}
	if err := validateState(st, project, env); err != nil {
		return State{}, fmt.Errorf("%s: %w: %w", path, err, ErrEvidenceInvalid)
	}
	return st, nil
}

// WriteState atomically replaces the observed state snapshot after strict
// validation — the same validator reads apply. The schema field is owned
// by the toolkit and set here.
func (t *Target) WriteState(ctx context.Context, st State) error {
	st.Schema = stateSchemaV1
	if err := validateState(st, st.Project, st.Environment); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	path, err := t.layout.StatePath(st.Project, st.Environment)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := t.tr.Put(ctx, transport.PutRequest{Path: path, Content: raw, Mode: 0o644}); err != nil {
		return fmt.Errorf("writing state %s: %w", path, err)
	}
	return nil
}
