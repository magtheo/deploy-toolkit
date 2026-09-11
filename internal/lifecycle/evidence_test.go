package lifecycle

// Cancellation-safe evidence finalization (M1-6): history records must
// survive a cancelled caller context, while every permission-carrying
// action — hook execution, state commits, marker writes/clears, lock
// release — stays on the caller's context. A cancelled deadline costs
// the operator the record of what happened; it must never cost them
// the safety state instead.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/magtheo/deploy-toolkit/internal/target"
	"github.com/magtheo/deploy-toolkit/internal/transport"
	"github.com/magtheo/deploy-toolkit/internal/transport/local"
)

// cancelOnHookTransport cancels the operation's context when a matching
// hook is invoked and reports the unknown fate — the deadline-expiry
// shape arriving mid-deployment.
type cancelOnHookTransport struct {
	inner  transport.Transport
	when   func(argv []string) bool
	cancel context.CancelFunc
}

func (t *cancelOnHookTransport) Put(ctx context.Context, req transport.PutRequest) error {
	return t.inner.Put(ctx, req)
}

func (t *cancelOnHookTransport) ProbePath(ctx context.Context, path string) (transport.PathState, error) {
	return t.inner.ProbePath(ctx, path)
}

func (t *cancelOnHookTransport) Run(ctx context.Context, req transport.RunRequest) (transport.RunResult, error) {
	if t.when(req.Argv) {
		t.cancel()
		return transport.RunResult{Fate: transport.RunUnknown}, context.Canceled
	}
	return t.inner.Run(ctx, req)
}

func TestDeployEvidenceSurvivesCallerCancellation(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tgt, err := target.New(&cancelOnHookTransport{
		inner:  local.New(),
		when:   func(argv []string) bool { return strings.Contains(argv[0], "apply.sh") },
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
		t.Fatalf("rep = %+v, want an error", rep)
	}
	// The caller context is dead by the time finalization runs; the
	// record must exist anyway.
	records := history(t, f)
	if len(records) == 0 || records[len(records)-1].Type != "deploy.failed" {
		t.Fatalf("history = %+v, want a terminal deploy.failed record despite cancellation", records)
	}
	stages, _ := records[len(records)-1].Data["stages"].(map[string]any)
	apply, _ := stages["apply"].(map[string]any)
	if apply == nil || apply["infrastructureError"] != true {
		t.Fatalf("apply in history = %#v, want infrastructureError without a manufactured exit", stages["apply"])
	}
	if _, hasExit := apply["exit"]; hasExit {
		t.Errorf("apply in history manufactures an exit code: %#v", apply)
	}
	// Safety state is untouched by the finalization context.
	if !rep.LockRetained || !rep.RecoveryRequired {
		t.Errorf("rep = %+v, want retained lock and recovery-required", rep)
	}
	if !attemptPresent(t, f) {
		t.Error("the attempt marker must survive")
	}
	assertLockRetained(t, f)
}

func TestRollbackEvidenceSurvivesCallerCancellation(t *testing.T) {
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
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tgt, err := target.New(&cancelOnHookTransport{
		inner:  local.New(),
		when:   func(argv []string) bool { return strings.Contains(argv[0], "rollback.sh") },
		cancel: cancel,
	}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
	in.Target = tgt
	rep, err := Rollback(ctx, in)
	if err == nil {
		t.Fatalf("rep = %+v, want an error", rep)
	}
	records := history(t, f)
	if records[len(records)-1].Type != "rollback.failed" {
		t.Fatalf("history tail = %+v, want rollback.failed despite cancellation", records[len(records)-1])
	}
	if !rep.LockRetained || !rep.RecoveryRequired {
		t.Errorf("rep = %+v, want retained lock and recovery-required", rep)
	}
	if !recoveryPresent(t, f) {
		t.Error("the recovery marker must survive")
	}
	assertLockRetained(t, f)
}

// cancelOnHistoryPutTransport cancels the PARENT context when a Put
// for the history log arrives — simulating cancellation racing
// finalization. The write itself must still succeed on the
// finalization context.
type cancelOnHistoryPutTransport struct {
	inner  transport.Transport
	cancel context.CancelFunc
}

func (t *cancelOnHistoryPutTransport) Put(ctx context.Context, req transport.PutRequest) error {
	if strings.HasSuffix(req.Path, "/history/production.jsonl") {
		t.cancel()
	}
	return t.inner.Put(ctx, req)
}

func (t *cancelOnHistoryPutTransport) ProbePath(ctx context.Context, path string) (transport.PathState, error) {
	return t.inner.ProbePath(ctx, path)
}

func (t *cancelOnHistoryPutTransport) Run(ctx context.Context, req transport.RunRequest) (transport.RunResult, error) {
	return t.inner.Run(ctx, req)
}

// The resolve authorization record is evidence too — and the boundary
// must hold in the strict direction: the record survives the dead
// caller context, but the marker removals that FOLLOW it do not run on
// the finalization context, so an unresolved removal still fails closed.
func TestResolveAuthorizationEvidenceSurvivesCancellation(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")

	// Produce an unresolved attempt: unknown-fate apply (no caller
	// cancellation needed here), then the operator removes the retained
	// lock and authorizes resolution of that attempt.
	tgt, err := target.New(&unknownFateTransport{
		inner: local.New(),
		unknownIn: func(argv []string) bool {
			return strings.Contains(argv[0], "apply.sh")
		},
	}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := Deploy(t.Context(), DeployInput{
		Target: tgt, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "test",
	})
	if err == nil {
		t.Fatal("setup: unknown-fate deploy must fail")
	}
	if err := os.RemoveAll(f.lockDir()); err != nil {
		t.Fatal(err)
	}

	// A transport whose Put cancels the PARENT context when the
	// resolution record arrives: the record write itself must succeed
	// (it runs on the finalization context), while the subsequent
	// marker removal runs on the now-dead caller context and fails.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tgt2, err := target.New(&cancelOnHistoryPutTransport{
		inner:  local.New(),
		cancel: cancel,
	}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	in := resolveInput(t, f, "", failed.AttemptID)
	in.Target = tgt2
	rep, err := Resolve(ctx, in)
	if err == nil {
		t.Fatalf("rep = %+v, want the marker removal to fail on the dead caller context", rep)
	}
	records := history(t, f)
	last := records[len(records)-1]
	if last.Type != "resolution.authorized" {
		t.Fatalf("history tail = %+v, want resolution.authorized recorded despite cancellation", last)
	}
	if last.Data["confirmedAttemptId"] != failed.AttemptID {
		t.Errorf("confirmedAttemptId = %v, want %s", last.Data["confirmedAttemptId"], failed.AttemptID)
	}
	// The block is still up: evidence says authorized, markers remain.
	if !attemptPresent(t, f) {
		t.Error("the attempt marker must survive a failed removal")
	}
}
