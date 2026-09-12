package lifecycle

// M1-10 boundary matrix: cancellation and transport-loss behavior at
// each lifecycle boundary. This file covers the boundaries whose exact
// shape was not yet pinned elsewhere; the rest of the matrix reuses
// existing proof (noted in docs/m1-10-concurrency-verification.md).

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

// cancelOnPutTransport cancels the caller context when a Put for a
// matching path arrives, then fails the Put — cancellation racing the
// durable write.
type cancelOnPutTransport struct {
	inner  transport.Transport
	suffix string
	cancel context.CancelFunc
}

func (t *cancelOnPutTransport) Put(ctx context.Context, req transport.PutRequest) error {
	if strings.HasSuffix(req.Path, t.suffix) {
		t.cancel()
		return t.inner.Put(ctx, req)
	}
	return t.inner.Put(ctx, req)
}

func (t *cancelOnPutTransport) ProbePath(ctx context.Context, path string) (transport.PathState, error) {
	return t.inner.ProbePath(ctx, path)
}

func (t *cancelOnPutTransport) Run(ctx context.Context, req transport.RunRequest) (transport.RunResult, error) {
	return t.inner.Run(ctx, req)
}

// Cancellation at the ATTEMPT MARKER boundary: the write fails, no
// consequential stage has run, no marker exists — infrastructure
// failure with the lock cleanly released (nothing can be running).
func TestBoundaryCancelAtAttemptMarkerWrite(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tgt, err := target.New(&cancelOnPutTransport{
		inner:  local.New(),
		suffix: "/attempts/production.json",
		cancel: cancel,
	}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Deploy(ctx, DeployInput{
		Target: tgt, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "test",
	})
	if err == nil {
		t.Fatalf("rep = %+v, want the marker-write infrastructure failure", rep)
	}
	if !strings.Contains(err.Error(), "attempt marker could not be persisted") {
		t.Errorf("err = %v, want the marker boundary named", err)
	}
	if rep.ConsequentialStarted {
		t.Errorf("rep = %+v, the durable boundary was never crossed", rep)
	}
	if attemptPresent(t, f) {
		t.Error("no marker may exist when its own write failed")
	}
	requireNoLock(t, f) // nothing can be running: clean release
}

// Cancellation between verify success and the state commit: the commit
// fails on the dead context, so the outcome stays UNCERTAIN — the
// marker survives, recovery is required, and the hook fate was
// determined so the lock is released normally.
func TestBoundaryCancelAtStateCommit(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Cancel when the observed-state Put arrives — verify has already
	// exited 0 by then (stages precede the commit).
	tgt, err := target.New(&cancelOnPutTransport{
		inner:  local.New(),
		suffix: "/state/production.json",
		cancel: cancel,
	}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Deploy(ctx, DeployInput{
		Target: tgt, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "test",
	})
	if err == nil {
		t.Fatalf("rep = %+v, want the commit failure", rep)
	}
	if !strings.Contains(err.Error(), "observed state commit failed") {
		t.Errorf("err = %v, want the commit boundary named", err)
	}
	if rep.Committed || !rep.RecoveryRequired || !attemptPresent(t, f) {
		t.Errorf("rep = %+v, want uncertain: committed=false, recovery required, marker present", rep)
	}
	requireNoLock(t, f) // verify's fate was determined; release is safe
}

// Marker-cleanup failure at the trusted terminal: the state IS
// committed, but the attempt marker could not be removed — the
// leftover is exactly the self-heal crash window, and the next deploy
// must heal it instead of demanding recovery.
func TestBoundaryMarkerCleanupFailureAtTerminal(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")

	// rm -f on the attempt marker fails; the state Put must succeed.
	failAttemptRm := &failingRunTransport{
		inner: local.New(),
		failRunWhen: func(argv []string) bool {
			return argv[0] == "rm" && strings.Contains(argv[len(argv)-1], "/attempts/")
		},
	}
	tgt, err := target.New(failAttemptRm, f.root)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Deploy(t.Context(), DeployInput{
		Target: tgt, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "test",
	})
	if err == nil {
		t.Fatalf("rep = %+v, want the cleanup failure", rep)
	}
	if !rep.Committed {
		t.Errorf("rep = %+v, the state commit must stand", rep)
	}
	if !attemptPresent(t, f) {
		t.Fatal("the attempt marker must survive its failed cleanup")
	}
	requireNoLock(t, f)

	// The retry recognizes the committed observation signed by the
	// SAME attempt id and heals the leftover.
	retry, err := deploy(t, f, "my-app", "1.0.0", nil)
	if err != nil || !retry.AlreadyCurrent {
		t.Fatalf("retry = %+v err = %v, want self-healed already-current", retry, err)
	}
	requireNoLock(t, f)
}

// Cancellation before any Run: nothing executed, lock released, clean
// infrastructure failure (the whole operation is a no-op).
func TestBoundaryCancelBeforeFirstRun(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rep, err := Deploy(ctx, DeployInput{
		Target: f.target, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "test",
	})
	if err == nil {
		t.Fatalf("rep = %+v, want a cancellation error", rep)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if len(order(t, f.marker)) != 0 {
		t.Error("hooks ran despite a pre-cancelled context")
	}
	requireNoLock(t, f)
}

// Boundary-matrix invariant A in engine terms: a surviving lock must be
// EXPLAINED by deliberate retention or a failed release — never silent.
func TestNoUnexplainedSurvivingLock(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")
	tgt, err := target.New(&unknownFateTransport{
		inner:     local.New(),
		unknownIn: func(argv []string) bool { return strings.Contains(argv[0], "apply.sh") },
	}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Deploy(t.Context(), DeployInput{
		Target: tgt, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "test",
	})
	if err == nil {
		t.Fatal("want the unknown-fate failure")
	}
	lockSurvives := func() bool {
		_, statErr := os.Stat(f.lockDir())
		return !os.IsNotExist(statErr)
	}
	if lockSurvives() && !rep.LockRetained && !errors.Is(err, ErrLockReleaseFailed) {
		t.Fatalf("unexplained surviving lock: rep = %+v err = %v", rep, err)
	}
	if !lockSurvives() || !rep.LockRetained {
		t.Fatalf("rep = %+v, want deliberate retention with the lock surviving", rep)
	}
}
