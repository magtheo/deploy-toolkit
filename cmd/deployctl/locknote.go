package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/magtheo/deploy-toolkit/internal/lifecycle"
)

// warnLockReleaseFailed prints the lock-release warning when the
// returned error carries the sentinel. Human rendering calls it at the
// TOP of every error path, before classification-specific guidance:
// whatever else the operation wants to tell the operator, a lock that
// could not be released comes first — it blocks the environment until
// manual cleanup.
func warnLockReleaseFailed(w io.Writer, err error) {
	if err == nil || !errors.Is(err, lifecycle.ErrLockReleaseFailed) {
		return
	}
	fmt.Fprintln(w, "  ⚠ THE ENVIRONMENT LOCK COULD NOT BE RELEASED — manual cleanup is required.")
	fmt.Fprintln(w, "  ⚠ The environment stays locked; remove the lock after verifying the target.")
}
