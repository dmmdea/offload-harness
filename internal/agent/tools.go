package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// OffloadFunc is the in-process offload capability injected by cmd/local-agent
// (wired to pipeline.RunTier, which is record=false → no ledger/cache/shadow/
// exemplar writes). Keeping it an injected func keeps this package free of any
// pipeline import and unit-testable with a fake. nil => no offload tools.
type OffloadFunc func(ctx context.Context, task, input string, params map[string]any) (result string, err error)

const maxReadBytes = 256 * 1024 // P0 read cap: keeps a single read from blowing the local context window

// ReadOnlyTools builds the Phase-0 tool set, scoped to root (the worktree):
// list_dir + read_file (both confined to root), plus the offload_* tools when
// an offloader is wired. It registers NO write/shell/network tool — P0 is
// read-only by construction; broader capability is gated to later phases behind
// the cage.
//
// Containment is OS-ENFORCED via os.Root: every file access goes through a root
// handle that refuses to escape — `..`, absolute paths, AND reparse points
// (symlinks / Windows directory junctions) are all rejected by the kernel-level
// openat-style traversal, and it FAILS CLOSED (any resolution error => the tool
// errors, never a silent fall-through). This replaces the earlier hand-rolled
// EvalSymlinks check, which a fresh-context review proved bypassable by a
// non-privileged Windows junction.
func ReadOnlyTools(root string, offload OffloadFunc) ([]Tool, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	s := &scope{root: absRoot}

	tools := []Tool{
		{
			ToolSpec: ToolSpec{
				Name:        "list_dir",
				Description: "List the files and subdirectories at a path within the workspace. path is relative to the workspace root.",
				Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"directory path relative to the workspace root (use \".\" for the root)"}},"required":["path"]}`),
			},
			Exec: s.listDir,
		},
		{
			ToolSpec: ToolSpec{
				Name:        "read_file",
				Description: "Read a file within the workspace as numbered lines (cat -n style). path is relative to the workspace root. Optional offset (1-indexed start line, default 1) and limit (number of lines, default 2000) read only a line range; a continuation hint tells you the offset for the next chunk. Pair with search_files to read just the lines around a match. Long lines are truncated; output has a 256 KB backstop.",
				Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file path relative to the workspace root"},"offset":{"type":"integer","description":"1-indexed start line (default 1)"},"limit":{"type":"integer","description":"number of lines to read (default 2000)"}},"required":["path"]}`),
			},
			Exec: s.readFile,
		},
	}

	searchTool, err := SearchTool(absRoot)
	if err != nil {
		return nil, err
	}
	tools = append(tools, searchTool)

	if offload != nil {
		tools = append(tools, offloadTools(offload)...)
		tools = append(tools, summarizeFileTool(s, offload))
	}
	return tools, nil
}

// summarizeFileTool digests a workspace file on the free offload cascade WITHOUT
// the file's bytes entering the agent transcript (file-as-external-memory): it
// reads the file under os.Root confinement (bounded by maxReadBytes) and returns
// only the short summary. Registered ONLY when an offloader is wired (see the
// offload != nil guard in ReadOnlyTools) since it has no fallback without one.
//
// Defer-not-crash: if the offload call fails, it returns a marker string (not an
// error) telling the agent to read_file ranged instead, so the run degrades
// gracefully rather than aborting the tool.
func summarizeFileTool(s *scope, offload OffloadFunc) Tool {
	return Tool{
		ToolSpec: ToolSpec{
			Name:        "summarize_file",
			Description: "Summarize a workspace file on a free local model WITHOUT reading its bytes into your context — use this to digest a big file. path is relative to the workspace root; optional max_points caps the summary length. Returns a short summary; on failure returns a marker telling you to read_file with offset/limit instead.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file path relative to the workspace root"},"max_points":{"type":"integer","description":"optional cap on summary bullet points"}},"required":["path"]}`),
		},
		Exec: func(ctx context.Context, args string) (string, error) {
			var in struct {
				Path      string `json:"path"`
				MaxPoints int    `json:"max_points"`
			}
			_ = json.Unmarshal([]byte(args), &in)
			if strings.TrimSpace(in.Path) == "" {
				return "", fmt.Errorf("summarize_file requires a path")
			}
			content, err := s.readBounded(in.Path)
			if err != nil {
				return "", err // includes os.Root escape rejection (../, absolute, symlink/junction)
			}
			if strings.TrimSpace(content) == "" {
				return fmt.Sprintf("(empty file %s — nothing to summarize)", in.Path), nil
			}
			params := map[string]any{}
			if in.MaxPoints > 0 {
				params["max_points"] = in.MaxPoints
			}
			summary, err := offload(ctx, "summarize", content, params)
			if err != nil {
				// Defer-not-crash: hand the agent a graceful fallback instead of a hard error.
				return fmt.Sprintf("could not summarize %s (%v); use read_file with offset/limit to read it directly instead", in.Path, err), nil
			}
			return summary, nil
		},
	}
}

// readBounded reads a workspace file through the os.Root handle, applying the same
// maxReadBytes ceiling readFile uses (io.LimitReader + io.ReadAll so a short Read
// can't silently truncate). It returns the escape/IO error unchanged so callers
// surface os.Root's fail-closed rejection.
func (s *scope) readBounded(rel string) (string, error) {
	r, name, err := s.open(rel)
	if err != nil {
		return "", err
	}
	defer r.Close()
	f, err := r.Open(name) // os.Root: fails closed on any escape (.., absolute, symlink/junction)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxReadBytes))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// scope confines file access to root using an OS root handle (os.Root).
type scope struct{ root string }

// open returns the root handle plus the validated relative name, or an error.
// It rejects absolute and volume-qualified inputs (e.g. "C:\\x", "C:x",
// "\\\\host\\share") up front for a clear message; os.Root then enforces the
// rest (.., reparse points) at the syscall layer. Caller must Close the *os.Root.
func (s *scope) open(rel string) (*os.Root, string, error) {
	if rel == "" {
		rel = "."
	}
	if filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return nil, "", fmt.Errorf("path must be relative to the workspace root (no absolute or volume-qualified paths): %q", rel)
	}
	r, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, "", fmt.Errorf("workspace root unavailable: %w", err)
	}
	return r, rel, nil
}

func (s *scope) listDir(_ context.Context, args string) (string, error) {
	var in struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal([]byte(args), &in)
	r, rel, err := s.open(in.Path)
	if err != nil {
		return "", err
	}
	defer r.Close()
	f, err := r.Open(rel) // os.Root: fails closed on any escape (.., absolute, symlink/junction)
	if err != nil {
		return "", err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() {
			n += "/"
		}
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "(empty directory)", nil
	}
	return strings.Join(names, "\n"), nil
}

const (
	defaultReadLimit = 2000 // default number of lines returned by read_file
	maxLineChars     = 2000 // per-line character cap (rune-safe); over-long lines get " (line truncated)"
)

func (s *scope) readFile(_ context.Context, args string) (string, error) {
	var in struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"` // 1-indexed start line; 0/absent => 1
		Limit  int    `json:"limit"`  // number of lines; 0/absent => defaultReadLimit
	}
	_ = json.Unmarshal([]byte(args), &in)
	if strings.TrimSpace(in.Path) == "" {
		return "", fmt.Errorf("read_file requires a path")
	}
	offset := in.Offset
	if offset < 1 {
		offset = 1
	}
	limit := in.Limit
	if limit < 1 {
		limit = defaultReadLimit
	}
	r, rel, err := s.open(in.Path)
	if err != nil {
		return "", err
	}
	defer r.Close()
	f, err := r.Open(rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	// io.LimitReader + io.ReadAll: a single Read may under-fill, silently
	// truncating a small file; ReadAll loops until EOF or the cap, and a real
	// read error propagates instead of being swallowed. The 256 KB byte ceiling
	// stays as a backstop so a single huge line can't blow the context window.
	data, err := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
	if err != nil {
		return "", err
	}
	byteTruncated := false
	if len(data) > maxReadBytes {
		data = data[:maxReadBytes]
		byteTruncated = true
	}

	lines := strings.Split(string(data), "\n")
	total := len(lines)

	// offset past the last line => nothing to show.
	if offset > total {
		return fmt.Sprintf("(end of file — %d lines)", total), nil
	}

	start := offset - 1 // 0-indexed
	end := start + limit
	if end > total {
		end = total
	}

	var b strings.Builder
	for i := start; i < end; i++ {
		line := lines[i]
		// Per-line rune-safe cap: count runes and cut on a rune boundary.
		if utf8.RuneCountInString(line) > maxLineChars {
			cut := 0
			n := 0
			for idx := range line { // range over string yields rune-boundary byte indexes
				if n == maxLineChars {
					cut = idx
					break
				}
				n++
			}
			line = line[:cut] + " (line truncated)"
		}
		fmt.Fprintf(&b, "%d: %s", i+1, line)
		if i < end-1 {
			b.WriteByte('\n')
		}
	}
	out := b.String()

	// Continuation hint when the line window didn't reach EOF (or the byte
	// backstop cut the tail): tell the caller the next offset to resume from.
	if end < total || byteTruncated {
		out += fmt.Sprintf("\n(showing lines %d–%d of %d; use offset=%d to continue)", offset, end, total, end+1)
	}
	return out, nil
}

// offloadTools wraps the in-process offloader as four local tools. Each returns
// the offload Result JSON as the tool result; an offloader error propagates and
// the loop feeds it back as an is_error tool result (defer-not-crash).
func offloadTools(offload OffloadFunc) []Tool {
	mk := func(name, task, desc, schema string, build func(json.RawMessage) (string, map[string]any, error)) Tool {
		return Tool{
			ToolSpec: ToolSpec{Name: name, Description: desc, Schema: json.RawMessage(schema)},
			Exec: func(ctx context.Context, args string) (string, error) {
				input, params, err := build(json.RawMessage(args))
				if err != nil {
					return "", err
				}
				return offload(ctx, task, input, params)
			},
		}
	}
	return []Tool{
		mk("offload_summarize", "summarize",
			"Summarize text on a free local model (keeps tokens out of the agent's context). Returns {summary, bullets} or a defer.",
			`{"type":"object","properties":{"text":{"type":"string"},"max_points":{"type":"integer"}},"required":["text"]}`,
			func(a json.RawMessage) (string, map[string]any, error) {
				var in struct {
					Text      string `json:"text"`
					MaxPoints int    `json:"max_points"`
				}
				if err := json.Unmarshal(a, &in); err != nil {
					return "", nil, err
				}
				p := map[string]any{}
				if in.MaxPoints > 0 {
					p["max_points"] = in.MaxPoints
				}
				return in.Text, p, nil
			}),
		mk("offload_classify", "classify",
			"Classify text into one of the given labels on a free local model. Returns {label, confidence} or a defer.",
			`{"type":"object","properties":{"text":{"type":"string"},"labels":{"type":"array","items":{"type":"string"}}},"required":["text","labels"]}`,
			func(a json.RawMessage) (string, map[string]any, error) {
				var in struct {
					Text   string   `json:"text"`
					Labels []string `json:"labels"`
				}
				if err := json.Unmarshal(a, &in); err != nil {
					return "", nil, err
				}
				return in.Text, map[string]any{"labels": in.Labels}, nil
			}),
		mk("offload_triage", "triage",
			"Answer a yes/no/unsure question about text on a free local model. Returns {decision, reason} or a defer.",
			`{"type":"object","properties":{"text":{"type":"string"},"question":{"type":"string"}},"required":["text","question"]}`,
			func(a json.RawMessage) (string, map[string]any, error) {
				var in struct {
					Text     string `json:"text"`
					Question string `json:"question"`
				}
				if err := json.Unmarshal(a, &in); err != nil {
					return "", nil, err
				}
				return in.Text, map[string]any{"question": in.Question}, nil
			}),
		mk("offload_extract", "extract",
			"Extract structured fields from text on a free local model, constrained to a JSON schema. Returns the object or a defer.",
			`{"type":"object","properties":{"text":{"type":"string"},"schema":{"type":"object"}},"required":["text","schema"]}`,
			func(a json.RawMessage) (string, map[string]any, error) {
				var in struct {
					Text   string         `json:"text"`
					Schema map[string]any `json:"schema"`
				}
				if err := json.Unmarshal(a, &in); err != nil {
					return "", nil, err
				}
				return in.Text, map[string]any{"schema": in.Schema}, nil
			}),
	}
}
