package history

import (
	"encoding/json"
	"strings"
	"testing"
)

// Synthetic /history payloads mirroring what ComfyUI 0.32.0 returns. Each fixture
// encodes one trap the parser has to survive.
const (
	// Epoch MILLISECONDS, the common shape. 42_500 ms of work.
	entryEpochMS = `{
		"prompt": [7, "p-ms", {"3": {"class_type": "KSampler", "inputs": {"seed": 1}}}, {}, ["9"]],
		"outputs": {"9": {"images": [{"filename": "out_00001_.png", "subfolder": "", "type": "output"}]}},
		"status": {
			"status_str": "success",
			"completed": true,
			"messages": [
				["execution_start", {"prompt_id": "p-ms", "timestamp": 1755000000000}],
				["execution_cached", {"nodes": ["4"], "prompt_id": "p-ms", "timestamp": 1755000000100}],
				["execution_success", {"prompt_id": "p-ms", "timestamp": 1755000042500}]
			]
		}
	}`

	// Epoch SECONDS. Same 42.5 s of work, expressed in the other unit.
	entryEpochSeconds = `{
		"prompt": [8, "p-sec", {"3": {"class_type": "KSampler", "inputs": {}}}, {}, ["9"]],
		"outputs": {"9": {"images": [{"filename": "out_00002_.png", "subfolder": "sub", "type": "output"}]}},
		"status": {
			"status_str": "success",
			"completed": true,
			"messages": [
				["execution_start", {"prompt_id": "p-sec", "timestamp": 1755000000}],
				["execution_success", {"prompt_id": "p-sec", "timestamp": 1755000042.5}]
			]
		}
	}`

	// An execution_error entry: the exception text is the whole diagnostic.
	entryError = `{
		"prompt": [9, "p-err", {"3": {"class_type": "KSampler", "inputs": {}}}, {}, ["9"]],
		"outputs": {},
		"status": {
			"status_str": "error",
			"completed": false,
			"messages": [
				["execution_start", {"prompt_id": "p-err", "timestamp": 1755000100000}],
				["execution_error", {
					"prompt_id": "p-err",
					"node_id": "3",
					"node_type": "KSampler",
					"exception_type": "torch.OutOfMemoryError",
					"exception_message": "Allocation on device 0 would exceed allowed memory",
					"traceback": ["File \"execution.py\", line 1", "torch.OutOfMemoryError"],
					"timestamp": 1755000105000
				}]
			]
		}
	}`

	// THE LAG RACE: success recorded, outputs map still empty.
	entryOutputsPending = `{
		"prompt": [10, "p-lag", {"3": {"class_type": "SaveImage", "inputs": {}}}, {}, ["9"]],
		"outputs": {},
		"status": {
			"status_str": "success",
			"completed": true,
			"messages": [
				["execution_start", {"prompt_id": "p-lag", "timestamp": 1755000200000}],
				["execution_success", {"prompt_id": "p-lag", "timestamp": 1755000260000}]
			]
		}
	}`

	// Started, nothing terminal yet.
	entryRunning = `{
		"prompt": [11, "p-run", {"3": {"class_type": "KSampler", "inputs": {}}}, {}, ["9"]],
		"outputs": {},
		"status": {
			"status_str": "",
			"completed": false,
			"messages": [["execution_start", {"prompt_id": "p-run", "timestamp": 1755000300000}]]
		}
	}`

	// Interrupted runs report status_str "error"; the interrupt message is the truth.
	entryInterrupted = `{
		"prompt": [12, "p-int", {"3": {"class_type": "KSampler", "inputs": {}}}, {}, ["9"]],
		"outputs": {},
		"status": {
			"status_str": "error",
			"completed": false,
			"messages": [
				["execution_start", {"prompt_id": "p-int", "timestamp": 1755000400000}],
				["execution_interrupted", {"prompt_id": "p-int", "timestamp": 1755000401000}]
			]
		}
	}`

	// Inverted pair: success BEFORE start. A missing duration beats a wrong one.
	entryInverted = `{
		"prompt": [13, "p-inv", {}, {}, []],
		"outputs": {"9": {"images": [{"filename": "x.png", "type": "output"}]}},
		"status": {
			"status_str": "success",
			"completed": true,
			"messages": [
				["execution_start", {"timestamp": 1755000500000}],
				["execution_success", {"timestamp": 1755000499000}]
			]
		}
	}`

	// Queued but not started.
	entryPending = `{
		"prompt": [14, "p-pend", {}, {}, []],
		"outputs": {},
		"status": {"status_str": "", "completed": false, "messages": []}
	}`
)

func mustParse(t *testing.T, promptID, body string) Entry {
	t.Helper()
	entry, err := ParseEntry(promptID, json.RawMessage(body))
	if err != nil {
		t.Fatalf("ParseEntry(%s): unexpected error: %v", promptID, err)
	}
	return entry
}

func TestParseEntryStateAndTiming(t *testing.T) {
	tests := []struct {
		name          string
		promptID      string
		body          string
		wantState     State
		wantTerminal  bool
		wantStartMS   int64
		wantSuccessMS int64
		wantDuration  int64
		wantDurOK     bool
		wantElapsed   int64
		wantAnomaly   bool
		wantOutputs   int
		wantNodes     int
	}{
		{
			name: "epoch milliseconds", promptID: "p-ms", body: entryEpochMS,
			wantState: StateCompleted, wantTerminal: true,
			wantStartMS: 1755000000000, wantSuccessMS: 1755000042500,
			wantDuration: 42500, wantDurOK: true,
			wantOutputs: 1, wantNodes: 1,
		},
		{
			// Same wall time as the millisecond fixture — proof the magnitude
			// rule normalises rather than mangles.
			name: "epoch seconds normalised to ms", promptID: "p-sec", body: entryEpochSeconds,
			wantState: StateCompleted, wantTerminal: true,
			wantStartMS: 1755000000000, wantSuccessMS: 1755000042500,
			wantDuration: 42500, wantDurOK: true,
			wantOutputs: 1, wantNodes: 1,
		},
		{
			name: "execution_error", promptID: "p-err", body: entryError,
			wantState: StateFailed, wantTerminal: true,
			wantStartMS: 1755000100000, wantSuccessMS: 0,
			wantDuration: 0, wantDurOK: false, wantElapsed: 5000,
			wantOutputs: 0, wantNodes: 0,
		},
		{
			// The lag race must NOT read as completed and must NOT be terminal.
			name: "completed with empty outputs", promptID: "p-lag", body: entryOutputsPending,
			wantState: StateCompletedOutputsPending, wantTerminal: false,
			wantStartMS: 1755000200000, wantSuccessMS: 1755000260000,
			wantDuration: 60000, wantDurOK: true,
			wantOutputs: 0, wantNodes: 0,
		},
		{
			name: "running", promptID: "p-run", body: entryRunning,
			wantState: StateRunning, wantTerminal: false,
			wantStartMS: 1755000300000,
			wantOutputs: 0, wantNodes: 0,
		},
		{
			name: "interrupted beats status_str error", promptID: "p-int", body: entryInterrupted,
			wantState: StateInterrupted, wantTerminal: true,
			wantStartMS: 1755000400000, wantElapsed: 1000,
			wantOutputs: 0, wantNodes: 0,
		},
		{
			name: "inverted timestamps refuse a duration", promptID: "p-inv", body: entryInverted,
			wantState: StateCompleted, wantTerminal: true,
			wantStartMS: 1755000500000, wantSuccessMS: 1755000499000,
			wantDuration: 0, wantDurOK: false, wantAnomaly: true,
			wantOutputs: 1, wantNodes: 1,
		},
		{
			name: "pending", promptID: "p-pend", body: entryPending,
			wantState: StatePending, wantTerminal: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry := mustParse(t, tc.promptID, tc.body)
			if entry.PromptID != tc.promptID {
				t.Errorf("PromptID = %q, want %q", entry.PromptID, tc.promptID)
			}
			if entry.State != tc.wantState {
				t.Errorf("State = %q, want %q", entry.State, tc.wantState)
			}
			if got := entry.State.Terminal(); got != tc.wantTerminal {
				t.Errorf("State.Terminal() = %v, want %v", got, tc.wantTerminal)
			}
			if entry.Timestamps.StartMS != tc.wantStartMS {
				t.Errorf("StartMS = %d, want %d", entry.Timestamps.StartMS, tc.wantStartMS)
			}
			if entry.Timestamps.SuccessMS != tc.wantSuccessMS {
				t.Errorf("SuccessMS = %d, want %d", entry.Timestamps.SuccessMS, tc.wantSuccessMS)
			}
			if entry.DurationMS != tc.wantDuration {
				t.Errorf("DurationMS = %d, want %d", entry.DurationMS, tc.wantDuration)
			}
			if entry.DurationOK != tc.wantDurOK {
				t.Errorf("DurationOK = %v, want %v", entry.DurationOK, tc.wantDurOK)
			}
			if entry.ElapsedMS != tc.wantElapsed {
				t.Errorf("ElapsedMS = %d, want %d", entry.ElapsedMS, tc.wantElapsed)
			}
			if hasAnomaly := entry.TimingAnomaly != ""; hasAnomaly != tc.wantAnomaly {
				t.Errorf("TimingAnomaly = %q, want anomaly=%v", entry.TimingAnomaly, tc.wantAnomaly)
			}
			if len(entry.Outputs) != tc.wantOutputs {
				t.Errorf("len(Outputs) = %d, want %d", len(entry.Outputs), tc.wantOutputs)
			}
			if len(entry.OutputNodes) != tc.wantNodes {
				t.Errorf("len(OutputNodes) = %d, want %d", len(entry.OutputNodes), tc.wantNodes)
			}
		})
	}
}

func TestParseEntryErrorIsVerbatim(t *testing.T) {
	entry := mustParse(t, "p-err", entryError)
	if entry.Error == nil {
		t.Fatal("Error = nil, want the execution_error payload")
	}
	if entry.Error.NodeID != "3" || entry.Error.NodeType != "KSampler" {
		t.Errorf("node = %q/%q, want 3/KSampler", entry.Error.NodeID, entry.Error.NodeType)
	}
	if entry.Error.ExceptionType != "torch.OutOfMemoryError" {
		t.Errorf("ExceptionType = %q", entry.Error.ExceptionType)
	}
	const wantMessage = "Allocation on device 0 would exceed allowed memory"
	if entry.Error.ExceptionMessage != wantMessage {
		t.Errorf("ExceptionMessage = %q, want %q", entry.Error.ExceptionMessage, wantMessage)
	}
	if len(entry.Error.Traceback) != 2 {
		t.Fatalf("len(Traceback) = %d, want 2", len(entry.Error.Traceback))
	}
	if !strings.Contains(entry.Error.Traceback[0], "execution.py") {
		t.Errorf("Traceback[0] = %q, want the raw frame text", entry.Error.Traceback[0])
	}
	// Raw must carry the whole payload so nothing is summarised away.
	if entry.Error.Raw["prompt_id"] != "p-err" {
		t.Errorf("Raw lost fields: %v", entry.Error.Raw)
	}
}

func TestParseOutputsOrderingAndNonFilePayloads(t *testing.T) {
	raw := json.RawMessage(`{
		"10": {"images": [{"filename": "b.png", "subfolder": "s", "type": "output"}]},
		"9":  {"gifs": [{"filename": "a.webp", "type": "output"}],
		       "images": [{"filename": "a.png", "type": "output"}]},
		"11": {"text": ["a caption, no file here"]}
	}`)
	outputs, nodes := ParseOutputs(raw)

	// Node ids sort numerically: 9 before 10 before 11.
	wantNodes := []string{"9", "10", "11"}
	if len(nodes) != len(wantNodes) {
		t.Fatalf("nodes = %v, want %v", nodes, wantNodes)
	}
	for i, want := range wantNodes {
		if nodes[i] != want {
			t.Fatalf("nodes = %v, want %v", nodes, wantNodes)
		}
	}
	// Node 11 contributes a node id (outputs ARE published) but no file.
	want := []Output{
		{NodeID: "9", Key: "gifs", Filename: "a.webp", Type: "output"},
		{NodeID: "9", Key: "images", Filename: "a.png", Type: "output"},
		{NodeID: "10", Key: "images", Filename: "b.png", Subfolder: "s", Type: "output"},
	}
	if len(outputs) != len(want) {
		t.Fatalf("outputs = %+v, want %+v", outputs, want)
	}
	for i := range want {
		if outputs[i] != want[i] {
			t.Errorf("outputs[%d] = %+v, want %+v", i, outputs[i], want[i])
		}
	}
}

func TestClassifyTable(t *testing.T) {
	tests := []struct {
		name         string
		statusStr    string
		completed    bool
		ts           Timestamps
		outputNodes  int
		hasError     bool
		hasInterrupt bool
		want         State
	}{
		{name: "empty", want: StatePending},
		{name: "started only", ts: Timestamps{StartMS: 1}, want: StateRunning},
		{name: "success with outputs", statusStr: "success", completed: true, ts: Timestamps{StartMS: 1, SuccessMS: 2}, outputNodes: 1, want: StateCompleted},
		{name: "success without outputs", statusStr: "success", completed: true, ts: Timestamps{StartMS: 1, SuccessMS: 2}, want: StateCompletedOutputsPending},
		{name: "completed flag without success message", completed: true, outputNodes: 2, want: StateCompleted},
		{name: "explicit error", statusStr: "error", hasError: true, ts: Timestamps{StartMS: 1, ErrorMS: 2}, want: StateFailed},
		{name: "status error without message", statusStr: "error", ts: Timestamps{StartMS: 1}, want: StateFailed},
		{name: "interrupt outranks status error", statusStr: "error", hasInterrupt: true, ts: Timestamps{StartMS: 1}, want: StateInterrupted},
		{name: "error outranks interrupt", hasError: true, hasInterrupt: true, want: StateFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.statusStr, tc.completed, tc.ts, tc.outputNodes, tc.hasError, tc.hasInterrupt)
			if got != tc.want {
				t.Errorf("Classify(...) = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDurationAndElapsed(t *testing.T) {
	tests := []struct {
		name        string
		ts          Timestamps
		wantMS      int64
		wantOK      bool
		wantAnomaly bool
		wantElapsed int64
		wantElapsOK bool
	}{
		{name: "clean pair", ts: Timestamps{StartMS: 1000, SuccessMS: 4000}, wantMS: 3000, wantOK: true, wantElapsed: 3000, wantElapsOK: true},
		{name: "no success", ts: Timestamps{StartMS: 1000}, wantOK: false},
		{name: "no start", ts: Timestamps{SuccessMS: 4000}, wantOK: false},
		{name: "inverted", ts: Timestamps{StartMS: 4000, SuccessMS: 1000}, wantOK: false, wantAnomaly: true},
		{name: "error elapsed", ts: Timestamps{StartMS: 1000, ErrorMS: 2500}, wantOK: false, wantElapsed: 1500, wantElapsOK: true},
		{name: "interrupt elapsed", ts: Timestamps{StartMS: 1000, InterruptedMS: 1200}, wantOK: false, wantElapsed: 200, wantElapsOK: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotMS, gotOK, anomaly := DurationMS(tc.ts)
			if gotMS != tc.wantMS || gotOK != tc.wantOK {
				t.Errorf("DurationMS = (%d, %v), want (%d, %v)", gotMS, gotOK, tc.wantMS, tc.wantOK)
			}
			if hasAnomaly := anomaly != ""; hasAnomaly != tc.wantAnomaly {
				t.Errorf("anomaly = %q, want anomaly=%v", anomaly, tc.wantAnomaly)
			}
			elapsed, elapsedOK := ElapsedMS(tc.ts)
			if elapsedOK != tc.wantElapsOK || elapsed != tc.wantElapsed {
				t.Errorf("ElapsedMS = (%d, %v), want (%d, %v)", elapsed, elapsedOK, tc.wantElapsed, tc.wantElapsOK)
			}
		})
	}
}

func TestTimestampsFromPicksEarliestStartAndLatestTerminal(t *testing.T) {
	messages := []Message{
		{Event: EventExecutionStart, Data: map[string]interface{}{"timestamp": float64(3000)}},
		{Event: EventExecutionStart, Data: map[string]interface{}{"timestamp": float64(2000)}},
		{Event: EventExecutionSuccess, Data: map[string]interface{}{"timestamp": float64(9000)}},
		{Event: EventExecutionSuccess, Data: map[string]interface{}{"timestamp": float64(8000)}},
		{Event: EventExecutionCached, Data: map[string]interface{}{"timestamp": float64(2500)}},
		{Event: EventExecutionStart, Data: map[string]interface{}{}},
	}
	ts := TimestampsFrom(messages)
	// Values below the ms threshold are treated as seconds and scaled.
	if ts.StartMS != 2_000_000 {
		t.Errorf("StartMS = %d, want 2000000 (2000s normalised)", ts.StartMS)
	}
	if ts.SuccessMS != 9_000_000 {
		t.Errorf("SuccessMS = %d, want 9000000 (9000s normalised)", ts.SuccessMS)
	}
}

func TestParseMessagesSkipsMalformed(t *testing.T) {
	raw := json.RawMessage(`[
		["execution_start", {"timestamp": 1}],
		"not-a-pair",
		[],
		[123, {"timestamp": 2}],
		["execution_success"]
	]`)
	messages := ParseMessages(raw)
	if len(messages) != 2 {
		t.Fatalf("len(messages) = %d, want 2: %+v", len(messages), messages)
	}
	if messages[0].Event != EventExecutionStart || messages[1].Event != EventExecutionSuccess {
		t.Errorf("events = %q/%q", messages[0].Event, messages[1].Event)
	}
	if messages[1].Data != nil {
		t.Errorf("data-less message got Data = %v", messages[1].Data)
	}
}

func TestParseAll(t *testing.T) {
	payload := []byte(`{
		"p-lag": ` + entryOutputsPending + `,
		"p-ms": ` + entryEpochMS + `,
		"p-err": ` + entryError + `,
		"p-broken": 42
	}`)
	entries, skipped, err := ParseAll(payload)
	if err != nil {
		t.Fatalf("ParseAll: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3", len(entries))
	}
	// Oldest first: p-ms (…042500) then p-err (…105000) then p-lag (…260000).
	wantOrder := []string{"p-ms", "p-err", "p-lag"}
	for i, want := range wantOrder {
		if entries[i].PromptID != want {
			t.Fatalf("order = [%s %s %s], want %v",
				entries[0].PromptID, entries[1].PromptID, entries[2].PromptID, wantOrder)
		}
	}
	// A malformed entry is reported, never silently dropped.
	if len(skipped) != 1 || skipped[0].PromptID != "p-broken" {
		t.Fatalf("skipped = %+v, want one entry for p-broken", skipped)
	}
}

func TestParseAllEmptyHistory(t *testing.T) {
	entries, skipped, err := ParseAll([]byte(`{}`))
	if err != nil {
		t.Fatalf("ParseAll: %v", err)
	}
	if len(entries) != 0 || len(skipped) != 0 {
		t.Errorf("entries=%v skipped=%v, want both empty", entries, skipped)
	}
}

func TestParseOne(t *testing.T) {
	tests := []struct {
		name      string
		payload   string
		promptID  string
		wantFound bool
		wantState State
	}{
		{
			name: "keyed by prompt id", payload: `{"p-ms": ` + entryEpochMS + `}`,
			promptID: "p-ms", wantFound: true, wantState: StateCompleted,
		},
		{
			// ComfyUI answers an unknown prompt with {} — that is "not found",
			// never "finished with nothing".
			name: "unknown prompt", payload: `{}`, promptID: "p-nope", wantFound: false,
		},
		{
			name: "single key under a different id", payload: `{"other": ` + entryOutputsPending + `}`,
			promptID: "p-lag", wantFound: true, wantState: StateCompletedOutputsPending,
		},
		{
			name: "bare entry body", payload: entryError, promptID: "p-err",
			wantFound: true, wantState: StateFailed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry, found, err := ParseOne([]byte(tc.payload), tc.promptID)
			if err != nil {
				t.Fatalf("ParseOne: %v", err)
			}
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}
			if found && entry.State != tc.wantState {
				t.Errorf("State = %q, want %q", entry.State, tc.wantState)
			}
		})
	}
}

func TestParsePromptTuple(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantOK     bool
		wantNumber int64
		wantID     string
		wantGraph  bool
	}{
		{
			name: "queue item", raw: `[42, "p-1", {"3": {"class_type": "KSampler"}}, {}, ["9"]]`,
			wantOK: true, wantNumber: 42, wantID: "p-1", wantGraph: true,
		},
		{name: "no graph", raw: `[1, "p-2"]`, wantOK: true, wantNumber: 1, wantID: "p-2"},
		{name: "graph slot is not an object", raw: `[1, "p-3", null, {}]`, wantOK: true, wantNumber: 1, wantID: "p-3"},
		{name: "too short", raw: `[1]`},
		{name: "missing id", raw: `[1, ""]`},
		{name: "not a tuple", raw: `{"prompt_id": "p-4"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tuple, ok := ParsePromptTuple(json.RawMessage(tc.raw))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if tuple.Number != tc.wantNumber || tuple.PromptID != tc.wantID {
				t.Errorf("tuple = %d/%q, want %d/%q", tuple.Number, tuple.PromptID, tc.wantNumber, tc.wantID)
			}
			if hasGraph := len(tuple.Graph) > 0; hasGraph != tc.wantGraph {
				t.Errorf("graph present = %v, want %v", hasGraph, tc.wantGraph)
			}
		})
	}
}

func TestEntryCarriesGraphForHashing(t *testing.T) {
	entry := mustParse(t, "p-ms", entryEpochMS)
	if len(entry.PromptGraph) == 0 {
		t.Fatal("PromptGraph is empty; an ingested run could not be hashed into a shape")
	}
	var graph map[string]json.RawMessage
	if err := json.Unmarshal(entry.PromptGraph, &graph); err != nil {
		t.Fatalf("PromptGraph is not an object: %v", err)
	}
	if _, ok := graph["3"]; !ok {
		t.Errorf("PromptGraph = %s, want node 3", entry.PromptGraph)
	}
	if entry.QueueNumber != 7 {
		t.Errorf("QueueNumber = %d, want 7", entry.QueueNumber)
	}
}
