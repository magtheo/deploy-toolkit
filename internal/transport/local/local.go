package local

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/transport"
)

type Transport struct{}

func New() *Transport { return &Transport{} }

var _ transport.Transport = (*Transport)(nil)

func checkTargetPath(path string) (string, error) {
	return transport.ValidateAbsolutePath(path)
}

// ProbePath implements transport.Transport. The local kernel
// distinguishes ENOENT from ENOTDIR natively, so a single os.Stat of the
// final path suffices: ENOENT is proven absence (the kernel resolved
// every parent and found nothing at the leaf — including a dangling
// symlink at the final component), ENOTDIR and every other failure are
// errors. Symlinks are followed, matching the v0.1 semantics.
func (t *Transport) ProbePath(ctx context.Context, path string) (transport.PathState, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	p, err := checkTargetPath(path)
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(p)
	switch {
	case err == nil:
		if fi.IsDir() {
			return transport.PathDirectory, nil
		}
		return transport.PathFile, nil
	case errors.Is(err, fs.ErrNotExist):
		return transport.PathAbsent, nil
	default:
		return 0, fmt.Errorf("target: probe %s: %w", p, err)
	}
}

func (t *Transport) Put(ctx context.Context, req transport.PutRequest) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	dst, err := checkTargetPath(req.Path)
	if err != nil {
		return err
	}
	mode := req.Mode
	if mode == 0 {
		mode = 0o644
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".put-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(req.Content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	if d, err := os.Open(dir); err != nil {
		return fmt.Errorf("open %s for sync: %w", dir, err)
	} else {
		if err := d.Sync(); err != nil {
			d.Close()
			return fmt.Errorf("sync %s: %w", dir, err)
		}
		if err := d.Close(); err != nil {
			return fmt.Errorf("close %s: %w", dir, err)
		}
	}
	return nil
}

// runWaitDelay bounds how long Wait may be kept open by processes
// still holding inherited stdout/stderr pipes after the direct child
// is gone (cancelled, or exited while a descendant keeps the pipe
// open). Var for test overriding.
var runWaitDelay = 5 * time.Second

func (t *Transport) Run(ctx context.Context, req transport.RunRequest) (transport.RunResult, error) {
	if ctx.Err() != nil {
		return transport.RunResult{Fate: transport.RunNotStarted}, ctx.Err()
	}
	if err := transport.ValidateRunRequest(req); err != nil {
		return transport.RunResult{Fate: transport.RunNotStarted}, err
	}
	dir, err := transport.ValidateAbsolutePath(req.Dir)
	if err != nil {
		return transport.RunResult{Fate: transport.RunNotStarted}, &transport.StartError{Err: fmt.Errorf("working directory: %w", err)}
	}
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Dir = dir
	if req.Env != nil {
		env := make([]string, 0, len(req.Env))
		for k, v := range req.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	// Own process group + group SIGKILL on cancellation: the default
	// exec.CommandContext cancellation kills ONLY the direct child,
	// leaving grandchildren running (verified). Group kill is still
	// best-effort — a descendant that escaped the group via setsid is
	// NOT covered, so cancellation remains fate RunUnknown, never a
	// claimed termination.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// A cancelled or exited child whose descendants hold the inherited
	// pipes would otherwise block Wait forever (verified M1-9 hang):
	// WaitDelay bounds it. Expiry makes the fate unknown, not negative.
	cmd.WaitDelay = runWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		// fork/exec failed synchronously: definitively nothing ran.
		return transport.RunResult{Fate: transport.RunNotStarted}, &transport.StartError{Err: fmt.Errorf("start %s in %s: %w", req.Argv[0], dir, err)}
	}
	runErr := cmd.Wait()
	// Cancellation is checked FIRST: the group SIGKILL surfaces as an
	// ExitError ("signal: killed"), which must never be mistaken for a
	// determined hook exit. A cancellation — even one racing a genuine
	// exit — leaves the tree's fate unknowable (setsid escape is
	// undetectable), so the fate is RunUnknown and the error is the
	// context error, never a manufactured exit code.
	if ctx.Err() != nil {
		return transport.RunResult{Fate: transport.RunUnknown, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, ctx.Err()
	}
	if runErr == nil {
		return transport.RunResult{Fate: transport.RunExited, ExitCode: 0, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		// Determined nonzero exit (including external kills): an
		// observable hook outcome, not an error return (the lifecycle
		// reads the exit code).
		return transport.RunResult{Fate: transport.RunExited, ExitCode: exitErr.ExitCode(), Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
	}
	// WaitDelay expiry while a pipe-holder survives, or an unexpected
	// Wait error: the process tree's fate cannot be established.
	return transport.RunResult{Fate: transport.RunUnknown, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, fmt.Errorf("run %s: %w", req.Argv[0], runErr)
}
