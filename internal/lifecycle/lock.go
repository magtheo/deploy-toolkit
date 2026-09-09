// Package lifecycle is the deployment state machine: it consumes the
// target substrate and turns a promoted, human-authorized release into the
// observed state of an environment.
//
// Deploy executes the forward sequence:
//
//	ACQUIRE cross-process environment lock
//	  → VALIDATE desired Environment + Release
//	  → READ observed state
//	  → STAGE release (digest-verified, marker-last, idempotent)
//	  → VERIFY staged deploymentContract.digest
//	  → PREFLIGHT → MIGRATE → APPLY → VERIFY (hooks from the staged contract)
//	  → WRITE observed state.current (only after verify succeeds)
//	  → APPEND structured final history outcome
//	  → RELEASE lock
//
// Rollback is the distinct recovery operation: it does not reuse Deploy
// (after a failed A→B, observed state may still say A while production
// partly runs B — Deploy's idempotency guard would never restore A). It
// holds the same lock, binds both releases to the recorded attempt marker
// and observed state, runs the failed release's rollback hook plus the
// restored release's preflight/apply/verify, and clears the marker only
// after the restored release verifies and observed state commits.
//
// Invariants carried from earlier phases: staged ≠ deployed, apply success
// ≠ verified, verify success is what allows observed state to advance. The
// lifecycle contract itself comes from the staged release — never from
// main's current state.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/transport"
)

// EnvLock is a target-scoped, cross-process, per-environment lock. POSIX
// mkdir is the primitive: it wins exactly once, and an existing lock fails
// closed. There are deliberately NO stale-lock semantics — a crashed
// runner leaves the lock behind, and an operator removes it manually after
// verifying no deployment is in flight. Guessing that another deployment
// is dead is how two deployments run at once.
type EnvLock struct {
	tr  transport.Transport
	dir string
}

type LockOwner struct {
	Host        string `json:"host"`
	PID         int    `json:"pid"`
	User        string `json:"user"`
	StartedAt   string `json:"startedAt"` // RFC3339, UTC
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Release     string `json:"release"`
}

// lockCleanupTimeout bounds lock-release attempts so a stuck target cannot
// hang finalization forever. Var for test overriding.
var lockCleanupTimeout = 30 * time.Second

// ErrEnvLockHeld marks the one lock-acquisition failure that is a REFUSAL
// rather than an infrastructure problem: the lock directory already
// exists, so an operation may be executing right now. Every other
// acquisition failure (transport death, filesystem trouble) is
// infrastructure.
var ErrEnvLockHeld = errors.New("environment lock is held")

// AcquireEnvLock creates the lock directory atomically. Callers must
// Release it; Release failure is a hard error because a stuck lock blocks
// every future deployment of the environment.
func AcquireEnvLock(ctx context.Context, tr transport.Transport, lockDir string, owner LockOwner) (*EnvLock, error) {
	run := func(argv ...string) (int, string, error) {
		res, err := tr.Run(ctx, transport.RunRequest{Argv: argv, Dir: "/"})
		if err != nil {
			return 0, "", err
		}
		return res.ExitCode, firstLine(res.Stderr), nil
	}
	// Idempotent parent setup, then the atomic claim itself. mkdir without
	// -p fails when the target exists — that failure is the lock.
	code, stderr, runErr := run("mkdir", "-p", parentDir(lockDir))
	if runErr != nil || code != 0 {
		if runErr != nil {
			return nil, fmt.Errorf("lock: create parent of %s: %w", lockDir, runErr)
		}
		return nil, fmt.Errorf("lock: create parent of %s: exit %d: %s", lockDir, code, stderr)
	}
	code, stderr, acquireErr := run("mkdir", lockDir)
	if acquireErr != nil {
		return nil, fmt.Errorf("lock: acquire %s: %w", lockDir, acquireErr)
	}
	if code != 0 {
		held, _, heldErr := run("test", "-d", lockDir)
		if heldErr == nil && held == 0 {
			return nil, fmt.Errorf("environment lock %s is held by another deployment (a crashed runner leaves the lock; remove it manually after verifying no deployment is in flight): %w", lockDir, ErrEnvLockHeld)
		}
		return nil, fmt.Errorf("lock: acquire %s: exit %d: %s", lockDir, code, stderr)
	}
	lk := &EnvLock{tr: tr, dir: lockDir}
	if err := lk.writeOwner(ctx, owner); err != nil {
		// Cleanup must not hang and must not be silently ignored: a failed
		// cleanup means an orphan lock that blocks the environment.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), lockCleanupTimeout)
		defer cancel()
		if cleanupErr := lk.Release(cleanupCtx); cleanupErr != nil {
			return nil, fmt.Errorf("lock: record owner in %s: %w (cleanup also failed: %v — an orphan lock may remain and requires manual removal)", lockDir, err, cleanupErr)
		}
		return nil, fmt.Errorf("lock: record owner in %s: %w", lockDir, err)
	}
	return lk, nil
}

func (l *EnvLock) writeOwner(ctx context.Context, owner LockOwner) error {
	if owner.StartedAt == "" {
		owner.StartedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if owner.Host == "" {
		owner.Host, _ = os.Hostname()
	}
	if owner.PID == 0 {
		owner.PID = os.Getpid()
	}
	if owner.User == "" {
		owner.User = os.Getenv("USER")
	}
	raw, err := marshalJSON(owner)
	if err != nil {
		return err
	}
	return l.tr.Put(ctx, transport.PutRequest{Path: l.dir + "/owner.json", Content: raw, Mode: 0o644})
}

// Release removes the lock. The owner file goes first, then the directory
// itself (rmdir only removes empty directories — if something unexpected
// is in there, the lock survives and the error says so).
func (l *EnvLock) Release(ctx context.Context) error {
	res, err := l.tr.Run(ctx, transport.RunRequest{Argv: []string{"rm", "-f", l.dir + "/owner.json"}, Dir: "/"})
	if err != nil {
		return fmt.Errorf("lock release %s: %w", l.dir, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("lock release %s: rm owner.json exit %d: %s", l.dir, res.ExitCode, firstLine(res.Stderr))
	}
	res, err = l.tr.Run(ctx, transport.RunRequest{Argv: []string{"rmdir", l.dir}, Dir: "/"})
	if err != nil {
		return fmt.Errorf("lock release %s: %w", l.dir, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("lock release %s: rmdir exit %d: %s (manual cleanup required)", l.dir, res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

func parentDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return p
}
