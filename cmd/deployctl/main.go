package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/magtheo/deploy-toolkit/internal/bundle"
	gh "github.com/magtheo/deploy-toolkit/internal/github"
	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/oci"
	"github.com/magtheo/deploy-toolkit/internal/promotion"
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
	case "promotion":
		return runPromotion(args[1:], stdout, stderr)
	case "deploy":
		return runDeploy(context.Background(), args[1:], stdout, stderr)
	case "rollback":
		return runRollback(context.Background(), args[1:], stdout, stderr)
	case "status":
		return runStatus(context.Background(), args[1:], stdout, stderr)
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
  deployctl promotion propose <env> [flags]  open the human-authorization PR for a release
  deployctl promotion check [flags]          verify a promotion diff against the Promotion Diff Policy
  deployctl deploy <env> [flags]             deploy the release the environment pins
  deployctl rollback <env> [flags]           emergency/manual recovery: restore a previous release
  deployctl status <env> [flags]             report desired vs observed state and all recovery facts
  deployctl version                          print version

Operational exit codes (deploy, rollback): 0 success, 1 reported outcome
failure (determined — history records what happened), 2 usage/configuration
error, 3 infrastructure failure. The failure REPORT — never the exit code
alone — distinguishes pre-execution failures (nothing ran), bookkeeping
failures after a committed state, and uncertain outcomes (an attempt or
recovery marker is unresolved). Automation keys on the reported recovery
state, not on exit 3.

Release creation stops at the Release boundary: it never updates environments
and never opens pull requests. Promotion proposals stop at the open, verified
PR; merging it is the human authorization act.

Release create flags:
  --repo owner/name             source repository (required)
  --revision <full-sha>         candidate commit (required)
  --version 0.1.0               semantic version of the release (required)
  --migration-head "043"        migration head (required)
  --migration-mode <mode>       none | forward-compatible | maintenance-required | irreversible (required)
  --rollback-safe               declare the migration rollback safe
  --repo-dir .                  local checkout containing the revision (for the bundle)
  --releases-dir .deploy/releases

The release policy is anchored to the trusted integration branch (main); the
candidate must be reachable from its head. Registry authentication uses the
standard OCI keychain (~/.docker/config.json and credential helpers) — it is
independent of GITHUB_TOKEN, which is only the source API credential.

Promotion propose flags:
  --repo owner/name                          source repository (required)
  --release .deploy/releases/<p>-<v>.yaml    immutable release to promote (required)
  --repo-dir .                               local checkout containing the revision (for re-verification)

Promotion check flags (for the trusted CI workflow):
  --repo owner/name                          source repository (required)
  --base <sha>                               trusted base commit (required)
  --head <sha>                               promotion branch head (required)

Both require GITHUB_TOKEN.

Deploy flags:
  --repo-dir .                  checkout containing the release revision (for the bundle)
  --owner identity              recorded in the lock and history (default user@host)

The deployment runs the full engine sequence — environment lock, staging,
contract verification, preflight, migrate, apply, verify, observed-state
commit — against the release the environment file pins. Merging the
promotion PR is the authorization for this command; running it is not.

Rollback flags (manual/emergency recovery):
  --to <version>                version to restore (required)
  --from <version>              version being undone (default: the pinned release)
  --repo-dir .                  checkout containing both release revisions
  --confirm "sentence"          typed confirmation; omit to be prompted

Requires typing exactly: rollback <env> to <version>. The confirmation gate
precedes target contact: nothing — not even the connection — happens before
the sentence matches. Normal rollback of a healthy deployment is an ordinary
promotion with a reverse diff, not this command.

Status flags:
  --repo-dir .                  checkout containing .deploy/

Status is read-only but uses the deploy credential to inspect the target.
It fails closed: an unreadable lock, attempt marker, recovery marker or
observed state is DEGRADED EVIDENCE — HEALTHY is never claimed while any
evidence is unreadable. A held lock dominates the markers (an operation in
flight creates them legitimately). Rollback guidance derives from the
attempt marker's own origin; a first deployment has no restore target.

SSH targets (deploy, rollback, status): the manifest names environment
variables; the environment holds values. credentialFrom and hostKeyFrom
name variables whose values are PATHS to the private-key file and the
pinned host-key file (authorized_keys format). Missing or unparseable
credential configuration is exit 2; unreachable targets are exit 3 with
"did not start — nothing was executed".`, version)
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

func runPromotion(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, `usage: deployctl promotion propose <env> [flags] | promotion check [flags]`)
		return 2
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		fmt.Fprintln(stderr, "deployctl promotion: GITHUB_TOKEN is not set")
		return 1
	}
	ctx := context.Background()
	switch args[0] {
	case "propose":
		return runPromotionPropose(ctx, args[1:], token, stdout, stderr)
	case "check":
		return runPromotionCheck(ctx, args[1:], token, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "deployctl promotion: unknown subcommand %q\n", args[0])
		return 2
	}
}

func runPromotionPropose(ctx context.Context, args []string, token string, stdout, stderr io.Writer) int {
	_, in, err := parseProposeArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 2
	}
	pr, err := promotion.Propose(ctx, *in, gh.New(token), oci.NewRemote(authn.DefaultKeychain), bundle.NewBuilder(in.RepoDir))
	if err != nil {
		fmt.Fprintf(stderr, "✗ promotion propose: %v\n", err)
		return 1
	}
	if pr.Unchanged {
		fmt.Fprintf(stdout, "✓ existing proposal is current: %s\n", pr.PR.URL)
		return 0
	}
	fmt.Fprintf(stdout, "✓ release re-verified against current eligibility policy\n")
	fmt.Fprintf(stdout, "✓ environment change limited to spec.release (%s → %s)\n", pr.From, pr.To)
	fmt.Fprintf(stdout, "✓ promotion commit %s created on %s\n", shortSHA(pr.CommitSHA), pr.Branch)
	fmt.Fprintf(stdout, "Opened:\n%s\n", pr.PR.URL)
	return 0
}

func parseProposeArgs(args []string) (string, *promotion.ProposeInput, error) {
	fs := flag.NewFlagSet("promotion propose", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		repo    = fs.String("repo", "", "source repository owner/name")
		relPath = fs.String("release", "", "path to the immutable release manifest")
		repoDir = fs.String("repo-dir", ".", "local checkout containing the revision")
	)
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", nil, fmt.Errorf("usage: deployctl promotion propose <environment> --release <path> --repo owner/name")
	}
	envName := args[0]
	if strings.Contains(envName, "/") {
		return "", nil, fmt.Errorf("environment must be a bare name, got %q", envName)
	}
	if err := fs.Parse(args[1:]); err != nil {
		return "", nil, err
	}
	if fs.NArg() > 0 {
		return "", nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return envName, &promotion.ProposeInput{
		Repo:        *repo,
		Environment: envName,
		ReleasePath: *relPath,
		RepoDir:     *repoDir,
	}, nil
}

func parseCheckArgs(args []string) (*promotion.CheckInput, error) {
	fs := flag.NewFlagSet("promotion check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		repo    = fs.String("repo", "", "source repository owner/name")
		base    = fs.String("base", "", "trusted base commit SHA")
		head    = fs.String("head", "", "promotion branch head SHA")
		repoDir = fs.String("repo-dir", "", "checkout containing the release source revision (required for new-release proposals)")
	)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return &promotion.CheckInput{Repo: *repo, Base: *base, Head: *head, RepoDir: *repoDir}, nil
}

func runPromotionCheck(ctx context.Context, args []string, token string, stdout, stderr io.Writer) int {
	in, err := parseCheckArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 2
	}
	res, err := promotion.Check(ctx, *in, gh.New(token), oci.NewRemote(authn.DefaultKeychain), bundle.NewBuilder(in.RepoDir))
	if err != nil {
		fmt.Fprintf(stderr, "✗ promotion check: %v\n", err)
		return 1
	}
	for _, m := range res.Messages {
		fmt.Fprintln(stdout, m)
	}
	if !res.Passed {
		return 1
	}
	fmt.Fprintln(stdout, "✓ promotion diff policy satisfied")
	return 0
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
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
		Revision:      *rev,
		Version:       *ver,
		MigrationHead: *migHead,
		MigrationMode: *migMode,
		RollbackSafe:  *rollbackSafe,
		ReleasesDir:   *releasesDir,
	}, gh.New(token), oci.NewRemote(authn.DefaultKeychain), bundle.NewBuilder(*repoDir))
	if err != nil {
		fmt.Fprintf(stderr, "✗ release create: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "✓ revision %s is reachable from %s@%s (head %s)\n", rep.SourceRevision, *repo, release.TrustedBranch, rep.BranchHead)
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
