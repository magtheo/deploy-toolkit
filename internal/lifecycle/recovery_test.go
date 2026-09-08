package lifecycle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/target"
	"github.com/magtheo/deploy-toolkit/internal/transport/local"
)

func mustRelease(t *testing.T, f *fixture, project, version string) *manifest.Release {
	t.Helper()
	rel := f.release(project, version)
	if rel == nil {
		t.Fatal("release build failed")
	}
	return rel
}

func mustBytes(t *testing.T, f *fixture) []byte {
	t.Helper()
	res, err := f.bundler.BuildFromRevision(t.Context(), f.revision)
	if err != nil {
		t.Fatal(err)
	}
	return res.Bytes
}

// attemptPath is where the durable unresolved-attempt marker lives.
func (f *fixture) attemptPath() string {
	return f.root + "/my-app/attempts/production.json"
}

func attemptPresent(t *testing.T, f *fixture) bool {
	t.Helper()
	_, err := f.target.ReadAttempt(t.Context(), "my-app", "production")
	if err == nil {
		return true
	}
	if errors.Is(err, target.ErrAttemptAbsent) {
		return false
	}
	t.Fatalf("read attempt marker: %v", err)
	return false
}

func TestDeployUncertainCommitRetryIsRefused(t *testing.T) {
	// verify succeeds, then the observed-state write fails: the service
	// may be on the new release while observed state says otherwise.
	// The attempt marker must survive (uncertain outcome), and a retry
	// must refuse instead of re-running migrate/apply into an unknown
	// target.
	f := newFixture(t, "my-app", nil)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")
	tr := &failingRunTransport{
		inner:       local.New(),
		failPutWhen: func(path string) bool { return strings.HasSuffix(path, "/state/production.json") },
	}
	tgt, err := target.New(tr, f.root)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Deploy(t.Context(), DeployInput{
		Target: tgt, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "test",
	})
	if err == nil || !strings.Contains(err.Error(), "observed state commit failed") {
		t.Fatalf("err = %v, want an uncertain-commit infrastructure error", err)
	}
	if rep.Committed {
		t.Error("rep must not claim commit")
	}
	if !attemptPresent(t, f) {
		t.Fatal("uncertain outcome must leave the attempt marker behind")
	}
	firstRun := order(t, f.marker)

	// Retry with a healthy transport: normal retry is REFUSED.
	rep2, err := deploy(t, f, "my-app", "1.0.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rep2.RecoveryRequired || rep2.AlreadyCurrent || rep2.Committed {
		t.Errorf("retry rep = %+v, want recovery-required refusal", rep2)
	}
	if !strings.Contains(rep2.FailureReason, "unresolved deployment attempt") {
		t.Errorf("failure reason = %q", rep2.FailureReason)
	}
	if got := order(t, f.marker); strings.Join(got, ",") != strings.Join(firstRun, ",") {
		t.Errorf("refused retry re-ran hooks: first %v, retry %v", firstRun, got)
	}
	records := history(t, f)
	if len(records) != 2 || records[1].Type != "deploy.failed" || records[1].Data["recoveryRequired"] != true {
		t.Fatalf("history = %+v", records)
	}
	requireNoLock(t, f)

	// Explicit recovery (operator/step 10) resolves the attempt.
	if err := f.target.ClearAttempt(t.Context(), "my-app", "production"); err != nil {
		t.Fatal(err)
	}
	if attemptPresent(t, f) {
		t.Error("cleared marker still present")
	}
}

func TestDeployTransportLossDuringApplyLeavesUnresolvedAttempt(t *testing.T) {
	// A transport error during apply does not prove the remote command
	// performed zero side effects. The outcome is uncertain: the marker
	// stays, and the next deployment refuses.
	f := newFixture(t, "my-app", nil)
	rel, bundleBytes := f.preparedBytes(t, "my-app", "1.0.0")
	tr := &failingRunTransport{
		inner:    local.New(),
		failWhen: func(argv0 string) bool { return strings.Contains(argv0, "apply.sh") },
	}
	tgt, err := target.New(tr, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Deploy(t.Context(), DeployInput{
		Target: tgt, TargetManifest: f.targetManifest(),
		Environment: f.environment("my-app", "1.0.0"), Release: rel, Bundle: bundleBytes, Owner: "test",
	}); err == nil {
		t.Fatal("transport loss must be an error")
	}
	if !attemptPresent(t, f) {
		t.Fatal("uncertain apply outcome must leave the attempt marker behind")
	}
	// Determined hook failures clear the marker; transport loss must not.
	rep, err := deploy(t, f, "my-app", "1.0.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.RecoveryRequired {
		t.Errorf("retry rep = %+v, want refusal", rep)
	}
}

func TestDeployAlreadyCurrentSelfHealsStaleMarker(t *testing.T) {
	// Crash window: observed state committed, attempt marker not yet
	// cleared. The next deployment recognizes the trusted terminal and
	// heals the leftover instead of demanding recovery.
	f := newFixture(t, "my-app", nil)
	if _, err := deploy(t, f, "my-app", "1.0.0", nil); err != nil {
		t.Fatal(err)
	}
	if err := f.target.WriteAttempt(t.Context(), target.AttemptMarker{
		AttemptID:    "0123456789abcdef",
		Project:      "my-app",
		Environment:  "production",
		ToRelease:    "1.0.0",
		BundleDigest: "sha256:" + strings.Repeat("aa", 32),
		StartedAt:    time.Unix(1700000000, 0).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := deploy(t, f, "my-app", "1.0.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.AlreadyCurrent || rep.RecoveryRequired {
		t.Errorf("rep = %+v, want self-healed already-current", rep)
	}
	if attemptPresent(t, f) {
		t.Error("stale marker was not healed")
	}
}

func TestVerifyStageDetectsMutatedHookBytes(t *testing.T) {
	// A staged release whose bytes were altered after staging must never
	// be executed from. First attempt: verify fails (determined outcome,
	// marker cleared, no commit). Then the staged verify.sh is mutated.
	// The retry's Stage hits the already-staged path — which now proves
	// the directory still matches the canonical bundle and refuses.
	f := newFixture(t, "my-app", func(marker, envFile, script string) string {
		if script == "verify" {
			return "#!/bin/sh\nexit 1\n"
		}
		return fmt.Sprintf("#!/bin/sh\necho %s >> %s\n", script, marker)
	})
	rep, err := deploy(t, f, "my-app", "1.0.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.FailureReason != "verify failed" || rep.Committed {
		t.Fatalf("first attempt rep = %+v", rep)
	}

	stagedVerify := filepath.Join(f.root, "my-app/releases/1.0.0/deploy/verify.sh")
	if err := os.WriteFile(stagedVerify, []byte("#!/bin/sh\necho TAMPERED >> "+f.marker+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err = deploy(t, f, "my-app", "1.0.0", nil)
	if err == nil || !strings.Contains(err.Error(), "no longer matches its marker") {
		t.Fatalf("retry err = %v, want staged-material verification failure", err)
	}
	for _, l := range order(t, f.marker) {
		if l == "TAMPERED" {
			t.Fatal("altered hook bytes were executed")
		}
	}
	if l := order(t, f.marker); len(l) > 0 && l[len(l)-1] == "verify" {
		t.Errorf("verify hook re-ran despite altered staged material: %v", l)
	}
	// The direct API contract, for rollback reuse:
	if err := f.target.VerifyStage(t.Context(), mustRelease(t, f, "my-app", "1.0.0"), mustBytes(t, f)); err == nil {
		t.Error("VerifyStage accepted altered staged material")
	}
}

func TestAttemptMarkerValidation(t *testing.T) {
	f := newFixture(t, "my-app", nil)
	valid := target.AttemptMarker{
		AttemptID:    "0123456789abcdef",
		Project:      "my-app",
		Environment:  "production",
		FromRelease:  "0.9.0",
		ToRelease:    "1.0.0",
		BundleDigest: "sha256:" + strings.Repeat("ab", 32),
		StartedAt:    time.Unix(1700000000, 0).UTC().Format(time.RFC3339),
	}
	if err := f.target.WriteAttempt(t.Context(), valid); err != nil {
		t.Fatal(err)
	}
	got, err := f.target.ReadAttempt(t.Context(), "my-app", "production")
	if err != nil || got.AttemptID != "0123456789abcdef" || got.FromRelease != "0.9.0" {
		t.Fatalf("roundtrip = %+v, %v", got, err)
	}

	bad := []func(*target.AttemptMarker){
		func(m *target.AttemptMarker) { m.AttemptID = "xyz" },
		func(m *target.AttemptMarker) { m.ToRelease = "not-semver" },
		func(m *target.AttemptMarker) { m.FromRelease = "../evil" },
		func(m *target.AttemptMarker) { m.BundleDigest = "garbage" },
		func(m *target.AttemptMarker) { m.StartedAt = "yesterday" },
	}
	for i, mutate := range bad {
		m := valid
		mutate(&m)
		if err := f.target.WriteAttempt(t.Context(), m); err == nil {
			t.Errorf("case %d: invalid marker accepted", i)
		}
	}
	// A corrupt on-disk marker is an error, not absence.
	if err := os.WriteFile(f.attemptPath(), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.target.ReadAttempt(t.Context(), "my-app", "production"); err == nil || errors.Is(err, target.ErrAttemptAbsent) {
		t.Errorf("corrupt marker err = %v, want fail-closed error", err)
	}
}
