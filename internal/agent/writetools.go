package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// WriteTools builds the Phase-2 MUTATING tools (write_file, delete_file),
// confined to worktreeRoot via os.Root (the same kernel-enforced, fail-closed,
// reparse-safe containment used for reads) AND gated by the deny→ask→allow
// policy broker (the cage). Every mutation is brokered + audited; a non-Allow
// decision returns a "NOT performed" tool result (defer-not-crash) — the file is
// untouched and the model can react. These tools are registered ONLY when the
// caller opts into write capability; P0/P1 stay read-only.
func WriteTools(worktreeRoot string, pol *Policy) ([]Tool, error) {
	absRoot, err := filepath.Abs(worktreeRoot)
	if err != nil {
		return nil, err
	}
	s := &scope{root: absRoot}

	write := Tool{
		ToolSpec: ToolSpec{
			Name:        "write_file",
			Description: "Write text to a file within the worktree. Creating a NEW file is allowed; OVERWRITING an existing file requires approval (denied on unattended runs). path is relative to the worktree root.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file path relative to the worktree root"},"content":{"type":"string","description":"file contents"}},"required":["path","content"]}`),
		},
		Exec: func(_ context.Context, args string) (string, error) {
			var in struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			_ = json.Unmarshal([]byte(args), &in)
			if strings.TrimSpace(in.Path) == "" {
				return "", fmt.Errorf("write_file requires a path")
			}
			r, rel, err := s.open(in.Path) // os.Root: rejects .., absolute, reparse escapes (fail-closed)
			if err != nil {
				return "", err
			}
			defer r.Close()
			exists := false
			if _, e := r.Stat(rel); e == nil {
				exists = true
			}
			// NOTE: a Stat that ERRORS (incl. an os.Root escape, or a race) is treated
			// as "not yet existing" here, but that is SAFE because the actual write
			// below uses O_EXCL — if the path turns out to exist (or escapes), the
			// create fails and we re-broker / return the error. O_EXCL, not Stat, is
			// the authoritative existence + containment guard.
			if d, reason := pol.Decide(Action{Kind: ActWrite, Path: rel, Exists: exists}); d != Allow {
				return fmt.Sprintf("NOT performed (%s): %s", d, reason), nil
			}
			if dir := filepath.Dir(rel); dir != "." && dir != "" {
				if err := r.MkdirAll(dir, 0o755); err != nil {
					return "", err
				}
			}
			// O_EXCL closes the Stat→Create TOCTOU: if the file raced into existence
			// after the "new → allow" decision, re-broker as an overwrite (deny on
			// unattended runs) instead of silently truncating it.
			f, err := r.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if errors.Is(err, fs.ErrExist) {
				if d, reason := pol.Decide(Action{Kind: ActWrite, Path: rel, Exists: true}); d != Allow {
					return fmt.Sprintf("NOT performed (%s): %s", d, reason), nil
				}
				f, err = r.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) // approved overwrite (attended only)
			}
			if err != nil {
				return "", err
			}
			defer f.Close()
			n, err := f.WriteString(in.Content)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes to %s", n, filepath.ToSlash(rel)), nil
		},
	}

	del := Tool{
		ToolSpec: ToolSpec{
			Name:        "delete_file",
			Description: "Delete a file within the worktree. Always requires approval (denied on unattended runs). path is relative to the worktree root.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file path relative to the worktree root"}},"required":["path"]}`),
		},
		Exec: func(_ context.Context, args string) (string, error) {
			var in struct {
				Path string `json:"path"`
			}
			_ = json.Unmarshal([]byte(args), &in)
			if strings.TrimSpace(in.Path) == "" {
				return "", fmt.Errorf("delete_file requires a path")
			}
			r, rel, err := s.open(in.Path)
			if err != nil {
				return "", err
			}
			defer r.Close()
			exists := false
			if _, e := r.Stat(rel); e == nil {
				exists = true
			}
			if d, reason := pol.Decide(Action{Kind: ActDelete, Path: rel, Exists: exists}); d != Allow {
				return fmt.Sprintf("NOT performed (%s): %s", d, reason), nil
			}
			if err := r.Remove(rel); err != nil {
				return "", err
			}
			return fmt.Sprintf("deleted %s", filepath.ToSlash(rel)), nil
		},
	}

	edit := Tool{
		ToolSpec: ToolSpec{
			Name:        "edit_file",
			Description: "Modify an EXISTING file by replacing ONE exact, unique snippet. old_string must appear EXACTLY once in the file (include enough surrounding context to make it unique); it is replaced by new_string. PREFER this over rewriting a whole file. To create a new file use write_file. path is relative to the worktree root.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file path relative to the worktree root"},"old_string":{"type":"string","description":"exact snippet to find; must be unique in the file"},"new_string":{"type":"string","description":"replacement text"}},"required":["path","old_string","new_string"]}`),
		},
		Exec: func(_ context.Context, args string) (string, error) {
			var in struct {
				Path      string `json:"path"`
				OldString string `json:"old_string"`
				NewString string `json:"new_string"`
			}
			_ = json.Unmarshal([]byte(args), &in)
			if strings.TrimSpace(in.Path) == "" {
				return "", fmt.Errorf("edit_file requires a path")
			}
			if in.OldString == "" {
				return "NOT performed: old_string is empty; use write_file to create or fully overwrite", nil
			}
			r, rel, err := s.open(in.Path)
			if err != nil {
				return "", err
			}
			defer r.Close()
			rf, err := r.OpenFile(rel, os.O_RDONLY, 0)
			if err != nil {
				return fmt.Sprintf("NOT performed: cannot open %s (%v); use write_file to create it", filepath.ToSlash(rel), err), nil
			}
			cur, rerr := io.ReadAll(rf)
			rf.Close()
			if rerr != nil {
				return "", rerr
			}
			switch strings.Count(string(cur), in.OldString) {
			case 0:
				return "NOT performed: old_string not found in " + filepath.ToSlash(rel) + " — re-read the file and copy an exact snippet", nil
			case 1:
				// ok, unique
			default:
				return fmt.Sprintf("NOT performed: old_string is not unique in %s — include more surrounding context", filepath.ToSlash(rel)), nil
			}
			if d, reason := pol.Decide(Action{Kind: ActWrite, Path: rel, Exists: true}); d != Allow {
				return fmt.Sprintf("NOT performed (%s): %s", d, reason), nil
			}
			updated := strings.Replace(string(cur), in.OldString, in.NewString, 1)
			wf, werr := r.OpenFile(rel, os.O_WRONLY|os.O_TRUNC, 0o644)
			if werr != nil {
				return "", werr
			}
			defer wf.Close()
			if _, werr = wf.WriteString(updated); werr != nil {
				return "", werr
			}
			return fmt.Sprintf("edited %s (1 replacement, %d→%d bytes)", filepath.ToSlash(rel), len(cur), len(updated)), nil
		},
	}

	return []Tool{write, del, edit}, nil
}
