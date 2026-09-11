package lifecycle

// M1-10: real-concurrency qualification. The environment lock is an
// atomic mkdir on a real filesystem; these tests drive GENUINE races
// with real goroutines and real transports — no simulated locking —
// and hold the winning operation at meaningful lifecycle points so the
// loser attempts entry while the winner is genuinely in-flight.
//
// Invariants under proof:
//
//	A  no surviving lock unless deliberately retained (unknown fate)
//	   or release was attempted and failed (joined sentinel);
//	B  uncertain consequential work always leaves its marker;
//	C  a loser in a lock race performs ZERO hooks and changes ZERO
//	   durable state; retries never repeat unresolved work.
//
// Determinism comes from handshake, not sleeps: blocking hooks park on
// a FIFO, and the test observes progress through the hook ledger
// (marker file), never through timing.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/target"
	"github.com/magtheo/deploy-toolkit/internal/transport"
	"github.com/magtheo/deploy-toolkit/internal/transport/local"
)

// awaitLedger polls the ledger until it contains want — an observation
// handshake: the winner is provably in-flight (hook running, lock held)
// before the test launches the loser.
func awaitLedger(t *testing.T, ledger, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(ledger)
		if err == nil && strings.Contains(string(raw), want) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("hook ledger %s never reached %q", ledger, want)
}

// releaseGate unblocks the parked hook.
func releaseGate(t *testing.T, gatePath string) {
	t.Helper()
	g, err := os.OpenFile(gatePath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if _, err := g.WriteString("go\n"); err != nil {
		t.Fatal(err)
	}
}

type deployOutcome struct {
	rep *Report
	err error
}

// runWinnerAsync launches the winner deploy on its own goroutine.
func runWinnerAsync(ctx context.Context, in DeployInput, done chan<- deployOutcome) {
	go func() {
		rep, err := Deploy(ctx, in)
		done <- deployOutcome{rep, err}
	}()
}

// racingFixture overrides the given stage so it parks on an
// ENVIRONMENT-SPECIFIC FIFO: staged releases are environment-independent
// (the same staged apply.sh serves every environment), so the gate file
// must be selected by $DEPLOY_ENVIRONMENT or two environments would
// share one gate. Non-gated stages still record themselves in the hook
// ledger (the fixture marker) so loser-immutability is really asserted.
func racingFixture(t *testing.T, stage string) (*fixture, string) {
	t.Helper()
	gateDir := t.TempDir()
	f := newFixture(t, "my-app", func(marker, envFile, script string) string {
		body := fmt.Sprintf("#!/bin/sh\necho %s >> %s\n", script, marker)
		if script == stage {
			body += fmt.Sprintf("echo started >> %s/$DEPLOY_ENVIRONMENT.ledger\ncat %s/$DEPLOY_ENVIRONMENT >/dev/null\necho %s-resumed >> %s\n", gateDir, gateDir, script, marker)
		}
		return body
	})
	return f, gateDir
}

func gateFor(gateDir, env string) string {
	return filepath.Join(gateDir, env)
}

func ledgerFor(gateDir, env string) string {
	return filepath.Join(gateDir, env) + ".ledger"
}

// losing actor facts, asserted for every lock race.
func assertLoserDidNothing(t *testing.T, f *fixture, err error, ledgerBefore []string) {
	t.Helper()
	if !errors.Is(err, ErrEnvLockHeld) {
		t.Fatalf("loser err = %v, want ErrEnvLockHeld", err)
	}
	ledger := order(t, f.marker)
	if strings.Join(ledger, ",") != strings.Join(ledgerBefore, ",") {
		t.Errorf("loser changed the hook ledger: %v → %v", ledgerBefore, ledger)
	}
}

// A: any surviving lock must be explained. In the race tests the winner
// completes normally, so NO lock may survive at all.
func requireNoUnexplainedLock(t *testing.T, f *fixture) {
	t.Helper()
	requireNoLock(t, f)
}

// 1. deploy vs deploy, same environment: the loser attempts entry while
// the winner is parked INSIDE apply — after the durable boundary.
func TestRaceDeployVsDeployHeldMidApply(t *testing.T) {
	f, gateDir := racingFixture(t, "apply")
	fifo := gateFor(gateDir, "production")
	mkfifo(t, fifo)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")

	done := make(chan deployOutcome, 1)
	runWinnerAsync(t.Context(), DeployInput{
		Target: f.target, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "winner",
	}, done)
	awaitLedger(t, ledgerFor(gateDir, "production"), "started")
	ledgerBefore := order(t, f.marker)

	// Loser: synchronous, must refuse fast while the winner holds the
	// lock from before preflight through commit.
	loserRep, loserErr := Deploy(t.Context(), DeployInput{
		Target: f.target, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "loser",
	})
	assertLoserDidNothing(t, f, loserErr, ledgerBefore)
	if loserRep != nil && loserRep.ConsequentialStarted {
		t.Errorf("loser claims consequential work: %+v", loserRep)
	}
	if _, err := os.Stat(f.lockDir()); err != nil {
		t.Fatal("the lock must still exist while the winner is in-flight")
	}

	releaseGate(t, fifo)
	win := <-done
	if win.err != nil || !win.rep.Committed {
		t.Fatalf("winner = %+v err = %v, want committed", win.rep, win.err)
	}
	requireNoUnexplainedLock(t, f)
	// Exactly the winner's hooks ran — each stage ledger line appears
	// exactly once (loser contributed nothing, winner contributed once).
	counts := map[string]int{}
	for _, line := range order(t, f.marker) {
		counts[line]++
	}
	for _, stage := range []string{"preflight", "apply", "verify"} {
		if counts[stage] != 1 {
			t.Errorf("hook %s appears %d times, want exactly the winner's one (ledger: %v)", stage, counts[stage], counts)
		}
	}
}

// 2. deploy vs rollback, same environment.
func TestRaceDeployVsRollbackHeldMidApply(t *testing.T) {
	f, gateDir := racingFixture(t, "apply")
	fifo := gateFor(gateDir, "production")
	mkfifo(t, fifo)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")

	done := make(chan deployOutcome, 1)
	runWinnerAsync(t.Context(), DeployInput{
		Target: f.target, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "winner",
	}, done)
	awaitLedger(t, ledgerFor(gateDir, "production"), "started")
	ledgerBefore := order(t, f.marker)

	revA, revB := f.revision, secondRevision(t, f)
	in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
	_, loserErr := Rollback(t.Context(), in)
	assertLoserDidNothing(t, f, loserErr, ledgerBefore)

	releaseGate(t, fifo)
	win := <-done
	if win.err != nil || !win.rep.Committed {
		t.Fatalf("winner = %+v err = %v", win.rep, win.err)
	}
	requireNoUnexplainedLock(t, f)
}

// 3. deploy vs recovery resolve: resolution can never remove evidence
// belonging to an in-flight operation — the attempt marker EXISTS
// while the winner parks in apply, and resolve still refuses on the
// lock.
func TestRaceDeployVsResolveHeldMidApply(t *testing.T) {
	f, gateDir := racingFixture(t, "apply")
	fifo := gateFor(gateDir, "production")
	mkfifo(t, fifo)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")

	done := make(chan deployOutcome, 1)
	runWinnerAsync(t.Context(), DeployInput{
		Target: f.target, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "winner",
	}, done)
	awaitLedger(t, ledgerFor(gateDir, "production"), "started")
	ledgerBefore := order(t, f.marker)

	// The in-flight attempt marker is on disk right now; a resolution
	// naming it must still be refused by the lock.
	if !attemptPresent(t, f) {
		t.Fatal("the in-flight attempt marker must exist mid-apply")
	}
	_, loserErr := Resolve(t.Context(), resolveInput(t, f, "", ""))
	assertLoserDidNothing(t, f, loserErr, ledgerBefore)
	if !attemptPresent(t, f) {
		t.Fatal("resolve removed the in-flight attempt marker")
	}

	releaseGate(t, fifo)
	win := <-done
	if win.err != nil || !win.rep.Committed {
		t.Fatalf("winner = %+v err = %v", win.rep, win.err)
	}
	requireNoUnexplainedLock(t, f)
}

// 4. rollback vs recovery resolve.
func TestRaceRollbackVsResolveHeldInRollbackHook(t *testing.T) {
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

	// Winner: recovery rollback parked inside the FROM release's
	// rollback hook.
	gateDir := t.TempDir()
	fifo := gateFor(gateDir, "production")
	mkfifo(t, fifo)
	rollbackTgt, err := target.New(&hookGateTransport{
		inner:    local.New(),
		stage:    "rollback",
		gatePath: fifo,
	}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan rollbackOutcome, 1)
	go func() {
		in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
		in.Target = rollbackTgt
		rep, err := Rollback(t.Context(), in)
		done <- rollbackOutcome{rep, err}
	}()
	awaitLedger(t, ledgerFor(gateDir, "production"), "started")
	ledgerBefore := order(t, f.marker)

	_, loserErr := Resolve(t.Context(), resolveInput(t, f, "", ""))
	assertLoserDidNothing(t, f, loserErr, ledgerBefore)
	if !recoveryPresent(t, f) {
		t.Fatal("resolve removed the in-flight recovery marker")
	}

	releaseGate(t, fifo)
	win := <-done
	if win.err != nil || !win.rep.Committed {
		t.Fatalf("winner = %+v err = %v", win.rep, win.err)
	}
	requireNoUnexplainedLock(t, f)
}

type rollbackOutcome struct {
	rep *RollbackReport
	err error
}

// hookGateTransport parks matching HOOK executions on a FIFO (used when
// the parked stage must be selected by role across staged contracts).
type hookGateTransport struct {
	inner    transport.Transport
	stage    string
	gatePath string
	once     bool
}

func (t *hookGateTransport) Put(ctx context.Context, req transport.PutRequest) error {
	return t.inner.Put(ctx, req)
}

func (t *hookGateTransport) ProbePath(ctx context.Context, path string) (transport.PathState, error) {
	return t.inner.ProbePath(ctx, path)
}

func (t *hookGateTransport) Run(ctx context.Context, req transport.RunRequest) (transport.RunResult, error) {
	if !t.once && strings.Contains(req.Argv[0], t.stage+".sh") {
		t.once = true
		ledger := t.gatePath + ".ledger"
		_ = os.WriteFile(ledger, []byte("started\n"), 0o644)
		f, err := os.OpenFile(t.gatePath, os.O_RDONLY, 0)
		if err != nil {
			return transport.RunResult{Fate: transport.RunUnknown}, err
		}
		buf := make([]byte, 8)
		_, _ = f.Read(buf)
		f.Close()
		_ = os.WriteFile(ledger, []byte("done\n"), 0o644)
	}
	return t.inner.Run(ctx, req)
}

// 5. deploy vs deploy on DIFFERENT environments: independent progress.
// Two deploys park simultaneously in their apply hooks, then both are
// released and both commit.
func TestRaceDifferentEnvironmentsAreIndependent(t *testing.T) {
	f, gateDir := racingFixture(t, "apply")
	fifo := gateFor(gateDir, "production")
	mkfifo(t, fifo)
	relProd, bytesProd := f.preparedBytes(t, "my-app", "1.0.0")

	// A second environment on the SAME target root — sharing the same
	// staged release (releases are environment-independent), gated by
	// its own FIFO via $DEPLOY_ENVIRONMENT.
	staging := f.environmentNamed("staging", "my-app", "1.0.0")
	fifo2 := gateFor(gateDir, "staging")
	mkfifo(t, fifo2)
	done1 := make(chan deployOutcome, 1)
	done2 := make(chan deployOutcome, 1)
	runWinnerAsync(t.Context(), DeployInput{
		Target: f.target, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: relProd, Bundle: bytesProd, Owner: "prod",
	}, done1)
	awaitLedger(t, ledgerFor(gateDir, "production"), "started")

	go func() {
		rep, err := Deploy(t.Context(), DeployInput{
			Target: f.target, TargetManifest: f.targetManifest(),
			Environment: staging, Release: relProd, Bundle: bytesProd, Owner: "staging",
		})
		done2 <- deployOutcome{rep, err}
	}()
	awaitLedger(t, ledgerFor(gateDir, "staging"), "started")

	// Both environments are in-flight at once; each has its own lock.
	prodLock := f.lockDir()
	stagingLock := f.root + "/my-app/.locks/staging"
	for _, lk := range []string{prodLock, stagingLock} {
		if _, err := os.Stat(lk); err != nil {
			t.Fatalf("lock %s must exist while in-flight: %v", lk, err)
		}
	}

	releaseGate(t, fifo)
	releaseGate(t, fifo2)
	w1, w2 := <-done1, <-done2
	if w1.err != nil || !w1.rep.Committed {
		t.Fatalf("production = %+v err = %v", w1.rep, w1.err)
	}
	if w2.err != nil || !w2.rep.Committed {
		t.Fatalf("staging = %+v err = %v", w2.rep, w2.err)
	}
	requireNoLock(t, f)
	if _, err := os.Stat(stagingLock); !os.IsNotExist(err) {
		t.Errorf("staging lock survived: %v", err)
	}
}

// environmentNamed mirrors fixture.environment but with an explicit
// environment name — the per-environment independence proof needs a
// second environment on the SAME target.
func (f *fixture) environmentNamed(name, project, version string) *manifest.Environment {
	env := f.environment(project, version)
	env.Metadata.Name = name
	return env
}

// mkfifo is not in os: create the named pipe via the syscall.
func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(path) })
}

// Lock churn: the PR #1 CI flake was a Release rmdir failing ENOTEMPTY
// at TempDir cleanup (passed on re-run; never reproduced locally).
// Release is strictly sequential (rm -f owner.json → rmdir, same
// goroutine, kernel-ordered), so an internal race would require the
// kernel to lie. This churn hammers acquire→release through full
// deploys, sequentially and in parallel across environments, to give
// any real condition a chance to surface. Verdict so far: watch-item.
func TestLockChurnThroughFullDeploys(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")
	in := DeployInput{
		Target: f.target, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "churn",
	}
	for i := 0; i < 40; i++ {
		if _, err := Deploy(t.Context(), in); err != nil {
			t.Fatalf("churn deploy %d: %v", i, err)
		}
		requireNoLock(t, f)
	}
}

func TestLockChurnParallelEnvironments(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")
	const envs = 6
	const rounds = 8
	var wg sync.WaitGroup
	errs := make(chan error, envs*rounds)
	for e := 0; e < envs; e++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			env := f.environmentNamed(fmt.Sprintf("env-%d", n), "my-app", "1.0.0")
			in := DeployInput{
				Target: f.target, TargetManifest: f.targetManifest(),
				Environment: env, Release: rel, Bundle: bundleBytes, Owner: "churn",
			}
			for r := 0; r < rounds; r++ {
				rep, err := Deploy(t.Context(), in)
				if errors.Is(err, target.ErrStageLockHeld) {
					// Legitimate cold-start contention: several
					// environments staging the same never-staged version
					// serialize; the loser refuses. The invariant under
					// proof is that the block is NOT permanent — a retry
					// after the winner finishes must proceed. Bound the
					// test-side retry; a persistent lock would fail it.
					recovered := false
					for attempt := 0; attempt < 500; attempt++ {
						time.Sleep(10 * time.Millisecond)
						if rep, err = Deploy(t.Context(), in); !errors.Is(err, target.ErrStageLockHeld) {
							recovered = true
							break
						}
					}
					if !recovered {
						errs <- fmt.Errorf("env-%d round %d: staging lock never freed: %w", n, r, err)
						return
					}
				} else if err != nil {
					errs <- fmt.Errorf("env-%d round %d: %v (rep: staged=%v committed=%v alreadyCurrent=%v)", n, r, err, rep.Staged, rep.Committed, rep.AlreadyCurrent)
					return
				}
				if rep == nil || (!rep.Committed && !rep.AlreadyCurrent) {
					errs <- fmt.Errorf("env-%d round %d: no successful terminal: %+v", n, r, rep)
					return
				}
			}
		}(e)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for e := 0; e < envs; e++ {
		lock := f.root + "/my-app/.locks/env-" + fmt.Sprint(e)
		if _, err := os.Stat(lock); !os.IsNotExist(err) {
			t.Errorf("lock %s survived the churn (stat err = %v) — invariant A", lock, err)
		}
	}
}

// The staging lock is refusal-class: a held staging lock (another
// environment staging the same version) refuses like a held environment
// lock, and the sentinel is reachable through the engine's wrapping.
func TestStageLockHeldIsRefusalSentinel(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")
	lockDir, err := f.target.Layout().StagingLockDir("my-app", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rep, err := Deploy(t.Context(), DeployInput{
		Target: f.target, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "test",
	})
	if !errors.Is(err, target.ErrStageLockHeld) {
		t.Fatalf("err = %v, want ErrStageLockHeld through the wrapping", err)
	}
	if rep.Staged != target.StageNotAttempted {
		t.Errorf("staged = %v, want not-attempted", rep.Staged)
	}
	requireNoLock(t, f) // env lock released; the STAGING lock remains, by design
	if _, statErr := os.Stat(lockDir); statErr != nil {
		t.Error("the injected staging lock must survive for the operator")
	}
}
