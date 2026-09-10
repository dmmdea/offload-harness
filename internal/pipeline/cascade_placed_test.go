// Composite tier (ADR 0039, Task 6): the mechanical cascade is placed too. On a
// composite box every text result carries `placed` naming the layer whose
// rung served it (the single layer's router by default); when the pair holds
// the cards and the display layer is awake and its guards pass, the chain runs
// the display twins instead (council R7) and says so. A plain box stamps
// nothing.
package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/placement"
)

// cascadeSeenModels is an httptest server that answers every completion with a
// valid summary and records the model ids it was asked for, in order.
func cascadeSeenModels(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, body.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fakeChat{content: `{"summary":"a summary","bullets":["one"]}`, finishReason: "stop", promptTokens: 100}.marshal())
	}))
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func cascadeTestCfg(srv *httptest.Server, cfg config.Config) config.Config {
	cfg.Endpoint = srv.URL
	cfg.Model = "gemma-4-e4b"
	cfg.TriageModel = "gemma-4-e2b"
	cfg.EscalationModel = "gemma-4-26b"
	cfg.ReasoningModel = ""
	cfg.MaxRetries = 0
	cfg.ThresholdsPath = ""
	cfg.RouterWeightsPath = ""
	cfg.TierOverridesPath = ""
	cfg.ConfHeadLabelsPath = ""
	cfg.CachePath = ""
	cfg.LedgerPath = ""
	return cfg
}

func cascadePipeline(t *testing.T, srv *httptest.Server, cfg config.Config) *Pipeline {
	t.Helper()
	return New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, nil)
}

func TestCascadeOnACompositeBoxStampsTheSingleLayerRouter(t *testing.T) {
	srv, seen := cascadeSeenModels(t)
	defer srv.Close()
	cfg := cascadeTestCfg(srv, config.CompositeFixture())
	p := cascadePipeline(t, srv, cfg)
	p.placementLive = func() placement.Live {
		return placement.Live{Seat: func(string, string) placement.SeatState { return placement.SeatState{Known: true, Loaded: false} }}
	}

	res := p.Run(context.Background(), core.Request{Task: core.TaskSummarize, Input: summaryInput})
	if !res.OK {
		t.Fatalf("defer: %s", res.Reason)
	}
	pl := res.Meta.Placed
	if pl == nil || pl.Layer != placement.LayerSingle || pl.Role != placement.RoleRouter {
		t.Fatalf("placed = %+v, want the single layer's router", pl)
	}
	if len(pl.Devices) != 1 || pl.Devices[0] != "0" {
		t.Fatalf("devices = %v, want the router seat's pin [0]", pl.Devices)
	}
	if pl.Seat != "gemma-4-e4b" || res.Meta.Model != "gemma-4-e4b" {
		t.Fatalf("placed.seat = %q / model %q, want the rung that served", pl.Seat, res.Meta.Model)
	}
	if got := seen(); len(got) != 1 || got[0] != "gemma-4-e4b" {
		t.Fatalf("served models = %v, want the workhorse rung untouched", got)
	}
	// The ledger row carries the layer (the column council R8 keeps summable).
	if row := entryFrom(core.TaskSummarize, res.Meta, false, len(summaryInput)); row.Layer != placement.LayerSingle {
		t.Fatalf("ledger layer = %q, want single", row.Layer)
	}
}

// TestCascadeSubstitutesTheDisplayTwinsWhenTheLayerIsAwake: an awake display
// layer (dormant:false), the pair seat loaded, floor and presence satisfied →
// the chain runs the layer's workhorse twin on device 1 and the result says
// layer display. The escalation rung has no twin and is dropped.
func TestCascadeSubstitutesTheDisplayTwinsWhenTheLayerIsAwake(t *testing.T) {
	srv, seen := cascadeSeenModels(t)
	defer srv.Close()
	cfg := cascadeTestCfg(srv, config.CompositeFixture())
	cfg.OperatorPresence = "away"
	for i := range cfg.Layers {
		if cfg.Layers[i].Name == placement.LayerDisplay {
			cfg.Layers[i].Dormant = false
		}
	}
	p := cascadePipeline(t, srv, cfg)
	p.placementLive = func() placement.Live {
		return placement.Live{
			Seat: func(layer, role string) placement.SeatState {
				if layer == placement.LayerPair && role == placement.RoleAgent {
					return placement.SeatState{Known: true, Loaded: true, Inflight: 3}
				}
				return placement.SeatState{Known: true}
			},
			DeviceFree:  func(string) (float64, bool) { return 15.5, true },
			DeviceIndex: func(d string) (string, bool) { return d, true },
			HostFree:    func() (float64, bool) { return 100, true },
			Presence:    func() placement.Presence { return placement.ProbePresence(cfg.PresenceMode(), cfg.OperatorIdle()) },
		}
	}

	res := p.Run(context.Background(), core.Request{Task: core.TaskSummarize, Input: summaryInput})
	if !res.OK {
		t.Fatalf("defer: %s", res.Reason)
	}
	if got := seen(); len(got) != 1 || got[0] != "gemma-4-e4b-display" {
		t.Fatalf("served models = %v, want the display layer's workhorse twin", got)
	}
	pl := res.Meta.Placed
	if pl == nil || pl.Layer != placement.LayerDisplay || pl.Seat != "gemma-4-e4b-display" || res.Meta.Model != "gemma-4-e4b-display" {
		t.Fatalf("placed = %+v / model %q", pl, res.Meta.Model)
	}
	if len(pl.Devices) != 1 || pl.Devices[0] != "1" {
		t.Fatalf("devices = %v, want the twin's pin [1]", pl.Devices)
	}
	// The chain itself: triage twin, workhorse twin, no escalation rung.
	chain := p.modelChain(core.TaskClassify, map[string]float64{}, false)
	if len(chain) != 2 || chain[0] != "gemma-4-e2b-display" || chain[1] != "gemma-4-e4b-display" {
		t.Fatalf("display chain = %v, want [gemma-4-e2b-display gemma-4-e4b-display]", chain)
	}
}

// TestCascadeLeavesADormantDisplayLayerAlone: the shipped default (dormant:
// true) never substitutes, whatever the pair is doing — the operator wakes it.
func TestCascadeLeavesADormantDisplayLayerAlone(t *testing.T) {
	srv, seen := cascadeSeenModels(t)
	defer srv.Close()
	cfg := cascadeTestCfg(srv, config.CompositeFixture())
	cfg.OperatorPresence = "away"
	p := cascadePipeline(t, srv, cfg)
	p.placementLive = func() placement.Live {
		return placement.Live{
			Seat: func(string, string) placement.SeatState {
				return placement.SeatState{Known: true, Loaded: true, Inflight: 3}
			},
			DeviceFree:  func(string) (float64, bool) { return 15.5, true },
			DeviceIndex: func(d string) (string, bool) { return d, true },
			HostFree:    func() (float64, bool) { return 100, true },
			Presence:    func() placement.Presence { return placement.ProbePresence("away", 0) },
		}
	}
	res := p.Run(context.Background(), core.Request{Task: core.TaskSummarize, Input: summaryInput})
	if !res.OK {
		t.Fatalf("defer: %s", res.Reason)
	}
	if got := seen(); len(got) != 1 || got[0] != "gemma-4-e4b" {
		t.Fatalf("served models = %v, want the single layer's rung", got)
	}
	if res.Meta.Placed == nil || res.Meta.Placed.Layer != placement.LayerSingle || res.Meta.Placed.Evicts != "agent-pool" {
		t.Fatalf("placed = %+v, want the single layer naming the pair seat it time-shares with", res.Meta.Placed)
	}
}

func TestCascadeOnAPlainBoxPublishesNoPlaced(t *testing.T) {
	srv, seen := cascadeSeenModels(t)
	defer srv.Close()
	p := cascadePipeline(t, srv, cascadeTestCfg(srv, config.Default()))
	res := p.Run(context.Background(), core.Request{Task: core.TaskSummarize, Input: summaryInput})
	if !res.OK {
		t.Fatalf("defer: %s", res.Reason)
	}
	if res.Meta.Placed != nil {
		t.Fatalf("a plain box must publish no placed block, got %+v", res.Meta.Placed)
	}
	if got := seen(); len(got) != 1 || got[0] != "gemma-4-e4b" {
		t.Fatalf("served models = %v", got)
	}
	raw, _ := json.Marshal(res.Meta)
	if json.Valid(raw) && containsKey(raw, "placed") {
		t.Fatalf("meta must not carry a placed key on a plain box: %s", raw)
	}
}

func containsKey(raw []byte, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
