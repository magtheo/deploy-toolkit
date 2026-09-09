package lifecycle

import (
	"strings"
	"testing"

	"github.com/magtheo/deploy-toolkit/internal/target"
	"github.com/magtheo/deploy-toolkit/internal/transport/local"
)

// resolveInput wires a Resolve call for the fixture's project/env.
func resolveInput(t *testing.T, f *fixture, recoveryID, attemptID string) ResolveInput {
	t.Helper()
	return ResolveInput{
		Target:            f.target,
		TargetManifest:    f.targetManifest(),
		Project:           "my-app",
		Environment:       f.environment("my-app", "1.0.0"),
		Owner:             "operator",
		ConfirmRecoveryID: recoveryID,
		ConfirmAttemptID:  attemptID,
	}
}

func attemptMarkerID(t *testing.T, f *fixture) string {
	t.Helper()
	m, err := f.target.ReadAttempt(t.Context(), "my-app", "production")
	if err != nil {
		t.Fatal(err)
	}
	return m.AttemptID
}

func recoveryMarkerID(t *testing.T, f *fixture) string {
	t.Helper()
	m, err := f.target.ReadRecovery(t.Context(), "my-app", "production")
	if err != nil {
		t.Fatal(err)
	}
	return m.RecoveryID
}

// The canonical flow: a failed deploy leaves the attempt marker; the
// operator verifies the target by hand; resolve records evidence and
// removes the marker; normal operation resumes. Observed state is not
// modified, no hooks run.
func TestResolveAttemptAfterFailedDeploy(t *testing.T) {
	f := newFixture(t, "my-app", func(marker, envFile, script string) string {
		if script == "verify" {
			return "#!/bin/sh\nexit 1\n"
		}
		return "#!/bin/sh\necho " + script + " >> " + marker + "\n"
	})
	if _, err := deploy(t, f, "my-app", "1.0.0", nil); err != nil {
		t.Fatal(err)
	}
	if !attemptPresent(t, f) {
		t.Fatal("fixture bug: expected an unresolved attempt marker")
	}
	before := order(t, f.marker)
	id := attemptMarkerID(t, f)

	// Wrong identity: refused, marker stays.
	if _, err := Resolve(t.Context(), resolveInput(t, f, "", "0000000000000000")); err == nil {
		t.Fatal("a mismatched confirmation must be refused")
	}
	if !attemptPresent(t, f) {
		t.Fatal("a refused resolution must not remove the marker")
	}

	rep, err := Resolve(t.Context(), resolveInput(t, f, "", id))
	if err != nil {
		t.Fatal(err)
	}
	if rep.ResolvedAttemptID != id || rep.ResolvedRecoveryID != "" || rep.NothingToResolve {
		t.Fatalf("rep = %+v", rep)
	}
	if attemptPresent(t, f) {
		t.Fatal("the marker must be gone after resolution")
	}
	if got := order(t, f.marker); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Errorf("resolution executed hooks: %v → %v", before, got)
	}
	st, err := f.target.ReadState(t.Context(), "my-app", "production")
	if err != nil && !strings.Contains(err.Error(), "no deployment recorded") {
		t.Fatalf("observed state after resolution = %+v, %v; want untouched (absent)", st.Current, err)
	}
	records := history(t, f)
	last := records[len(records)-1]
	if last.Type != "resolution.authorized" {
		t.Fatalf("history = %+v, want resolution.authorized", last)
	}
	if last.Data["actor"] != "operator" {
		t.Errorf("evidence actor = %#v", last.Data["actor"])
	}

	// Idempotent: nothing left to resolve.
	rep2, err := Resolve(t.Context(), resolveInput(t, f, "", id))
	if err != nil || !rep2.NothingToResolve {
		t.Fatalf("rep2 = %+v, %v; want a no-op", rep2, err)
	}
}

// The failed-recovery shape: attempt + recovery markers coexist; resolve
// must remove the attempt marker FIRST — a crash during the recovery
// marker's removal leaves the more informative fact in place.
func TestResolveRemovesAttemptBeforeRecovery(t *testing.T) {
	f := newFixture(t, "my-app", func(marker, envFile, script string) string {
		body := "#!/bin/sh\necho " + script + " >> " + marker + "\n"
		if script == "verify" {
			// 2.0.0's verify fails: the failed deploy leaves the attempt marker.
			body += "if [ \"$DEPLOY_RELEASE_VERSION\" = \"2.0.0\" ]; then\n  exit 7\nfi\n"
		}
		if script == "rollback" {
			// 2.0.0's rollback hook fails: the recovery leaves its marker.
			body += "exit 1\n"
		}
		return body
	})
	revA := f.revision
	if _, err := deploy(t, f, "my-app", "1.0.0", nil); err != nil {
		t.Fatal(err)
	}
	revB := secondRevision(t, f)
	relB, bytesB := f.preparedBytesAt(t, revB, "my-app", "2.0.0")
	if _, err := Deploy(t.Context(), DeployInput{
		Target: f.target, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "2.0.0"), Release: relB, Bundle: bytesB, Owner: "test",
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := Rollback(t.Context(), rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery))
	if err != nil || rep.RecoveryRequired != true {
		t.Fatalf("rep = %+v, %v; want a failed recovery", rep, err)
	}
	if !attemptPresent(t, f) || !recoveryPresent(t, f) {
		t.Fatal("fixture bug: expected attempt + recovery markers")
	}
	recID, attID := recoveryMarkerID(t, f), attemptMarkerID(t, f)

	// Fail the recovery marker's removal: the attempt marker (cleared
	// first) must already be gone.
	tr := &failingRunTransport{
		inner: local.New(),
		failRunWhen: func(argv []string) bool {
			return argv[0] == "rm" && strings.HasSuffix(argv[len(argv)-1], "/recoveries/production.json")
		},
	}
	tgt2, err := target.New(tr, f.root)
	if err != nil {
		t.Fatal(err)
	}
	in := resolveInput(t, f, recID, attID)
	in.Target = tgt2
	if _, err := Resolve(t.Context(), in); err == nil || !strings.Contains(err.Error(), "clear recovery marker") {
		t.Fatalf("err = %v, want the recovery-clear failure", err)
	}
	if attemptPresent(t, f) {
		t.Error("the attempt marker must be cleared before the recovery marker")
	}
	if !recoveryPresent(t, f) {
		t.Error("the recovery marker must survive its own failed removal")
	}

	// The retry must confirm what is ACTUALLY still there: the attempt
	// marker is already gone, so the stale attempt id is refused...
	if _, err := Resolve(t.Context(), resolveInput(t, f, recID, attID)); err == nil || !strings.Contains(err.Error(), "does not carry that marker") {
		t.Fatalf("err = %v, want a stale-confirmation refusal", err)
	}
	// ...and the resolution completes by confirming the remaining fact.
	if _, err := Resolve(t.Context(), resolveInput(t, f, recID, "")); err != nil {
		t.Fatal(err)
	}
	if recoveryPresent(t, f) || attemptPresent(t, f) {
		t.Error("markers must be gone after the completed resolution")
	}
	last := history(t, f)[3]
	if last.Type != "resolution.authorized" {
		t.Errorf("retry history = %+v, want resolution.authorized", last)
	}
}

// A held lock means an operation may be executing: resolution must
// refuse, or it could delete the live operation's own markers.
func TestResolveRefusesUnderHeldLock(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	if _, err := deploy(t, f, "my-app", "1.0.0", nil); err != nil {
		t.Fatal(err)
	}
	if err := f.target.WriteAttempt(t.Context(), target.AttemptMarker{
		Schema: "toolkit.attempt/v1", AttemptID: "0123456789abcdef",
		Project: "my-app", Environment: "production",
		FromRelease: "0.9.0", ToRelease: "1.0.0",
		BundleDigest: "sha256:" + strings.Repeat("aa", 32),
		StartedAt:    "2026-09-09T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireEnvLock(t.Context(), local.New(), f.lockDir(), LockOwner{User: "someone-else"})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release(t.Context())
	if _, err := Resolve(t.Context(), resolveInput(t, f, "", "0123456789abcdef")); err == nil || !strings.Contains(err.Error(), "acquire environment lock") {
		t.Fatalf("err = %v, want a lock refusal", err)
	}
	if !attemptPresent(t, f) {
		t.Error("a refused resolution must not remove the marker")
	}
}

// Emergency rollbacks carry sourceAttemptId "" — no source attempt. A
// coexisting attempt marker under such a recovery is inconsistent
// evidence, not permission to clear both: refuse as inspect-first.
func TestResolveRefusesInconsistentMarkerCoexistence(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	if _, err := deploy(t, f, "my-app", "1.0.0", nil); err != nil {
		t.Fatal(err)
	}
	// Forge the inconsistent pair: an emergency-style recovery marker
	// (no source attempt) plus an unrelated attempt marker.
	if err := f.target.WriteRecovery(t.Context(), target.RecoveryMarker{
		Schema: "toolkit.recovery/v1", RecoveryID: "0123456789abcdef",
		SourceAttemptID: "", Project: "my-app", Environment: "production",
		FromRelease: "1.0.0", FromBundleDigest: "sha256:" + strings.Repeat("aa", 32),
		ToRelease: "0.9.0", ToBundleDigest: "sha256:" + strings.Repeat("bb", 32),
		Authorization: "manual", StartedAt: "2026-09-09T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.target.WriteAttempt(t.Context(), target.AttemptMarker{
		Schema: "toolkit.attempt/v1", AttemptID: "fedcba9876543210",
		Project: "my-app", Environment: "production",
		FromRelease: "0.9.0", ToRelease: "1.0.0",
		BundleDigest: "sha256:" + strings.Repeat("cc", 32),
		StartedAt:    "2026-09-09T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	// Clearing BOTH in one authorization is refused...
	if _, err := Resolve(t.Context(), resolveInput(t, f, "0123456789abcdef", "fedcba9876543210")); err == nil || !strings.Contains(err.Error(), "inconsistent evidence") {
		t.Fatalf("err = %v, want the coexistence refusal", err)
	}
	if !recoveryPresent(t, f) || !attemptPresent(t, f) {
		t.Error("a refused resolution must not remove either marker")
	}
	// ...but resolving ONE AT A TIME is the inspect-first path and
	// works: each partial resolution leaves the other marker blocking.
	rep1, err := Resolve(t.Context(), resolveInput(t, f, "", "fedcba9876543210"))
	if err != nil || rep1.ResolvedAttemptID == "" || rep1.LeftRecoveryID != "0123456789abcdef" {
		t.Fatalf("rep1 = %+v, %v", rep1, err)
	}
	if attemptPresent(t, f) || !recoveryPresent(t, f) {
		t.Fatal("only the attempt marker must be gone after the partial resolution")
	}
	rep2, err := Resolve(t.Context(), resolveInput(t, f, "0123456789abcdef", ""))
	if err != nil || rep2.ResolvedRecoveryID == "" {
		t.Fatalf("rep2 = %+v, %v", rep2, err)
	}
	if recoveryPresent(t, f) || attemptPresent(t, f) {
		t.Error("both markers must be gone after one-at-a-time resolution")
	}
}

// Resolve owns the same manifest binding Deploy and Rollback enforce:
// the environment must point at the target manifest it is resolving on.
func TestResolveRefusesEnvironmentTargetMismatch(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	if _, err := deploy(t, f, "my-app", "1.0.0", nil); err != nil {
		t.Fatal(err)
	}
	in := resolveInput(t, f, "", "")
	other := *f.targetManifest()
	other.Metadata.Name = "some-other-target"
	in.TargetManifest = &other
	if _, err := Resolve(t.Context(), in); err == nil || !strings.Contains(err.Error(), "targets") {
		t.Fatalf("err = %v, want the environment/target binding refusal", err)
	}
}
