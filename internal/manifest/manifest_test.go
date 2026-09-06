package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

func templatePath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("..", "..", "templates", name)
}

func TestParseAllTemplatesAllFourKinds(t *testing.T) {
	cases := map[string]string{
		"project.yaml":     KindProject,
		"release.yaml":     KindRelease,
		"environment.yaml": KindEnvironment,
		"target.yaml":      KindTarget,
	}
	for name, kind := range cases {
		res, err := Parse(mustRead(t, templatePath(t, name)), kind)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if res.Header.Kind != kind {
			t.Errorf("%s: parsed kind %q, want %q", name, res.Header.Kind, kind)
		}
	}
}

func TestTypedLoadCoverageAllFourKinds(t *testing.T) {
	if p, err := LoadProject(templatePath(t, "project.yaml")); err != nil {
		t.Errorf("LoadProject: %v", err)
	} else if p.Metadata.Name != "my-app" || p.Lifecycle.Apply == nil || len(p.Lifecycle.Apply.Argv) == 0 {
		t.Error("LoadProject: unexpected decoded content")
	}
	if r, err := LoadRelease(templatePath(t, "release.yaml")); err != nil {
		t.Errorf("LoadRelease: %v", err)
	} else if r.Source.Type != SourceGitHub || r.Source.Repository != "example/my-app" {
		t.Errorf("LoadRelease: source = %+v", r.Source)
	}
	if e, err := LoadEnvironment(templatePath(t, "environment.yaml")); err != nil {
		t.Errorf("LoadEnvironment: %v", err)
	} else if e.AutoRollback() != "safe-only" {
		t.Errorf("AutoRollback = %q", e.AutoRollback())
	}
	if tr, err := LoadTarget(templatePath(t, "target.yaml")); err != nil {
		t.Errorf("LoadTarget: %v", err)
	} else if tr.Spec.Transport.EffectivePort() != 22 || tr.Spec.Transport.Type != TransportSSH {
		t.Errorf("LoadTarget: transport = %+v", tr.Spec.Transport)
	}
}

func TestParseRejections(t *testing.T) {
	validReleaseTail := `artifacts: {app: {type: oci, image: ghcr.io/example/app, digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}}
bundle: {digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}
deploymentContract: {digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}
`
	releaseDoc := func(version, source, migration string) string {
		return "apiVersion: deploy.toolkit/v1\nkind: Release\nmetadata: {project: x, version: " + version + "}\n" +
			"source: " + source + "\n" + validReleaseTail + "migration: " + migration + "\n"
	}

	cases := map[string]string{
		"unknown kind":          "apiVersion: deploy.toolkit/v1\nkind: Blob\nmetadata: {name: x}\n",
		"wrong apiVersion":      "apiVersion: deploy.toolkit/v9\nkind: Project\nmetadata: {name: x}\n",
		"extra property":        "apiVersion: deploy.toolkit/v1\nkind: Project\nmetadata: {name: x}\nrelease: {source: {type: github, repository: o/r, branch: main}, requiredChecks: [c]}\nartifacts: {}\nbundle: {include: [a]}\nlifecycle: {apply: {argv: [a]}}\nextra: 1\n",
		"missing release":       "apiVersion: deploy.toolkit/v1\nkind: Project\nmetadata: {name: x}\nartifacts: {app: {type: oci, repository: ghcr.io/e/a}}\nbundle: {include: [a]}\nlifecycle: {apply: {argv: [a]}}\n",
		"missing bundle":        "apiVersion: deploy.toolkit/v1\nkind: Project\nmetadata: {name: x}\nrelease: {source: {type: github, repository: o/r, branch: main}, requiredChecks: [c]}\nartifacts: {}\nlifecycle: {apply: {argv: [a]}}\n",
		"missing checks":        "apiVersion: deploy.toolkit/v1\nkind: Project\nmetadata: {name: x}\nrelease: {source: {type: github, repository: o/r, branch: main}}\nartifacts: {}\nbundle: {include: [a]}\nlifecycle: {apply: {argv: [a]}}\n",
		"source not github":     "apiVersion: deploy.toolkit/v1\nkind: Project\nmetadata: {name: x}\nrelease: {source: {type: gitlab, repository: o/r, branch: main}, requiredChecks: [c]}\nartifacts: {}\nbundle: {include: [a]}\nlifecycle: {apply: {argv: [a]}}\n",
		"source registry":       "apiVersion: deploy.toolkit/v1\nkind: Project\nmetadata: {name: x}\nrelease: {source: {type: github, repository: ghcr.io/example, branch: main}, requiredChecks: [c]}\nartifacts: {}\nbundle: {include: [a]}\nlifecycle: {apply: {argv: [a]}}\n",
		"tagged artifact":       "apiVersion: deploy.toolkit/v1\nkind: Project\nmetadata: {name: x}\nrelease: {source: {type: github, repository: o/r, branch: main}, requiredChecks: [c]}\nartifacts: {app: {type: oci, repository: ghcr.io/example/app:latest}}\nbundle: {include: [a]}\nlifecycle: {apply: {argv: [a]}}\n",
		"digest artifact":       "apiVersion: deploy.toolkit/v1\nkind: Project\nmetadata: {name: x}\nrelease: {source: {type: github, repository: o/r, branch: main}, requiredChecks: [c]}\nartifacts: {app: {type: oci, repository: ghcr.io/example/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}}\nbundle: {include: [a]}\nlifecycle: {apply: {argv: [a]}}\n",
		"bad semver":            releaseDoc("1.2", "{type: github, repository: o/r, revision: \"0123456789abcdef0123456789abcdef01234567\"}", "{head: \"1\", mode: none, rollbackSafe: true}"),
		"semver bad prereq":     releaseDoc("1.2.3-..", "{type: github, repository: o/r, revision: \"0123456789abcdef0123456789abcdef01234567\"}", "{head: \"1\", mode: none, rollbackSafe: true}"),
		"short revision":        releaseDoc("1.2.3", "{type: github, repository: o/r, revision: abc}", "{head: \"1\", mode: none, rollbackSafe: true}"),
		"irreversible+safe":     releaseDoc("1.2.3", "{type: github, repository: o/r, revision: \"0123456789abcdef0123456789abcdef01234567\"}", "{head: \"1\", mode: irreversible, rollbackSafe: true}"),
		"tagged release img":    "apiVersion: deploy.toolkit/v1\nkind: Release\nmetadata: {project: x, version: 1.2.3}\nsource: {type: github, repository: o/r, revision: \"0123456789abcdef0123456789abcdef01234567\"}\nartifacts: {app: {type: oci, image: ghcr.io/example/app:latest, digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}}\n" + validReleaseTail + "migration: {head: \"1\", mode: none, rollbackSafe: true}\n",
		"provider leak":         "apiVersion: deploy.toolkit/v1\nkind: Target\nmetadata: {name: t}\nspec: {transport: {type: ssh, hostFrom: H, user: u, hostKeyFrom: K, credentialFrom: C, provider: netcup}}\n",
		"unpinned host key":     "apiVersion: deploy.toolkit/v1\nkind: Target\nmetadata: {name: t}\nspec: {transport: {type: ssh, hostFrom: H, user: u, credentialFrom: C}}\n",
		"lifecycle shell":       "apiVersion: deploy.toolkit/v1\nkind: Project\nmetadata: {name: x}\nrelease: {source: {type: github, repository: o/r, branch: main}, requiredChecks: [c]}\nartifacts: {}\nbundle: {include: [a]}\nlifecycle: {apply: {command: \"make deploy\"}}\n",
		"missing apply":         "apiVersion: deploy.toolkit/v1\nkind: Project\nmetadata: {name: x}\nrelease: {source: {type: github, repository: o/r, branch: main}, requiredChecks: [c]}\nartifacts: {}\nbundle: {include: [a]}\nlifecycle: {preflight: {argv: [a]}}\n",
		"artifact bad type":     "apiVersion: deploy.toolkit/v1\nkind: Project\nmetadata: {name: x}\nrelease: {source: {type: github, repository: o/r, branch: main}, requiredChecks: [c]}\nartifacts: {app: {type: tar, repository: r}}\nbundle: {include: [a]}\nlifecycle: {apply: {argv: [a]}}\n",
		"release ref parent":    "apiVersion: deploy.toolkit/v1\nkind: Environment\nmetadata: {name: e}\nspec: {release: ../releases/x.yaml, target: t}\n",
		"release ref abs":       "apiVersion: deploy.toolkit/v1\nkind: Environment\nmetadata: {name: e}\nspec: {release: /deploy/releases/x.yaml, target: t}\n",
		"release ref loose":     "apiVersion: deploy.toolkit/v1\nkind: Environment\nmetadata: {name: e}\nspec: {release: releases/x.yaml, target: t}\n",
		"release ref nested":    "apiVersion: deploy.toolkit/v1\nkind: Environment\nmetadata: {name: e}\nspec: {release: .deploy/releases/sub/x.yaml, target: t}\n",
		"release ref fake":      "apiVersion: deploy.toolkit/v1\nkind: Environment\nmetadata: {name: e}\nspec: {release: .deploy/releases/..yaml, target: t}\n",
		"bundle include abs":    "apiVersion: deploy.toolkit/v1\nkind: Project\nmetadata: {name: x}\nrelease: {source: {type: github, repository: o/r, branch: main}, requiredChecks: [c]}\nartifacts: {}\nbundle: {include: [/etc/passwd]}\nlifecycle: {apply: {argv: [a]}}\n",
		"bundle include dotdot": "apiVersion: deploy.toolkit/v1\nkind: Project\nmetadata: {name: x}\nrelease: {source: {type: github, repository: o/r, branch: main}, requiredChecks: [c]}\nartifacts: {}\nbundle: {include: [config/../../secrets]}\nlifecycle: {apply: {argv: [a]}}\n",
	}

	for name, doc := range cases {
		if _, err := Parse([]byte(doc), ""); err == nil {
			t.Errorf("%s: expected rejection, got success", name)
		}
	}
}

func TestParseSemanticLayerCatchesWhatSchemaPasses(t *testing.T) {
	cases := map[string]string{
		"bundle include dotdot segment": "apiVersion: deploy.toolkit/v1\nkind: Project\nmetadata: {name: x}\nrelease: {source: {type: github, repository: o/r, branch: main}, requiredChecks: [c]}\nartifacts: {}\nbundle: {include: [a/..]}\nlifecycle: {apply: {argv: [a]}}\n",
		"release ref dotdot in name":    "apiVersion: deploy.toolkit/v1\nkind: Environment\nmetadata: {name: e}\nspec: {release: .deploy/releases/my-app..yaml, target: t}\n",
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc), ""); err == nil {
			t.Errorf("%s: expected semantic rejection, got success", name)
		}
	}
}

func TestParseAcceptsStrictSemVer(t *testing.T) {
	doc := "apiVersion: deploy.toolkit/v1\nkind: Release\nmetadata: {project: x, version: 1.2.3-rc.1+build.2}\n" +
		"source: {type: github, repository: o/r, revision: \"0123456789abcdef0123456789abcdef01234567\"}\n" +
		"artifacts: {app: {type: oci, image: ghcr.io/example/app, digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}}\n" +
		"bundle: {digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}\n" +
		"deploymentContract: {digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}\n" +
		"migration: {head: \"043\", mode: forward-compatible, rollbackSafe: true}\n"
	if _, err := Parse([]byte(doc), KindRelease); err != nil {
		t.Errorf("strict semver rejected: %v", err)
	}
}

func TestParseKindMismatch(t *testing.T) {
	if _, err := Parse(mustRead(t, templatePath(t, "project.yaml")), KindTarget); err == nil {
		t.Error("project validated as target: expected kind mismatch error")
	}
}

func TestSemanticHelpers(t *testing.T) {
	if err := checkOCIRepository("ghcr.io/example/app:latest"); err == nil {
		t.Error("tagged image accepted")
	}
	if err := checkOCIRepository("registry:5000/team/app"); err != nil {
		t.Errorf("registry port rejected: %v", err)
	}
	if err := checkOCIRepository("ghcr.io/example/app"); err != nil {
		t.Errorf("plain repo rejected: %v", err)
	}
	if err := checkBundleInclude("config/**"); err != nil {
		t.Errorf("config/** rejected: %v", err)
	}
	if err := checkBundleInclude("a/.."); err == nil {
		t.Error("traversal segment accepted")
	}
	if err := checkReleaseRef(".deploy/releases/my-app-0.1.0.yaml"); err != nil {
		t.Errorf("valid release ref rejected: %v", err)
	}
	if err := checkReleaseRef(".deploy/releases/..yaml"); err == nil {
		t.Error("dotdot release ref accepted")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}
