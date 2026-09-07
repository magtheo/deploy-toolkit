package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/transport"
)

func TestSSHStartFailuresAreTransportErrors(t *testing.T) {
	_, tr := newTransport(t)
	dir := t.TempDir()
	cases := []transport.RunRequest{
		{Argv: []string{"/bin/echo", "x"}, Dir: dir + "/missing-dir"},
		{Argv: []string{dir + "/no-such-program"}, Dir: dir},
	}
	perm := dir + "/perm-denied.sh"
	if err := os.WriteFile(perm, []byte("#!/bin/sh\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	cases = append(cases, transport.RunRequest{Argv: []string{perm}, Dir: dir})
	for i, req := range cases {
		_, err := tr.Run(context.Background(), req)
		if err == nil {
			t.Errorf("case %d: start failure must be a transport error", i)
			continue
		}
		var startErr *transport.StartError
		if !errors.As(err, &startErr) {
			t.Errorf("case %d: err = %v, want StartError", i, err)
		}
	}
}

func TestSSHHostileEnvKeys(t *testing.T) {
	_, tr := newTransport(t)
	res, err := tr.Run(context.Background(), transport.RunRequest{
		Argv: []string{"/usr/bin/env"},
		Dir:  "/tmp",
		Env:  map[string]string{"-S": "value-with-dash-key", "X": "-n not an option"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("env entries = %q", res.Stdout)
	}
	seen := map[string]bool{}
	for _, l := range lines {
		seen[l] = true
	}
	if !seen["-S=value-with-dash-key"] || !seen["X=-n not an option"] {
		t.Errorf("hostile env keys mangled: %q", res.Stdout)
	}
}

func TestSSHRunRequestValidation(t *testing.T) {
	_, tr := newTransport(t)
	dir := t.TempDir()
	cases := []transport.RunRequest{
		{Argv: []string{"/bin/echo", "a\x00b"}, Dir: dir},
		{Argv: []string{"/usr/bin/env"}, Dir: dir, Env: map[string]string{"A=B": "1"}},
		{Argv: []string{"/usr/bin/env"}, Dir: dir, Env: map[string]string{"": "1"}},
		{Argv: []string{"/usr/bin/env"}, Dir: dir, Env: map[string]string{"K": "v\x00"}},
	}
	for i, req := range cases {
		if _, err := tr.Run(context.Background(), req); err == nil {
			t.Errorf("case %d: invalid request accepted", i)
		}
	}
}

func TestSSHConcurrentPutsAreIsolated(t *testing.T) {
	_, tr := newTransport(t)
	dir := t.TempDir()
	dst := filepath.Join(dir, "state.json")
	const writers = 16
	contents := make([][]byte, writers)
	for i := range contents {
		contents[i] = []byte(fmt.Sprintf("payload-%02d-%s", i, strings.Repeat("x", i*64)))
	}
	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(content []byte) {
			defer wg.Done()
			for attempt := 0; attempt < 3; attempt++ {
				err := tr.Put(context.Background(), transport.PutRequest{Path: dst, Content: content, Mode: 0o644})
				if err == nil {
					return
				}
				if attempt == 2 {
					errCh <- err
				}
			}
		}(contents[i])
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent put failed: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	ok := false
	for _, c := range contents {
		if string(got) == string(c) {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("destination content %q is not one of the complete payloads (interleaving/truncation)", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".put-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestStalledHandshakeHonorsContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 1)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	signer := clientSigner(t)
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = New(ctx, Config{Host: "127.0.0.1", Port: port, User: "deploy", Signer: signer, HostKey: generateSigner(t).PublicKey()})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("handshake cancellation took too long")
	}
}

func TestStalledPutHonorsContext(t *testing.T) {
	signer := clientSigner(t)
	ts := startTestServer(t, signer.PublicKey())
	ts.stallSftp = true
	tr, err := dialTransport(t, ts, signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tr.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = tr.Put(ctx, transport.PutRequest{Path: "/tmp/never-arrives.bin", Content: []byte("x")})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("put cancellation took too long")
	}
}
