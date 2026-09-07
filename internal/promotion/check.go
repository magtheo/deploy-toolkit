package promotion

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
)

var releaseFilePattern = regexp.MustCompile(`^\.deploy/releases/([a-z0-9][a-z0-9-]{0,62})-([0-9A-Za-z.\-+]+)\.yaml$`)

type CheckInput struct {
	Repo string
	Base string
	Head string
}

type CheckResult2 struct {
	Passed   bool
	Messages []string
}

func Check(ctx context.Context, in CheckInput, src Store) (*CheckResult2, error) {
	res := &CheckResult2{Passed: true}
	fail := func(format string, args ...any) {
		res.Passed = false
		res.Messages = append(res.Messages, fmt.Sprintf(format, args...))
	}
	files, err := src.CompareFiles(ctx, in.Repo, in.Base, in.Head)
	if err != nil {
		return nil, err
	}

	var (
		addedRelease string
		envChanged   []string
		other        []string
	)
	for _, f := range files {
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
			content, err := src.BlobAt(ctx, in.Repo, blobFor(files, addedRelease))
			if err != nil {
				return nil, err
			}
			res2, err := manifest.Parse(content, manifest.KindRelease)
			if err != nil {
				fail("added release file %s is not a valid Release: %v", addedRelease, err)
			} else {
				r := res2.Release
				if r.Metadata.Project != m[1] {
					fail("release file %s declares project %q; filename must agree", addedRelease, r.Metadata.Project)
				}
				if r.Metadata.Version != m[2] {
					fail("release file %s declares version %q; filename must agree", addedRelease, r.Metadata.Version)
				}
			}
		}
	}

	if envPath != "" {
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
		envName := strings.TrimSuffix(path.Base(envPath), ".yaml")
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
		if addedRelease != "" && he.Spec.Release != addedRelease {
			fail("spec.release points at %q but the PR adds %q", he.Spec.Release, addedRelease)
		}
		if addedRelease == "" {
			if _, err := src.FileAt(ctx, in.Repo, he.Spec.Release, in.Base); err != nil {
				fail("spec.release points at %q which does not exist in base", he.Spec.Release)
			}
		}
	}

	if res.Passed {
		res.Messages = append(res.Messages, "promotion diff policy satisfied")
	}
	return res, nil
}

func blobFor(files []ChangedFile, path string) string {
	for _, f := range files {
		if f.Path == path {
			return f.BlobSHA
		}
	}
	return ""
}
