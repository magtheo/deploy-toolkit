package promotion

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/release"
)

var releaseFilePattern = regexp.MustCompile(`^\.deploy/releases/([a-z0-9][a-z0-9-]{0,62})-([0-9A-Za-z.\-+]+)\.yaml$`)

var releaseNamePattern = regexp.MustCompile(`^([a-z0-9][a-z0-9-]{0,62})-([0-9A-Za-z.\-+]+)\.yaml$`)

func releaseNameFromPath(filePath string) (string, string, error) {
	m := releaseNamePattern.FindStringSubmatch(path.Base(filePath))
	if m == nil {
		return "", "", fmt.Errorf("release file %s must be named <project>-<version>.yaml", filePath)
	}
	return m[1], m[2], nil
}

type CheckInput struct {
	Repo    string
	Base    string
	Head    string
	RepoDir string
}

type CheckOutcome struct {
	Passed      bool
	Messages    []string
	Environment string
	From        string
	To          string
}

func Check(ctx context.Context, in CheckInput, src Store, resolver release.Resolver, bundler release.Bundler) (*CheckOutcome, error) {
	res := &CheckOutcome{Passed: true}
	fail := func(format string, args ...any) {
		res.Passed = false
		res.Messages = append(res.Messages, fmt.Sprintf(format, args...))
	}

	if in.Repo == "" || in.Base == "" || in.Head == "" {
		return nil, fmt.Errorf("--repo, --base and --head are required")
	}

	liveHead, err := src.BranchHead(ctx, in.Repo, release.TrustedBranch)
	if err != nil {
		return nil, err
	}
	if in.Base != liveHead {
		fail("base %s is not the current %s head (%s); the proposal is stale and must be regenerated", in.Base, release.TrustedBranch, liveHead)
		return res, nil
	}
	parents, err := src.CommitParents(ctx, in.Repo, in.Head)
	if err != nil {
		return nil, err
	}
	if len(parents) != 1 || parents[0] != liveHead {
		fail("promotion head %s must be exactly one commit on top of the current %s head", in.Head, release.TrustedBranch)
		return res, nil
	}

	baseLeaves, err := src.CommitTreeLeaves(ctx, in.Repo, in.Base)
	if err != nil {
		return nil, err
	}
	headLeaves, err := src.CommitTreeLeaves(ctx, in.Repo, in.Head)
	if err != nil {
		return nil, err
	}

	out, err := evaluatePolicy(ctx, policyInput{Repo: in.Repo, Base: in.Base, Head: in.Head, RepoDir: in.RepoDir},
		diffTrees{baseLeaves: baseLeaves, headLeaves: headLeaves}, src, resolver, bundler)
	if err != nil {
		return nil, err
	}
	if out.Passed {
		out.Messages = append(out.Messages, "promotion diff policy satisfied")
	}
	return out, nil
}

// policyInput carries the identities evaluatePolicy reads content for.
type policyInput struct {
	Repo    string
	Base    string
	Head    string
	RepoDir string
}

// diffTrees holds the complete Git-tree leaf sets of the two sides of a
// transition. An empty leaf map stands for a zero SHA (branch creation or
// deletion); callers substitute it before calling evaluatePolicy.
type diffTrees struct {
	baseLeaves map[string]TreeLeaf
	headLeaves map[string]TreeLeaf
}

// evaluatePolicy is the semantic core shared by Check and Classify: the
// Promotion Diff Policy tree walk plus release-evidence re-verification over
// one base→head transition. Freshness — CAS, single-commit head, live head,
// ancestry — is the caller's responsibility and happens before this runs.
// Infrastructure failures (source reads, check-runs API, OCI, bundle I/O,
// unreadable trusted state) return an error — classified as ERROR by
// Classify and as a failed gate by Check; every policy failure about the
// transition's content is reported in the outcome, never as an error.
func evaluatePolicy(ctx context.Context, in policyInput, trees diffTrees, src Store, resolver release.Resolver, bundler release.Bundler) (*CheckOutcome, error) {
	res := &CheckOutcome{Passed: true}
	fail := func(format string, args ...any) {
		res.Passed = false
		res.Messages = append(res.Messages, fmt.Sprintf(format, args...))
	}

	changed := treeDiff(trees.baseLeaves, trees.headLeaves)

	trustedProjectBytes, err := src.FileAt(ctx, in.Repo, release.ProjectPath, in.Base)
	if err != nil {
		// Reading trusted state fails the transition with ERROR, not a
		// policy verdict — the classifier cannot interpret the repository.
		return nil, fmt.Errorf("trusted %s could not be read from base: %w", release.ProjectPath, err)
	}
	trustedRes, err := manifest.Parse(trustedProjectBytes, manifest.KindProject)
	if err != nil {
		return nil, fmt.Errorf("trusted %s is not a valid Project: %v", release.ProjectPath, err)
	}
	trusted := trustedRes.Project

	var (
		addedRelease string
		envChanged   []string
		other        []string
	)
	for _, f := range changed {
		switch {
		case strings.HasPrefix(f.Path, ReleasesDir+"/"):
			if f.Status == "added" {
				if addedRelease != "" {
					fail("more than one release file added (%s, %s)", addedRelease, f.Path)
				}
				addedRelease = f.Path
			} else {
				fail("existing release file %s must never be modified or removed (status %s) — releases are immutable", f.Path, f.Status)
			}
		case strings.HasPrefix(f.Path, EnvironmentsDir+"/"):
			if f.Status == "modified" {
				envChanged = append(envChanged, f.Path)
			} else {
				fail("environment file %s has illegal status %s", f.Path, f.Status)
			}
		default:
			other = append(other, f.Path)
		}
	}
	if len(other) > 0 {
		fail("promotion PRs may only touch %s and %s; illegal changes: %s", ReleasesDir, EnvironmentsDir, strings.Join(other, ", "))
	}
	envPath := ""
	if len(envChanged) == 1 {
		envPath = envChanged[0]
	} else {
		fail("exactly one environment file must change, found %d", len(envChanged))
	}

	if addedRelease != "" {
		m := releaseFilePattern.FindStringSubmatch(addedRelease)
		if m == nil {
			fail("added release file %s does not match canonical naming %s/<project>-<version>.yaml", addedRelease, ReleasesDir)
		} else {
			content, err := src.BlobAt(ctx, in.Repo, trees.headLeaves[addedRelease].OID)
			if err != nil {
				return nil, err
			}
			relRes, err := manifest.Parse(content, manifest.KindRelease)
			if err != nil {
				fail("added release file %s is not a valid Release: %v", addedRelease, err)
			} else {
				r := relRes.Release
				if r.Metadata.Project != m[1] {
					fail("release file %s declares project %q; filename must agree", addedRelease, r.Metadata.Project)
				}
				if r.Metadata.Version != m[2] {
					fail("release file %s declares version %q; filename must agree", addedRelease, r.Metadata.Version)
				}
				if trusted.Metadata.Name != r.Metadata.Project || trusted.Release.Source.Repository != r.Source.Repository {
					fail("added release %s/%s is not bound to trusted project %q (repository %q)", r.Metadata.Project, r.Metadata.Version, trusted.Metadata.Name, trusted.Release.Source.Repository)
				}
				if r.Source.Repository != in.Repo {
					fail("added release declares source repository %q, but %s is being evaluated", r.Source.Repository, in.Repo)
				}
				if bundler == nil || resolver == nil || in.RepoDir == "" {
					fail("new release evidence cannot be verified without a checkout (--repo-dir); refusing to pass a promotion on unverified evidence")
				} else if res.Passed {
					ev, err := release.Evaluate(ctx, release.EvalInput{Repo: in.Repo, Revision: r.Source.Revision}, src, resolver, bundler)
					if err != nil {
						var infra *release.InfraError
						if errors.As(err, &infra) {
							// Infrastructure failure (source reads, check-runs
							// API, OCI, bundle I/O): not a verdict about the
							// release — surface it for ERROR classification.
							return nil, err
						}
						fail("added release is not eligible under current policy: %v", err)
					} else {
						expected, err := release.RenderReleaseFor(ev, r.Metadata.Version, r.Migration)
						if err != nil {
							return nil, err
						}
						if string(expected) != string(content) {
							fail("added release file differs from what current deterministic eligibility would generate")
						}
					}
				}
			}
		}
	}

	if envPath != "" {
		envName := strings.TrimSuffix(path.Base(envPath), ".yaml")
		baseBytes, err := src.FileAt(ctx, in.Repo, envPath, in.Base)
		if err != nil {
			return nil, fmt.Errorf("read base environment: %w", err)
		}
		headBytes, err := src.FileAt(ctx, in.Repo, envPath, in.Head)
		if err != nil {
			return nil, fmt.Errorf("read head environment: %w", err)
		}
		baseRes, err := manifest.Parse(baseBytes, manifest.KindEnvironment)
		if err != nil {
			fail("base environment %s is not valid: %v", envPath, err)
			return res, nil
		}
		headRes, err := manifest.Parse(headBytes, manifest.KindEnvironment)
		if err != nil {
			fail("head environment %s is not valid: %v", envPath, err)
			return res, nil
		}
		be, he := baseRes.Environment, headRes.Environment
		res.Environment = envName
		res.From = be.Spec.Release
		res.To = he.Spec.Release
		if be.Metadata.Name != envName || he.Metadata.Name != envName {
			fail("environment file %s must declare metadata.name %q", envPath, envName)
		}
		if be.Spec.Target != he.Spec.Target {
			fail("spec.target must not change during promotion (%q → %q)", be.Spec.Target, he.Spec.Target)
		}
		if be.AutoRollback() != he.AutoRollback() {
			fail("failurePolicy must not change during promotion")
		}
		if be.Spec.Release == he.Spec.Release {
			fail("spec.release is unchanged (%q); this proposal is stale — regenerate it", be.Spec.Release)
		}
		target := he.Spec.Release
		if addedRelease != "" && target != addedRelease {
			fail("promotion adds release %q but spec.release points at %q; the added release must be the promotion target — releases enter trusted main only as part of their own promotion", addedRelease, target)
			return res, nil
		}
		m := releaseFilePattern.FindStringSubmatch(target)
		if m == nil {
			fail("spec.release target %q is not a canonical release path", target)
		} else if trusted.Metadata.Name != m[1] {
			fail("spec.release target %q belongs to project %q, not trusted project %q", target, m[1], trusted.Metadata.Name)
		}
		if target != addedRelease {
			if _, ok := trees.baseLeaves[target]; !ok {
				fail("spec.release points at %q which does not exist in the trusted base", target)
				return res, nil
			}
			content, err := src.BlobAt(ctx, in.Repo, trees.baseLeaves[target].OID)
			if err != nil {
				return nil, err
			}
			relRes, err := manifest.Parse(content, manifest.KindRelease)
			if err != nil {
				fail("existing release %s is not a valid Release: %v", target, err)
				return res, nil
			}
			r := relRes.Release
			if r.Metadata.Project != trusted.Metadata.Name || r.Source.Repository != trusted.Release.Source.Repository {
				fail("existing release %s is not bound to trusted project %q", target, trusted.Metadata.Name)
			}
			if r.Source.Repository != in.Repo {
				fail("existing release declares source repository %q, but %s is being evaluated", r.Source.Repository, in.Repo)
			}
		}
	}

	if res.Passed {
		res.Messages = append(res.Messages, "promotion diff policy satisfied")
	}
	return res, nil
}

func treeDiff(base, head map[string]TreeLeaf) []ChangedFile {
	var out []ChangedFile
	paths := make([]string, 0, len(base)+len(head))
	seen := make(map[string]bool)
	for p := range base {
		paths = append(paths, p)
		seen[p] = true
	}
	for p := range head {
		if !seen[p] {
			paths = append(paths, p)
			seen[p] = true
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		bl, hl := base[p], head[p]
		switch {
		case bl.OID == "":
			out = append(out, ChangedFile{Path: p, Status: "added", Leaf: hl})
		case hl.OID == "":
			out = append(out, ChangedFile{Path: p, Status: "removed"})
		case bl != hl:
			out = append(out, ChangedFile{Path: p, Status: "modified", Leaf: hl})
		}
	}
	return out
}
