package lifecycle

// Execution-fate tests: the environment lock is retained exactly when a
// started hook's fate is RunUnknown — preflight included — and released
// for every determined or not-started outcome. These tests encode the
// M1-1 boundary: no transport can prove a process tree has stopped, so
// unknown fate must behave like a controlled crash.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/magtheo/deploy-toolkit/internal/target"
	"github.com/magtheo/deploy-toolkit/internal/transport"
	"github.com/magtheo/deploy-toolkit/internal/transport/local"
)

// unknownFateTransport reports RunUnknown (with cancellation) for
// selected hook programs; everything else passes through.
type unknownFateTransport struct {
	inner     transport.Transport
	unknownIn func(argv []string) bool
}

func (t *unknownFateTransport) Put(ctx context.Context, req transport.PutRequest) error {
	return t.inner.Put(ctx, req)
}

func (t *unknownFateTransport) ProbePath(ctx context.Context, path string) (transport.PathState, error) {
	return t.inner.ProbePath(ctx, path)
}

func (t *unknownFateTransport) Run(ctx context.Context, req transport.RunRequest) (transport.RunResult, error) {
	if t.unknownIn(req.Argv) {
		return transport.RunResult{Fate: transport.RunUnknown, Stderr: []byte("partial output before the connection was lost")}, context.Canceled
	}
	return t.inner.Run(ctx, req)
}

func fateTarget(t *testing.T, f *fixture, match func(argv []string) bool) *target.Target {
	t.Helper()
	tgt, err := target.New(&unknownFateTransport{inner: local.New(), unknownIn: match}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	return tgt
}

func assertLockRetained(t *testing.T, f *fixture) {
	t.Helper()
	if _, err := os.Stat(f.lockDir()); err != nil {
		t.Fatalf("environment lock must survive an unknown-fate hook (stat err = %v)", err)
	}
}

// Preflight runs BEFORE the attempt marker: an unknown fate there is
// infrastructure-failure shape (no marker to resolve) but STILL retains
// the lock — a possibly-running hook process is exactly what the lock
// guards, regardless of the durable boundary. After the operator
// removes the lock, a retry succeeds: no marker exists, nothing to
// resolve.
func TestPreflightUnknownFateRetainsLock(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")
	tgt := fateTarget(t, f, func(argv []string) bool { return strings.Contains(argv[0], "preflight.sh") })
	rep, err := Deploy(t.Context(), DeployInput{
		Target: tgt, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "test",
	})
	if err == nil {
		t.Fatalf("rep = %+v, want an error", rep)
	}
	if !rep.LockRetained || rep.RecoveryRequired || rep.ConsequentialStarted {
		t.Errorf("rep = %+v, want retained lock, no marker, infra shape", rep)
	}
	if attemptPresent(t, f) {
		t.Error("preflight unknown fate must not fabricate an attempt marker")
	}
	assertLockRetained(t, f)

	// The retained lock refuses the retry before anything runs.
	if _, err := Deploy(t.Context(), DeployInput{
		Target: f.target, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "test",
	}); !errors.Is(err, ErrEnvLockHeld) {
		t.Fatalf("retry err = %v, want held-lock refusal while the retained lock exists", err)
	}

	// Operator procedure: verify, remove the lock by hand. Then the
	// retry succeeds — no marker existed, so this stays retryable.
	if err := os.RemoveAll(f.lockDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := deploy(t, f, "my-app", "1.0.0", nil); err != nil {
		t.Fatalf("retry after lock removal: %v", err)
	}
	requireNoLock(t, f)
}

// Post-boundary unknown fates (migrate and verify here; apply is covered
// by the transport-loss tests) retain the lock AND leave the attempt
// marker: uncertain + recovery-required. The recovery resolve is
// refused while the retained lock exists.
func TestPostBoundaryUnknownFateRetainsLockAndMarker(t *testing.T) {
	for _, stage := range []string{"migrate.sh", "verify.sh"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t, "my-app", nil)
			rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")
			tgt := fateTarget(t, f, func(argv []string) bool { return strings.Contains(argv[0], stage) })
			rep, err := Deploy(t.Context(), DeployInput{
				Target: tgt, TargetManifest: f.targetManifest(),
				Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "test",
			})
			if err == nil {
				t.Fatalf("rep = %+v, want an error", rep)
			}
			if !rep.LockRetained {
				t.Errorf("rep = %+v, want retained lock", rep)
			}
			if !rep.RecoveryRequired || !rep.ConsequentialStarted {
				t.Errorf("rep = %+v, want uncertain classification facts", rep)
			}
			if !attemptPresent(t, f) {
				t.Error("the attempt marker must survive")
			}
			assertLockRetained(t, f)

			// recovery resolve is refused while the lock is retained.
			if _, err := Resolve(t.Context(), resolveInput(t, f, "", rep.AttemptID)); !errors.Is(err, ErrEnvLockHeld) {
				t.Fatalf("resolve err = %v, want held-lock refusal", err)
			}
		})
	}
}

// The rollback hook runs via its own stage step; an unknown fate there
// retains the lock alongside the recovery evidence.
func TestRollbackHookUnknownFateRetainsLock(t *testing.T) {
	f := newFixture(t, "my-app", versionTaggedHooks)
	revA := f.revision
	relA, bytesA := f.preparedBytes(t, "my-app", "1.0.0")
	if _, err := Deploy(t.Context(), DeployInput{
		Target: f.target, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: relA, Bundle: bytesA, Owner: "test",
	}); err != nil {
		t.Fatal(err)
	}
	revB := secondRevision(t, f)
	relB, bytesB := f.preparedBytesAt(t, revB, "my-app", "2.0.0")
	if _, err := Deploy(t.Context(), DeployInput{
		Target: f.target, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "2.0.0"), Release: relB, Bundle: bytesB, Owner: "test",
	}); err != nil {
		t.Fatal(err)
	}
	// The FROM release's rollback hook goes fate-unknown. Staged hook
	// argv is relative ("./deploy/rollback.sh"); only the rollback
	// operation ever executes this role, so the role name is enough.
	tgt := fateTarget(t, f, func(argv []string) bool {
		return strings.Contains(argv[0], "rollback.sh")
	})
	in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
	in.Target = tgt
	rep, err := Rollback(t.Context(), in)
	if err == nil {
		t.Fatalf("rep = %+v, want an error", rep)
	}
	if !rep.LockRetained {
		t.Errorf("rep = %+v, want retained lock", rep)
	}
	if !rep.RecoveryStarted || !rep.RecoveryRequired {
		t.Errorf("rep = %+v, want uncertain recovery facts", rep)
	}
	if !recoveryPresent(t, f) {
		t.Error("the recovery marker must survive")
	}
	assertLockRetained(t, f)

	// A second recovery is refused while the retained lock exists.
	in2 := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
	if _, err := Rollback(t.Context(), in2); !errors.Is(err, ErrEnvLockHeld) {
		t.Fatalf("retry err = %v, want held-lock refusal", err)
	}
}

// A definite not-started failure releases the lock: nothing can be
// running. The post-boundary marker still survives (determined
// infrastructure failure after the durable boundary).
func TestNotStartedFateReleasesLock(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")
	notStarted := transport.RunNotStarted
	tr := &failingRunTransport{
		inner:    local.New(),
		fate:     &notStarted,
		failWhen: func(argv0 string) bool { return strings.Contains(argv0, "apply.sh") },
	}
	tgt, err := target.New(tr, f.root)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Deploy(t.Context(), DeployInput{
		Target: tgt, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "test",
	})
	if err == nil {
		t.Fatalf("rep = %+v, want an error", rep)
	}
	if rep.LockRetained {
		t.Errorf("rep = %+v, want NO retention for a not-started fate", rep)
	}
	if !rep.ConsequentialStarted || !attemptPresent(t, f) {
		t.Error("the post-boundary marker must survive a not-started stage failure")
	}
	requireNoLock(t, f)
}

// A determined nonzero hook exit is a hook OUTCOME, not a fate problem:
// normal lock release (covered end-to-end elsewhere; pinned here for
// the retention flag specifically).
func TestDeterminedFailureReleasesLock(t *testing.T) {
	f := newFixture(t, "my-app", func(marker, envFile, script string) string {
		if script == "apply" {
			return "#!/bin/sh\nexit 9\n"
		}
		return "#!/bin/sh\nexit 0\n"
	})
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")
	rep, err := Deploy(t.Context(), DeployInput{
		Target: f.target, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "test",
	})
	if err != nil || rep.FailureReason == "" {
		t.Fatalf("rep = %+v err = %v, want determined failure report", rep, err)
	}
	if rep.LockRetained {
		t.Errorf("rep = %+v, a determined exit must never retain the lock", rep)
	}
	requireNoLock(t, f)
}

// contractViolator returns an arbitrary (res, err) pair for matching
// hooks — the transport-contract-failure shapes.
type contractViolator struct {
	inner transport.Transport
	when  func(argv []string) bool
	res   transport.RunResult
	err   error
}

func (t *contractViolator) Put(ctx context.Context, req transport.PutRequest) error {
	return t.inner.Put(ctx, req)
}

func (t *contractViolator) ProbePath(ctx context.Context, path string) (transport.PathState, error) {
	return t.inner.ProbePath(ctx, path)
}

func (t *contractViolator) Run(ctx context.Context, req transport.RunRequest) (transport.RunResult, error) {
	if t.when(req.Argv) {
		return t.res, t.err
	}
	return t.inner.Run(ctx, req)
}

// Every fate combination that is not a proven not-started or a
// determined exit must fail closed: zero-value RunResult (the classic
// wrapper mistake), unknown fate with nil error, and unknown-or-invalid
// fate values — all retain the lock and never look like a successful
// or determined hook.
func TestFateContractViolationsFailClosed(t *testing.T) {
	tests := []struct {
		name string
		res  transport.RunResult
		err  error
	}{
		{"zero-value RunResult + error", transport.RunResult{}, errors.New("connection lost")},
		{"unknown fate + nil error", transport.RunResult{Fate: transport.RunUnknown}, nil},
		{"invalid fate + nil error", transport.RunResult{Fate: transport.RunFate(42)}, nil},
		{"invalid fate + error", transport.RunResult{Fate: transport.RunFate(7)}, errors.New("connection lost")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "my-app", nil)
			rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")
			tgt, err := target.New(&contractViolator{
				inner: local.New(),
				when:  func(argv []string) bool { return strings.Contains(argv[0], "preflight.sh") },
				res:   tc.res,
				err:   tc.err,
			}, f.root)
			if err != nil {
				t.Fatal(err)
			}
			rep, err := Deploy(t.Context(), DeployInput{
				Target: tgt, TargetManifest: f.targetManifest(),
				Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "test",
			})
			if err == nil {
				t.Fatalf("rep = %+v, want an error", rep)
			}
			if !rep.LockRetained {
				t.Errorf("rep = %+v, want the lock retained (fail closed)", rep)
			}
			if attemptPresent(t, f) {
				t.Error("preflight contract violation must not fabricate an attempt marker")
			}
			assertLockRetained(t, f)
		})
	}
}
