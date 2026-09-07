package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"

	"github.com/pkg/sftp"
)

type testServer struct {
	addr    string
	hostKey gossh.PublicKey
	ln      net.Listener
}

func generateSigner(t *testing.T) gossh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func startTestServer(t *testing.T, authorized ...gossh.PublicKey) *testServer {
	t.Helper()
	hostSigner := generateSigner(t)
	config := &gossh.ServerConfig{
		PublicKeyCallback: func(_ gossh.ConnMetadata, key gossh.PublicKey) (*gossh.Permissions, error) {
			for _, a := range authorized {
				if keysEqual(a, key) {
					return &gossh.Permissions{}, nil
				}
			}
			return nil, fmt.Errorf("auth rejected")
		},
	}
	config.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ts := &testServer{addr: ln.Addr().String(), hostKey: hostSigner.PublicKey(), ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go ts.handleConn(conn, config)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ts
}

func (ts *testServer) handleConn(conn net.Conn, config *gossh.ServerConfig) {
	sconn, chans, reqs, err := gossh.NewServerConn(conn, config)
	if err != nil {
		conn.Close()
		return
	}
	defer sconn.Close()
	go gossh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(gossh.UnknownChannelType, "unsupported")
			continue
		}
		channel, requests, err := newCh.Accept()
		if err != nil {
			continue
		}
		go ts.handleSession(channel, requests)
	}
}

func (ts *testServer) handleSession(channel gossh.Channel, requests <-chan *gossh.Request) {
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
			if err := gossh.Unmarshal(req.Payload, &payload); err != nil || payload.Name != "sftp" {
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

func (ts *testServer) runCommand(channel gossh.Channel, command string) {
	if strings.Contains(command, "__disconnect__") {
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
