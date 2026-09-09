package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"os/exec"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"

	"github.com/pkg/sftp"
)

// fakeSSHServer is a minimal in-process SSH server for transport-level
// failure injection. It performs a real handshake and public-key
// authorization and executes commands through /bin/sh exactly like a
// real target — except when the command matches failIf, in which case
// the exec request is accepted and the channel is closed instead of
// answered. To the client that is a dead transport: the connection is
// up, the session starts, and the read produces no exit status.
func startFakeSSH(t *testing.T, authorized gossh.PublicKey, failIf string) *fakeSSHServer {
	return startFakeSSHOpts(t, authorized, failIf, false)
}

// startFakeSSHOpts additionally accepts a dead-probe mode: the server
// performs the real handshake and answers exec, but REJECTS the sftp
// subsystem — connect succeeds, every substrate read fails. This is how
// transport death during a ProbePath walk presents to the CLI.
func startFakeSSHOpts(t *testing.T, authorized gossh.PublicKey, failIf string, rejectSFTP bool) *fakeSSHServer {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := gossh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	config := &gossh.ServerConfig{
		PublicKeyCallback: func(_ gossh.ConnMetadata, key gossh.PublicKey) (*gossh.Permissions, error) {
			if keysEqual(authorized, key) {
				return &gossh.Permissions{}, nil
			}
			return nil, io.EOF
		},
	}
	config.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ts := &fakeSSHServer{ln: ln, config: config, failIf: failIf, rejectSFTP: rejectSFTP, hostKeyLine: string(gossh.MarshalAuthorizedKey(hostSigner.PublicKey()))}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go ts.handleConn(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ts
}

func keysEqual(a, b gossh.PublicKey) bool {
	return strings.EqualFold(a.Type(), b.Type()) && string(a.Marshal()) == string(b.Marshal())
}

type fakeSSHServer struct {
	ln          net.Listener
	config      *gossh.ServerConfig
	failIf      string
	rejectSFTP  bool
	hostKeyLine string
}

func (ts *fakeSSHServer) addr() string    { return ts.ln.Addr().String() }
func (ts *fakeSSHServer) hostKey() string { return ts.hostKeyLine }
func (ts *fakeSSHServer) hostPort() (string, int) {
	host, portStr, _ := net.SplitHostPort(ts.addr())
	port := 0
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	return host, port
}

func (ts *fakeSSHServer) handleConn(conn net.Conn) {
	sconn, chans, reqs, err := gossh.NewServerConn(conn, ts.config)
	if err != nil {
		conn.Close()
		return
	}
	_ = sconn
	go gossh.DiscardRequests(reqs)
	for newChan := range chans {
		go ts.handleSession(newChan)
	}
}

func (ts *fakeSSHServer) handleSession(newChan gossh.NewChannel) {
	channel, requests, err := newChan.Accept()
	if err != nil {
		newChan.Reject(gossh.ConnectionFailed, "session rejected")
		return
	}
	defer channel.Close()
	for req := range requests {
		switch req.Type {
		case "exec":
			var payload struct{ Command string }
			if err := gossh.Unmarshal(req.Payload, &payload); err != nil {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			ts.runCommand(channel, payload.Command)
			return
		case "subsystem":
			var payload struct{ Name string }
			if err := gossh.Unmarshal(req.Payload, &payload); err != nil || payload.Name != "sftp" || ts.rejectSFTP {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			srv, err := sftp.NewServer(channel)
			if err != nil {
				return
			}
			srv.Serve()
			srv.Close()
			return
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

func (ts *fakeSSHServer) runCommand(channel gossh.Channel, command string) {
	if strings.Contains(command, ts.failIf) {
		channel.Close()
		return
	}
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Stdout = channel
	cmd.Stderr = channel.Stderr()
	exitCode := uint32(0)
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = uint32(exitErr.ExitCode())
		} else {
			exitCode = 127
		}
	}
	channel.SendRequest("exit-status", false, gossh.Marshal(struct{ Status uint32 }{exitCode}))
}
