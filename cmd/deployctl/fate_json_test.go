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
