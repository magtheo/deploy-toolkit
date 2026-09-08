package target

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedTime(unix int64) string {
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

func TestHistoryAbsent(t *testing.T) {
	tgt := newLocalTarget(t)
	if _, err := tgt.ReadHistory(t.Context(), "my-app", "production"); !errors.Is(err, ErrHistoryAbsent) {
		t.Fatalf("err = %v, want ErrHistoryAbsent", err)
	}
}

func TestAppendAndVerifyChain(t *testing.T) {
	tgt := newLocalTarget(t)
	err := tgt.AppendHistory(t.Context(), "my-app", "production",
		Entry{Time: fixedTime(1700000000), Type: "stage.completed", Data: map[string]any{"release": "1.0.0"}},
		Entry{Time: fixedTime(1700000100), Type: "deploy.started", Data: map[string]any{"release": "1.0.0", "actor": "deploy-bot"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	records, err := tgt.ReadHistory(t.Context(), "my-app", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	if records[0].Seq != 1 || records[0].Prev != "" {
		t.Errorf("genesis record: seq=%d prev=%q", records[0].Seq, records[0].Prev)
	}
	if records[1].Seq != 2 || records[1].Prev != records[0].Hash {
		t.Errorf("chain link broken: seq=%d prev=%q want %q", records[1].Seq, records[1].Prev, records[0].Hash)
	}
	if records[1].Data["actor"] != "deploy-bot" {
		t.Errorf("data lost: %+v", records[1].Data)
	}
}

func TestAppendContinuesExistingChain(t *testing.T) {
	tgt := newLocalTarget(t)
	if err := tgt.AppendHistory(t.Context(), "my-app", "production",
		Entry{Time: fixedTime(1700000000), Type: "stage.completed", Data: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if err := tgt.AppendHistory(t.Context(), "my-app", "production",
		Entry{Time: fixedTime(1700000500), Type: "deploy.completed", Data: map[string]any{}},
		Entry{Time: fixedTime(1700000600), Type: "verify.completed", Data: map[string]any{}},
	); err != nil {
		t.Fatal(err)
	}
	records, err := tgt.ReadHistory(t.Context(), "my-app", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 || records[2].Seq != 3 || records[2].Prev != records[1].Hash {
		t.Fatalf("chain after append: %+v", records)
	}
}

func TestHistoryTamperDetected(t *testing.T) {
	tgt := newLocalTarget(t)
	if err := tgt.AppendHistory(t.Context(), "my-app", "production",
		Entry{Time: fixedTime(1700000000), Type: "stage.completed", Data: map[string]any{"release": "1.0.0"}},
		Entry{Time: fixedTime(1700000100), Type: "deploy.started", Data: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tgt.layout.Root(), "my-app/history/production.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), "stage.completed", "stage.FALSIFIED", 1)
	if tampered == string(raw) {
		t.Fatal("tamper setup failed")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tgt.ReadHistory(t.Context(), "my-app", "production"); err == nil || !strings.Contains(err.Error(), "altered") {
		t.Fatalf("tampered record must be detected: %v", err)
	}
}

func TestHistoryBrokenLinkDetected(t *testing.T) {
	tgt := newLocalTarget(t)
	if err := tgt.AppendHistory(t.Context(), "my-app", "production",
		Entry{Time: fixedTime(1700000000), Type: "stage.completed", Data: map[string]any{}},
		Entry{Time: fixedTime(1700000100), Type: "deploy.started", Data: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tgt.layout.Root(), "my-app/history/production.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Drop the first record: seq/prev/hash all re-verify as wrong.
	lines := strings.SplitN(string(raw), "\n", 2)
	if err := os.WriteFile(path, []byte(lines[1]), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tgt.ReadHistory(t.Context(), "my-app", "production"); err == nil {
		t.Fatal("truncated chain must be detected")
	}
}

func TestHistorySuffixDeletionIsNotDetected(t *testing.T) {
	// This pins the documented LIMIT of a self-contained hash chain:
	// dropping a valid suffix leaves a perfectly valid chain. Detection
	// requires an external anchor for the expected terminal (seq, hash);
	// until that exists, the guarantee is "integrity-checked", not
	// "tamper-evident". If this test starts failing, the guarantee has
	// become stronger than the documentation claims — update both.
	tgt := newLocalTarget(t)
	if err := tgt.AppendHistory(t.Context(), "my-app", "production",
		Entry{Time: fixedTime(1700000000), Type: "stage.completed", Data: map[string]any{}},
		Entry{Time: fixedTime(1700000100), Type: "deploy.started", Data: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tgt.layout.Root(), "my-app/history/production.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	suffixDropped := strings.SplitN(string(raw), "\n", 2)[0] + "\n"
	if err := os.WriteFile(path, []byte(suffixDropped), 0o644); err != nil {
		t.Fatal(err)
	}
	records, err := tgt.ReadHistory(t.Context(), "my-app", "production")
	if err != nil {
		t.Fatalf("suffix deletion must verify as a valid chain per the documented guarantee: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want the remaining 1", len(records))
	}
}

func TestHistoryHashIsDeterministic(t *testing.T) {
	// Same entries appended to two independent targets must produce
	// byte-identical logs, regardless of map insertion order.
	entryA := Entry{Time: fixedTime(1700000000), Type: "stage.completed", Data: map[string]any{"alpha": 1, "beta": "x", "gamma": true}}
	entryB := Entry{Time: fixedTime(1700000000), Type: "stage.completed", Data: map[string]any{"gamma": true, "beta": "x", "alpha": 1}}
	t1, t2 := newLocalTarget(t), newLocalTarget(t)
	if err := t1.AppendHistory(t.Context(), "my-app", "production", entryA); err != nil {
		t.Fatal(err)
	}
	if err := t2.AppendHistory(t.Context(), "my-app", "production", entryB); err != nil {
		t.Fatal(err)
	}
	raw1, err := os.ReadFile(filepath.Join(t1.layout.Root(), "my-app/history/production.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	raw2, err := os.ReadFile(filepath.Join(t2.layout.Root(), "my-app/history/production.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw1) != string(raw2) {
		t.Errorf("hashing is order-dependent:\n%s\n%s", raw1, raw2)
	}
}

func TestAppendValidation(t *testing.T) {
	tgt := newLocalTarget(t)
	if err := tgt.AppendHistory(t.Context(), "my-app", "production"); err == nil {
		t.Error("empty append must be rejected")
	}
	if err := tgt.AppendHistory(t.Context(), "my-app", "production", Entry{Type: ""}); err == nil {
		t.Error("typeless entry must be rejected")
	}
	if err := tgt.AppendHistory(t.Context(), "my-app", "production", Entry{Type: "x", Time: "not-a-time"}); err == nil {
		t.Error("invalid time must be rejected")
	}
}
