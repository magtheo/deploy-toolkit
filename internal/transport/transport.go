package transport

import (
	"context"
	"os"
)

type Transport interface {
	Put(ctx context.Context, req PutRequest) error
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}

type PutRequest struct {
	Path    string
	Content []byte
	Mode    os.FileMode
}

type RunRequest struct {
	Argv []string
	Dir  string
	Env  map[string]string
}

type RunResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}
