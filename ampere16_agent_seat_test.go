package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAmpere16AgentSeatIsTheMeasuredWinner pins the ampere-16 agent seat to the
// 2026-09-16 blind re-audit on the tier's reference box (Lenovo M720q, NVIDIA A2
// 16 GB at its accepted 40 W / 1200 MHz profile): Qwen3.8-27B UD-IQ3_S with the MTP
// head embedded in the same GGUF beat the incumbent Qwen3.5-4B seat 24 of 24 blind
// judgements, the Gemma 4 12B 24/24 and gpt-oss-20b 24/24, overall 9.32 against the
// 4B's 5.39 — the 4B took zero firsts and fourteen lasts. Record:
// Benchmarks and Optimizations/2026-09-16-a07-ampere16-seat-reaudit/, ADR 0047.
//
// Every flag this asserts is load-bearing and was measured, not chosen:
//
//   - the LITERAL --ctx-size 49152: the window that fits (14,410 MiB of a 15,356 MiB
//     card, INCLUDING the ~456 MiB memory-stack embedder already resident). The same
//     model one quant smaller aborted at load at 81,920 with CUDA out-of-memory, so a
//     bigger window is not available on this silicon regardless of weights.
//   - --spec-type draft-mtp: the head ships inside the GGUF (blk.64.nextn.*) and
//     drafted at 0.592 acceptance on free text. Without the flag it is dead weight.
//   - NO `agent-seat` alias: that alias belongs to the 4B/9B fallback entry, which
//     stays rendered. The lane binds here by name through config_seed.agent_model,
//     which is why this test also pins that binding.
//
// The gate this replaces is the reason it exists: the 2026-09-14 "measured tie" that
// kept a 4B on a 16 GB card was taken on a card running stock and thermally throttled
// (register A-103), at a 1,024-token step budget, with the 12B capped at 32k. A tier
// seat is only ever as good as the conditions it was measured under, so this test
// pins the winner AND the window it was measured at.
func TestAmpere16AgentSeatIsTheMeasuredWinner(t *testing.T) {
	const (
		tier = "ampere-16"
		seat = "qwen38-27b-agent"
	)
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			AgentCtxTokens int                        `json:"agent_ctx_tokens"`
			IncludeQ354B   bool                       `json:"include_qwen35_4b"`
			IncludeQ3827B  bool                       `json:"include_qwen38_27b"`
			VLLMSeat       *struct{}                  `json:"vllm_seat"`
			ConfigSeed     map[string]json.RawMessage `json:"config_seed"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	p, ok := doc.Profiles[tier]
	if !ok {
		t.Fatalf("tier %q not found — this gate went blind", tier)
	}

	if !p.IncludeQ3827B {
		t.Errorf("%s does not set include_qwen38_27b: the measured winner (ADR 0047) is %q, which beat the "+
			"previous 4B seat 24/24 on the blind instrument", tier, seat)
	}
	if p.VLLMSeat != nil {
		t.Errorf("%s still declares a vllm_seat: it existed to render the 4B as the AGENT seat, and the 4B is "+
			"no longer the agent seat (ADR 0047). The llama.cpp fallback entry carries that role now", tier)
	}
	if !p.IncludeQ354B {
		t.Errorf("%s dropped include_qwen35_4b: the smaller entry stays rendered as the FALLBACK so a box "+
			"without the 27B weights still serves an agent lane (ADR 0047)", tier)
	}
	if p.AgentCtxTokens != 49152 {
		t.Errorf("%s advertises agent_ctx_tokens %d, want 49152 — the window the seat was measured to hold on "+
			"the reference card; the fleet sizes every contract from the advertised figure",
			tier, p.AgentCtxTokens)
	}
	var gotModel string
	if rawModel, ok := p.ConfigSeed["agent_model"]; ok {
		_ = json.Unmarshal(rawModel, &gotModel)
	}
	if gotModel != seat {
		t.Errorf("%s seeds config_seed.agent_model = %q, want %q: this entry does not claim the `agent-seat` "+
			"alias on purpose, so the binding by name is the ONLY thing routing the lane to it", tier, gotModel, seat)
	}
	// A 6.3 tok/s seat starves at the built-in wall: FitFinalBudget floors the final
	// answer at 1,024 tokens when the contract's wall cannot decode it. The tier must
	// state the budget and wall it was measured under rather than inherit them.
	for key, want := range map[string]string{"agent_max_tokens": "4096", "agent_timeout_sec": "900"} {
		rawVal, ok := p.ConfigSeed[key]
		if !ok {
			t.Errorf("%s seeds no config_seed.%s: the seat decodes at ~6.3 tok/s, so an inherited budget is "+
				"silently cut by FitFinalBudget (ADR 0047, the wall section)", tier, key)
			continue
		}
		if got := strings.TrimSpace(string(rawVal)); got != want {
			t.Errorf("%s seeds config_seed.%s = %s, want %s (the value the winner was measured under)",
				tier, key, got, want)
		}
	}

	// The rendered entry must carry the two flags that make it the measured seat.
	for _, tmplName := range []string{"llama-swap.linux-cuda.yaml", "llama-swap.win-cuda.yaml"} {
		tmpl, err := os.ReadFile(filepath.Join("setup", "templates", tmplName))
		if err != nil {
			t.Fatal(err)
		}
		block := seatBlock(t, string(tmpl), seat)
		if block == "" {
			t.Errorf("%s defines no %q entry, but a tier gates it on include_qwen38_27b", tmplName, seat)
			continue
		}
		if !strings.Contains(block, "--spec-type draft-mtp") {
			t.Errorf("%s: %q lost --spec-type draft-mtp — the MTP head is embedded in the GGUF and drafted at "+
				"0.592 acceptance; without the flag the seat carries it as dead weight", tmplName, seat)
		}
		if !strings.Contains(block, "--ctx-size 49152") {
			t.Errorf("%s: %q does not serve a literal --ctx-size 49152 — that is the measured fit, and the same "+
				"model one quant smaller aborted at load at a larger window", tmplName, seat)
		}
		if strings.Contains(block, "agent-seat") {
			t.Errorf("%s: %q claims the `agent-seat` alias, which belongs to the 4B/9B fallback entry — two "+
				"entries sharing it render a config llama-swap rejects", tmplName, seat)
		}
		if !strings.Contains(block, "ttl: 300") {
			t.Errorf("%s: %q must unload at 5 idle minutes like every other seat (INV-2)", tmplName, seat)
		}
	}
}

// seatBlock returns the YAML text of one model block, or "" when absent.
func seatBlock(t *testing.T, tmpl, model string) string {
	t.Helper()
	start := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(model) + `:$`).FindStringIndex(tmpl)
	if start == nil {
		return ""
	}
	rest := tmpl[start[1]:]
	if next := regexp.MustCompile(`(?m)^  [A-Za-z0-9_.\-]+:$`).FindStringIndex(rest); next != nil {
		return rest[:next[0]]
	}
	return rest
}
