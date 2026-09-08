package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/bundle"
	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/release"
	"github.com/magtheo/deploy-toolkit/internal/target"
	"github.com/magtheo/deploy-toolkit/internal/transport"
)

// Bundler builds the canonical bundle for a release revision, deriving the
// include list from the revision's own project manifest — the deploy-time
// half of "the deployment contract comes from the promoted release".
// *bundle.Builder implements it.
type Bundler interface {
	BuildFromRevision(ctx context.Context, revision string) (bundle.Result, error)
}

var _ Bundler = (*bundle.Builder)(nil)

// StageResult is one lifecycle hook's outcome. Stdout/Stderr are returned
// to the caller for CI-console diagnosis; they are deliberately NOT part
// of anything persisted on the target — raw hook output can contain
// application secrets, and history must not.
type StageResult struct {
	Name     string
	Skipped  bool
	ExitCode int
	Failed   bool
	Stdout   []byte
	Stderr   []byte
}

// Report describes one deployment attempt. A completed attempt with a
// failed hook is reported, not returned as an error: the outcome is a
// fact, and it is in the history log.
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
	HistorySeq       int64
	FailureReason    string
}

// DeployInput carries the desired state (parsed through the manifest
// pipeline by the caller) and the substrate bindings.
type DeployInput struct {
	Target         *target.Target
	TargetManifest *manifest.Target      // for cross-checking the Environment's target reference
	Environment    *manifest.Environment // desired, from promoted main
	Release        *manifest.Release     // desired, from promoted main
	Bundler        Bundler
	Owner          string // lock owner identity, e.g. "runner:x/y@id"
	Now            func() time.Time
}

// Deploy executes the full sequence. The returned error covers
// infrastructure failures (lock, transport, bundle build); deployment
// outcomes — failed hooks, contract mismatches — come back as a Report
// with FailureReason set and Committed false.
func Deploy(ctx context.Context, in DeployInput) (rep *Report, err error) {
	now := in.Now
	if now == nil {
		now = time.Now
	}
	rel := in.Release
	env := in.Environment
	rep = &Report{
		Project:     rel.Metadata.Project,
		Environment: env.Metadata.Name,
		Version:     rel.Metadata.Version,
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
		// Release with a live context even when the deploy was cancelled:
		// a stuck lock blocks the environment far worse than a rmdir on a
		// cancelled context does.
		if relErr := lock.Release(context.WithoutCancel(ctx)); relErr != nil && err == nil {
			err = fmt.Errorf("deployment of %s %s succeeded, but the environment lock could not be released (manual cleanup of %s required): %w", rep.Project, rep.Version, lockDir, relErr)
		}
	}()

	fail := func(reason string) (*Report, error) {
		rep.FailureReason = reason
		_, herr := in.Target.AppendHistory(ctx, rep.Project, rep.Environment, target.Entry{
			Time: now().UTC().Format(time.RFC3339),
			Type: "deploy.failed",
			Data: outcomeData(rep, in),
		})
		if herr != nil {
			return rep, fmt.Errorf("%s (history outcome not recorded: %w)", reason, herr)
		}
		return rep, nil
	}

	// Observed state: a fresh target has none — that is normal, not fatal.
	observed, err := in.Target.ReadState(ctx, rep.Project, rep.Environment)
	if err != nil && !errors.Is(err, target.ErrStateAbsent) {
		return rep, fmt.Errorf("read observed state: %w", err)
	}
	if errors.Is(err, target.ErrStateAbsent) {
		observed = target.State{Project: rep.Project, Environment: rep.Environment}
	}

	// Stage: digest-verified, marker-last, idempotent. Nothing here can
	// affect the running service.
	bres, err := in.Bundler.BuildFromRevision(ctx, rel.Source.Revision)
	if err != nil {
		return rep, fmt.Errorf("build bundle for %s: %w", rel.Source.Revision, err)
	}
	rep.BundleDigest = bres.Digest
	staged, err := in.Target.Stage(ctx, rel, bres.Bytes, now())
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
		return fail(fmt.Sprintf("staged deployment contract unreadable: %v", err))
	}
	if digestOf(contractBytes) != rel.DeploymentContract.Digest {
		return fail(fmt.Sprintf("staged deployment contract digest %s does not match the release pin %s", digestOf(contractBytes), rel.DeploymentContract.Digest))
	}
	parsed, err := manifest.Parse(contractBytes, manifest.KindProject)
	if err != nil {
		return fail(fmt.Sprintf("staged deployment contract is not a valid project manifest: %v", err))
	}
	stagedProject := parsed.Project
	if stagedProject.Metadata.Name != rep.Project {
		return fail(fmt.Sprintf("staged deployment contract is for project %q, deploying %q", stagedProject.Metadata.Name, rep.Project))
	}
	rep.ContractVerified = true

	henv, err := hookEnv(rel, rep.Environment)
	if err != nil {
		return rep, fmt.Errorf("hook environment: %w", err)
	}

	if stagedProject.Lifecycle.Apply == nil {
		// apply is the only mandatory hook (Consumer Contract v1).
		return fail("staged deployment contract declares no apply hook; apply is mandatory")
	}

	runStage := func(name string, step *manifest.LifecycleStep) bool {
		if step == nil {
			rep.Stages = append(rep.Stages, StageResult{Name: name, Skipped: true})
			return true
		}
		res, err := in.Target.Transport().Run(ctx, transport.RunRequest{
			Argv: step.Argv,
			Dir:  releaseDir,
			Env:  henv,
		})
		sr := StageResult{Name: name}
		if res.Stdout != nil {
			sr.Stdout = res.Stdout
		}
		if res.Stderr != nil {
			sr.Stderr = res.Stderr
		}
		if err != nil {
			sr.Failed = true
			sr.Stderr = append(sr.Stderr, []byte("\n"+err.Error())...)
			rep.Stages = append(rep.Stages, sr)
			return false
		}
		sr.ExitCode = res.ExitCode
		sr.Failed = res.ExitCode != 0
		rep.Stages = append(rep.Stages, sr)
		return !sr.Failed
	}

	// Staging is non-impacting; preflight decides whether the staged
	// release may begin consequential execution.
	if !runStage("preflight", stagedProject.Lifecycle.Preflight) {
		return fail("preflight failed")
	}

	// Migration hooks follow the release's declared migration semantics:
	// mode none means no migration runs even if a hook is declared.
	var migrateStep *manifest.LifecycleStep
	if rel.Migration.Mode != manifest.MigrationNone {
		migrateStep = stagedProject.Lifecycle.Migrate
	}
	if !runStage("migrate", migrateStep) {
		return fail("migrate failed")
	}
	if !runStage("apply", stagedProject.Lifecycle.Apply) {
		return fail("apply failed")
	}
	if !runStage("verify", stagedProject.Lifecycle.Verify) {
		return fail("verify failed")
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

	seq, err := in.Target.AppendHistory(ctx, rep.Project, rep.Environment, target.Entry{
		Time: now().UTC().Format(time.RFC3339),
		Type: "deploy.succeeded",
		Data: outcomeData(rep, in),
	})
	if err != nil {
		return rep, fmt.Errorf("record history outcome: %w", err)
	}
	rep.HistorySeq = seq
	return rep, nil
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
	if in.TargetManifest != nil && env.Spec.Target != in.TargetManifest.Metadata.Name {
		return fmt.Errorf("environment %q targets %q, but the deployment targets %q", env.Metadata.Name, env.Spec.Target, in.TargetManifest.Metadata.Name)
	}
	wantRef := ".deploy/releases/" + release.ReleaseFileName(rel.Metadata.Project, rel.Metadata.Version)
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
	if rep.Committed {
		data["committed"] = true
	}
	if rep.FailureReason != "" {
		data["reason"] = rep.FailureReason
	}
	return data
}
