package fleetnode

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// multiRoster is a llama-swap roster serving several models, each with its own
// aliases — the shape a composite box has (one seat per layer role). It counts
// GETs so a test can pin that health never probes per request.
func multiRoster(t *testing.T, probes *atomic.Int64, models map[string][]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if probes != nil {
			probes.Add(1)
		}
		entries := make([]string, 0, len(models))
		for id, aliases := range models {
			quoted := make([]string, 0, len(aliases))
			for _, a := range aliases {
				quoted = append(quoted, fmt.Sprintf("%q", a))
			}
			entries = append(entries, fmt.Sprintf(`{"id":%q,"object":"model","meta":{"llamaswap":{"aliases":[%s]}}}`, id, strings.Join(quoted, ",")))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","data":[%s]}`, strings.Join(entries, ","))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// compositeNodeCfg is the shipped composite box as a fleet node: the tier's
// layers (the fixture minus the three-card layer the tier no longer declares),
// the agent lane on, and a roster endpoint.
func compositeNodeCfg(endpoint string) config.Config {
	cfg := agentHealthCfg(endpoint)
	fx := config.CompositeFixture()
	cfg.TierProfile, cfg.Tiers = fx.TierProfile, fx.Tiers
	for _, l := range fx.Layers {
		if l.Name != "triple" {
			cfg.Layers = append(cfg.Layers, l)
		}
	}
	cfg.AgentModel = "agent-pool"
	return cfg
}

// TestHealthPublishesTiersAndLayerRowsOnlyOnACompositeLane: a composite node
// advertises WHAT IT IS (tiers) and WHAT IT CAN PLACE ON (layer rows with each
// layer's own admissibility verdict), because the delegator runs the same
// placement table over those rows and the display-card guards can only be read
// where the card is. It is lane-gated like every other agent field, absent on a
// plain node, and it costs no extra probe: the rows are built from the roster
// the residency refresh already fetched and the VRAM snapshot the sampler
// already holds.
func TestHealthPublishesTiersAndLayerRowsOnlyOnACompositeLane(t *testing.T) {
	var probes atomic.Int64
	roster := multiRoster(t, &probes, map[string][]string{
		"qwen3.8-27b-vllm":  {"agent-pool"},
		"qwen3.8-27b-262k":  {"qwen38-262k"},
		"gemma-4-26b-agent": nil,
		"qwen3-vl-8b":       {"ocr"},
		// qwen3-vl-32b is deliberately NOT served: a declared seat the roster
		// does not answer for must publish served=false, not disappear.
	})
	s, _ := newTestServer(t, compositeNodeCfg(roster.URL), &fakeRunner{}, authOpts(true))

	// First GET warms the cache the way production does (background refresh).
	_ = do(t, s, http.MethodGet, "/fleet/health", "", nil)
	waitForResidencyProbe(t, s)
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if m["schema_version"] != float64(1) {
		t.Fatalf("the layer rows are additive, never a version bump: %v", m["schema_version"])
	}
	tiers, _ := m["tiers"].([]any)
	if len(tiers) != 3 || tiers[0] != "blackwell-16" || tiers[2] != "blackwell-3x16" {
		t.Fatalf("tiers = %v — a composite node is a complete instance of each", m["tiers"])
	}
	rows, _ := m["layers"].([]any)
	if len(rows) != 3 {
		t.Fatalf("layers = %v, want one row per declared layer", m["layers"])
	}
	byName := map[string]map[string]any{}
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		name, _ := row["name"].(string)
		byName[name] = row
	}
	pair := byName["pair"]
	if pair == nil || pair["admissible"] != true {
		t.Fatalf("the pair layer has no guards and must publish admissible=true: %v", pair)
	}
	served := map[string]bool{}
	for _, raw := range pair["seats"].([]any) {
		seat, _ := raw.(map[string]any)
		role, _ := seat["role"].(string)
		served[role] = seat["served"] == true
		if _, present := seat["loaded"]; present {
			t.Fatalf("a node's cached health knows the roster, not load state — seat %q must publish no loaded flag: %v", role, seat)
		}
	}
	if !served["agent"] || !served["long"] {
		t.Fatalf("seats the roster answers for (by id or alias) must publish served=true: %v", served)
	}
	if served["vision"] {
		t.Fatalf("a seat the roster does not serve must publish served=false: %v", served)
	}
	display := byName["display"]
	if display == nil || display["admissible"] != false {
		t.Fatalf("the dormant display layer must publish admissible=false: %v", display)
	}

	// Cached: 20 more requests must not cost one more roster GET (two per TTL
	// cycle — rosterServes + rosterServedModels — and the rows ride those).
	for i := 0; i < 20; i++ {
		_ = do(t, s, http.MethodGet, "/fleet/health", "", nil)
	}
	if n := probes.Load(); n != 2 {
		t.Fatalf("roster GETs = %d, want exactly 2: the layer rows must never add a probe", n)
	}

	// Lane OFF on the same composite config: nothing new is published (a node
	// that will refuse a dispatch must not advertise where it could run one).
	off := compositeNodeCfg(roster.URL)
	off.FleetAgentEnabled = false
	sOff, _ := newTestServer(t, off, &fakeRunner{}, authOpts(true))
	mOff := decodeMap(t, do(t, sOff, http.MethodGet, "/fleet/health", "", nil))
	for _, k := range []string{"tiers", "layers"} {
		if _, present := mOff[k]; present {
			t.Fatalf("lane off must publish no %s: %v", k, mOff[k])
		}
	}

	// A PLAIN node with the lane on: byte-identical to before the feature.
	sPlain, _ := newTestServer(t, agentHealthCfg(roster.URL), &fakeRunner{}, authOpts(true))
	mPlain := decodeMap(t, do(t, sPlain, http.MethodGet, "/fleet/health", "", nil))
	for _, k := range []string{"tiers", "layers"} {
		if _, present := mPlain[k]; present {
			t.Fatalf("a plain node must publish no %s: %v", k, mPlain[k])
		}
	}
}

// TestHealthLayerRowsDecodeIntoTheDelegatorsNodeView pins the wire both sides
// share: what the node marshals is exactly what delegate.NodeView decodes and
// what placement.FromRows rebuilds a layer spec from. A row shape that only
// one side understands is a placement decision made on nothing.
func TestHealthLayerRowsDecodeIntoTheDelegatorsNodeView(t *testing.T) {
	roster := multiRoster(t, nil, map[string][]string{"qwen3.8-27b-vllm": {"agent-pool"}})
	s, _ := newTestServer(t, compositeNodeCfg(roster.URL), &fakeRunner{}, authOpts(true))
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	var wire struct {
		Tiers  []string         `json:"tiers"`
		Layers []map[string]any `json:"layers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatalf("health is not decodable: %v", err)
	}
	if len(wire.Layers) != 3 {
		t.Fatalf("layers = %v", wire.Layers)
	}
	for _, row := range wire.Layers {
		if _, ok := row["name"].(string); !ok {
			t.Fatalf("every row names its layer: %v", row)
		}
		if _, ok := row["admissible"].(bool); !ok {
			t.Fatalf("every row carries the node's own verdict: %v", row)
		}
		if _, ok := row["reason"].(string); !ok {
			t.Fatalf("every verdict carries its reason: %v", row)
		}
	}
}

// TestDispatchedLayerNamesTheRealSeatOnTheJobFeed: a contract dispatched at a
// LAYER runs on that layer's seat, so the node's own job feed must name it.
// Until now every agent row said `agent_seat` whatever the contract asked for,
// which is the same class of untruth the placement block exists to end — an
// operator watching /fleet/jobs during a long-context run would see the
// planner seat while the 262k twin held the cards.
func TestDispatchedLayerNamesTheRealSeatOnTheJobFeed(t *testing.T) {
	cfg := agentNodeCfg(t)
	fx := config.CompositeFixture()
	cfg.TierProfile, cfg.Tiers = fx.TierProfile, fx.Tiers
	for _, l := range fx.Layers {
		if l.Name != "triple" {
			cfg.Layers = append(cfg.Layers, l)
		}
	}
	s, _ := newTestServer(t, cfg, &fakeRunner{}, authOpts(true))
	auth := map[string]string{"Authorization": "Bearer " + cfg.FleetAuthToken}

	for _, tc := range []struct{ id, layer, want string }{
		{"agd-layer-single", `"layer":"single",`, "gemma-4-26b-agent"},
		{"agd-layer-none", "", s.agentSeat},
	} {
		body := `{"job_id":"` + tc.id + `","task_type":"agent","payload":{"schema_version":1,"goal":"g",` + tc.layer +
			`"output_schema":` + agentSchemaJSON + `}}`
		rec := do(t, s, http.MethodPost, "/fleet/dispatch", body, auth)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("dispatch %s: status %d (%s)", tc.id, rec.Code, rec.Body.String())
		}
		feed := decodeMap(t, do(t, s, http.MethodGet, "/fleet/jobs", "", auth))
		rows, _ := feed["jobs"].([]any)
		var model string
		for _, raw := range rows {
			row, _ := raw.(map[string]any)
			if row["id"] == tc.id {
				model, _ = row["model"].(string)
			}
		}
		if model != tc.want {
			t.Fatalf("job %s: feed model = %q, want %q", tc.id, model, tc.want)
		}
	}
}
