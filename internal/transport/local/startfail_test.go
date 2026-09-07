package local

import (
	"context"
	"errors"
	"testing"

	"github.com/magtheo/deploy-toolkit/internal/transport"
)

func TestLocalStartFailuresAreTransportErrors(t *testing.T) {
	tr := New()
	dir := t.TempDir()
	cases := []transport.RunRequest{
		{Argv: []string{"/bin/echo", "x"}, Dir: dir + "/missing-dir"},
		{Argv: []string{dir + "/no-such-program"}, Dir: dir},
	}
	perm := dir + "/perm-denied.sh"
	if err := writeFile(perm, "#!/bin/sh\n", 0o000); err != nil {
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

func TestLocalHostileEnvKeys(t *testing.T) {
	tr := New()
	res, err := tr.Run(context.Background(), transport.RunRequest{
		Argv: []string{"/usr/bin/env"},
		Dir:  t.TempDir(),
		Env:  map[string]string{"-S": "value-with-dash-key", "X": "-n not an option"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(string(res.Stdout))
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

func TestLocalRunRequestValidation(t *testing.T) {
	tr := New()
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
