package promotion

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/release"
)

var releaseFilePattern = regexp.MustCompile(`^\.deploy/releases/([a-z0-9][a-z0-9-]{0,62})-([0-9A-Za-z.\-+]+)\.yaml$`)

type CheckInput struct {
	Repo    string
	Base    string
	Head    string
	RepoDir string
}

type CheckOutcome struct {
	Passed   bool
	Messages []string
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

	basePaths, err := src.CommitTreePaths(ctx, in.Repo, in.Base)
	if err != nil {
		return nil, err
	}
	headPaths, err := src.CommitTreePaths(ctx, in.Repo, in.Head)
	if err != nil {
		return nil, err
	}
	changed := treeDiff(basePaths, headPaths)

	trustedProjectBytes, err := src.FileAt(ctx, in.Repo, release.ProjectPath, liveHead)
	if err != nil {
		fail("trusted %s could not be read from base: %v", release.ProjectPath, err)
		return res, nil
	}
	trustedRes, err := manifest.Parse(trustedProjectBytes, manifest.KindProject)
	if err != nil {
		fail("trusted %s is not a valid Project: %v", release.ProjectPath, err)
		return res, nil
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
			content, err := src.BlobAt(ctx, in.Repo, headPaths[addedRelease])
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
		m := releaseFilePattern.FindStringSubmatch(target)
		if m == nil {
			fail("spec.release target %q is not a canonical release path", target)
		} else if trusted.Metadata.Name != m[1] {
			fail("spec.release target %q belongs to project %q, not trusted project %q", target, m[1], trusted.Metadata.Name)
		}
		if target != addedRelease {
			if _, ok := basePaths[target]; !ok {
				fail("spec.release points at %q which does not exist in the trusted base", target)
				return res, nil
			}
			content, err := src.BlobAt(ctx, in.Repo, basePaths[target])
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

func treeDiff(base, head map[string]string) []ChangedFile {
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
	seenBase := func(p string) bool { _, ok := base[p]; return ok }
	seenHead := func(p string) bool { _, ok := head[p]; return ok }
	sort.Strings(paths)
	for _, p := range paths {
		switch {
		case !seenBase(p):
			out = append(out, ChangedFile{Path: p, Status: "added", BlobSHA: head[p]})
		case !seenHead(p):
			out = append(out, ChangedFile{Path: p, Status: "removed"})
		case base[p] != head[p]:
			out = append(out, ChangedFile{Path: p, Status: "modified", BlobSHA: head[p]})
		}
	}
	return out
}
