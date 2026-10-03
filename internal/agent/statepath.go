package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// StateFile is where the coding agent keeps one of its own files (the policy audit
// trail, the ask queue, per-goal traces): name under the harness install root base
// (config `home`, which every other derived store already follows), else under
// <user home>/.local-offload, which is where every release before the install root
// became a knob wrote them. "" when neither can be resolved.
//
// A base that is not absolute counts as no base. config.DefaultBase() answers a bare
// ".local-offload" when the user home cannot be resolved, and a path relative to the
// working directory may sit inside the very worktree these files must stay outside of
// (the audit-outside-the-worktree invariant); "" is what makes Build refuse a browse
// grant that has no trail.
func StateFile(base, name string) string {
	if base = strings.TrimSpace(base); base != "" && filepath.IsAbs(base) {
		return filepath.Join(base, name)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local-offload", name)
}

// noteWriter is where DefaultAuditPath says that an earlier trail was left behind: the
// process's stderr, the channel every door already logs to.
var noteWriter io.Writer = os.Stderr

// legacyNoted holds the notes already written by this process, so a long-lived door that
// resolves the same default for every browse run says it once.
var legacyNoted sync.Map

// DefaultStateFile is StateFile for a path the caller DEFAULTED (not one the operator
// named with a flag), and it says when that default moved under an earlier release's
// feet. Until the install root governed these files they all lived under
// <user home>/.local-offload, so a node whose `home` points elsewhere now writes a new
// file and leaves the old one where it was: the append-only trail is split in two and
// nothing carries the old entries over. When the earlier file or directory exists and is
// not the one this run writes, one note goes to warn (nil means no notes), once per
// process, naming both paths and what to do about it. The earlier history is never moved,
// merged or deleted here: that is the operator's call.
func DefaultStateFile(base, name string, warn io.Writer) string {
	p := StateFile(base, name)
	if warn == nil || p == "" {
		return p
	}
	note := legacyStateNote(name, p)
	if note == "" {
		return p
	}
	if _, dup := legacyNoted.LoadOrStore(note, struct{}{}); !dup {
		fmt.Fprintln(warn, "note: "+note)
	}
	return p
}

// legacyStateNote is the sentence DefaultStateFile writes, "" when there is nothing to say:
// no user home to look in, nothing at the earlier location, or the earlier location IS the
// file this run writes (no install root configured, or a root that is a link to the old one).
func legacyStateNote(name, current string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	old := filepath.Join(home, ".local-offload", name)
	oldInfo, err := os.Stat(old)
	if err != nil {
		return ""
	}
	if curInfo, err := os.Stat(current); err == nil && os.SameFile(oldInfo, curInfo) {
		return ""
	}
	return fmt.Sprintf("%s is now written to %s (under the harness install root); an earlier release wrote %s, "+
		"which is not moved or merged, so the history is split between the two. "+
		"Merge the old one into the new one by hand if you want a single history, then delete it to silence this note.",
		name, current, old)
}
