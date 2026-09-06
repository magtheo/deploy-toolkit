package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
)

const version = "0.0.0-dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "validate":
		return runValidate(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "deployctl %s\n", version)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "deployctl: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `deployctl %s — release promotion and deployment engine

Usage:
  deployctl validate <manifest.yaml>...   validate project, release, environment or target manifests
  deployctl version                       print version

Manifests are validated against the frozen Consumer Contract v1 schemas
embedded in the binary.
`, version)
}

func runValidate(paths []string, stdout, stderr io.Writer) int {
	if len(paths) == 0 {
		fmt.Fprintln(stderr, "deployctl validate: no manifests given")
		return 2
	}
	failed := false
	for _, path := range paths {
		if err := validateFile(path, stdout); err != nil {
			failed = true
			fmt.Fprintf(stdout, "✗ %s\n  %v\n", path, err)
			continue
		}
	}
	if failed {
		return 1
	}
	return 0
}

func validateFile(path string, stdout io.Writer) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	res, err := manifest.Parse(data, "")
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "✓ %s  %s  %s\n", filepath.Base(path), res.Header.Kind, res.Header.APIVersion)
	return nil
}
