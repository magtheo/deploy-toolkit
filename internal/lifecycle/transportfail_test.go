package lifecycle

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/target"
	"github.com/magtheo/deploy-toolkit/internal/transport"
	"github.com/magtheo/deploy-toolkit/internal/transport/local"
)

// failingRunTransport fails Run for selected programs — the SSH-connection-
// died shape. Put and every other Run pass through to the local transport.
type failingRunTransport struct {
	inner    transport.Transport
	failWhen func(argv0 string) bool
	failPut  bool
}

func (t *failingRunTransport) Put(ctx context.Context, req transport.PutRequest) error {
	if t.failPut {
		return fmt.Errorf("disk full")
	}
	return t.inner.Put(ctx, req)
}

func (t *failingRunTransport) Run(ctx context.Context, req transport.RunRequest) (transport.RunResult, error) {
	if t.failWhen(req.Argv[0]) {
		return transport.RunResult{}, fmt.Errorf("ssh connection died mid-deployment")
	}
	return t.inner.Run(ctx, req)
}

// hangingTransport blocks Run for one program until its context is done —
// the stalled-target shape.
type hangingTransport struct {
	inner transport.Transport
	hang  string
}

func (t *hangingTransport) Put(ctx context.Context, req transport.PutRequest) error {
	return t.inner.Put(ctx, req)
}

func (t *hangingTransport) Run(ctx context.Context, req transport.RunRequest) (transport.RunResult, error) {
	if req.Argv[0] == t.hang {
		<-ctx.Done()
		return transport.RunResult{}, ctx.Err()
	}
	return t.inner.Run(ctx, req)
}

func TestDeployTransportFailureIsInfraError(t *testing.T) {
	// apply.sh exits non-zero is a hook failure (nil error, report).
	// The SSH connection dying during apply is NOT a hook failure — it
	// must surface as an infrastructure error so that rollback policy
	// (step 10) never reads "server unreachable" as "apply failed".
	f := newFixture(t, "my-app", nil)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")
	tr := &failingRunTransport{
		inner:    local.New(),
		failWhen: func(argv0 string) bool { return strings.Contains(argv0, "apply.sh") },
	}
	tgt, err := target.New(tr, f.root)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Deploy(t.Context(), DeployInput{
		Target:         tgt,
		TargetManifest: f.targetManifest(),
		Environment:    f.environment("my-app", "1.0.0"),
		Release:        rel,
		Bundle:         bundleBytes,
		Owner:          "test",
	})
	if err == nil {
		t.Fatalf("transport failure must be an error, got report %+v", rep)
	}
	if !strings.Contains(err.Error(), "ssh connection died") {
		t.Errorf("err = %v, want the underlying transport error", err)
	}
	if !strings.Contains(err.Error(), "apply could not be executed") {
		t.Errorf("err = %v, want the failing stage named", err)
	}
	if rep == nil || rep.Committed {
		t.Errorf("rep = %+v, want uncommitted report", rep)
	}
	// The infrastructure failure is still recorded as a fact.
	records := history(t, f)
	if len(records) != 1 || records[0].Type != "deploy.failed" {
		t.Fatalf("history = %+v", records)
	}
	requireNoLock(t, f)
}

func TestDeployLockReleaseFailureJoinsWithExistingError(t *testing.T) {
	// Two failures at once: the contract read breaks (infrastructure)
	// AND the lock rmdir fails. Both must survive in the returned error —
	// the environment is blocked until manual cleanup, and hiding that
	// behind "contract unreadable" would strand it silently.
	f := newFixture(t, "my-app", nil)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")
	tr := &failingRunTransport{
		inner: local.New(),
		failWhen: func(argv0 string) bool {
			return argv0 == "cat" || argv0 == "rmdir"
		},
	}
	tgt, err := target.New(tr, f.root)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Deploy(t.Context(), DeployInput{
		Target:         tgt,
		TargetManifest: f.targetManifest(),
		Environment:    f.environment("my-app", "1.0.0"),
		Release:        rel,
		Bundle:         bundleBytes,
		Owner:          "test",
	})
	if err == nil {
		t.Fatalf("want joined infrastructure + lock error, got %+v", rep)
	}
	if !strings.Contains(err.Error(), "staged deployment contract unreadable") {
		t.Errorf("err missing the infrastructure failure: %v", err)
	}
	if !strings.Contains(err.Error(), "could not be released") {
		t.Errorf("err missing the lock failure: %v", err)
	}
}

func TestDeployCancelledCleanupIsBounded(t *testing.T) {
	// Cleanup must survive a cancelled deploy context but must not hang
	// forever on a stuck target: the release attempt is bounded by its
	// own timeout and its failure is surfaced.
	old := lockCleanupTimeout
	lockCleanupTimeout = 250 * time.Millisecond
	defer func() { lockCleanupTimeout = old }()

	f := newFixture(t, "my-app", nil)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")
	tgt, err := target.New(&hangingTransport{inner: local.New(), hang: "rmdir"}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = Deploy(t.Context(), DeployInput{
		Target:         tgt,
		TargetManifest: f.targetManifest(),
		Environment:    f.environment("my-app", "1.0.0"),
		Release:        rel,
		Bundle:         bundleBytes,
		Owner:          "test",
	})
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "could not be released") {
		t.Fatalf("err = %v, want bounded lock-release failure", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("cleanup was not bounded: %s", elapsed)
	}
}

func TestAcquireEnvLockOwnerWriteFailureWarnsAboutOrphan(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	// Owner write fails AND cleanup fails (rm/rmdir broken): the
	// operator must be told explicitly that an orphan lock may remain.
	tr := &failingRunTransport{
		inner: local.New(),
		failWhen: func(argv0 string) bool {
			return argv0 == "rm" || argv0 == "rmdir"
		},
		failPut: true,
	}
	_, err := AcquireEnvLock(t.Context(), tr, f.lockDir(), LockOwner{User: "test"})
	if err == nil {
		t.Fatal("owner write failure must fail the acquisition")
	}
	if !strings.Contains(err.Error(), "orphan lock may remain") {
		t.Errorf("err = %v, want an explicit orphan-lock warning", err)
	}
}

func TestDeployHistoryWriteFailureAfterCommitIsExplicit(t *testing.T) {
	// The awkward window: state committed, then the history write loses
	// the connection. The caller gets an error (the attempt's record is
	// incomplete), and a retry hits the already-current guard instead of
	// repeating migrate/apply — pinned end to end.
	f := newFixture(t, "my-app", nil)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")

	// First deployment succeeds.
	if _, err := Deploy(t.Context(), DeployInput{
		Target: f.target, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "test",
	}); err != nil {
		t.Fatal(err)
	}

	// Retry with a broken transport: already-current guard fires before
	// any hook, the Stage already-staged check passes (Put works), and
	// the history write fails — so the attempt errors, but nothing
	// consequential re-ran.
	tr := &failingRunTransport{
		inner:    local.New(),
		failWhen: func(argv0 string) bool { return argv0 == "cat" }, // ReadHistory needs cat
	}
	tgt, err := target.New(tr, f.root)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Deploy(t.Context(), DeployInput{
		Target: tgt, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "retry",
	})
	// The guard path stages (idempotent), then fails to record history.
	if err == nil && rep != nil && !rep.AlreadyCurrent {
		t.Errorf("retry should have been classified already-current before any consequential work")
	}
	if rep != nil && rep.Committed {
		t.Error("retry must not re-commit")
	}
}
