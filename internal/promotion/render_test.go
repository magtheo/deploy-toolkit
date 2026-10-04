package promotion

import (
	"strings"
	"testing"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/release"
)

func TestRenderBodyAuthorizationNamesEnvironment(t *testing.T) {
	base := BodyInput{
		Project:    "platform-core",
		Version:    "0.1.7",
		BaseSHA:    strings.Repeat("a", 40),
		BranchHead: strings.Repeat("b", 40),
		Revision:   strings.Repeat("c", 40),
		Evaluation: &release.Evaluation{Artifacts: map[string]release.ResolvedArtifact{}},
		Migration:  manifest.MigrationSpec{Head: "051", Mode: manifest.MigrationForwardCompatible},
	}

	for _, env := range []string{"staging", "production", "canary"} {
		in := base
		in.Environment = env
		body := RenderBody(in)
		if !strings.Contains(body, "authorizes promotion of this exact release to environment `"+env+"`") {
			t.Errorf("authorization sentence must name environment %q, body:\n%s", env, body)
		}
		for _, other := range []string{"staging", "production", "canary"} {
			if other == env {
				continue
			}
			if strings.Contains(body, "to environment `"+other+"`") {
				t.Errorf("body for %q must not claim environment %q", env, other)
			}
		}
		if strings.Contains(body, "production promotion") {
			t.Errorf("hard-coded production wording must not appear, body:\n%s", body)
		}
	}
}
