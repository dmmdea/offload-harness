package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dmmdea/local-offload/internal/config"
)

// fakeSwap serves /health and a /v1/models roster like llama-swap does.
func fakeSwap(t *testing.T, ids []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		var rows []string
		for _, id := range ids {
			rows = append(rows, fmt.Sprintf(`{"id":%q,"object":"model"}`, id))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","data":[%s]}`, strings.Join(rows, ","))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// defaultAliasIDs returns every alias config.Default() routes to.
func defaultAliasIDs() []string {
	cfg := config.Default()
	var ids []string
	for _, a := range modelAliases(cfg) {
		if a.Alias != "" {
			ids = append(ids, a.Alias)
		}
	}
	return ids
}

// TestDoctorRosterAllPresent (LO-11): with every configured alias served,
// doctor passes and prints OK per alias.
func TestDoctorRosterAllPresent(t *testing.T) {
	srv := fakeSwap(t, defaultAliasIDs())
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	var out strings.Builder
	if err := doctorRun(cfg, &out); err != nil {
		t.Fatalf("doctor must pass with a full roster: %v\n%s", err, out.String())
	}
	got := out.String()
	if strings.Contains(got, "FAIL") {
		t.Fatalf("no FAIL expected:\n%s", got)
	}
	for _, a := range modelAliases(cfg) {
		if a.Alias != "" && !strings.Contains(got, a.Alias) {
			t.Errorf("output missing alias %s", a.Alias)
		}
	}
}

// TestDoctorRosterMissingAliasFails (LO-11): a configured alias absent from the
// live roster prints FAIL for that alias and returns a non-nil error (non-zero
// exit) — the old doctor only GET /health, so a renamed llama-swap alias passed
// doctor while every real call deferred.
func TestDoctorRosterMissingAliasFails(t *testing.T) {
	cfg := config.Default()
	var served []string
	for _, id := range defaultAliasIDs() {
		if id != cfg.EscalationModel { // drop qwythos from the roster
			served = append(served, id)
		}
	}
	srv := fakeSwap(t, served)
	cfg.Endpoint = srv.URL
	var out strings.Builder
	err := doctorRun(cfg, &out)
	if err == nil {
		t.Fatalf("doctor must fail when an alias is missing\n%s", out.String())
	}
	got := out.String()
	if !strings.Contains(got, "FAIL") || !strings.Contains(got, cfg.EscalationModel) {
		t.Fatalf("expected a FAIL line naming %q:\n%s", cfg.EscalationModel, got)
	}
	// reasoning_model shares the qwythos alias, so BOTH keys must FAIL.
	if !strings.Contains(got, "escalation_model") || !strings.Contains(got, "reasoning_model") {
		t.Fatalf("both keys routing to the missing alias must FAIL:\n%s", got)
	}
}

// TestDoctorEndpointDownFails: an unreachable endpoint is a non-zero exit too
// (doctor is the CI gate for the whole local stack).
func TestDoctorEndpointDownFails(t *testing.T) {
	srv := fakeSwap(t, nil)
	url := srv.URL
	srv.Close()
	cfg := config.Default()
	cfg.Endpoint = url
	var out strings.Builder
	if err := doctorRun(cfg, &out); err == nil {
		t.Fatal("doctor must fail when the endpoint is down")
	}
	if !strings.Contains(out.String(), "DOWN") {
		t.Fatalf("expected DOWN in output:\n%s", out.String())
	}
}

// TestModelsReportUsesConfigValues (LO-11): `models` renders the CURRENT
// config — not the old hardcoded tier prose that still claimed
// gemma4-26b-a4b after the default escalation moved to qwythos.
func TestModelsReportUsesConfigValues(t *testing.T) {
	got := modelsReport(config.Default())
	for _, want := range []string{"offload-e4b", "gemma4-e2b", "qwythos", "qwen3vl-4b", "whisper-stt", "whisper-stt-hq"} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing configured value %q:\n%s", want, got)
		}
	}
	for _, stale := range []string{"gemma4-26b-a4b", "MTP", "~120 tok/s", "llama-swap on :11436"} {
		if strings.Contains(got, stale) {
			t.Errorf("report still carries hardcoded stale prose %q:\n%s", stale, got)
		}
	}
	// An overridden config must flow through verbatim.
	cfg := config.Default()
	cfg.EscalationModel = "custom-tier-x"
	if !strings.Contains(modelsReport(cfg), "custom-tier-x") {
		t.Error("report must reflect a non-default escalation_model")
	}
}
