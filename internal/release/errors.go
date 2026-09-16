package release

// InfraError marks an evaluation failure caused by infrastructure — GitHub
// API or Git reads, the check-runs API, OCI resolution, bundle construction —
// as opposed to a policy verdict about the release under evaluation.
// Consumers separate ERROR from INVALID structurally via errors.As, never by
// parsing messages. Errors about candidate content (eligibility, binding,
// manifest semantics) are plain errors and stay policy verdicts.
type InfraError struct {
	Err error
}

func (e *InfraError) Error() string { return e.Err.Error() }

func (e *InfraError) Unwrap() error { return e.Err }

func infraErr(err error) error { return &InfraError{Err: err} }
