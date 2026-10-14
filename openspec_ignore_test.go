package main

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// TestOpenSpecGeneratedToolFilesStayUntracked pins the .gitignore lines for the
// files `openspec init` and `openspec update` generate for each coding tool (the
// skills and slash commands under .claude/ and .opencode/). They are regenerated
// from the tracked openspec/ tree, they would put tool prompt text into the
// public tree, and a tracked copy would drift from the tool version that wrote
// it. The openspec/ tree itself stays tracked, so the leak gate scans it like
// every other path (internal/leakgate TestScanTreeCoversTheOpenSpecTree).
func TestOpenSpecGeneratedToolFilesStayUntracked(t *testing.T) {
	if err := winexec.Command("git", "rev-parse", "--is-inside-work-tree").Run(); err != nil {
		t.Skip("not a git work tree (a source tarball): .gitignore is not consulted")
	}
	// The paths openspec 1.14.1 `init --tools claude,opencode --profile core` writes, as observed in a scratch
	// project; the singular .opencode/command/ is the spelling older opencode releases read.
	ignored := []string{
		".claude/skills/openspec-propose/SKILL.md",
		".claude/skills/openspec-apply-change/SKILL.md",
		".claude/skills/openspec-sync-specs/SKILL.md",
		".claude/commands/opsx/propose.md",
		".claude/commands/opsx/archive.md",
		".opencode/commands/opsx-propose.md",
		".opencode/command/opsx-propose.md",
		".opencode/skills/openspec-propose/SKILL.md",
	}
	for _, p := range ignored {
		// check-ignore exits 0 when the path is ignored, 1 when it is not.
		if err := winexec.Command("git", "check-ignore", "-q", "--no-index", p).Run(); err != nil {
			t.Errorf("%s is not ignored: OpenSpec's generated tool files must stay out of the tracked tree", p)
		}
	}
	tracked := []string{
		"openspec/config.yaml",
		"openspec/specs/gate/spec.md",
		"openspec/changes/add-gate/proposal.md",
		".claude/settings.json",
		".claude/commands/review.md",
		".claude/skills/review/SKILL.md",
		".opencode/agent/review.md",
		".opencode/commands/review.md",
		".opencode/skills/review/SKILL.md",
	}
	for _, p := range tracked {
		if err := winexec.Command("git", "check-ignore", "-q", "--no-index", p).Run(); err == nil {
			t.Errorf("%s is ignored: only OpenSpec's generated tool files may be ignored, the spec tree and other tool files stay visible", p)
		}
	}
}
