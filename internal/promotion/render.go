package promotion

import (
	"fmt"
	"strings"

	"github.com/magtheo/deploy-toolkit/internal/manifest"
	"github.com/magtheo/deploy-toolkit/internal/release"
)

type BodyInput struct {
	Project     string
	Version     string
	Environment string
	BaseSHA     string
	BranchHead  string
	Revision    string
	From        string
	To          string
	OldRelease  *manifest.Release
	Evaluation  *release.Evaluation
	Migration   manifest.MigrationSpec
}

func RenderBody(in BodyInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Deploy %s %s → %s\n\n", in.Project, in.Version, in.Environment)

	writeWarnings(&b, in.Migration)

	fmt.Fprintf(&b, "## Source\n\n")
	fmt.Fprintf(&b, "| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| revision | `%s` |\n", short(in.Revision))
	fmt.Fprintf(&b, "| trusted branch | %s |\n", release.TrustedBranch)
	fmt.Fprintf(&b, "| branch head | `%s` |\n", short(in.BranchHead))
	fmt.Fprintf(&b, "| base commit | `%s` |\n", short(in.BaseSHA))

	fmt.Fprintf(&b, "\n## Environment `%s`\n\n", in.Environment)
	fmt.Fprintf(&b, "| | |\n|---|---|\n")
	if in.OldRelease != nil {
		fmt.Fprintf(&b, "| current | `%s` (`%s`) |\n", in.OldRelease.Metadata.Version, short(in.OldRelease.Source.Revision))
	} else {
		fmt.Fprintf(&b, "| current | `%s` |\n", in.From)
	}
	if in.OldRelease != nil && in.OldRelease.Metadata.Version != "" {
		fmt.Fprintf(&b, "| proposed | `%s` (`%s`) |\n", in.Version, short(in.Revision))
	} else {
		fmt.Fprintf(&b, "| proposed | `%s` |\n", in.To)
	}

	fmt.Fprintf(&b, "\n## Required checks\n\n")
	for _, c := range in.Evaluation.Checks {
		fmt.Fprintf(&b, "- ✓ %s\n", c.Name)
	}
	if len(in.Evaluation.Checks) == 0 {
		b.WriteString("- (none required)\n")
	}

	fmt.Fprintf(&b, "\n## Artifacts\n\n")
	fmt.Fprintf(&b, "| artifact | digest |\n|---|---|\n")
	names := sortedArtifactNames(in.Evaluation.Artifacts)
	for _, n := range names {
		a := in.Evaluation.Artifacts[n]
		fmt.Fprintf(&b, "| %s | `%s` |\n", n, a.Digest)
	}

	fmt.Fprintf(&b, "\n## Deployment material\n\n")
	fmt.Fprintf(&b, "| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| bundle | `%s` |\n", in.Evaluation.BundleDigest)
	fmt.Fprintf(&b, "| deployment contract | `%s` |\n", in.Evaluation.ContractDigest)
	fmt.Fprintf(&b, "| bundle files | %d |\n", len(in.Evaluation.BundleFiles))

	fmt.Fprintf(&b, "\n## Migration\n\n")
	fmt.Fprintf(&b, "| | |\n|---|---|\n")
	if in.OldRelease != nil && in.OldRelease.Migration.Head != "" {
		fmt.Fprintf(&b, "| head | %s → %s |\n", in.OldRelease.Migration.Head, in.Migration.Head)
	} else {
		fmt.Fprintf(&b, "| head | %s |\n", in.Migration.Head)
	}
	fmt.Fprintf(&b, "| mode | %s |\n", in.Migration.Mode)
	fmt.Fprintf(&b, "| rollback safe | %t |\n", in.Migration.RollbackSafe)

	b.WriteString("\n## Authorization\n\n")
	b.WriteString("**Merging this PR authorizes the production promotion of this exact release.** ")
	b.WriteString("No other button is required. ")
	b.WriteString("This proposal was generated from deterministic eligibility evidence; AI review is advisory only.\n")
	return b.String()
}

func writeWarnings(b *strings.Builder, m manifest.MigrationSpec) {
	var warnings []string
	switch {
	case m.Mode == manifest.MigrationMaintenanceRequired:
		warnings = append(warnings, "**MIGRATION REQUIRES MAINTENANCE / DOWNTIME**")
	case m.Mode == manifest.MigrationIrreversible:
		warnings = append(warnings, "**MIGRATION IS IRREVERSIBLE — this transition cannot be undone**")
	}
	if m.Mode != manifest.MigrationIrreversible && !m.RollbackSafe {
		warnings = append(warnings, "**rollbackSafe: false — auto rollback is disabled for this release**")
	}
	if len(warnings) == 0 {
		return
	}
	for _, w := range warnings {
		fmt.Fprintf(b, "> [!WARNING]\n> %s\n\n", w)
	}
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func sortedArtifactNames(m map[string]release.ResolvedArtifact) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}
