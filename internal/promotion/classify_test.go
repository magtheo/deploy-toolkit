package promotion

import (
	"context"
	"strings"
	"testing"

	"github.com/magtheo/deploy-toolkit/internal/release"
)

// pushFixture is a checkFixture whose trusted-branch head is the pushed
// commit: in push mode, head (push.after) must be the live head and base
// (push.before) the state before the push. Eligibility reads policy from the
// live head, so the head ref must carry the project manifest too.
func pushFixture(t *testing.T) (release.Bundler, *fakeStore, *fakeResolver, string) {
	t.Helper()
	bundler, store, res, dir := checkFixture(t)
	store.heads[release.TrustedBranch] = headSHA
	store.files[headSHA][release.ProjectPath] = projectDoc()
	return bundler, store, res, dir
}

type classifyWant struct {
	classification  Classification
	reasonContains  string
	messagesContain string
}

func classifyOnce(t *testing.T, store *fakeStore, res *fakeResolver, bundler release.Bundler, dir string, in ClassifyInput) *ClassifyOutcome {
	t.Helper()
	out, err := Classify(context.Background(), in, store, res, bundler)
	if err != nil {
		t.Fatalf("Classify returned usage error: %v", err)
	}
	return out
}

func (w classifyWant) verify(t *testing.T, out *ClassifyOutcome) {
	t.Helper()
	if out.Classification != w.classification {
		t.Fatalf("classification = %s (%s), want %s", out.Classification, out.Reason, w.classification)
	}
	if w.reasonContains != "" && !strings.Contains(out.Reason, w.reasonContains) {
		t.Fatalf("reason %q does not contain %q", out.Reason, w.reasonContains)
	}
	if w.messagesContain != "" && !strings.Contains(strings.Join(out.Messages, "; "), w.messagesContain) {
		t.Fatalf("messages %q do not contain %q", strings.Join(out.Messages, "; "), w.messagesContain)
	}
}

// TestClassifyPushModeTopology covers plan test item 1: push-mode transitions
// are classified by content (before → after), never by commit topology.
func TestClassifyPushModeTopology(t *testing.T) {
	t.Run("merge commit with two parents is PROMOTION", func(t *testing.T) {
		bundler, store, res, dir := pushFixture(t)
		store.parents[headSHA] = []string{checkBaseSHA, "feature-tip"}
		out := classifyOnce(t, store, res, bundler, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePush, Base: checkBaseSHA, Head: headSHA, RepoDir: dir})
		classifyWant{classification: ClassificationPromotion}.verify(t, out)
		if out.Environment != "production" || out.From != oldRelConst || out.To != relPathConst {
			t.Errorf("transition facts = %s %s → %s", out.Environment, out.From, out.To)
		}
	})
	t.Run("rebase multi-commit transition is content-based", func(t *testing.T) {
		bundler, store, res, dir := pushFixture(t)
		store.parents[headSHA] = []string{"intermediate-1"}
		store.parents["intermediate-1"] = []string{"intermediate-2"}
		store.parents["intermediate-2"] = []string{checkBaseSHA}
		out := classifyOnce(t, store, res, bundler, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePush, Base: checkBaseSHA, Head: headSHA, RepoDir: dir})
		classifyWant{classification: ClassificationPromotion}.verify(t, out)
	})
	t.Run("stale after is INVALID", func(t *testing.T) {
		bundler, store, res, dir := pushFixture(t)
		store.heads[release.TrustedBranch] = "even-newer"
		out := classifyOnce(t, store, res, bundler, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePush, Base: checkBaseSHA, Head: headSHA, RepoDir: dir})
		classifyWant{classification: ClassificationInvalid, reasonContains: "not the current"}.verify(t, out)
	})
	t.Run("force push is INVALID", func(t *testing.T) {
		bundler, store, res, dir := pushFixture(t)
		store.anc = false
		out := classifyOnce(t, store, res, bundler, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePush, Base: checkBaseSHA, Head: headSHA, RepoDir: dir})
		classifyWant{classification: ClassificationInvalid, reasonContains: "ancestor"}.verify(t, out)
	})
	t.Run("zero before (branch creation) is INVALID", func(t *testing.T) {
		bundler, store, res, dir := pushFixture(t)
		out := classifyOnce(t, store, res, bundler, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePush, Base: zeroSHA, Head: headSHA, RepoDir: dir})
		classifyWant{classification: ClassificationInvalid, reasonContains: "zero SHA"}.verify(t, out)
	})
	t.Run("zero after (branch deletion) is INVALID", func(t *testing.T) {
		bundler, store, res, dir := pushFixture(t)
		out := classifyOnce(t, store, res, bundler, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePush, Base: checkBaseSHA, Head: zeroSHA, RepoDir: dir})
		classifyWant{classification: ClassificationInvalid, reasonContains: "zero SHA"}.verify(t, out)
	})
	t.Run("ordinary push is ORDINARY", func(t *testing.T) {
		bundler, store, res, dir := pushFixture(t)
		delete(store.trees[headSHA], relPathConst) // no release change
		store.files[headSHA][envPathConst] = envDoc(oldRelConst)
		store.blobs["envblob-head"] = envDoc(oldRelConst)
		store.trees[headSHA][".github/workflows/ci.yml"] = "ciblob"
		store.blobs["ciblob"] = []byte("on: push")
		out := classifyOnce(t, store, res, bundler, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePush, Base: checkBaseSHA, Head: headSHA, RepoDir: dir})
		classifyWant{classification: ClassificationOrdinary, reasonContains: "no promotion intent"}.verify(t, out)
	})
}

// TestClassifyIntentOrdering covers plan test item 2: intent is detected
// before any promotion rule, so ordinary topology and administrative
// environment changes never become INVALID — and release-namespace mutation
// is always promotion-sensitive.
func TestClassifyIntentOrdering(t *testing.T) {
	t.Run("multi-commit ordinary PR stays ORDINARY (CAS not applied)", func(t *testing.T) {
		bundler, store, res, dir := checkFixture(t)
		delete(store.trees[headSHA], relPathConst) // no release change
		store.files[headSHA][envPathConst] = envDoc(oldRelConst)
		store.blobs["envblob-head"] = envDoc(oldRelConst)
		store.parents[headSHA] = []string{checkBaseSHA, "other"} // multi-commit head
		out := classifyOnce(t, store, res, bundler, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePR, Base: checkBaseSHA, Head: headSHA, RepoDir: dir})
		classifyWant{classification: ClassificationOrdinary}.verify(t, out)
	})
	t.Run("stale-base ordinary PR stays ORDINARY (CAS not applied)", func(t *testing.T) {
		bundler, store, res, dir := checkFixture(t)
		delete(store.trees[headSHA], relPathConst)
		store.files[headSHA][envPathConst] = envDoc(oldRelConst)
		store.blobs["envblob-head"] = envDoc(oldRelConst)
		store.heads[release.TrustedBranch] = "moved-on"
		out := classifyOnce(t, store, res, bundler, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePR, Base: checkBaseSHA, Head: headSHA, RepoDir: dir})
		classifyWant{classification: ClassificationOrdinary}.verify(t, out)
	})
	t.Run("spec.target-only environment change is ORDINARY", func(t *testing.T) {
		bundler, store, res, dir := checkFixture(t)
		store.trees[checkBaseSHA] = map[string]string{release.ProjectPath: "pblob", envPathConst: "envblob-base", oldRelConst: "oldrelblob"}
		store.trees[headSHA] = map[string]string{release.ProjectPath: "pblob", envPathConst: "envblob-head-target", oldRelConst: "oldrelblob"}
		swapped := strings.Replace(string(envDoc(oldRelConst)), "target: production-primary", "target: other-target", 1)
		store.files[headSHA] = map[string][]byte{envPathConst: []byte(swapped)}
		store.blobs["envblob-base"] = envDoc(oldRelConst)
		store.blobs["envblob-head-target"] = []byte(swapped)
		out := classifyOnce(t, store, res, bundler, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePR, Base: checkBaseSHA, Head: headSHA, RepoDir: dir})
		classifyWant{classification: ClassificationOrdinary}.verify(t, out)
	})
	t.Run("environment file removed is ORDINARY", func(t *testing.T) {
		bundler, store, res, dir := checkFixture(t)
		delete(store.trees[headSHA], relPathConst)
		delete(store.trees[headSHA], envPathConst)
		store.files[headSHA] = map[string][]byte{}
		out := classifyOnce(t, store, res, bundler, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePR, Base: checkBaseSHA, Head: headSHA, RepoDir: dir})
		classifyWant{classification: ClassificationOrdinary}.verify(t, out)
	})
	t.Run("uninterpretable modified environment file is promotion-sensitive", func(t *testing.T) {
		bundler, store, res, dir := checkFixture(t)
		delete(store.trees[headSHA], relPathConst)
		store.files[headSHA][envPathConst] = []byte("::: not yaml at all")
		store.blobs["envblob-head"] = []byte("::: not yaml at all")
		out := classifyOnce(t, store, res, bundler, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePR, Base: checkBaseSHA, Head: headSHA, RepoDir: dir})
		classifyWant{classification: ClassificationInvalid}.verify(t, out)
	})
	t.Run("lone release addition without a flip is INVALID", func(t *testing.T) {
		bundler, store, res, dir := checkFixture(t)
		store.trees[headSHA][".deploy/releases/my-app-0.2.0.yaml"] = "relblob"
		store.files[headSHA][envPathConst] = envDoc(oldRelConst)
		store.blobs["envblob-head"] = envDoc(oldRelConst)
		out := classifyOnce(t, store, res, bundler, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePR, Base: checkBaseSHA, Head: headSHA, RepoDir: dir})
		classifyWant{classification: ClassificationInvalid, reasonContains: "not a valid promotion transition"}.verify(t, out)
	})
	t.Run("existing release modification is INVALID", func(t *testing.T) {
		bundler, store, res, dir := checkFixture(t)
		delete(store.trees[headSHA], relPathConst)
		store.trees[headSHA][oldRelConst] = "tampered-oldrel"
		store.files[headSHA][envPathConst] = envDoc(oldRelConst)
		store.blobs["envblob-head"] = envDoc(oldRelConst)
		store.blobs["tampered-oldrel"] = []byte("apiVersion: deploy.toolkit/v1\nkind: Release\n")
		out := classifyOnce(t, store, res, bundler, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePR, Base: checkBaseSHA, Head: headSHA, RepoDir: dir})
		classifyWant{classification: ClassificationInvalid, messagesContain: "releases are immutable"}.verify(t, out)
	})
	t.Run("existing release removal is INVALID", func(t *testing.T) {
		bundler, store, res, dir := checkFixture(t)
		delete(store.trees[headSHA], relPathConst)
		delete(store.trees[headSHA], oldRelConst)
		store.files[headSHA][envPathConst] = envDoc(oldRelConst)
		store.blobs["envblob-head"] = envDoc(oldRelConst)
		out := classifyOnce(t, store, res, bundler, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePR, Base: checkBaseSHA, Head: headSHA, RepoDir: dir})
		classifyWant{classification: ClassificationInvalid, messagesContain: "releases are immutable"}.verify(t, out)
	})
	t.Run("source change plus spec.release flip is INVALID", func(t *testing.T) {
		bundler, store, res, dir := checkFixture(t)
		store.trees[headSHA]["src/app.ts"] = "srcblob"
		store.blobs["srcblob"] = []byte("export {}\n")
		out := classifyOnce(t, store, res, bundler, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePR, Base: checkBaseSHA, Head: headSHA, RepoDir: dir})
		classifyWant{classification: ClassificationInvalid, reasonContains: "not a valid promotion transition"}.verify(t, out)
	})
	t.Run("read failure is ERROR, never a policy verdict", func(t *testing.T) {
		_, store, _, dir := checkFixture(t)
		delete(store.trees, headSHA) // tree read fails
		out := classifyOnce(t, store, nil, nil, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePR, Base: checkBaseSHA, Head: headSHA})
		classifyWant{classification: ClassificationError, reasonContains: "read head tree"}.verify(t, out)
	})
	t.Run("rollback environment-only flip on push is PROMOTION without registry", func(t *testing.T) {
		_, store, _, dir := pushFixture(t)
		store.trees[checkBaseSHA] = map[string]string{
			release.ProjectPath: "pblob",
			envPathConst:        "envblob-base",
			relPathConst:        "relblob",
			oldRelConst:         "oldrelblob",
		}
		store.trees[headSHA] = map[string]string{
			release.ProjectPath: "pblob",
			envPathConst:        "envblob-head",
			relPathConst:        "relblob",
			oldRelConst:         "oldrelblob",
		}
		store.files[checkBaseSHA][envPathConst] = envDoc(relPathConst)
		store.files[headSHA][envPathConst] = envDoc(oldRelConst)
		store.blobs["envblob-base"] = envDoc(relPathConst)
		store.blobs["envblob-head"] = envDoc(oldRelConst)
		out := classifyOnce(t, store, nil, nil, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePush, Base: checkBaseSHA, Head: headSHA})
		classifyWant{classification: ClassificationPromotion}.verify(t, out)
	})
	t.Run("stale-base promotion attempt is INVALID", func(t *testing.T) {
		bundler, store, res, dir := checkFixture(t)
		store.heads[release.TrustedBranch] = "moved-on"
		out := classifyOnce(t, store, res, bundler, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePR, Base: checkBaseSHA, Head: headSHA, RepoDir: dir})
		classifyWant{classification: ClassificationInvalid, reasonContains: "stale"}.verify(t, out)
	})
	t.Run("multi-commit promotion attempt is INVALID", func(t *testing.T) {
		bundler, store, res, dir := checkFixture(t)
		store.parents[headSHA] = []string{checkBaseSHA, "other"}
		out := classifyOnce(t, store, res, bundler, dir, ClassifyInput{Repo: "example/my-app", Mode: ModePR, Base: checkBaseSHA, Head: headSHA, RepoDir: dir})
		classifyWant{classification: ClassificationInvalid, reasonContains: "exactly one commit"}.verify(t, out)
	})
}

// TestClassifyInvalidIsNotUsageError pins the CLI-consumable distinction:
// INVALID is a determined classification (with messages), distinct from
// input validation errors, which return an error to the caller. A missing
// checkout for new-release evidence is likewise a determined INVALID (fail
// closed on authority), not a usage error — mirroring Check.
func TestClassifyInvalidIsNotUsageError(t *testing.T) {
	bundler, store, res, dir := checkFixture(t)
	store.trees[headSHA]["src/app.ts"] = "srcblob"
	store.blobs["srcblob"] = []byte("export {}\n")
	out, err := Classify(context.Background(), ClassifyInput{Repo: "example/my-app", Mode: ModePR, Base: checkBaseSHA, Head: headSHA, RepoDir: dir}, store, res, bundler)
	if err != nil {
		t.Fatal(err)
	}
	if out.Classification != ClassificationInvalid || len(out.Messages) == 0 {
		t.Fatalf("outcome = %+v", out)
	}
	noCheckout, err := Classify(context.Background(), ClassifyInput{Repo: "example/my-app", Mode: ModePR, Base: checkBaseSHA, Head: headSHA}, store, res, bundler)
	if err != nil {
		t.Fatal(err)
	}
	classifyWant{classification: ClassificationInvalid, messagesContain: "without a checkout"}.verify(t, noCheckout)
	if _, err := Classify(context.Background(), ClassifyInput{Repo: "example/my-app", Mode: "bogus", Base: checkBaseSHA, Head: headSHA}, store, res, bundler); err == nil {
		t.Error("unknown mode must be a usage error")
	}
	if _, err := Classify(context.Background(), ClassifyInput{Repo: "example/my-app", Mode: ModePR, Base: checkBaseSHA}, store, res, bundler); err == nil {
		t.Error("missing head must be a usage error")
	}
}
