package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/target"
	"github.com/magtheo/deploy-toolkit/internal/transport"
)

// StageResult is one lifecycle hook's outcome. Stdout/Stderr are returned
// to the caller for CI-console diagnosis; they are deliberately NOT part
// of anything persisted on the target — raw hook output can contain
// application secrets, and history must not.
//
// A non-zero exit code is a hook failure (the command ran and reported
// failure). A transport failure — unreachable target, StartError,
// cancellation — is NOT a hook outcome and never becomes one; it is
// returned as an infrastructure error by Deploy.
type StageResult struct {
	Name     string
	Skipped  bool
	ExitCode int
	Failed   bool
	Stdout   []byte
	Stderr   []byte
}

// Report describes one deployment attempt. A completed attempt with a
// failed hook or a refused condition is reported, not returned as an
// error: the outcome is a fact, and it is in the history log.
// Infrastructure failures are returned as errors.
type Report struct {
	Project          string
	Environment      string
	Version          string
	LockDir          string
	Staged           target.StageStatus
	BundleDigest     string
	ContractVerified bool
	Stages           []StageResult
	Committed        bool // observed state advanced (verify succeeded)
	AlreadyCurrent   bool // desired release+digest already observed; nothing consequential ran
	HistorySeq       int64
	FailureReason    string
}

// DeployInput carries the desired state (parsed through the manifest
// pipeline by the caller) and the substrate bindings.
//
// The bundle arrives as prepared canonical bytes — the product of the
// PREPARE side, which owns Git and the source checkout. This is the
// prepare/deploy trust split: the deployment operation holds only the
// target credential, never the source repository. Stage verifies the
// bytes against Release.bundle.digest before anything is written.
type DeployInput struct {
	Target         *target.Target
	TargetManifest *manifest.Target // required: cross-checked against the Environment
	Environment    *manifest.Environment
	Release        *manifest.Release
	Bundle         []byte // prepared canonical bundle bytes (prepare side)
	Owner          string // lock owner identity, e.g. "runner:x/y@id"
	Now            func() time.Time
}

// Deploy executes the full sequence. The returned error covers
// infrastructure failures (incomplete input, lock, transport, cancellation);
// deployment outcomes — failed hooks, contract mismatches, drift refusals —
// come back as a Report with FailureReason set and Committed false.
func Deploy(ctx context.Context, in DeployInput) (rep *Report, err error) {
	// Fail closed on incomplete input instead of dereferencing nils.
	switch {
	case in.Target == nil:
		return nil, fmt.Errorf("DeployInput.Target is required")
	case in.TargetManifest == nil:
		return nil, fmt.Errorf("DeployInput.TargetManifest is required")
	case in.Environment == nil:
		return nil, fmt.Errorf("DeployInput.Environment is required")
	case in.Release == nil:
		return nil, fmt.Errorf("DeployInput.Release is required")
	case len(in.Bundle) == 0:
		return nil, fmt.Errorf("DeployInput.Bundle is required (prepared canonical bundle bytes from the prepare side)")
	}
	now := in.Now
	if now == nil {
		now = time.Now
	}
	rel := in.Release
	env := in.Environment
	rep = &Report{
		Project:      rel.Metadata.Project,
		Environment:  env.Metadata.Name,
		Version:      rel.Metadata.Version,
		BundleDigest: rel.Bundle.Digest,
	}
	if err := validateDesired(in); err != nil {
		return rep, err
	}

	layout := in.Target.Layout()
	lockDir, err := layout.LockPath(rep.Project, rep.Environment)
	if err != nil {
		return rep, err
	}
	rep.LockDir = lockDir
	lock, err := AcquireEnvLock(ctx, in.Target.Transport(), lockDir, LockOwner{
		User:        in.Owner,
		Project:     rep.Project,
		Environment: rep.Environment,
		Release:     rep.Version,
	})
	if err != nil {
		return rep, err
	}
	defer func() {
		// Cleanup must survive a cancelled deploy context but must not run
		// unbounded: a fresh, bounded context. Lock-release failure is a
		// hard failure — the environment is blocked until an operator
		// cleans up — so it is joined with any existing error, never
		// swallowed by it.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), lockCleanupTimeout)
		defer cancel()
		if relErr := lock.Release(cleanupCtx); relErr != nil {
			err = errors.Join(err, fmt.Errorf("environment lock %s could not be released (manual cleanup required): %w", lockDir, relErr))
		}
	}()

	failDeployment := func(reason string) (*Report, error) {
		rep.FailureReason = reason
		if herr := recordOutcome(ctx, in, now, rep, "deploy.failed"); herr != nil {
			return rep, errors.Join(fmt.Errorf("%s", reason), fmt.Errorf("history outcome not recorded: %w", herr))
		}
		return rep, nil
	}
	failInfra := func(what string, cause error) (*Report, error) {
		rep.FailureReason = what + " (infrastructure failure)"
		if herr := recordOutcome(ctx, in, now, rep, "deploy.failed"); herr != nil {
			cause = errors.Join(cause, fmt.Errorf("history outcome not recorded: %w", herr))
		}
		return rep, fmt.Errorf("%s: %w", what, cause)
	}

	// Observed state: a fresh target has none — that is normal, not fatal.
	observed, err := in.Target.ReadState(ctx, rep.Project, rep.Environment)
	if err != nil && !errors.Is(err, target.ErrStateAbsent) {
		return rep, fmt.Errorf("read observed state: %w", err)
	}
	if errors.Is(err, target.ErrStateAbsent) {
		observed = target.State{Project: rep.Project, Environment: rep.Environment}
	}

	// Idempotency guard: observed state is a decision input, not a log.
	if observed.Current != nil {
		switch {
		case observed.Current.Release == rep.Version && observed.Current.BundleDigest == rel.Bundle.Digest:
			// Already current: the environment is running exactly the
			// desired release. Re-running migrate/apply is not idempotent
			// in general — and a retry after a lost history write must
			// never repeat consequential work. Record the fact and stop.
			staged, serr := in.Target.Stage(ctx, rel, in.Bundle, now())
			if serr != nil {
				return rep, fmt.Errorf("stage release: %w", serr)
			}
			rep.Staged = staged
			rep.AlreadyCurrent = true
			if herr := recordOutcome(ctx, in, now, rep, "deploy.already-current"); herr != nil {
				return rep, fmt.Errorf("record history outcome: %w", herr)
			}
			return rep, nil
		case observed.Current.Release == rep.Version:
			// Same release identity, different observed digest: the state
			// and the release have drifted apart. Refuse — this is not a
			// deployment condition anyone asked for.
			return failDeployment(fmt.Sprintf(
				"state drift: observed release %s carries bundle digest %s, but the release pins %s — refusing to redeploy over drifted state",
				rep.Version, observed.Current.BundleDigest, rel.Bundle.Digest))
		}
		// A different release is current: proceed with the upgrade.
	}

	// Stage: digest-verified, marker-last, idempotent. Nothing here can
	// affect the running service.
	staged, err := in.Target.Stage(ctx, rel, in.Bundle, now())
	if err != nil {
		return rep, fmt.Errorf("stage release: %w", err)
	}
	rep.Staged = staged

	// The lifecycle contract comes from the STAGED release, byte for byte:
	// read what actually landed on the target, hash it, match it against
	// the release's pinned digest, and parse THAT manifest for hooks.
	releaseDir, err := layout.ReleaseDir(rep.Project, rep.Version)
	if err != nil {
		return rep, err
	}
	contractBytes, err := in.Target.ReadFile(ctx, releaseDir+"/.deploy/project.yaml")
	if err != nil {
		return failInfra("staged deployment contract unreadable", err)
	}
	if digestOf(contractBytes) != rel.DeploymentContract.Digest {
		return failDeployment(fmt.Sprintf("staged deployment contract digest %s does not match the release pin %s", digestOf(contractBytes), rel.DeploymentContract.Digest))
	}
	parsed, err := manifest.Parse(contractBytes, manifest.KindProject)
	if err != nil {
		return failDeployment(fmt.Sprintf("staged deployment contract is not a valid project manifest: %v", err))
	}
	stagedProject := parsed.Project
	if stagedProject.Metadata.Name != rep.Project {
		return failDeployment(fmt.Sprintf("staged deployment contract is for project %q, deploying %q", stagedProject.Metadata.Name, rep.Project))
	}
	rep.ContractVerified = true
	if stagedProject.Lifecycle.Verify == nil {
		// Defense in depth: the schema already requires verify. State
		// must never advance on a skipped verification.
		return failDeployment("staged deployment contract declares no verify hook; verify is mandatory")
	}

	henv, err := hookEnv(rel, rep.Environment)
	if err != nil {
		return rep, fmt.Errorf("hook environment: %w", err)
	}

	// runStage preserves the transport contract: exit codes are hook
	// outcomes, Run errors are infrastructure errors.
	runStage := func(name string, step *manifest.LifecycleStep) (StageResult, error) {
		if step == nil {
			return StageResult{Name: name, Skipped: true}, nil
		}
		res, err := in.Target.Transport().Run(ctx, transport.RunRequest{
			Argv: step.Argv,
			Dir:  releaseDir,
			Env:  henv,
		})
		if err != nil {
			return StageResult{Name: name, Failed: true}, err
		}
		return StageResult{
			Name:     name,
			ExitCode: res.ExitCode,
			Failed:   res.ExitCode != 0,
			Stdout:   res.Stdout,
			Stderr:   res.Stderr,
		}, nil
	}

	// Staging is non-impacting; preflight decides whether the staged
	// release may begin consequential execution.
	sr, ierr := runStage("preflight", stagedProject.Lifecycle.Preflight)
	rep.Stages = append(rep.Stages, sr)
	if ierr != nil {
		return failInfra("preflight could not be executed", ierr)
	}
	if sr.Failed {
		return failDeployment("preflight failed")
	}

	// Migration hooks follow the release's declared migration semantics:
	// mode none means no migration runs even if a hook is declared.
	var migrateStep *manifest.LifecycleStep
	if rel.Migration.Mode != manifest.MigrationNone {
		migrateStep = stagedProject.Lifecycle.Migrate
	}
	sr, ierr = runStage("migrate", migrateStep)
	rep.Stages = append(rep.Stages, sr)
	if ierr != nil {
		return failInfra("migrate could not be executed", ierr)
	}
	if sr.Failed {
		return failDeployment("migrate failed")
	}

	sr, ierr = runStage("apply", stagedProject.Lifecycle.Apply)
	rep.Stages = append(rep.Stages, sr)
	if ierr != nil {
		return failInfra("apply could not be executed", ierr)
	}
	if sr.Failed {
		return failDeployment("apply failed")
	}

	sr, ierr = runStage("verify", stagedProject.Lifecycle.Verify)
	rep.Stages = append(rep.Stages, sr)
	if ierr != nil {
		return failInfra("verify could not be executed", ierr)
	}
	if sr.Failed {
		return failDeployment("verify failed")
	}

	// Verify succeeded — and only verify success — lets observed state
	// advance. staged ≠ deployed; apply success ≠ verified.
	observed.Current = &target.CurrentDeployment{
		Release:      rep.Version,
		BundleDigest: rel.Bundle.Digest,
		Since:        now().UTC().Format(time.RFC3339),
	}
	observed.UpdatedAt = now().UTC().Format(time.RFC3339)
	if err := in.Target.WriteState(ctx, observed); err != nil {
		return rep, fmt.Errorf("commit observed state: %w", err)
	}
	rep.Committed = true

	if err := recordOutcome(ctx, in, now, rep, "deploy.succeeded"); err != nil {
		return rep, fmt.Errorf("record history outcome: %w", err)
	}
	return rep, nil
}

func recordOutcome(ctx context.Context, in DeployInput, now func() time.Time, rep *Report, kind string) error {
	data := outcomeData(rep, in)
	if kind == "deploy.succeeded" {
		// The state commit is part of the success fact.
		data["committed"] = true
	}
	seq, err := in.Target.AppendHistory(ctx, rep.Project, rep.Environment, target.Entry{
		Time: now().UTC().Format(time.RFC3339),
		Type: kind,
		Data: data,
	})
	if err != nil {
		return err
	}
	rep.HistorySeq = seq
	return nil
}

// validateDesired cross-checks the desired manifests against each other
// before anything touches the target: the Environment must point at this
// Target and at exactly this Release.
func validateDesired(in DeployInput) error {
	env := in.Environment
	rel := in.Release
	if env.Spec.Target == "" {
		return fmt.Errorf("environment %q declares no target", env.Metadata.Name)
	}
	if env.Spec.Target != in.TargetManifest.Metadata.Name {
		return fmt.Errorf("environment %q targets %q, but the deployment targets %q", env.Metadata.Name, env.Spec.Target, in.TargetManifest.Metadata.Name)
	}
	wantRef := fmt.Sprintf(".deploy/releases/%s-%s.yaml", rel.Metadata.Project, rel.Metadata.Version)
	if env.Spec.Release != wantRef {
		return fmt.Errorf("environment %q pins release %q, but the deployment is for %q", env.Metadata.Name, env.Spec.Release, wantRef)
	}
	if err := target.CheckVersion(rel.Metadata.Version); err != nil {
		return fmt.Errorf("release version: %w", err)
	}
	return nil
}

// outcomeData is the structured, secret-free history payload: identities,
// digests, per-stage exit codes, timestamps. Raw hook output never enters.
func outcomeData(rep *Report, in DeployInput) map[string]any {
	stages := make(map[string]any, len(rep.Stages))
	for _, s := range rep.Stages {
		if s.Skipped {
			stages[s.Name] = "skipped"
			continue
		}
		entry := map[string]any{"exit": s.ExitCode}
		if s.Failed {
			entry["failed"] = true
		}
		stages[s.Name] = entry
	}
	data := map[string]any{
		"project":          rep.Project,
		"environment":      rep.Environment,
		"release":          rep.Version,
		"bundleDigest":     rep.BundleDigest,
		"target":           in.TargetManifest.Metadata.Name,
		"staged":           rep.Staged.String(),
		"contractVerified": rep.ContractVerified,
		"stages":           stages,
		"actor":            in.Owner,
	}
	if rep.AlreadyCurrent {
		data["alreadyCurrent"] = true
	}
	if rep.Committed {
		data["committed"] = true
	}
	if rep.FailureReason != "" {
		data["reason"] = rep.FailureReason
	}
	return data
}
