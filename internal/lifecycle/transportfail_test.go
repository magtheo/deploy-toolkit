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
	inner       transport.Transport
	failWhen    func(argv0 string) bool
	failRunWhen func(argv []string) bool // finer-grained: matches the full argv
	failPut     bool
	failPutWhen func(path string) bool
}

func (t *failingRunTransport) Put(ctx context.Context, req transport.PutRequest) error {
	if t.failPut || (t.failPutWhen != nil && t.failPutWhen(req.Path)) {
		return fmt.Errorf("disk full")
	}
	return t.inner.Put(ctx, req)
}

func (t *failingRunTransport) Run(ctx context.Context, req transport.RunRequest) (transport.RunResult, error) {
	if t.failWhen != nil && t.failWhen(req.Argv[0]) {
		return transport.RunResult{}, fmt.Errorf("ssh connection died mid-deployment")
	}
	if t.failRunWhen != nil && t.failRunWhen(req.Argv) {
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
	// (the rollback operation) never reads "server unreachable" as
	// "apply failed".
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
	// The infrastructure failure is still recorded as a fact — and the
	// evidence model is truthful: apply produced NO exit code.
	records := history(t, f)
	if len(records) != 1 || records[0].Type != "deploy.failed" {
		t.Fatalf("history = %+v", records)
	}
	stages, _ := records[0].Data["stages"].(map[string]any)
	applyEntry, _ := stages["apply"].(map[string]any)
	if applyEntry == nil || applyEntry["infrastructureError"] != true {
		t.Fatalf("apply in history = %#v, want infrastructureError without an exit code", stages["apply"])
	}
	if _, hasExit := applyEntry["exit"]; hasExit {
		t.Errorf("apply in history manufactures an exit code: %#v", applyEntry)
	}
	// Stages that did run report real exit codes.
	if pf, _ := stages["preflight"].(map[string]any); pf == nil || fmt.Sprint(pf["exit"]) != "0" {
		t.Errorf("preflight in history = %#v, want exit 0", stages["preflight"])
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

// The durable boundary must be reported by the ENGINE, never
// reconstructed by probing the target after the failure: the probe
// would travel over the same transport that just died (or the same
// context that was cancelled), and a read error is not absence.
func TestDeployInfraErrorCarriesDurableBoundary(t *testing.T) {
	// Transport loss during apply — AFTER the marker write. The
	// outcome is unknown; the report says so via ConsequentialStarted.
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
		Target: tgt, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "test",
	})
	if err == nil || rep == nil {
		t.Fatalf("rep, err = %+v, %v; want an infrastructure error with its report", rep, err)
	}
	if !rep.ConsequentialStarted {
		t.Error("ConsequentialStarted = false, but the attempt marker was persisted before apply")
	}
	if rep.AttemptID == "" {
		t.Error("AttemptID must be reported once the marker is written")
	}
	if rep.Committed {
		t.Error("an interrupted apply must not report Committed")
	}

	// Transport loss during preflight — BEFORE the marker write: the
	// boundary was NOT crossed; a rerun after infra repair is safe.
	f2 := newFixture(t, "my-app", nil)
	rel2, bundleBytes2 := f2.preparedBytes(t, "my-app", "1.0.0")
	tr2 := &failingRunTransport{
		inner:    local.New(),
		failWhen: func(argv0 string) bool { return strings.Contains(argv0, "preflight.sh") },
	}
	tgt2, err := target.New(tr2, f2.root)
	if err != nil {
		t.Fatal(err)
	}
	rep2, err := Deploy(t.Context(), DeployInput{
		Target: tgt2, TargetManifest: f2.targetManifest(),
		Environment: f2.environment("my-app", "1.0.0"), Release: rel2, Bundle: bundleBytes2, Owner: "test",
	})
	if err == nil || rep2 == nil {
		t.Fatalf("rep2, err = %+v, %v; want an infrastructure error with its report", rep2, err)
	}
	if rep2.ConsequentialStarted || rep2.AttemptID != "" {
		t.Errorf("pre-boundary failure must not claim the boundary: %+v", rep2)
	}
}

func TestRollbackInfraErrorCarriesDurableBoundary(t *testing.T) {
	// Standard emergency shape: 1.0.0 healthy, 2.0.0 failed verify and
	// left its attempt marker; the recovery restores 1.0.0.
	f := newFixture(t, "my-app", versionTaggedHooks)
	revA := f.revision
	if _, err := deploy(t, f, "my-app", "1.0.0", nil); err != nil {
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

	// Transport dies during the from-release rollback hook — AFTER the
	// recovery marker write. Uncertain, by the engine's own word.
	tr := &failingRunTransport{
		inner: local.New(),
		failRunWhen: func(argv []string) bool {
			return strings.Contains(argv[0], "rollback.sh")
		},
	}
	tgt, err := target.New(tr, f.root)
	if err != nil {
		t.Fatal(err)
	}
	in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
	in.Target = tgt
	rep, err := Rollback(t.Context(), in)
	if err == nil || rep == nil {
		t.Fatalf("rep, err = %+v, %v; want an infrastructure error with its report", rep, err)
	}
	if !rep.RecoveryStarted {
		t.Error("RecoveryStarted = false, but the recovery marker was persisted before the rollback hook")
	}
	if rep.RecoveryID == "" {
		t.Error("the recovery id must be reported once the marker is written")
	}
	if rep.Committed {
		t.Error("an interrupted recovery must not report Committed")
	}

	// Transport dies during the To-release preflight — BEFORE the
	// marker write: pre-boundary, retryable after infra repair.
	f2 := newFixture(t, "my-app", versionTaggedHooks)
	revA2 := f2.revision
	if _, err := deploy(t, f2, "my-app", "1.0.0", nil); err != nil {
		t.Fatal(err)
	}
	revB2 := secondRevision(t, f2)
	relB2, bytesB2 := f2.preparedBytesAt(t, revB2, "my-app", "2.0.0")
	if _, err := Deploy(t.Context(), DeployInput{
		Target: f2.target, TargetManifest: f2.targetManifest(),
		Environment: f2.environment("my-app", "2.0.0"), Release: relB2, Bundle: bytesB2, Owner: "test",
	}); err != nil {
		t.Fatal(err)
	}
	tr2 := &failingRunTransport{
		inner: local.New(),
		failRunWhen: func(argv []string) bool {
			return strings.Contains(argv[0], "preflight.sh")
		},
	}
	tgt2, err := target.New(tr2, f2.root)
	if err != nil {
		t.Fatal(err)
	}
	in2 := rollbackInput(t, f2, revB2, "2.0.0", revA2, "1.0.0", RollbackRecovery)
	in2.Target = tgt2
	rep2, err := Rollback(t.Context(), in2)
	if err == nil || rep2 == nil {
		t.Fatalf("rep2, err = %+v, %v; want an infrastructure error with its report", rep2, err)
	}
	if rep2.RecoveryStarted || rep2.RecoveryID != "" {
		t.Errorf("pre-boundary failure must not claim the boundary: %+v", rep2)
	}
}
