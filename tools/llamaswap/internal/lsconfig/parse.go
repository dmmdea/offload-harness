// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package lsconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// EnvVarConfigPath overrides config discovery. Set it to point the CLI at a
// llama-swap YAML on a host whose layout differs from the default candidates.
const EnvVarConfigPath = "LLAMASWAP_YAML"

// defaultConfigCandidates are probed in order when EnvVarConfigPath is unset.
// The list is a convenience, not a requirement: every command that takes a
// config accepts an explicit path, and DefaultConfigPath returns an error
// naming the env var when nothing is found, rather than guessing.
var defaultConfigCandidates = []string{
	`C:\llama-swap\llama-swap.yaml`,
	`C:/llama-swap/llama-swap.yaml`,
	"/etc/llama-swap/config.yaml",
	"/etc/llama-swap/llama-swap.yaml",
}

// DefaultConfigPath resolves the live llama-swap config. Order: the
// LLAMASWAP_YAML env var, then the first existing default candidate.
func DefaultConfigPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv(EnvVarConfigPath)); p != "" {
		return p, nil
	}
	for _, c := range defaultConfigCandidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("no llama-swap config found; pass the path explicitly or set %s", EnvVarConfigPath)
}

// SeatKind classifies a model entry by the binary its cmd launches.
type SeatKind string

const (
	// SeatLlamaServer is a seat served by llama.cpp's llama-server. Every
	// GGUF/context/flag check in this package applies.
	SeatLlamaServer SeatKind = "llama-server"
	// SeatNonLlamaServer is a seat served by some other inference binary
	// (whisper-server, a wrapper script, a remote shim). llama-server-specific
	// checks MUST skip these with an explicit note; one noisy false positive
	// kills lint adoption.
	SeatNonLlamaServer SeatKind = "non-llama-server"
	// SeatUnknown means the cmd could not be tokenized into a leading binary.
	SeatUnknown SeatKind = "unknown"
)

// InlineComment is a trailing `# ...` comment attached to a key inside a model
// block. Carried separately from the raw block so machine-readable output can
// point at the exact key an operator annotated.
type InlineComment struct {
	Key  string `json:"key"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// Model is one entry of the top-level models: mapping, with its source
// provenance. Cmd is kept in BOTH forms deliberately: CmdRaw is what an
// operator edits and greps for, CmdExpanded is what actually gets executed and
// what a live /running cmd string must be compared against.
type Model struct {
	ID          string            `json:"id"`
	CmdRaw      string            `json:"cmd_raw"`
	CmdExpanded string            `json:"cmd_expanded"`
	CmdStop     string            `json:"cmd_stop,omitempty"`
	Env         []string          `json:"env,omitempty"`
	Aliases     []string          `json:"aliases,omitempty"`
	TTL         *int              `json:"ttl,omitempty"`
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Proxy       string            `json:"proxy,omitempty"`
	CheckPath   string            `json:"check_endpoint,omitempty"`
	Unlisted    bool              `json:"unlisted,omitempty"`
	Filters     map[string]any    `json:"filters,omitempty"`
	Macros      map[string]string `json:"macros,omitempty"`

	// Provenance. Lines are 1-based and index File.Lines[n-1].
	KeyLine        int             `json:"key_line"`
	StartLine      int             `json:"start_line"`
	EndLine        int             `json:"end_line"`
	HeaderComment  string          `json:"header_comment,omitempty"`
	InlineComments []InlineComment `json:"inline_comments,omitempty"`

	// Classification and expansion trace.
	Seat       SeatKind    `json:"seat_kind"`
	Binary     string      `json:"binary"`
	Expansions []Expansion `json:"expansions,omitempty"`

	file *File
}

// RawBlock returns the model's source text verbatim, comments included, from
// the leading comment block through the last line of its value. Copied from
// the file bytes — never re-rendered from the parsed model.
func (m *Model) RawBlock() string {
	if m == nil || m.file == nil {
		return ""
	}
	return m.file.LineRange(m.StartLine, m.EndLine)
}

// BodyBlock is RawBlock without the leading comment block.
func (m *Model) BodyBlock() string {
	if m == nil || m.file == nil {
		return ""
	}
	return m.file.LineRange(m.KeyLine, m.EndLine)
}

// Names returns the id followed by every alias, which is the set a caller may
// legitimately address the seat by.
func (m *Model) Names() []string {
	out := make([]string, 0, 1+len(m.Aliases))
	out = append(out, m.ID)
	out = append(out, m.Aliases...)
	return out
}

// MatrixConfig is the solver-based routing block (llama-swap v239+).
type MatrixConfig struct {
	Vars       map[string]string `json:"vars,omitempty"`
	EvictCosts map[string]int    `json:"evict_costs,omitempty"`
	Sets       map[string]string `json:"sets,omitempty"`
	Line       int               `json:"line,omitempty"`
	VarLines   map[string]int    `json:"-"`
	CostLines  map[string]int    `json:"-"`
	SetLines   map[string]int    `json:"-"`
}

// GroupConfig is one entry of the legacy groups: block.
type GroupConfig struct {
	Name       string   `json:"name"`
	Members    []string `json:"members,omitempty"`
	Swap       *bool    `json:"swap,omitempty"`
	Exclusive  *bool    `json:"exclusive,omitempty"`
	Persistent *bool    `json:"persistent,omitempty"`
	Line       int      `json:"line,omitempty"`
}

// File is a parsed llama-swap config plus everything needed to talk about it
// honestly: the original bytes, its content hash, its mtime, and per-model
// source spans. Nothing here is ever written back.
type File struct {
	Path    string    `json:"path"`
	Sha256  string    `json:"sha256"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`

	Lines []string `json:"-"`
	Raw   []byte   `json:"-"`

	TopKeys    []string       `json:"top_keys"`
	TopKeyLine map[string]int `json:"-"`
	Generic    map[string]any `json:"-"`

	Macros     map[string]string `json:"macros,omitempty"`
	MacroOrder []string          `json:"-"`
	MacroLine  map[string]int    `json:"-"`

	Models     []*Model          `json:"models"`
	ModelIndex map[string]*Model `json:"-"`

	StartPort          int    `json:"start_port,omitempty"`
	GlobalTTL          *int   `json:"global_ttl,omitempty"`
	HealthCheckTimeout *int   `json:"health_check_timeout,omitempty"`
	StorePath          string `json:"store_path,omitempty"`
	APIKeyCount        int    `json:"api_key_count"`

	Matrix      *MatrixConfig                `json:"matrix,omitempty"`
	Groups      []GroupConfig                `json:"groups,omitempty"`
	Profiles    map[string]map[string]string `json:"profiles,omitempty"`
	Selectors   map[string]any               `json:"selectors,omitempty"`
	HookPreload []string                     `json:"hook_preload,omitempty"`

	// ExpandErrors records macro-resolution failures (cycles, undefined names)
	// found while expanding model cmds. Parsing does not fail on them; lint
	// reports them, because a config with a bad macro still needs explaining.
	ExpandErrors []string `json:"expand_errors,omitempty"`
}

// LineRange returns source lines [start,end] (1-based, inclusive) joined with
// "\n". Out-of-range bounds are clamped.
func (f *File) LineRange(start, end int) string {
	if f == nil || len(f.Lines) == 0 {
		return ""
	}
	if start < 1 {
		start = 1
	}
	if end > len(f.Lines) {
		end = len(f.Lines)
	}
	if end < start {
		return ""
	}
	return strings.Join(f.Lines[start-1:end], "\n")
}

// Resolve finds a model by id or by alias (exact match, case-sensitive — the
// llama-swap router is case-sensitive too).
func (f *File) Resolve(name string) (*Model, bool) {
	if f == nil {
		return nil, false
	}
	if m, ok := f.ModelIndex[name]; ok {
		return m, true
	}
	for _, m := range f.Models {
		for _, a := range m.Aliases {
			if a == name {
				return m, true
			}
		}
	}
	return nil, false
}

// LoadOptions tunes parsing. The zero value uses the process environment for
// ${env.VAR} expansion, which is what an operator wants interactively — with
// the caveat that a service running under SYSTEM has a different environment,
// which lint reports rather than pretends away.
type LoadOptions struct {
	LookupEnv func(string) (string, bool)
}

// Load reads and parses a llama-swap config. The file is opened READ-ONLY and
// is never reopened for writing anywhere in this package.
func Load(path string, opts LoadOptions) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var mtime time.Time
	var size int64
	if st, err := os.Stat(path); err == nil {
		mtime = st.ModTime()
		size = st.Size()
	}
	f, err := ParseBytes(raw, path, opts)
	if err != nil {
		return nil, err
	}
	f.ModTime = mtime
	f.Size = size
	return f, nil
}

// ParseBytes parses config bytes that are already in memory (a corpus member
// read once and hashed, a test fixture). path is used only for messages.
func ParseBytes(raw []byte, path string, opts LoadOptions) (*File, error) {
	sum := sha256.Sum256(raw)
	f := &File{
		Path:       path,
		Sha256:     hex.EncodeToString(sum[:]),
		Size:       int64(len(raw)),
		Raw:        raw,
		Lines:      splitLines(raw),
		TopKeyLine: map[string]int{},
		Macros:     map[string]string{},
		MacroLine:  map[string]int{},
		ModelIndex: map[string]*Model{},
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil, fmt.Errorf("parse %s: empty document", path)
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse %s: top level must be a mapping, got %s", path, kindName(root.Kind))
	}

	// Generic decode for schema validation. Round-tripped through JSON so the
	// value tree carries JSON types (float64, map[string]any) the way the
	// validator expects, instead of yaml's int/map[string]interface{} mix.
	if generic, err := genericTree(root); err == nil {
		f.Generic = generic
	}

	topValue := map[string]*yaml.Node{}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		f.TopKeys = append(f.TopKeys, k.Value)
		f.TopKeyLine[k.Value] = k.Line
		topValue[k.Value] = v
	}

	if n := topValue["macros"]; n != nil && n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			f.Macros[k.Value] = v.Value
			f.MacroOrder = append(f.MacroOrder, k.Value)
			f.MacroLine[k.Value] = k.Line
		}
	}
	if n := topValue["startPort"]; n != nil {
		f.StartPort = atoiDefault(n.Value, 0)
	}
	if n := topValue["globalTTL"]; n != nil {
		v := atoiDefault(n.Value, 0)
		f.GlobalTTL = &v
	}
	if n := topValue["healthCheckTimeout"]; n != nil {
		v := atoiDefault(n.Value, 0)
		f.HealthCheckTimeout = &v
	}
	if n := topValue["store"]; n != nil && n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "path" {
				f.StorePath = n.Content[i+1].Value
			}
		}
	}
	if n := topValue["apiKeys"]; n != nil && n.Kind == yaml.SequenceNode {
		f.APIKeyCount = len(n.Content)
	}
	if n := topValue["hooks"]; n != nil {
		f.HookPreload = hookPreload(n)
	}
	if n := topValue["profiles"]; n != nil && n.Kind == yaml.MappingNode {
		f.Profiles = map[string]map[string]string{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			pins := map[string]string{}
			pv := n.Content[i+1]
			if pv.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(pv.Content); j += 2 {
					pins[pv.Content[j].Value] = pv.Content[j+1].Value
				}
			}
			f.Profiles[n.Content[i].Value] = pins
		}
	}
	if n := topValue["selectors"]; n != nil {
		if tree, err := genericNode(n); err == nil {
			if m, ok := tree.(map[string]any); ok {
				f.Selectors = m
			}
		}
	}
	if n := topValue["matrix"]; n != nil && n.Kind == yaml.MappingNode {
		f.Matrix = parseMatrix(n, f.TopKeyLine["matrix"])
	}
	if n := topValue["groups"]; n != nil && n.Kind == yaml.MappingNode {
		f.Groups = parseGroups(n)
	}

	modelsNode := topValue["models"]
	if modelsNode == nil {
		return nil, fmt.Errorf("parse %s: no models: block", path)
	}
	if modelsNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse %s: models: must be a mapping, got %s", path, kindName(modelsNode.Kind))
	}

	modelsKeyLine := f.TopKeyLine["models"]
	prevEnd := modelsKeyLine
	for i := 0; i+1 < len(modelsNode.Content); i += 2 {
		k, v := modelsNode.Content[i], modelsNode.Content[i+1]
		m := parseModel(f, k, v, prevEnd)
		f.Models = append(f.Models, m)
		f.ModelIndex[m.ID] = m
		prevEnd = m.EndLine
	}

	exp := NewExpander(f.Macros, opts.LookupEnv)
	for _, m := range f.Models {
		me := exp
		if len(m.Macros) > 0 {
			merged := make(map[string]string, len(f.Macros)+len(m.Macros))
			for k, v := range f.Macros {
				merged[k] = v
			}
			for k, v := range m.Macros {
				merged[k] = v
			}
			me = NewExpander(merged, opts.LookupEnv)
		}
		expanded, subs, errs := me.Expand(m.CmdRaw)
		m.CmdExpanded = expanded
		m.Expansions = subs
		for _, e := range errs {
			f.ExpandErrors = append(f.ExpandErrors, fmt.Sprintf("model %q: %s", m.ID, e))
		}
		spec := ParseCmd(expanded)
		m.Binary = spec.Binary
		m.Seat = ClassifySeat(spec.Binary)
	}

	return f, nil
}

func parseModel(f *File, key, val *yaml.Node, prevEnd int) *Model {
	m := &Model{ID: key.Value, KeyLine: key.Line, file: f}
	m.EndLine = maxLine(val, key.Line)
	m.StartLine = headerStart(f.Lines, key.Line, prevEnd)
	if m.StartLine < key.Line {
		m.HeaderComment = f.LineRange(m.StartLine, key.Line-1)
	}
	if val.Kind != yaml.MappingNode {
		return m
	}
	for i := 0; i+1 < len(val.Content); i += 2 {
		k, v := val.Content[i], val.Content[i+1]
		if txt := strings.TrimSpace(v.LineComment); txt != "" {
			m.InlineComments = append(m.InlineComments, InlineComment{Key: k.Value, Line: v.Line, Text: txt})
		} else if txt := strings.TrimSpace(k.LineComment); txt != "" {
			m.InlineComments = append(m.InlineComments, InlineComment{Key: k.Value, Line: k.Line, Text: txt})
		}
		switch k.Value {
		case "cmd":
			m.CmdRaw = v.Value
		case "cmdStop":
			m.CmdStop = v.Value
		case "name":
			m.Name = v.Value
		case "description":
			m.Description = v.Value
		case "proxy":
			m.Proxy = v.Value
		case "checkEndpoint":
			m.CheckPath = v.Value
		case "unlisted":
			m.Unlisted = v.Value == "true"
		case "ttl":
			ttl := atoiDefault(v.Value, 0)
			m.TTL = &ttl
		case "env":
			m.Env = stringSeq(v)
		case "aliases":
			m.Aliases = stringSeq(v)
		case "macros":
			m.Macros = map[string]string{}
			if v.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(v.Content); j += 2 {
					m.Macros[v.Content[j].Value] = v.Content[j+1].Value
				}
			}
		case "filters":
			if tree, err := genericNode(v); err == nil {
				if mm, ok := tree.(map[string]any); ok {
					m.Filters = mm
				}
			}
		}
	}
	return m
}

// headerStart walks backwards from the line above keyLine over CONTIGUOUS
// comment lines, stopping at the first blank line, the first non-comment line,
// or floor (the previous model's last line / the models: key).
//
// Stopping at a blank line is the load-bearing part. Operator configs put
// several unrelated comment blocks in the gap between two seats — a note about
// a model that was deleted, a commented-out stanza kept for flag archaeology,
// and then the actual header for the seat that follows — separated by blank
// lines. Swallowing all of them would attribute one seat's reasoning to
// another, which is worse than attributing none: `seat log` prints this block
// as the WHY behind a flag change.
//
// This is also why yaml.v3's HeadComment is not used: it strips the '#'
// markers and collapses the blank-line structure that carries the grouping.
func headerStart(lines []string, keyLine, floor int) int {
	start := keyLine
	for n := keyLine - 1; n > floor; n-- {
		if n-1 < 0 || n-1 >= len(lines) {
			break
		}
		if !strings.HasPrefix(strings.TrimSpace(lines[n-1]), "#") {
			break
		}
		start = n
	}
	return start
}

func maxLine(n *yaml.Node, fallback int) int {
	if n == nil {
		return fallback
	}
	best := n.Line
	if best < fallback {
		best = fallback
	}
	for _, c := range n.Content {
		if l := maxLine(c, best); l > best {
			best = l
		}
	}
	// A multi-line scalar's Line is its first line; add the remaining lines.
	if n.Kind == yaml.ScalarNode && strings.Contains(n.Value, "\n") {
		if l := n.Line + strings.Count(strings.TrimRight(n.Value, "\n"), "\n"); l > best {
			best = l
		}
	}
	return best
}

func parseMatrix(n *yaml.Node, line int) *MatrixConfig {
	mc := &MatrixConfig{
		Vars: map[string]string{}, EvictCosts: map[string]int{}, Sets: map[string]string{},
		VarLines: map[string]int{}, CostLines: map[string]int{}, SetLines: map[string]int{},
		Line: line,
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		switch k.Value {
		case "vars":
			for j := 0; j+1 < len(v.Content); j += 2 {
				mc.Vars[v.Content[j].Value] = v.Content[j+1].Value
				mc.VarLines[v.Content[j].Value] = v.Content[j].Line
			}
		case "evict_costs":
			for j := 0; j+1 < len(v.Content); j += 2 {
				mc.EvictCosts[v.Content[j].Value] = atoiDefault(v.Content[j+1].Value, 0)
				mc.CostLines[v.Content[j].Value] = v.Content[j].Line
			}
		case "sets":
			for j := 0; j+1 < len(v.Content); j += 2 {
				mc.Sets[v.Content[j].Value] = v.Content[j+1].Value
				mc.SetLines[v.Content[j].Value] = v.Content[j].Line
			}
		}
	}
	return mc
}

func parseGroups(n *yaml.Node) []GroupConfig {
	var out []GroupConfig
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		g := GroupConfig{Name: k.Value, Line: k.Line}
		for j := 0; j+1 < len(v.Content); j += 2 {
			gk, gv := v.Content[j], v.Content[j+1]
			switch gk.Value {
			case "members":
				g.Members = stringSeq(gv)
			case "swap":
				b := gv.Value == "true"
				g.Swap = &b
			case "exclusive":
				b := gv.Value == "true"
				g.Exclusive = &b
			case "persistent":
				b := gv.Value == "true"
				g.Persistent = &b
			}
		}
		out = append(out, g)
	}
	return out
}

func hookPreload(n *yaml.Node) []string {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value != "on_startup" {
			continue
		}
		s := n.Content[i+1]
		for j := 0; j+1 < len(s.Content); j += 2 {
			if s.Content[j].Value == "preload" {
				return stringSeq(s.Content[j+1])
			}
		}
	}
	return nil
}

func stringSeq(n *yaml.Node) []string {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	out := make([]string, 0, len(n.Content))
	for _, c := range n.Content {
		out = append(out, c.Value)
	}
	return out
}

func genericTree(root *yaml.Node) (map[string]any, error) {
	v, err := genericNode(root)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("top level is not a mapping")
	}
	return m, nil
}

// genericNode decodes a node into JSON-typed Go values. The yaml->JSON
// round-trip is deliberate: the schema validator matches on JSON types, and
// yaml.v3 hands back int/map[string]interface{} where draft-07 expects
// float64/map[string]any.
func genericNode(n *yaml.Node) (any, error) {
	var intermediate any
	if err := n.Decode(&intermediate); err != nil {
		return nil, err
	}
	buf, err := json.Marshal(intermediate)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(buf, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func splitLines(raw []byte) []string {
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func atoiDefault(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	neg := false
	if s[0] == '-' {
		neg = true
		s = s[1:]
	} else if s[0] == '+' {
		s = s[1:]
	}
	if s == "" {
		return def
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	if neg {
		return -n
	}
	return n
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.DocumentNode:
		return "document"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	}
	return "unknown"
}

// SortedKeys is a small helper so report output is deterministic.
func SortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RelOrBase renders a corpus path compactly relative to base when possible.
func RelOrBase(base, p string) string {
	if rel, err := filepath.Rel(base, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(p)
}
