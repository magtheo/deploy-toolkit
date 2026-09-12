package main

// Machine-surface tests for the lockRetained v1 extension: the JSON
// shape must distinguish deliberate retention (unknown hook fate) from
// ordinary infrastructure trouble, on both sides of the durable
// boundary, for deploy and rollback.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/magtheo/deploy-toolkit/internal/lifecycle"
	"github.com/magtheo/deploy-toolkit/internal/target"
)

func TestJSONLockRetainedClassification(t *testing.T) {
	canceled := errors.New("context canceled")
	t.Run("deploy pre-boundary unknown", func(t *testing.T) {
		rep := &lifecycle.Report{Project: "my-app", Environment: "production", LockRetained: true}
		doc, code := deployResult(rep, canceled)
		if code != exitInfra {
			t.Fatalf("exit = %d, want %d", code, exitInfra)
		}
		if doc.Outcome != "infrastructure-failure" || doc.SafeToRetry != false || doc.RecoveryRequired != false {
			t.Errorf("shape = %v/%v/%v, want infrastructure-failure/false/false", doc.Outcome, doc.SafeToRetry, doc.RecoveryRequired)
		}
		if !dataLockRetained(t, doc) {
			t.Errorf("lockRetained missing, want true")
		}
	})
	t.Run("deploy post-boundary unknown", func(t *testing.T) {
		rep := &lifecycle.Report{Project: "my-app", Environment: "production", ConsequentialStarted: true, RecoveryRequired: true, LockRetained: true, AttemptID: "0123456789abcdef"}
		doc, code := deployResult(rep, canceled)
		if code != exitInfra {
			t.Fatalf("exit = %d, want %d", code, exitInfra)
		}
		if doc.Outcome != "uncertain" || doc.SafeToRetry != false || doc.RecoveryRequired != true {
			t.Errorf("shape = %v/%v/%v, want uncertain/false/true", doc.Outcome, doc.SafeToRetry, doc.RecoveryRequired)
		}
		if !dataLockRetained(t, doc) {
			t.Errorf("lockRetained missing, want true")
		}
		if !containsAny(doc.Message, "lock was deliberately retained", "verify the target") {
			t.Errorf("message = %q, want retention guidance", doc.Message)
		}
	})
	t.Run("rollback pre-boundary unknown", func(t *testing.T) {
		rep := &lifecycle.RollbackReport{Project: "my-app", Environment: "production", LockRetained: true}
		doc, code := rollbackResult(rep, canceled)
		if code != exitInfra {
			t.Fatalf("exit = %d, want %d", code, exitInfra)
		}
		if doc.Outcome != "infrastructure-failure" || doc.SafeToRetry != false {
			t.Errorf("shape = %v/%v, want infrastructure-failure/false", doc.Outcome, doc.SafeToRetry)
		}
		if !dataLockRetained(t, doc) {
			t.Errorf("lockRetained missing, want true")
		}
	})
	t.Run("rollback post-boundary unknown", func(t *testing.T) {
		rep := &lifecycle.RollbackReport{Project: "my-app", Environment: "production", RecoveryStarted: true, RecoveryRequired: true, LockRetained: true, RecoveryID: "0123456789abcdef"}
		doc, code := rollbackResult(rep, canceled)
		if code != exitInfra {
			t.Fatalf("exit = %d, want %d", code, exitInfra)
		}
		if doc.Outcome != "uncertain" || doc.RecoveryRequired != true {
			t.Errorf("shape = %v/%v, want uncertain/true", doc.Outcome, doc.RecoveryRequired)
		}
		if !dataLockRetained(t, doc) {
			t.Errorf("lockRetained missing, want true")
		}
	})
	t.Run("ordinary infrastructure failure does not set lockRetained", func(t *testing.T) {
		rep := &lifecycle.Report{Project: "my-app", Environment: "production"}
		doc, _ := deployResult(rep, errors.New("connection refused"))
		if dataLockRetained(t, doc) {
			t.Errorf("lockRetained must be absent without retention")
		}
		if doc.SafeToRetry != true {
			t.Errorf("pre-boundary not-started infrastructure stays safe-to-retry, got %v", doc.SafeToRetry)
		}
	})
}

// dataLockRetained reads data.lockRetained through the wire format —
// what automation actually sees.
func dataLockRetained(t *testing.T, doc *resultEnvelope) bool {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	data, _ := m["data"].(map[string]any)
	v, _ := data["lockRetained"].(bool)
	return v
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// A compound failure — evidence refusal whose lock release also failed —
// must render BOTH facts on every surface. The refusal classification
// stays, but the cleanup problem must not hide behind it.
func TestCompoundFailureSurfacesBothFacts(t *testing.T) {
	evidence := fmt.Errorf("read observed state: state.json: %w", target.ErrEvidenceInvalid)
	joined := errors.Join(evidence, fmt.Errorf("environment lock /locks/production could not be released (manual cleanup required): %w: rmdir: not empty", lifecycle.ErrLockReleaseFailed))

	t.Run("deploy json", func(t *testing.T) {
		doc, code := deployResult(&lifecycle.Report{Project: "my-app", Environment: "production"}, joined)
		if code != exitFailed || doc.Outcome != "refused" {
			t.Fatalf("shape = %v/%d, want refused/1", doc.Outcome, code)
		}
		// The machine fact is the STRUCTURED field — never the message.
		if doc.LockReleaseFailed != true {
			t.Errorf("lockReleaseFailed = %v, want true (cli-v1: never parse the message)", doc.LockReleaseFailed)
		}
	})
	t.Run("rollback json", func(t *testing.T) {
		doc, code := rollbackResult(&lifecycle.RollbackReport{Project: "my-app", Environment: "production"}, joined)
		if code != exitFailed || doc.Outcome != "refused" {
			t.Fatalf("shape = %v/%d, want refused/1", doc.Outcome, code)
		}
		if doc.LockReleaseFailed != true {
			t.Errorf("lockReleaseFailed = %v, want true (cli-v1: never parse the message)", doc.LockReleaseFailed)
		}
	})
	t.Run("human warning", func(t *testing.T) {
		var buf bytes.Buffer
		warnLockReleaseFailed(&buf, joined)
		if !strings.Contains(buf.String(), "COULD NOT BE RELEASED") || !strings.Contains(buf.String(), "manual cleanup") {
			t.Errorf("human warning missing: %q", buf.String())
		}
		buf.Reset()
		warnLockReleaseFailed(&buf, evidence)
		if buf.Len() != 0 {
			t.Errorf("warning printed without the sentinel: %q", buf.String())
		}
	})
}

// The lockReleaseFailed envelope fact on recovery resolve, with
// fact-driven rendering: a cleanup-window release failure after
// successful marker removals must say "completed", not claim markers
// remain; a partial clear names exactly what survives.
func TestResolveLockReleaseFailedRendering(t *testing.T) {
	t.Run("full resolve then release failure", func(t *testing.T) {
		rep := &lifecycle.ResolveReport{
			Project: "my-app", Environment: "production",
			ResolvedAttemptID: "0123456789abcdef", HistorySeq: 3,
		}
		joined := errors.Join(errors.New("resolution recorded"), fmt.Errorf("lock: %w", lifecycle.ErrLockReleaseFailed))
		doc, code := resolveResult(rep, joined, false)
		if code != exitInfra || doc.Outcome != "infrastructure-failure" {
			t.Fatalf("shape = %v/%d", doc.Outcome, code)
		}
		if doc.LockReleaseFailed != true {
			t.Errorf("lockReleaseFailed = %v, want true", doc.LockReleaseFailed)
		}
		if doc.RecoveryRequired != false {
			t.Errorf("recoveryRequired = %v, want false: markers were removed", doc.RecoveryRequired)
		}
		if !strings.Contains(doc.Message, "completed") {
			t.Errorf("message = %q, want completion (not markers-remain)", doc.Message)
		}
	})
	t.Run("partial resolve then release failure", func(t *testing.T) {
		rep := &lifecycle.ResolveReport{
			Project: "my-app", Environment: "production",
			ResolvedRecoveryID:  "0123456789abcdef",
			AttemptPresentAfter: true, AttemptMarkerID: "fedcba9876543210",
			RecoveryRequired: true,
		}
		joined := errors.Join(errors.New("clear attempt marker: exit 1"), fmt.Errorf("lock: %w", lifecycle.ErrLockReleaseFailed))
		doc, _ := resolveResult(rep, joined, false)
		if doc.LockReleaseFailed != true || doc.RecoveryRequired != true {
			t.Errorf("shape = %v/%v, want true/true", doc.LockReleaseFailed, doc.RecoveryRequired)
		}
		if !strings.Contains(doc.Message, "block remains") {
			t.Errorf("message = %q, want the block-remains fact", doc.Message)
		}
		data, _ := doc.Data.(*resolveResultData)
		if data == nil || data.RemainingAttemptID != "fedcba9876543210" {
			t.Errorf("remaining attempt id = %+v, want the survivor", data)
		}
	})
}

// A staging-lock release failure is a structured envelope fact: it must
// appear whatever the outcome classification, and it must never bleed
// into (or hide behind) the environment-lock fact.
func TestStagingLockReleaseFailureSurfaces(t *testing.T) {
	primary := errors.New("staging my-app/1.0.0/deploy/run.sh: forced put failure")
	joined := errors.Join(primary, fmt.Errorf("staging lock /srv/my-app/.staging/1.0.0 could not be released (manual cleanup required): %w: rmdir: not empty", target.ErrStageLockReleaseFailed))
	successful := errors.Join(fmt.Errorf("staging lock /srv/my-app/.staging/1.0.0 could not be released (manual cleanup required): %w: rmdir: not empty", target.ErrStageLockReleaseFailed))

	t.Run("deploy json staging failure plus release failure", func(t *testing.T) {
		doc, code := deployResult(nil, joined)
		if code != exitInfra || doc.Outcome != outcomeInfraFailed {
			t.Fatalf("shape = %v/%d, want infrastructure-failure/3", doc.Outcome, code)
		}
		if doc.StagingLockReleaseFailed != true {
			t.Errorf("stagingLockReleaseFailed = %v, want true (cli-v1: never parse the message)", doc.StagingLockReleaseFailed)
		}
		if doc.LockReleaseFailed {
			t.Errorf("lockReleaseFailed = %v, want false — the environment lock is a distinct fact", doc.LockReleaseFailed)
		}
		if doc.SafeToRetry {
			t.Errorf("safeToRetry = true, want false while the staging lock blocks this version")
		}
	})
	t.Run("rollback json staging failure plus release failure", func(t *testing.T) {
		doc, code := rollbackResult(nil, joined)
		if code != exitInfra || doc.Outcome != outcomeInfraFailed {
			t.Fatalf("shape = %v/%d, want infrastructure-failure/3", doc.Outcome, code)
		}
		if doc.StagingLockReleaseFailed != true {
			t.Errorf("stagingLockReleaseFailed = %v, want true", doc.StagingLockReleaseFailed)
		}
	})
	t.Run("successful stage still reports the release failure", func(t *testing.T) {
		// The stage itself succeeded; only the deferred lock release
		// failed. The classification is unchanged; the cleanup fact
		// must still be machine-visible.
		doc, code := deployResult(nil, successful)
		if code != exitInfra || doc.Outcome != outcomeInfraFailed {
			t.Fatalf("shape = %v/%d, want infrastructure-failure/3", doc.Outcome, code)
		}
		if doc.StagingLockReleaseFailed != true {
			t.Errorf("stagingLockReleaseFailed = %v, want true", doc.StagingLockReleaseFailed)
		}
		if doc.LockReleaseFailed {
			t.Errorf("lockReleaseFailed = %v, want false", doc.LockReleaseFailed)
		}
	})
	t.Run("absent on clean failure", func(t *testing.T) {
		doc, _ := deployResult(nil, errors.New("git fetch failed"))
		if doc.StagingLockReleaseFailed || doc.LockReleaseFailed {
			t.Errorf("facts = %v/%v, want absent", doc.StagingLockReleaseFailed, doc.LockReleaseFailed)
		}
	})
}

// The held-staging-lock refusal must classify identically on both
// surfaces: refused, exit 1 — not infrastructure failure, exit 3.
func TestStageLockHeldRefusalOnBothSurfaces(t *testing.T) {
	held := fmt.Errorf("stage release: %w: /srv/my-app/.staging/1.0.0 is being staged by another operation", target.ErrStageLockHeld)

	t.Run("deploy human", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := reportDeploy(nil, held, &stdout, &stderr)
		if code != exitFailed {
			t.Errorf("exit = %d, want %d (refused)", code, exitFailed)
		}
		if !strings.Contains(stderr.String(), "refused") || !strings.Contains(stderr.String(), "staging") {
			t.Errorf("stderr = %q, want a staging-lock refusal", stderr.String())
		}
	})
	t.Run("rollback human", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := reportRollback(nil, held, "production", &stdout, &stderr)
		if code != exitFailed {
			t.Errorf("exit = %d, want %d (refused)", code, exitFailed)
		}
		if !strings.Contains(stderr.String(), "refused") || !strings.Contains(stderr.String(), "staging") {
			t.Errorf("stderr = %q, want a staging-lock refusal", stderr.String())
		}
	})
	t.Run("deploy json", func(t *testing.T) {
		doc, code := deployResult(nil, held)
		if code != exitFailed || doc.Outcome != outcomeRefused {
			t.Errorf("shape = %v/%d, want refused/%d", doc.Outcome, code, exitFailed)
		}
	})
}

// End to end: a staging lock pre-created on the target (as a crashed
// stager would leave it) refuses a human deploy with exit 1.
func TestStageLockHeldEndToEndRefusal(t *testing.T) {
	f := newCLIFixture(t)
	lockDir := filepath.Join(f.deployDir, "my-app", ".staging", "1.0.0")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI("deploy", "production", "--repo-dir", f.repoDir, "--owner", "test")
	if code != exitFailed {
		t.Fatalf("exit = %d, want %d (refused)", code, exitFailed)
	}
	if !strings.Contains(errOut, "refused") || !strings.Contains(errOut, "staging") {
		t.Errorf("stderr = %q", errOut)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
}
