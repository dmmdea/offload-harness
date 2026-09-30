package pipeline

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// coldRescueFake models a delegator whose agent seat was idle-unloaded: /running
// lists nothing until the warm request has been made, the passthrough answers only
// after coldLoad, and a completion that reaches a seat nobody warmed pays that
// load itself, exactly as llama-swap loads on the first request that reaches it.
func coldRescueFake(coldLoad time.Duration) *agentFake {
	var fake *agentFake
	fake = &agentFake{
		rosterIDs: []string{agentTestSeat},
		running: func(int64) string {
			if fake.upstreamCNT.Load() == 0 {
				return `{"running":[]}`
			}
			return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"y"}]}`
		},
		upstreamModels: func(int64) string {
			time.Sleep(coldLoad)
			return `{"object":"list","data":[{"id":"` + agentTestSeat + `"}]}`
		},
		repackStream: func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
			if fake.upstreamCNT.Load() == 0 {
				select { // the load this completion has to pay itself
				case <-time.After(coldLoad):
				case <-r.Context().Done():
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"answer\":\"42\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":7}}`))
		},
	}
	return fake
}

// The delegator's seat unloads after five minutes idle, so the rescue routinely
// finds it cold (a spread that dealt the local slot elsewhere, a remote route).
// A cold load is minutes on a vLLM seat, longer than the whole re-pack allowance:
// the completion was cut by its own load and left a loaded seat nobody used, so
// PR-4's gate could not hold. The rescue warms the seat first, on the box's
// admission budget and outside the allowance, exactly as a run's admission does
// (D-64).
func TestRescueRepackWarmsAnAbsentSeatBeforeItsCompletion(t *testing.T) {
	defer compressLiveness(t, 300*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressRepackBound(t, 300*time.Millisecond)()
	fake := coldRescueFake(900 * time.Millisecond) // the load outlasts the compressed allowance three times over
	srv := fake.server(t)
	defer srv.Close()

	got, err := admissionTestPipeline(t, srv.URL, 30).RescueRepack(context.Background(), testContract(), "The answer is 42.", 0)
	if err != nil || !strings.Contains(string(got.Structured), `"answer":"42"`) {
		t.Fatalf("rescued = %+v err = %v, want the object: a cold seat is warmed first, not charged to the completion's allowance", got, err)
	}
	if n := fake.upstreamCNT.Load(); n < 1 {
		t.Fatalf("warm requests = %d, want the seat warmed before the completion", n)
	}
	if !strings.Contains(got.How, "cold load") {
		t.Fatalf("how = %q, want the warm-up named in the note the caller reads", got.How)
	}
}

// A resident seat costs one /running read and nothing else: no warm request, no
// note.
func TestRescueRepackDoesNotWarmAResidentSeat(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		repack:    func(int64) string { return `{"answer":"42"}` },
		running:   func(int64) string { return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"y"}]}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	got, err := admissionTestPipeline(t, srv.URL, 30).RescueRepack(context.Background(), testContract(), "The answer is 42.", 0)
	if err != nil || !strings.Contains(string(got.Structured), `"answer":"42"`) {
		t.Fatalf("rescued = %+v err = %v", got, err)
	}
	if n := fake.upstreamCNT.Load(); n != 0 {
		t.Fatalf("warm requests = %d, want none for a resident seat", n)
	}
	if strings.Contains(got.How, "cold load") {
		t.Fatalf("how = %q, want no warm-up in the note", got.How)
	}
}

// agent_admission_wait_sec -1 turns the admission gate off, warm-up included: the
// rescue honours it.
func TestRescueRepackHonoursTheAdmissionSwitch(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		repack:    func(int64) string { return `{"answer":"42"}` },
		running:   func(int64) string { return `{"running":[]}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	got, err := admissionTestPipeline(t, srv.URL, -1).RescueRepack(context.Background(), testContract(), "The answer is 42.", 0)
	if err != nil || !strings.Contains(string(got.Structured), `"answer":"42"`) {
		t.Fatalf("rescued = %+v err = %v", got, err)
	}
	if n := fake.upstreamCNT.Load(); n != 0 {
		t.Fatalf("warm requests = %d, want none with the admission gate off", n)
	}
}

// The rescue's allowance is sized from this seat's rate, and the rate comes from a
// store that may be unreadable or a state root that is refused. Falling back to the
// configured rate is right; doing it without a word is what the project's own
// admission code calls silent fail-open, and it left the rescue's allowance
// unexplained.
func TestRescueTokSSaysWhyItFellBackToTheConfiguredRate(t *testing.T) {
	corrupt := t.TempDir()
	root, err := gpulease.ResolveStateRoot(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	path := seatrate.Path(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json {"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ stateDir, want string }{
		"a corrupt store":      {corrupt, "is not readable"},
		"a refused state root": {filepath.Join(t.TempDir(), "OneDrive", "state"), "cloud-sync root"},
	} {
		t.Run(name, func(t *testing.T) {
			p := New(config.Config{StateDir: tc.stateDir, AgentSeatTokS: 55}, nil, nil, nil)
			var logged strings.Builder
			log.SetOutput(&logged)
			defer log.SetOutput(os.Stderr)
			if got := p.rescueTokS(agentTestSeat); got != 55 {
				t.Fatalf("rescueTokS = %v, want the configured 55", got)
			}
			if !strings.Contains(logged.String(), tc.want) || !strings.Contains(logged.String(), "configured rate") {
				t.Fatalf("log = %q, want %q and the fallback named", logged.String(), tc.want)
			}
		})
	}
}
