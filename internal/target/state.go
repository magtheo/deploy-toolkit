package target

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/transport"
)

// stateSchemaV1 identifies observed-state snapshots.
const stateSchemaV1 = "toolkit.state/v1"

// ErrStateAbsent reports that no deployment has ever been observed for a
// project/environment pair. It is a normal condition for a fresh target,
// not a failure.
var ErrStateAbsent = errors.New("observed state: no deployment recorded")

// CurrentDeployment is the observed fact "this environment is (believed to
// be) running this release". It is written by the deployment state machine
// after lifecycle verification — never by staging.
type CurrentDeployment struct {
	Release      string `json:"release"`
	BundleDigest string `json:"bundleDigest"`
	Since        string `json:"since"` // RFC3339, UTC — when the observation was recorded
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

// ReadState returns the observed state snapshot for project/env.
func (t *Target) ReadState(ctx context.Context, project, env string) (State, error) {
	path, err := t.layout.StatePath(project, env)
	if err != nil {
		return State{}, err
	}
	present, err := t.exists(ctx, path)
	if err != nil {
		return State{}, err
	}
	if !present {
		return State{}, fmt.Errorf("%s/%s: %w", project, env, ErrStateAbsent)
	}
	raw, err := t.readFile(ctx, path)
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return State{}, fmt.Errorf("%s: %w", path, err)
	}
	if st.Schema != stateSchemaV1 {
		return State{}, fmt.Errorf("%s: schema %q, want %q", path, st.Schema, stateSchemaV1)
	}
	if st.Project != project || st.Environment != env {
		return State{}, fmt.Errorf("%s: records %s/%s, refusing to serve it as %s/%s", path, st.Project, st.Environment, project, env)
	}
	return st, nil
}

// WriteState atomically replaces the observed state snapshot. Validation
// fails closed: identities must be well-formed and timestamps must parse,
// so a garbage snapshot can never be laundered onto a target.
func (t *Target) WriteState(ctx context.Context, st State) error {
	path, err := t.layout.StatePath(st.Project, st.Environment)
	if err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339, st.UpdatedAt); err != nil {
		return fmt.Errorf("state updatedAt %q: %w", st.UpdatedAt, err)
	}
	if st.Current != nil {
		if st.Current.Release == "" {
			return fmt.Errorf("state current.release must not be empty")
		}
		if st.Current.BundleDigest == "" {
			return fmt.Errorf("state current.bundleDigest must not be empty")
		}
		if _, err := time.Parse(time.RFC3339, st.Current.Since); err != nil {
			return fmt.Errorf("state current.since %q: %w", st.Current.Since, err)
		}
	}
	st.Schema = stateSchemaV1
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
