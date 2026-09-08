package target

import (
	"fmt"
	"regexp"

	"github.com/magtheo/deploy-toolkit/internal/transport"
)

// slugPattern and versionPattern mirror the JSON Schema patterns in
// schemas/*.schema.json (metadata.name, metadata.project, metadata.version).
// They are re-applied here because paths are derived from these identities:
// manifest validation never travels with manifest bytes. Keep in lockstep.
var (
	slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	// strict SemVer (semver.org), identical to release.schema.json.
	versionPattern = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)
)

// Layout derives every path the toolkit owns on a target:
//
//	<deployRoot>/<project>/releases/<version>/   staged release trees
//	<deployRoot>/<project>/state/<env>.json      observed state snapshot
//	<deployRoot>/<project>/history/<env>.jsonl   durable history log
//
// Paths are a pure function of validated identities — project, environment
// and version — never of time, sequence numbers or configuration state.
type Layout struct {
	root string
}

func NewLayout(deployRoot string) (Layout, error) {
	cleaned, err := transport.ValidateAbsolutePath(deployRoot)
	if err != nil {
		return Layout{}, fmt.Errorf("deployRoot: %w", err)
	}
	return Layout{root: cleaned}, nil
}

func (l Layout) Root() string { return l.root }

func checkSlug(kind, s string) error {
	if !slugPattern.MatchString(s) {
		return fmt.Errorf("%s %q must match %s", kind, s, slugPattern)
	}
	return nil
}

func CheckVersion(v string) error {
	if !versionPattern.MatchString(v) {
		return fmt.Errorf("version %q is not strict SemVer", v)
	}
	return nil
}

func (l Layout) ReleaseDir(project, version string) (string, error) {
	if err := checkSlug("project", project); err != nil {
		return "", err
	}
	if err := CheckVersion(version); err != nil {
		return "", err
	}
	return l.root + "/" + project + "/releases/" + version, nil
}

func (l Layout) StatePath(project, env string) (string, error) {
	if err := checkSlug("project", project); err != nil {
		return "", err
	}
	if err := checkSlug("environment", env); err != nil {
		return "", err
	}
	return l.root + "/" + project + "/state/" + env + ".json", nil
}

// AttemptPath is the durable unresolved-attempt marker: evidence that the
// previous deployment may have performed consequential work with an
// unresolved outcome. Presence requires explicit recovery before any new
// deployment of the environment.
func (l Layout) AttemptPath(project, env string) (string, error) {
	if err := checkSlug("project", project); err != nil {
		return "", err
	}
	if err := checkSlug("environment", env); err != nil {
		return "", err
	}
	return l.root + "/" + project + "/attempts/" + env + ".json", nil
}

// LockPath is the environment lock directory: acquired atomically with
// mkdir (which wins exactly once on POSIX), released with rmdir. Locks
// have no staleness semantics — a crashed runner leaves the lock for
// explicit operator removal.
func (l Layout) LockPath(project, env string) (string, error) {
	if err := checkSlug("project", project); err != nil {
		return "", err
	}
	if err := checkSlug("environment", env); err != nil {
		return "", err
	}
	return l.root + "/" + project + "/.locks/" + env, nil
}

func (l Layout) HistoryPath(project, env string) (string, error) {
	if err := checkSlug("project", project); err != nil {
		return "", err
	}
	if err := checkSlug("environment", env); err != nil {
		return "", err
	}
	return l.root + "/" + project + "/history/" + env + ".jsonl", nil
}
