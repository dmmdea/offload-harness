package exp

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"comfyui-pp-cli/internal/store"
)

func TestParseAddress(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantNode string
		wantIn   string
		wantErr  bool
	}{
		{name: "short form", raw: "12.virtual_vram_gb", wantNode: "12", wantIn: "virtual_vram_gb"},
		{name: "explicit inputs form", raw: "12.inputs.donor_device", wantNode: "12", wantIn: "donor_device"},
		{name: "non numeric node id", raw: "sampler.seed", wantNode: "sampler", wantIn: "seed"},
		{name: "surrounding space", raw: "  12.cfg  ", wantNode: "12", wantIn: "cfg"},
		{name: "empty", raw: "", wantErr: true},
		{name: "no dot", raw: "virtual_vram_gb", wantErr: true},
		{name: "too deep", raw: "12.inputs.a.b", wantErr: true},
		{name: "empty node", raw: ".cfg", wantErr: true},
		{name: "empty input", raw: "12.", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAddress(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseAddress(%q) = %+v, want error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAddress(%q): %v", tc.raw, err)
			}
			if got.NodeID != tc.wantNode || got.Input != tc.wantIn {
				t.Fatalf("ParseAddress(%q) = (%q, %q), want (%q, %q)",
					tc.raw, got.NodeID, got.Input, tc.wantNode, tc.wantIn)
			}
		})
	}
}

func TestParseVary(t *testing.T) {
	tests := []struct {
		name       string
		spec       string
		wantAddr   string
		wantValues []string
		wantErr    string
	}{
		{
			name:       "memory knob sweep",
			spec:       "12.virtual_vram_gb=7,10,13",
			wantAddr:   "12.virtual_vram_gb",
			wantValues: []string{"7", "10", "13"},
		},
		{
			name:       "device values keep their colon",
			spec:       "12.inputs.donor_device=cpu,cuda:1",
			wantAddr:   "12.inputs.donor_device",
			wantValues: []string{"cpu", "cuda:1"},
		},
		{
			name:       "only the first equals splits",
			spec:       "6.text=a=b,c",
			wantAddr:   "6.text",
			wantValues: []string{"a=b", "c"},
		},
		{
			name:       "escaped comma stays in the value",
			spec:       `6.text=a\,b,c`,
			wantAddr:   "6.text",
			wantValues: []string{"a,b", "c"},
		},
		{
			name:       "single value is legal",
			spec:       "12.virtual_vram_gb=10",
			wantAddr:   "12.virtual_vram_gb",
			wantValues: []string{"10"},
		},
		{name: "missing equals", spec: "12.virtual_vram_gb", wantErr: "expected <addr>=<v1>"},
		{name: "no values", spec: "12.virtual_vram_gb=", wantErr: "no values"},
		{name: "empty value in the middle", spec: "12.a=1,,2", wantErr: "is empty"},
		{name: "duplicate value", spec: "12.a=7,7", wantErr: "duplicate value"},
		{name: "bad address", spec: "novnode=1,2", wantErr: "invalid slot address"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseVary(tc.spec)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseVary(%q) = %+v, want error containing %q", tc.spec, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseVary(%q) error = %v, want it to contain %q", tc.spec, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseVary(%q): %v", tc.spec, err)
			}
			if got.Addr != tc.wantAddr {
				t.Fatalf("addr = %q, want %q", got.Addr, tc.wantAddr)
			}
			if !reflect.DeepEqual(got.Values, tc.wantValues) {
				t.Fatalf("values = %#v, want %#v", got.Values, tc.wantValues)
			}
		})
	}
}

func TestExpand(t *testing.T) {
	vram := Var{Addr: "12.virtual_vram_gb", Values: []string{"7", "10", "13"}}
	donor := Var{Addr: "12.donor_device", Values: []string{"cpu", "cuda:1"}}

	t.Run("cartesian is the real memory sweep", func(t *testing.T) {
		arms, err := Expand([]Var{vram, donor}, Cartesian)
		if err != nil {
			t.Fatalf("Expand: %v", err)
		}
		if len(arms) != 6 {
			t.Fatalf("got %d arms, want 6", len(arms))
		}
		wantLabels := []string{
			"virtual_vram_gb=7+donor_device=cpu",
			"virtual_vram_gb=7+donor_device=cuda_1",
			"virtual_vram_gb=10+donor_device=cpu",
			"virtual_vram_gb=10+donor_device=cuda_1",
			"virtual_vram_gb=13+donor_device=cpu",
			"virtual_vram_gb=13+donor_device=cuda_1",
		}
		for i, arm := range arms {
			if arm.Label != wantLabels[i] {
				t.Fatalf("arm %d label = %q, want %q", i, arm.Label, wantLabels[i])
			}
			if arm.Index != i {
				t.Fatalf("arm %d index = %d", i, arm.Index)
			}
		}
		if got := arms[3].Vars["12.virtual_vram_gb"]; got != "10" {
			t.Fatalf("arm 3 vram = %q, want 10", got)
		}
		if got := arms[3].Vars["12.donor_device"]; got != "cuda:1" {
			t.Fatalf("arm 3 donor = %q, want cuda:1 (the label sanitises, the value must not)", got)
		}
	})

	t.Run("zip pairs index by index", func(t *testing.T) {
		arms, err := Expand([]Var{
			{Addr: "12.virtual_vram_gb", Values: []string{"7", "10"}},
			{Addr: "12.donor_device", Values: []string{"cpu", "cuda:1"}},
		}, Zip)
		if err != nil {
			t.Fatalf("Expand: %v", err)
		}
		if len(arms) != 2 {
			t.Fatalf("got %d arms, want 2", len(arms))
		}
		if arms[0].Values[0] != "7" || arms[0].Values[1] != "cpu" {
			t.Fatalf("arm 0 = %#v", arms[0].Values)
		}
		if arms[1].Values[0] != "10" || arms[1].Values[1] != "cuda:1" {
			t.Fatalf("arm 1 = %#v", arms[1].Values)
		}
	})

	t.Run("zip rejects ragged dimensions", func(t *testing.T) {
		_, err := Expand([]Var{vram, donor}, Zip)
		if err == nil {
			t.Fatal("want an error for a 3-value dimension zipped against a 2-value one")
		}
		if !strings.Contains(err.Error(), "equal value counts") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("no dimensions is an error", func(t *testing.T) {
		if _, err := Expand(nil, Cartesian); err == nil {
			t.Fatal("want an error when nothing is varied")
		}
	})

	t.Run("duplicate addresses are rejected", func(t *testing.T) {
		_, err := Expand([]Var{vram, {Addr: vram.Addr, Values: []string{"20"}}}, Cartesian)
		if err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("error = %v, want a duplicate-address error", err)
		}
	})

	t.Run("refuses a runaway cartesian product", func(t *testing.T) {
		var vars []Var
		for i := 0; i < 9; i++ {
			vars = append(vars, Var{Addr: "1" + string(rune('a'+i)) + ".x", Values: []string{"a", "b"}})
		}
		_, err := Expand(vars, Cartesian)
		if err == nil || !strings.Contains(err.Error(), "refusing to expand") {
			t.Fatalf("error = %v, want a MaxArms refusal for 512 arms", err)
		}
	})

	t.Run("label collisions get distinct suffixes", func(t *testing.T) {
		// Both values sanitise to the same token; without collision handling the two arms
		// would share a label and one would vanish from the comparison table.
		arms, err := Expand([]Var{{Addr: "12.donor_device", Values: []string{"cuda:1", "cuda/1"}}}, Cartesian)
		if err != nil {
			t.Fatalf("Expand: %v", err)
		}
		if arms[0].Label == arms[1].Label {
			t.Fatalf("labels collided: %q and %q", arms[0].Label, arms[1].Label)
		}
		if arms[1].Label != "donor_device=cuda_1#2" {
			t.Fatalf("second label = %q, want donor_device=cuda_1#2", arms[1].Label)
		}
	})

	t.Run("long values are truncated but stay distinct", func(t *testing.T) {
		long1 := "very-long-checkpoint-name-variant-a.safetensors"
		long2 := "very-long-checkpoint-name-variant-b.safetensors"
		arms, err := Expand([]Var{{Addr: "4.ckpt_name", Values: []string{long1, long2}}}, Cartesian)
		if err != nil {
			t.Fatalf("Expand: %v", err)
		}
		if arms[0].Label == arms[1].Label {
			t.Fatalf("truncated labels collided: %q", arms[0].Label)
		}
		if len(arms[0].Label) > maxLabelLen {
			t.Fatalf("label %q is longer than the clamp", arms[0].Label)
		}
	})
}

func testGraph() store.APIGraph {
	return store.APIGraph{
		"4": {ClassType: "CheckpointLoaderSimple", Inputs: map[string]interface{}{
			"ckpt_name": "sdxl.safetensors",
		}},
		"12": {ClassType: "UnetLoaderGGUFDisTorchMultiGPU", Inputs: map[string]interface{}{
			"unet_name":       "flux.gguf",
			"virtual_vram_gb": float64(13),
			"donor_device":    "cpu",
		}},
	}
}

func TestApplyArm(t *testing.T) {
	base := testGraph()
	arms, err := Expand([]Var{
		{Addr: "12.virtual_vram_gb", Values: []string{"7", "10"}},
		{Addr: "12.inputs.donor_device", Values: []string{"cuda:1"}},
	}, Cartesian)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}

	patched, records, err := ApplyArm(base, arms[0])
	if err != nil {
		t.Fatalf("ApplyArm: %v", err)
	}
	if got := patched["12"].Inputs["virtual_vram_gb"]; got != int64(7) {
		t.Fatalf("virtual_vram_gb = %#v (%T), want int64(7): a widget typed INT rejects the string \"7\"", got, got)
	}
	if got := patched["12"].Inputs["donor_device"]; got != "cuda:1" {
		t.Fatalf("donor_device = %#v, want the string cuda:1", got)
	}
	if len(records) != 2 {
		t.Fatalf("got %d patch records, want 2", len(records))
	}
	if records[0].ClassType != "UnetLoaderGGUFDisTorchMultiGPU" || records[0].Input != "virtual_vram_gb" {
		t.Fatalf("patch record 0 = %+v", records[0])
	}
	if records[0].OldValue != float64(13) {
		t.Fatalf("patch record 0 old value = %#v, want the pre-patch 13", records[0].OldValue)
	}

	t.Run("the template is never mutated", func(t *testing.T) {
		if got := base["12"].Inputs["virtual_vram_gb"]; got != float64(13) {
			t.Fatalf("template mutated to %#v; later arms would inherit an earlier arm's values", got)
		}
		second, _, err := ApplyArm(base, arms[1])
		if err != nil {
			t.Fatalf("ApplyArm: %v", err)
		}
		if got := second["12"].Inputs["virtual_vram_gb"]; got != int64(10) {
			t.Fatalf("second arm vram = %#v, want 10", got)
		}
	})

	t.Run("unknown node is rejected", func(t *testing.T) {
		_, _, err := ApplyArm(base, Arm{Label: "x", Addrs: []string{"99.seed"}, Values: []string{"1"}})
		if err == nil || !strings.Contains(err.Error(), "not found in graph") {
			t.Fatalf("error = %v, want an unknown-node error", err)
		}
	})

	t.Run("unknown input is rejected, not silently created", func(t *testing.T) {
		_, _, err := ApplyArm(base, Arm{Label: "x", Addrs: []string{"12.no_such_knob"}, Values: []string{"1"}})
		if err == nil || !strings.Contains(err.Error(), "has no input") {
			t.Fatalf("error = %v, want an unknown-input error", err)
		}
		if !strings.Contains(err.Error(), "virtual_vram_gb") {
			t.Fatalf("error %v should list the available inputs", err)
		}
	})
}

func TestCoerceValue(t *testing.T) {
	tests := []struct {
		in   string
		want any
	}{
		{in: "7", want: int64(7)},
		{in: "-3", want: int64(-3)},
		{in: "1.5", want: 1.5},
		{in: "true", want: true},
		{in: "false", want: false},
		{in: "cpu", want: "cpu"},
		{in: "cuda:1", want: "cuda:1"},
		{in: "true story", want: "true story"},
		{in: "7 8", want: "7 8"},
		{in: "07", want: "07"},
		{in: "a photo of a cat", want: "a photo of a cat"},
		{in: "flux.gguf", want: "flux.gguf"},
		{in: `"quoted"`, want: "quoted"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := CoerceValue(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("CoerceValue(%q) = %#v (%T), want %#v (%T)", tc.in, got, got, tc.want, tc.want)
			}
		})
	}
}

func TestClassifySubmit(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			name:   "clean accept",
			status: 200,
			body:   `{"prompt_id":"p1","number":3,"node_errors":{}}`,
			want:   SubmitAccepted,
		},
		{
			name:   "HTTP 200 with node_errors is a partial accept, never success",
			status: 200,
			body:   `{"prompt_id":"p1","number":3,"node_errors":{"12":{"errors":[{"type":"value_not_in_list","message":"value not in list"}]}}}`,
			want:   SubmitPartialAccept,
		},
		{
			name:   "every branch invalid is a 400 rejection",
			status: 400,
			body:   `{"error":{"type":"prompt_outputs_failed_validation"},"node_errors":{"12":{}}}`,
			want:   SubmitRejected,
		},
		{
			name:   "2xx without a prompt id is unrecognisable",
			status: 200,
			body:   `{"ok":true}`,
			want:   SubmitUnrecognisable,
		},
		{
			name:   "null node_errors is not a partial accept",
			status: 200,
			body:   `{"prompt_id":"p1","node_errors":null}`,
			want:   SubmitAccepted,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outcome := ParseSubmitResponse([]byte(tc.body))
			if got := ClassifySubmit(tc.status, outcome); got != tc.want {
				t.Fatalf("ClassifySubmit(%d) = %q, want %q", tc.status, got, tc.want)
			}
			if tc.want == SubmitPartialAccept {
				// Rule: node_errors are surfaced VERBATIM, never summarised away.
				if !strings.Contains(string(outcome.NodeErrors), "value_not_in_list") {
					t.Fatalf("node_errors were not preserved verbatim: %s", outcome.NodeErrors)
				}
			}
		})
	}
}

const successHistory = `{"p1":{"prompt":[],"outputs":{},"status":{"status_str":"success","completed":true,"messages":[
	["execution_start",{"prompt_id":"p1","timestamp":1754000000000}],
	["execution_cached",{"nodes":["4","5"],"prompt_id":"p1","timestamp":1754000000100}],
	["execution_success",{"prompt_id":"p1","timestamp":1754000123456}]]}}}`

const secondsHistory = `{"p2":{"status":{"status_str":"success","completed":true,"messages":[
	["execution_start",{"timestamp":1754000000}],
	["execution_success",{"timestamp":1754000123}]]}}}`

const oomHistory = `{"p3":{"status":{"status_str":"error","completed":false,"messages":[
	["execution_start",{"timestamp":1754000000000}],
	["execution_error",{"node_id":"12","node_type":"KSampler","exception_type":"torch.cuda.OutOfMemoryError",
	 "exception_message":"CUDA out of memory. Tried to allocate 2.00 GiB"}]]}}}`

func TestParseHistoryOutcome(t *testing.T) {
	t.Run("millisecond timestamps", func(t *testing.T) {
		entry, ok := FindHistoryEntry([]byte(successHistory), "p1")
		if !ok {
			t.Fatal("entry p1 not found")
		}
		got := ParseHistoryOutcome(entry)
		if !got.Found || !got.Completed || got.StatusStr != "success" {
			t.Fatalf("outcome = %+v", got)
		}
		d, ok := got.DurationMS()
		if !ok || d != 123456 {
			t.Fatalf("duration = %d (ok=%v), want 123456", d, ok)
		}
		if !reflect.DeepEqual(got.CachedNodes, []string{"4", "5"}) {
			t.Fatalf("cached nodes = %#v", got.CachedNodes)
		}
		if !got.Terminal() {
			t.Fatal("a completed run must be terminal")
		}
	})

	t.Run("second timestamps are normalised", func(t *testing.T) {
		entry, _ := FindHistoryEntry([]byte(secondsHistory), "p2")
		got := ParseHistoryOutcome(entry)
		d, ok := got.DurationMS()
		if !ok || d != 123000 {
			t.Fatalf("duration = %d (ok=%v), want 123000", d, ok)
		}
	})

	t.Run("an OOM is terminal, has no duration, and keeps its error verbatim", func(t *testing.T) {
		entry, _ := FindHistoryEntry([]byte(oomHistory), "p3")
		got := ParseHistoryOutcome(entry)
		if !got.Terminal() {
			t.Fatal("an execution_error must be terminal")
		}
		if _, ok := got.DurationMS(); ok {
			t.Fatal("a run that never reached execution_success has no duration")
		}
		if got.ErrorType != "torch.cuda.OutOfMemoryError" || got.ErrorNodeID != "12" {
			t.Fatalf("outcome = %+v", got)
		}
		if !strings.Contains(string(got.ErrorRaw), "Tried to allocate") {
			t.Fatalf("raw error was not preserved: %s", got.ErrorRaw)
		}
		if class := ClassifyFailure(got.ErrorType, got.ErrorMessage); class != ExitOOM {
			t.Fatalf("ClassifyFailure = %q, want %q", class, ExitOOM)
		}
	})

	t.Run("a queued run is not terminal", func(t *testing.T) {
		got := ParseHistoryOutcome([]byte(`{"status":{"status_str":"","completed":false,"messages":[]}}`))
		if got.Terminal() {
			t.Fatal("a run with no messages must not be treated as finished")
		}
	})

	t.Run("a missing entry is not found", func(t *testing.T) {
		if _, ok := FindHistoryEntry([]byte(`{}`), "p1"); ok {
			t.Fatal("empty history must not resolve an entry")
		}
	})

	t.Run("an inverted pair yields no duration", func(t *testing.T) {
		got := HistoryOutcome{HasStart: true, HasSuccess: true, StartMS: 200, SuccessMS: 100}
		if _, ok := got.DurationMS(); ok {
			t.Fatal("success-before-start must not produce a negative duration")
		}
	})
}

func TestClassifyFailure(t *testing.T) {
	tests := []struct {
		name    string
		excType string
		message string
		want    string
	}{
		{name: "torch oom", excType: "torch.cuda.OutOfMemoryError", message: "CUDA out of memory", want: ExitOOM},
		{name: "allocator phrasing", excType: "RuntimeError", message: "Allocation on device 0 would exceed", want: ExitOOM},
		{name: "unregistered model class", excType: "", message: "value not in list: upscale.pt not in []", want: ExitMissingModel},
		{name: "interrupt", excType: "ProcessInterrupted", message: "", want: ExitInterrupted},
		{name: "validation", excType: "", message: "Invalid prompt: required input is missing", want: ExitValidation},
		{name: "anything else", excType: "TypeError", message: "expected Tensor", want: ExitError},
		{name: "nothing at all", excType: "", message: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyFailure(tc.excType, tc.message); got != tc.want {
				t.Fatalf("ClassifyFailure(%q,%q) = %q, want %q", tc.excType, tc.message, got, tc.want)
			}
		})
	}
}

func TestBuildComparison(t *testing.T) {
	arms, err := Expand([]Var{
		{Addr: "12.virtual_vram_gb", Values: []string{"7", "10", "13"}},
	}, Cartesian)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	results := map[string]RunFacts{
		"virtual_vram_gb=7": {
			PromptID: "p3", ServerID: "s1", State: "failed", ExitClass: ExitOOM,
			ErrorType: "torch.cuda.OutOfMemoryError", ErrorMessage: "CUDA out of memory\nTried to allocate 2 GiB",
		},
		"virtual_vram_gb=10": {
			PromptID: "p1", ServerID: "s1", State: "completed", DurationMS: 90000, HasDuration: true,
		},
		// virtual_vram_gb=13 was never run: the OOM restarted the server mid-sweep.
	}

	cmp := BuildComparison(arms, results)

	if cmp.Total != 3 || len(cmp.Rows) != 3 {
		t.Fatalf("got %d rows for %d arms; every arm must be a row", len(cmp.Rows), cmp.Total)
	}
	if cmp.Passed != 1 || cmp.Failed != 1 || cmp.NotRun != 1 {
		t.Fatalf("tallies = passed %d failed %d not-run %d", cmp.Passed, cmp.Failed, cmp.NotRun)
	}

	oom := cmp.Rows[0]
	if oom.Verdict != VerdictFail {
		t.Fatalf("the OOM'd arm rendered as %q, want FAIL", oom.Verdict)
	}
	if oom.ExitClass != ExitOOM {
		t.Fatalf("the OOM'd arm lost its exit class: %+v", oom)
	}
	if oom.Duration != "n/a" {
		t.Fatalf("failed-arm duration = %q, want an explicit n/a rather than an empty cell", oom.Duration)
	}
	if !strings.Contains(oom.Note, "OutOfMemoryError") {
		t.Fatalf("the OOM'd arm lost its reason: %q", oom.Note)
	}

	notRun := cmp.Rows[2]
	if notRun.Verdict != VerdictNotRun || notRun.Note != "never submitted" {
		t.Fatalf("the unrun arm rendered as %+v, want a first-class NOT-RUN row", notRun)
	}

	pass := cmp.Rows[1]
	if pass.Verdict != VerdictPass || pass.Duration != "1m30.0s" || pass.Relative != "1.00x" {
		t.Fatalf("the passing arm rendered as %+v", pass)
	}
	if cmp.BaselineMS != 90000 {
		t.Fatalf("baseline = %d, want the fastest passing arm", cmp.BaselineMS)
	}

	rows := cmp.TableRows()
	if len(rows) != 3 {
		t.Fatalf("TableRows dropped a row: %d", len(rows))
	}
	if len(rows[0]) != len(cmp.Headers) {
		t.Fatalf("row width %d != header width %d", len(rows[0]), len(cmp.Headers))
	}
}

func TestVerdict(t *testing.T) {
	tests := []struct {
		name  string
		facts RunFacts
		found bool
		want  string
	}{
		{name: "completed", facts: RunFacts{State: "completed"}, found: true, want: VerdictPass},
		{name: "completed with node errors", facts: RunFacts{State: "completed", NodeErrors: `{"12":{}}`}, found: true, want: VerdictPartial},
		{name: "partial accept", facts: RunFacts{State: "partial-accept"}, found: true, want: VerdictPartial},
		{name: "failed", facts: RunFacts{State: "failed"}, found: true, want: VerdictFail},
		{name: "submitted", facts: RunFacts{State: "submitted"}, found: true, want: VerdictPending},
		{name: "running", facts: RunFacts{State: "running"}, found: true, want: VerdictPending},
		{name: "absent", found: false, want: VerdictNotRun},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Verdict(tc.facts, tc.found); got != tc.want {
				t.Fatalf("Verdict = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestServerIdentity(t *testing.T) {
	stats := []byte(`{"system":{"os":"nt","comfyui_version":"0.32.0","python_version":"3.12.7 (main)",
		"pytorch_version":"2.6.0+cu128","required_frontend_version":"1.30.0",
		"argv":["main.py","--listen","--reserve-vram","2"]},"devices":[{"name":"cuda:0 NVIDIA RTX 5070 Ti","index":0}]}`)
	id, err := ParseSystemStats(stats)
	if err != nil {
		t.Fatalf("ParseSystemStats: %v", err)
	}
	if id.ComfyUIVersion != "0.32.0" || id.TorchVersion != "2.6.0+cu128" || !id.ArgvKnown {
		t.Fatalf("identity = %+v", id)
	}
	if len(id.Argv) != 4 {
		t.Fatalf("argv = %#v", id.Argv)
	}

	t.Run("argv is part of the identity", func(t *testing.T) {
		other := id
		other.Argv = []string{"main.py", "--listen"}
		if id.ID() == other.ID() {
			t.Fatal("two servers with different argv must not share an id: arms span restarts with different flags")
		}
	})

	t.Run("identical identities hash identically", func(t *testing.T) {
		again, err := ParseSystemStats(stats)
		if err != nil {
			t.Fatalf("ParseSystemStats: %v", err)
		}
		if again.ID() != id.ID() {
			t.Fatal("the same server must hash to the same id")
		}
	})

	t.Run("an empty identity has no id", func(t *testing.T) {
		if got := (ServerIdentity{}).ID(); got != "" {
			t.Fatalf("ID() = %q, want empty so callers store NULL", got)
		}
	})

	t.Run("a server that reports no argv is flagged", func(t *testing.T) {
		older, err := ParseSystemStats([]byte(`{"system":{"comfyui_version":"0.32.0"}}`))
		if err != nil {
			t.Fatalf("ParseSystemStats: %v", err)
		}
		if older.ArgvKnown {
			t.Fatal("a system_stats without argv must not claim argv is known")
		}
		if older.ID() == "" {
			t.Fatal("a partially known server still has an id")
		}
	})
}

func TestAttributeDelta(t *testing.T) {
	base := Side{
		PromptID: "p1", ServerID: "s1", Argv: []string{"main.py", "--lowvram"}, ArgvKnown: true,
		DurationMS: 60000, HasDuration: true, State: "completed",
	}

	t.Run("no stored argv on the original is not attributable", func(t *testing.T) {
		before := base
		before.Argv, before.ArgvKnown = nil, false
		after := base
		after.DurationMS = 89400
		d := AttributeDelta(before, after)
		if !d.HasDelta || d.DeltaMS != 29400 {
			t.Fatalf("delta = %+v", d)
		}
		if d.Attributable {
			t.Fatal("a delta against a run with no stored argv must not be presented as attributable")
		}
		if !strings.Contains(d.Attribution, "+49% regression") {
			t.Fatalf("attribution = %q; it must name the failure mode it prevents", d.Attribution)
		}
		if int(d.PercentChange) != 49 {
			t.Fatalf("percent change = %v, want ~49", d.PercentChange)
		}
	})

	t.Run("same server and identical argv attributes to code or data", func(t *testing.T) {
		after := base
		after.PromptID = "p2"
		after.DurationMS = 61000
		d := AttributeDelta(base, after)
		if !d.Attributable || !d.SameServer {
			t.Fatalf("delta = %+v", d)
		}
		if len(d.ArgvChanges) != 0 {
			t.Fatalf("argv changes = %#v, want none", d.ArgvChanges)
		}
		if !strings.Contains(d.Attribution, "not launch flags") {
			t.Fatalf("attribution = %q", d.Attribution)
		}
	})

	t.Run("changed argv is named line by line", func(t *testing.T) {
		after := base
		after.ServerID = "s2"
		after.Argv = []string{"main.py", "--normalvram", "--reserve-vram", "2"}
		after.DurationMS = 40000
		d := AttributeDelta(base, after)
		if !d.Attributable || d.SameServer {
			t.Fatalf("delta = %+v", d)
		}
		want := []string{"- --lowvram", "+ --normalvram", "+ --reserve-vram", "+ 2"}
		if !reflect.DeepEqual(d.ArgvChanges, want) {
			t.Fatalf("argv changes = %#v, want %#v", d.ArgvChanges, want)
		}
		if d.DeltaMS != -20000 {
			t.Fatalf("delta ms = %d", d.DeltaMS)
		}
	})

	t.Run("a missing duration is a caveat, not a zero", func(t *testing.T) {
		after := base
		after.HasDuration = false
		after.DurationMS = 0
		d := AttributeDelta(base, after)
		if d.HasDelta {
			t.Fatal("a missing execution_success must not yield a delta")
		}
		if len(d.Caveats) == 0 {
			t.Fatal("the missing timing must be stated")
		}
	})

	t.Run("a cache hit is called out", func(t *testing.T) {
		after := base
		after.DurationMS = 120
		after.CacheHit = true
		d := AttributeDelta(base, after)
		joined := strings.Join(d.Caveats, " ")
		if !strings.Contains(joined, "execution cache") {
			t.Fatalf("caveats = %#v, want the cache hit named", d.Caveats)
		}
	})
}

func TestArgvChanges(t *testing.T) {
	tests := []struct {
		name           string
		before, after  []string
		want           []string
		wantEmptyDiffs bool
	}{
		{name: "identical", before: []string{"a", "b"}, after: []string{"b", "a"}, wantEmptyDiffs: true},
		{name: "added", before: []string{"a"}, after: []string{"a", "b"}, want: []string{"+ b"}},
		{name: "removed", before: []string{"a", "b"}, after: []string{"a"}, want: []string{"- b"}},
		{name: "both nil", wantEmptyDiffs: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ArgvChanges(tc.before, tc.after)
			if tc.wantEmptyDiffs {
				if len(got) != 0 {
					t.Fatalf("ArgvChanges = %#v, want none", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ArgvChanges = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		ms   int64
		want string
	}{
		{ms: 0, want: "-"},
		{ms: -5, want: "-"},
		{ms: 812, want: "812ms"},
		{ms: 45000, want: "45.0s"},
		{ms: 90000, want: "1m30.0s"},
		{ms: 1234567, want: "20m34.6s"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			if got := FormatDuration(tc.ms); got != tc.want {
				t.Fatalf("FormatDuration(%d) = %q, want %q", tc.ms, got, tc.want)
			}
		})
	}
	if got := FormatSignedDuration(-20000); got != "-20.0s" {
		t.Fatalf("FormatSignedDuration(-20000) = %q", got)
	}
	if got := FormatSignedDuration(29400); got != "+29.4s" {
		t.Fatalf("FormatSignedDuration(29400) = %q", got)
	}
	if got := FormatSignedDuration(0); got != "0ms" {
		t.Fatalf("FormatSignedDuration(0) = %q", got)
	}
}

func TestFormatUUIDv4(t *testing.T) {
	var b [16]byte
	for i := range b {
		b[i] = byte(i)
	}
	got := FormatUUIDv4(b)
	if len(got) != 36 {
		t.Fatalf("uuid = %q (len %d)", got, len(got))
	}
	if got[14] != '4' {
		t.Fatalf("uuid %q is not version 4", got)
	}
	switch got[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Fatalf("uuid %q has a bad variant nibble", got)
	}
	if strings.Count(got, "-") != 4 {
		t.Fatalf("uuid %q is not dash-grouped", got)
	}
}

func TestArmRecordRoundTrip(t *testing.T) {
	arms, err := Expand([]Var{
		{Addr: "12.virtual_vram_gb", Values: []string{"7"}},
		{Addr: "12.donor_device", Values: []string{"cuda:1"}},
	}, Cartesian)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	record := arms[0].ToRecord("abc123")
	blob, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ArmRecord
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.GraphSHA != "abc123" {
		t.Fatalf("graph sha lost: %+v", back)
	}
	restored := back.ToArm()
	if !reflect.DeepEqual(restored.Values, arms[0].Values) || restored.Label != arms[0].Label {
		t.Fatalf("round trip changed the arm: %+v vs %+v", restored, arms[0])
	}
	// The stored arm must still materialise the same graph after a restart wiped /history.
	if _, _, err := ApplyArm(testGraph(), restored); err != nil {
		t.Fatalf("ApplyArm on a restored arm: %v", err)
	}
}
