// Package submit holds the pure decision logic behind `comfyui-pp-cli submit`
// and `comfyui-pp-cli attach`.
//
// NOT generated — hand-written and preserved across regeneration.
//
// WHY THIS PACKAGE EXISTS. ComfyUI dedupes NOTHING: every POST to /prompt mints a new
// prompt_id and starts a new render that can run for 20 minutes. A wrapper that resubmitted
// instead of waiting burned ~30 GPU-minutes on this box. The fix is structural, not
// documentary: the CLI leases a submission on store.GraphSHA and ATTACHES to an in-flight run
// instead of POSTing a second copy.
//
// The second load-bearing job here is telling the four possible answers apart. ComfyUI
// validates each output branch INDEPENDENTLY and only returns HTTP 400 when NO branch
// survives — so a 200 can carry a non-empty node_errors map with some outputs silently
// dropped. Classifying that as success is how a half-rendered batch gets reported as a clean
// run. Classify() separates:
//
//	accepted       2xx, prompt_id present, node_errors empty
//	partial-accept 2xx, prompt_id present, node_errors NON-empty  (branches were dropped)
//	rejected       4xx/5xx — nothing was queued
//	malformed      2xx with no prompt_id (a gateway error envelope is valid JSON too),
//	               or a body that is not a JSON object at all
//
// Node errors are carried VERBATIM (NodeErrorsRaw is the exact byte slice from the response)
// alongside the structured breakdown. Nothing here summarises an error away.
package submit

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"comfyui-pp-cli/internal/store"
)

// ClientID is the client_id sent with every submission. ComfyUI echoes it on the websocket
// feed, so a stable value makes this CLI's traffic identifiable in a shared server's log.
const ClientID = "comfyui-pp-cli"

// Distinct exit codes. The framework already owns 2 (usage), 3 (not found), 4 (auth),
// 5 (api), 6 (partial failure), 7 (rate limit) and 10 (config); these three sit above that
// range so a caller can branch on "the graph was rejected" vs "some branches were dropped"
// vs "the reply was not a ComfyUI reply" without parsing text.
const (
	// ExitRejected is returned when the server rejected the whole graph (validation).
	ExitRejected = 21
	// ExitPartialAccept is returned when the server queued SOME output branches and
	// dropped others. Never treat this as success.
	ExitPartialAccept = 22
	// ExitMalformed is returned when a 2xx reply carried no prompt_id, so there is no
	// handle to attach to and no way to poll.
	ExitMalformed = 23
)

// Outcome is the classification of one /prompt reply.
type Outcome string

const (
	OutcomeAccepted      Outcome = "accepted"
	OutcomePartialAccept Outcome = "partial-accept"
	OutcomeRejected      Outcome = "rejected"
	OutcomeMalformed     Outcome = "malformed"
)

// ExitCode maps an outcome to the process exit code.
func (o Outcome) ExitCode() int {
	switch o {
	case OutcomeAccepted:
		return 0
	case OutcomePartialAccept:
		return ExitPartialAccept
	case OutcomeRejected:
		return ExitRejected
	default:
		return ExitMalformed
	}
}

// ------------------------------------------------------------------ graph input

// ParseGraph decodes a ComfyUI API-format graph and returns both the typed graph (used for
// hashing and linting) and the canonical raw JSON of the graph itself (posted verbatim, so
// node fields this CLI does not model are never silently dropped on the wire).
//
// It accepts two friendly variants and rejects one hostile one:
//   - a bare API graph            {"3": {"class_type": ..., "inputs": {...}}, ...}
//   - a saved POST body           {"prompt": {...}, "client_id": ...}  → unwrapped
//   - a UI workflow               {"nodes": [...], "links": [...]}     → rejected with the fix
//
// The UI-workflow rejection matters because both files are called "workflow.json" and the UI
// export is the one people have; posting it yields an opaque server-side failure.
func ParseGraph(data []byte) (store.APIGraph, json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil, fmt.Errorf("empty input: expected a ComfyUI API-format graph (a JSON object of node-id -> {class_type, inputs})")
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, nil, fmt.Errorf("parsing graph JSON: %w", err)
	}
	if len(envelope) == 0 {
		return nil, nil, fmt.Errorf("empty graph: the JSON object contains no nodes")
	}

	if _, hasNodes := envelope["nodes"]; hasNodes {
		_, hasLinks := envelope["links"]
		_, hasLast := envelope["last_node_id"]
		if hasLinks || hasLast {
			return nil, nil, fmt.Errorf("this is a ComfyUI UI workflow, not an API graph: re-export it with \"Workflow > Export (API)\" — the API format is a flat object of node-id -> {class_type, inputs}")
		}
	}

	// A saved POST body round-trips: unwrap {"prompt": <graph>} so `submit` accepts the
	// exact payload a previous run recorded.
	if raw, ok := envelope["prompt"]; ok && !looksLikeNodeMap(envelope) {
		var inner map[string]json.RawMessage
		if json.Unmarshal(raw, &inner) == nil && looksLikeNodeMap(inner) {
			trimmed = bytes.TrimSpace(raw)
		}
	}

	var g store.APIGraph
	if err := json.Unmarshal(trimmed, &g); err != nil {
		return nil, nil, fmt.Errorf("parsing API graph: %w", err)
	}
	if len(g) == 0 {
		return nil, nil, fmt.Errorf("empty graph: the JSON object contains no nodes")
	}

	var bad []string
	for id, node := range g {
		if strings.TrimSpace(node.ClassType) == "" {
			bad = append(bad, id)
			continue
		}
		// A node with no "inputs" key decodes to a nil map, which marshals as null and
		// would hash differently from an identical graph that wrote {}. Normalise so the
		// submit lease recognises the same graph either way.
		if node.Inputs == nil {
			node.Inputs = map[string]interface{}{}
			g[id] = node
		}
	}
	if len(bad) > 0 {
		sortNodeIDs(bad)
		return nil, nil, fmt.Errorf("not an API graph: %d node(s) have no class_type (%s) — export with \"Workflow > Export (API)\"",
			len(bad), strings.Join(truncateList(bad, 8), ", "))
	}

	return g, json.RawMessage(trimmed), nil
}

// looksLikeNodeMap reports whether a decoded object looks like a map of API nodes: at least
// one value carries a non-empty class_type.
func looksLikeNodeMap(m map[string]json.RawMessage) bool {
	for _, raw := range m {
		var node struct {
			ClassType string `json:"class_type"`
		}
		if json.Unmarshal(raw, &node) == nil && strings.TrimSpace(node.ClassType) != "" {
			return true
		}
	}
	return false
}

// Identity is the pair of hashes plus the cheap fingerprint of a graph.
type Identity struct {
	GraphSHA  string         `json:"graph_sha"`
	ShapeSHA  string         `json:"shape_sha"`
	NodeCount int            `json:"node_count"`
	Classes   []string       `json:"class_types,omitempty"`
	Histogram map[string]int `json:"class_histogram,omitempty"`
}

// Identify computes both hashes. GraphSHA is what the submit lease dedupes on (exact
// identity); ShapeSHA groups runs that differ only by seed so timings stay comparable.
func Identify(g store.APIGraph) (Identity, error) {
	gsha, err := store.GraphSHA(g)
	if err != nil {
		return Identity{}, err
	}
	ssha, err := store.ShapeSHA(g)
	if err != nil {
		return Identity{}, err
	}
	return Identity{
		GraphSHA:  gsha,
		ShapeSHA:  ssha,
		NodeCount: len(g),
		Classes:   store.SortedClassTypes(g),
		Histogram: store.ClassHistogram(g),
	}, nil
}

// ------------------------------------------------------------------ request

// Request is the /prompt body. Prompt is raw so the graph reaches the server byte-identical
// to the file on disk.
type Request struct {
	Prompt   json.RawMessage `json:"prompt"`
	PromptID string          `json:"prompt_id"`
	ClientID string          `json:"client_id"`
}

// BuildRequest wraps an already-parsed raw graph with the client-minted prompt_id.
//
// Minting the id client-side (rather than accepting whatever the server returns) is what
// makes a lost reply recoverable: the run row exists under a known id before the POST, so a
// dropped connection is answered by `attach <prompt_id>`, never by a second render.
func BuildRequest(graphJSON json.RawMessage, promptID string) Request {
	return Request{Prompt: graphJSON, PromptID: promptID, ClientID: ClientID}
}

// BuildRequestFromGraph is BuildRequest for callers holding a typed graph.
func BuildRequestFromGraph(g store.APIGraph, promptID string) (Request, error) {
	raw, err := json.Marshal(g)
	if err != nil {
		return Request{}, fmt.Errorf("marshaling graph: %w", err)
	}
	return BuildRequest(raw, promptID), nil
}

// NewPromptID mints a RFC 4122 version-4 UUID from crypto/rand.
func NewPromptID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("minting prompt_id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

// ------------------------------------------------------------------ lease

// LeaseDecision is the answer to "may I POST this graph?".
type LeaseDecision struct {
	// Attach is true when an identical graph is already in flight: print the handle and
	// stop. This is the whole point of the command.
	Attach bool `json:"attach"`
	// PromptID is the in-flight run to attach to (only when Attach is true).
	PromptID string `json:"prompt_id,omitempty"`
	// Reason explains the decision in one line, for the user and the JSON envelope.
	Reason string `json:"reason"`
}

// DecideLease turns the store lookup into a decision. force is the deliberate escape hatch
// for "yes, I really do want a second render of the same graph".
func DecideLease(activePromptID string, found, force bool) LeaseDecision {
	activePromptID = strings.TrimSpace(activePromptID)
	if !found || activePromptID == "" {
		return LeaseDecision{Reason: "no in-flight run for this graph_sha"}
	}
	if force {
		return LeaseDecision{Reason: "forced: submitting a second render despite in-flight run " + activePromptID}
	}
	return LeaseDecision{
		Attach:   true,
		PromptID: activePromptID,
		Reason:   "an identical graph is already in flight as " + activePromptID,
	}
}

// ------------------------------------------------------------------ lint

// Finding is one pre-submit defect found in the graph itself.
type Finding struct {
	NodeID    string `json:"node_id"`
	ClassType string `json:"class_type"`
	Input     string `json:"input"`
	Value     string `json:"value"`
	Rule      string `json:"rule"`
	Message   string `json:"message"`
}

// loadImageInputs maps the image-loading classes to the inputs that must hold a bare
// filename relative to ComfyUI's input directory.
var loadImageInputs = map[string][]string{
	"LoadImage":       {"image"},
	"LoadImageMask":   {"image"},
	"LoadImageOutput": {"image"},
	"LoadAudio":       {"audio"},
	"LoadVideo":       {"video"},
	"VHS_LoadVideo":   {"video"},
}

// Lint catches graph defects that are certain to fail server-side, before a submission is
// spent on them.
//
// Today it enforces one rule that cost real time on this box: LoadImage takes a BARE
// FILENAME inside ComfyUI's input directory, never an absolute host path. A path like
// C:\renders\a.png or /home/x/a.png is joined against the input dir and never resolves, and
// the resulting server error names the value without explaining the rule.
//
// Subfolders ("batch7/a.png") are legal and are NOT flagged.
func Lint(g store.APIGraph) []Finding {
	var out []Finding
	for id, node := range g {
		inputs, watched := loadImageInputs[node.ClassType]
		if !watched {
			continue
		}
		for _, name := range inputs {
			value, ok := node.Inputs[name].(string)
			if !ok || !isAbsoluteHostPath(value) {
				continue
			}
			out = append(out, Finding{
				NodeID:    id,
				ClassType: node.ClassType,
				Input:     name,
				Value:     value,
				Rule:      "absolute-host-path",
				Message: fmt.Sprintf("%s.%s must be a bare filename inside ComfyUI's input directory (e.g. %q), not an absolute host path",
					node.ClassType, name, baseName(value)),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeID != out[j].NodeID {
			return lessNodeID(out[i].NodeID, out[j].NodeID)
		}
		return out[i].Input < out[j].Input
	})
	return out
}

func isAbsoluteHostPath(s string) bool {
	if len(s) >= 2 && s[1] == ':' && isASCIILetter(s[0]) {
		return true // C:\... or C:/...
	}
	if strings.HasPrefix(s, `\\`) {
		return true // UNC
	}
	if strings.HasPrefix(s, "/") {
		return true // POSIX absolute
	}
	return false
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func baseName(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ------------------------------------------------------------------ response classification

// ErrorDetail is one validation error on one node, with the COMBO options resolved through
// store.ParseComboOptions so the "why can't ComfyUI see my model" question is answered
// honestly rather than guessed.
type ErrorDetail struct {
	Type          string   `json:"type,omitempty"`
	Message       string   `json:"message,omitempty"`
	Details       string   `json:"details,omitempty"`
	InputName     string   `json:"input_name,omitempty"`
	ReceivedValue string   `json:"received_value,omitempty"`
	Options       []string `json:"valid_options,omitempty"`
	OptionCount   int      `json:"valid_option_count"`
	ComboShape    string   `json:"combo_shape,omitempty"`
	Visibility    string   `json:"model_visibility,omitempty"`
	Diagnosis     string   `json:"diagnosis,omitempty"`
}

// NodeError is every error ComfyUI reported for one node, plus the output branches that were
// dropped because they depend on it.
type NodeError struct {
	NodeID           string        `json:"node_id"`
	ClassType        string        `json:"class_type,omitempty"`
	DependentOutputs []string      `json:"dependent_outputs,omitempty"`
	Errors           []ErrorDetail `json:"errors,omitempty"`
}

// Result is the classified reply to one POST /prompt.
type Result struct {
	Outcome     Outcome `json:"outcome"`
	HTTPStatus  int     `json:"http_status"`
	PromptID    string  `json:"prompt_id,omitempty"`
	QueueNumber *int    `json:"queue_number,omitempty"`

	// NodeErrorsRaw is the node_errors value exactly as it arrived — never reformatted,
	// never summarised. The structured breakdown below is additive.
	NodeErrorsRaw json.RawMessage `json:"node_errors,omitempty"`
	NodeErrors    []NodeError     `json:"node_errors_detail,omitempty"`

	// DroppedOutputs names the output branches ComfyUI refused to queue.
	DroppedOutputs []string `json:"dropped_output_branches,omitempty"`

	ErrorType    string `json:"error_type,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	ErrorDetails string `json:"error_details,omitempty"`

	// Reason explains a malformed classification, or notes a body that could not be
	// fully parsed.
	Reason string `json:"reason,omitempty"`
	// RawBody is populated only when the body was not a JSON object, so the operator
	// still sees what the endpoint actually returned (an HTML proxy page, usually).
	RawBody string `json:"raw_body,omitempty"`
}

// Accepted reports whether the whole graph was queued.
func (r Result) Accepted() bool { return r.Outcome == OutcomeAccepted }

// Classify turns (status, body) into an outcome. It never returns an error: an unparseable
// body is itself a classification.
//
// Ordering is deliberate. A 2xx with no prompt_id is MALFORMED even when node_errors is
// present, because there is no handle: nothing can be attached to or polled, so calling it
// "partial" would invent a run that does not exist.
func Classify(status int, body []byte) Result {
	r := Result{HTTPStatus: status}
	trimmed := bytes.TrimSpace(body)

	var top map[string]json.RawMessage
	if len(trimmed) == 0 || json.Unmarshal(trimmed, &top) != nil {
		r.RawBody = string(trimmed)
		if status >= 400 {
			r.Outcome = OutcomeRejected
			r.Reason = fmt.Sprintf("HTTP %d with a body that is not a JSON object", status)
			return r
		}
		r.Outcome = OutcomeMalformed
		r.Reason = fmt.Sprintf("HTTP %d with a body that is not a JSON object — this is not a ComfyUI /prompt reply", status)
		return r
	}

	if raw, ok := top["prompt_id"]; ok {
		var id string
		if json.Unmarshal(raw, &id) == nil {
			r.PromptID = strings.TrimSpace(id)
		}
	}
	if raw, ok := top["number"]; ok {
		var n json.Number
		if json.Unmarshal(raw, &n) == nil {
			if i, err := n.Int64(); err == nil {
				v := int(i)
				r.QueueNumber = &v
			}
		}
	}
	if raw, ok := top["error"]; ok {
		var e struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Details string `json:"details"`
		}
		if json.Unmarshal(raw, &e) == nil {
			r.ErrorType, r.ErrorMessage, r.ErrorDetails = e.Type, e.Message, e.Details
		}
	}

	nodeErrorsPresent := false
	if raw, ok := top["node_errors"]; ok {
		r.NodeErrorsRaw = raw
		nodeErrorsPresent = rawIsNonEmpty(raw)
		parsed, parseOK := parseNodeErrors(raw)
		r.NodeErrors = parsed
		if nodeErrorsPresent && !parseOK {
			// Never swallow a node_errors value this parser did not recognise: the
			// verbatim bytes still reach the user and the outcome still degrades.
			r.Reason = "node_errors was non-empty but did not match ComfyUI's known shape; see the verbatim value"
		}
		r.DroppedOutputs = droppedOutputs(parsed)
	}

	switch {
	case status >= 400:
		r.Outcome = OutcomeRejected
	case status < 200 || status >= 300:
		r.Outcome = OutcomeMalformed
		r.Reason = fmt.Sprintf("unexpected HTTP status %d for POST /prompt", status)
	case r.PromptID == "":
		r.Outcome = OutcomeMalformed
		r.Reason = fmt.Sprintf("HTTP %d reply carries no prompt_id — a gateway or proxy error envelope is valid JSON too, so this was NOT treated as an accepted submission", status)
	case nodeErrorsPresent:
		r.Outcome = OutcomePartialAccept
	default:
		r.Outcome = OutcomeAccepted
	}
	return r
}

// rawIsNonEmpty reports whether a JSON value carries content: null, {}, [] and "" do not.
func rawIsNonEmpty(raw json.RawMessage) bool {
	t := string(bytes.TrimSpace(raw))
	switch t {
	case "", "null", "{}", "[]", `""`:
		return false
	}
	return true
}

// parseNodeErrors decodes ComfyUI's node_errors map. The second return is false when the
// value was non-empty but not decodable in the documented shape.
func parseNodeErrors(raw json.RawMessage) ([]NodeError, bool) {
	if !rawIsNonEmpty(raw) {
		return nil, true
	}
	var entries map[string]struct {
		ClassType        string            `json:"class_type"`
		DependentOutputs []json.RawMessage `json:"dependent_outputs"`
		Errors           []struct {
			Type      string                 `json:"type"`
			Message   string                 `json:"message"`
			Details   string                 `json:"details"`
			ExtraInfo map[string]interface{} `json:"extra_info"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, false
	}

	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sortNodeIDs(ids)

	out := make([]NodeError, 0, len(ids))
	for _, id := range ids {
		entry := entries[id]
		ne := NodeError{NodeID: id, ClassType: entry.ClassType}
		for _, dep := range entry.DependentOutputs {
			ne.DependentOutputs = append(ne.DependentOutputs, scalarString(dep))
		}
		for _, e := range entry.Errors {
			detail := ErrorDetail{Type: e.Type, Message: e.Message, Details: e.Details}
			if e.ExtraInfo != nil {
				if name, ok := e.ExtraInfo["input_name"].(string); ok {
					detail.InputName = name
				}
				if received, ok := e.ExtraInfo["received_value"]; ok {
					detail.ReceivedValue = formatValue(received)
				}
				if spec, ok := e.ExtraInfo["input_config"]; ok {
					annotateCombo(&detail, spec)
				}
			}
			ne.Errors = append(ne.Errors, detail)
		}
		out = append(out, ne)
	}
	return out, true
}

// annotateCombo resolves a COMBO input spec through store.ParseComboOptions — the one reader
// that handles BOTH the v3 (options at index 1) and legacy (options at index 0) shapes,
// which ComfyUI ships simultaneously. An EMPTY option list means the model CLASS is
// unregistered (a missing extra_model_paths.yaml key), NOT a missing file; saying so here is
// the difference between a two-minute fix and an afternoon hunting a download you already
// have.
func annotateCombo(detail *ErrorDetail, spec interface{}) {
	visibility, options := store.ClassifyModelVisibility(spec, detail.ReceivedValue)
	_, shape := store.ParseComboOptions(spec)
	if shape == store.ComboNone {
		return
	}
	detail.ComboShape = string(shape)
	detail.Visibility = string(visibility)
	detail.Options = options
	detail.OptionCount = len(options)
	switch visibility {
	case store.ModelClassUnregistered:
		detail.Diagnosis = "the loader offers ZERO options for this input: the model CLASS is unregistered (a missing key in extra_model_paths.yaml), NOT a missing file. Add the folder key, restart ComfyUI, then re-check with `objectinfo`."
	case store.ModelNotListed:
		detail.Diagnosis = fmt.Sprintf("the loader offers %d option(s) but not this filename: check the spelling/subfolder, or that the file sits under the registered folder for this class.", len(options))
	case store.ModelVisible:
		detail.Diagnosis = "the loader does offer this filename — the rejection came from a different field on this input."
	}
}

// droppedOutputs is the union of dependent_outputs across every failing node: the output
// branches ComfyUI refused to queue. When ComfyUI reported no dependents, the failing nodes
// themselves are named so the message is never empty.
func droppedOutputs(nodeErrors []NodeError) []string {
	seen := map[string]bool{}
	var out []string
	for _, ne := range nodeErrors {
		for _, dep := range ne.DependentOutputs {
			if dep == "" || seen[dep] {
				continue
			}
			seen[dep] = true
			out = append(out, dep)
		}
	}
	if len(out) == 0 {
		for _, ne := range nodeErrors {
			if ne.NodeID == "" || seen[ne.NodeID] {
				continue
			}
			seen[ne.NodeID] = true
			out = append(out, ne.NodeID)
		}
	}
	sortNodeIDs(out)
	return out
}

// FormatReport renders the structured breakdown that accompanies the verbatim node_errors.
// Returns "" when there is nothing to report.
func FormatReport(r Result) string {
	var b strings.Builder
	if r.ErrorType != "" || r.ErrorMessage != "" {
		fmt.Fprintf(&b, "%s: %s\n", orDash(r.ErrorType), orDash(r.ErrorMessage))
		if strings.TrimSpace(r.ErrorDetails) != "" {
			fmt.Fprintf(&b, "  details: %s\n", r.ErrorDetails)
		}
	}
	for _, ne := range r.NodeErrors {
		fmt.Fprintf(&b, "node %s (%s)\n", ne.NodeID, orDash(ne.ClassType))
		for _, e := range ne.Errors {
			fmt.Fprintf(&b, "  - %s: %s\n", orDash(e.Type), orDash(e.Message))
			if strings.TrimSpace(e.Details) != "" {
				fmt.Fprintf(&b, "    details: %s\n", e.Details)
			}
			if e.InputName != "" {
				fmt.Fprintf(&b, "    input: %s", e.InputName)
				if e.ReceivedValue != "" {
					fmt.Fprintf(&b, "  received: %s", e.ReceivedValue)
				}
				b.WriteString("\n")
			}
			if e.ComboShape != "" {
				if e.OptionCount == 0 {
					fmt.Fprintf(&b, "    valid options: NONE (%s spec)\n", e.ComboShape)
				} else {
					fmt.Fprintf(&b, "    valid options (%d, %s spec): %s\n", e.OptionCount, e.ComboShape,
						strings.Join(truncateList(e.Options, 12), ", "))
				}
			}
			if e.Diagnosis != "" {
				fmt.Fprintf(&b, "    diagnosis: %s\n", e.Diagnosis)
			}
		}
		if len(ne.DependentOutputs) > 0 {
			fmt.Fprintf(&b, "  dropped output branches: %s\n", strings.Join(ne.DependentOutputs, ", "))
		}
	}
	return b.String()
}

// ------------------------------------------------------------------ history / queue reads

// HistoryStatus is the terminal-state view of one /history entry.
//
// TIMING RULE: StartMS and SuccessMS come ONLY from the execution_start / execution_success
// messages. The server log's "Prompt executed in N seconds" line is stale mid-run (it once
// produced a false "+49% regression") and an s/it progress sample is a transient, not a
// rate. Neither is ever read here.
type HistoryStatus struct {
	Found           bool   `json:"found"`
	StatusStr       string `json:"status_str,omitempty"`
	Completed       bool   `json:"completed"`
	Terminal        bool   `json:"terminal"`
	Started         bool   `json:"started"`
	StartMS         int64  `json:"execution_start_ms,omitempty"`
	SuccessMS       int64  `json:"execution_success_ms,omitempty"`
	TimingInverted  bool   `json:"timing_inverted,omitempty"`
	OutputNodeCount int    `json:"output_node_count"`
	ErrorNodeID     string `json:"error_node_id,omitempty"`
	ErrorNodeType   string `json:"error_node_type,omitempty"`
	ExceptionType   string `json:"exception_type,omitempty"`
	ExceptionMsg    string `json:"exception_message,omitempty"`
}

// DurationMS is the only duration this CLI will report: success minus start, or 0 when
// either timestamp is missing.
func (h HistoryStatus) DurationMS() int64 {
	if h.StartMS <= 0 || h.SuccessMS <= 0 || h.SuccessMS < h.StartMS {
		return 0
	}
	return h.SuccessMS - h.StartMS
}

// Failed reports whether the run ended in an execution error.
func (h HistoryStatus) Failed() bool {
	return h.ExceptionMsg != "" || h.ExceptionType != "" || strings.EqualFold(h.StatusStr, "error")
}

// ParseHistory reads GET /history or GET /history/{prompt_id} and extracts the state of one
// prompt. A missing entry is not an error: ComfyUI only materialises the entry once
// execution starts, so "not found yet" is the normal answer while a job is queued.
func ParseHistory(body []byte, promptID string) (HistoryStatus, error) {
	var st HistoryStatus
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return st, nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &entries); err != nil {
		return st, fmt.Errorf("parsing /history response: %w", err)
	}
	raw, ok := entries[promptID]
	if !ok {
		return st, nil
	}

	var entry struct {
		Outputs map[string]json.RawMessage `json:"outputs"`
		Status  struct {
			StatusStr string            `json:"status_str"`
			Completed bool              `json:"completed"`
			Messages  []json.RawMessage `json:"messages"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return st, fmt.Errorf("parsing /history entry for %s: %w", promptID, err)
	}

	st.Found = true
	st.StatusStr = entry.Status.StatusStr
	st.Completed = entry.Status.Completed
	st.OutputNodeCount = len(entry.Outputs)

	for _, msgRaw := range entry.Status.Messages {
		name, payload, ok := decodeHistoryMessage(msgRaw)
		if !ok {
			continue
		}
		switch name {
		case "execution_start":
			st.Started = true
			if ts, ok := payload["timestamp"].(float64); ok {
				st.StartMS = store.NormaliseEpochMS(ts)
			}
		case "execution_success":
			if ts, ok := payload["timestamp"].(float64); ok {
				st.SuccessMS = store.NormaliseEpochMS(ts)
			}
		case "execution_error":
			st.ErrorNodeID = formatValue(payload["node_id"])
			st.ErrorNodeType, _ = payload["node_type"].(string)
			st.ExceptionType, _ = payload["exception_type"].(string)
			st.ExceptionMsg, _ = payload["exception_message"].(string)
		case "execution_interrupted":
			if st.StatusStr == "" {
				st.StatusStr = "interrupted"
			}
		}
	}

	if st.StartMS > 0 && st.SuccessMS > 0 && st.SuccessMS < st.StartMS {
		// Surfaced rather than silently stored: a negative duration is the exact defect
		// class that produced a false regression report.
		st.TimingInverted = true
	}
	st.Terminal = st.Completed ||
		strings.EqualFold(st.StatusStr, "success") ||
		strings.EqualFold(st.StatusStr, "error") ||
		strings.EqualFold(st.StatusStr, "interrupted") ||
		st.Failed()
	return st, nil
}

func decodeHistoryMessage(raw json.RawMessage) (string, map[string]interface{}, bool) {
	var tuple []interface{}
	if err := json.Unmarshal(raw, &tuple); err != nil || len(tuple) < 2 {
		return "", nil, false
	}
	name, ok := tuple[0].(string)
	if !ok {
		return "", nil, false
	}
	payload, ok := tuple[1].(map[string]interface{})
	if !ok {
		return name, map[string]interface{}{}, true
	}
	return name, payload, true
}

// QueueState is where a prompt sits in ComfyUI's queue.
type QueueState struct {
	Found    bool   `json:"found"`
	Running  bool   `json:"running"`
	Pending  bool   `json:"pending"`
	Position int    `json:"position,omitempty"` // 1-based position among pending items
	Ahead    int    `json:"ahead,omitempty"`    // pending items queued before this one
	State    string `json:"state,omitempty"`    // running | pending | ""
}

// ParseQueue reads GET /queue and locates one prompt. Each queue entry is the tuple
// [number, prompt_id, prompt, extra_data, outputs]; only index 1 is needed here.
func ParseQueue(body []byte, promptID string) (QueueState, error) {
	var qs QueueState
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return qs, nil
	}
	var queue struct {
		Running []json.RawMessage `json:"queue_running"`
		Pending []json.RawMessage `json:"queue_pending"`
	}
	if err := json.Unmarshal(trimmed, &queue); err != nil {
		return qs, fmt.Errorf("parsing /queue response: %w", err)
	}
	for _, raw := range queue.Running {
		if queueEntryPromptID(raw) == promptID {
			qs.Found, qs.Running, qs.State = true, true, "running"
			return qs, nil
		}
	}
	for i, raw := range queue.Pending {
		if queueEntryPromptID(raw) == promptID {
			qs.Found, qs.Pending, qs.State = true, true, "pending"
			qs.Position = i + 1
			qs.Ahead = i
			return qs, nil
		}
	}
	return qs, nil
}

func queueEntryPromptID(raw json.RawMessage) string {
	var tuple []json.RawMessage
	if err := json.Unmarshal(raw, &tuple); err != nil || len(tuple) < 2 {
		return ""
	}
	var id string
	if json.Unmarshal(tuple[1], &id) != nil {
		return ""
	}
	return id
}

// ------------------------------------------------------------------ reference resolution

// RefKind is how `attach <ref>` interpreted its argument.
type RefKind string

const (
	RefFile     RefKind = "file"
	RefPromptID RefKind = "prompt_id"
	RefGraphSHA RefKind = "graph_sha"
)

// ClassifyRef resolves the one positional argument of `attach`: a graph file, a graph_sha
// (full or a >=8-char prefix), or a prompt_id. exists is injected so the decision stays pure
// and testable; pass a filesystem probe at the call site.
//
// An existing file always wins, so a run named like a hash is still attachable by path.
func ClassifyRef(arg string, exists func(string) bool) (RefKind, error) {
	trimmed := strings.TrimSpace(arg)
	if trimmed == "" {
		return "", fmt.Errorf("empty reference: pass a graph file, a graph_sha, or a prompt_id")
	}
	if exists != nil && exists(trimmed) {
		return RefFile, nil
	}
	if isUUIDShape(trimmed) {
		return RefPromptID, nil
	}
	if isHex(trimmed) && len(trimmed) >= 8 {
		return RefGraphSHA, nil
	}
	if strings.HasSuffix(strings.ToLower(trimmed), ".json") ||
		strings.ContainsAny(trimmed, `/\`) ||
		strings.HasPrefix(trimmed, ".") {
		return RefFile, nil
	}
	return "", fmt.Errorf("cannot interpret %q: expected a path to an API graph (.json), a graph_sha (>=8 hex chars), or a prompt_id (UUID)", trimmed)
}

func isUUIDShape(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !isHexRune(r) {
				return false
			}
		}
	}
	return true
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isHexRune(r) {
			return false
		}
	}
	return true
}

func isHexRune(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

// ------------------------------------------------------------------ small helpers

// ShortSHA renders the first 12 characters of a hash for human output.
func ShortSHA(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

func sortNodeIDs(ids []string) {
	sort.Slice(ids, func(i, j int) bool { return lessNodeID(ids[i], ids[j]) })
}

// lessNodeID orders numeric node ids numerically ("2" before "10") and falls back to
// lexical order for non-numeric ids.
func lessNodeID(a, b string) bool {
	ai, aerr := strconv.Atoi(a)
	bi, berr := strconv.Atoi(b)
	switch {
	case aerr == nil && berr == nil:
		return ai < bi
	case aerr == nil:
		return true
	case berr == nil:
		return false
	default:
		return a < b
	}
}

func scalarString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

func formatValue(v interface{}) string {
	switch tv := v.(type) {
	case nil:
		return ""
	case string:
		return tv
	case float64:
		return strconv.FormatFloat(tv, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(tv)
	default:
		if b, err := json.Marshal(tv); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", tv)
	}
}

func truncateList(in []string, max int) []string {
	if len(in) <= max {
		return in
	}
	out := make([]string, 0, max+1)
	out = append(out, in[:max]...)
	return append(out, fmt.Sprintf("... (+%d more)", len(in)-max))
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
