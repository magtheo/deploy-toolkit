package main

// Contract tests for the reusable deployment workflow. The workflow is
// part of Consumer Contract v1: these tests pin its SEMANTIC invariants
// (trust split, least privilege, full-SHA pinning, machine result
// capture) — never brittle byte snapshots of the YAML.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const workflowRelPath = ".github/workflows/deploy.yml"

type workflowFile struct {
	On struct {
		WorkflowCall struct {
			Inputs map[string]struct {
				Required bool   `yaml:"required"`
				Type     string `yaml:"type"`
			} `yaml:"inputs"`
			Secrets map[string]struct {
				Required bool `yaml:"required"`
			} `yaml:"secrets"`
			Outputs map[string]struct {
				Value       string `yaml:"value"`
				Description string `yaml:"description"`
			} `yaml:"outputs"`
		} `yaml:"workflow_call"`
	} `yaml:"on"`
	Permissions map[string]any   `yaml:"permissions"`
	Jobs        map[string]wfJob `yaml:"jobs"`
}

type wfJob struct {
	Needs       stringOrList      `yaml:"needs"`
	Permissions map[string]any    `yaml:"permissions"`
	Outputs     map[string]string `yaml:"outputs"`
	Steps       []map[string]any  `yaml:"steps"`
}

// stepField pulls a step property out of the loosely typed step map.
func stepField(step map[string]any, key string) string {
	v, _ := step[key].(string)
	return v
}

// findStep returns the step with the given id in a job.
func findStep(t *testing.T, job wfJob, id string) map[string]any {
	t.Helper()
	for _, st := range job.Steps {
		if stepField(st, "id") == id {
			return st
		}
	}
	t.Fatalf("no step with id %q in job", id)
	return nil
}

// stringOrList accepts both `needs: prepare` and a list form.
type stringOrList []string

func (s *stringOrList) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		*s = []string{value.Value}
		return nil
	}
	var l []string
	if err := value.Decode(&l); err != nil {
		return err
	}
	*s = l
	return nil
}

func loadWorkflow(t *testing.T) (*workflowFile, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", workflowRelPath))
	if err != nil {
		t.Fatal(err)
	}
	var wf workflowFile
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("workflow YAML does not parse: %v", err)
	}
	return &wf, string(raw)
}

// jobText extracts one job's YAML text so step-level properties are
// checked within the right job (never by brittle line position).
func jobText(t *testing.T, raw, job string) string {
	t.Helper()
	start := strings.Index(raw, "\n  "+job+":\n")
	if start < 0 {
		t.Fatalf("job %q not found", job)
	}
	rest := raw[start+len("\n  "+job+":\n"):]
	if loc := regexp.MustCompile(`(?m)^  \S`).FindStringIndex(rest); loc != nil {
		rest = rest[:loc[0]]
	}
	return rest
}

// The workflow is invokable as a reusable workflow with the contract
// inputs and explicit secret inputs (no inherit-all on a reusable call).
func TestWorkflowIsReusableWithContractInputs(t *testing.T) {
	wf, _ := loadWorkflow(t)
	call := wf.On.WorkflowCall
	in, ok := call.Inputs["environment"]
	if !ok || !in.Required {
		t.Errorf("input environment must exist and be required")
	}
	for _, banned := range []string{"toolkit_ref", "ref"} {
		if _, ok := call.Inputs[banned]; ok {
			t.Errorf("input %q must not exist — the workflow's own SHA anchors the machinery and the promoted commit anchors the source", banned)
		}
	}
	for _, name := range []string{"target_host", "target_ssh_key", "target_host_key"} {
		sec, ok := call.Secrets[name]
		if !ok || !sec.Required {
			t.Errorf("secret input %q must exist and be required", name)
		}
	}
}

// The trust split: prepare holds repository authority and no secret;
// deploy holds the credential and never checks out consumer source.
func TestWorkflowTrustSplit(t *testing.T) {
	wf, raw := loadWorkflow(t)
	if _, ok := wf.Jobs["prepare"]; !ok {
		t.Fatal("no prepare job")
	}
	deploy, ok := wf.Jobs["deploy"]
	if !ok {
		t.Fatal("no deploy job")
	}
	if len(deploy.Needs) == 0 || deploy.Needs[0] != "prepare" {
		t.Error("deploy must need prepare — the artifact is the only material boundary")
	}

	prepareText := jobText(t, raw, "prepare")
	if !strings.Contains(prepareText, "actions/checkout@") {
		t.Error("prepare job must check out the consumer repository (repository authority lives here)")
	}
	if strings.Contains(prepareText, "secrets.") {
		t.Error("prepare job must not reference any secret — it runs with zero target credential")
	}
	if !strings.Contains(prepareText, "deployctl prepare") {
		t.Error("prepare job must build the artifact with deployctl prepare")
	}

	deployText := jobText(t, raw, "deploy")
	repos := checkoutRepos(deployText)
	if len(repos) == 0 {
		t.Fatal("deploy job must check out the pinned toolkit")
	}
	for _, repo := range repos {
		if repo != "${{ job.workflow_repository }}" {
			t.Errorf("deploy job checks out %q — consumer source must never be checked out", repo)
		}
	}
	if !strings.Contains(deployText, "actions/download-artifact@") {
		t.Error("deploy job must receive material ONLY via the prepared artifact")
	}
	if !strings.Contains(deployText, "deployctl deploy-prepared") {
		t.Error("deploy job must deploy via deployctl deploy-prepared")
	}
}

// checkoutRepos extracts the repository each checkout in a job text
// references ("" when default — i.e. the invoking consumer repository).
func checkoutRepos(job string) []string {
	var refs []string
	for _, b := range strings.Split(job, "- name:") {
		if !strings.Contains(b, "actions/checkout@") {
			continue
		}
		if m := regexp.MustCompile(`repository:[ \t]+(.+)`).FindStringSubmatch(b); m != nil {
			refs = append(refs, m[1])
		} else {
			refs = append(refs, "")
		}
	}
	return refs
}

// Every action is pinned by a full 40-hex commit SHA, the toolkit is
// rebuilt from job.workflow_repository + job.workflow_sha (the
// reusable workflow's OWN commit — the single machinery trust anchor),
// and each job fails closed unless the workflow itself was invoked by
// full SHA (job.workflow_ref gate).
func TestWorkflowAnchoredByItsOwnSHA(t *testing.T) {
	_, raw := loadWorkflow(t)
	if strings.Contains(raw, "toolkit_ref") {
		t.Error("toolkit_ref must not exist anywhere — a duplicated caller-controlled pin defeats the trust anchor")
	}
	if strings.Contains(raw, "inputs.ref") {
		t.Error("an arbitrary consumer ref must not exist — deployment prepares the promoted invoking commit only")
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
	for _, job := range []string{"prepare", "deploy"} {
		jt := jobText(t, raw, job)
		if !strings.Contains(jt, "repository: ${{ job.workflow_repository }}") || !strings.Contains(jt, "ref: ${{ job.workflow_sha }}") {
			t.Errorf("%s job must build deployctl from job.workflow_repository@job.workflow_sha", job)
		}
		if !strings.Contains(jt, "WORKFLOW_REF: ${{ job.workflow_ref }}") || !strings.Contains(jt, "floating ref is not a trust anchor") {
			t.Errorf("%s job must fail closed on a floating workflow invocation ref", job)
		}
	}
	if got := strings.Count(raw, "${{ job.workflow_sha }}"); got != 2 {
		t.Errorf("job.workflow_sha used %d times, want exactly once per job", got)
	}
}

// Least privilege: the workflow grants nothing by default; prepare is
// contents:read only; deploy is actions:read only and never touches the
// consumer repository; no checkout leaks a token.
func TestWorkflowLeastPrivilege(t *testing.T) {
	wf, raw := loadWorkflow(t)
	if len(wf.Permissions) != 0 {
		t.Errorf("top-level permissions = %v, want none (no default grants)", wf.Permissions)
	}
	p := wf.Jobs["prepare"].Permissions
	if len(p) != 1 || p["contents"] != "read" {
		t.Errorf("prepare permissions = %v, want exactly contents: read", p)
	}
	d := wf.Jobs["deploy"].Permissions
	if d["actions"] != "read" {
		t.Errorf("deploy permissions = %v, want actions: read", d)
	}
	if _, has := d["contents"]; has {
		t.Errorf("deploy job must have no contents scope: %v", d)
	}
	for _, b := range strings.Split(raw, "- name:") {
		if strings.Contains(b, "actions/checkout@") && !strings.Contains(b, "persist-credentials: false") {
			t.Error("every checkout must set persist-credentials: false")
		}
	}
}

// The deployctl.result/v1 machine result must traverse the complete
// GitHub output chain — capture step → deploy job → workflow_call —
// whatever the outcome. Presence of the string GITHUB_OUTPUT alone
// proves nothing; the mappings are parsed here.
func TestWorkflowCapturesMachineResult(t *testing.T) {
	wf, raw := loadWorkflow(t)
	deploy := wf.Jobs["deploy"]
	deployText := jobText(t, raw, "deploy")
	if !strings.Contains(deployText, "--json") {
		t.Error("the deploy step must run deployctl in --json machine mode")
	}
	if !strings.Contains(deployText, "result.json") {
		t.Error("the deploy step must write the machine result to result.json")
	}

	for name, want := range map[string]string{
		"result":    "${{ steps.capture.outputs.result }}",
		"exit_code": "${{ steps.capture.outputs.exit_code }}",
	} {
		got, ok := deploy.Outputs[name]
		if !ok || got != want {
			t.Errorf("jobs.deploy.outputs.%s = %q, want %s", name, got, want)
		}
		wfOut, ok := wf.On.WorkflowCall.Outputs[name]
		if !ok || wfOut.Value != "${{ jobs.deploy.outputs."+name+" }}" {
			t.Errorf("workflow_call.outputs.%s = %+v, want the job output mapping", name, wfOut)
		}
	}

	capture := findStep(t, deploy, "capture")
	if stepField(capture, "if") != "always()" {
		t.Errorf("capture step runs with if=%q, want always()", stepField(capture, "if"))
	}
	run := stepField(capture, "run")
	for _, want := range []string{"GITHUB_STEP_SUMMARY", "GITHUB_OUTPUT", "result<<DEPLOYCTL_RESULT_EOF", "DEPLOYCTL_RESULT_EOF", "exit_code="} {
		if !strings.Contains(run, want) {
			t.Errorf("capture step run misses %q", want)
		}
	}
}
