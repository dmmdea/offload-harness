package pipeline

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// fakeChat is a minimal /v1/chat/completions response matching llamaclient's
// decode shape. Logprobs are optional (only the entry tier returns them here).
type fakeChat struct {
	content      string
	finishReason string
	promptTokens int
	logprobs     []fakeTok // per-output-token, only the value token needs Top
}

type fakeTok struct {
	token string
	top   []fakeAlt
}

type fakeAlt struct {
	token   string
	logprob float64
}

func (fc fakeChat) marshal() []byte {
	type alt struct {
		Token   string  `json:"token"`
		Logprob float64 `json:"logprob"`
	}
	type tok struct {
		Token       string  `json:"token"`
		Logprob     float64 `json:"logprob"`
		TopLogprobs []alt   `json:"top_logprobs"`
	}
	resp := map[string]any{
		"choices": []map[string]any{{
			"message":       map[string]any{"content": fc.content},
			"finish_reason": fc.finishReason,
		}},
		"usage": map[string]any{"prompt_tokens": fc.promptTokens, "completion_tokens": 8},
	}
	if fc.logprobs != nil {
		toks := make([]tok, 0, len(fc.logprobs))
		for _, t := range fc.logprobs {
			alts := make([]alt, 0, len(t.top))
			for _, a := range t.top {
				alts = append(alts, alt{Token: a.token, Logprob: a.logprob})
			}
			toks = append(toks, tok{Token: t.token, TopLogprobs: alts})
		}
		resp["choices"].([]map[string]any)[0]["logprobs"] = map[string]any{"content": toks}
	}
	b, _ := json.Marshal(resp)
	return b
}

// tokenizeDecision splits a triage answer into per-token logprobs so the
// reconstructed string is exactly `content` and the chosen "yes" token carries a
// raw class distribution between yes/no whose margin is `wantMargin` (< 0.35).
func tokenizeDecision(reason string) []fakeTok {
	// content == {"decision":"yes","reason":"<reason>"} ; split so one token is
	// exactly "yes" at the value position. Tokens just need to concatenate back.
	pYes, pNo := 0.6, 0.4 // margin = (0.6-0.4)/1.0 = 0.2  (non-zero, below 0.35)
	return []fakeTok{
		{token: `{"decision":"`},
		{token: "yes", top: []fakeAlt{
			{token: "yes", logprob: math.Log(pYes)},
			{token: "no", logprob: math.Log(pNo)},
		}},
		{token: `","reason":"` + reason + `"}`},
	}
}

// TestRunLabelsAgreementWithEntryMargin drives a real low-confidence triage
// escalation through Run: the entry tier (TriageModel) returns a valid "yes"
// whose logprob decision margin (0.2) is below the 0.35 threshold, so it defers
// escalatable; the escalation tier (Model) returns an AGREEING "yes" with no
// logprobs (margin N/A -> accepted). The test asserts the sidecar row was
// snapshotted from res.Meta (FIX 1): EscalatedAgreed == true AND Margin != 0.
func TestRunLabelsAgreementWithEntryMargin(t *testing.T) {
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
			// valid answer, low decision margin -> defers escalatable
			_, _ = w.Write(fakeChat{
				content:      `{"decision":"yes","reason":"likely"}`,
				finishReason: "stop",
				promptTokens: 100,
				logprobs:     tokenizeDecision("likely"),
			}.marshal())
		case escModel:
			// agreeing answer, NO logprobs -> margin N/A -> accepted (res.OK)
			_, _ = w.Write(fakeChat{
				content:      `{"decision":"yes","reason":"confirmed"}`,
				finishReason: "stop",
				promptTokens: 120,
			}.marshal())
		default:
			http.Error(w, "unexpected model "+body.Model, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	labelsPath := filepath.Join(t.TempDir(), "confhead-labels.jsonl")

	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.Model = escModel            // escalation/workhorse tier
	cfg.TriageModel = entryModel    // fast entry tier
	cfg.EscalationModel = ""        // chain = [entry, model]
	cfg.MaxRetries = 0              // single attempt per tier
	cfg.ConfHeadLabelsPath = labelsPath
	// keep the logprob gate on its default 0.35 so margin 0.2 escalates
	cfg.ThresholdsPath = ""
	cfg.RouterWeightsPath = ""
	cfg.TierOverridesPath = ""

	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	// nil cache + nil ledger: labelAgreement only needs ConfHeadLabelsPath.
	p := New(cfg, client, nil, nil)

	req := core.Request{
		Task:   core.TaskTriage,
		Input:  "The customer reports the invoice was charged twice and wants a refund processed today.",
		Params: map[string]any{"question": "Is this a billing issue?"},
	}

	res := p.Run(context.Background(), req)
	if !res.OK {
		t.Fatalf("expected escalation to accept, got defer: %s", res.Reason)
	}
	if res.Meta.Escalations == 0 {
		t.Fatalf("expected an escalation (Escalations>0), got %d", res.Meta.Escalations)
	}

	rows, err := ledger.ReadLabelFile(labelsPath)
	if err != nil {
		t.Fatalf("ReadLabelFile: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 sidecar label row, got %d", len(rows))
	}
	got := rows[0]

	if got.EscalatedAgreed == nil || !*got.EscalatedAgreed {
		t.Fatalf("EscalatedAgreed: want non-nil true, got %v", got.EscalatedAgreed)
	}
	// FIX 1: the row must carry the ENTRY tier's real margin (snapshotted from
	// res.Meta), not the outer meta's constant 0.
	if got.Margin == 0 {
		t.Fatalf("Margin: want non-zero entry-tier margin (FIX 1), got 0 — snapshot used the outer meta")
	}
	// sanity: it's the low margin we injected (0.2), not the escalation tier's
	if math.Abs(got.Margin-0.2) > 1e-6 {
		t.Fatalf("Margin: want ~0.2 (entry tier), got %v", got.Margin)
	}
	if got.Task != string(core.TaskTriage) {
		t.Fatalf("Task: want triage, got %q", got.Task)
	}
}
