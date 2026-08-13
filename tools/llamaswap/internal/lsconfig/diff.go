// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package lsconfig

import (
	"fmt"
	"sort"
	"strings"
)

// FlagDelta is one changed flag between two command lines.
type FlagDelta struct {
	Flag string `json:"flag"`
	// Kind is added | removed | changed.
	Kind string `json:"kind"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// String renders a delta the way seat log and drift print it.
func (d FlagDelta) String() string {
	switch d.Kind {
	case "added":
		return "+ " + d.Flag + valueSuffix(d.To)
	case "removed":
		return "- " + d.Flag + valueSuffix(d.From)
	default:
		return "~ " + d.Flag + " " + emptyDash(d.From) + " -> " + emptyDash(d.To)
	}
}

func valueSuffix(v string) string {
	if v == "" {
		return ""
	}
	return " " + v
}

func emptyDash(v string) string {
	if v == "" {
		return "(set)"
	}
	return v
}

// FieldDelta is a changed non-cmd model field (ttl, aliases, env, name...).
type FieldDelta struct {
	Field string `json:"field"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
}

// ModelDiff is the semantic difference for one model between two configs.
type ModelDiff struct {
	Model string `json:"model"`
	// Status is added | removed | changed | unchanged.
	Status        string       `json:"status"`
	BinaryFrom    string       `json:"binary_from,omitempty"`
	BinaryTo      string       `json:"binary_to,omitempty"`
	FlagDeltas    []FlagDelta  `json:"flag_deltas,omitempty"`
	FieldDeltas   []FieldDelta `json:"field_deltas,omitempty"`
	CommentChange bool         `json:"comment_changed,omitempty"`
	// CommentFrom/To are the model's leading comment block on each side. Kept
	// verbatim because that block IS the operator's reason for the change; a
	// flag delta without it is a fact without a motive.
	CommentFrom string   `json:"comment_from,omitempty"`
	CommentTo   string   `json:"comment_to,omitempty"`
	SeatFrom    SeatKind `json:"seat_from,omitempty"`
	SeatTo      SeatKind `json:"seat_to,omitempty"`
}

// Changed reports whether anything semantic moved (comments alone do not
// count as a semantic change, but they are still reported).
func (d ModelDiff) Changed() bool {
	return d.Status != "unchanged" || len(d.FlagDeltas) > 0 || len(d.FieldDeltas) > 0
}

// ConfigDiff is a whole-file semantic diff.
type ConfigDiff struct {
	APath     string       `json:"a_path"`
	BPath     string       `json:"b_path"`
	ASha      string       `json:"a_sha256"`
	BSha      string       `json:"b_sha256"`
	Identical bool         `json:"identical"`
	Models    []ModelDiff  `json:"models"`
	TopLevel  []FieldDelta `json:"top_level,omitempty"`
}

// DiffConfigs computes a semantic diff between two parsed configs. Order is
// a=before, b=after.
func DiffConfigs(a, b *File) *ConfigDiff {
	d := &ConfigDiff{APath: a.Path, BPath: b.Path, ASha: a.Sha256, BSha: b.Sha256}
	d.Identical = a.Sha256 == b.Sha256

	d.TopLevel = diffTopLevel(a, b)

	seen := map[string]bool{}
	for _, am := range a.Models {
		seen[am.ID] = true
		bm, ok := b.ModelIndex[am.ID]
		if !ok {
			d.Models = append(d.Models, ModelDiff{
				Model: am.ID, Status: "removed", BinaryFrom: am.Binary,
				SeatFrom: am.Seat, CommentFrom: am.HeaderComment,
			})
			continue
		}
		d.Models = append(d.Models, diffModel(am, bm))
	}
	for _, bm := range b.Models {
		if seen[bm.ID] {
			continue
		}
		d.Models = append(d.Models, ModelDiff{
			Model: bm.ID, Status: "added", BinaryTo: bm.Binary,
			SeatTo: bm.Seat, CommentTo: bm.HeaderComment,
		})
	}
	sort.SliceStable(d.Models, func(i, j int) bool { return d.Models[i].Model < d.Models[j].Model })
	return d
}

func diffModel(a, b *Model) ModelDiff {
	md := ModelDiff{
		Model: a.ID, Status: "unchanged",
		BinaryFrom: a.Binary, BinaryTo: b.Binary,
		SeatFrom: a.Seat, SeatTo: b.Seat,
	}
	md.FlagDeltas = DiffCmds(a.CmdExpanded, b.CmdExpanded)
	md.FieldDeltas = diffModelFields(a, b)
	if normalizeComment(a.HeaderComment) != normalizeComment(b.HeaderComment) {
		md.CommentChange = true
		md.CommentFrom = a.HeaderComment
		md.CommentTo = b.HeaderComment
	}
	if len(md.FlagDeltas) > 0 || len(md.FieldDeltas) > 0 || a.Binary != b.Binary {
		md.Status = "changed"
	}
	return md
}

// DiffCmds computes the per-flag delta between two command lines. Port values
// are normalized on both sides so a runtime-assigned port never reads as
// drift; that is the ONE field llama-swap is supposed to rewrite.
func DiffCmds(aCmd, bCmd string) []FlagDelta {
	as := NormalizePortValues(ParseCmd(aCmd))
	bs := NormalizePortValues(ParseCmd(bCmd))
	return diffSpecs(as, bs)
}

func diffSpecs(as, bs CmdSpec) []FlagDelta {
	aMap := flagValueMap(as)
	bMap := flagValueMap(bs)
	var out []FlagDelta
	for _, name := range SortedKeys(aMap) {
		bv, ok := bMap[name]
		if !ok {
			out = append(out, FlagDelta{Flag: name, Kind: "removed", From: aMap[name]})
			continue
		}
		if bv != aMap[name] {
			out = append(out, FlagDelta{Flag: name, Kind: "changed", From: aMap[name], To: bv})
		}
	}
	for _, name := range SortedKeys(bMap) {
		if _, ok := aMap[name]; !ok {
			out = append(out, FlagDelta{Flag: name, Kind: "added", To: bMap[name]})
		}
	}
	if as.Binary != bs.Binary {
		out = append([]FlagDelta{{Flag: "<binary>", Kind: "changed", From: as.Binary, To: bs.Binary}}, out...)
	}
	aPos, bPos := strings.Join(as.Positionals, " "), strings.Join(bs.Positionals, " ")
	if aPos != bPos {
		out = append(out, FlagDelta{Flag: "<positional args>", Kind: "changed", From: aPos, To: bPos})
	}
	return out
}

// flagValueMap collapses repeated flags to the LAST occurrence, matching how
// llama-server and whisper-server both resolve duplicates.
func flagValueMap(spec CmdSpec) map[string]string {
	out := map[string]string{}
	for _, f := range spec.Flags {
		out[f.Name] = strings.Join(f.Values, " ")
	}
	return out
}

func diffModelFields(a, b *Model) []FieldDelta {
	var out []FieldDelta
	cmp := func(field, av, bv string) {
		if av != bv {
			out = append(out, FieldDelta{Field: field, From: av, To: bv})
		}
	}
	cmp("ttl", ttlString(a.TTL), ttlString(b.TTL))
	cmp("aliases", strings.Join(a.Aliases, ","), strings.Join(b.Aliases, ","))
	cmp("env", strings.Join(a.Env, ","), strings.Join(b.Env, ","))
	cmp("name", a.Name, b.Name)
	cmp("description", a.Description, b.Description)
	cmp("cmdStop", a.CmdStop, b.CmdStop)
	cmp("proxy", a.Proxy, b.Proxy)
	cmp("checkEndpoint", a.CheckPath, b.CheckPath)
	cmp("unlisted", fmt.Sprint(a.Unlisted), fmt.Sprint(b.Unlisted))
	return out
}

func ttlString(t *int) string {
	if t == nil {
		return ""
	}
	return fmt.Sprint(*t)
}

func diffTopLevel(a, b *File) []FieldDelta {
	var out []FieldDelta
	cmp := func(field, av, bv string) {
		if av != bv {
			out = append(out, FieldDelta{Field: field, From: av, To: bv})
		}
	}
	cmp("startPort", intString(a.StartPort), intString(b.StartPort))
	cmp("globalTTL", ttlString(a.GlobalTTL), ttlString(b.GlobalTTL))
	cmp("healthCheckTimeout", ttlString(a.HealthCheckTimeout), ttlString(b.HealthCheckTimeout))
	cmp("store.path", a.StorePath, b.StorePath)
	cmp("apiKeys(count)", intString(a.APIKeyCount), intString(b.APIKeyCount))
	cmp("routing", routingStyle(a), routingStyle(b))
	for _, name := range SortedKeys(a.Macros) {
		bv, ok := b.Macros[name]
		if !ok {
			out = append(out, FieldDelta{Field: "macros." + name, From: a.Macros[name]})
			continue
		}
		cmp("macros."+name, a.Macros[name], bv)
	}
	for _, name := range SortedKeys(b.Macros) {
		if _, ok := a.Macros[name]; !ok {
			out = append(out, FieldDelta{Field: "macros." + name, To: b.Macros[name]})
		}
	}
	if a.Matrix != nil && b.Matrix != nil {
		for _, k := range SortedKeys(a.Matrix.Sets) {
			cmp("matrix.sets."+k, a.Matrix.Sets[k], b.Matrix.Sets[k])
		}
		for _, k := range SortedKeys(b.Matrix.Sets) {
			if _, ok := a.Matrix.Sets[k]; !ok {
				out = append(out, FieldDelta{Field: "matrix.sets." + k, To: b.Matrix.Sets[k]})
			}
		}
	}
	return out
}

func routingStyle(f *File) string {
	switch {
	case f.Matrix != nil && len(f.Groups) > 0:
		return "matrix+groups"
	case f.Matrix != nil:
		return "matrix"
	case len(f.Groups) > 0:
		return "groups"
	}
	return "none"
}

func intString(v int) string {
	if v == 0 {
		return ""
	}
	return fmt.Sprint(v)
}

func normalizeComment(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// UnifiedDiff renders a standard unified diff between two texts. Used by
// `config apply` and `seat try` to SHOW an operator exactly what a change
// would do — the output is a report, never an instruction executed against a
// file.
func UnifiedDiff(aName, bName string, a, b []string, context int) string {
	if context < 0 {
		context = 3
	}
	ops := diffOps(a, b)
	if len(ops) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", aName, bName)
	i := 0
	for i < len(ops) {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := i - context
		if start < 0 {
			start = 0
		}
		end := i
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			run := 0
			for end+run < len(ops) && ops[end+run].kind == ' ' {
				run++
			}
			if run > context*2 || end+run >= len(ops) {
				break
			}
			end += run
		}
		tail := end + context
		if tail > len(ops) {
			tail = len(ops)
		}
		aStart, bStart, aCount, bCount := 0, 0, 0, 0
		for j := 0; j < start; j++ {
			if ops[j].kind != '+' {
				aStart++
			}
			if ops[j].kind != '-' {
				bStart++
			}
		}
		for j := start; j < tail; j++ {
			if ops[j].kind != '+' {
				aCount++
			}
			if ops[j].kind != '-' {
				bCount++
			}
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", aStart+1, aCount, bStart+1, bCount)
		for j := start; j < tail; j++ {
			sb.WriteByte(byte(ops[j].kind))
			sb.WriteString(ops[j].text)
			sb.WriteByte('\n')
		}
		i = tail
	}
	return sb.String()
}

type diffOp struct {
	kind rune // ' ', '-', '+'
	text string
}

// diffOps is a straightforward LCS diff. Config files are hundreds of lines,
// so the O(n*m) table is fine and its output is exact — which matters when the
// diff is what an operator reads before touching a production service.
func diffOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var out []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, diffOp{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, diffOp{'-', a[i]})
			i++
		default:
			out = append(out, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		out = append(out, diffOp{'+', b[j]})
	}
	hasChange := false
	for _, o := range out {
		if o.kind != ' ' {
			hasChange = true
			break
		}
	}
	if !hasChange {
		return nil
	}
	return out
}
