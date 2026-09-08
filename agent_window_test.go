package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// `agent_ctx_tokens` is what the harness ADVERTISES: the fleet sizes every delegation
// contract from it. If it understates what the seat actually serves, the node quietly
// works at a fraction of its window and nothing ever errors — the failure mode the
// delegation rules call out by name ("size contracts from the live ceiling, never from
// a written figure", after guidance drifted to a quarter of the real window and work
// that fits trivially was declined for weeks).
//
// blackwell-8 shipped exactly that: its rendered agent seat serves `--ctx-size 32768`
// — a literal in the template, backed by its own fit measurement on the RTX 5060
// (6,344 MiB at 16K, 6,696 MiB at 32K; Qwen3.5's Gated-DeltaNet hybrid KV barely grows
// with context) — while the tier advertised 16,384. Every contract to such a node was
// sized to half the window the seat was already serving.
//
// The rule this asserts: whatever the agent seat serves, the tier advertises.
//   - a seat with a LITERAL --ctx-size  -> agent_ctx_tokens must equal that literal;
//   - a seat carrying __CTX__           -> it serves the tier's window, so
//     agent_ctx_tokens must equal ctx_size.
func TestAgentWindowMatchesWhatTheAgentSeatServes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			CtxSize        int  `json:"ctx_size"`
			AgentCtxTokens int  `json:"agent_ctx_tokens"`
			IncludeQ354B   bool `json:"include_qwen35_4b"`
			IncludeQ359B   bool `json:"include_qwen35_9b"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}

	// The agent seats live in the Windows CUDA template; both claim the `agent-seat`
	// alias, which is how the harness finds the lane.
	tmplRaw, err := os.ReadFile(filepath.Join("setup", "templates", "llama-swap.win-cuda.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	seatCtx := map[string]string{
		"qwen3.5-4b-agent": ctxExprFor(t, string(tmplRaw), "qwen3.5-4b-agent"),
		"qwen3.5-9b-agent": ctxExprFor(t, string(tmplRaw), "qwen3.5-9b-agent"),
	}

	checked := 0
	for tier, p := range doc.Profiles {
		seat := ""
		switch {
		case p.IncludeQ359B:
			seat = "qwen3.5-9b-agent"
		case p.IncludeQ354B:
			seat = "qwen3.5-4b-agent"
		default:
			continue
		}
		checked++
		expr := seatCtx[seat]
		if expr == "__CTX__" {
			if p.AgentCtxTokens != p.CtxSize {
				t.Errorf("tier %s: agent seat %q serves the tier window (__CTX__ = %d) but the tier advertises "+
					"agent_ctx_tokens %d — contracts are sized from the advertised figure",
					tier, seat, p.CtxSize, p.AgentCtxTokens)
			}
			continue
		}
		n, err := strconv.Atoi(expr)
		if err != nil {
			t.Errorf("tier %s: cannot read seat %q ctx expression %q", tier, seat, expr)
			continue
		}
		if p.AgentCtxTokens != n {
			t.Errorf("tier %s: agent seat %q serves --ctx-size %d but the tier advertises agent_ctx_tokens %d. "+
				"The fleet sizes every delegation contract from the advertised figure, so the node works at "+
				"%d/%d of the window it is already serving and nothing errors",
				tier, seat, n, p.AgentCtxTokens, p.AgentCtxTokens, n)
		}
	}
	if checked == 0 {
		t.Fatal("no tier declares an agent seat — this gate went blind")
	}
	t.Logf("checked %d tiers with a declared agent seat", checked)
}

// ctxExprFor returns the --ctx-size argument of a model block: a literal, or __CTX__.
func ctxExprFor(t *testing.T, tmpl, model string) string {
	t.Helper()
	start := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(model) + `:$`).FindStringIndex(tmpl)
	if start == nil {
		t.Fatalf("template declares no %q model block — this gate went blind", model)
	}
	seg := tmpl[start[1]:]
	if nxt := regexp.MustCompile(`(?m)^  [A-Za-z0-9._-]+:$`).FindStringIndex(seg); nxt != nil {
		seg = seg[:nxt[0]]
	}
	m := regexp.MustCompile(`--ctx-size (\S+)`).FindStringSubmatch(seg)
	if m == nil {
		t.Fatalf("model %q declares no --ctx-size — this gate went blind", model)
	}
	return strings.TrimSpace(m[1])
}
