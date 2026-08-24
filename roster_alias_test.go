package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/acceptance"
	"github.com/dmmdea/offload-harness/internal/config"
)

// fakeSwapAliasOnly serves a roster in llama-swap's REAL shape: a CANONICAL model
// id in data[].id that the config never mentions, with the configured binding
// published only under meta.llamaswap.aliases.
//
// This is not a synthetic case. Every seat on the reference deployment is bound
// this way — the config says `offload-e4b` and `gemma4-26b-a4b`, the roster says
// `gemma-4-e4b` and `gemma-4-26b` — so an id-only reader reported EVERY correctly
// served alias as MISSING, and doctor, acceptance and report all exited non-zero
// on a completely healthy box.
func fakeSwapAliasOnly(t *testing.T, aliases []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		var rows []string
		for i, a := range aliases {
			rows = append(rows, fmt.Sprintf(
				`{"id":"canonical-seat-%d","object":"model","meta":{"llamaswap":{"aliases":[%q]}}}`, i, a))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","data":[%s]}`, strings.Join(rows, ","))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestDoctorMatchesAliasOnlySeats: doctor must pass when every configured binding
// is served as an alias and NOTHING matches by canonical id.
func TestDoctorMatchesAliasOnlySeats(t *testing.T) {
	cfg := config.Default()
	srv := fakeSwapAliasOnly(t, defaultAliasIDs())
	cfg.Endpoint = srv.URL
	var out strings.Builder
	if err := doctorRun(cfg, nil, &out); err != nil {
		t.Fatalf("an alias-only roster must pass doctor: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "FAIL") {
		t.Fatalf("no FAIL expected on an alias-only roster:\n%s", out.String())
	}
}

// TestDoctorStillFailsAGenuinelyAbsentAlias: the alias fix must not turn the gate
// into a no-op — a binding served under neither an id nor an alias still FAILs.
func TestDoctorStillFailsAGenuinelyAbsentAlias(t *testing.T) {
	cfg := config.Default()
	var served []string
	for _, id := range defaultAliasIDs() {
		if id != cfg.EscalationModel {
			served = append(served, id)
		}
	}
	srv := fakeSwapAliasOnly(t, served)
	cfg.Endpoint = srv.URL
	var out strings.Builder
	if err := doctorRun(cfg, nil, &out); err == nil {
		t.Fatalf("an alias absent from ids AND aliases must still fail doctor\n%s", out.String())
	}
	if !strings.Contains(out.String(), "FAIL") {
		t.Fatalf("expected a FAIL line:\n%s", out.String())
	}
}

// TestAcceptanceMatchesAliasOnlySeats: the same roster, through the acceptance
// gate that decides whether the fleet may hand this node work.
func TestAcceptanceMatchesAliasOnlySeats(t *testing.T) {
	cfg := config.Default()
	srv := fakeSwapAliasOnly(t, defaultAliasIDs())
	cfg.Endpoint = srv.URL
	got := aliasCheck2(context.Background(), cfg)
	if got.Status != acceptance.Pass {
		t.Fatalf("acceptance = %s (%s), want PASS on an alias-only roster", got.Status, got.Detail)
	}
}

// TestReportMatchesAliasOnlySeats: and through the report a collaborator reads,
// where the old reader printed **MISSING** beside every healthy seat.
func TestReportMatchesAliasOnlySeats(t *testing.T) {
	cfg := config.Default()
	srv := fakeSwapAliasOnly(t, defaultAliasIDs())
	cfg.Endpoint = srv.URL
	in := gatherReport(cfg, config.Source{}, nil, time.Now())
	if in.Health != "OK" {
		t.Fatalf("health = %q, want OK", in.Health)
	}
	if len(in.Aliases) == 0 {
		t.Fatal("no alias verdicts were produced")
	}
	for _, a := range in.Aliases {
		if a.Alias != "" && a.State != "OK" {
			t.Errorf("%s=%s reported %s, want OK on an alias-only roster", a.Key, a.Alias, a.State)
		}
	}
}

// TestModelRoutesAreOneTable: doctor's gate and offload_status's roster used to be
// two hardcoded lists that had drifted 8 keys to 10. They now read one table, so
// this asserts the table itself: every route carries both labels, the diffed set
// is exactly the bindings that gate a node, and embed is reported but not gated
// (it resolves to a non-empty fallback, so gating it would fail nodes with no
// embed seat). ocr_model JOINED the gated set in 0.88.0: the `ocr` media-seat
// kind makes it tier-seeded like vision_model/stt_model, the gate reads the
// CONFIGURED value (not the vision fallback), and every consumer skips an empty
// alias — so a tier with no ocr seat is untouched while a declared one is
// closure-checked (the 0.87.0 "reported, not gated" fear assumed the fallback
// was what gated).
func TestModelRoutesAreOneTable(t *testing.T) {
	routes := config.Default().ModelRoutes()
	if len(routes) != 10 {
		t.Fatalf("ModelRoutes returned %d routes, want the 10-key superset", len(routes))
	}
	seenKey, seenStatus := map[string]bool{}, map[string]bool{}
	var diffed []string
	for _, r := range routes {
		if r.Key == "" || r.StatusKey == "" {
			t.Errorf("route %+v is missing a label", r)
		}
		if seenKey[r.Key] || seenStatus[r.StatusKey] {
			t.Errorf("duplicate route label: %+v", r)
		}
		seenKey[r.Key], seenStatus[r.StatusKey] = true, true
		if r.Diffed {
			diffed = append(diffed, r.Key)
		}
	}
	want := []string{"model", "agent_model", "triage_model", "escalation_model",
		"reasoning_model", "vision_model", "ocr_model", "stt_model", "stt_model_hq"}
	if strings.Join(diffed, ",") != strings.Join(want, ",") {
		t.Errorf("diffed set = %v, want %v", diffed, want)
	}
	for _, r := range routes {
		if r.Key == "embed_model" {
			if r.Diffed {
				t.Errorf("%s must be reported, not gated", r.Key)
			}
		}
	}
}

// TestModelRoutesEffectiveAppliesFallbacks: Configured is what the operator wrote
// (an unset binding is an honest "this task defers"); Effective is what will
// actually run. offload_status publishes Effective, the doctor gate checks
// Configured, and conflating them is what made the two lists drift.
func TestModelRoutesEffectiveAppliesFallbacks(t *testing.T) {
	cfg := config.Config{Model: "workhorse", VisionModel: "vis"}
	byKey := map[string]config.ModelRoute{}
	for _, r := range cfg.ModelRoutes() {
		byKey[r.Key] = r
	}
	if got := byKey["agent_model"]; got.Configured != "" || got.Effective != "workhorse" {
		t.Errorf("agent_model = %+v, want Configured=\"\" Effective=workhorse", got)
	}
	if got := byKey["ocr_model"]; got.Configured != "" || got.Effective != "vis" {
		t.Errorf("ocr_model = %+v, want Configured=\"\" Effective=vis", got)
	}
	if got := byKey["embed_model"]; got.Effective == "" {
		t.Errorf("embed_model must always resolve to a non-empty embedder, got %+v", got)
	}
	// An explicit binding wins over every fallback.
	cfg.OCRModel, cfg.AgentModel = "ocr-seat", "agent-seat"
	for _, r := range cfg.ModelRoutes() {
		switch r.Key {
		case "ocr_model":
			if r.Effective != "ocr-seat" {
				t.Errorf("explicit ocr_model must win, got %q", r.Effective)
			}
		case "agent_model":
			if r.Effective != "agent-seat" {
				t.Errorf("explicit agent_model must win, got %q", r.Effective)
			}
		}
	}
}

// jsonRoundTrip guards that the table drives a plain string map, so the MCP
// status payload keeps the shape its consumers parse.
func TestModelRouteStatusKeysAreStable(t *testing.T) {
	roster := map[string]any{}
	for _, r := range config.Default().ModelRoutes() {
		roster[r.StatusKey] = r.Effective
	}
	b, err := json.Marshal(roster)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"workhorse", "agent", "triage", "escalation", "reasoning",
		"vision", "ocr", "stt", "stt_hq", "embed"} {
		if !strings.Contains(string(b), `"`+key+`"`) {
			t.Errorf("offload_status roster lost the %q key: %s", key, b)
		}
	}
}
