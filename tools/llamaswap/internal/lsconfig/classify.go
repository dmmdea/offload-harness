// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package lsconfig

import (
	"path"
	"strconv"
	"strings"
)

// Flag is one parsed command-line flag occurrence with the values that
// followed it.
type Flag struct {
	Name   string   `json:"name"`
	Values []string `json:"values,omitempty"`
	// Ordinal is the flag's position among all flags, 0-based. Diffs report it
	// so a reordered-but-equivalent cmd is visibly distinguishable from a
	// changed one.
	Ordinal int `json:"ordinal"`
}

// String renders the flag the way it appeared on the command line.
func (f Flag) String() string {
	if len(f.Values) == 0 {
		return f.Name
	}
	return f.Name + " " + strings.Join(f.Values, " ")
}

// CmdSpec is a tokenized inference-server command line.
type CmdSpec struct {
	Binary      string   `json:"binary"`
	Tokens      []string `json:"-"`
	Flags       []Flag   `json:"flags"`
	Positionals []string `json:"positionals,omitempty"`
}

// Get returns the LAST occurrence of a flag (last-wins is how llama-server and
// whisper-server both treat repeated flags).
func (c CmdSpec) Get(name string) (Flag, bool) {
	var out Flag
	found := false
	for _, f := range c.Flags {
		if f.Name == name {
			out, found = f, true
		}
	}
	return out, found
}

// GetAny returns the last occurrence of whichever of names is present, trying
// them in order of preference for reporting only — all are searched.
func (c CmdSpec) GetAny(names ...string) (Flag, bool) {
	var out Flag
	found := false
	for _, f := range c.Flags {
		for _, n := range names {
			if f.Name == n {
				out, found = f, true
			}
		}
	}
	return out, found
}

// FlagNames returns the distinct flag names in first-appearance order.
func (c CmdSpec) FlagNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range c.Flags {
		if seen[f.Name] {
			continue
		}
		seen[f.Name] = true
		out = append(out, f.Name)
	}
	return out
}

// ParseCmd tokenizes a command string into a binary, ordered flags, and
// positionals.
//
// Two llama-swap-specific behaviors are honored:
//   - A multi-line cmd (YAML block scalar) may carry `#` comments; llama-swap
//     strips them, so we do too. A single-line cmd never does — its trailing
//     comment was already removed by the YAML parser, and a bare '#' inside a
//     single-line value is a legitimate argument character.
//   - Quoting is shell-like (single and double quotes group tokens), because
//     that is how the value reaches the OS process spawner.
func ParseCmd(cmd string) CmdSpec {
	spec := CmdSpec{}
	spec.Tokens = TokenizeCmd(cmd)
	if len(spec.Tokens) == 0 {
		return spec
	}
	spec.Binary = spec.Tokens[0]
	ordinal := 0
	for i := 1; i < len(spec.Tokens); i++ {
		tok := spec.Tokens[i]
		if !IsFlagToken(tok) {
			spec.Positionals = append(spec.Positionals, tok)
			continue
		}
		// --flag=value is a single token in some tools; split it so the
		// diff compares values, not spellings.
		if eq := strings.Index(tok, "="); eq > 1 && strings.HasPrefix(tok, "--") {
			spec.Flags = append(spec.Flags, Flag{Name: tok[:eq], Values: []string{tok[eq+1:]}, Ordinal: ordinal})
			ordinal++
			continue
		}
		f := Flag{Name: tok, Ordinal: ordinal}
		ordinal++
		for i+1 < len(spec.Tokens) && !IsFlagToken(spec.Tokens[i+1]) {
			f.Values = append(f.Values, spec.Tokens[i+1])
			i++
		}
		spec.Flags = append(spec.Flags, f)
	}
	return spec
}

// IsFlagToken reports whether a token starts a flag. A bare "-" and a negative
// number are values, not flags — without the numeric exclusion, `--temp -1`
// would parse as two flags and a diff would report phantom changes.
func IsFlagToken(tok string) bool {
	if len(tok) < 2 || tok[0] != '-' {
		return false
	}
	if _, err := strconv.ParseFloat(tok, 64); err == nil {
		return false
	}
	return true
}

// TokenizeCmd splits a command string shell-style.
func TokenizeCmd(cmd string) []string {
	if strings.Contains(cmd, "\n") {
		cmd = stripBlockComments(cmd)
	}
	var out []string
	var cur strings.Builder
	inTok := false
	var quote rune
	for _, r := range cmd {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
			inTok = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inTok {
				out = append(out, cur.String())
				cur.Reset()
				inTok = false
			}
		default:
			cur.WriteRune(r)
			inTok = true
		}
	}
	if inTok {
		out = append(out, cur.String())
	}
	return out
}

func stripBlockComments(cmd string) string {
	lines := strings.Split(cmd, "\n")
	for i, ln := range lines {
		if idx := strings.Index(ln, "#"); idx >= 0 {
			// Only treat '#' as a comment when it starts a token.
			if idx == 0 || ln[idx-1] == ' ' || ln[idx-1] == '\t' {
				lines[i] = ln[:idx]
			}
		}
	}
	return strings.Join(lines, "\n")
}

// llamaServerBinaries are basenames (extension stripped, lowercased) treated
// as llama.cpp's llama-server. Matching is on the BASENAME, never the full
// path: an operator running a self-built binary out of
// C:/llama.cpp-b10356/llama-server.exe and a distro one out of
// /usr/bin/llama-server are the same seat kind.
var llamaServerBinaries = map[string]bool{
	"llama-server": true,
	"llama_server": true,
	"llamaserver":  true,
	"server":       true, // llama.cpp's pre-rename binary name
}

// ClassifySeat decides whether a seat is served by llama-server. This is the
// whisper-server escape hatch in one function: everything downstream that
// knows about GGUF headers, -ngl, -c/--ctx-size, --jinja, or KV cache types
// asks this first and SKIPS non-llama-server seats with an explicit note
// rather than reporting a missing llama-server flag as a defect.
func ClassifySeat(binary string) SeatKind {
	base := BinaryBase(binary)
	if base == "" {
		return SeatUnknown
	}
	if llamaServerBinaries[base] {
		return SeatLlamaServer
	}
	return SeatNonLlamaServer
}

// BinaryBase returns the lowercased basename of a binary path with a trailing
// .exe/.cmd/.bat removed. Handles both separators because a Windows config is
// routinely written with forward slashes.
func BinaryBase(binary string) string {
	if binary == "" {
		return ""
	}
	b := strings.ReplaceAll(binary, "\\", "/")
	b = path.Base(b)
	b = strings.ToLower(b)
	for _, ext := range []string{".exe", ".cmd", ".bat", ".sh"} {
		if strings.HasSuffix(b, ext) {
			b = strings.TrimSuffix(b, ext)
			break
		}
	}
	return b
}

// ModelFileFlags are the flags whose values name a file that must exist on
// disk. Split by seat kind so a whisper seat's -m (a .bin) is still stat'ed
// while llama-server-only flags are not invented for it.
var (
	// LlamaServerFileFlags name weights/projector/draft files for llama-server.
	LlamaServerFileFlags = []string{"-m", "--model", "--mmproj", "-md", "--model-draft", "--lora", "--control-vector"}
	// WhisperFileFlags name weights/VAD models for whisper-server.
	WhisperFileFlags = []string{"-m", "--model", "-vm", "--vad-model", "-dtw"}
)

// FileFlagsFor returns the file-valued flags appropriate to a seat kind.
func FileFlagsFor(kind SeatKind, binary string) []string {
	if kind == SeatLlamaServer {
		return LlamaServerFileFlags
	}
	if strings.Contains(BinaryBase(binary), "whisper") {
		return WhisperFileFlags
	}
	// Unknown non-llama-server binary: only the universal -m/--model, which
	// every inference server in this family uses for weights.
	return []string{"-m", "--model"}
}

// ContextFlagsFor returns the context-size flags for a seat kind, or nil when
// the concept does not apply to that binary (whisper has no KV window in the
// llama.cpp sense).
func ContextFlagsFor(kind SeatKind) []string {
	if kind == SeatLlamaServer {
		return []string{"-c", "--ctx-size"}
	}
	return nil
}

// NormalizePortValues rewrites the value of every port-naming flag to a stable
// placeholder so a file cmd (with ${PORT} unresolved) and a live cmd (with the
// runtime-assigned port substituted) compare equal on everything BUT the port.
// Without this, every seat would report drift on the one field llama-swap is
// supposed to control.
func NormalizePortValues(spec CmdSpec) CmdSpec {
	out := spec
	out.Flags = make([]Flag, len(spec.Flags))
	copy(out.Flags, spec.Flags)
	for i, f := range out.Flags {
		if f.Name != "--port" && f.Name != "-p" {
			continue
		}
		vals := make([]string, len(f.Values))
		copy(vals, f.Values)
		for j := range vals {
			vals[j] = "<PORT>"
		}
		out.Flags[i].Values = vals
	}
	return out
}
