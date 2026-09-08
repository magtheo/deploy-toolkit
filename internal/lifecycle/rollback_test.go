package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/target"
	"github.com/magtheo/deploy-toolkit/internal/transport/local"
)

// secondRevision creates a genuinely different release identity: the same
// project one commit later — different bundle bytes, different digest.
func secondRevision(t *testing.T, f *fixture) string {
	t.Helper()
	p := filepath.Join(f.repoDir, "deploy/apply.sh")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, append(raw, []byte("# release-2.0.0 variant\n")...), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, f.repoDir, "add", "-A")
	git(t, f.repoDir, "commit", "-q", "-m", "second revision")
	return git(t, f.repoDir, "rev-parse", "HEAD")
}

// rollbackInput wires a recovery transition from two revisions: fromRev
// being rolled back as fromVer, toRev being restored as toVer. The
// environment pins the FROM release (the auto/emergency situation before
// Git is reconciled).

// relA0Digest pins the restored release's bundle digest for forged-state
// scenarios.
func relA0Digest(t *testing.T, f *fixture) string {
	t.Helper()
	rel, _ := f.preparedBytes(t, "my-app", "1.0.0")
	return rel.Bundle.Digest
}

func rollbackInput(t *testing.T, f *fixture, fromRev, fromVer, toRev, toVer string, auth RollbackAuthorization) RollbackInput {
	t.Helper()
	fromRel, fromBytes := f.preparedBytesAt(t, fromRev, "my-app", fromVer)
	toRel, toBytes := f.preparedBytesAt(t, toRev, "my-app", toVer)
	return RollbackInput{
		Target:         f.target,
		TargetManifest: f.targetManifest(),
		Environment:    f.environment("my-app", fromVer),
		FromRelease:    fromRel,
		FromBundle:     fromBytes,
		ToRelease:      toRel,
		ToBundle:       toBytes,
		Authorization:  auth,
		Owner:          "test",
	}
}

// versionTaggedHooks makes every hook record its name AND the release
// version it ran for — the only way to prove WHICH release's staged
// contract supplied a hook. verify additionally fails (exit 7) when the
// hook environment says it is 2.0.0: a post-consequential, determined
// failure of exactly the new release.
func versionTaggedHooks(marker, envFile, script string) string {
	body := fmt.Sprintf("#!/bin/sh\necho %s-$DEPLOY_RELEASE_VERSION >> %s\n", script, marker)
	if script == "verify" {
		body += "if [ \"$DEPLOY_RELEASE_VERSION\" = \"2.0.0\" ]; then\n  exit 7\nfi\n"
	}
	return body
}

func TestRollbackResolvesUnresolvedAttempt(t *testing.T) {
	// The full recovery story: 1.0.0 runs; the 2.0.0 deployment fails at
	// verify (marker kept, observed still 1.0.0, production may partly
	// run 2.0.0); explicit recovery rolls 2.0.0 back to 1.0.0.
	//
	// Hook ownership is the point: 2.0.0's staged contract supplies the
	// rollback hook (B undoes B's consequences — no 1.0.0 migrate runs as
	// an undo mechanism), and 1.0.0's staged contract supplies
	// preflight/apply/verify (A installs and verifies A).
	f := newFixture(t, "my-app", versionTaggedHooks)
	revA := f.revision
	if _, err := deploy(t, f, "my-app", "1.0.0", nil); err != nil {
		t.Fatal(err)
	}
	revB := secondRevision(t, f)

	relB, bytesB := f.preparedBytesAt(t, revB, "my-app", "2.0.0")
	repB, err := Deploy(t.Context(), DeployInput{
		Target: f.target, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "2.0.0"), Release: relB, Bundle: bytesB, Owner: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if repB.FailureReason != "verify failed" || repB.Committed {
		t.Fatalf("2.0.0 deploy rep = %+v, want determined verify failure", repB)
	}
	if !attemptPresent(t, f) {
		t.Fatal("failed 2.0.0 deploy must leave the attempt marker")
	}
	st, err := f.target.ReadState(t.Context(), "my-app", "production")
	if err != nil || st.Current.Release != "1.0.0" {
		t.Fatalf("observed state after failed deploy = %+v, %v; want 1.0.0", st.Current, err)
	}

	rep, err := Rollback(t.Context(), rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery))
	if err != nil {
		t.Fatal(err)
	}
	if rep.FailureReason != "" || !rep.Committed || !rep.RecoveryResolved || !rep.RecoveryMarkerCreated {
		t.Fatalf("rollback rep = %+v", rep)
	}
	if rep.RecoveryID == "" {
		t.Fatal("a recovery must carry its marker id in the report")
	}
	want := "preflight-1.0.0,migrate-1.0.0,apply-1.0.0,verify-1.0.0," +
		"preflight-2.0.0,migrate-2.0.0,apply-2.0.0,verify-2.0.0," +
		"preflight-1.0.0,rollback-2.0.0,apply-1.0.0,verify-1.0.0"
	if got := strings.Join(order(t, f.marker), ","); got != want {
		t.Fatalf("hook execution = %v\nwant %v", got, want)
	}
	names := make([]string, 0, len(rep.Stages))
	for _, s := range rep.Stages {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "preflight,rollback,apply,verify" {
		t.Errorf("rollback stages = %v", names)
	}
	// The exact sequence above IS the ownership proof: exactly one
	// rollback hook run, from 2.0.0's staged directory with 2.0.0's hook
	// environment (rollback-2.0.0); 1.0.0's migrate never re-runs as an
	// undo mechanism; 2.0.0's forward apply/verify never re-run during
	// the recovery (their only appearances are the failed deployment
	// itself).

	// Observed state is back to the restored release, signed by THIS
	// recovery; both markers are gone.
	st, err = f.target.ReadState(t.Context(), "my-app", "production")
	if err != nil || st.Current == nil || st.Current.Release != "1.0.0" || st.Current.BundleDigest != rep.ToBundleDigest {
		t.Fatalf("state after rollback = %+v, %v", st.Current, err)
	}
	if st.Current.OperationID != "recovery:"+rep.RecoveryID {
		t.Errorf("state operationId = %q, want recovery:%s", st.Current.OperationID, rep.RecoveryID)
	}
	if attemptPresent(t, f) || recoveryPresent(t, f) {
		t.Error("trusted terminal did not clear the attempt and recovery markers")
	}
	requireNoLock(t, f)

	records := history(t, f)
	if len(records) != 3 ||
		records[0].Type != "deploy.succeeded" ||
		records[1].Type != "deploy.failed" ||
		records[2].Type != "rollback.succeeded" {
		t.Fatalf("history = %+v", records)
	}
	rb := records[2].Data
	if rb["fromRelease"] != "2.0.0" || rb["toRelease"] != "1.0.0" || rb["authorization"] != "recovery" || rb["recoveryResolved"] != true {
		t.Errorf("rollback history data = %+v", rb)
	}
	if rb["recoveryId"] != rep.RecoveryID {
		t.Errorf("rollback history recoveryId = %#v, want %s", rb["recoveryId"], rep.RecoveryID)
	}
}

func TestRollbackEmergencyWithoutMarker(t *testing.T) {
	// Emergency path: production healthy on 2.0.0 (no unresolved
	// attempt), an operator forces the previous release back. Rollback
	// creates its own recovery marker before the first consequential
	// stage, and clears it at the trusted terminal.
	f := newFixture(t, "my-app", nil)
	if _, err := deploy(t, f, "my-app", "1.0.0", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := deploy(t, f, "my-app", "2.0.0", nil); err != nil {
		t.Fatal(err)
	}
	if attemptPresent(t, f) {
		t.Fatal("healthy deployments must not leave attempt markers")
	}

	// Same revision → same bundle bytes for both versions; the identity
	// bindings work on release VERSION + digest, so this exercises the
	// version binding precisely.
	rep, err := Rollback(t.Context(), rollbackInput(t, f, f.revision, "2.0.0", f.revision, "1.0.0", RollbackManual))
	if err != nil {
		t.Fatal(err)
	}
	if rep.FailureReason != "" || !rep.Committed || !rep.RecoveryMarkerCreated || rep.RecoveryResolved {
		t.Fatalf("rollback rep = %+v", rep)
	}
	if got := strings.Join(order(t, f.marker), ","); got != "preflight,migrate,apply,verify,preflight,migrate,apply,verify,preflight,rollback,apply,verify" {
		t.Errorf("hook execution = %q", got)
	}
	st, err := f.target.ReadState(t.Context(), "my-app", "production")
	if err != nil || st.Current == nil || st.Current.Release != "1.0.0" {
		t.Fatalf("state after emergency rollback = %+v, %v", st.Current, err)
	}
	if attemptPresent(t, f) || recoveryPresent(t, f) {
		t.Error("emergency rollback's recovery marker was not cleared")
	}
	requireNoLock(t, f)
	records := history(t, f)
	if len(records) != 3 || records[2].Type != "rollback.succeeded" || records[2].Data["recoveryMarkerCreated"] != true {
		t.Fatalf("history = %+v", records)
	}
}

func TestRollbackBindingsRefuseMismatches(t *testing.T) {
	// The marker is the recovery contract: a rollback that does not
	// resolve EXACTLY the recorded attempt is refused — in either
	// direction — and the marker survives the refusal.
	f := newFixture(t, "my-app", versionTaggedHooks)
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
	before := order(t, f.marker)

	t.Run("wrong from release", func(t *testing.T) {
		// The marker records an attempt TO 2.0.0; rolling back "1.0.0"
		// does not resolve it.
		rep, err := Rollback(t.Context(), rollbackInput(t, f, revA, "1.0.0", revA, "0.9.0", RollbackRecovery))
		if err != nil {
			t.Fatal(err)
		}
		if rep.Committed || !strings.Contains(rep.FailureReason, "must resolve the recorded attempt") {
			t.Fatalf("rep = %+v, want attempt-binding refusal", rep)
		}
		if !attemptPresent(t, f) {
			t.Fatal("a refused recovery deleted the marker")
		}
	})
	t.Run("wrong restore target", func(t *testing.T) {
		// The attempt started from 1.0.0; restoring "0.9.0" would guess.
		rep, err := Rollback(t.Context(), rollbackInput(t, f, revB, "2.0.0", revA, "0.9.0", RollbackRecovery))
		if err != nil {
			t.Fatal(err)
		}
		if rep.Committed || !strings.Contains(rep.FailureReason, "did not replace") {
			t.Fatalf("rep = %+v, want restore-target refusal", rep)
		}
		if !attemptPresent(t, f) {
			t.Fatal("a refused recovery deleted the marker")
		}
	})
	t.Run("no hooks ran and refusals are recorded facts", func(t *testing.T) {
		if got := strings.Join(order(t, f.marker), ","); got != strings.Join(before, ",") {
			t.Errorf("hooks ran during refused recoveries: %v → %v", before, got)
		}
		records := history(t, f)
		n := len(records)
		if n < 2 || records[n-2].Type != "rollback.failed" || records[n-1].Type != "rollback.failed" {
			t.Fatalf("history tail = %+v", records[max(n-2, 0):])
		}
		if records[n-1].Data["stagedFrom"] != "not-attempted" || records[n-1].Data["stagedTo"] != "not-attempted" {
			t.Errorf("refusals must record truthful staging evidence: %+v", records[n-1].Data)
		}
	})
	requireNoLock(t, f)
}

func TestRollbackAutoRequiresMarkerAndSafeRelease(t *testing.T) {
	// Auto rollback is pre-authorization, not a blanket authority: it
	// needs an unresolved attempt (nothing to auto-undo otherwise), the
	// environment's safe-only policy, and a failed release that claims
	// its migration is rollback-safe.
	f := newFixture(t, "my-app", versionTaggedHooks)
	revA := f.revision
	if _, err := deploy(t, f, "my-app", "1.0.0", nil); err != nil {
		t.Fatal(err)
	}
	revB := secondRevision(t, f)
	simulateFailedAttempt := func(t *testing.T) {
		t.Helper()
		relB, _ := f.preparedBytesAt(t, revB, "my-app", "2.0.0")
		if err := f.target.WriteAttempt(t.Context(), target.AttemptMarker{
			AttemptID:    "abcdef0123456789",
			Project:      "my-app",
			Environment:  "production",
			FromRelease:  "1.0.0",
			ToRelease:    "2.0.0",
			BundleDigest: relB.Bundle.Digest,
			StartedAt:    "2026-09-08T00:00:00Z",
		}); err != nil {
			t.Fatal(err)
		}
	}
	autoInput := func(t *testing.T, policy string, rollbackSafe bool) RollbackInput {
		t.Helper()
		in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackAuto)
		if policy != "" {
			in.Environment.Spec.FailurePolicy = &manifest.FailurePolicy{AutoRollback: policy}
		}
		if !rollbackSafe {
			in.FromRelease.Migration.RollbackSafe = false
		}
		return in
	}

	t.Run("no marker", func(t *testing.T) {
		rep, err := Rollback(t.Context(), autoInput(t, "", true))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(rep.FailureReason, "no unresolved attempt marker") {
			t.Fatalf("rep = %+v", rep)
		}
	})
	t.Run("policy off", func(t *testing.T) {
		simulateFailedAttempt(t)
		rep, err := Rollback(t.Context(), autoInput(t, manifest.AutoRollbackOff, true))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(rep.FailureReason, "autoRollback: off") {
			t.Fatalf("rep = %+v", rep)
		}
		if !attemptPresent(t, f) {
			t.Fatal("a policy refusal must not touch the marker")
		}
	})
	t.Run("release not rollback-safe", func(t *testing.T) {
		simulateFailedAttempt(t)
		rep, err := Rollback(t.Context(), autoInput(t, manifest.AutoRollbackSafeOnly, false))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(rep.FailureReason, "rollbackSafe: false disables automatic rollback") {
			t.Fatalf("rep = %+v", rep)
		}
		if !attemptPresent(t, f) {
			t.Fatal("a policy refusal must not touch the marker")
		}
		if _, err := f.target.ReadState(t.Context(), "my-app", "production"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("safe-only with rollback-safe release resolves", func(t *testing.T) {
		simulateFailedAttempt(t)
		// The 2.0.0 release has never been staged here (the simulated
		// attempt never reached staging) — auto recovery must stage it.
		rep, err := Rollback(t.Context(), autoInput(t, manifest.AutoRollbackSafeOnly, true))
		if err != nil {
			t.Fatal(err)
		}
		if rep.FailureReason != "" || !rep.Committed {
			t.Fatalf("rep = %+v", rep)
		}
		if rep.StagedFrom != target.StageNew {
			t.Errorf("stagedFrom = %s, want new (auto recovery stages the failed release itself)", rep.StagedFrom)
		}
		if attemptPresent(t, f) || recoveryPresent(t, f) {
			t.Error("resolved auto rollback left attempt/recovery markers behind")
		}
		requireNoLock(t, f)
	})
}

func TestRollbackIrreversibleRefusedForEveryAuthority(t *testing.T) {
	// "irreversible" is a semantic claim that the migration cannot be
	// undone. No authorization path may override it — the toolkit will
	// not pretend a rollback restores pre-migration state. (Deploying an
	// irreversible release is allowed; undoing it is not.)
	f := newFixture(t, "my-app", versionTaggedHooks)
	revA := f.revision
	if _, err := deploy(t, f, "my-app", "1.0.0", nil); err != nil {
		t.Fatal(err)
	}
	revB := secondRevision(t, f)
	relB, bytesB := f.preparedBytesAt(t, revB, "my-app", "2.0.0")
	relB.Migration = manifest.MigrationSpec{Mode: manifest.MigrationIrreversible, Head: "001_init", RollbackSafe: false}
	if _, err := Deploy(t.Context(), DeployInput{
		Target: f.target, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "2.0.0"), Release: relB, Bundle: bytesB, Owner: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if !attemptPresent(t, f) {
		t.Fatal("test setup: the irreversible deploy must have failed and left its marker")
	}

	in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
	in.FromRelease.Migration = manifest.MigrationSpec{Mode: manifest.MigrationIrreversible, Head: "001_init", RollbackSafe: false}
	rep, err := Rollback(t.Context(), in)
	if err != nil || !strings.Contains(rep.FailureReason, "cannot be undone") {
		t.Fatalf("recovery rep = %+v, %v; want irreversible refusal", rep, err)
	}
	in = rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackAuto)
	in.FromRelease.Migration = manifest.MigrationSpec{Mode: manifest.MigrationIrreversible, Head: "001_init", RollbackSafe: false}
	rep, err = Rollback(t.Context(), in)
	if err != nil || !strings.Contains(rep.FailureReason, "cannot be undone") {
		t.Fatalf("auto rep = %+v, %v; want irreversible refusal", rep, err)
	}
	if !attemptPresent(t, f) {
		t.Fatal("a refused irreversible rollback must not touch the marker")
	}
	requireNoLock(t, f)
}

func TestRollbackFailedRecoveryRequiresExplicitResolution(t *testing.T) {
	// The recovery-of-recovery rule: once the recovery marker exists, ANY
	// failure keeps it, and an ordinary retry REFUSES instead of
	// repeating consequential work. B's rollback hook may have reversed a
	// migration and A's apply may have half-run — "try the whole
	// sequence again" is exactly the behavior the attempt-marker
	// machinery exists to prevent. Resolution is explicit: an operator
	// verifies the target, removes the marker, and takes a fresh,
	// deliberate recovery.
	f := newFixture(t, "my-app", versionTaggedHooks)
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

	// Recovery loses the transport during 1.0.0's apply — AFTER 2.0.0's
	// rollback hook ran.
	tr := &failingRunTransport{
		inner:    local.New(),
		failWhen: func(argv0 string) bool { return strings.Contains(argv0, "apply.sh") },
	}
	tgt, err := target.New(tr, f.root)
	if err != nil {
		t.Fatal(err)
	}
	in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
	in.Target = tgt
	if _, err := Rollback(t.Context(), in); err == nil {
		t.Fatal("transport loss during recovery must be an error")
	}
	if !attemptPresent(t, f) || !recoveryPresent(t, f) {
		t.Fatal("a failed recovery must keep BOTH markers — its own outcome is unresolved")
	}
	if st, err := f.target.ReadState(t.Context(), "my-app", "production"); err != nil || st.Current.Release != "1.0.0" {
		t.Fatalf("state must be untouched by the failed recovery: %+v, %v", st.Current, err)
	}
	requireNoLock(t, f)
	afterFirst := order(t, f.marker)
	if strings.Count(strings.Join(afterFirst, ","), "rollback-2.0.0") != 1 {
		t.Fatalf("rollback hook execution = %v, want exactly one rollback hook run", afterFirst)
	}

	// An ordinary retry REFUSES and repeats nothing.
	rep, err := Rollback(t.Context(), rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.RecoveryRequired || rep.Committed {
		t.Fatalf("retry rep = %+v, want recovery-required refusal", rep)
	}
	if !strings.Contains(rep.FailureReason, "not known-safe") {
		t.Errorf("failure reason = %q", rep.FailureReason)
	}
	if got := order(t, f.marker); strings.Join(got, ",") != strings.Join(afterFirst, ",") {
		t.Fatalf("the refused retry re-ran hooks: %v → %v", afterFirst, got)
	}
	if !attemptPresent(t, f) || !recoveryPresent(t, f) {
		t.Fatal("a refused retry must keep both markers")
	}

	// Explicit resolution: the operator verifies the target and removes
	// the recovery marker — then a FRESH recovery is authorized and
	// resolves the original deployment attempt.
	removeRecoveryMarker(t, f)
	rep, err = Rollback(t.Context(), rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery))
	if err != nil {
		t.Fatal(err)
	}
	if rep.FailureReason != "" || !rep.Committed || !rep.RecoveryResolved || !rep.RecoveryMarkerCreated {
		t.Fatalf("fresh recovery rep = %+v", rep)
	}
	if attemptPresent(t, f) || recoveryPresent(t, f) {
		t.Error("resolved recovery left markers behind")
	}
	records := history(t, f)
	if records[len(records)-1].Type != "rollback.succeeded" {
		t.Fatalf("history tail = %+v", records[len(records)-1])
	}
}

func TestRollbackEmergencyRetryIsRefusedNotReplayed(t *testing.T) {
	// A marker-less emergency rollback that fails mid-flight must leave a
	// RECOVERY marker whose direction is the rollback's own (B → A). The
	// pre-recovery-marker design recorded that direction on the attempt
	// marker and then misread it as an original deployment attempt,
	// making the retry impossible; now the retry is simply refused —
	// with no hook repetition — until explicit resolution. (Plain hooks:
	// both deployments must be healthy here.)
	f := newFixture(t, "my-app", nil)
	if _, err := deploy(t, f, "my-app", "1.0.0", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := deploy(t, f, "my-app", "2.0.0", nil); err != nil {
		t.Fatal(err)
	}

	tr := &failingRunTransport{
		inner:    local.New(),
		failWhen: func(argv0 string) bool { return strings.Contains(argv0, "apply.sh") },
	}
	tgt, err := target.New(tr, f.root)
	if err != nil {
		t.Fatal(err)
	}
	in := rollbackInput(t, f, f.revision, "2.0.0", f.revision, "1.0.0", RollbackManual)
	in.Target = tgt
	if _, err := Rollback(t.Context(), in); err == nil {
		t.Fatal("transport loss during emergency rollback must be an error")
	}
	if attemptPresent(t, f) {
		t.Fatal("an emergency rollback must not fabricate a deployment attempt marker")
	}
	if !recoveryPresent(t, f) {
		t.Fatal("a failed emergency rollback must leave its recovery marker")
	}
	rm, err := f.target.ReadRecovery(t.Context(), "my-app", "production")
	if err != nil {
		t.Fatal(err)
	}
	if rm.FromRelease != "2.0.0" || rm.ToRelease != "1.0.0" || rm.SourceAttemptID != "" || rm.Authorization != "manual" {
		t.Fatalf("recovery marker = %+v, want the rollback's own transition", rm)
	}
	afterFirst := order(t, f.marker)

	rep, err := Rollback(t.Context(), rollbackInput(t, f, f.revision, "2.0.0", f.revision, "1.0.0", RollbackManual))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.RecoveryRequired || rep.Committed {
		t.Fatalf("retry rep = %+v, want refusal", rep)
	}
	if got := order(t, f.marker); strings.Join(got, ",") != strings.Join(afterFirst, ",") {
		t.Fatalf("the refused retry re-ran hooks: %v → %v", afterFirst, got)
	}
	if !recoveryPresent(t, f) {
		t.Fatal("a refused retry deleted the recovery marker")
	}
	requireNoLock(t, f)
}

func TestRollbackStaleRecoveryMarkerSelfHealsWithoutHooks(t *testing.T) {
	// The crash window that release identity cannot close: recovery
	// commits observed state (A, operationId recovery:X) and the marker
	// cleanup fails. Durable state — observed A + recovery marker — is
	// observationally identical to "rollback never started" (observed A
	// already said A). The operationId breaks the tie: when observed
	// state carries recovery:X, the NEXT recovery invocation for the
	// same transition may complete the cleanup — and must run NO hooks,
	// because re-running B's rollback hook or A's apply over a committed
	// A would be blind consequential work.
	f := newFixture(t, "my-app", versionTaggedHooks)
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

	// The recovery succeeds through the state commit; removing the
	// recovery marker then loses the connection.
	tr := &failingRunTransport{
		inner: local.New(),
		failRunWhen: func(argv []string) bool {
			return argv[0] == "rm" && strings.HasSuffix(argv[len(argv)-1], "/recoveries/production.json")
		},
	}
	tgt, err := target.New(tr, f.root)
	if err != nil {
		t.Fatal(err)
	}
	in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
	in.Target = tgt
	rep, err := Rollback(t.Context(), in)
	if err == nil || !strings.Contains(err.Error(), "recovery marker could not be cleared") {
		t.Fatalf("err = %v, want a recovery-cleanup failure", err)
	}
	if !rep.Committed {
		t.Fatal("the state commit happened and must be reported")
	}
	// Clearing order: the attempt marker went first and is gone; the
	// leftover RECOVERY marker is the healable one.
	if !recoveryPresent(t, f) {
		t.Fatal("the recovery marker must survive the failed cleanup")
	}
	if attemptPresent(t, f) {
		t.Fatal("the attempt marker should already be cleared (attempt-first order)")
	}
	if st, err := f.target.ReadState(t.Context(), "my-app", "production"); err != nil || st.Current.OperationID != "recovery:"+rep.RecoveryID {
		t.Fatalf("state = %+v, %v; want operationId recovery:%s", st.Current, err, rep.RecoveryID)
	}
	before := order(t, f.marker)

	// The next recovery invocation for the same transition recognizes the
	// proven commit, runs nothing, and completes the cleanup.
	rep2, err := Rollback(t.Context(), rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery))
	if err != nil {
		t.Fatal(err)
	}
	if !rep2.AlreadyRecovered || rep2.Committed || rep2.RecoveryMarkerCreated {
		t.Fatalf("self-heal rep = %+v, want cleanup-only", rep2)
	}
	if got := order(t, f.marker); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Fatalf("the self-heal executed hooks: %v → %v", before, got)
	}
	if attemptPresent(t, f) || recoveryPresent(t, f) {
		t.Error("self-heal did not clear the markers")
	}
	requireNoLock(t, f)
	records := history(t, f)
	last := records[len(records)-1]
	if last.Type != "rollback.already-recovered" || last.Data["alreadyRecovered"] != true {
		t.Fatalf("history tail = %+v", last)
	}
}

func TestRollbackRecoveryMarkerMismatchAndCorruptionFailClosed(t *testing.T) {
	f := newFixture(t, "my-app", versionTaggedHooks)
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
	before := order(t, f.marker)

	t.Run("corrupt recovery marker", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Dir(f.recoveryPath()), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.recoveryPath(), []byte("{broken"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Rollback(t.Context(), rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)); err == nil {
			t.Fatal("a corrupt recovery marker must fail closed, not count as absent")
		}
		if got := order(t, f.marker); strings.Join(got, ",") != strings.Join(before, ",") {
			t.Error("hooks ran despite a corrupt recovery marker")
		}
	})
	t.Run("different transition", func(t *testing.T) {
		relB2, _ := f.preparedBytesAt(t, revB, "my-app", "2.0.0")
		if err := f.target.WriteRecovery(t.Context(), target.RecoveryMarker{
			RecoveryID:       "0123456789abcdef",
			SourceAttemptID:  "",
			Project:          "my-app",
			Environment:      "production",
			FromRelease:      "2.0.0",
			FromBundleDigest: relB2.Bundle.Digest,
			ToRelease:        "0.9.0",
			ToBundleDigest:   relB2.Bundle.Digest,
			Authorization:    "manual",
			StartedAt:        "2026-09-08T00:00:00Z",
		}); err != nil {
			t.Fatal(err)
		}
		rep, err := Rollback(t.Context(), rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery))
		if err != nil {
			t.Fatal(err)
		}
		if !rep.RecoveryRequired || rep.Committed || !strings.Contains(rep.FailureReason, "not known-safe") {
			t.Fatalf("rep = %+v, want unresolved-recovery refusal", rep)
		}
		if got := order(t, f.marker); strings.Join(got, ",") != strings.Join(before, ",") {
			t.Error("hooks ran despite an unresolved unrelated recovery")
		}
		if !recoveryPresent(t, f) {
			t.Error("the refusal deleted the unrelated recovery marker")
		}
	})
	requireNoLock(t, f)
}

func TestDeployRefusesWhileRecoveryUnresolved(t *testing.T) {
	// An unresolved recovery means production may be between the two
	// releases. Normal Deploy of ANYTHING must refuse — including the
	// release the recovery was restoring — and must never heal or clear
	// the recovery marker; resolution belongs to the recovery operation.
	f := newFixture(t, "my-app", versionTaggedHooks)
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
	tr := &failingRunTransport{
		inner:    local.New(),
		failWhen: func(argv0 string) bool { return strings.Contains(argv0, "apply.sh") },
	}
	tgt, err := target.New(tr, f.root)
	if err != nil {
		t.Fatal(err)
	}
	in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
	in.Target = tgt
	if _, err := Rollback(t.Context(), in); err == nil {
		t.Fatal("test setup: recovery must fail")
	}
	before := order(t, f.marker)

	rep, err := deploy(t, f, "my-app", "1.0.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.RecoveryRequired || rep.Committed || rep.AlreadyCurrent {
		t.Fatalf("deploy rep = %+v, want recovery-required refusal", rep)
	}
	if !strings.Contains(rep.FailureReason, "unresolved recovery marker") {
		t.Errorf("failure reason = %q", rep.FailureReason)
	}
	if got := order(t, f.marker); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Fatalf("deploy ran hooks during an unresolved recovery: %v → %v", before, got)
	}
	if !recoveryPresent(t, f) || !attemptPresent(t, f) {
		t.Fatal("deploy must not touch either marker")
	}
	requireNoLock(t, f)
	records := history(t, f)
	last := records[len(records)-1]
	if last.Type != "deploy.failed" || last.Data["recoveryRequired"] != true {
		t.Fatalf("history tail = %+v", last)
	}
}

func TestRollbackSelfHealRequiresWholeCommittedState(t *testing.T) {
	// operationId alone must not be enough to delete recovery evidence:
	// the observed release AND digest must also equal the recovery
	// marker's toRelease/toBundleDigest. A structurally valid but
	// semantically inconsistent state snapshot must never talk the
	// toolkit into treating a recovery as committed.
	f := newFixture(t, "my-app", versionTaggedHooks)
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
	before := order(t, f.marker)

	forgeState := func(t *testing.T, release, digest string) {
		t.Helper()
		st, err := f.target.ReadState(t.Context(), "my-app", "production")
		if err != nil {
			t.Fatal(err)
		}
		st.Current = &target.CurrentDeployment{
			Release:      release,
			BundleDigest: digest,
			Since:        st.Current.Since,
			OperationID:  "recovery:0123456789abcdef",
		}
		st.UpdatedAt = "2026-09-08T01:00:00Z"
		if err := f.target.WriteState(t.Context(), st); err != nil {
			t.Fatal(err)
		}
		if err := f.target.WriteRecovery(t.Context(), target.RecoveryMarker{
			RecoveryID:       "0123456789abcdef",
			SourceAttemptID:  "",
			Project:          "my-app",
			Environment:      "production",
			FromRelease:      "2.0.0",
			FromBundleDigest: relB.Bundle.Digest,
			ToRelease:        "1.0.0",
			ToBundleDigest:   relA0Digest(t, f),
			Authorization:    "manual",
			StartedAt:        "2026-09-08T00:30:00Z",
		}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("right operationId, wrong release", func(t *testing.T) {
		forgeState(t, "2.0.0", relB.Bundle.Digest)
		rep, err := Rollback(t.Context(), rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery))
		if err != nil {
			t.Fatal(err)
		}
		if rep.AlreadyRecovered || !rep.RecoveryRequired {
			t.Fatalf("rep = %+v, want refusal: operationId alone is not proof", rep)
		}
	})
	t.Run("right operationId, wrong digest", func(t *testing.T) {
		forgeState(t, "1.0.0", relB.Bundle.Digest)
		rep, err := Rollback(t.Context(), rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery))
		if err != nil {
			t.Fatal(err)
		}
		if rep.AlreadyRecovered || !rep.RecoveryRequired {
			t.Fatalf("rep = %+v, want refusal: digest must match too", rep)
		}
	})
	if got := order(t, f.marker); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Errorf("hooks ran during forged-state recoveries: %v → %v", before, got)
	}
	if !recoveryPresent(t, f) {
		t.Error("the forged-state refusals deleted the recovery marker")
	}
}

func TestRollbackCleanupReportsOriginalAuthority(t *testing.T) {
	// The cleanup invocation is bookkeeping; it must not rewrite the
	// historical authority of the operation that changed production.
	// Step 11 reconciliation decisions depend on reading the ORIGINAL
	// authorization from rollback.already-recovered evidence.
	f := newFixture(t, "my-app", versionTaggedHooks)
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

	// A MANUAL emergency recovery commits, then loses the recovery-marker
	// cleanup (its success evidence was persisted first, so the marker
	// remains as the healable breadcrumb).
	tr := &failingRunTransport{
		inner: local.New(),
		failRunWhen: func(argv []string) bool {
			return argv[0] == "rm" && strings.HasSuffix(argv[len(argv)-1], "/recoveries/production.json")
		},
	}
	tgt, err := target.New(tr, f.root)
	if err != nil {
		t.Fatal(err)
	}
	in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackManual)
	in.Target = tgt
	rep1, err := Rollback(t.Context(), in)
	if err == nil || !strings.Contains(err.Error(), "recovery marker could not be cleared") {
		t.Fatalf("err = %v, want recovery-cleanup failure", err)
	}
	if !rep1.Committed {
		t.Fatal("the state commit happened and must be reported")
	}

	// A RECOVERY-authorized invocation performs the cleanup: the evidence
	// must carry the marker's original identity and authority.
	rep2, err := Rollback(t.Context(), rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery))
	if err != nil {
		t.Fatal(err)
	}
	if !rep2.AlreadyRecovered {
		t.Fatalf("cleanup rep = %+v, want cleanup-only", rep2)
	}
	if rep2.RecoveryID != rep1.RecoveryID {
		t.Errorf("cleanup reported recoveryId %q, want the original %q", rep2.RecoveryID, rep1.RecoveryID)
	}
	records := history(t, f)
	last := records[len(records)-1]
	if last.Type != "rollback.already-recovered" {
		t.Fatalf("history tail = %+v", last)
	}
	if last.Data["authorization"] != "manual" {
		t.Errorf("already-recovered authorization = %#v, want the original manual", last.Data["authorization"])
	}
	if last.Data["recoveryId"] != rep1.RecoveryID {
		t.Errorf("already-recovered recoveryId = %#v, want %s", last.Data["recoveryId"], rep1.RecoveryID)
	}
	if recoveryPresent(t, f) {
		t.Error("cleanup did not remove the recovery marker")
	}
}

func TestRollbackHistoryFailureKeepsSelfHealAvailable(t *testing.T) {
	// The recovery marker is the LAST durable fact removed: after the
	// state commit and the attempt-marker clear, the success evidence is
	// persisted while the marker still exists. If the history write
	// fails, the marker remains and the next invocation self-heals
	// without hooks — production is restored, and nothing is replayed.
	f := newFixture(t, "my-app", versionTaggedHooks)
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

	tr := &failingRunTransport{
		inner: local.New(),
		failRunWhen: func(argv []string) bool {
			return argv[0] == "cat" && strings.HasSuffix(argv[len(argv)-1], "/history/production.jsonl")
		},
	}
	tgt, err := target.New(tr, f.root)
	if err != nil {
		t.Fatal(err)
	}
	in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
	in.Target = tgt
	rep, err := Rollback(t.Context(), in)
	if err == nil || !strings.Contains(err.Error(), "history outcome not recorded") {
		t.Fatalf("err = %v, want the evidence-write failure", err)
	}
	if !rep.Committed {
		t.Fatal("the state commit happened and must be reported")
	}
	if !recoveryPresent(t, f) {
		t.Fatal("the recovery marker must survive the evidence-write failure — it is the self-heal breadcrumb")
	}
	if attemptPresent(t, f) {
		t.Fatal("the attempt marker should be cleared before the evidence write (attempt-first order)")
	}
	before := order(t, f.marker)

	// The next invocation recognizes the proven commit and finishes the
	// bookkeeping without executing anything.
	rep2, err := Rollback(t.Context(), rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery))
	if err != nil {
		t.Fatal(err)
	}
	if !rep2.AlreadyRecovered || rep2.Committed {
		t.Fatalf("self-heal rep = %+v, want cleanup-only", rep2)
	}
	if got := order(t, f.marker); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Fatalf("the self-heal executed hooks: %v → %v", before, got)
	}
	if recoveryPresent(t, f) {
		t.Error("self-heal did not remove the recovery marker")
	}
	requireNoLock(t, f)
}

func TestRollbackFailsClosedOnIncompleteInput(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	revA := f.revision
	revB := secondRevision(t, f)

	cases := []struct {
		name string
		in   func() RollbackInput
		want string
	}{
		{"nil target", func() RollbackInput {
			in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
			in.Target = nil
			return in
		}, "Target is required"},
		{"nil target manifest", func() RollbackInput {
			in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
			in.TargetManifest = nil
			return in
		}, "TargetManifest is required"},
		{"nil environment", func() RollbackInput {
			in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
			in.Environment = nil
			return in
		}, "Environment is required"},
		{"nil from release", func() RollbackInput {
			in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
			in.FromRelease = nil
			return in
		}, "FromRelease is required"},
		{"nil from bundle", func() RollbackInput {
			in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
			in.FromBundle = nil
			return in
		}, "FromBundle is required"},
		{"nil to release", func() RollbackInput {
			in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
			in.ToRelease = nil
			return in
		}, "ToRelease is required"},
		{"nil to bundle", func() RollbackInput {
			in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
			in.ToBundle = nil
			return in
		}, "ToBundle is required"},
		{"unknown authorization", func() RollbackInput {
			in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", "")
			return in
		}, "Authorization"},
		{"same release", func() RollbackInput {
			return rollbackInput(t, f, revB, "2.0.0", revA, "2.0.0", RollbackRecovery)
		}, "nothing to restore"},
		{"environment pins neither release", func() RollbackInput {
			in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
			in.Environment.Spec.Release = ".deploy/releases/my-app-9.9.9.yaml"
			return in
		}, "neither the rollback source"},
		{"wrong target", func() RollbackInput {
			in := rollbackInput(t, f, revB, "2.0.0", revA, "1.0.0", RollbackRecovery)
			in.Environment.Spec.Target = "other-target"
			return in
		}, "targets"},
	}
	for _, c := range cases {
		if _, err := Rollback(t.Context(), c.in()); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	// Nothing touched the target: no lock, no history, no marker.
	if _, err := os.Stat(f.lockDir()); !os.IsNotExist(err) {
		t.Errorf("lock created despite incomplete input (stat err = %v)", err)
	}
	if len(history(t, f)) != 0 {
		t.Error("history written for refused input")
	}
	if attemptPresent(t, f) {
		t.Error("marker written for refused input")
	}
}
