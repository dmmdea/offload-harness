// Package exp is the pure core of the experiment (`exp`) and `replay` surfaces.
//
// NOT generated — hand-written and preserved across regeneration.
//
// WHY THIS PACKAGE EXISTS SEPARATELY FROM THE COMMANDS. A memory-knob sweep on this box
// varied virtual_vram_gb over 7 / 10 / 13 and donor_device over cpu / cuda:1; 7 OOM'd and 10
// succeeded. Arms like that usually require RESTARTING ComfyUI with different argv, and a
// restart wipes /history (`self.history = {}` in RAM). So the experiment can only exist as one
// object in an external store, and every piece of reasoning about it — how the cells expand,
// what an arm is called, which arms passed, whether a before/after delta is even attributable
// — has to be testable without a GPU, a server, or a database. That reasoning lives here.
//
// RULES ENCODED IN THIS PACKAGE, each from a real defect:
//   - Timing NEVER comes from log text or an s/it progress sample. ParseHistoryOutcome reads
//     only the execution_start / execution_success timestamps, which is the sole authoritative
//     source; a stale "Prompt executed in N seconds" line once produced a false "+49%
//     regression".
//   - A /prompt reply can carry node_errors on HTTP 200: ComfyUI validates each output branch
//     independently and only 400s when NO branch is good. ClassifySubmit returns a distinct
//     partial-accept outcome for that case; it is never success.
//   - A failed arm is a FIRST-CLASS ROW. BuildComparison emits a row for every arm, including
//     arms that OOM'd and arms that never ran — negative results are the highest-value
//     artifact of a memory sweep and the most aggressively discarded.
//   - Without the argv each side ran under, a duration change cannot be attributed to anything.
//     AttributeDelta says so out loud instead of printing a confident percentage.
package exp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"comfyui-pp-cli/internal/store"
)

// MaxArms caps an expansion. Every arm is a real render (30 s to 20 min on this box), so a
// mistyped cartesian product is a multi-hour footgun rather than a slow command; refusing is
// cheaper than interrupting.
const MaxArms = 256

// ---------------------------------------------------------------------------
// Addresses
// ---------------------------------------------------------------------------

// Address is a resolved slot address: which node, which input.
type Address struct {
	Raw    string
	NodeID string
	Input  string
}

// ParseAddress accepts `<node_id>.<input>` and the more explicit
// `<node_id>.inputs.<input>`. API-format graphs have flat inputs (a scalar widget value or a
// [node_id, slot] link), so a deeper path is always a typo and is rejected rather than
// silently creating a key no node reads.
func ParseAddress(raw string) (Address, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Address{}, errors.New("empty slot address: expected <node_id>.<input> or <node_id>.inputs.<input>")
	}
	parts := strings.Split(trimmed, ".")
	if len(parts) == 3 && parts[1] == "inputs" {
		parts = []string{parts[0], parts[2]}
	}
	if len(parts) != 2 {
		return Address{}, fmt.Errorf("invalid slot address %q: expected <node_id>.<input> or <node_id>.inputs.<input>", raw)
	}
	nodeID := strings.TrimSpace(parts[0])
	input := strings.TrimSpace(parts[1])
	if nodeID == "" || input == "" {
		return Address{}, fmt.Errorf("invalid slot address %q: node id and input name must both be non-empty", raw)
	}
	return Address{Raw: trimmed, NodeID: nodeID, Input: input}, nil
}

// ---------------------------------------------------------------------------
// --vary parsing
// ---------------------------------------------------------------------------

// Var is one varied dimension of an experiment.
//
// The json tags are load-bearing: this struct is serialised straight into the
// `vary` array of `exp new --json`, and without them Go emits the Go field names
// (Addr / Values) into a document that is snake_case everywhere else.
type Var struct {
	Addr   string   `json:"addr"`
	Values []string `json:"values"`
}

// ParseVary parses one `--vary <addr>=<v1>,<v2>,...` token.
//
// Only the FIRST '=' splits, so a value may contain '='. A comma inside a value is escaped as
// `\,`. Duplicate values are rejected: they would expand into two arms that are identical in
// every respect including their label, which makes the comparison table lie about how many
// distinct configurations were measured.
func ParseVary(spec string) (Var, error) {
	addrPart, valuePart, ok := strings.Cut(spec, "=")
	if !ok {
		return Var{}, fmt.Errorf("invalid --vary %q: expected <addr>=<v1>,<v2>,...", spec)
	}
	addr, err := ParseAddress(addrPart)
	if err != nil {
		return Var{}, fmt.Errorf("invalid --vary %q: %w", spec, err)
	}
	values, err := splitValues(valuePart)
	if err != nil {
		return Var{}, fmt.Errorf("invalid --vary %q: %w", spec, err)
	}
	seen := map[string]bool{}
	for _, v := range values {
		if seen[v] {
			return Var{}, fmt.Errorf("invalid --vary %q: duplicate value %q", spec, v)
		}
		seen[v] = true
	}
	return Var{Addr: addr.Raw, Values: values}, nil
}

// splitValues splits on unescaped commas, honoring `\,` as a literal comma.
func splitValues(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("no values given (expected at least one after '=')")
	}
	var out []string
	var cur strings.Builder
	escaped := false
	for _, r := range raw {
		switch {
		case escaped:
			if r != ',' && r != '\\' {
				cur.WriteRune('\\')
			}
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == ',':
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if escaped {
		cur.WriteRune('\\')
	}
	out = append(out, strings.TrimSpace(cur.String()))
	for i, v := range out {
		if v == "" {
			return nil, fmt.Errorf("value %d is empty (use \\, to embed a literal comma)", i+1)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Expansion
// ---------------------------------------------------------------------------

// Mode selects how the varied dimensions combine.
type Mode int

const (
	// Cartesian is every combination of every dimension (the default).
	Cartesian Mode = iota
	// Zip pairs the dimensions index-by-index; all dimensions must be the same length.
	Zip
)

// ParseMode maps the --zip flag to a Mode.
func ParseMode(zip bool) Mode {
	if zip {
		return Zip
	}
	return Cartesian
}

func (m Mode) String() string {
	if m == Zip {
		return "zip"
	}
	return "cartesian"
}

// Arm is one cell of the expansion: a label, and the value each varied address takes.
type Arm struct {
	Index  int               `json:"index"`
	Label  string            `json:"label"`
	Addrs  []string          `json:"addrs"`
	Values []string          `json:"values"`
	Vars   map[string]string `json:"vars"`
}

// Expand turns the varied dimensions into arms. Cartesian order is odometer order with the
// LAST dimension varying fastest, which keeps the slowest-changing knob (usually the one that
// needs a server restart) grouped together in the run order.
func Expand(vars []Var, mode Mode) ([]Arm, error) {
	if len(vars) == 0 {
		return nil, errors.New("no varied dimensions: pass at least one --vary <addr>=<v1>,<v2>")
	}
	seen := map[string]bool{}
	for _, v := range vars {
		if len(v.Values) == 0 {
			return nil, fmt.Errorf("dimension %q has no values", v.Addr)
		}
		if seen[v.Addr] {
			return nil, fmt.Errorf("duplicate --vary address %q", v.Addr)
		}
		seen[v.Addr] = true
	}

	var cells [][]string
	switch mode {
	case Zip:
		n := len(vars[0].Values)
		for _, v := range vars[1:] {
			if len(v.Values) != n {
				return nil, fmt.Errorf("--zip requires equal value counts: %s has %d, %s has %d",
					vars[0].Addr, n, v.Addr, len(v.Values))
			}
		}
		for i := 0; i < n; i++ {
			row := make([]string, len(vars))
			for j := range vars {
				row[j] = vars[j].Values[i]
			}
			cells = append(cells, row)
		}
	case Cartesian:
		total := 1
		for _, v := range vars {
			total *= len(v.Values)
			if total > MaxArms {
				return nil, fmt.Errorf("refusing to expand more than %d arms: every arm is a real render; narrow the sweep or split it into several experiments", MaxArms)
			}
		}
		idx := make([]int, len(vars))
		for c := 0; c < total; c++ {
			row := make([]string, len(vars))
			for j := range vars {
				row[j] = vars[j].Values[idx[j]]
			}
			cells = append(cells, row)
			for j := len(vars) - 1; j >= 0; j-- {
				idx[j]++
				if idx[j] < len(vars[j].Values) {
					break
				}
				idx[j] = 0
			}
		}
	default:
		return nil, fmt.Errorf("unknown expansion mode %d", int(mode))
	}

	if len(cells) > MaxArms {
		return nil, fmt.Errorf("refusing to expand more than %d arms: every arm is a real render; narrow the sweep or split it into several experiments", MaxArms)
	}

	addrs := make([]string, len(vars))
	for i, v := range vars {
		addrs[i] = v.Addr
	}
	labels := assignLabels(addrs, cells)

	arms := make([]Arm, 0, len(cells))
	for i, row := range cells {
		vals := make([]string, len(row))
		copy(vals, row)
		varMap := make(map[string]string, len(addrs))
		for j, addr := range addrs {
			varMap[addr] = row[j]
		}
		arms = append(arms, Arm{
			Index:  i,
			Label:  labels[i],
			Addrs:  append([]string(nil), addrs...),
			Values: vals,
			Vars:   varMap,
		})
	}
	return arms, nil
}

const maxLabelTokenLen = 24
const maxLabelLen = 96

// assignLabels builds a stable, filesystem-and-shell-safe label per cell and resolves
// collisions deterministically. Collisions are real: two distinct values can sanitize or
// truncate to the same token (`cuda:1` and `cuda/1`, or two long checkpoint paths sharing a
// prefix), and two arms sharing a label would silently overwrite one another in the
// comparison table — losing exactly the arm a sweep exists to find.
func assignLabels(addrs []string, cells [][]string) []string {
	labels := make([]string, len(cells))
	used := map[string]int{}
	for i, row := range cells {
		var parts []string
		for j, val := range row {
			parts = append(parts, shortAddr(addrs[j])+"="+sanitizeToken(val))
		}
		label := clampLabel(strings.Join(parts, "+"))
		if n, taken := used[label]; taken {
			n++
			used[label] = n
			label = fmt.Sprintf("%s#%d", label, n)
		} else {
			used[label] = 1
		}
		labels[i] = label
	}
	return labels
}

func shortAddr(addr string) string {
	parts := strings.Split(addr, ".")
	last := parts[len(parts)-1]
	return sanitizeToken(last)
}

// sanitizeToken keeps a label copy-pasteable: only [A-Za-z0-9._-] survive. Over-long values
// are truncated with a short hash suffix so two long values that share a prefix still differ.
func sanitizeToken(v string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				b.WriteRune('_')
				lastUnderscore = true
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		out = "empty"
	}
	if len(out) > maxLabelTokenLen {
		out = out[:maxLabelTokenLen-5] + "~" + shortHash(v)
	}
	return out
}

func clampLabel(label string) string {
	if len(label) <= maxLabelLen {
		return label
	}
	return label[:maxLabelLen-5] + "~" + shortHash(label)
}

func shortHash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:2])
}

// ---------------------------------------------------------------------------
// Graph materialisation
// ---------------------------------------------------------------------------

// PatchRecord describes one slot mutation, in the shape the `patch` table stores. Recording
// them is what makes a run reconstructable after the original graph file is gone.
type PatchRecord struct {
	Address   string `json:"address"`
	NodeID    string `json:"node_id"`
	Input     string `json:"input"`
	ClassType string `json:"class_type"`
	OldValue  any    `json:"old_value"`
	NewValue  any    `json:"new_value"`
}

// ApplyArm returns a COPY of g with the arm's values applied, plus one PatchRecord per slot.
// The input graph is never mutated: arms are materialised in a loop off one template, and an
// in-place edit would make every later arm inherit the earlier arm's values.
func ApplyArm(g store.APIGraph, arm Arm) (store.APIGraph, []PatchRecord, error) {
	out := CloneGraph(g)
	records := make([]PatchRecord, 0, len(arm.Addrs))
	for i, rawAddr := range arm.Addrs {
		if i >= len(arm.Values) {
			return nil, nil, fmt.Errorf("arm %q: address %q has no value", arm.Label, rawAddr)
		}
		addr, err := ParseAddress(rawAddr)
		if err != nil {
			return nil, nil, err
		}
		node, ok := out[addr.NodeID]
		if !ok {
			return nil, nil, fmt.Errorf("arm %q: node %q not found in graph (known node ids: %s)",
				arm.Label, addr.NodeID, strings.Join(sampleKeys(out, 10), ", "))
		}
		if node.Inputs == nil {
			return nil, nil, fmt.Errorf("arm %q: node %q (%s) has no inputs at all",
				arm.Label, addr.NodeID, node.ClassType)
		}
		old, exists := node.Inputs[addr.Input]
		if !exists {
			// Creating an unknown key would be a silent no-op: ComfyUI ignores inputs the
			// class does not declare, so the arm would render the UNPATCHED graph and be
			// reported as a measured configuration it never was.
			return nil, nil, fmt.Errorf("arm %q: node %q (%s) has no input %q (available: %s)",
				arm.Label, addr.NodeID, node.ClassType, addr.Input, strings.Join(sortedInputNames(node.Inputs), ", "))
		}
		newVal := CoerceValue(arm.Values[i])
		node.Inputs[addr.Input] = newVal
		out[addr.NodeID] = node
		records = append(records, PatchRecord{
			Address:   addr.Raw,
			NodeID:    addr.NodeID,
			Input:     addr.Input,
			ClassType: node.ClassType,
			OldValue:  old,
			NewValue:  newVal,
		})
	}
	return out, records, nil
}

// CloneGraph deep-copies the node map and each node's input map. Input VALUES are shared:
// nothing here mutates a value in place, only replaces it.
func CloneGraph(g store.APIGraph) store.APIGraph {
	out := make(store.APIGraph, len(g))
	for id, node := range g {
		inputs := make(map[string]interface{}, len(node.Inputs))
		for k, v := range node.Inputs {
			inputs[k] = v
		}
		meta := node.Meta
		if meta != nil {
			copied := make(map[string]interface{}, len(meta))
			for k, v := range meta {
				copied[k] = v
			}
			meta = copied
		}
		out[id] = store.APINode{ClassType: node.ClassType, Inputs: inputs, Meta: meta}
	}
	return out
}

// CoerceValue turns a command-line string into the JSON type ComfyUI expects. A widget typed
// as INT/FLOAT rejects the string "7" at validation, so `virtual_vram_gb=7` must land as a
// number while `donor_device=cpu` must stay a string.
func CoerceValue(s string) any {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return s
	}
	switch trimmed[0] {
	case '{', '[', '"', 't', 'f', 'n', '-', '+', '.', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
	default:
		return s
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return s
	}
	// Reject trailing garbage ("7 8" must stay a string, not become 7).
	if dec.More() {
		return s
	}
	if num, ok := v.(json.Number); ok {
		if i, err := num.Int64(); err == nil {
			return i
		}
		if f, err := num.Float64(); err == nil {
			return f
		}
		return s
	}
	return v
}

func sampleKeys(g store.APIGraph, limit int) []string {
	keys := make([]string, 0, len(g))
	for k := range g {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > limit {
		keys = append(keys[:limit:limit], "...")
	}
	return keys
}

func sortedInputNames(inputs map[string]interface{}) []string {
	names := make([]string, 0, len(inputs))
	for k := range inputs {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// ---------------------------------------------------------------------------
// Submit outcome (HTTP 200 + node_errors is NOT success)
// ---------------------------------------------------------------------------

// Submit outcome classes.
const (
	SubmitAccepted       = "accepted"
	SubmitPartialAccept  = "partial-accept"
	SubmitRejected       = "rejected"
	SubmitUnrecognisable = "unrecognisable"
)

// SubmitOutcome is the parsed /prompt reply.
type SubmitOutcome struct {
	PromptID      string          `json:"prompt_id,omitempty"`
	Number        float64         `json:"number,omitempty"`
	NodeErrors    json.RawMessage `json:"node_errors,omitempty"`
	HasNodeErrors bool            `json:"has_node_errors"`
	ErrorObject   json.RawMessage `json:"error,omitempty"`
}

// ParseSubmitResponse reads a /prompt reply body. A non-JSON body is not an error here: the
// caller still needs the status code and the verbatim text.
func ParseSubmitResponse(body []byte) SubmitOutcome {
	var out SubmitOutcome
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return out
	}
	if raw, ok := envelope["prompt_id"]; ok {
		_ = json.Unmarshal(raw, &out.PromptID)
	}
	if raw, ok := envelope["number"]; ok {
		_ = json.Unmarshal(raw, &out.Number)
	}
	if raw, ok := envelope["error"]; ok && !isJSONEmpty(raw) {
		out.ErrorObject = raw
	}
	if raw, ok := envelope["node_errors"]; ok && !isJSONEmpty(raw) {
		out.NodeErrors = raw
		out.HasNodeErrors = true
	}
	return out
}

// ClassifySubmit turns (status, body) into an outcome class.
//
// THE RULE THIS ENCODES: ComfyUI validates each OUTPUT BRANCH independently and returns 400
// only when NO branch is executable. A graph with one broken branch and one good branch comes
// back HTTP 200 with a populated node_errors map — the good branch really is queued, and the
// broken branch really will never render. Reporting that as success loses half the render;
// reporting it as failure loses the half that is running. It is its own outcome.
func ClassifySubmit(status int, outcome SubmitOutcome) string {
	switch {
	case status >= 200 && status < 300:
		if outcome.HasNodeErrors {
			return SubmitPartialAccept
		}
		if outcome.PromptID == "" {
			return SubmitUnrecognisable
		}
		return SubmitAccepted
	case status == 0:
		return SubmitUnrecognisable
	default:
		return SubmitRejected
	}
}

func isJSONEmpty(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t == "" || t == "null" || t == "{}" || t == "[]"
}

// ---------------------------------------------------------------------------
// /history parsing — the ONLY authoritative timing source
// ---------------------------------------------------------------------------

// HistoryOutcome is everything an arm's result needs, read from /history alone.
type HistoryOutcome struct {
	Found        bool            `json:"found"`
	Completed    bool            `json:"completed"`
	StatusStr    string          `json:"status_str,omitempty"`
	StartMS      int64           `json:"start_ms,omitempty"`
	SuccessMS    int64           `json:"success_ms,omitempty"`
	HasStart     bool            `json:"has_start"`
	HasSuccess   bool            `json:"has_success"`
	ErrorType    string          `json:"error_type,omitempty"`
	ErrorMessage string          `json:"error_message,omitempty"`
	ErrorNodeID  string          `json:"error_node_id,omitempty"`
	ErrorNode    string          `json:"error_node_type,omitempty"`
	ErrorRaw     json.RawMessage `json:"error_raw,omitempty"`
	CachedNodes  []string        `json:"cached_nodes,omitempty"`
}

// DurationMS returns the authoritative duration and whether it exists. It refuses an inverted
// pair rather than returning a negative number, mirroring store.SetRunTiming.
func (h HistoryOutcome) DurationMS() (int64, bool) {
	if !h.HasStart || !h.HasSuccess {
		return 0, false
	}
	if h.SuccessMS < h.StartMS {
		return 0, false
	}
	return h.SuccessMS - h.StartMS, true
}

// Terminal reports whether the run reached a final state and polling can stop.
func (h HistoryOutcome) Terminal() bool {
	if !h.Found {
		return false
	}
	if h.ErrorType != "" || h.StatusStr == "error" {
		return true
	}
	return h.Completed || h.HasSuccess
}

// FindHistoryEntry pulls the entry for promptID out of a /history or /history/{id} body, both
// of which are objects keyed by prompt id.
func FindHistoryEntry(body []byte, promptID string) (json.RawMessage, bool) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, false
	}
	if raw, ok := envelope[promptID]; ok {
		return raw, true
	}
	return nil, false
}

// ParseHistoryOutcome reads one /history entry.
//
// TIMING COMES FROM HERE AND NOWHERE ELSE. The status.messages array carries
// ["execution_start", {timestamp}] and ["execution_success", {timestamp}]; those two numbers
// are the only honest duration on this box. The server log's "Prompt executed in N seconds"
// line returns a STALE value mid-run (it once produced a false "+49% regression") and an s/it
// progress sample is a transient, not a rate.
func ParseHistoryOutcome(entry []byte) HistoryOutcome {
	out := HistoryOutcome{}
	var record struct {
		Status struct {
			StatusStr string            `json:"status_str"`
			Completed bool              `json:"completed"`
			Messages  []json.RawMessage `json:"messages"`
		} `json:"status"`
	}
	if err := json.Unmarshal(entry, &record); err != nil {
		return out
	}
	out.Found = true
	out.StatusStr = record.Status.StatusStr
	out.Completed = record.Status.Completed

	for _, raw := range record.Status.Messages {
		var pair []json.RawMessage
		if err := json.Unmarshal(raw, &pair); err != nil || len(pair) == 0 {
			continue
		}
		var kind string
		if err := json.Unmarshal(pair[0], &kind); err != nil {
			continue
		}
		var payload map[string]any
		if len(pair) > 1 {
			_ = json.Unmarshal(pair[1], &payload)
		}
		switch kind {
		case "execution_start":
			if ms, ok := timestampMS(payload); ok {
				out.StartMS, out.HasStart = ms, true
			}
		case "execution_success":
			if ms, ok := timestampMS(payload); ok {
				out.SuccessMS, out.HasSuccess = ms, true
			}
		case "execution_cached":
			out.CachedNodes = append(out.CachedNodes, stringList(payload["nodes"])...)
		case "execution_error", "execution_interrupted":
			if len(pair) > 1 {
				out.ErrorRaw = pair[1]
			}
			out.ErrorType, _ = payload["exception_type"].(string)
			out.ErrorMessage, _ = payload["exception_message"].(string)
			out.ErrorNodeID = asString(payload["node_id"])
			out.ErrorNode, _ = payload["node_type"].(string)
			if out.ErrorType == "" && kind == "execution_interrupted" {
				out.ErrorType = "ProcessInterrupted"
			}
		}
	}
	return out
}

func timestampMS(payload map[string]any) (int64, bool) {
	raw, ok := payload["timestamp"]
	if !ok {
		return 0, false
	}
	switch v := raw.(type) {
	case float64:
		ms := store.NormaliseEpochMS(v)
		return ms, ms > 0
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, false
		}
		ms := store.NormaliseEpochMS(f)
		return ms, ms > 0
	default:
		return 0, false
	}
}

func stringList(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s := asString(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

// ---------------------------------------------------------------------------
// Failure classification — the negative-result corpus
// ---------------------------------------------------------------------------

// Exit classes.
const (
	ExitOOM          = "oom"
	ExitMissingModel = "missing-model"
	ExitValidation   = "validation"
	ExitInterrupted  = "interrupted"
	ExitError        = "error"
	ExitTimeout      = "timeout"
)

// ClassifyFailure names a failure so a sweep's negative results are queryable instead of being
// one undifferentiated "it broke". The OOM class is the point of the whole exercise: on this
// box virtual_vram_gb=7 OOM'd and 10 did not, and the execution_error carrying that verdict
// lives in a RAM dict until the next restart — which the OOM itself usually causes.
func ClassifyFailure(excType, message string) string {
	hay := strings.ToLower(excType + " " + message)
	switch {
	case strings.Contains(hay, "outofmemory"),
		strings.Contains(hay, "out of memory"),
		strings.Contains(hay, "allocation on device"),
		strings.Contains(hay, "cuda error: out of memory"),
		strings.Contains(hay, "not enough memory"):
		return ExitOOM
	case strings.Contains(hay, "value not in list"),
		strings.Contains(hay, "not in []"),
		strings.Contains(hay, "no such file or directory"),
		strings.Contains(hay, "unable to find"):
		return ExitMissingModel
	case strings.Contains(hay, "interrupt"):
		return ExitInterrupted
	case strings.Contains(hay, "validation"),
		strings.Contains(hay, "invalid prompt"),
		strings.Contains(hay, "required input is missing"):
		return ExitValidation
	case strings.TrimSpace(hay) == "":
		return ""
	default:
		return ExitError
	}
}

// ---------------------------------------------------------------------------
// Comparison table
// ---------------------------------------------------------------------------

// Verdicts.
const (
	VerdictPass    = "PASS"
	VerdictFail    = "FAIL"
	VerdictPartial = "PARTIAL"
	VerdictPending = "PENDING"
	VerdictNotRun  = "NOT-RUN"
)

// RunFacts is what the store knows about the run an arm produced. Every field is optional:
// an arm that OOM'd before /history could record a duration still has to render as a row.
type RunFacts struct {
	PromptID     string `json:"prompt_id,omitempty"`
	ServerID     string `json:"server_id,omitempty"`
	State        string `json:"state,omitempty"`
	ExitClass    string `json:"exit_class,omitempty"`
	Completeness string `json:"completeness,omitempty"`
	DurationMS   int64  `json:"duration_ms,omitempty"`
	HasDuration  bool   `json:"has_duration"`
	NodeErrors   string `json:"node_errors,omitempty"`
	ErrorType    string `json:"error_type,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	Missing      bool   `json:"missing"`
}

// Row is one line of the comparison table — one per ARM, always.
type Row struct {
	Label      string            `json:"label"`
	Index      int               `json:"index"`
	Vars       map[string]string `json:"vars"`
	Values     []string          `json:"values"`
	Verdict    string            `json:"verdict"`
	State      string            `json:"state,omitempty"`
	ExitClass  string            `json:"exit_class,omitempty"`
	DurationMS int64             `json:"duration_ms,omitempty"`
	Duration   string            `json:"duration"`
	Relative   string            `json:"relative,omitempty"`
	PromptID   string            `json:"prompt_id,omitempty"`
	ServerID   string            `json:"server_id,omitempty"`
	Note       string            `json:"note,omitempty"`
	NodeErrors string            `json:"node_errors,omitempty"`
}

// Comparison is the assembled table plus its tallies.
type Comparison struct {
	VarAddrs   []string `json:"var_addrs"`
	Headers    []string `json:"headers"`
	Rows       []Row    `json:"rows"`
	Total      int      `json:"total"`
	Passed     int      `json:"passed"`
	Failed     int      `json:"failed"`
	Partial    int      `json:"partial"`
	Pending    int      `json:"pending"`
	NotRun     int      `json:"not_run"`
	BaselineMS int64    `json:"baseline_ms,omitempty"`
}

// Verdict maps a run's stored state onto a pass/fail verdict.
func Verdict(facts RunFacts, found bool) string {
	if !found || facts.Missing {
		return VerdictNotRun
	}
	switch strings.ToLower(strings.TrimSpace(facts.State)) {
	case "completed":
		if facts.NodeErrors != "" || strings.EqualFold(facts.Completeness, "partial") {
			return VerdictPartial
		}
		return VerdictPass
	case "failed", "error", "cancelled", "canceled":
		return VerdictFail
	case "partial-accept":
		return VerdictPartial
	case "":
		return VerdictNotRun
	default:
		// submitted, running, completed-outputs-pending, ...
		return VerdictPending
	}
}

// BuildComparison assembles the table.
//
// EVERY ARM GETS A ROW. An arm that OOM'd, an arm that was rejected at validation, and an arm
// that never ran each render as a first-class line with its own verdict. A sweep exists to
// find the boundary between what fits in VRAM and what does not; a table that shows only the
// arms that succeeded has deleted the answer.
func BuildComparison(arms []Arm, results map[string]RunFacts) Comparison {
	cmp := Comparison{Total: len(arms)}
	if len(arms) > 0 {
		cmp.VarAddrs = append([]string(nil), arms[0].Addrs...)
	}
	cmp.Headers = append([]string{"ARM"}, shortHeaders(cmp.VarAddrs)...)
	cmp.Headers = append(cmp.Headers, "STATE", "EXIT", "DURATION", "VS BEST", "VERDICT")

	var baseline int64
	for _, arm := range arms {
		facts, found := results[arm.Label]
		if found && Verdict(facts, found) == VerdictPass && facts.HasDuration {
			if baseline == 0 || facts.DurationMS < baseline {
				baseline = facts.DurationMS
			}
		}
	}
	cmp.BaselineMS = baseline

	for _, arm := range arms {
		facts, found := results[arm.Label]
		verdict := Verdict(facts, found)
		row := Row{
			Label:      arm.Label,
			Index:      arm.Index,
			Vars:       arm.Vars,
			Values:     append([]string(nil), arm.Values...),
			Verdict:    verdict,
			State:      facts.State,
			ExitClass:  facts.ExitClass,
			PromptID:   facts.PromptID,
			ServerID:   facts.ServerID,
			NodeErrors: facts.NodeErrors,
			Duration:   "-",
		}
		if !found {
			row.State = "not-run"
			row.Note = "never submitted"
		}
		if facts.HasDuration {
			row.DurationMS = facts.DurationMS
			row.Duration = FormatDuration(facts.DurationMS)
			if baseline > 0 && verdict == VerdictPass {
				row.Relative = fmt.Sprintf("%.2fx", float64(facts.DurationMS)/float64(baseline))
			}
		} else if verdict == VerdictFail || verdict == VerdictPartial {
			// A failed arm legitimately has no duration: it never reached
			// execution_success. Saying so beats an empty cell that reads as missing data.
			row.Duration = "n/a"
		}
		if row.Note == "" {
			row.Note = failureNote(facts)
		}
		switch verdict {
		case VerdictPass:
			cmp.Passed++
		case VerdictFail:
			cmp.Failed++
		case VerdictPartial:
			cmp.Partial++
		case VerdictPending:
			cmp.Pending++
		default:
			cmp.NotRun++
		}
		cmp.Rows = append(cmp.Rows, row)
	}
	return cmp
}

func failureNote(facts RunFacts) string {
	switch {
	case facts.ErrorType != "" && facts.ErrorMessage != "":
		return facts.ErrorType + ": " + firstLine(facts.ErrorMessage)
	case facts.ErrorType != "":
		return facts.ErrorType
	case facts.ErrorMessage != "":
		return firstLine(facts.ErrorMessage)
	case facts.NodeErrors != "":
		return "node_errors present (see --json for the verbatim map)"
	default:
		return ""
	}
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func shortHeaders(addrs []string) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, strings.ToUpper(shortAddr(a)))
	}
	return out
}

// TableRows renders the comparison as plain string cells for a tabwriter.
func (c Comparison) TableRows() [][]string {
	rows := make([][]string, 0, len(c.Rows))
	for _, r := range c.Rows {
		cells := []string{r.Label}
		cells = append(cells, r.Values...)
		state := r.State
		if state == "" {
			state = "-"
		}
		exit := r.ExitClass
		if exit == "" {
			exit = "-"
		}
		rel := r.Relative
		if rel == "" {
			rel = "-"
		}
		cells = append(cells, state, exit, r.Duration, rel, r.Verdict)
		rows = append(rows, cells)
	}
	return rows
}

// ---------------------------------------------------------------------------
// Server identity — argv is part of it
// ---------------------------------------------------------------------------

// ServerIdentity identifies one ComfyUI process. argv is INSIDE the identity because a
// memory-knob experiment changes argv between arms; two runs under different launch flags are
// not the same server, and pretending they are is what makes a duration delta unattributable.
type ServerIdentity struct {
	ComfyUIVersion  string   `json:"comfyui_version,omitempty"`
	FrontendVersion string   `json:"frontend_version,omitempty"`
	PythonVersion   string   `json:"python_version,omitempty"`
	TorchVersion    string   `json:"torch_version,omitempty"`
	Argv            []string `json:"argv,omitempty"`
	ArgvKnown       bool     `json:"argv_known"`
}

// ID is a stable content hash of the identity. Empty when nothing at all is known, so callers
// store NULL rather than inventing a shared id for every unidentifiable server.
func (s ServerIdentity) ID() string {
	argvJSON := ""
	if s.ArgvKnown {
		if b, err := json.Marshal(s.Argv); err == nil {
			argvJSON = string(b)
		}
	}
	key := strings.Join([]string{
		"comfyui=" + s.ComfyUIVersion,
		"frontend=" + s.FrontendVersion,
		"python=" + s.PythonVersion,
		"torch=" + s.TorchVersion,
		"argv=" + argvJSON,
	}, "|")
	if s.ComfyUIVersion == "" && s.FrontendVersion == "" && s.PythonVersion == "" &&
		s.TorchVersion == "" && !s.ArgvKnown {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// Describe renders the identity for a delta report.
func (s ServerIdentity) Describe() string {
	parts := []string{}
	if s.ComfyUIVersion != "" {
		parts = append(parts, "ComfyUI "+s.ComfyUIVersion)
	}
	if s.TorchVersion != "" {
		parts = append(parts, "torch "+s.TorchVersion)
	}
	if s.PythonVersion != "" {
		parts = append(parts, "python "+firstToken(s.PythonVersion))
	}
	if len(parts) == 0 {
		return "unidentified server"
	}
	return strings.Join(parts, ", ")
}

func firstToken(s string) string {
	if i := strings.IndexAny(s, " \t("); i > 0 {
		return s[:i]
	}
	return s
}

// ParseSystemStats reads /system_stats.
//
// Deliberately does NOT return devices. /system_stats identifies GPUs by index and name only,
// and on this box torch's cuda:N ordering is the INVERSE of nvidia-smi's — an index is not an
// identity. The `device` table is keyed by UUID for that reason, so device rows must come from
// a source that actually carries one.
func ParseSystemStats(raw []byte) (ServerIdentity, error) {
	var envelope struct {
		System struct {
			ComfyUIVersion          string   `json:"comfyui_version"`
			RequiredFrontendVersion string   `json:"required_frontend_version"`
			FrontendVersion         string   `json:"frontend_version"`
			PythonVersion           string   `json:"python_version"`
			PytorchVersion          string   `json:"pytorch_version"`
			Argv                    []string `json:"argv"`
		} `json:"system"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return ServerIdentity{}, fmt.Errorf("parsing /system_stats: %w", err)
	}
	frontend := envelope.System.FrontendVersion
	if frontend == "" {
		frontend = envelope.System.RequiredFrontendVersion
	}
	return ServerIdentity{
		ComfyUIVersion:  envelope.System.ComfyUIVersion,
		FrontendVersion: frontend,
		PythonVersion:   envelope.System.PythonVersion,
		TorchVersion:    envelope.System.PytorchVersion,
		Argv:            envelope.System.Argv,
		ArgvKnown:       envelope.System.Argv != nil,
	}, nil
}

// ---------------------------------------------------------------------------
// Replay delta attribution
// ---------------------------------------------------------------------------

// Side is one half of a replay comparison.
type Side struct {
	PromptID    string   `json:"prompt_id,omitempty"`
	Name        string   `json:"name,omitempty"`
	ServerID    string   `json:"server_id,omitempty"`
	Argv        []string `json:"argv,omitempty"`
	ArgvKnown   bool     `json:"argv_known"`
	DurationMS  int64    `json:"duration_ms,omitempty"`
	HasDuration bool     `json:"has_duration"`
	State       string   `json:"state,omitempty"`
	SubmittedAt string   `json:"submitted_at,omitempty"`
	CacheHit    bool     `json:"cache_hit"`
}

// Delta is an ATTRIBUTED before/after comparison.
type Delta struct {
	Before        Side     `json:"before"`
	After         Side     `json:"after"`
	DeltaMS       int64    `json:"delta_ms,omitempty"`
	HasDelta      bool     `json:"has_delta"`
	PercentChange float64  `json:"percent_change,omitempty"`
	SameServer    bool     `json:"same_server"`
	Attributable  bool     `json:"attributable"`
	Attribution   string   `json:"attribution"`
	ArgvChanges   []string `json:"argv_changes,omitempty"`
	Caveats       []string `json:"caveats,omitempty"`
}

// AttributeDelta compares two runs of the same graph and says what the difference can honestly
// be attributed to.
//
// THE DEFECT THIS PREVENTS: a "+49% regression" was once reported between two runs whose launch
// flags nobody had recorded. With no stored argv there is no way to tell a code regression from
// a different --lowvram, a different model path, or a stale log line — so this returns
// Attributable=false and says why, instead of printing a confident percentage.
func AttributeDelta(before, after Side) Delta {
	d := Delta{Before: before, After: after}
	d.SameServer = before.ServerID != "" && before.ServerID == after.ServerID

	if before.HasDuration && after.HasDuration {
		d.HasDelta = true
		d.DeltaMS = after.DurationMS - before.DurationMS
		if before.DurationMS > 0 {
			d.PercentChange = float64(d.DeltaMS) / float64(before.DurationMS) * 100
		}
	} else {
		switch {
		case !before.HasDuration && !after.HasDuration:
			d.Caveats = append(d.Caveats, "neither run has execution_start/execution_success timestamps; there is no duration to compare")
		case !before.HasDuration:
			d.Caveats = append(d.Caveats, "the original run has no execution_start/execution_success pair; its duration was never recorded")
		default:
			d.Caveats = append(d.Caveats, "the replay has not produced an execution_success timestamp yet")
		}
	}

	if after.CacheHit {
		d.Caveats = append(d.Caveats, "the replay hit ComfyUI's server-side execution cache; its duration measures cache lookup, not the render")
	}

	switch {
	case !before.ArgvKnown && !after.ArgvKnown:
		d.Attributable = false
		d.Attribution = "neither run has a stored argv — a duration change cannot be attributed to code, config, or launch flags. This is the exact shape of the false \"+49% regression\": two numbers with nothing recorded about what produced them."
	case !before.ArgvKnown:
		d.Attributable = false
		d.Attribution = "the original run has no stored argv — the before/after cannot be attributed. This is the exact shape of the false \"+49% regression\"."
	case !after.ArgvKnown:
		d.Attributable = false
		d.Attribution = "the replay's server did not report its argv — the before/after cannot be attributed."
	default:
		d.ArgvChanges = ArgvChanges(before.Argv, after.Argv)
		d.Attributable = true
		if len(d.ArgvChanges) == 0 {
			if d.SameServer {
				d.Attribution = "same server identity and identical argv — any delta is code, models, data, or machine state, not launch flags"
			} else {
				d.Attribution = "identical argv on a different server build — any delta is the build, models, data, or machine state, not launch flags"
			}
		} else {
			d.Attribution = "argv changed between the runs — the delta is attributable to the launch flags below (plus anything else that changed with them)"
		}
	}
	return d
}

// ArgvChanges is an order-insensitive multiset diff, rendered as -removed / +added lines.
func ArgvChanges(before, after []string) []string {
	counts := map[string]int{}
	for _, a := range before {
		counts[a]++
	}
	for _, a := range after {
		counts[a]--
	}
	keys := make([]string, 0, len(counts))
	for k, v := range counts {
		if v != 0 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		n := counts[k]
		if n > 0 {
			for i := 0; i < n; i++ {
				out = append(out, "- "+k)
			}
			continue
		}
		for i := 0; i < -n; i++ {
			out = append(out, "+ "+k)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

// FormatDuration renders a millisecond duration for a comparison table.
func FormatDuration(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	seconds := float64(ms) / 1000
	if seconds < 60 {
		return fmt.Sprintf("%.1fs", seconds)
	}
	minutes := int64(seconds) / 60
	rem := seconds - float64(minutes*60)
	return fmt.Sprintf("%dm%04.1fs", minutes, rem)
}

// FormatSignedDuration renders a delta with an explicit sign.
func FormatSignedDuration(ms int64) string {
	if ms == 0 {
		return "0ms"
	}
	if ms < 0 {
		return "-" + FormatDuration(-ms)
	}
	return "+" + FormatDuration(ms)
}

// FormatUUIDv4 renders 16 random bytes as a version-4 UUID string. The prompt_id is minted
// CLIENT-SIDE before the POST so a lost reply is recoverable by lookup instead of by guessing
// which queued job was ours.
func FormatUUIDv4(b [16]byte) string {
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]))
}

// ArmSpec is the serialised form of an experiment, stored in experiment.spec_json so the
// sweep can be re-derived after a restart wiped everything the server knew.
type ArmSpec struct {
	Mode      string   `json:"mode"`
	Vary      []Var    `json:"vary"`
	GraphSHA  string   `json:"graph_sha,omitempty"`
	GraphPath string   `json:"graph_path,omitempty"`
	Labels    []string `json:"labels,omitempty"`
}

// ArmRecord is the serialised form of one arm, stored in exp_arm.vars_json. The graph sha
// lives here because an arm's materialised graph is what makes it replayable months later.
type ArmRecord struct {
	Index    int               `json:"index"`
	Label    string            `json:"label"`
	Addrs    []string          `json:"addrs"`
	Values   []string          `json:"values"`
	Vars     map[string]string `json:"vars"`
	GraphSHA string            `json:"graph_sha,omitempty"`
}

// ToRecord projects an arm for storage.
func (a Arm) ToRecord(graphSHA string) ArmRecord {
	return ArmRecord{
		Index:    a.Index,
		Label:    a.Label,
		Addrs:    append([]string(nil), a.Addrs...),
		Values:   append([]string(nil), a.Values...),
		Vars:     a.Vars,
		GraphSHA: graphSHA,
	}
}

// ToArm reverses ToRecord.
func (r ArmRecord) ToArm() Arm {
	return Arm{
		Index:  r.Index,
		Label:  r.Label,
		Addrs:  append([]string(nil), r.Addrs...),
		Values: append([]string(nil), r.Values...),
		Vars:   r.Vars,
	}
}
