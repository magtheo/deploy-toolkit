package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

func load(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "templates", name))
	if err != nil {
		t.Fatalf("read template %s: %v", name, err)
	}
	return data
}

func TestValidateTemplates(t *testing.T) {
	cases := map[string]string{
		"project.yaml":     KindProject,
		"release.yaml":     KindRelease,
		"environment.yaml": KindEnvironment,
		"target.yaml":      KindTarget,
	}
	for name, kind := range cases {
		data := load(t, name)
		if _, err := Validate(data, kind); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestValidateRejections(t *testing.T) {
	cases := map[string]string{
		"unknown kind":      "apiVersion: deploy.toolkit/v1\nkind: Blob\nmetadata: {name: x}\n",
		"wrong apiVersion":  "apiVersion: deploy.toolkit/v9\nkind: Project\nmetadata: {name: x}\n",
		"extra property":    "apiVersion: deploy.toolkit/v1\nkind: Project\nmetadata: {name: x}\nartifacts: {}\nlifecycle: {apply: {argv: [a]}}\nextra: 1\n",
		"bad semver":        "apiVersion: deploy.toolkit/v1\nkind: Release\nmetadata: {project: x, version: 1.2}\nsource: {repository: r, revision: \"0123456789abcdef0123456789abcdef01234567\"}\nartifacts: {app: {type: oci, image: i, digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}}\nbundle: {digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}\ndeploymentContract: {digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}\nmigration: {head: \"1\", mode: none, rollbackSafe: true}\n",
		"short revision":    "apiVersion: deploy.toolkit/v1\nkind: Release\nmetadata: {project: x, version: 1.2.3}\nsource: {repository: r, revision: abc}\nartifacts: {app: {type: oci, image: i, digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}}\nbundle: {digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}\ndeploymentContract: {digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}\nmigration: {head: \"1\", mode: none, rollbackSafe: true}\n",
		"provider leak":     "apiVersion: deploy.toolkit/v1\nkind: Target\nmetadata: {name: t}\nspec: {transport: {type: ssh, hostFrom: H, user: u, hostKeyFrom: K, credentialFrom: C, provider: netcup}}\n",
		"unpinned host key": "apiVersion: deploy.toolkit/v1\nkind: Target\nmetadata: {name: t}\nspec: {transport: {type: ssh, hostFrom: H, user: u, credentialFrom: C}}\n",
		"lifecycle shell":   "apiVersion: deploy.toolkit/v1\nkind: Project\nmetadata: {name: x}\nartifacts: {}\nlifecycle: {apply: {command: \"make deploy\"}}\n",
		"missing apply":     "apiVersion: deploy.toolkit/v1\nkind: Project\nmetadata: {name: x}\nartifacts: {}\nlifecycle: {preflight: {argv: [a]}}\n",
		"artifact bad type": "apiVersion: deploy.toolkit/v1\nkind: Project\nmetadata: {name: x}\nartifacts: {app: {type: tar, repository: r}}\nlifecycle: {apply: {argv: [a]}}\n",
	}

	for name, doc := range cases {
		if _, err := Validate([]byte(doc), ""); err == nil {
			t.Errorf("%s: expected rejection, got success", name)
		}
	}
}

func TestValidateKindMismatch(t *testing.T) {
	data := load(t, "project.yaml")
	if _, err := Validate(data, KindTarget); err == nil {
		t.Error("project validated as target: expected kind mismatch error")
	}
}

func TestLoadProjectTyped(t *testing.T) {
	p, err := LoadProject(filepath.Join("..", "..", "templates", "project.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Metadata.Name != "my-app" {
		t.Errorf("name = %q", p.Metadata.Name)
	}
	if p.Lifecycle.Apply == nil || len(p.Lifecycle.Apply.Argv) == 0 {
		t.Error("apply lifecycle missing")
	}
}
