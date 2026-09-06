package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/magtheo/deploy-toolkit/internal/bundle"
	gh "github.com/magtheo/deploy-toolkit/internal/github"
	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/oci"
	"github.com/magtheo/deploy-toolkit/internal/release"
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
	case "release":
		return runRelease(args[1:], stdout, stderr)
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
  deployctl validate <manifest.yaml>...      validate project, release, environment or target manifests
  deployctl release create [flags]           run eligibility and create an immutable release manifest
  deployctl version                          print version

Release creation stops at the Release boundary: it never updates environments
and never opens pull requests.

Release create flags:
  --repo owner/name             source repository (required)
  --branch main                 permitted promotion branch (required)
  --revision <full-sha>         candidate commit (required)
  --version 0.1.0               semantic version of the release (required)
  --migration-head "043"        migration head (required)
  --migration-mode <mode>       none | forward-compatible | maintenance-required | irreversible (required)
  --rollback-safe               declare the migration rollback safe
  --repo-dir .                  local checkout containing the revision (for the bundle)
  --releases-dir .deploy/releases

Release creation requires GITHUB_TOKEN.
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

func runRelease(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "create" {
		fmt.Fprintln(stderr, `usage: deployctl release create [flags]  (see "deployctl help")`)
		return 2
	}
	fs := flag.NewFlagSet("release create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		repo         = fs.String("repo", "", "source repository owner/name")
		branch       = fs.String("branch", "main", "permitted promotion branch")
		rev          = fs.String("revision", "", "candidate full commit SHA")
		ver          = fs.String("version", "", "release version")
		migHead      = fs.String("migration-head", "", "migration head")
		migMode      = fs.String("migration-mode", "", "migration mode")
		rollbackSafe = fs.Bool("rollback-safe", false, "migration is rollback safe")
		repoDir      = fs.String("repo-dir", ".", "local checkout containing the revision")
		releasesDir  = fs.String("releases-dir", filepath.Join(".deploy", "releases"), "output directory")
	)
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "deployctl release create: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		fmt.Fprintln(stderr, "deployctl release create: GITHUB_TOKEN is not set")
		return 1
	}
	ctx := context.Background()
	rep, err := release.Create(ctx, release.CreateInput{
		Repo:          *repo,
		Branch:        *branch,
		Revision:      *rev,
		Version:       *ver,
		MigrationHead: *migHead,
		MigrationMode: *migMode,
		RollbackSafe:  *rollbackSafe,
		ReleasesDir:   *releasesDir,
	}, gh.New(token), oci.NewRemote(&authn.Basic{Username: "deployctl", Password: token}), bundle.NewBuilder(*repoDir))
	if err != nil {
		fmt.Fprintf(stderr, "✗ release create: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "✓ revision %s is reachable from %s@%s (head %s)\n", rep.SourceRevision, *repo, *branch, rep.BranchHead)
	fmt.Fprintf(stdout, "✓ required checks passed (%d/%d)\n", len(rep.Checks), len(rep.Checks))
	fmt.Fprintf(stdout, "✓ artifacts resolved (%d)\n", len(rep.Artifacts))
	fmt.Fprintf(stdout, "✓ bundle constructed (%d files, %s)\n", len(rep.BundleFiles), rep.BundleDigest)
	fmt.Fprintf(stdout, "✓ release manifest validated\n")
	if rep.Unchanged {
		fmt.Fprintf(stdout, "Unchanged: %s\n", rep.ReleasePath)
	} else {
		fmt.Fprintf(stdout, "Created: %s\n", rep.ReleasePath)
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
