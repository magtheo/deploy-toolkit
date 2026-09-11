package lifecycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/target"
	"github.com/magtheo/deploy-toolkit/internal/transport"
)

// hookDefaultPATH is the complete PATH hooks receive. The hook environment
// is deterministic by construction: exactly the variables below, never the
// runner's or login user's ambient environment. Hooks that need more must
// declare it themselves or read configuration from the release.
const hookDefaultPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// hookEnv builds the exact environment a lifecycle hook runs with:
//
//	PATH                        fixed default (see hookDefaultPATH)
//	DEPLOY_PROJECT              release's project
//	DEPLOY_ENVIRONMENT          environment name
//	DEPLOY_RELEASE_VERSION      release version
//	DEPLOY_SOURCE_REVISION      pinned source revision (full SHA)
//	DEPLOY_ARTIFACT_<NAME>      <image>@<digest> per declared artifact
//
// The artifact mapping is injective: artifact names are [a-z0-9-] slugs,
// and uppercasing with '-'→'_' cannot collide. Hooks are pinned to the
// exact Release artifacts without copying the Release manifest into the
// bundle.
func hookEnv(rel *manifest.Release, environment string) (map[string]string, error) {
	env := map[string]string{
		"PATH":                   hookDefaultPATH,
		"DEPLOY_PROJECT":         rel.Metadata.Project,
		"DEPLOY_ENVIRONMENT":     environment,
		"DEPLOY_RELEASE_VERSION": rel.Metadata.Version,
		"DEPLOY_SOURCE_REVISION": rel.Source.Revision,
	}
	for name, a := range rel.Artifacts {
		key, err := artifactEnvName(name)
		if err != nil {
			return nil, err
		}
		env[key] = a.Image + "@" + a.Digest
	}
	return env, nil
}

func artifactEnvName(name string) (string, error) {
	// Names arrive schema-validated, but this path derives environment
	// variable syntax from them — validate locally too.
	if name == "" {
		return "", fmt.Errorf("artifact name must not be empty")
	}
	var b strings.Builder
	b.WriteString("DEPLOY_ARTIFACT_")
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r - ('a' - 'A'))
		case r == '-':
			b.WriteByte('_')
		default:
			return "", fmt.Errorf("artifact name %q: only [a-z0-9-] map to hook environment variables", name)
		}
	}
	return b.String(), nil
}

func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// runStageStep executes one lifecycle hook and preserves the transport
// contract shared by Deploy and Rollback: a non-zero exit code is a hook
// outcome (reported, nil error); a Transport.Run error is an
// infrastructure error (the command may or may not have executed, and no
// exit code exists — never manufacture one).
func runStageStep(ctx context.Context, tr transport.Transport, name, dir string, henv map[string]string, step *manifest.LifecycleStep) (StageResult, error) {
	if step == nil {
		return StageResult{Name: name, Skipped: true}, nil
	}
	res, err := tr.Run(ctx, transport.RunRequest{Argv: step.Argv, Dir: dir, Env: henv})
	if err != nil {
		// Fate decides safety, and it is VALIDATED, not trusted: only
		// the two fates that prove "nothing can be running" may skip
		// retention (RunNotStarted: definite dispatch failure;
		// RunExited with a StartError: the SSH 126/127 convention).
		// RunUnknown — the zero value — and any INVALID fate value
		// retain the lock, so a transport that forgets to set Fate, or
		// invents a new value this version does not know, fails closed.
		unknown := res.Fate != transport.RunNotStarted && res.Fate != transport.RunExited
		return StageResult{Name: name, Failed: true, InfraError: true, Unknown: unknown}, err
	}
	// A nil error is only defined for a determined exit. Any other fate
	// with nil error is a transport-contract violation and fails closed:
	// reported as an infrastructure error with unknown fate (lock
	// retained), never as a successful or determined hook.
	if res.Fate != transport.RunExited {
		return StageResult{Name: name, Failed: true, InfraError: true, Unknown: true},
			fmt.Errorf("hook %s: transport contract violation: Run returned fate %d with nil error; treated as unknown fate (lock retained)", name, int(res.Fate))
	}
	return StageResult{
		Name:     name,
		ExitCode: res.ExitCode,
		Failed:   res.ExitCode != 0,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
	}, nil
}

// evidenceFinalizationTimeout bounds history (evidence) writes so a
// stuck target cannot hang finalization forever. Var for test
// overriding.
var evidenceFinalizationTimeout = 30 * time.Second

// evidenceCtx is the context for DURABLE EVIDENCE WRITES ONLY: history
// outcome records. It is deliberately independent of the caller's
// context — a cancelled deadline must not cost the operator the
// historical record of what happened — and bounded so a stuck target
// cannot hang finalization forever.
//
// The boundary is strict. Evidence finalization may persist facts the
// lifecycle already established; it must never CREATE permission:
//
//   - no lifecycle hook is executed on this context (it only carries
//     file reads/writes of the history log);
//   - observed-state commits, marker writes and marker clears stay on
//     the caller's context — an uncertain outcome stays uncertain, a
//     retained lock stays retained;
//   - a failed evidence write is still a joined error, never swallowed.
func evidenceCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), evidenceFinalizationTimeout)
}

// errContractRefused marks a staged-contract violation: an outcome of the
// operation (recorded, no hooks run), not an infrastructure failure.
var errContractRefused = errors.New("staged deployment contract refused")

// verifyStagedContract proves that the staged release directory still
// carries exactly the release's pinned deployment contract — read from the
// TARGET, hashed, matched against Release.deploymentContract.digest, then
// parsed through the one manifest pipeline — and returns the parsed
// contract. The lifecycle that runs is the one staged at this revision,
// never main's current state. Unreadable contract bytes are an
// infrastructure failure; any mismatch or invalid manifest is
// errContractRefused.
func verifyStagedContract(ctx context.Context, tgt *target.Target, rel *manifest.Release) (*manifest.Project, error) {
	releaseDir, err := tgt.Layout().ReleaseDir(rel.Metadata.Project, rel.Metadata.Version)
	if err != nil {
		return nil, err
	}
	contractBytes, err := tgt.ReadFile(ctx, releaseDir+"/.deploy/project.yaml")
	if err != nil {
		return nil, fmt.Errorf("staged deployment contract unreadable: %w", err)
	}
	if digestOf(contractBytes) != rel.DeploymentContract.Digest {
		return nil, fmt.Errorf("%w: digest %s does not match the release pin %s", errContractRefused, digestOf(contractBytes), rel.DeploymentContract.Digest)
	}
	parsed, err := manifest.Parse(contractBytes, manifest.KindProject)
	if err != nil {
		return nil, fmt.Errorf("%w: not a valid project manifest: %v", errContractRefused, err)
	}
	if parsed.Project.Metadata.Name != rel.Metadata.Project {
		return nil, fmt.Errorf("%w: contract is for project %q, operating on %q", errContractRefused, parsed.Project.Metadata.Name, rel.Metadata.Project)
	}
	return parsed.Project, nil
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("sha256:%064x", sum)
}

func firstLine(b []byte) string {
	for i, c := range b {
		if c == '\n' {
			return string(b[:i])
		}
	}
	return string(b)
}
