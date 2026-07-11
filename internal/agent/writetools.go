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
			Description: "CREATE a NEW file within the worktree with the given contents. Use this ONLY to create a new file; to change an EXISTING file use edit_file instead. Creating a new file is allowed; OVERWRITING an existing file requires approval (denied on unattended runs). path is relative to the worktree root.",
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
			Description: "CHANGE an EXISTING file by replacing ONE unique snippet. old_string must match a snippet that appears EXACTLY once in the file — include enough surrounding context (whole lines around the change) to make it unique; it is replaced by new_string. Use this to modify a file that already exists; to CREATE a new file use write_file instead. PREFER this over rewriting a whole file. path is relative to the worktree root.",
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
			// EXACT match is primary. Weak local models frequently reproduce a
			// snippet with slightly different per-line indentation than the file, so
			// on a non-unique EXACT count we attempt ONE fallback: match with each
			// line's leading/trailing whitespace normalized, and apply the edit ONLY
			// if that normalized match is UNIQUE (else keep today's guidance). This
			// forgives whitespace WITHOUT weakening the unique-match guarantee.
			wsTolerant := false
			matchStart, matchLen := -1, 0
			switch strings.Count(string(cur), in.OldString) {
			case 0:
				if start, length, ok := uniqueWSMatch(string(cur), in.OldString); ok {
					matchStart, matchLen, wsTolerant = start, length, true
					break
				}
				return "NOT performed: old_string not found in " + filepath.ToSlash(rel) + " — re-read the file and copy an exact snippet", nil
			case 1:
				matchStart = strings.Index(string(cur), in.OldString)
				matchLen = len(in.OldString)
			default:
				if start, length, ok := uniqueWSMatch(string(cur), in.OldString); ok {
					matchStart, matchLen, wsTolerant = start, length, true
					break
				}
				return fmt.Sprintf("NOT performed: old_string is not unique in %s — include more surrounding context", filepath.ToSlash(rel)), nil
			}
			if d, reason := pol.Decide(Action{Kind: ActWrite, Path: rel, Exists: true}); d != Allow {
				return fmt.Sprintf("NOT performed (%s): %s", d, reason), nil
			}
			// Splice at the matched byte span (works for both the exact and the
			// whitespace-tolerant path — the latter matched against the file's ACTUAL
			// bytes, so we replace exactly those bytes).
			updated := string(cur[:matchStart]) + in.NewString + string(cur[matchStart+matchLen:])
			wf, werr := r.OpenFile(rel, os.O_WRONLY|os.O_TRUNC, 0o644)
			if werr != nil {
				return "", werr
			}
			defer wf.Close()
			if _, werr = wf.WriteString(updated); werr != nil {
				return "", werr
			}
			note := ""
			if wsTolerant {
				note = " via whitespace-tolerant match (exact old_string differed only in per-line whitespace)"
			}
			return fmt.Sprintf("edited %s (1 replacement, %d→%d bytes)%s", filepath.ToSlash(rel), len(cur), len(updated), note), nil
		},
	}

	return []Tool{write, del, edit}, nil
}

// uniqueWSMatch finds old within content ignoring per-line leading/trailing
// whitespace, returning the byte span [start, start+length) of the ACTUAL match
// in content (so the caller splices the file's real bytes, not the model's
// approximation). It succeeds ONLY when exactly ONE whitespace-normalized match
// exists — the unique-match guarantee is preserved; an ambiguous normalized
// match returns ok=false so the caller falls back to today's guidance.
//
// Approach: normalize both content and old the SAME way (trim each line's outer
// whitespace, keep line structure), find matches in the normalized content, and
// for the single match map its normalized line span back to the corresponding
// raw byte span in content. Matching is anchored to whole lines, which is how a
// weak model's indentation drift actually manifests.
func uniqueWSMatch(content, old string) (start, length int, ok bool) {
	oldLines := splitLinesKeepEnds(old)
	// A single-line old_string with no newline: normalize and scan line by line.
	cLines := splitLinesKeepEnds(content)
	normOld := make([]string, len(oldLines))
	for i, l := range oldLines {
		normOld[i] = strings.TrimSpace(l)
	}
	// Precompute byte offsets of each content line and its normalized form.
	offsets := make([]int, len(cLines)+1)
	normC := make([]string, len(cLines))
	pos := 0
	for i, l := range cLines {
		offsets[i] = pos
		normC[i] = strings.TrimSpace(l)
		pos += len(l)
	}
	offsets[len(cLines)] = pos

	n := len(normOld)
	if n == 0 {
		return 0, 0, false
	}
	matchIdx := -1
	count := 0
	for i := 0; i+n <= len(normC); i++ {
		hit := true
		for j := 0; j < n; j++ {
			if normC[i+j] != normOld[j] {
				hit = false
				break
			}
		}
		if hit {
			count++
			matchIdx = i
		}
	}
	if count != 1 {
		return 0, 0, false // not found, or ambiguous → refuse (unique-match guarantee holds)
	}
	start = offsets[matchIdx]
	// End at the end of the last matched line's CONTENT (drop a trailing newline
	// on the final line so a "\n}"-style edit replaces up to and including the
	// brace but not the line break the model didn't ask to remove).
	last := cLines[matchIdx+n-1]
	end := offsets[matchIdx+n-1] + len(strings.TrimRight(last, "\r\n"))
	return start, end - start, true
}

// splitLinesKeepEnds splits s into lines, KEEPING the trailing "\n" on each line
// (so byte offsets reconstruct s exactly). The final line has no newline unless
// s ends with one.
func splitLinesKeepEnds(s string) []string {
	var out []string
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			return out
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
}
