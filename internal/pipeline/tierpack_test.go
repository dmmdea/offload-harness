package pipeline

// tierpack_test.go — TO-3: tier-aware repacking at the escalation boundary.
// Hermetic: one fat fake serves /upstream/{model}/props, /upstream/{model}/
// tokenize (fixed 4-byte-chunk tokenizer, pieces as byte arrays so rune splits
// survive JSON), and the chat path, capturing the last user prompt per model.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/cache"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/tokclient"
)

const chunkTokBytes = 4 // the fake tokenizer's fixed bytes-per-token

type repackFake struct {
	t  *testing.T
	mu sync.Mutex
	// nCtx: per-model /props answer; a model absent from the map 404s.
	nCtx map[string]int
	// answers: per-model chat content.
	answers map[string]string
	// prompts: per-model captured USER message contents, in call order.
	prompts   map[string][]string
	propsHits int
	chatHits  int
}

func (f *repackFake) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/props"):
			model := strings.TrimSuffix(strings.TrimPrefix(path, "/upstream/"), "/props")
			f.mu.Lock()
			f.propsHits++
			n, ok := f.nCtx[model]
			f.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"default_generation_settings":{"n_ctx":%d}}`, n)
		case strings.HasSuffix(path, "/tokenize"):
			var req struct {
				Content    string `json:"content"`
				WithPieces bool   `json:"with_pieces"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.Header().Set("Content-Type", "application/json")
			if !req.WithPieces {
				n := (len(req.Content) + chunkTokBytes - 1) / chunkTokBytes
				ids := make([]string, n)
				for i := range ids {
					ids[i] = "1"
				}
				fmt.Fprintf(w, `{"tokens":[%s]}`, strings.Join(ids, ","))
				return
			}
			// Pieces as BYTE ARRAYS: a 4-byte chunk can split a rune, and a
			// JSON string would mangle invalid UTF-8 — byte arrays carry exact
			// lengths the way llama.cpp's byte-fallback pieces do.
			var sb strings.Builder
			sb.WriteString(`{"tokens":[`)
			for i := 0; i < len(req.Content); i += chunkTokBytes {
				end := i + chunkTokBytes
				if end > len(req.Content) {
					end = len(req.Content)
				}
				if i > 0 {
					sb.WriteString(",")
				}
				sb.WriteString(`{"piece":[`)
				for j := i; j < end; j++ {
					if j > i {
						sb.WriteString(",")
					}
					fmt.Fprintf(&sb, "%d", req.Content[j])
				}
				sb.WriteString(`]}`)
			}
			sb.WriteString(`]}`)
			_, _ = w.Write([]byte(sb.String()))
		default: // chat completions
			var body struct {
				Model    string `json:"model"`
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			user := ""
			for _, m := range body.Messages {
				if m.Role == "user" {
					user = m.Content
				}
			}
			f.mu.Lock()
			f.chatHits++
			if f.prompts == nil {
				f.prompts = map[string][]string{}
			}
			f.prompts[body.Model] = append(f.prompts[body.Model], user)
			content, ok := f.answers[body.Model]
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if !ok {
				http.Error(w, "unexpected model "+body.Model, http.StatusBadRequest)
				return
			}
			_, _ = w.Write(fakeChat{content: content, finishReason: "stop", promptTokens: 50}.marshal())
		}
	}))
}

func (f *repackFake) lastPrompt(model string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ps := f.prompts[model]
	if len(ps) == 0 {
		return ""
	}
	return ps[len(ps)-1]
}

func repackCfg(srv *httptest.Server, small, big string, maxInputChars int) config.Config {
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.TriageModel = small
	cfg.Model = small
	cfg.EscalationModel = big
	cfg.ReasoningModel = ""
	cfg.MaxRetries = 0
	cfg.MaxInputChars = maxInputChars
	cfg.ThresholdsPath = ""
	cfg.RouterWeightsPath = ""
	cfg.TierOverridesPath = ""
	cfg.ConfHeadLabelsPath = ""
	cfg.CachePath = ""
	cfg.LedgerPath = ""
	return cfg
}

// repackInput builds an input whose middle carries a needle the ENTRY trim
// (small MaxInputChars) provably discards.
func repackInput(needle string, size int) string {
	half := strings.Repeat("a", size/2)
	return half + " " + needle + " " + strings.Repeat("b", size/2)
}

// The escalated tier must see the ORIGINAL source when its window fits it —
// not the entry tier's cut (TO-3's core promise).
func TestRunEscalationRepacksFromOriginal(t *testing.T) {
	const small, big, needle = "tier-small", "tier-big", "NEEDLE-ONLY-THE-BIG-TIER-SEES"
	f := &repackFake{t: t,
		nCtx: map[string]int{big: 8192},
		answers: map[string]string{
			small: `{"label":"billing","confidence":0.30}`, // below the 0.88 floor -> escalatable defer
			big:   `{"label":"billing","confidence":0.99}`,
		}}
	srv := f.server()
	defer srv.Close()

	cfg := repackCfg(srv, small, big, 600)
	p := New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, nil)
	req := core.Request{Task: core.TaskClassify, Input: repackInput(needle, 4000),
		Params: map[string]any{"labels": []string{"billing", "technical"}}}
	res := p.Run(context.Background(), req)
	if !res.OK {
		t.Fatalf("expected the escalation tier to answer, got defer: %s", res.Reason)
	}
	if got := f.lastPrompt(small); strings.Contains(got, needle) {
		t.Fatal("fixture invalid: the entry trim kept the needle — the test cannot distinguish the tiers")
	}
	if got := f.lastPrompt(big); !strings.Contains(got, needle) {
		t.Fatalf("the escalated tier did NOT re-read the original source — its prompt lacks the needle (len=%d)", len(f.lastPrompt(big)))
	}
	if res.Meta.TierPack != "token-exact (full source)" {
		t.Fatalf("TierPack = %q, want token-exact (full source)", res.Meta.TierPack)
	}
	if res.Meta.Escalations == 0 {
		t.Fatal("fixture invalid: the result did not come from an escalated tier")
	}
}

// When even the escalation window cannot hold the original, the repack cuts
// token-exact (head+tail+marker) — never inherits the entry cut, never sends
// the over-budget whole.
func TestRunEscalationCutsTokenExactWhenOver(t *testing.T) {
	const small, big = "tier-small", "tier-big"
	f := &repackFake{t: t,
		// 4-byte tokens: an 8000-char input is ~2000 tokens; scaffold adds a
		// few hundred. n_ctx 1300 - 64 (classify budget) - 128 reserve ≈ 1108
		// allowance -> over -> cut, with input allowance comfortably >= 256.
		nCtx: map[string]int{big: 1300},
		answers: map[string]string{
			small: `{"label":"billing","confidence":0.30}`,
			big:   `{"label":"billing","confidence":0.99}`,
		}}
	srv := f.server()
	defer srv.Close()

	cfg := repackCfg(srv, small, big, 600)
	p := New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, nil)
	req := core.Request{Task: core.TaskClassify, Input: repackInput("MIDDLE", 8000),
		Params: map[string]any{"labels": []string{"billing", "technical"}}}
	res := p.Run(context.Background(), req)
	if !res.OK {
		t.Fatalf("expected the escalation tier to answer, got defer: %s", res.Reason)
	}
	bigPrompt := f.lastPrompt(big)
	if !strings.Contains(bigPrompt, repackMarker) {
		t.Fatal("the over-window repack must carry the elision marker (head+tail cut)")
	}
	if !strings.HasPrefix(res.Meta.TierPack, "token-exact (cut ") {
		t.Fatalf("TierPack = %q, want a token-exact cut disposition", res.Meta.TierPack)
	}
	// The cut view must still be LARGER than the entry view — otherwise the
	// repack bought nothing.
	if len(bigPrompt) <= len(f.lastPrompt(small)) {
		t.Fatalf("escalated prompt (%d bytes) is not larger than the entry prompt (%d bytes)", len(bigPrompt), len(f.lastPrompt(small)))
	}
}

// Fail-open: no /props answer -> the escalated tier sees EXACTLY the entry
// packing (byte-identical fallback), the disposition is recorded, and the
// probe failure is cached so a second climb does not re-pay the probe.
func TestRunEscalationFailsOpenToEntryPacking(t *testing.T) {
	const small, big = "tier-small", "tier-big"
	f := &repackFake{t: t,
		nCtx: map[string]int{}, // nothing answers /props
		answers: map[string]string{
			small: `{"label":"billing","confidence":0.30}`,
			big:   `{"label":"billing","confidence":0.99}`,
		}}
	srv := f.server()
	defer srv.Close()

	cfg := repackCfg(srv, small, big, 600)
	p := New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, nil)
	req := core.Request{Task: core.TaskClassify, Input: repackInput("MIDDLE", 4000),
		Params: map[string]any{"labels": []string{"billing", "technical"}}}
	res := p.Run(context.Background(), req)
	if !res.OK {
		t.Fatalf("expected the escalation tier to answer, got defer: %s", res.Reason)
	}
	if f.lastPrompt(big) != f.lastPrompt(small) {
		t.Fatal("on probe failure the escalated tier must see the ENTRY packing byte-identically (fail-open contract)")
	}
	if !strings.HasPrefix(res.Meta.TierPack, "entry-inherited (") {
		t.Fatalf("TierPack = %q, want an entry-inherited disposition naming the failure", res.Meta.TierPack)
	}
	hitsAfterFirst := func() int { f.mu.Lock(); defer f.mu.Unlock(); return f.propsHits }()
	if hitsAfterFirst == 0 {
		t.Fatal("fixture invalid: the probe was never attempted")
	}
	// Second run inside the TTL: the cached failure must answer without a
	// fresh probe (the escalation path must not stall per climb).
	_ = p.Run(context.Background(), req)
	if got := func() int { f.mu.Lock(); defer f.mu.Unlock(); return f.propsHits }(); got != hitsAfterFirst {
		t.Fatalf("probe hits went %d -> %d within the TTL — the failure result was not cached", hitsAfterFirst, got)
	}
}

// The terminal reasoning tier is a callee too: it re-packs from the original
// against its own window.
func TestRunReasoningTierRepacksFromOriginal(t *testing.T) {
	const small, reason, needle = "tier-small", "tier-reason", "NEEDLE-FOR-THE-REASONER"
	f := &repackFake{t: t,
		nCtx: map[string]int{reason: 8192},
		answers: map[string]string{
			small:  `{"label":"billing","confidence":0.30}`,
			reason: `<think>ok</think>{"label":"billing","confidence":0.95}`,
		}}
	srv := f.server()
	defer srv.Close()

	cfg := repackCfg(srv, small, "", 600) // no escalation tier
	cfg.ReasoningModel = reason
	p := New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, nil)
	req := core.Request{Task: core.TaskClassify, Input: repackInput(needle, 4000),
		Params: map[string]any{"labels": []string{"billing", "technical"}}}
	res := p.Run(context.Background(), req)
	if !res.OK || !res.Meta.Reasoning {
		t.Fatalf("expected the reasoning tier to reclaim the defer (ok=%v reasoning=%v reason=%s)", res.OK, res.Meta.Reasoning, res.Reason)
	}
	if got := f.lastPrompt(reason); !strings.Contains(got, needle) {
		t.Fatal("the reasoning tier did not re-read the original source")
	}
	if res.Meta.TierPack != "token-exact (full source)" {
		t.Fatalf("TierPack = %q, want token-exact (full source)", res.Meta.TierPack)
	}
}

// The cache key is the ORIGINAL input — two different oversized originals that
// happen to share an entry trim must NOT collide (they did before TO-3), and
// the same original still hits.
func TestCacheKeyedOnOriginalInput(t *testing.T) {
	const small = "tier-small"
	f := &repackFake{t: t, nCtx: map[string]int{},
		answers: map[string]string{small: `{"label":"billing","confidence":0.99}`}}
	srv := f.server()
	defer srv.Close()

	cfg := repackCfg(srv, small, "", 600)
	ca, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	p := New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), ca, nil)

	// A and B share head and tail beyond the trim windows; only the (elided)
	// middle differs.
	shared := strings.Repeat("h", 500)
	tail := strings.Repeat("t", 500)
	reqA := core.Request{Task: core.TaskClassify, Input: shared + " ALPHA-MIDDLE " + tail,
		Params: map[string]any{"labels": []string{"billing", "technical"}}}
	reqB := core.Request{Task: core.TaskClassify, Input: shared + " BRAVO-MIDDLE " + tail,
		Params: map[string]any{"labels": []string{"billing", "technical"}}}

	if res := p.Run(context.Background(), reqA); !res.OK || res.Meta.CacheHit {
		t.Fatalf("first A run: ok=%v cacheHit=%v, want fresh success", res.OK, res.Meta.CacheHit)
	}
	if res := p.Run(context.Background(), reqB); !res.OK || res.Meta.CacheHit {
		t.Fatalf("B must MISS despite sharing A's entry trim (ok=%v cacheHit=%v) — the old trimmed-input key collided here", res.OK, res.Meta.CacheHit)
	}
	if res := p.Run(context.Background(), reqA); !res.OK || !res.Meta.CacheHit {
		t.Fatalf("repeat A must HIT (ok=%v cacheHit=%v) — cache continuity on the original", res.OK, res.Meta.CacheHit)
	}
}

// cutTokenExact never splits a rune even when the fixed-size chunk tokenizer
// splits one across pieces (the LO-13 mojibake class).
func TestCutTokenExactRuneSafety(t *testing.T) {
	f := &repackFake{t: t, nCtx: map[string]int{}}
	srv := f.server()
	defer srv.Close()

	// Multibyte content sized so 4-byte chunks split runes at both cut points.
	text := strings.Repeat("áß≠ล", 400) // 9 bytes per repeat, deliberately not chunk-aligned
	tokc := tokclient.New(srv.URL, "m", 0)
	packed, kept, ok := cutTokenExact(context.Background(), tokc, text, 100)
	if !ok {
		t.Fatal("cut failed against the healthy fake")
	}
	if kept != 100 {
		t.Fatalf("kept %d tokens, want the full allowance 100", kept)
	}
	if !strings.Contains(packed, repackMarker) {
		t.Fatal("cut result must carry the elision marker")
	}
	for _, part := range strings.Split(packed, repackMarker) {
		if !utf8.ValidString(part) {
			t.Fatalf("cut emitted invalid UTF-8 (a split rune): %q...", part[:24])
		}
	}
}
