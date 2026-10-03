package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStateFileFollowsTheInstallRoot: the coding agent's audit trail, ask queue and
// traces are data. They used to hang off ~/.local-offload whatever `home` said, so a
// node whose install root had moved to a data drive kept writing them on the OS drive
// (register C-92). They hang off the install root now.
func TestStateFileFollowsTheInstallRoot(t *testing.T) {
	base := filepath.Join(t.TempDir(), "local-offload")
	if got, want := StateFile(base, "agent-asks.jsonl"), filepath.Join(base, "agent-asks.jsonl"); got != want {
		t.Fatalf("StateFile(%q) = %q, want %q", base, got, want)
	}
	if got, want := DefaultAuditPath(base), filepath.Join(base, "agent-audit.jsonl"); got != want {
		t.Fatalf("DefaultAuditPath(%q) = %q, want %q", base, got, want)
	}
}

// TestStateFileWithoutAnInstallRootKeepsTheOldDefault: a caller with no config in hand
// gets exactly what every release before this one wrote, so nothing that already
// finds its trail there loses it.
func TestStateFileWithoutAnInstallRootKeepsTheOldDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	want := filepath.Join(home, ".local-offload", "agent-audit.jsonl")
	if got := DefaultAuditPath(""); got != want {
		t.Fatalf("DefaultAuditPath(\"\") = %q, want %q", got, want)
	}
}

// TestStateFileNeverResolvesAgainstTheWorkingDirectory: with no user home to anchor
// on, config.DefaultBase() hands back the bare ".local-offload". A trail written
// there lands wherever the process happens to run, possibly inside the worktree the
// audit trail must stay outside of, so a relative root is no root: the answer is ""
// and Build refuses the grant that needed a trail.
func TestStateFileNeverResolvesAgainstTheWorkingDirectory(t *testing.T) {
	t.Setenv("USERPROFILE", "")
	t.Setenv("HOME", "")
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	if got := StateFile(".local-offload", "agent-audit.jsonl"); got != "" {
		t.Fatalf("StateFile(relative, ...) = %q with no user home, want \"\"", got)
	}
	if got := DefaultAuditPath(".local-offload"); got != "" {
		t.Fatalf("DefaultAuditPath(relative) = %q with no user home, want \"\"", got)
	}
}

// legacyHome points the user home at a temp directory and returns the earlier release's
// state directory under it, with name already written there.
func legacyHome(t *testing.T, name string, asDir bool) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	old := filepath.Join(home, ".local-offload", name)
	if asDir {
		if err := os.MkdirAll(old, 0o755); err != nil {
			t.Fatal(err)
		}
		return old
	}
	if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte(`{"event":"earlier"}`+string(rune(10))), 0o600); err != nil {
		t.Fatal(err)
	}
	return old
}

// TestDefaultStateFileSaysWhenAnEarlierTrailIsLeftBehind: the audit trail, ask queue and
// traces used to sit under ~/.local-offload whatever `home` said. A node that sets (or
// already has) a `home` writes them under it now, so the append-only trail splits in two on
// upgrade: the old entries stay where they are and nothing carries them over. The run says
// so, once, naming both files, rather than leaving the operator to find a half-empty trail.
func TestDefaultStateFileSaysWhenAnEarlierTrailIsLeftBehind(t *testing.T) {
	for _, c := range []struct {
		name  string
		isDir bool
	}{{"agent-audit.jsonl", false}, {"agent-asks.jsonl", false}, {"agent-traces", true}} {
		t.Run(c.name, func(t *testing.T) {
			old := legacyHome(t, c.name, c.isDir)
			base := filepath.Join(t.TempDir(), "local-offload")
			var buf bytes.Buffer
			got := DefaultStateFile(base, c.name, &buf)
			if want := filepath.Join(base, c.name); got != want {
				t.Fatalf("DefaultStateFile = %q, want %q", got, want)
			}
			note := buf.String()
			for _, want := range []string{c.name, got, old, "not moved"} {
				if !strings.Contains(note, want) {
					t.Errorf("the note must contain %q:\n%s", want, note)
				}
			}
			// A second resolution in the same process (every browse run of a long-lived door
			// asks again) must not repeat it.
			DefaultStateFile(base, c.name, &buf)
			if n := strings.Count(buf.String(), old); n != 1 {
				t.Errorf("the note was written %d times, want once:\n%s", n, buf.String())
			}
		})
	}
}

// TestDefaultStateFileIsSilentWhenThereIsNothingToSay: no earlier file, the install root is
// the user home's own, or no root is configured at all.
func TestDefaultStateFileIsSilentWhenThereIsNothingToSay(t *testing.T) {
	var buf bytes.Buffer
	// No earlier file.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	DefaultStateFile(filepath.Join(t.TempDir(), "local-offload"), "agent-audit.jsonl", &buf)

	// An earlier file, but this run writes the very same one: no root, or the default root.
	legacyHome(t, "agent-audit.jsonl", false)
	DefaultStateFile("", "agent-audit.jsonl", &buf)
	DefaultStateFile(filepath.Join(os.Getenv("HOME"), ".local-offload"), "agent-audit.jsonl", &buf)
	// The install root is a different path to the very same file (a link, a second name):
	// the file identity decides, not the spelling.
	other := filepath.Join(t.TempDir(), "agent-audit.jsonl")
	if err := os.Link(filepath.Join(os.Getenv("HOME"), ".local-offload", "agent-audit.jsonl"), other); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	DefaultStateFile(filepath.Dir(other), "agent-audit.jsonl", &buf)
	// A nil writer is a caller that does not want notes.
	DefaultStateFile(filepath.Join(t.TempDir(), "elsewhere"), "agent-audit.jsonl", nil)
	if buf.Len() != 0 {
		t.Errorf("nothing to report, but the note was written:\n%s", buf.String())
	}
}

// TestDefaultAuditPathSaysItToo: the door that hands Build the browse audit path (agent_run,
// agent_delegate) resolves it through DefaultAuditPath, so a long-lived door says it once.
func TestDefaultAuditPathSaysItToo(t *testing.T) {
	old := legacyHome(t, "agent-audit.jsonl", false)
	var buf bytes.Buffer
	prev := noteWriter
	noteWriter = &buf
	t.Cleanup(func() { noteWriter = prev })
	base := filepath.Join(t.TempDir(), "local-offload")
	if got := DefaultAuditPath(base); got != filepath.Join(base, "agent-audit.jsonl") {
		t.Fatalf("DefaultAuditPath = %q", got)
	}
	if !strings.Contains(buf.String(), old) {
		t.Errorf("DefaultAuditPath must say that the earlier trail at %s was left behind:\n%s", old, buf.String())
	}
}
