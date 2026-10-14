package backendchain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// stub probes by seat name: a seat absent from the map fails with "not stubbed".
func stub(m map[string]Verdict) Prober {
	return ProberFunc(func(_ context.Context, c Candidate) Verdict {
		if v, ok := m[c.Seat]; ok {
			return v
		}
		return Verdict{Detail: "not stubbed"}
	})
}

// TestResolveChain is the ACCEPT table of EXP-1: a failing first backend makes the chain fall to the
// second, and the report names the first one's failure.
func TestResolveChain(t *testing.T) {
	vllm := Candidate{Seat: "big-vllm", Engine: EngineVLLM, Bound: true}
	cpp := Candidate{Seat: "small-cpp", Engine: EngineLlamaCpp}
	cases := []struct {
		name       string
		chain      []Candidate
		verdicts   map[string]Verdict
		wantActive string
		wantSeat   string
		wantProbe  string
		wantChain  []string
		inReason   []string
		notReason  []string
	}{
		{
			name:       "the first backend answers and the chain stops there",
			chain:      []Candidate{vllm, cpp},
			verdicts:   map[string]Verdict{"big-vllm": {OK: true, Probe: ProbeEndpoint, Detail: "loaded and ready"}, "small-cpp": {OK: true, Probe: ProbeProcess, Detail: "idle"}},
			wantActive: EngineVLLM, wantSeat: "big-vllm", wantProbe: ProbeEndpoint,
			wantChain: []string{"vllm", "llama.cpp"},
			inReason:  []string{"loaded and ready"},
			notReason: []string{"unavailable", "fell to"},
		},
		{
			name:       "a failing first backend falls to the second and the reason names the failure",
			chain:      []Candidate{vllm, cpp},
			verdicts:   map[string]Verdict{"big-vllm": {Probe: ProbeEndpoint, Detail: "its health endpoint answered 503"}, "small-cpp": {OK: true, Probe: ProbeProcess, Detail: "not loaded; loads on first request"}},
			wantActive: EngineLlamaCpp, wantSeat: "small-cpp", wantProbe: ProbeProcess,
			wantChain: []string{"vllm", "llama.cpp"},
			inReason:  []string{"vllm seat big-vllm unavailable: its health endpoint answered 503", "fell to llama.cpp seat small-cpp", "not loaded; loads on first request"},
		},
		{
			name:       "every backend failing is none, at the deepest probe reached, naming each failure",
			chain:      []Candidate{vllm, cpp},
			verdicts:   map[string]Verdict{"big-vllm": {Probe: ProbeEndpoint, Detail: "health 503"}, "small-cpp": {Probe: ProbeProcess, Detail: "not in the roster"}},
			wantActive: EngineNone, wantSeat: "big-vllm", wantProbe: ProbeEndpoint,
			wantChain: []string{"vllm", "llama.cpp"},
			inReason:  []string{"vllm seat big-vllm unavailable: health 503", "llama.cpp seat small-cpp unavailable: not in the roster"},
		},
		{
			name:       "an empty chain is none and says there is nothing to try",
			chain:      nil,
			wantActive: EngineNone, wantProbe: ProbeProcess, wantChain: []string{},
			inReason: []string{"no backend"},
		},
		{
			name:       "a candidate that answers while the lane is bound to another seat says so",
			chain:      []Candidate{vllm, {Seat: "small-cpp", Engine: EngineLlamaCpp}},
			verdicts:   map[string]Verdict{"big-vllm": {Detail: "not in the roster"}, "small-cpp": {OK: true, Detail: "idle"}},
			wantActive: EngineLlamaCpp, wantSeat: "small-cpp", wantProbe: ProbeProcess,
			wantChain: []string{"vllm", "llama.cpp"},
			inReason:  []string{"the lane is bound to big-vllm"},
		},
		{
			name:       "an engine the config does not say reads as unknown, never as a guess",
			chain:      []Candidate{{Seat: "mystery", Bound: true}},
			verdicts:   map[string]Verdict{"mystery": {OK: true, Detail: "idle"}},
			wantActive: EngineUnknown, wantSeat: "mystery", wantProbe: ProbeProcess,
			wantChain: []string{"unknown"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Resolve(context.Background(), Lane{Name: "agent", Chain: tc.chain}, stub(tc.verdicts))
			if e.Lane != "agent" {
				t.Errorf("lane = %q", e.Lane)
			}
			if e.ActiveBackend != tc.wantActive {
				t.Errorf("active_backend = %q, want %q (reason: %s)", e.ActiveBackend, tc.wantActive, e.BackendReason)
			}
			if e.Seat != tc.wantSeat {
				t.Errorf("seat = %q, want %q", e.Seat, tc.wantSeat)
			}
			if e.BackendProbe != tc.wantProbe {
				t.Errorf("backend_probe = %q, want %q", e.BackendProbe, tc.wantProbe)
			}
			if strings.Join(e.BackendChain, ",") != strings.Join(tc.wantChain, ",") || e.BackendChain == nil {
				t.Errorf("backend_chain = %#v, want %#v (never nil: the key is always present)", e.BackendChain, tc.wantChain)
			}
			for _, s := range tc.inReason {
				if !strings.Contains(e.BackendReason, s) {
					t.Errorf("backend_reason %q must contain %q", e.BackendReason, s)
				}
			}
			for _, s := range tc.notReason {
				if strings.Contains(e.BackendReason, s) {
					t.Errorf("backend_reason %q must not contain %q", e.BackendReason, s)
				}
			}
		})
	}
}

// TestEntryJSONShape pins the four wire keys and the omitempty on lane and seat.
func TestEntryJSONShape(t *testing.T) {
	b, err := json.Marshal(Entry{ActiveBackend: EngineNone, BackendChain: []string{}, BackendProbe: ProbeProcess, BackendReason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, k := range []string{`"active_backend":"none"`, `"backend_chain":[]`, `"backend_probe":"process"`, `"backend_reason":"r"`} {
		if !strings.Contains(got, k) {
			t.Errorf("entry JSON %s lacks %s", got, k)
		}
	}
	for _, k := range []string{`"lane"`, `"seat"`} {
		if strings.Contains(got, k) {
			t.Errorf("an unset %s must be omitted: %s", k, got)
		}
	}
	rb, _ := json.Marshal(Report{})
	if string(rb) != "{}" {
		t.Errorf("an empty report must add no keys at all: %s", rb)
	}
}

func TestClassifyEngine(t *testing.T) {
	cases := []struct {
		name string
		f    SeatFacts
		want string
	}{
		{"declared vLLM wins over a wrapper command that names nothing", SeatFacts{DeclaredVLLM: true, Cmd: "/seats/x-cmd.sh"}, EngineVLLM},
		{"llama-server in the command", SeatFacts{Cmd: `C:\llama\llama-server.exe -m a.gguf --port 1`}, EngineLlamaCpp},
		{"an rkllm launcher", SeatFacts{Cmd: "/opt/rknpu/rkllm-serve.sh --port 1"}, EngineRKLLM},
		{"a command that names no known engine", SeatFacts{Cmd: "/bin/whisper-server -m w.bin"}, EngineUnknown},
		{"no command and no declaration", SeatFacts{}, EngineUnknown},
	}
	for _, tc := range cases {
		if got := ClassifyEngine(tc.f); got != tc.want {
			t.Errorf("%s: ClassifyEngine = %q, want %q", tc.name, got, tc.want)
		}
	}
}
