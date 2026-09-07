package target

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/transport"
)

// ErrHistoryAbsent reports that no history log exists yet for a
// project/environment pair.
var ErrHistoryAbsent = errors.New("history: no records")

// Record is one immutable entry of the durable history log. Records form a
// hash chain: Prev carries the previous record's Hash ("" for the genesis
// record), and Hash covers the record's full content. Rewriting or removing
// an earlier record breaks the chain and is detected on read — append-only
// is enforced by verification, not by hope.
//
// Records are operator-visible evidence. They must never contain
// application secrets; they carry release identities, digests, decision
// outcomes and timestamps only.
type Record struct {
	Seq  int64          `json:"seq"`
	Time string         `json:"time"` // RFC3339, UTC
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
	Prev string         `json:"prev"`
	Hash string         `json:"hash"`
}

// Entry is a caller-supplied history record without chain fields; the
// Target assigns Seq, Prev and Hash on append.
type Entry struct {
	Time string // RFC3339, UTC; empty means append-time
	Type string
	Data map[string]any
}

// ReadHistory returns the verified history chain for project/env. A
// missing log yields ErrHistoryAbsent; a log that fails chain verification
// (tampered, truncated, reordered) is an error — never silently accepted.
func (t *Target) ReadHistory(ctx context.Context, project, env string) ([]Record, error) {
	records, _, err := t.readHistoryVerified(ctx, project, env)
	return records, err
}

func (t *Target) readHistoryVerified(ctx context.Context, project, env string) ([]Record, string, error) {
	path, err := t.layout.HistoryPath(project, env)
	if err != nil {
		return nil, "", err
	}
	present, err := t.exists(ctx, path)
	if err != nil {
		return nil, "", err
	}
	if !present {
		return nil, "", fmt.Errorf("%s/%s: %w", project, env, ErrHistoryAbsent)
	}
	raw, err := t.readFile(ctx, path)
	if err != nil {
		return nil, "", err
	}
	records, err := parseHistory(path, raw)
	if err != nil {
		return nil, "", err
	}
	return records, path, nil
}

func parseHistory(path string, raw []byte) ([]Record, error) {
	var records []Record
	for i, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, i+1, err)
		}
		records = append(records, rec)
	}
	for i := range records {
		rec := records[i]
		if rec.Seq != int64(i+1) {
			return nil, fmt.Errorf("%s: record %d has seq %d, want %d (history is append-only and gapless)", path, i+1, rec.Seq, i+1)
		}
		wantPrev := ""
		if i > 0 {
			wantPrev = records[i-1].Hash
		}
		if rec.Prev != wantPrev {
			return nil, fmt.Errorf("%s: record %d links to %q, want %q — the chain has been broken", path, rec.Seq, rec.Prev, wantPrev)
		}
		computed, err := recordHash(rec)
		if err != nil {
			return nil, fmt.Errorf("%s: record %d: %w", path, rec.Seq, err)
		}
		if rec.Hash != computed {
			return nil, fmt.Errorf("%s: record %d hash mismatch: recorded %s, computed %s — the record has been altered", path, rec.Seq, rec.Hash, computed)
		}
	}
	return records, nil
}

// AppendHistory appends entries to the durable history log, extending and
// re-verifying the hash chain. The log is rewritten in full through the
// transport's atomic file publication; entries already recorded stay
// bit-identical in meaning (their chain hashes are carried over untouched).
func (t *Target) AppendHistory(ctx context.Context, project, env string, entries ...Entry) error {
	if len(entries) == 0 {
		return fmt.Errorf("history: AppendHistory requires at least one entry")
	}
	for _, e := range entries {
		if e.Type == "" {
			return fmt.Errorf("history: entry type must not be empty")
		}
		if e.Time != "" {
			if _, err := time.Parse(time.RFC3339, e.Time); err != nil {
				return fmt.Errorf("history: entry time %q: %w", e.Time, err)
			}
		}
	}

	existing, _, err := t.readHistoryVerified(ctx, project, env)
	if err != nil && !errors.Is(err, ErrHistoryAbsent) {
		return err
	}

	records := existing
	now := time.Now().UTC().Format(time.RFC3339)
	for _, e := range entries {
		recTime := e.Time
		if recTime == "" {
			recTime = now
		}
		rec := Record{
			Seq:  int64(len(records) + 1),
			Time: recTime,
			Type: e.Type,
			Data: e.Data,
		}
		if len(records) > 0 {
			rec.Prev = records[len(records)-1].Hash
		}
		h, err := recordHash(rec)
		if err != nil {
			return fmt.Errorf("history: record %d: %w", rec.Seq, err)
		}
		rec.Hash = h
		records = append(records, rec)
	}

	path, err := t.layout.HistoryPath(project, env)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, rec := range records {
		if err := enc.Encode(rec); err != nil {
			return err
		}
	}
	if err := t.tr.Put(ctx, transport.PutRequest{Path: path, Content: buf.Bytes(), Mode: 0o644}); err != nil {
		return fmt.Errorf("writing history %s: %w", path, err)
	}
	return nil
}

// recordHash covers every field except Hash itself. json.Marshal sorts map
// keys, so hashing is deterministic regardless of Data insertion order.
func recordHash(rec Record) (string, error) {
	canonical := struct {
		Seq  int64          `json:"seq"`
		Time string         `json:"time"`
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
		Prev string         `json:"prev"`
	}{rec.Seq, rec.Time, rec.Type, rec.Data, rec.Prev}
	b, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("sha256:%064x", sum), nil
}
