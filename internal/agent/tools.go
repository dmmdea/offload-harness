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
				Description: "Read the contents of a file within the workspace. path is relative to the workspace root. Output is truncated at 256 KB.",
				Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file path relative to the workspace root"}},"required":["path"]}`),
			},
			Exec: s.readFile,
		},
	}

	if offload != nil {
		tools = append(tools, offloadTools(offload)...)
	}
	return tools, nil
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

func (s *scope) readFile(_ context.Context, args string) (string, error) {
	var in struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal([]byte(args), &in)
	if strings.TrimSpace(in.Path) == "" {
		return "", fmt.Errorf("read_file requires a path")
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
	// read error propagates instead of being swallowed.
	data, err := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxReadBytes {
		return string(data[:maxReadBytes]) + "\n…(truncated at 256 KB)", nil
	}
	return string(data), nil
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
