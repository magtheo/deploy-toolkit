package main

// Contract tests for the reusable promotion-classification workflow.
// Like deploy.yml, promotion.yml is part of Consumer Contract v1: these
// tests pin its SEMANTIC invariants — zero secrets, least privilege,
// full-SHA anchoring, the always-success output contract ("the classifier
// reports; the gate decides"), and never-executes-PR-code — never
// brittle byte snapshots of the YAML.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const promotionWorkflowRelPath = ".github/workflows/promotion.yml"

func loadPromotionWorkflow(t *testing.T) (*workflowFile, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", promotionWorkflowRelPath))
	if err != nil {
		t.Fatal(err)
	}
	var wf workflowFile
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("workflow YAML does not parse: %v", err)
	}
	return &wf, string(raw)
}

func promotionClassifyStep(t *testing.T, raw string) map[string]any {
	t.Helper()
	wf := &workflowFile{}
	if err := yaml.Unmarshal([]byte(raw), wf); err != nil {
		t.Fatal(err)
	}
	return findStep(t, wf.Jobs["classify"], "classify")
}

// The workflow is invokable as a reusable workflow with the contract
// inputs (all optional overrides), the three contract outputs mapped
// from the classify job, and — unlike deploy.yml — NO secret inputs:
// the classifier reads with the implicit token only.
func TestPromotionWorkflowIsReusableWithContractOutputs(t *testing.T) {
	wf, _ := loadPromotionWorkflow(t)
	call := wf.On.WorkflowCall
	for _, name := range []string{"base", "head", "repo_dir"} {
		in, ok := call.Inputs[name]
		if !ok || in.Required {
			t.Errorf("input %q must exist and be optional (event-derived defaults)", name)
		}
	}
	for _, banned := range []string{"toolkit_ref", "ref"} {
		if _, ok := call.Inputs[banned]; ok {
			t.Errorf("input %q must not exist — the workflow's own SHA anchors the machinery", banned)
		}
	}
	if len(call.Secrets) != 0 {
		t.Errorf("secret inputs = %v, want none — classification needs zero secrets", call.Secrets)
	}
	for name, want := range map[string]string{
		"promotion_only": "${{ jobs.classify.outputs.promotion_only }}",
		"classification": "${{ jobs.classify.outputs.classification }}",
		"reason":         "${{ jobs.classify.outputs.reason }}",
	} {
		out, ok := call.Outputs[name]
		if !ok || out.Value != want {
			t.Errorf("workflow_call.outputs.%s = %+v, want %s", name, out, want)
		}
	}
	job := wf.Jobs["classify"]
	for name, want := range map[string]string{
		"promotion_only": "${{ steps.classify.outputs.promotion_only }}",
		"classification": "${{ steps.classify.outputs.classification }}",
		"reason":         "${{ steps.classify.outputs.reason }}",
	} {
		if got, ok := job.Outputs[name]; !ok || got != want {
			t.Errorf("jobs.classify.outputs.%s = %q, want %s", name, got, want)
		}
	}
}

// Zero secrets end to end: no secret input, no secrets.* reference —
// classification reads with the implicit GITHUB_TOKEN only.
func TestPromotionWorkflowZeroSecrets(t *testing.T) {
	_, raw := loadPromotionWorkflow(t)
	if strings.Contains(raw, "secrets.") {
		t.Error("workflow must not reference any secret — classification needs none")
	}
	if !strings.Contains(raw, "GITHUB_TOKEN: ${{ github.token }}") {
		t.Error("classify step must authenticate with the implicit github.token")
	}
}

// Full-SHA anchoring, mirroring deploy.yml: every action pinned by a full
// 40-hex SHA, deployctl rebuilt from job.workflow_repository@job.workflow_sha
// (the workflow's OWN commit), and a fail-closed floating-ref gate.
func TestPromotionWorkflowAnchoredByItsOwnSHA(t *testing.T) {
	_, raw := loadPromotionWorkflow(t)
	if strings.Contains(raw, "toolkit_ref") {
		t.Error("toolkit_ref must not exist anywhere — a duplicated caller-controlled pin defeats the trust anchor")
	}
	pins := regexp.MustCompile(`uses: ([\w.-]+/[\w.-]+)@(\S+)`).FindAllStringSubmatch(raw, -1)
	if len(pins) == 0 {
		t.Fatal("no action pins found")
	}
	for _, pin := range pins {
		if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(pin[2]) {
			t.Errorf("%s pinned by %q — actions must be pinned by full commit SHA", pin[1], pin[2])
		}
	}
	jt := jobText(t, raw, "classify")
	if !strings.Contains(jt, "repository: ${{ job.workflow_repository }}") || !strings.Contains(jt, "ref: ${{ job.workflow_sha }}") {
		t.Error("classify job must build deployctl from job.workflow_repository@job.workflow_sha")
	}
	if !strings.Contains(jt, "WORKFLOW_REF: ${{ job.workflow_ref }}") || !strings.Contains(jt, "floating ref is not a trust anchor") {
		t.Error("classify job must fail closed on a floating workflow invocation ref")
	}
	if got := strings.Count(raw, "${{ job.workflow_sha }}"); got != 1 {
		t.Errorf("job.workflow_sha used %d times, want exactly once (one job)", got)
	}
}

// Least privilege: the workflow grants nothing by default; the classify
// job holds exactly contents:read + checks:read (check-run evidence and
// Git data API reads); runs-on is hardcoded to ubuntu-latest so the
// never-executes-PR-code property never lands on consumer self-hosted
// runners; every checkout leaks no token and the consumer checkout has
// full history (the release pins a revision older than the head).
func TestPromotionWorkflowLeastPrivilege(t *testing.T) {
	wf, raw := loadPromotionWorkflow(t)
	if len(wf.Permissions) != 0 {
		t.Errorf("top-level permissions = %v, want none (no default grants)", wf.Permissions)
	}
	p := wf.Jobs["classify"].Permissions
	if len(p) != 2 || p["contents"] != "read" || p["checks"] != "read" {
		t.Errorf("classify permissions = %v, want exactly contents: read + checks: read", p)
	}
	jt := jobText(t, raw, "classify")
	if !strings.Contains(jt, "runs-on: ubuntu-latest") {
		t.Error("classify job must hardcode runs-on: ubuntu-latest — never consumer self-hosted runners")
	}
	checkouts := checkoutBlocks(jt)
	if len(checkouts) != 2 {
		t.Fatalf("classify job has %d checkouts, want 2 (toolkit + consumer)", len(checkouts))
	}
	for _, b := range checkouts {
		if !strings.Contains(b, "persist-credentials: false") {
			t.Error("every checkout must set persist-credentials: false")
		}
	}
	toolkit := checkouts[0]
	if !strings.Contains(toolkit, "path: toolkit") || !strings.Contains(toolkit, "repository: ${{ job.workflow_repository }}") {
		t.Error("first checkout must be the pinned toolkit, isolated in toolkit/")
	}
	consumer := checkouts[1]
	if strings.Contains(consumer, "repository:") {
		t.Errorf("second checkout must be the caller repository, got: %s", consumer)
	}
	if !strings.Contains(consumer, "path: consumer") {
		t.Error("consumer checkout must land in the sibling consumer/ directory, never beside the trusted build")
	}
	if !strings.Contains(consumer, "fetch-depth: 0") {
		t.Error("consumer checkout must set fetch-depth: 0 — the release pins a revision older than the transition head")
	}
	// The trusted build must complete BEFORE any PR-controlled content
	// exists on disk.
	build := strings.Index(jt, "go build -o /tmp/deployctl")
	consumerAt := strings.Index(jt, "path: consumer")
	if build < 0 || consumerAt < 0 || consumerAt < build {
		t.Error("deployctl must be built before the consumer checkout — no PR content on disk during the trusted build")
	}
}

// The caller permission floor published in docs/consumer-contract-v1.md
// for this workflow (contents: read, checks: read) must always COVER what
// the reusable workflow itself requests. If this test fails, a job gained
// a new permission — raise the documented floor in the same change.
func TestPromotionWorkflowPermissionFloorCoversAllJobs(t *testing.T) {
	wf, _ := loadPromotionWorkflow(t)
	floor := map[string]string{"contents": "read", "checks": "read"}
	seen := map[string]bool{}
	collect := func(name string, perms map[string]any) {
		if len(perms) == 0 {
			return
		}
		for scope, want := range perms {
			if floor[scope] != want || floor[scope] == "" {
				t.Errorf("%s requests %s: %v — beyond the documented caller floor %v; update docs/consumer-contract-v1.md in lockstep", name, scope, want, floor)
			}
			seen[scope] = true
		}
	}
	collect("workflow", wf.Permissions)
	for name, job := range wf.Jobs {
		collect("job "+name, job.Permissions)
	}
	if len(seen) == 0 {
		t.Fatal("no job-level permissions found — the floor test has nothing to pin")
	}
}

// The classifier reports; it never rules. Reported classifications
// (exit 0/1/3) always set the three outputs and always conclude success;
// only machinery failures (unsupported event, usage exit, unparsable
// result) fail the step with outputs absent. The step must also derive
// mode and SHAs from the triggering event.
func TestPromotionWorkflowClassifierReportsNeverRules(t *testing.T) {
	_, raw := loadPromotionWorkflow(t)
	run := stepField(promotionClassifyStep(t, raw), "run")

	for _, want := range []string{
		"promotion classify",
		"--json",
		"--repo \"$REPO\"",
		"--mode \"$mode\"",
		"--base \"$base\"",
		"--head \"$head\"",
		"--repo-dir \"$repo_dir\"",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("classify step run misses %q", want)
		}
	}
	if !strings.Contains(run, "pull_request_target)") || !strings.Contains(run, "push)") {
		t.Error("classify step must infer mode from the triggering event (pr vs push)")
	}
	if !strings.Contains(run, "case \"$code\" in") || !strings.Contains(run, "0|1|3) ;;") {
		t.Error("classify step must capture reported classifications (exit 0/1/3) instead of failing the job")
	}
	if !strings.Contains(run, "exit 1") {
		t.Error("classify step must fail on machinery failures (unsupported event, usage exit, unparsable result)")
	}
	if !strings.HasSuffix(strings.TrimSpace(run), "exit 0") {
		t.Error("a reported classification must conclude success — a failing classifier job would skip the fallback jobs it protects")
	}
	for _, want := range []string{"classification=$classification", "promotion_only=$promotion_only"} {
		if !strings.Contains(run, want) {
			t.Errorf("classify step must export %q to GITHUB_OUTPUT", want)
		}
	}
	// GitHub warns against fixed heredoc delimiters for
	// operator-influenced values: the reason delimiter must be generated
	// and verified absent from the reason.
	for _, want := range []string{"reason<<$reason_delim", "gen_delim()", "grep -qF \"$reason_delim\""} {
		if !strings.Contains(run, want) {
			t.Errorf("reason output must use a generated, collision-checked delimiter; misses %q", want)
		}
	}
	if strings.Contains(run, "reason<<PROMOTION_REASON_EOF") {
		t.Error("reason output must not use a fixed heredoc delimiter")
	}
}

// PR code is never executed: the trusted build is isolated from consumer
// content on disk (sibling directories, build first), dependency
// resolution cannot be redirected by a parent go.work, and scratch files
// never mix into the consumer tree.
func TestPromotionWorkflowNeverExecutesConsumerCode(t *testing.T) {
	_, raw := loadPromotionWorkflow(t)
	if !strings.Contains(raw, "go build -o /tmp/deployctl ./cmd/deployctl") {
		t.Error("deployctl must be built from the pinned toolkit checkout")
	}
	if !strings.Contains(raw, "GOWORK: off") {
		t.Error("the trusted build must set GOWORK: off — a parent go.work in the consumer checkout could otherwise redirect module resolution into attacker-controlled replacements")
	}
	if !strings.Contains(raw, "path: toolkit") || !strings.Contains(raw, "path: consumer") {
		t.Error("toolkit and consumer content must live in separate sibling checkout directories")
	}
	if !strings.Contains(raw, "$GITHUB_WORKSPACE/consumer/$REPO_DIR") {
		t.Error("classification must read the consumer checkout under consumer/, never the workspace root")
	}
	if !strings.Contains(raw, "$RUNNER_TEMP/classify.json") {
		t.Error("classifier scratch output must go under RUNNER_TEMP, not the consumer or toolkit tree")
	}
	for _, m := range regexp.MustCompile(`(?m)^\s*working-directory: (.+)$`).FindAllStringSubmatch(raw, -1) {
		if strings.TrimSpace(m[1]) != "toolkit" {
			t.Errorf("working-directory %q is outside the toolkit checkout — consumer content must never be a working directory", m[1])
		}
	}
	if strings.Count(raw, "/tmp/deployctl promotion classify") != 1 {
		t.Error("exactly one classification invocation, using the trusted binary")
	}
}

// The documented consumer routing pattern must be fail-closed: GitHub
// treats skipped jobs as satisfied required checks, so a plain
// `promotion_only != 'true'` condition would let a failed classify job
// skip application CI entirely. Uncertainty must mean the expensive path.
// The canonical caller must also grant the permission floor on the
// calling job — a reusable workflow cannot elevate past its caller, and
// GitHub's default token lacks checks:read.
func TestPromotionCIDocumentsFailClosedRouting(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "promotion-ci.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	fence := docFenceContaining(t, doc, "promotion_only != 'true'")
	if !strings.Contains(fence, "!cancelled()") || !strings.Contains(fence, "needs.classify.result != 'success'") {
		t.Error("the documented test routing condition must be status-aware (fail closed)")
	}
	if !strings.Contains(fence, "needs.classify.result == 'success'") {
		t.Error("the documented publish condition must require a decisive non-promotion classification — uncertainty qualifies but never publishes")
	}
	for _, want := range []string{"permissions:", "contents: read", "checks: read"} {
		if !strings.Contains(fence, want) {
			t.Errorf("the documented classify caller must grant the permission floor; misses %q — a called workflow cannot elevate past its caller and GitHub's default token lacks checks:read", want)
		}
	}
}

// docFenceContaining returns the fenced markdown code block containing
// marker (the first one, in document order).
func docFenceContaining(t *testing.T, doc, marker string) string {
	t.Helper()
	for _, fence := range strings.Split(doc, "```") {
		if strings.Contains(fence, marker) {
			return fence
		}
	}
	t.Fatalf("no fenced block containing %q", marker)
	return ""
}
