// Package history parses ComfyUI /history payloads into typed, honest records.
//
// NOT generated — hand-written and preserved across regeneration.
//
// WHY THIS IS A PACKAGE OF ITS OWN. Every timing number this CLI reports has to come
// from one place: the `status.messages` array of a /history entry, which carries
// `execution_start` / `execution_success` / `execution_error` events with epoch
// timestamps. The two tempting alternatives are both wrong and both have produced
// real, published, wrong numbers on this box:
//
//   - The server log line "Prompt executed in N seconds" is the PREVIOUS prompt's line
//     while the current one is still running. Reading it mid-run reports a stale value.
//   - An "s/it" sample from a progress bar is a transient instantaneous rate, not a
//     duration. Treating one as a rate once produced a false "+49% regression" on a
//     build that had actually got faster.
//
// So the parsing lives here as pure functions over bytes, with table-driven tests over
// synthetic payloads, and nothing else in the CLI is allowed to invent a duration.
//
// THE OTHER TWO TRAPS ENCODED HERE:
//
//   - Timestamps arrive as epoch SECONDS or epoch MILLISECONDS depending on the event
//     source and the ComfyUI build. Normalisation goes through store.NormaliseEpochMS so
//     the magnitude rule lives in exactly one place.
//   - A just-finished prompt can appear in /history with `status_str: success` and an
//     EMPTY `outputs` map, because ComfyUI records outputs a beat after it records the
//     success message. That is StateCompletedOutputsPending — a distinct, NON-terminal
//     state. Reporting it as "done, produced nothing" is a lie that loses a render.
package history

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"comfyui-pp-cli/internal/store"
)

// Message event names emitted in status.messages.
const (
	EventExecutionStart       = "execution_start"
	EventExecutionSuccess     = "execution_success"
	EventExecutionError       = "execution_error"
	EventExecutionInterrupted = "execution_interrupted"
	EventExecutionCached      = "execution_cached"
)

// State is the lifecycle position of one prompt as /history reports it.
type State string

const (
	// StatePending — the entry exists but no execution_start has been recorded.
	StatePending State = "pending"
	// StateRunning — execution_start seen, no terminal event yet.
	StateRunning State = "running"
	// StateCompletedOutputsPending — success recorded, outputs map still empty. This is
	// the /history lag race: NON-terminal, poll again. Never report it as success.
	StateCompletedOutputsPending State = "completed-outputs-pending"
	// StateCompleted — success recorded and the outputs map is populated.
	StateCompleted State = "completed"
	// StateFailed — an execution_error was recorded.
	StateFailed State = "failed"
	// StateInterrupted — the prompt was interrupted before finishing.
	StateInterrupted State = "interrupted"
)

// Terminal reports whether a state is final. StateCompletedOutputsPending is deliberately
// NOT terminal: the run finished but /history has not yet published what it produced, and
// a poller that stops here loses the outputs.
func (s State) Terminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateInterrupted:
		return true
	}
	return false
}

func (s State) String() string { return string(s) }

// Message is one element of status.messages: ["execution_start", {...}].
type Message struct {
	Event string                 `json:"event"`
	Data  map[string]interface{} `json:"data,omitempty"`
}

// Timestamps are the authoritative per-prompt clock readings, normalised to epoch
// milliseconds. A zero value means the event was not present.
type Timestamps struct {
	StartMS       int64 `json:"start_ms,omitempty"`
	SuccessMS     int64 `json:"success_ms,omitempty"`
	ErrorMS       int64 `json:"error_ms,omitempty"`
	InterruptedMS int64 `json:"interrupted_ms,omitempty"`
}

// TerminalMS returns the timestamp of the terminal event, or 0 when none was recorded.
func (t Timestamps) TerminalMS() int64 {
	switch {
	case t.ErrorMS > 0:
		return t.ErrorMS
	case t.InterruptedMS > 0:
		return t.InterruptedMS
	default:
		return t.SuccessMS
	}
}

// Output is one file a prompt produced, as /history reports it.
type Output struct {
	NodeID    string `json:"node_id"`
	Key       string `json:"key"`
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder,omitempty"`
	Type      string `json:"type,omitempty"`
}

// ExecError is an execution_error payload. Raw carries the event data verbatim because
// the exception text IS the diagnostic — summarising it away destroys the only evidence
// an OOM or a validation failure leaves behind once the server restarts.
type ExecError struct {
	NodeID           string                 `json:"node_id,omitempty"`
	NodeType         string                 `json:"node_type,omitempty"`
	ExceptionType    string                 `json:"exception_type,omitempty"`
	ExceptionMessage string                 `json:"exception_message,omitempty"`
	Traceback        []string               `json:"traceback,omitempty"`
	Raw              map[string]interface{} `json:"raw,omitempty"`
}

// PromptTuple is ComfyUI's positional prompt record:
// [number, prompt_id, graph, extra_data, outputs_to_execute].
// The same shape appears as the `prompt` field of a /history entry AND as each element of
// /queue's queue_running / queue_pending arrays, so both go through one parser.
type PromptTuple struct {
	Number   int64           `json:"number"`
	PromptID string          `json:"prompt_id"`
	Graph    json.RawMessage `json:"-"`
}

// Entry is one fully-parsed /history record.
type Entry struct {
	PromptID    string     `json:"prompt_id"`
	QueueNumber int64      `json:"queue_number,omitempty"`
	StatusStr   string     `json:"status_str,omitempty"`
	Completed   bool       `json:"completed"`
	State       State      `json:"state"`
	Timestamps  Timestamps `json:"timestamps"`

	// DurationMS is the ONLY honest duration: execution_success - execution_start.
	// It is set only when DurationOK is true.
	DurationMS int64 `json:"duration_ms,omitempty"`
	DurationOK bool  `json:"duration_ok"`
	// ElapsedMS is start -> terminal event for runs that ended in error or
	// interruption. It is NOT a success duration and never feeds shape statistics.
	ElapsedMS int64 `json:"elapsed_ms,omitempty"`
	// TimingAnomaly is non-empty when the timestamps are unusable (e.g. success
	// precedes start). A missing duration beats a silently wrong one.
	TimingAnomaly string `json:"timing_anomaly,omitempty"`

	Outputs []Output `json:"outputs"`
	// OutputNodes are the node ids present in the outputs map, including nodes whose
	// payload carries no file (text outputs). An empty list next to a recorded success
	// is the lag race, which is why the count is kept separately from Outputs.
	OutputNodes []string `json:"output_nodes,omitempty"`

	Error    *ExecError `json:"error,omitempty"`
	Messages []Message  `json:"messages,omitempty"`

	// PromptGraph is the API-format graph the prompt ran, verbatim, when the entry
	// carried one. It is what lets an ingested history row be hashed into a shape.
	PromptGraph json.RawMessage `json:"-"`
	// Raw is the untouched entry body.
	Raw json.RawMessage `json:"-"`
}

// LatestMS is the most recent clock reading on the entry — used for ordering.
func (e Entry) LatestMS() int64 {
	latest := e.Timestamps.StartMS
	for _, v := range []int64{e.Timestamps.SuccessMS, e.Timestamps.ErrorMS, e.Timestamps.InterruptedMS} {
		if v > latest {
			latest = v
		}
	}
	return latest
}

// SkippedEntry records a /history key that could not be parsed. Malformed entries are
// reported rather than dropped silently: a hole in an ingest must be visible.
type SkippedEntry struct {
	PromptID string `json:"prompt_id"`
	Reason   string `json:"reason"`
}

// ParsePromptTuple decodes a positional prompt record. Returns false when the value is
// not a tuple carrying a prompt id.
func ParsePromptTuple(raw json.RawMessage) (PromptTuple, bool) {
	if len(raw) == 0 {
		return PromptTuple{}, false
	}
	var tuple []json.RawMessage
	if err := json.Unmarshal(raw, &tuple); err != nil || len(tuple) < 2 {
		return PromptTuple{}, false
	}
	var out PromptTuple
	var number float64
	if json.Unmarshal(tuple[0], &number) == nil {
		out.Number = int64(number)
	}
	if err := json.Unmarshal(tuple[1], &out.PromptID); err != nil || strings.TrimSpace(out.PromptID) == "" {
		return PromptTuple{}, false
	}
	if len(tuple) > 2 && isJSONObject(tuple[2]) {
		out.Graph = append(json.RawMessage(nil), tuple[2]...)
	}
	return out, true
}

// ParseMessages decodes status.messages. Elements that are not ["event", {...}] pairs are
// skipped; ComfyUI has shipped extra bookkeeping entries in this array before.
func ParseMessages(raw json.RawMessage) []Message {
	if len(raw) == 0 {
		return nil
	}
	var rawMessages []json.RawMessage
	if err := json.Unmarshal(raw, &rawMessages); err != nil {
		return nil
	}
	out := make([]Message, 0, len(rawMessages))
	for _, rm := range rawMessages {
		var pair []json.RawMessage
		if err := json.Unmarshal(rm, &pair); err != nil || len(pair) == 0 {
			continue
		}
		var event string
		if err := json.Unmarshal(pair[0], &event); err != nil || event == "" {
			continue
		}
		msg := Message{Event: event}
		if len(pair) > 1 {
			var data map[string]interface{}
			if json.Unmarshal(pair[1], &data) == nil {
				msg.Data = data
			}
		}
		out = append(out, msg)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// TimestampsFrom extracts the authoritative clock readings from the message list,
// normalising epoch seconds and epoch milliseconds to milliseconds.
//
// The earliest execution_start and the latest terminal event win, so a prompt that was
// re-queued inside one entry still reports the full wall time it occupied.
func TimestampsFrom(messages []Message) Timestamps {
	var ts Timestamps
	for _, msg := range messages {
		value, ok := messageTimestampMS(msg)
		if !ok {
			continue
		}
		switch msg.Event {
		case EventExecutionStart:
			if ts.StartMS == 0 || value < ts.StartMS {
				ts.StartMS = value
			}
		case EventExecutionSuccess:
			if value > ts.SuccessMS {
				ts.SuccessMS = value
			}
		case EventExecutionError:
			if value > ts.ErrorMS {
				ts.ErrorMS = value
			}
		case EventExecutionInterrupted:
			if value > ts.InterruptedMS {
				ts.InterruptedMS = value
			}
		}
	}
	return ts
}

// messageTimestampMS reads the `timestamp` field of an event payload and normalises it.
func messageTimestampMS(msg Message) (int64, bool) {
	if msg.Data == nil {
		return 0, false
	}
	value, ok := toFloat(msg.Data["timestamp"])
	if !ok || value <= 0 {
		return 0, false
	}
	normalised := store.NormaliseEpochMS(value)
	if normalised <= 0 {
		return 0, false
	}
	return normalised, true
}

// DurationMS returns the success duration and whether it is trustworthy. An inverted pair
// yields (0, false, reason): a missing duration is recoverable, a wrong one is not.
func DurationMS(ts Timestamps) (int64, bool, string) {
	if ts.StartMS <= 0 || ts.SuccessMS <= 0 {
		return 0, false, ""
	}
	if ts.SuccessMS < ts.StartMS {
		return 0, false, fmt.Sprintf("execution_success %d precedes execution_start %d", ts.SuccessMS, ts.StartMS)
	}
	return ts.SuccessMS - ts.StartMS, true, ""
}

// ElapsedMS returns start -> terminal-event wall time for a run that did not succeed.
// It is reported separately from DurationMS so a failed run's wall time can never be
// mistaken for, or averaged into, a success duration.
func ElapsedMS(ts Timestamps) (int64, bool) {
	terminal := ts.TerminalMS()
	if ts.StartMS <= 0 || terminal <= 0 || terminal < ts.StartMS {
		return 0, false
	}
	return terminal - ts.StartMS, true
}

// ParseOutputs decodes the outputs map into file records plus the node ids present.
// Nodes whose payload carries no file (a text output, for instance) still contribute a
// node id, because their presence proves /history has published this run's outputs.
func ParseOutputs(raw json.RawMessage) ([]Output, []string) {
	if len(raw) == 0 {
		return nil, nil
	}
	var nodes map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nodes); err != nil {
		return nil, nil
	}
	nodeIDs := make([]string, 0, len(nodes))
	for id := range nodes {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Slice(nodeIDs, func(i, j int) bool { return lessNodeID(nodeIDs[i], nodeIDs[j]) })

	var outputs []Output
	for _, nodeID := range nodeIDs {
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(nodes[nodeID], &payload); err != nil {
			continue
		}
		keys := make([]string, 0, len(payload))
		for key := range payload {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			var files []map[string]interface{}
			if err := json.Unmarshal(payload[key], &files); err != nil {
				continue
			}
			for _, file := range files {
				filename, _ := file["filename"].(string)
				if strings.TrimSpace(filename) == "" {
					continue
				}
				subfolder, _ := file["subfolder"].(string)
				fileType, _ := file["type"].(string)
				outputs = append(outputs, Output{
					NodeID:    nodeID,
					Key:       key,
					Filename:  filename,
					Subfolder: subfolder,
					Type:      fileType,
				})
			}
		}
	}
	if len(nodeIDs) == 0 {
		nodeIDs = nil
	}
	return outputs, nodeIDs
}

// ExecErrorFrom returns the execution_error payload, or nil when the run did not fail.
func ExecErrorFrom(messages []Message) *ExecError {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Event != EventExecutionError {
			continue
		}
		out := &ExecError{Raw: msg.Data}
		if msg.Data == nil {
			return out
		}
		out.NodeID = stringField(msg.Data, "node_id")
		out.NodeType = stringField(msg.Data, "node_type")
		out.ExceptionType = stringField(msg.Data, "exception_type")
		out.ExceptionMessage = stringField(msg.Data, "exception_message")
		if raw, ok := msg.Data["traceback"]; ok {
			out.Traceback = toStringSlice(raw)
		}
		return out
	}
	return nil
}

// Classify decides the lifecycle state.
//
// Order is load-bearing. An explicit execution_error wins over everything. An
// interruption is checked before status_str, because ComfyUI reports an interrupted
// prompt with status_str "error" and calling that a failure hides the real cause. A
// recorded success with an empty outputs map is the lag race, not a completed run.
func Classify(statusStr string, completed bool, ts Timestamps, outputNodeCount int, hasError, hasInterrupt bool) State {
	if hasError {
		return StateFailed
	}
	if hasInterrupt || ts.InterruptedMS > 0 {
		return StateInterrupted
	}
	if ts.ErrorMS > 0 || strings.EqualFold(statusStr, "error") {
		return StateFailed
	}
	succeeded := ts.SuccessMS > 0 || completed || strings.EqualFold(statusStr, "success")
	if succeeded {
		if outputNodeCount == 0 {
			return StateCompletedOutputsPending
		}
		return StateCompleted
	}
	if ts.StartMS > 0 {
		return StateRunning
	}
	return StatePending
}

// ParseEntry decodes one /history record. promptID is the map key it was found under;
// when empty, the id embedded in the prompt tuple is used.
func ParseEntry(promptID string, raw json.RawMessage) (Entry, error) {
	if len(raw) == 0 {
		return Entry{}, fmt.Errorf("history entry %q: empty body", promptID)
	}
	var envelope struct {
		Prompt  json.RawMessage `json:"prompt"`
		Outputs json.RawMessage `json:"outputs"`
		Status  struct {
			StatusStr string          `json:"status_str"`
			Completed bool            `json:"completed"`
			Messages  json.RawMessage `json:"messages"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Entry{}, fmt.Errorf("history entry %q: %w", promptID, err)
	}

	entry := Entry{
		PromptID:  promptID,
		StatusStr: envelope.Status.StatusStr,
		Completed: envelope.Status.Completed,
		Raw:       append(json.RawMessage(nil), raw...),
	}
	if tuple, ok := ParsePromptTuple(envelope.Prompt); ok {
		entry.QueueNumber = tuple.Number
		entry.PromptGraph = tuple.Graph
		if entry.PromptID == "" {
			entry.PromptID = tuple.PromptID
		}
	}
	if entry.PromptID == "" {
		return Entry{}, fmt.Errorf("history entry has no prompt_id")
	}

	entry.Messages = ParseMessages(envelope.Status.Messages)
	entry.Timestamps = TimestampsFrom(entry.Messages)
	entry.Outputs, entry.OutputNodes = ParseOutputs(envelope.Outputs)
	entry.Error = ExecErrorFrom(entry.Messages)

	hasInterrupt := false
	for _, msg := range entry.Messages {
		if msg.Event == EventExecutionInterrupted {
			hasInterrupt = true
			break
		}
	}
	entry.State = Classify(entry.StatusStr, entry.Completed, entry.Timestamps, len(entry.OutputNodes), entry.Error != nil, hasInterrupt)

	duration, ok, anomaly := DurationMS(entry.Timestamps)
	entry.DurationMS, entry.DurationOK, entry.TimingAnomaly = duration, ok, anomaly
	if !ok {
		if elapsed, elapsedOK := ElapsedMS(entry.Timestamps); elapsedOK {
			entry.ElapsedMS = elapsed
		}
	}
	return entry, nil
}

// ParseAll decodes a full /history payload. Entries that fail to parse are returned in
// skipped rather than dropped, so an ingest can report exactly what it could not read.
// Results are ordered oldest-first, matching /history's documented "newest last".
func ParseAll(raw []byte) ([]Entry, []SkippedEntry, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, nil, fmt.Errorf("parsing /history payload: %w", err)
	}
	entries := make([]Entry, 0, len(payload))
	var skipped []SkippedEntry
	for promptID, rawEntry := range payload {
		entry, err := ParseEntry(promptID, rawEntry)
		if err != nil {
			skipped = append(skipped, SkippedEntry{PromptID: promptID, Reason: err.Error()})
			continue
		}
		entries = append(entries, entry)
	}
	SortByRecency(entries)
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].PromptID < skipped[j].PromptID })
	if len(entries) == 0 {
		entries = nil
	}
	return entries, skipped, nil
}

// ParseOne decodes a /history/{prompt_id} payload, which is a single-key map. Returns
// found=false for the empty `{}` ComfyUI answers for an unknown prompt.
func ParseOne(raw []byte, promptID string) (Entry, bool, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return Entry{}, false, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Entry{}, false, fmt.Errorf("parsing /history/%s payload: %w", promptID, err)
	}
	if len(payload) == 0 {
		return Entry{}, false, nil
	}
	if body, ok := payload[promptID]; ok {
		entry, err := ParseEntry(promptID, body)
		return entry, err == nil, err
	}
	// A bare entry body (no prompt-id key) — defensive, some proxies unwrap it.
	if _, hasStatus := payload["status"]; hasStatus {
		entry, err := ParseEntry(promptID, raw)
		return entry, err == nil, err
	}
	if len(payload) == 1 {
		for key, body := range payload {
			entry, err := ParseEntry(key, body)
			return entry, err == nil, err
		}
	}
	return Entry{}, false, nil
}

// SortByRecency orders entries oldest-first with prompt_id as a deterministic tie-break.
func SortByRecency(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		li, lj := entries[i].LatestMS(), entries[j].LatestMS()
		if li != lj {
			return li < lj
		}
		return entries[i].PromptID < entries[j].PromptID
	})
}

// --- small helpers -------------------------------------------------------------

func isJSONObject(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return strings.HasPrefix(trimmed, "{")
}

func stringField(data map[string]interface{}, key string) string {
	if data == nil {
		return ""
	}
	switch v := data[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return v.String()
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

func toStringSlice(v interface{}) []string {
	switch typed := v.(type) {
	case []interface{}:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s, ok := item.(string); ok {
				out = append(out, s)
				continue
			}
			out = append(out, fmt.Sprintf("%v", item))
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case string:
		return []string{typed}
	default:
		return nil
	}
}

func toFloat(v interface{}) (float64, bool) {
	switch typed := v.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		f, err := typed.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// lessNodeID orders node ids numerically when both are numeric ("9" before "10") and
// lexically otherwise, so output ordering is stable and human-sensible.
func lessNodeID(a, b string) bool {
	ai, aerr := strconv.ParseInt(a, 10, 64)
	bi, berr := strconv.ParseInt(b, 10, 64)
	if aerr == nil && berr == nil {
		if ai != bi {
			return ai < bi
		}
		return a < b
	}
	if aerr == nil {
		return true
	}
	if berr == nil {
		return false
	}
	return a < b
}
