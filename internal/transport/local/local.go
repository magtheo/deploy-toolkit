package local

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/magtheo/deploy-toolkit/internal/transport"
)

type Transport struct{}

func New() *Transport { return &Transport{} }

var _ transport.Transport = (*Transport)(nil)

func checkTargetPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("destination path is empty")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("destination path %q must be absolute", path)
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return "", fmt.Errorf("destination path %q contains %q segments", path, "..")
		}
	}
	return filepath.Clean(path), nil
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
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
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
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		return transport.RunResult{}, fmt.Errorf("argv must name a program")
	}
	if req.Dir == "" {
		return transport.RunResult{}, fmt.Errorf("an explicit working directory is required")
	}
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.Dir
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
		return transport.RunResult{}, ctx.Err()
	}
	res := transport.RunResult{
		ExitCode: 0,
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
	}
	if runErr != nil {
		exitErr, ok := runErr.(*exec.ExitError)
		if !ok {
			return transport.RunResult{Stdout: res.Stdout, Stderr: res.Stderr}, fmt.Errorf("start %s in %s: %w", req.Argv[0], req.Dir, runErr)
		}
		res.ExitCode = exitErr.ExitCode()
	}
	return res, nil
}
