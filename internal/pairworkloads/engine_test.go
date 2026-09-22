package pairworkloads

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/swapclient"

	"llamaswap-pp-cli/pkg/llamaswap"
)

// TestLocalEngineResolvesVLLMAlias pins the card label for an alias-bound
// vLLM seat: the Qube's agent seat is `agent-pool`, an alias of the declared
// `qwen3.8-27b-vllm-3card`, and PAIR showed its jobs as "llamacpp" because
// the label was read off the name alone.
func TestLocalEngineResolvesVLLMAlias(t *testing.T) {
	fetches := 0
	var fetchErr error
	e := New(Config{Enabled: true, VLLMSeats: []string{"qwen3.8-27b-vllm-3card"}, SwapEndpoint: "http://127.0.0.1:11436"})
	e.fetchRoster = func(context.Context, string, time.Duration) (swapclient.Roster, error) {
		fetches++
		if fetchErr != nil {
			return swapclient.Roster{}, fetchErr
		}
		return swapclient.NewRoster([]llamaswap.Model{
			{ID: "qwen3.8-27b-vllm-3card", Aliases: []string{"agent-pool", "agent-pool-3card"}},
			{ID: "qwen3.5-9b-agent"},
		}), nil
	}

	cases := map[string]string{
		"agent-pool":             "vllm",
		"AGENT-POOL-3CARD":       "vllm",
		"qwen3.8-27b-vllm-3card": "vllm",     // declared id, no roster read
		"qwen3.5-9b-agent":       "llamacpp", // served, not declared
		"unknown-seat":           "llamacpp",
	}
	for seat, want := range cases {
		if got := e.LocalEngine("agent_delegate", seat); got != want {
			t.Errorf("LocalEngine(%q) = %q, want %q", seat, got, want)
		}
	}
	before := fetches
	e.LocalEngine("agent_delegate", "agent-pool")
	if fetches != before {
		t.Errorf("a cached seat read the roster again (%d -> %d)", before, fetches)
	}

	// A seat the name already labels is never looked up.
	if got := e.LocalEngine("generate_image", "flux"); got != "comfyui" {
		t.Errorf("comfyui task = %q", got)
	}

	// An unreadable roster keeps the name-based answer rather than guessing.
	fetchErr = errors.New("connection refused")
	fresh := New(Config{Enabled: true, VLLMSeats: []string{"qwen3.8-27b-vllm-3card"}, SwapEndpoint: "http://127.0.0.1:11436"})
	fresh.fetchRoster = e.fetchRoster
	if got := fresh.LocalEngine("agent_delegate", "agent-pool"); got != "llamacpp" {
		t.Errorf("roster down: %q, want the name-based llamacpp", got)
	}

	// A box that declares no vLLM seat never reads the roster.
	none := New(Config{Enabled: true, SwapEndpoint: "http://127.0.0.1:11436"})
	none.fetchRoster = func(context.Context, string, time.Duration) (swapclient.Roster, error) {
		t.Fatal("roster read on a box with no vllm_seats")
		return swapclient.Roster{}, nil
	}
	if got := none.LocalEngine("agent_delegate", "agent-pool"); got != "llamacpp" {
		t.Errorf("no vllm_seats: %q", got)
	}
}

// TestFromLedgerLabelsAliasedVLLMSeat covers the ledger path (cascade and
// other non-delegation tool calls) through the same resolution.
func TestFromLedgerLabelsAliasedVLLMSeat(t *testing.T) {
	e := New(Config{Enabled: true, VLLMSeats: []string{"qwen3.8-27b-vllm"}, SwapEndpoint: "http://x"})
	e.fetchRoster = func(context.Context, string, time.Duration) (swapclient.Roster, error) {
		return swapclient.NewRoster([]llamaswap.Model{{ID: "qwen3.8-27b-vllm", Aliases: []string{"agent-pool-2card"}}}), nil
	}
	ev := e.FromLedger(ledger.Entry{Task: "summarize", ModelTier: "agent-pool-2card", TS: 1})
	if ev.Engine != "vllm" {
		t.Fatalf("engine = %q, want vllm", ev.Engine)
	}
}
