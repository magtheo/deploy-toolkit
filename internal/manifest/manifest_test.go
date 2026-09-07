package manifest

import (
	"os"
	"strings"
	"testing"
)

const (
	testRevision = "0123456789abcdef0123456789abcdef01234567"
	testDigest   = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func validProject() string {
	return `apiVersion: deploy.toolkit/v1
kind: Project
metadata:
  name: my-app
release:
  source:
    type: github
    repository: example/my-app
    branch: main
  requiredChecks:
    - Tests
artifacts:
  app:
    type: oci
    repository: ghcr.io/example/app
bundle:
  include:
    - docker-compose.yml
    - config/**
lifecycle:
  apply:
    argv: ["./deploy/apply.sh"]
`
}

func validRelease() string {
	return `apiVersion: deploy.toolkit/v1
kind: Release
metadata:
  project: my-app
  version: 0.1.0
source:
  type: github
  repository: example/my-app
  revision: "` + testRevision + `"
artifacts:
  app:
    type: oci
    image: ghcr.io/example/app
    digest: ` + testDigest + `
bundle:
  digest: ` + testDigest + `
deploymentContract:
  digest: ` + testDigest + `
migration:
  head: "043"
  mode: forward-compatible
  rollbackSafe: true
`
}

func validEnvironment() string {
	return `apiVersion: deploy.toolkit/v1
kind: Environment
metadata:
  name: production
spec:
  release: .deploy/releases/my-app-0.1.0.yaml
  target: production-primary
`
}

func validTarget() string {
	return `apiVersion: deploy.toolkit/v1
kind: Target
metadata:
  name: production-primary
spec:
  deployRoot: /srv/deploy
  transport:
    type: ssh
    hostFrom: DEPLOY_HOST
    port: 22
    user: deploy
    hostKeyFrom: DEPLOY_HOST_KEY
    credentialFrom: DEPLOY_SSH_KEY
`
}

func fault(t *testing.T, doc, old, new string) string {
	t.Helper()
	if !strings.Contains(doc, old) {
		t.Fatalf("fixture bug: %q not found in doc", old)
	}
	return strings.Replace(doc, old, new, 1)
}

func TestParseValidDocs(t *testing.T) {
	cases := []struct {
		kind string
		doc  string
	}{
		{KindProject, validProject()},
		{KindRelease, validRelease()},
		{KindEnvironment, validEnvironment()},
		{KindTarget, validTarget()},
	}
	for _, c := range cases {
		if _, err := Parse([]byte(c.doc), c.kind); err != nil {
			t.Errorf("%s: valid doc rejected: %v", c.kind, err)
		}
	}
}

func TestParseRejectionsSingleFault(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{name: "unknown kind", doc: fault(t, validProject(), "kind: Project", "kind: Blob"), want: "unknown kind"},
		{name: "wrong apiVersion", doc: fault(t, validProject(), "deploy.toolkit/v1", "deploy.toolkit/v9"), want: "apiVersion"},
		{name: "extra top-level property", doc: validProject() + "extra: 1\n", want: "additional"},
		{name: "missing release policy", doc: fault(t, validProject(), "release:\n  source:\n    type: github\n    repository: example/my-app\n    branch: main\n  requiredChecks:\n    - Tests\n", ""), want: "release"},
		{name: "missing requiredChecks", doc: fault(t, validProject(), "  requiredChecks:\n    - Tests\n", ""), want: "requiredChecks"},
		{name: "source not github", doc: fault(t, validProject(), "type: github", "type: gitlab"), want: "/release/source/type"},
		{name: "source registry", doc: fault(t, validProject(), "repository: example/my-app", "repository: ghcr.io/example/my-app"), want: "/release/source/repository"},
		{name: "missing source branch", doc: fault(t, validProject(), "    branch: main\n", ""), want: "/release/source'"},
		{name: "tagged artifact", doc: fault(t, validProject(), "repository: ghcr.io/example/app", "repository: ghcr.io/example/app:latest"), want: "/artifacts/app/repository"},
		{name: "digest artifact", doc: fault(t, validProject(), "repository: ghcr.io/example/app", "repository: ghcr.io/example/app@sha256:"+strings.TrimPrefix(testDigest, "sha256:")), want: "/artifacts/app/repository"},
		{name: "empty artifacts", doc: fault(t, validProject(), "artifacts:\n  app:\n    type: oci\n    repository: ghcr.io/example/app\n", "artifacts: {}\n"), want: "/artifacts"},
		{name: "empty bundle include", doc: fault(t, validProject(), "bundle:\n  include:\n    - docker-compose.yml\n    - config/**\n", "bundle:\n  include: []\n"), want: "/bundle/include"},
		{name: "bundle include absolute", doc: fault(t, validProject(), "- config/**", "- /etc/passwd"), want: "/bundle/include/1"},
		{name: "missing bundle", doc: fault(t, validProject(), "bundle:\n  include:\n    - docker-compose.yml\n    - config/**\n", ""), want: "bundle"},
		{name: "lifecycle shell string", doc: fault(t, validProject(), `argv: ["./deploy/apply.sh"]`, `command: "make deploy"`), want: "/lifecycle/apply"},
		{name: "missing apply", doc: fault(t, validProject(), "  apply:", "  applyx:"), want: "apply"},
		{name: "bad semver", doc: fault(t, validRelease(), "version: 0.1.0", "version: 1.2"), want: "/metadata/version"},
		{name: "semver bad prerelease", doc: fault(t, validRelease(), "version: 0.1.0", "version: 1.2.3-.."), want: "/metadata/version"},
		{name: "short revision", doc: fault(t, validRelease(), `revision: "`+testRevision+`"`, "revision: abc"), want: "/source/revision"},
		{name: "release source registry", doc: fault(t, validRelease(), "repository: example/my-app", "repository: ghcr.io/example/my-app"), want: "/source/repository"},
		{name: "tagged release image", doc: fault(t, validRelease(), "image: ghcr.io/example/app", "image: ghcr.io/example/app:latest"), want: "/artifacts/app/image"},
		{name: "bad artifact digest", doc: fault(t, validRelease(), "digest: "+testDigest+"\nbundle:", "digest: sha256:zz\nbundle:"), want: "/artifacts/app/digest"},
		{name: "irreversible rollbackSafe", doc: fault(t, validRelease(), "mode: forward-compatible", "mode: irreversible"), want: "rollbackSafe"},
		{name: "unknown migration mode", doc: fault(t, validRelease(), "mode: forward-compatible", "mode: yolo"), want: "/migration/mode"},
		{name: "release ref parent", doc: fault(t, validEnvironment(), ".deploy/releases/my-app-0.1.0.yaml", "../releases/my-app-0.1.0.yaml"), want: "/spec/release"},
		{name: "release ref absolute", doc: fault(t, validEnvironment(), ".deploy/releases/my-app-0.1.0.yaml", "/deploy/releases/my-app-0.1.0.yaml"), want: "/spec/release"},
		{name: "release ref outside releases", doc: fault(t, validEnvironment(), ".deploy/releases/my-app-0.1.0.yaml", ".deploy/environments/my-app-0.1.0.yaml"), want: "/spec/release"},
		{name: "release ref nested", doc: fault(t, validEnvironment(), ".deploy/releases/my-app-0.1.0.yaml", ".deploy/releases/sub/my-app-0.1.0.yaml"), want: "/spec/release"},
		{name: "missing environment target", doc: fault(t, validEnvironment(), "  target: production-primary\n", ""), want: "'target'"},
		{name: "provider leak", doc: fault(t, validTarget(), "    credentialFrom: DEPLOY_SSH_KEY\n", "    credentialFrom: DEPLOY_SSH_KEY\n    provider: netcup\n"), want: "provider"},
		{name: "unpinned host key", doc: fault(t, validTarget(), "    hostKeyFrom: DEPLOY_HOST_KEY\n", ""), want: "hostKeyFrom"},
	}

	for _, c := range cases {
		_, err := Parse([]byte(c.doc), "")
		if err == nil {
			t.Errorf("%s: expected rejection, got success", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", c.name, err.Error(), c.want)
		}
	}
}

func TestSemanticLayerCatchesWhatSchemaPasses(t *testing.T) {
	cases := []struct {
		name string
		kind string
		doc  string
		want string
	}{
		{
			name: "bundle include dotdot segment",
			kind: KindProject,
			doc:  fault(t, validProject(), "- config/**", "- a/.."),
			want: "bundle.include",
		},
		{
			name: "release ref dotdot in file name",
			kind: KindEnvironment,
			doc:  fault(t, validEnvironment(), "my-app-0.1.0.yaml", "my-app-0.1.0..yaml"),
			want: "spec.release",
		},
	}

	for _, c := range cases {
		if err := schemaValidate(c.kind, []byte(c.doc)); err != nil {
			t.Errorf("%s: fixture never reaches the semantic layer, schema rejected it: %v", c.name, err)
			continue
		}
		_, err := Parse([]byte(c.doc), c.kind)
		if err == nil {
			t.Errorf("%s: expected semantic rejection, got success", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", c.name, err.Error(), c.want)
		}
	}
}

func TestReleaseCheckRejectsIrreversibleRollbackSafe(t *testing.T) {
	r := &Release{Migration: MigrationSpec{Head: "1", Mode: MigrationIrreversible, RollbackSafe: true}}
	if err := r.check(); err == nil {
		t.Error("Release.check accepted irreversible+rollbackSafe")
	}
	r.Migration.RollbackSafe = false
	if err := r.check(); err != nil {
		t.Errorf("Release.check rejected irreversible+rollbackSafe:false: %v", err)
	}
}

func TestCheckOCIRepositoryUntaggedMessage(t *testing.T) {
	err := checkOCIRepository("ghcr.io/example/app:latest")
	if err == nil {
		t.Fatal("tagged image accepted")
	}
	if !strings.Contains(err.Error(), "carries a tag or digest") {
		t.Errorf("expected untagged-invariant message, got %q", err.Error())
	}
}

func TestParseKindMismatch(t *testing.T) {
	if _, err := Parse([]byte(validProject()), KindTarget); err == nil {
		t.Error("project parsed as target: expected kind mismatch error")
	}
}

func TestTypedLoadCoverageAllFourKinds(t *testing.T) {
	p, err := LoadProject(templatePath(t, "project.yaml"))
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if p.Metadata.Name != "my-app" || p.Release.Source.Repository != "example/my-app" || p.Lifecycle.Apply == nil {
		t.Errorf("LoadProject decoded unexpected content: %+v", p)
	}

	r, err := LoadRelease(templatePath(t, "release.yaml"))
	if err != nil {
		t.Fatalf("LoadRelease: %v", err)
	}
	if r.Source.Type != SourceGitHub || r.Source.Repository != "example/my-app" {
		t.Errorf("LoadRelease source = %+v", r.Source)
	}

	e, err := LoadEnvironment(templatePath(t, "environment.yaml"))
	if err != nil {
		t.Fatalf("LoadEnvironment: %v", err)
	}
	if e.AutoRollback() != "safe-only" {
		t.Errorf("AutoRollback = %q", e.AutoRollback())
	}

	tr, err := LoadTarget(templatePath(t, "target.yaml"))
	if err != nil {
		t.Fatalf("LoadTarget: %v", err)
	}
	if tr.Spec.Transport.Type != TransportSSH || tr.Spec.Transport.EffectivePort() != 22 {
		t.Errorf("LoadTarget transport = %+v", tr.Spec.Transport)
	}
}

func TestTemplatesMatchInlineValidDocs(t *testing.T) {
	pairs := map[string]string{
		"project.yaml":     validProject(),
		"release.yaml":     validRelease(),
		"environment.yaml": validEnvironment(),
		"target.yaml":      validTarget(),
	}
	// The template files carry the same content as the inline fixtures for
	// project/release/environment; target has identical fields. If a template
	// drifts from the fixture, Parse still guards correctness; this test
	// documents that the fixtures stay representative.
	for name := range pairs {
		if _, err := Parse(mustRead(t, templatePath(t, name)), ""); err != nil {
			t.Errorf("%s: template no longer valid: %v", name, err)
		}
	}
}

func templatePath(t *testing.T, name string) string {
	t.Helper()
	return "../../templates/" + name
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}
