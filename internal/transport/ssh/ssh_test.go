package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/magtheo/deploy-toolkit/internal/transport"
)

func clientSigner(t *testing.T) gossh.Signer {
	t.Helper()
	return generateSigner(t)
}

func dialTransport(t *testing.T, ts *testServer, signer gossh.Signer, pinned gossh.PublicKey) (*Transport, error) {
	t.Helper()
	if pinned == nil {
		pinned = ts.hostKey
	}
	return New(context.Background(), Config{
		Host:    "127.0.0.1",
		Port:    mustPort(t, ts),
		User:    "deploy",
		Signer:  signer,
		HostKey: pinned,
	})
}

func mustPort(t *testing.T, ts *testServer) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(ts.addr)
	if err != nil {
		t.Fatal(err)
	}
	var port int
	_, err = fmt.Sscanf(portStr, "%d", &port)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func newTransport(t *testing.T) (*testServer, *Transport) {
	t.Helper()
	signer := clientSigner(t)
	ts := startTestServer(t, signer.PublicKey())
	tr, err := New(context.Background(), Config{
		Host:    "127.0.0.1",
		Port:    mustPort(t, ts),
		User:    "deploy",
		Signer:  signer,
		HostKey: ts.hostKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tr.Close() })
	return ts, tr
}

func TestSSHConformanceHostileArgv(t *testing.T) {
	_, tr := newTransport(t)
	hostile := []string{
		"; rm -rf /",
		"$(echo pwned)",
		"`echo pwned`",
		"arg with spaces",
		"quote'\"d",
		"line1\nline2",
		"a|b && c > f",
		"it's quoted",
	}
	res, err := tr.Run(context.Background(), transport.RunRequest{
		Argv: append([]string{"/bin/echo"}, hostile...),
		Dir:  "/tmp",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(hostile, " ") + "\n"
	if string(res.Stdout) != want {
		t.Errorf("argv altered or interpreted over ssh:\n got %q\nwant %q", res.Stdout, want)
	}
}

func TestSSHConformanceStreamsAndExitCode(t *testing.T) {
	_, tr := newTransport(t)
	res, err := tr.Run(context.Background(), transport.RunRequest{
		Argv: []string{"/bin/sh", "-c", "echo out-line; echo err-line >&2; exit 3"},
		Dir:  "/tmp",
	})
	if err != nil {
		t.Fatalf("non-zero exit must be a result, not a transport error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
	if string(res.Stdout) != "out-line\n" || string(res.Stderr) != "err-line\n" {
		t.Errorf("stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

func TestSSHConformanceExactEnv(t *testing.T) {
	_, tr := newTransport(t)
	res, err := tr.Run(context.Background(), transport.RunRequest{
		Argv: []string{"/usr/bin/env"},
		Dir:  "/tmp",
		Env:  map[string]string{"DEPLOY_TEST": "1", "PATH": "/usr/bin:/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("child env must be exactly the provided map, got %q", res.Stdout)
	}
	seen := map[string]bool{}
	for _, l := range lines {
		seen[l] = true
	}
	if !seen["DEPLOY_TEST=1"] || !seen["PATH=/usr/bin:/bin"] {
		t.Errorf("env entries missing: %q", res.Stdout)
	}
}

func TestSSHConformanceWorkingDirectory(t *testing.T) {
	_, tr := newTransport(t)
	dir := t.TempDir()
	res, err := tr.Run(context.Background(), transport.RunRequest{
		Argv: []string{"/bin/pwd"},
		Dir:  dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); filepath.Clean(got) != filepath.Clean(dir) {
		t.Errorf("pwd = %q, want %q", got, dir)
	}
}

func TestSSHConformanceRelativeDirRejected(t *testing.T) {
	_, tr := newTransport(t)
	if _, err := tr.Run(context.Background(), transport.RunRequest{Argv: []string{"/bin/pwd"}, Dir: "relative/dir"}); err == nil {
		t.Error("relative Dir must be rejected")
	}
}

func TestSSHConformanceCancellationPreservesPartialOutput(t *testing.T) {
	_, tr := newTransport(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	res, err := tr.Run(ctx, transport.RunRequest{
		Argv: []string{"/bin/sh", "-c", "echo partial-line; sleep 30"},
		Dir:  "/tmp",
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if !strings.Contains(string(res.Stdout), "partial-line") {
		t.Errorf("partial output lost on cancellation: %q", res.Stdout)
	}
}

func TestSSHConformancePutRoundTrip(t *testing.T) {
	_, tr := newTransport(t)
	dir := t.TempDir()
	dst := filepath.Join(dir, "releases", "0.1.0", "bundle.tar")
	content := []byte{0x00, 0x01, 0xfe, 0xff, 'h', 'i'}
	if err := tr.Put(context.Background(), transport.PutRequest{Path: dst, Content: content, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Error("bytes differ after sftp put")
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", info.Mode().Perm())
	}
	if err := tr.Put(context.Background(), transport.PutRequest{Path: dst, Content: []byte("v2-longer")}); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(dst)
	if string(got) != "v2-longer" {
		t.Errorf("overwrite failed: %q", got)
	}
	info, _ = os.Stat(dst)
	if info.Mode().Perm() != 0o644 {
		t.Errorf("default mode after overwrite = %v", info.Mode().Perm())
	}
}

func TestSSHConformanceUnsafePathsRejected(t *testing.T) {
	_, tr := newTransport(t)
	dir := t.TempDir()
	for _, p := range []string{"relative.bin", "", dir + "/../escape.bin"} {
		if err := tr.Put(context.Background(), transport.PutRequest{Path: p, Content: []byte("x")}); err == nil {
			t.Errorf("unsafe path %q accepted", p)
		}
	}
}

func TestWrongHostKeyRefused(t *testing.T) {
	signer := clientSigner(t)
	ts := startTestServer(t, signer.PublicKey())
	otherKey := generateSigner(t).PublicKey()
	_, err := dialTransport(t, ts, signer, otherKey)
	if err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Fatalf("err = %v, want host key mismatch", err)
	}
}

func TestMissingHostKeyRefused(t *testing.T) {
	ts := startTestServer(t)
	signer := clientSigner(t)
	_, err := New(context.Background(), Config{
		Host:   "127.0.0.1",
		Port:   mustPort(t, ts),
		User:   "deploy",
		Signer: signer,
	})
	if err == nil || !strings.Contains(err.Error(), "pinned host key") {
		t.Fatalf("err = %v, want pinned host key requirement", err)
	}
}

func TestBadPrivateKeyRefused(t *testing.T) {
	if _, err := ParsePrivateKey([]byte("not a key")); err == nil {
		t.Fatal("garbage private key accepted")
	}
}

func TestAuthFailureRefused(t *testing.T) {
	ts := startTestServer(t)
	_, err := dialTransport(t, ts, clientSigner(t), nil)
	if err == nil {
		t.Fatal("handshake against a server that does not know our key must fail")
	}
}

func TestParseHostKeyRoundTrip(t *testing.T) {
	signer := clientSigner(t)
	ts := startTestServer(t, signer.PublicKey())
	line := strings.TrimSpace(string(gossh.MarshalAuthorizedKey(ts.hostKey)))
	parsed, err := ParseHostKey(line)
	if err != nil {
		t.Fatal(err)
	}
	if !keysEqual(parsed, ts.hostKey) {
		t.Error("parsed host key differs")
	}
}

func TestServerDisconnectIsTransportError(t *testing.T) {
	_, tr := newTransport(t)
	_, err := tr.Run(context.Background(), transport.RunRequest{
		Argv: []string{"__disconnect__"},
		Dir:  "/tmp",
	})
	if err == nil {
		t.Fatal("disconnect must surface as a transport error")
	}
}

func TestBuildCommandDeterministic(t *testing.T) {
	env := map[string]string{"B": "2", "A": "1"}
	c1, err := buildCommand("/opt/x y", env, []string{"./run.sh", "--flag", "it's"})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := buildCommand("/opt/x y", map[string]string{"A": "1", "B": "2"}, []string{"./run.sh", "--flag", "it's"})
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Errorf("command not deterministic:\n%s\n%s", c1, c2)
	}
	if !strings.Contains(c1, `cd '/opt/x y'`) {
		t.Errorf("dir not quoted: %s", c1)
	}
	if !strings.Contains(c1, `'it'\''s'`) {
		t.Errorf("single quote not encoded: %s", c1)
	}
	if !strings.Contains(c1, `env -i 'A=1' 'B=2'`) {
		t.Errorf("env not sorted/quoted: %s", c1)
	}
}
