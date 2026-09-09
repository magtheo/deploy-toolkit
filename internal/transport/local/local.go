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
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Dir = dir
	if req.Env != nil {
		env := make([]string, 0, len(req.Env))
		for k, v := range req.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return transport.RunResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, ctx.Err()
	}
	res := transport.RunResult{
		ExitCode: 0,
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
	}
	if runErr != nil {
		exitErr, ok := runErr.(*exec.ExitError)
		if !ok {
			return transport.RunResult{Stdout: res.Stdout, Stderr: res.Stderr}, &transport.StartError{Err: fmt.Errorf("start %s in %s: %w", req.Argv[0], dir, runErr)}
		}
		res.ExitCode = exitErr.ExitCode()
	}
	return res, nil
}
