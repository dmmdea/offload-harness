// Tests for the submit/attach decision logic.
//
// NOT generated — hand-written and preserved across regeneration.
//
// Every case here pins a real failure mode observed against a live ComfyUI 0.32.0: the
// resubmit that burned ~30 GPU-minutes, the 200-with-node_errors partial accept, the gateway
// envelope that is valid JSON with no prompt_id, and the empty COMBO option list that reads
// as a missing file but means an unregistered model class.
package submit

import (
	"encoding/json"
	"strings"
	"testing"

	"comfyui-pp-cli/internal/store"
)

func mustGraph(t *testing.T, s string) store.APIGraph {
	t.Helper()
	g, _, err := ParseGraph([]byte(s))
	if err != nil {
		t.Fatalf("ParseGraph(%s): %v", s, err)
	}
	return g
}

// ------------------------------------------------------------------ graph input

func TestParseGraph(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantErr   string // substring; "" means success
		wantNodes int
	}{
		{
			name:      "plain API graph",
			in:        `{"1":{"class_type":"KSampler","inputs":{"steps":20}},"2":{"class_type":"SaveImage","inputs":{"images":["1",0]}}}`,
			wantNodes: 2,
		},
		{
			name:      "saved POST body is unwrapped",
			in:        `{"prompt":{"1":{"class_type":"KSampler","inputs":{"steps":20}}},"client_id":"x","prompt_id":"y"}`,
			wantNodes: 1,
		},
		{
			// Both files are called workflow.json; posting the UI one yields an opaque
			// server-side failure, so it is rejected with the actual fix.
			name:    "UI workflow is rejected with the export instruction",
			in:      `{"last_node_id":9,"nodes":[{"id":1,"type":"KSampler"}],"links":[]}`,
			wantErr: "Export (API)",
		},
		{
			name:    "node without class_type is rejected",
			in:      `{"1":{"inputs":{"steps":20}}}`,
			wantErr: "no class_type",
		},
		{
			name:    "empty object",
			in:      `{}`,
			wantErr: "no nodes",
		},
		{
			name:    "empty input",
			in:      `   `,
			wantErr: "empty input",
		},
		{
			name:    "not JSON",
			in:      `<html>502 Bad Gateway</html>`,
			wantErr: "parsing graph JSON",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g, raw, err := ParseGraph([]byte(tc.in))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(g) != tc.wantNodes {
				t.Fatalf("nodes = %d, want %d", len(g), tc.wantNodes)
			}
			if !json.Valid(raw) {
				t.Fatalf("raw graph is not valid JSON: %s", raw)
			}
		})
	}
}

func TestParseGraph_NormalisesMissingInputsSoTheLeaseStillMatches(t *testing.T) {
	// A node written without an "inputs" key must hash identically to the same node with
	// an empty object, or the submit lease fails to recognise a resubmit of the same work.
	without := mustGraph(t, `{"1":{"class_type":"PreviewImage"}}`)
	with := mustGraph(t, `{"1":{"class_type":"PreviewImage","inputs":{}}}`)

	a, err := Identify(without)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Identify(with)
	if err != nil {
		t.Fatal(err)
	}
	if a.GraphSHA != b.GraphSHA {
		t.Fatalf("missing inputs hashed differently: %s vs %s", a.GraphSHA, b.GraphSHA)
	}
}

func TestParseGraph_KeepsRawBytesForTheWire(t *testing.T) {
	// Unknown node fields must survive to the server: the CLI posts the file's own bytes,
	// not a re-marshalled projection of the fields this CLI happens to model.
	in := `{"1":{"class_type":"KSampler","inputs":{"steps":20},"is_changed":["abc"]}}`
	_, raw, err := ParseGraph([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "is_changed") {
		t.Fatalf("raw graph dropped an unmodelled field: %s", raw)
	}
}

func TestIdentify_SeedOnlyChangeKeepsTheShape(t *testing.T) {
	a, err := Identify(mustGraph(t, `{"1":{"class_type":"KSampler","inputs":{"seed":1,"steps":20}}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Identify(mustGraph(t, `{"1":{"class_type":"KSampler","inputs":{"seed":777,"steps":20}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.GraphSHA == b.GraphSHA {
		t.Fatal("a seed change must produce a different graph_sha (the lease dedupes on exact identity)")
	}
	if a.ShapeSHA != b.ShapeSHA {
		t.Fatal("a seed change must NOT change the shape_sha (timings stay comparable)")
	}
	if a.NodeCount != 1 || a.Histogram["KSampler"] != 1 {
		t.Fatalf("fingerprint = %+v", a)
	}
}

// ------------------------------------------------------------------ prompt id + request

func TestNewPromptID_IsAVersion4UUID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id, err := NewPromptID()
		if err != nil {
			t.Fatal(err)
		}
		if !isUUIDShape(id) {
			t.Fatalf("prompt_id %q is not UUID-shaped", id)
		}
		if id[14] != '4' {
			t.Fatalf("prompt_id %q is not version 4", id)
		}
		switch id[19] {
		case '8', '9', 'a', 'b':
		default:
			t.Fatalf("prompt_id %q has the wrong RFC 4122 variant nibble", id)
		}
		if seen[id] {
			t.Fatalf("prompt_id %q was minted twice", id)
		}
		seen[id] = true
	}
}

func TestBuildRequest_PostsTheGraphVerbatim(t *testing.T) {
	graph := json.RawMessage(`{"1":{"class_type":"KSampler","inputs":{"steps":20},"is_changed":["x"]}}`)
	body, err := json.Marshal(BuildRequest(graph, "11111111-2222-4333-8444-555555555555"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Prompt   json.RawMessage `json:"prompt"`
		PromptID string          `json:"prompt_id"`
		ClientID string          `json:"client_id"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.PromptID != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("prompt_id = %q", got.PromptID)
	}
	if got.ClientID != ClientID {
		t.Fatalf("client_id = %q, want %q", got.ClientID, ClientID)
	}
	if !strings.Contains(string(got.Prompt), "is_changed") {
		t.Fatalf("prompt field lost graph content: %s", got.Prompt)
	}
}

// ------------------------------------------------------------------ the lease

func TestDecideLease(t *testing.T) {
	// This is the structural fix for the wrapper that resubmitted instead of waiting.
	tests := []struct {
		name       string
		active     string
		found      bool
		force      bool
		wantAttach bool
		wantID     string
	}{
		{name: "no active run -> submit", found: false, wantAttach: false},
		{name: "empty id treated as no active run", active: "  ", found: true, wantAttach: false},
		{name: "active run -> attach, never resubmit", active: "p1", found: true, wantAttach: true, wantID: "p1"},
		{name: "--force overrides the lease deliberately", active: "p1", found: true, force: true, wantAttach: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideLease(tc.active, tc.found, tc.force)
			if got.Attach != tc.wantAttach {
				t.Fatalf("Attach = %v, want %v (reason: %s)", got.Attach, tc.wantAttach, got.Reason)
			}
			if got.PromptID != tc.wantID {
				t.Fatalf("PromptID = %q, want %q", got.PromptID, tc.wantID)
			}
			if strings.TrimSpace(got.Reason) == "" {
				t.Fatal("every decision must carry a reason")
			}
		})
	}
}

// ------------------------------------------------------------------ lint

func TestLint_LoadImageTakesABareFilename(t *testing.T) {
	tests := []struct {
		name     string
		graph    string
		wantFind int
	}{
		{
			name:     "windows absolute path is rejected",
			graph:    `{"1":{"class_type":"LoadImage","inputs":{"image":"C:\\renders\\a.png"}}}`,
			wantFind: 1,
		},
		{
			name:     "UNC path is rejected",
			graph:    `{"1":{"class_type":"LoadImage","inputs":{"image":"\\\\nas\\share\\a.png"}}}`,
			wantFind: 1,
		},
		{
			name:     "posix absolute path is rejected",
			graph:    `{"1":{"class_type":"LoadImageMask","inputs":{"image":"/home/x/a.png"}}}`,
			wantFind: 1,
		},
		{
			name:     "bare filename is fine",
			graph:    `{"1":{"class_type":"LoadImage","inputs":{"image":"a.png"}}}`,
			wantFind: 0,
		},
		{
			name:     "subfolder inside the input dir is fine",
			graph:    `{"1":{"class_type":"LoadImage","inputs":{"image":"batch7/a.png"}}}`,
			wantFind: 0,
		},
		{
			name:     "unrelated class with an absolute path is not this rule's business",
			graph:    `{"1":{"class_type":"SaveImage","inputs":{"filename_prefix":"/tmp/out"}}}`,
			wantFind: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Lint(mustGraph(t, tc.graph))
			if len(got) != tc.wantFind {
				t.Fatalf("findings = %d (%+v), want %d", len(got), got, tc.wantFind)
			}
			if tc.wantFind > 0 && !strings.Contains(got[0].Message, "bare filename") {
				t.Fatalf("finding message does not state the rule: %q", got[0].Message)
			}
		})
	}
}

// ------------------------------------------------------------------ response classification

const acceptedBody = `{"prompt_id":"11111111-2222-4333-8444-555555555555","number":12,"node_errors":{}}`

// partialBody is the shape that matters most: HTTP 200, a real prompt_id, AND node_errors.
// ComfyUI validates each output branch independently and only 400s when NO branch is good.
const partialBody = `{"prompt_id":"11111111-2222-4333-8444-555555555555","number":13,"node_errors":{"12":{"class_type":"UpscaleModelLoader","dependent_outputs":["9"],"errors":[{"type":"value_not_in_list","message":"Value not in list","details":"upscale_model: 'x.safetensors' not in []","extra_info":{"input_name":"upscale_model","input_config":[["COMBO"],{"options":[]}],"received_value":"x.safetensors"}}]}}}`

const rejectedBody = `{"error":{"type":"prompt_outputs_failed_validation","message":"Prompt outputs failed validation","details":"","extra_info":{}},"node_errors":{"4":{"class_type":"CheckpointLoaderSimple","dependent_outputs":["9","14"],"errors":[{"type":"value_not_in_list","message":"Value not in list","details":"ckpt_name: 'nope.safetensors' not in ['sd15.safetensors']","extra_info":{"input_name":"ckpt_name","input_config":[["sd15.safetensors"],{}],"received_value":"nope.safetensors"}}]}}}`

func TestClassify(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantOutcome Outcome
		wantExit    int
		wantPrompt  string
		wantDropped []string
	}{
		{
			name:        "200 with prompt_id and empty node_errors is accepted",
			status:      200,
			body:        acceptedBody,
			wantOutcome: OutcomeAccepted,
			wantExit:    0,
			wantPrompt:  "11111111-2222-4333-8444-555555555555",
		},
		{
			name:        "200 WITH node_errors is a partial accept, never success",
			status:      200,
			body:        partialBody,
			wantOutcome: OutcomePartialAccept,
			wantExit:    ExitPartialAccept,
			wantPrompt:  "11111111-2222-4333-8444-555555555555",
			wantDropped: []string{"9"},
		},
		{
			name:        "400 validation failure is a rejection",
			status:      400,
			body:        rejectedBody,
			wantOutcome: OutcomeRejected,
			wantExit:    ExitRejected,
			wantDropped: []string{"9", "14"},
		},
		{
			// A gateway error envelope is valid JSON. Without the prompt_id assertion
			// this would be reported as an accepted 20-minute render that never
			// existed.
			name:        "200 without prompt_id is malformed, not accepted",
			status:      200,
			body:        `{"status":"ok","message":"queued"}`,
			wantOutcome: OutcomeMalformed,
			wantExit:    ExitMalformed,
		},
		{
			name:        "200 with an HTML body is malformed",
			status:      200,
			body:        `<html><title>Just a moment</title></html>`,
			wantOutcome: OutcomeMalformed,
			wantExit:    ExitMalformed,
		},
		{
			name:        "500 with an HTML body is a rejection",
			status:      500,
			body:        `<html>502</html>`,
			wantOutcome: OutcomeRejected,
			wantExit:    ExitRejected,
		},
		{
			name:        "empty 200 body is malformed",
			status:      200,
			body:        ``,
			wantOutcome: OutcomeMalformed,
			wantExit:    ExitMalformed,
		},
		{
			// Unrecognised but non-empty node_errors must still degrade the outcome:
			// a shape this parser does not know is not permission to call it success.
			name:        "unparseable non-empty node_errors still degrades to partial",
			status:      200,
			body:        `{"prompt_id":"11111111-2222-4333-8444-555555555555","node_errors":["something new"]}`,
			wantOutcome: OutcomePartialAccept,
			wantExit:    ExitPartialAccept,
			wantPrompt:  "11111111-2222-4333-8444-555555555555",
		},
		{
			name:        "3xx is malformed",
			status:      302,
			body:        `{"prompt_id":"11111111-2222-4333-8444-555555555555"}`,
			wantOutcome: OutcomeMalformed,
			wantExit:    ExitMalformed,
			wantPrompt:  "11111111-2222-4333-8444-555555555555",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.status, []byte(tc.body))
			if got.Outcome != tc.wantOutcome {
				t.Fatalf("outcome = %q, want %q (reason: %s)", got.Outcome, tc.wantOutcome, got.Reason)
			}
			if got.Outcome.ExitCode() != tc.wantExit {
				t.Fatalf("exit code = %d, want %d", got.Outcome.ExitCode(), tc.wantExit)
			}
			if got.PromptID != tc.wantPrompt {
				t.Fatalf("prompt_id = %q, want %q", got.PromptID, tc.wantPrompt)
			}
			if len(tc.wantDropped) > 0 {
				if strings.Join(got.DroppedOutputs, ",") != strings.Join(tc.wantDropped, ",") {
					t.Fatalf("dropped outputs = %v, want %v", got.DroppedOutputs, tc.wantDropped)
				}
			}
			if got.Outcome != OutcomeAccepted && got.Outcome != OutcomeMalformed && got.HTTPStatus != tc.status {
				t.Fatalf("http status = %d, want %d", got.HTTPStatus, tc.status)
			}
		})
	}
}

func TestClassify_NodeErrorsSurvivedVerbatim(t *testing.T) {
	// Verbatim means byte-identical: no re-indenting, no summarising, no dropping the
	// fields this CLI does not model.
	got := Classify(400, []byte(rejectedBody))
	raw := string(got.NodeErrorsRaw)
	if !strings.Contains(rejectedBody, raw) {
		t.Fatalf("node_errors was not preserved byte-for-byte:\n%s", raw)
	}
	for _, needle := range []string{"value_not_in_list", "nope.safetensors", "sd15.safetensors", "dependent_outputs"} {
		if !strings.Contains(raw, needle) {
			t.Fatalf("verbatim node_errors lost %q: %s", needle, raw)
		}
	}
	if got.ErrorType != "prompt_outputs_failed_validation" {
		t.Fatalf("error type = %q", got.ErrorType)
	}
	if len(got.NodeErrors) != 1 || got.NodeErrors[0].NodeID != "4" || got.NodeErrors[0].ClassType != "CheckpointLoaderSimple" {
		t.Fatalf("structured breakdown = %+v", got.NodeErrors)
	}
}

func TestClassify_EmptyOptionsMeansUnregisteredClassNotMissingFile(t *testing.T) {
	// The real incident: latent_upscale_models had no extra_model_paths.yaml key, the
	// loader offered ZERO options, and ComfyUI reported only `not in []`. Reading that as
	// "file missing" sends you hunting for a download you already have.
	got := Classify(200, []byte(partialBody))
	if len(got.NodeErrors) != 1 || len(got.NodeErrors[0].Errors) != 1 {
		t.Fatalf("breakdown = %+v", got.NodeErrors)
	}
	detail := got.NodeErrors[0].Errors[0]
	if detail.ComboShape != "v3" {
		t.Fatalf("combo shape = %q, want v3 (options at index 1)", detail.ComboShape)
	}
	if detail.Visibility != "class-unregistered" {
		t.Fatalf("visibility = %q, want class-unregistered", detail.Visibility)
	}
	if !strings.Contains(detail.Diagnosis, "extra_model_paths.yaml") {
		t.Fatalf("diagnosis does not name the actual fix: %q", detail.Diagnosis)
	}
	if detail.InputName != "upscale_model" || detail.ReceivedValue != "x.safetensors" {
		t.Fatalf("input/received = %q/%q", detail.InputName, detail.ReceivedValue)
	}

	// The legacy spec shape (options at index 0) must be read too — ComfyUI ships both
	// simultaneously, so a reader that handles one silently mis-reads the other.
	legacy := Classify(400, []byte(rejectedBody))
	ld := legacy.NodeErrors[0].Errors[0]
	if ld.ComboShape != "legacy" {
		t.Fatalf("legacy combo shape = %q", ld.ComboShape)
	}
	if ld.Visibility != "not-listed" || ld.OptionCount != 1 {
		t.Fatalf("legacy visibility = %q with %d options", ld.Visibility, ld.OptionCount)
	}
}

func TestFormatReport_NamesNodeInputAndDiagnosis(t *testing.T) {
	report := FormatReport(Classify(400, []byte(rejectedBody)))
	for _, needle := range []string{"node 4", "CheckpointLoaderSimple", "value_not_in_list", "ckpt_name", "dropped output branches"} {
		if !strings.Contains(report, needle) {
			t.Fatalf("report missing %q:\n%s", needle, report)
		}
	}
	if FormatReport(Classify(200, []byte(acceptedBody))) != "" {
		t.Fatal("a clean acceptance must produce no error report")
	}
}

// ------------------------------------------------------------------ history + queue

const historySuccess = `{"11111111-2222-4333-8444-555555555555":{"outputs":{"9":{"images":[{"filename":"a.png"}]}},"status":{"status_str":"success","completed":true,"messages":[["execution_start",{"prompt_id":"11111111-2222-4333-8444-555555555555","timestamp":1786556884894}],["execution_cached",{"nodes":[]}],["execution_success",{"prompt_id":"11111111-2222-4333-8444-555555555555","timestamp":1786556914894}]]}}}`

func TestParseHistory(t *testing.T) {
	const id = "11111111-2222-4333-8444-555555555555"
	tests := []struct {
		name          string
		body          string
		wantFound     bool
		wantTerminal  bool
		wantDuration  int64
		wantFailed    bool
		wantInverted  bool
		wantOutputLen int
	}{
		{
			name:          "completed run yields a duration from the two timestamps only",
			body:          historySuccess,
			wantFound:     true,
			wantTerminal:  true,
			wantDuration:  30000,
			wantOutputLen: 1,
		},
		{
			// ComfyUI only materialises the entry once execution starts, so an empty
			// object is the normal "still queued" answer, not an error.
			name: "queued run is simply not found yet",
			body: `{}`,
		},
		{
			name:         "epoch seconds are normalised like epoch milliseconds",
			body:         `{"` + id + `":{"outputs":{},"status":{"status_str":"success","completed":true,"messages":[["execution_start",{"timestamp":1786556884}],["execution_success",{"timestamp":1786556890}]]}}}`,
			wantFound:    true,
			wantTerminal: true,
			wantDuration: 6000,
		},
		{
			name:         "execution error is terminal and carries the node",
			body:         `{"` + id + `":{"outputs":{},"status":{"status_str":"error","completed":false,"messages":[["execution_start",{"timestamp":1786556884894}],["execution_error",{"node_id":"12","node_type":"KSampler","exception_type":"torch.OutOfMemoryError","exception_message":"CUDA out of memory"}]]}}}`,
			wantFound:    true,
			wantTerminal: true,
			wantFailed:   true,
		},
		{
			// A silently wrong duration is worse than a missing one: that is exactly
			// how a false "+49% regression" got reported.
			name:         "inverted timestamps are flagged, not silently negated",
			body:         `{"` + id + `":{"outputs":{},"status":{"status_str":"success","completed":true,"messages":[["execution_start",{"timestamp":1786556914894}],["execution_success",{"timestamp":1786556884894}]]}}}`,
			wantFound:    true,
			wantTerminal: true,
			wantDuration: 0,
			wantInverted: true,
		},
		{
			name:         "a running entry is found but not terminal",
			body:         `{"` + id + `":{"outputs":{},"status":{"status_str":"","completed":false,"messages":[["execution_start",{"timestamp":1786556884894}]]}}}`,
			wantFound:    true,
			wantTerminal: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseHistory([]byte(tc.body), id)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Found != tc.wantFound {
				t.Fatalf("found = %v, want %v", got.Found, tc.wantFound)
			}
			if got.Terminal != tc.wantTerminal {
				t.Fatalf("terminal = %v, want %v", got.Terminal, tc.wantTerminal)
			}
			if got.DurationMS() != tc.wantDuration {
				t.Fatalf("duration = %d, want %d", got.DurationMS(), tc.wantDuration)
			}
			if got.Failed() != tc.wantFailed {
				t.Fatalf("failed = %v, want %v", got.Failed(), tc.wantFailed)
			}
			if got.TimingInverted != tc.wantInverted {
				t.Fatalf("timing inverted = %v, want %v", got.TimingInverted, tc.wantInverted)
			}
			if got.OutputNodeCount != tc.wantOutputLen {
				t.Fatalf("output nodes = %d, want %d", got.OutputNodeCount, tc.wantOutputLen)
			}
		})
	}
}

func TestParseHistory_OtherPromptsAreNotOurs(t *testing.T) {
	got, err := ParseHistory([]byte(historySuccess), "99999999-9999-4999-8999-999999999999")
	if err != nil {
		t.Fatal(err)
	}
	if got.Found {
		t.Fatal("a history entry for a different prompt_id must never be adopted")
	}
}

func TestParseQueue(t *testing.T) {
	body := `{"queue_running":[[11,"aaaa1111-2222-4333-8444-555555555555",{},{},[]]],"queue_pending":[[12,"bbbb1111-2222-4333-8444-555555555555",{},{},[]],[13,"cccc1111-2222-4333-8444-555555555555",{},{},[]]]}`
	tests := []struct {
		name     string
		id       string
		wantOK   bool
		wantRun  bool
		wantPos  int
		wantHead int
	}{
		{name: "running", id: "aaaa1111-2222-4333-8444-555555555555", wantOK: true, wantRun: true},
		{name: "first pending", id: "bbbb1111-2222-4333-8444-555555555555", wantOK: true, wantPos: 1, wantHead: 0},
		{name: "second pending", id: "cccc1111-2222-4333-8444-555555555555", wantOK: true, wantPos: 2, wantHead: 1},
		{name: "absent", id: "dddd1111-2222-4333-8444-555555555555"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseQueue([]byte(body), tc.id)
			if err != nil {
				t.Fatal(err)
			}
			if got.Found != tc.wantOK || got.Running != tc.wantRun || got.Position != tc.wantPos || got.Ahead != tc.wantHead {
				t.Fatalf("state = %+v, want found=%v running=%v pos=%d ahead=%d", got, tc.wantOK, tc.wantRun, tc.wantPos, tc.wantHead)
			}
		})
	}
}

// ------------------------------------------------------------------ ref resolution

func TestClassifyRef(t *testing.T) {
	existing := map[string]bool{
		"graph.json":                           true,
		"deadbeefdeadbeef":                     true,
		"C:\\renders\\graph.json":              true,
		"/home/x/renders/g.json":               true,
		"1234abcd-1234-4bcd-8bcd-1234abcd1234": false,
	}
	exists := func(p string) bool { return existing[p] }

	tests := []struct {
		name    string
		arg     string
		want    RefKind
		wantErr bool
	}{
		{name: "existing file wins even when hash-shaped", arg: "deadbeefdeadbeef", want: RefFile},
		{name: "uuid is a prompt id", arg: "1234abcd-1234-4bcd-8bcd-1234abcd1234", want: RefPromptID},
		{name: "64 hex chars is a graph sha", arg: strings.Repeat("ab", 32), want: RefGraphSHA},
		{name: "short hex prefix is a graph sha", arg: "abcdef12", want: RefGraphSHA},
		{name: "json path is a file", arg: "some/other/graph.json", want: RefFile},
		{name: "existing path is a file", arg: "graph.json", want: RefFile},
		{name: "windows path is a file", arg: "C:\\renders\\graph.json", want: RefFile},
		{name: "empty is an error", arg: "   ", wantErr: true},
		{name: "gibberish is an error", arg: "zzz-not-a-thing", wantErr: true},
		{name: "too-short hex is an error", arg: "abc", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ClassifyRef(tc.arg, exists)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got kind %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("kind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestShortSHA(t *testing.T) {
	if got := ShortSHA(strings.Repeat("a", 64)); len(got) != 12 {
		t.Fatalf("ShortSHA length = %d, want 12", len(got))
	}
	if got := ShortSHA("abc"); got != "abc" {
		t.Fatalf("ShortSHA(%q) = %q", "abc", got)
	}
}
