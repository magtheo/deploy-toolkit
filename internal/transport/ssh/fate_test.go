package ssh

// Fate-contract test against the real SSH test server: cancellation
// after the exec request is sent is RunUnknown — never a claimed
// termination (verified against OpenSSH: channel close does not stop
// the remote process).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/transport"
)

func TestRunCancelIsUnknownFate(t *testing.T) {
	signer := clientSigner(t)
	ts := startTestServer(t, signer.PublicKey())
	tr, err := dialTransport(t, ts, signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := tr.Run(ctx, transport.RunRequest{
		Argv: []string{"sleep", "30"},
		Dir:  "/tmp",
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if res.Fate != transport.RunUnknown {
		t.Errorf("fate = %v, want RunUnknown", res.Fate)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Run returned after %v, want promptly after cancellation", elapsed)
	}
}

func TestRunExitIsExitedFate(t *testing.T) {
	signer := clientSigner(t)
	ts := startTestServer(t, signer.PublicKey())
	tr, err := dialTransport(t, ts, signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	res, err := tr.Run(context.Background(), transport.RunRequest{
		Argv: []string{"sh", "-c", "exit 3"},
		Dir:  "/tmp",
	})
	if err != nil || res.Fate != transport.RunExited || res.ExitCode != 3 {
		t.Errorf("res = %+v err = %v, want RunExited 3", res, err)
	}
}
