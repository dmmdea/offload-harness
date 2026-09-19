package pipeline

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// Register D-127: a margin escalation was never written to the ledger —
// confidenceGate's defer returned before p.record, so esc_source counted 4 rows
// in 9,217 while the 43 real firings lived only in the labels sidecar. The
// escalating attempt is a call the seat answered: it records its own row (its
// tier, its margin, its esc_source) before the defer, and the successor tier's
// row follows as before.
func TestMarginEscalationRecordsTheEscalatingAttempt(t *testing.T) {
	const entryModel = "fake-e2b"
	const escModel = "fake-e4b"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch body.Model {
		case entryModel:
			_, _ = w.Write(fakeChat{content: `{"decision":"yes","reason":"likely"}`, finishReason: "stop", promptTokens: 100, logprobs: tokenizeDecision("likely")}.marshal())
		case escModel:
			_, _ = w.Write(fakeChat{content: `{"decision":"yes","reason":"confirmed"}`, finishReason: "stop", promptTokens: 120}.marshal())
		default:
			http.Error(w, "unexpected model "+body.Model, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.Model = escModel
	cfg.TriageModel = entryModel
	cfg.EscalationModel = ""
	cfg.MaxRetries = 0
	cfg.ConfHeadLabelsPath = filepath.Join(dir, "confhead-labels.jsonl")
	cfg.ThresholdsPath, cfg.RouterWeightsPath, cfg.TierOverridesPath = "", "", ""
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	cfg.LedgerPath = ledgerPath

	led, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	p := New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, nil)
	p.led = led
	defer func() { p.led = nil; _ = led.Close() }()

	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskTriage,
		Input:  "The customer reports the invoice was charged twice and wants a refund processed today.",
		Params: map[string]any{"question": "Is this a billing issue?"},
	})
	if !res.OK {
		t.Fatalf("cascade must succeed on the escalation tier: %+v", res)
	}
	_ = led.Close()

	var rows []ledger.Entry
	f, err := os.Open(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e ledger.Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			rows = append(rows, e)
		}
	}
	var escalating, successor *ledger.Entry
	for i := range rows {
		switch rows[i].ModelTier {
		case entryModel:
			escalating = &rows[i]
		case escModel:
			successor = &rows[i]
		}
	}
	if successor == nil {
		t.Fatalf("the successor tier's row is missing: %+v", rows)
	}
	if escalating == nil {
		t.Fatalf("the escalating attempt must write its own ledger row (D-127); rows: %+v", rows)
	}
	if escalating.EscSource != string(core.EscMargin) {
		t.Fatalf("escalating row esc_source = %q, want %q", escalating.EscSource, core.EscMargin)
	}
	if escalating.Margin < 0.19 || escalating.Margin > 0.21 {
		t.Fatalf("escalating row margin = %v, want the entry tier's 0.2", escalating.Margin)
	}
	if escalating.Deferred {
		t.Fatalf("the escalating attempt answered; it is not a deferred row: %+v", *escalating)
	}
	if n := len(rows); n != 2 {
		t.Fatalf("exactly one row per attempt, got %d: %+v", n, rows)
	}
}
