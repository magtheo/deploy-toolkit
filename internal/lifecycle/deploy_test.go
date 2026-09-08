package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/magtheo/deploy-toolkit/internal/bundle"
	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/target"
	"github.com/magtheo/deploy-toolkit/internal/transport/local"
)

// fixture builds a git repo whose project manifest declares lifecycle
// hooks that record their execution order into a marker file and dump the
// hook environment. Everything downstream (bundle, staging, contract
// verification, hook execution) is the real machinery.
type fixture struct {
	repoDir  string
	revision string
	bundler  *bundle.Builder
	marker   string // execution-order marker
	envFile  string // DEPLOY_* dump
	target   *target.Target
	root     string
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String())
}

func newFixture(t *testing.T, project string, hooks func(marker, envFile, script string) string) *fixture {
	t.Helper()
	repoDir := t.TempDir()
	git(t, repoDir, "init", "-q", "-b", "main")
	git(t, repoDir, "config", "user.email", "test@example.com")
	git(t, repoDir, "config", "user.name", "Test")

	marker := filepath.Join(t.TempDir(), "order.txt")
	envFile := filepath.Join(t.TempDir(), "env.txt")

	hookBody := func(name string) string {
		script := filepath.Join(t.TempDir(), name+".body")
		content := fmt.Sprintf("#!/bin/sh\necho %s >> %s\n", name, marker)
		if hooks != nil {
			content = hooks(marker, envFile, name)
		}
		if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	mustWrite := func(rel, content string) {
		p := filepath.Join(repoDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("deploy/preflight.sh", hookBody("preflight"))
	mustWrite("deploy/migrate.sh", hookBody("migrate"))
	mustWrite("deploy/apply.sh", hookBody("apply"))
	mustWrite("deploy/verify.sh", hookBody("verify"))
	mustWrite(".deploy/project.yaml", fmt.Sprintf(`apiVersion: deploy.toolkit/v1
kind: Project
metadata:
  name: %s
release:
  source:
    type: github
    repository: example/%s
    branch: main
  requiredChecks:
    - ci/build
artifacts:
  app:
    type: oci
    repository: ghcr.io/example/app
bundle:
  include:
    - "deploy/**"
lifecycle:
  preflight:
    argv: ["./deploy/preflight.sh"]
  migrate:
    argv: ["./deploy/migrate.sh"]
  apply:
    argv: ["./deploy/apply.sh"]
  verify:
    argv: ["./deploy/verify.sh"]
`, project, project))
	git(t, repoDir, "add", "-A")
	git(t, repoDir, "commit", "-q", "-m", "fixture")
	rev := git(t, repoDir, "rev-parse", "HEAD")

	root := t.TempDir()
	tgt, err := target.New(local.New(), root)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{
		repoDir:  repoDir,
		revision: rev,
		bundler:  bundle.NewBuilder(repoDir),
		marker:   marker,
		envFile:  envFile,
		target:   tgt,
		root:     root,
	}
}

func (f *fixture) release(project, version string) *manifest.Release {
	res, err := f.bundler.BuildFromRevision(context.Background(), f.revision)
	if err != nil {
		return nil
	}
	return &manifest.Release{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.KindRelease,
		Metadata:   manifest.ReleaseMetadata{Project: project, Version: version},
		Source:     manifest.ReleaseSource{Type: manifest.SourceGitHub, Repository: "example/" + project, Revision: f.revision},
		Artifacts: map[string]manifest.BuiltArtifact{
			"app": {Type: "oci", Image: "ghcr.io/example/app", Digest: "sha256:" + strings.Repeat("aa", 32)},
		},
		Bundle:             manifest.BundleDigest{Digest: res.Digest},
		DeploymentContract: manifest.ContractDigest{Digest: res.ContractDigest},
		Migration:          manifest.MigrationSpec{Mode: manifest.MigrationForwardCompatible, Head: "001_init", RollbackSafe: true},
	}
}

func (f *fixture) environment(project, version string) *manifest.Environment {
	return &manifest.Environment{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.KindEnvironment,
		Metadata:   manifest.EnvironmentMetadata{Name: "production"},
		Spec: manifest.EnvironmentSpec{
			Release: ".deploy/releases/" + project + "-" + version + ".yaml",
			Target:  "local-dev",
		},
	}
}

func (f *fixture) targetManifest() *manifest.Target {
	return &manifest.Target{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.KindTarget,
		Metadata:   manifest.TargetMetadata{Name: "local-dev"},
		Spec:       manifest.TargetSpec{DeployRoot: f.root},
	}
}

func (f *fixture) lockDir() string {
	return f.root + "/" + "my-app" + "/.locks/production"
}

func order(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

func requireNoLock(t *testing.T, f *fixture) {
	t.Helper()
	if _, err := os.Stat(f.lockDir()); !os.IsNotExist(err) {
		t.Errorf("environment lock survived the deployment (stat err = %v)", err)
	}
}

func deploy(t *testing.T, f *fixture, project, version string, mutate func(*manifest.Release)) (*Report, error) {
	t.Helper()
	rel := f.release(project, version)
	if rel == nil {
		t.Fatal("bundle build failed")
	}
	if mutate != nil {
		mutate(rel)
	}
	return Deploy(t.Context(), DeployInput{
		Target:         f.target,
		TargetManifest: f.targetManifest(),
		Environment:    f.environment(project, version),
		Release:        rel,
		Bundler:        f.bundler,
		Owner:          "test",
	})
}

func history(t *testing.T, f *fixture) []target.Record {
	t.Helper()
	records, err := f.target.ReadHistory(t.Context(), "my-app", "production")
	if err != nil {
		if errors.Is(err, target.ErrHistoryAbsent) {
			return nil
		}
		t.Fatal(err)
	}
	return records
}

func TestDeployHappyPath(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	rep, err := deploy(t, f, "my-app", "1.0.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.FailureReason != "" {
		t.Fatalf("failure reason: %s", rep.FailureReason)
	}
	if !rep.Committed || !rep.ContractVerified {
		t.Errorf("rep = committed:%v contract:%v", rep.Committed, rep.ContractVerified)
	}
	if got := order(t, f.marker); strings.Join(got, ",") != "preflight,migrate,apply,verify" {
		t.Errorf("stage order = %v", got)
	}
	if rep.Staged != target.StageNew {
		t.Errorf("staged = %s", rep.Staged)
	}

	// Observed state advanced to the verified release.
	st, err := f.target.ReadState(t.Context(), "my-app", "production")
	if err != nil {
		t.Fatal(err)
	}
	if st.Current == nil || st.Current.Release != "1.0.0" || st.Current.BundleDigest != rep.BundleDigest {
		t.Errorf("state = %+v", st)
	}

	// Exactly one structured, secret-free history outcome.
	records := history(t, f)
	if len(records) != 1 || records[0].Type != "deploy.succeeded" {
		t.Fatalf("history = %+v", records)
	}
	if records[0].Data["release"] != "1.0.0" || records[0].Data["environment"] != "production" {
		t.Errorf("history data = %+v", records[0].Data)
	}
	if rep.HistorySeq != 1 {
		t.Errorf("history seq = %d", rep.HistorySeq)
	}
	requireNoLock(t, f)
}

func TestDeployMigrateSkippedWhenModeNone(t *testing.T) {
	// The release pins migration.mode none: the migrate hook must not run
	// even though the staged contract declares it.
	f := newFixture(t, "my-app", nil)
	rep, err := deploy(t, f, "my-app", "1.0.0", func(rel *manifest.Release) {
		rel.Migration = manifest.MigrationSpec{Mode: manifest.MigrationNone}
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.FailureReason != "" {
		t.Fatalf("failure reason: %s", rep.FailureReason)
	}
	if got := order(t, f.marker); strings.Join(got, ",") != "preflight,apply,verify" {
		t.Errorf("stage order = %v (migrate must be skipped for mode none)", got)
	}
	var mig *StageResult
	for i := range rep.Stages {
		if rep.Stages[i].Name == "migrate" {
			mig = &rep.Stages[i]
		}
	}
	if mig == nil || !mig.Skipped {
		t.Errorf("migrate stage = %+v, want skipped", mig)
	}
}

func TestDeployMigrateRunsWhenDeclared(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	if _, err := deploy(t, f, "my-app", "1.0.0", func(rel *manifest.Release) {
		rel.Migration = manifest.MigrationSpec{Mode: manifest.MigrationForwardCompatible, Head: "001_init", RollbackSafe: true}
	}); err != nil {
		t.Fatal(err)
	}
	if got := order(t, f.marker); strings.Join(got, ",") != "preflight,migrate,apply,verify" {
		t.Errorf("stage order = %v", got)
	}
}

func TestDeployHookFailureDoesNotCommit(t *testing.T) {
	// verify exits 3: apply already ran (consequence happened), but
	// observed state must NOT advance — apply success ≠ verified.
	f := newFixture(t, "my-app", func(marker, envFile, script string) string {
		if script == "verify" {
			return "#!/bin/sh\nexit 3\n"
		}
		return fmt.Sprintf("#!/bin/sh\necho %s >> %s\n", script, marker)
	})
	rep, err := deploy(t, f, "my-app", "1.0.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.FailureReason != "verify failed" || rep.Committed {
		t.Errorf("rep = %+v", rep)
	}
	if got := order(t, f.marker); strings.Join(got, ",") != "preflight,migrate,apply" {
		t.Errorf("stage order = %v", got)
	}
	if _, err := f.target.ReadState(t.Context(), "my-app", "production"); !errors.Is(err, target.ErrStateAbsent) {
		t.Errorf("state err = %v, want ErrStateAbsent", err)
	}
	records := history(t, f)
	if len(records) != 1 || records[0].Type != "deploy.failed" {
		t.Fatalf("history = %+v", records)
	}
	stages, _ := records[0].Data["stages"].(map[string]any)
	vs, _ := stages["verify"].(map[string]any)
	if vs == nil || fmt.Sprint(vs["exit"]) != "3" {
		t.Errorf("verify stage in history = %#v, want exit 3", stages["verify"])
	}
	if raw, ok := records[0].Data["reason"]; !ok || raw != "verify failed" {
		t.Errorf("history reason = %#v", records[0].Data["reason"])
	}
	requireNoLock(t, f)
}

func TestDeployPreflightFailureStops(t *testing.T) {
	f := newFixture(t, "my-app", func(marker, envFile, script string) string {
		if script == "preflight" {
			return "#!/bin/sh\necho cannot proceed >&2\nexit 1\n"
		}
		return fmt.Sprintf("#!/bin/sh\necho %s >> %s\n", script, marker)
	})
	rep, err := deploy(t, f, "my-app", "1.0.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.FailureReason != "preflight failed" || rep.Committed {
		t.Errorf("rep = %+v", rep)
	}
	if got := order(t, f.marker); len(got) != 0 {
		t.Errorf("consequential stages ran after preflight failure: %v", got)
	}
	// The preflight failure's own stderr is returned for diagnosis.
	if vr := rep.Stages[0]; vr.Name != "preflight" || !strings.Contains(string(vr.Stderr), "cannot proceed") {
		t.Errorf("preflight result = %+v", rep.Stages[0])
	}
	requireNoLock(t, f)
}

func TestDeployContractDigestMismatchFails(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	badDigest := "sha256:" + strings.Repeat("00", 32)
	rep, err := deploy(t, f, "my-app", "1.0.0", func(rel *manifest.Release) {
		rel.DeploymentContract = manifest.ContractDigest{Digest: badDigest}
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.FailureReason == "" || !strings.Contains(rep.FailureReason, "does not match the release pin") {
		t.Fatalf("failure reason = %q", rep.FailureReason)
	}
	if rep.ContractVerified || rep.Committed {
		t.Errorf("rep = %+v", rep)
	}
	// No hook ran: the contract could not be trusted.
	if got := order(t, f.marker); len(got) != 0 {
		t.Errorf("hooks ran with an unverifiable contract: %v", got)
	}
	if rep.Stages != nil {
		t.Errorf("stages recorded = %v", rep.Stages)
	}
	requireNoLock(t, f)
}

func TestDeployStagedContractIdentityMismatchFails(t *testing.T) {
	// The staged contract is for my-app; the release claims other-app.
	// The bundle staged under other-app contains my-app's contract — the
	// identity cross-check must catch it.
	f := newFixture(t, "my-app", nil)
	rep, err := deploy(t, f, "other-app", "1.0.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.FailureReason == "" || !strings.Contains(rep.FailureReason, `contract is for project "my-app"`) {
		t.Fatalf("failure reason = %q", rep.FailureReason)
	}
	if got := order(t, f.marker); len(got) != 0 {
		t.Errorf("hooks ran: %v", got)
	}
	requireNoLock(t, f)
}

func TestDeployLockHeldFailsClosed(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	lock, err := AcquireEnvLock(t.Context(), f.target.Transport(), f.lockDir(), LockOwner{User: "someone-else"})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release(t.Context())
	if _, err := deploy(t, f, "my-app", "1.0.0", nil); err == nil || !strings.Contains(err.Error(), "held by another deployment") {
		t.Fatalf("err = %v, want lock-held failure", err)
	}
	if len(history(t, f)) != 0 {
		t.Error("a lock refusal must not write history")
	}
}

func TestDeploySecondAttemptIsIdempotent(t *testing.T) {
	// Staging the same release twice: first attempt commits, second
	// attempt re-stages (already-staged), re-runs hooks, re-commits.
	f := newFixture(t, "my-app", nil)
	if _, err := deploy(t, f, "my-app", "1.0.0", nil); err != nil {
		t.Fatal(err)
	}
	rep, err := deploy(t, f, "my-app", "1.0.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Staged != target.StageAlreadyStaged {
		t.Errorf("second stage status = %s, want already-staged", rep.Staged)
	}
	records := history(t, f)
	if len(records) != 2 || records[1].Type != "deploy.succeeded" {
		t.Fatalf("history = %+v", records)
	}
	if rep.HistorySeq != 2 {
		t.Errorf("history seq = %d", rep.HistorySeq)
	}
	requireNoLock(t, f)
}

func TestDeployHookEnvironmentIsExact(t *testing.T) {
	var seen string
	f := newFixture(t, "my-app", func(marker, envFile, script string) string {
		if script == "apply" {
			return fmt.Sprintf("#!/bin/sh\n/usr/bin/env > %s\n", envFile)
		}
		return fmt.Sprintf("#!/bin/sh\necho %s >> %s\n", script, marker)
	})
	if _, err := deploy(t, f, "my-app", "1.0.0", nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(f.envFile)
	if err != nil {
		t.Fatal(err)
	}
	seen = string(raw)
	for _, want := range []string{
		"DEPLOY_PROJECT=my-app",
		"DEPLOY_ENVIRONMENT=production",
		"DEPLOY_RELEASE_VERSION=1.0.0",
		"DEPLOY_SOURCE_REVISION=" + f.revision,
		"DEPLOY_ARTIFACT_APP=ghcr.io/example/app@sha256:" + strings.Repeat("aa", 32),
		"PATH=" + hookDefaultPATH,
	} {
		if !strings.Contains(seen, want+"\n") {
			t.Errorf("hook env missing %q; env =\n%s", want, seen)
		}
	}
	for _, line := range strings.Split(seen, "\n") {
		if strings.HasPrefix(line, "HOME=") || strings.HasPrefix(line, "GITHUB_") {
			t.Errorf("ambient environment leaked into hook: %q", line)
		}
	}
}

func TestValidateDesiredRejectsWrongTargetAndRelease(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	env := f.environment("my-app", "1.0.0")
	rel := f.release("my-app", "1.0.0")
	if rel == nil {
		t.Fatal("bundle build failed")
	}

	env.Spec.Target = "some-other-target"
	if _, err := Deploy(t.Context(), DeployInput{
		Target: f.target, TargetManifest: f.targetManifest(), Environment: env, Release: rel, Bundler: f.bundler,
	}); err == nil || !strings.Contains(err.Error(), "targets") {
		t.Errorf("wrong target accepted: %v", err)
	}

	env = f.environment("my-app", "1.0.0")
	env.Spec.Release = ".deploy/releases/my-app-9.9.9.yaml"
	if _, err := Deploy(t.Context(), DeployInput{
		Target: f.target, TargetManifest: f.targetManifest(), Environment: env, Release: rel, Bundler: f.bundler,
	}); err == nil || !strings.Contains(err.Error(), "pins release") {
		t.Errorf("wrong release ref accepted: %v", err)
	}
}
