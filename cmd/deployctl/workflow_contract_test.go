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
		} `yaml:"workflow_call"`
	} `yaml:"on"`
	Permissions map[string]any   `yaml:"permissions"`
	Jobs        map[string]wfJob `yaml:"jobs"`
}

type wfJob struct {
	Needs       stringOrList     `yaml:"needs"`
	Permissions map[string]any   `yaml:"permissions"`
	Steps       []map[string]any `yaml:"steps"`
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
	for _, name := range []string{"environment", "toolkit_ref"} {
		in, ok := call.Inputs[name]
		if !ok || !in.Required {
			t.Errorf("input %q must exist and be required", name)
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
		if repo != "magtheo/deploy-toolkit" {
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
		if m := regexp.MustCompile(`repository:\s*(\S+)`).FindStringSubmatch(b); m != nil {
			refs = append(refs, m[1])
		} else {
			refs = append(refs, "")
		}
	}
	return refs
}

// Every third-party action is pinned by a full 40-hex commit SHA, and
// the toolkit itself is consumed via inputs.toolkit_ref with an
// explicit full-SHA validation step in each job.
func TestWorkflowActionsPinnedByFullSHA(t *testing.T) {
	_, raw := loadWorkflow(t)
	pins := regexp.MustCompile(`uses: ([\w.-]+/[\w.-]+)@(\S+)`).FindAllStringSubmatch(raw, -1)
	if len(pins) == 0 {
		t.Fatal("no action pins found")
	}
	for _, pin := range pins {
		if pin[1] == "magtheo/deploy-toolkit" {
			continue // pins the consumer-provided toolkit_ref, checked below
		}
		if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(pin[2]) {
			t.Errorf("%s pinned by %q — actions must be pinned by full commit SHA", pin[1], pin[2])
		}
	}
	if !strings.Contains(raw, "ref: ${{ inputs.toolkit_ref }}") {
		t.Error("the toolkit checkout must be pinned by inputs.toolkit_ref")
	}
	if strings.Count(raw, "floating") < 2 || strings.Count(raw, "full 40-hex commit SHA") < 2 {
		t.Error("each job must reject a floating toolkit_ref with a full-SHA requirement")
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

// The deploy step captures deployctl.result/v1 as the machine result —
// job summary and step output — whatever the outcome.
func TestWorkflowCapturesMachineResult(t *testing.T) {
	_, raw := loadWorkflow(t)
	deployText := jobText(t, raw, "deploy")
	if !strings.Contains(deployText, "--json") {
		t.Error("the deploy step must run deployctl in --json machine mode")
	}
	if !strings.Contains(deployText, "result.json") {
		t.Error("the deploy step must write the machine result to result.json")
	}
	if !strings.Contains(deployText, "if: always()") {
		t.Error("the result must be captured even when the deploy step fails")
	}
	if !strings.Contains(deployText, "GITHUB_STEP_SUMMARY") || !strings.Contains(deployText, "GITHUB_OUTPUT") {
		t.Error("the machine result must reach the job summary and the step output")
	}
}
