package local

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/transport"
)

func TestRunCapturesStreamsAndPreservesExitCode(t *testing.T) {
	tr := New()
	res, err := tr.Run(context.Background(), transport.RunRequest{
		Argv: []string{"/bin/sh", "-c", "echo out-line; echo err-line >&2; exit 3"},
		Dir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("non-zero exit must be a result, not a transport error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
	if string(res.Stdout) != "out-line\n" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if string(res.Stderr) != "err-line\n" {
		t.Errorf("stderr = %q", res.Stderr)
	}
}

func TestRunSuccessfulZeroExit(t *testing.T) {
	tr := New()
	res, err := tr.Run(context.Background(), transport.RunRequest{
		Argv: []string{"/bin/echo", "hello"},
		Dir:  t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || string(res.Stdout) != "hello\n" {
		t.Errorf("res = %+v", res)
	}
}

func TestRunNoShellInterpretation(t *testing.T) {
	tr := New()
	hostile := []string{
		"; rm -rf /",
		"$(echo pwned)",
		"`echo pwned`",
		"arg with spaces",
		"quote'\"d",
		"line1\nline2",
		"a|b && c > f",
	}
	argv := append([]string{"/bin/echo"}, hostile...)
	res, err := tr.Run(context.Background(), transport.RunRequest{Argv: argv, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(hostile, " ") + "\n"
	if string(res.Stdout) != want {
		t.Errorf("argv was altered or interpreted:\n got %q\nwant %q", res.Stdout, want)
	}
}

func TestRunExactEnvironment(t *testing.T) {
	tr := New()
	dir := t.TempDir()
	res, err := tr.Run(context.Background(), transport.RunRequest{
		Argv: []string{"/usr/bin/env"},
		Dir:  dir,
		Env:  map[string]string{"DEPLOY_TEST": "1", "PATH": "/usr/bin:/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.Stdout)
	lines := splitLines(out)
	if len(lines) != 2 {
		t.Errorf("child env must be exactly the provided map, got %d entries: %q", len(lines), out)
	}
	seen := map[string]bool{}
	for _, l := range lines {
		seen[l] = true
	}
	if !seen["DEPLOY_TEST=1"] || !seen["PATH=/usr/bin:/bin"] {
		t.Errorf("env entries missing: %q", out)
	}
}

func TestRunNilEnvInheritsParent(t *testing.T) {
	tr := New()
	res, err := tr.Run(context.Background(), transport.RunRequest{
		Argv: []string{"/bin/sh", "-c", "echo $HOME"},
		Dir:  t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) == "\n" || string(res.Stdout) == "" {
		t.Errorf("expected inherited HOME, got %q", res.Stdout)
	}
}

func TestRunExplicitWorkingDirectory(t *testing.T) {
	tr := New()
	dir := t.TempDir()
	res, err := tr.Run(context.Background(), transport.RunRequest{
		Argv: []string{"/bin/pwd"},
		Dir:  dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := filepath.Clean(string(bytes.TrimRight(res.Stdout, "\n")))
	if got != filepath.Clean(dir) {
		t.Errorf("pwd = %q, want %q", got, dir)
	}
}

func TestRunMissingDirFailsClosed(t *testing.T) {
	tr := New()
	if _, err := tr.Run(context.Background(), transport.RunRequest{Argv: []string{"/bin/echo", "x"}}); err == nil {
		t.Error("empty Dir must be rejected")
	}
	if _, err := tr.Run(context.Background(), transport.RunRequest{Argv: nil, Dir: t.TempDir()}); err == nil {
		t.Error("empty argv must be rejected")
	}
}

func TestRunContextCancellation(t *testing.T) {
	tr := New()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := tr.Run(ctx, transport.RunRequest{
		Argv: []string{"/bin/sleep", "30"},
		Dir:  t.TempDir(),
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cancellation took %v", elapsed)
	}
}

func TestPutRoundTripExactBytesAndMode(t *testing.T) {
	tr := New()
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
	if !bytes.Equal(got, content) {
		t.Errorf("bytes differ: %v vs %v", got, content)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", info.Mode().Perm())
	}
}

func TestPutDefaultModeAndOverwrite(t *testing.T) {
	tr := New()
	dir := t.TempDir()
	dst := filepath.Join(dir, "state.json")
	if err := tr.Put(context.Background(), transport.PutRequest{Path: dst, Content: []byte("v1")}); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(dst)
	if info.Mode().Perm() != 0o644 {
		t.Errorf("default mode = %v", info.Mode().Perm())
	}
	if err := tr.Put(context.Background(), transport.PutRequest{Path: dst, Content: []byte("v2-longer-content")}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "v2-longer-content" {
		t.Errorf("overwrite failed: %q", got)
	}
}

func TestPutRejectsUnsafePaths(t *testing.T) {
	tr := New()
	dir := t.TempDir()
	cases := []string{
		"relative/path.bin",
		"",
		dir + "/../escape.bin",
		"/tmp/../escape2.bin",
	}
	for _, p := range cases {
		if err := tr.Put(context.Background(), transport.PutRequest{Path: p, Content: []byte("x")}); err == nil {
			t.Errorf("unsafe path %q accepted", p)
		}
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
