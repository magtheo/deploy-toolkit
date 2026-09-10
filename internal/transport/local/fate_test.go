package local

// Execution-fate tests for the local transport: cancellation must
// return promptly (bounded by WaitDelay) even when a descendant holds
// the inherited pipes — the verified M1-9 wedge — and the fate
// contract must separate not-started, exited and unknown.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/transport"
)

func TestRunCancelBoundedWithPipeHoldingGrandchild(t *testing.T) {
	old := runWaitDelay
	runWaitDelay = 300 * time.Millisecond
	t.Cleanup(func() { runWaitDelay = old })

	tr := New()
	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	res, err := tr.Run(ctx, transport.RunRequest{
		Argv: []string{"/bin/sh", "-c", "sleep 30 & echo started; sleep 30"},
		Dir:  t.TempDir(),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res.Fate != transport.RunUnknown {
		t.Errorf("fate = %v, want RunUnknown: a cancelled tree's fate is never known", res.Fate)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Run returned after %v — the pipe-holding grandchild must bound at WaitDelay", elapsed)
	}
}

// A NORMAL exit whose grandchild keeps the pipes open must also return,
// bounded — but as an unknown fate, because a process is still alive.
func TestRunOrdinaryExitWithPipeHolderIsUnknown(t *testing.T) {
	old := runWaitDelay
	runWaitDelay = 300 * time.Millisecond
	t.Cleanup(func() { runWaitDelay = old })

	tr := New()
	res, err := tr.Run(context.Background(), transport.RunRequest{
		Argv: []string{"/bin/sh", "-c", "sleep 30 & echo started; exit 0"},
		Dir:  t.TempDir(),
	})
	if err == nil {
		t.Fatal("err = nil, want the WaitDelay bounded pipe error")
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want a pipe/wait-delay error, not cancellation", err)
	}
	if res.Fate != transport.RunUnknown {
		t.Errorf("fate = %v, want RunUnknown: the pipe holder is still running", res.Fate)
	}
}

func TestRunFateFacts(t *testing.T) {
	tr := New()
	dir := t.TempDir()

	// Determined exit.
	res, err := tr.Run(context.Background(), transport.RunRequest{Argv: []string{"/bin/sh", "-c", "exit 3"}, Dir: dir})
	if err != nil || res.Fate != transport.RunExited || res.ExitCode != 3 {
		t.Errorf("res = %+v err = %v, want RunExited 3", res, err)
	}

	// Definite start failure.
	res, err = tr.Run(context.Background(), transport.RunRequest{Argv: []string{filepath.Join(dir, "no-such-binary")}, Dir: dir})
	if err == nil || res.Fate != transport.RunNotStarted {
		t.Errorf("res = %+v err = %v, want RunNotStarted", res, err)
	}
	var startErr *transport.StartError
	if !errors.As(err, &startErr) {
		t.Errorf("err = %v, want a StartError for a definite start failure", err)
	}

	// Pre-cancelled context: nothing attempted.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err = tr.Run(ctx, transport.RunRequest{Argv: []string{"/bin/echo", "x"}, Dir: dir})
	if !errors.Is(err, context.Canceled) || res.Fate != transport.RunNotStarted {
		t.Errorf("res = %+v err = %v, want RunNotStarted with the context error", res, err)
	}
}
