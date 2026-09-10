package main

// Machine-surface tests for the lockRetained v1 extension: the JSON
// shape must distinguish deliberate retention (unknown hook fate) from
// ordinary infrastructure trouble, on both sides of the durable
// boundary, for deploy and rollback.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/magtheo/deploy-toolkit/internal/lifecycle"
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
