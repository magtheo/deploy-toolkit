package ssh

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	gossh "golang.org/x/crypto/ssh"

	"github.com/pkg/sftp"

	"github.com/magtheo/deploy-toolkit/internal/transport"
)

type Config struct {
	Host    string
	Port    int
	User    string
	Signer  gossh.Signer
	HostKey gossh.PublicKey
}

type Transport struct {
	conn *gossh.Client
}

func New(ctx context.Context, cfg Config) (*Transport, error) {
	if cfg.Host == "" || cfg.User == "" {
		return nil, fmt.Errorf("ssh: host and user are required")
	}
	if cfg.Signer == nil {
		return nil, fmt.Errorf("ssh: a private key signer is required")
	}
	if cfg.HostKey == nil {
		return nil, fmt.Errorf("ssh: a pinned host key is required; refusing to connect without strict host verification")
	}
	port := cfg.Port
	if port == 0 {
		port = 22
	}
	transportConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, fmt.Sprint(port)))
	if err != nil {
		return nil, fmt.Errorf("ssh: dial %s: %w", net.JoinHostPort(cfg.Host, fmt.Sprint(port)), err)
	}
	type handshake struct {
		conn  gossh.Conn
		chans <-chan gossh.NewChannel
		reqs  <-chan *gossh.Request
		err   error
	}
	handshakeDone := make(chan handshake, 1)
	go func() {
		conn, chans, reqs, err := gossh.NewClientConn(transportConn, net.JoinHostPort(cfg.Host, fmt.Sprint(port)), &gossh.ClientConfig{
			User: cfg.User,
			Auth: []gossh.AuthMethod{gossh.PublicKeys(cfg.Signer)},
			HostKeyCallback: func(hostname string, remote net.Addr, key gossh.PublicKey) error {
				if !keysEqual(key, cfg.HostKey) {
					return fmt.Errorf("ssh: host key mismatch for %s: got %s, pinned %s", hostname, gossh.FingerprintSHA256(key), gossh.FingerprintSHA256(cfg.HostKey))
				}
				return nil
			},
		})
		handshakeDone <- handshake{conn, chans, reqs, err}
	}()
	var hs handshake
	select {
	case hs = <-handshakeDone:
	case <-ctx.Done():
		transportConn.Close()
		return nil, ctx.Err()
	}
	if hs.err != nil {
		transportConn.Close()
		return nil, fmt.Errorf("ssh: handshake with %s: %w", cfg.Host, hs.err)
	}
	go gossh.DiscardRequests(hs.reqs)
	return &Transport{conn: gossh.NewClient(hs.conn, hs.chans, hs.reqs)}, nil
}

func keysEqual(a, b gossh.PublicKey) bool {
	return strings.EqualFold(a.Type(), b.Type()) && bytes.Equal(a.Marshal(), b.Marshal())
}

func (t *Transport) Close() error {
	if t.conn == nil {
		return nil
	}
	err := t.conn.Close()
	t.conn = nil
	return err
}

// ParseHostKey parses a pinned OpenSSH public host key line, e.g.
// "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... some comment".
func ParseHostKey(line string) (gossh.PublicKey, error) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 {
		return nil, fmt.Errorf("ssh: host key must be an OpenSSH public key line")
	}
	key, _, _, _, err := gossh.ParseAuthorizedKey([]byte(strings.Join(fields[:2], " ")))
	if err != nil {
		return nil, fmt.Errorf("ssh: parse host key: %w", err)
	}
	return key, nil
}

func ParsePrivateKey(data []byte) (gossh.Signer, error) {
	signer, err := gossh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("ssh: parse private key: %w", err)
	}
	return signer, nil
}

func (t *Transport) Run(ctx context.Context, req transport.RunRequest) (transport.RunResult, error) {
	if ctx.Err() != nil {
		return transport.RunResult{}, ctx.Err()
	}
	if err := transport.ValidateRunRequest(req); err != nil {
		return transport.RunResult{}, err
	}
	dir, err := transport.ValidateAbsolutePath(req.Dir)
	if err != nil {
		return transport.RunResult{}, &transport.StartError{Err: fmt.Errorf("working directory: %w", err)}
	}
	command, err := buildCommand(dir, req.Env, req.Argv)
	if err != nil {
		return transport.RunResult{}, err
	}
	session, err := t.conn.NewSession()
	if err != nil {
		return transport.RunResult{}, fmt.Errorf("ssh: open session: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()

	select {
	case <-ctx.Done():
		session.Close()
		<-done
		return transport.RunResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, ctx.Err()
	case err := <-done:
		res := transport.RunResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
		if err == nil {
			return res, nil
		}
		exitErr, ok := err.(*gossh.ExitError)
		if !ok {
			return res, fmt.Errorf("ssh: run: %w", err)
		}
		res.ExitCode = exitErr.ExitStatus()
		if res.ExitCode == 126 || res.ExitCode == 127 {
			return res, &transport.StartError{ExitCode: res.ExitCode, Err: fmt.Errorf("target dispatch failed for %s in %s", req.Argv[0], dir)}
		}
		return res, nil
	}
}

// Put stages req.Content atomically at req.Path via a dedicated SFTP
// session. The session is owned by this call: the subsystem request, the
// SFTP handshake and the transfer all run on one worker goroutine, so a
// single session.Close() on context cancellation unblocks the worker at
// any stage — including while the handshake is still waiting for the
// server's version packet. Closing the session leaves the shared SSH
// connection usable for later operations.
func (t *Transport) Put(ctx context.Context, req transport.PutRequest) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	dst, err := transport.ValidateAbsolutePath(req.Path)
	if err != nil {
		return err
	}
	mode := req.Mode
	if mode == 0 {
		mode = 0o644
	}
	session, err := t.conn.NewSession()
	if err != nil {
		return fmt.Errorf("ssh: open sftp session: %w", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		return fmt.Errorf("ssh: sftp stdout pipe: %w", err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		return fmt.Errorf("ssh: sftp stdin pipe: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		defer session.Close()
		if err := session.RequestSubsystem("sftp"); err != nil {
			done <- fmt.Errorf("ssh: request sftp subsystem: %w", err)
			return
		}
		cl, err := sftp.NewClientPipe(stdout, stdin)
		if err != nil {
			done <- fmt.Errorf("ssh: open sftp: %w", err)
			return
		}
		defer cl.Close()
		done <- sftpPut(cl, dst, mode, req.Content)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		session.Close()
		<-done
		return ctx.Err()
	}
}

func sftpPut(cl *sftp.Client, dst string, mode os.FileMode, content []byte) error {
	dir := filepath.Dir(dst)
	if err := cl.MkdirAll(dir); err != nil {
		return fmt.Errorf("ssh: create %s: %w", dir, err)
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	tmp, err := cl.Create(fmt.Sprintf("%s/.put-%d-%s.tmp", dir, os.Getpid(), hex.EncodeToString(suffix[:])))
	if err != nil {
		return fmt.Errorf("ssh: create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		cl.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		cl.Remove(tmpName)
		return err
	}
	if err := syncBestEffort(tmp); err != nil {
		tmp.Close()
		cl.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		cl.Remove(tmpName)
		return err
	}
	if err := cl.PosixRename(tmpName, dst); err != nil {
		cl.Remove(tmpName)
		return fmt.Errorf("ssh: publish %s: %w", dst, err)
	}
	return nil
}

func syncBestEffort(f *sftp.File) error {
	err := f.Sync()
	if err == nil {
		return nil
	}
	var statusErr *sftp.StatusError
	if errors.As(err, &statusErr) && statusErr.Code == uint32(sftp.ErrSSHFxOpUnsupported) {
		return nil
	}
	return err
}
