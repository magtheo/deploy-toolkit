package promotion

import (
	"context"
	"fmt"
	"strings"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/release"
)

// Classification is the four-state verdict of the promotion classifier.
// The states are a contract of the promotion-aware CI path
// (docs/promotion-ci-plan.md): the classifier reports, the gate decides.
type Classification string

const (
	ClassificationPromotion Classification = "PROMOTION"
	ClassificationOrdinary  Classification = "ORDINARY"
	ClassificationInvalid   Classification = "INVALID"
	ClassificationError     Classification = "ERROR"
)

// Mode selects the freshness rules applied to promotion attempts. Ordinary
// transitions are never subject to them.
type Mode string

const (
	ModePR   Mode = "pr"   // base = trusted base SHA, head = PR head SHA
	ModePush Mode = "push" // base = push.before, head = push.after
)

// zeroSHA is the all-zeros object name GitHub uses for branch creation
// (before) and deletion (after) push events.
const zeroSHA = "0000000000000000000000000000000000000000"

type ClassifyInput struct {
	Repo    string
	Mode    Mode
	Base    string
	Head    string
	RepoDir string
}

type ClassifyOutcome struct {
	Classification Classification
	Reason         string
	Environment    string
	From           string
	To             string
	Messages       []string
}

// Classify decides whether one transition is a promotion attempt and, if so,
// whether it is a valid promotion. Intent is detected semantically before any
// promotion rule runs: a transition is promotion-sensitive when it changes an
// environment spec.release pointer or mutates the release-manifest namespace.
// Everything else is ORDINARY and never touches CAS, evidence, or topology
// rules. Read and infrastructure failures are the ERROR state, never a
// policy verdict — the classifier reports, the gate decides.
func Classify(ctx context.Context, in ClassifyInput, src Store, resolver release.Resolver, bundler release.Bundler) (*ClassifyOutcome, error) {
	if in.Repo == "" || in.Base == "" || in.Head == "" {
		return nil, fmt.Errorf("--repo, --base and --head are required")
	}
	if in.Mode != ModePR && in.Mode != ModePush {
		return nil, fmt.Errorf("--mode must be pr or push")
	}

	state := func(c Classification, format string, args ...any) *ClassifyOutcome {
		return &ClassifyOutcome{Classification: c, Reason: fmt.Sprintf(format, args...)}
	}

	liveHead, err := src.BranchHead(ctx, in.Repo, release.TrustedBranch)
	if err != nil {
		return state(ClassificationError, "resolve trusted branch head: %v", err), nil
	}

	baseZero, headZero := in.Base == zeroSHA, in.Head == zeroSHA
	if baseZero && headZero {
		return state(ClassificationError, "transition identities are both zero SHAs"), nil
	}

	baseLeaves := map[string]TreeLeaf{}
	if !baseZero {
		baseLeaves, err = src.CommitTreeLeaves(ctx, in.Repo, in.Base)
		if err != nil {
			return state(ClassificationError, "read base tree: %v", err), nil
		}
	}
	headLeaves := map[string]TreeLeaf{}
	if !headZero {
		headLeaves, err = src.CommitTreeLeaves(ctx, in.Repo, in.Head)
		if err != nil {
			return state(ClassificationError, "read head tree: %v", err), nil
		}
	}

	changed := treeDiff(baseLeaves, headLeaves)

	intent, reason, err := detectIntent(ctx, in, src, changed)
	if err != nil {
		return state(ClassificationError, "detect promotion intent: %v", err), nil
	}
	if !intent {
		return state(ClassificationOrdinary, "%s", reason), nil
	}

	if baseZero || headZero {
		return state(ClassificationInvalid, "transition involves a zero SHA (branch creation or deletion); it is never a promotion"), nil
	}

	switch in.Mode {
	case ModePR:
		if in.Base != liveHead {
			return state(ClassificationInvalid, "base %s is not the current %s head (%s); the proposal is stale and must be regenerated", in.Base, release.TrustedBranch, liveHead), nil
		}
		parents, err := src.CommitParents(ctx, in.Repo, in.Head)
		if err != nil {
			return state(ClassificationError, "read commit parents: %v", err), nil
		}
		if len(parents) != 1 || parents[0] != liveHead {
			return state(ClassificationInvalid, "promotion head %s must be exactly one commit on top of the current %s head", in.Head, release.TrustedBranch), nil
		}
	case ModePush:
		if in.Head != liveHead {
			return state(ClassificationInvalid, "head %s is not the current %s head (%s); refusing to classify a stale transition as a promotion", in.Head, release.TrustedBranch, liveHead), nil
		}
		ancestor, err := src.IsAncestor(ctx, in.Repo, in.Base, in.Head)
		if err != nil {
			return state(ClassificationError, "check ancestry: %v", err), nil
		}
		if !ancestor {
			return state(ClassificationInvalid, "base %s is not an ancestor of head %s; history rewrites never classify as promotions", in.Base, in.Head), nil
		}
	}

	pol, err := evaluatePolicy(ctx, policyInput{Repo: in.Repo, Base: in.Base, Head: in.Head, RepoDir: in.RepoDir},
		diffTrees{baseLeaves: baseLeaves, headLeaves: headLeaves}, src, resolver, bundler)
	if err != nil {
		return state(ClassificationError, "%v", err), nil
	}
	if !pol.Passed {
		return &ClassifyOutcome{
			Classification: ClassificationInvalid,
			Reason:         "not a valid promotion transition",
			Environment:    pol.Environment,
			From:           pol.From,
			To:             pol.To,
			Messages:       pol.Messages,
		}, nil
	}
	return &ClassifyOutcome{
		Classification: ClassificationPromotion,
		Reason:         "valid promotion transition",
		Environment:    pol.Environment,
		From:           pol.From,
		To:             pol.To,
	}, nil
}

// detectIntent applies the promotion-intent predicate: a transition is
// promotion-sensitive when it mutates the release-manifest namespace, or when
// a modified environment file semantically changes spec.release. Environment
// files that are added or removed change no pointer and stay ordinary; a
// modified environment file whose contents cannot be interpreted is treated
// as an attempt (fail closed on authority), because the absence of a pointer
// transition cannot be established.
func detectIntent(ctx context.Context, in ClassifyInput, src Store, changed []ChangedFile) (bool, string, error) {
	for _, f := range changed {
		if strings.HasPrefix(f.Path, ReleasesDir+"/") {
			return true, fmt.Sprintf("release-manifest namespace mutated (%s %s)", f.Status, f.Path), nil
		}
	}
	for _, f := range changed {
		if !strings.HasPrefix(f.Path, EnvironmentsDir+"/") || f.Status != "modified" {
			continue
		}
		baseBytes, err := src.FileAt(ctx, in.Repo, f.Path, in.Base)
		if err != nil {
			return false, "", err
		}
		headBytes, err := src.FileAt(ctx, in.Repo, f.Path, in.Head)
		if err != nil {
			return false, "", err
		}
		baseRes, baseErr := manifest.Parse(baseBytes, manifest.KindEnvironment)
		headRes, headErr := manifest.Parse(headBytes, manifest.KindEnvironment)
		if baseErr != nil || headErr != nil {
			return true, fmt.Sprintf("environment file %s changed but could not be interpreted; treating it as a promotion attempt (fail closed)", f.Path), nil
		}
		if baseRes.Environment.Spec.Release != headRes.Environment.Spec.Release {
			return true, fmt.Sprintf("spec.release transition in %s (%s → %s)", f.Path, baseRes.Environment.Spec.Release, headRes.Environment.Spec.Release), nil
		}
	}
	return false, "no promotion intent: no spec.release transition and no release-manifest namespace change", nil
}
