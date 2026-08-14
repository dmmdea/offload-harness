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
	prompts      map[string][]string
	propsHits    int
	chatHits     int
	tokenizeHits int
	tokenize404  bool // serve /props but 404 every /tokenize (route absent)
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
			f.mu.Lock()
			f.tokenizeHits++
			deny := f.tokenize404
			f.mu.Unlock()
			if deny {
				http.NotFound(w, r)
				return
			}
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
	// Entry-only runs must generate ZERO repack traffic — the hot path is
	// untouched by construction, pinned here.
	f.mu.Lock()
	props, toks := f.propsHits, f.tokenizeHits
	f.mu.Unlock()
	if props != 0 || toks != 0 {
		t.Fatalf("entry-only runs probed the endpoint (props=%d tokenize=%d) — the hot path must stay free", props, toks)
	}
}

// The repack must never SHRINK the escalated view below the entry view: a
// callee served with a small window passes the fixed 256-token floor yet
// would see LESS than the entry tier — the exact inversion TO-3 exists to
// prevent (round-1 review finding, rating 8).
func TestRunEscalationNeverShrinksBelowEntryView(t *testing.T) {
	const small, big = "tier-small", "tier-big"
	f := &repackFake{t: t,
		// orig 8000 chars = 2000 tokens; entry view 6000 chars = 1500 tokens.
		// nCtx 900: allowance = 900-64-128 = 708; scaffold ~50 -> inputAllowance
		// ~658 >= 256 (passes the floor) but 658 <= 1500 (buys no view).
		nCtx: map[string]int{big: 900},
		answers: map[string]string{
			small: `{"label":"billing","confidence":0.30}`,
			big:   `{"label":"billing","confidence":0.99}`,
		}}
	srv := f.server()
	defer srv.Close()

	cfg := repackCfg(srv, small, big, 6000)
	p := New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, nil)
	req := core.Request{Task: core.TaskClassify, Input: repackInput("MIDDLE", 8000),
		Params: map[string]any{"labels": []string{"billing", "technical"}}}
	res := p.Run(context.Background(), req)
	if !res.OK {
		t.Fatalf("expected the escalation tier to answer, got defer: %s", res.Reason)
	}
	if f.lastPrompt(big) != f.lastPrompt(small) {
		t.Fatal("a repack that buys no view must fall back to the ENTRY packing byte-identically — the escalated tier saw a SMALLER view than the entry tier")
	}
	if !strings.Contains(res.Meta.TierPack, "buys no view") {
		t.Fatalf("TierPack = %q, want the buys-no-view disposition", res.Meta.TierPack)
	}
}

// The reasoning tier generates with MaxTokens+reasoningThinkBudget, so its
// repack must budget against that REAL completion request — budgeting bare
// MaxTokens overshot the served window by ~384 tokens (round-1 CRITICAL).
func TestRunReasoningRepackBudgetsThinkSpan(t *testing.T) {
	const small, reason = "tier-small", "tier-reason"
	f := &repackFake{t: t,
		// orig 4000 chars = 1000 tokens, scaffold ~50 -> tokFull ~1050.
		// nCtx 1300: WITHOUT the think budget, allowance = 1300-64-128 = 1108
		// >= 1050 -> full source would be (wrongly) accepted and the request
		// would overshoot: prompt 1050 + generation 576 = 1626 > 1300.
		// WITH it, allowance = 1300-576-128 = 596 < 1050 -> token-exact cut.
		nCtx: map[string]int{reason: 1300},
		answers: map[string]string{
			small:  `{"label":"billing","confidence":0.30}`,
			reason: `<think>ok</think>{"label":"billing","confidence":0.95}`,
		}}
	srv := f.server()
	defer srv.Close()

	cfg := repackCfg(srv, small, "", 600)
	cfg.ReasoningModel = reason
	p := New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, nil)
	req := core.Request{Task: core.TaskClassify, Input: repackInput("MIDDLE", 4000),
		Params: map[string]any{"labels": []string{"billing", "technical"}}}
	res := p.Run(context.Background(), req)
	if !res.OK || !res.Meta.Reasoning {
		t.Fatalf("expected the reasoning tier to answer (ok=%v reasoning=%v reason=%s)", res.OK, res.Meta.Reasoning, res.Reason)
	}
	if !strings.HasPrefix(res.Meta.TierPack, "token-exact (cut ") {
		t.Fatalf("TierPack = %q, want a CUT: full-source acceptance here means the think budget was not reserved and the request overshoots the window", res.Meta.TierPack)
	}
	if !strings.Contains(f.lastPrompt(reason), repackMarker) {
		t.Fatal("the reasoning prompt must carry the cut marker — the think-budget reservation forces a cut at this window")
	}
}

// Tokenize-route failure AFTER a good probe: byte-identical entry fallback,
// and the failure is TTL-cached so a second climb re-pays nothing (round-1
// review finding: the doc claimed stickiness the code did not have).
func TestRunEscalationTokenizeFailureFailsOpenAndCaches(t *testing.T) {
	const small, big = "tier-small", "tier-big"
	f := &repackFake{t: t,
		nCtx:        map[string]int{big: 8192},
		tokenize404: true,
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
		t.Fatal("on tokenize failure the escalated tier must see the ENTRY packing byte-identically")
	}
	if !strings.HasPrefix(res.Meta.TierPack, "entry-inherited (tokenize: ") {
		t.Fatalf("TierPack = %q, want the tokenize failure named", res.Meta.TierPack)
	}
	hits := func() int { f.mu.Lock(); defer f.mu.Unlock(); return f.tokenizeHits }()
	if hits == 0 {
		t.Fatal("fixture invalid: tokenize was never attempted")
	}
	res2 := p.Run(context.Background(), req)
	if got := func() int { f.mu.Lock(); defer f.mu.Unlock(); return f.tokenizeHits }(); got != hits {
		t.Fatalf("tokenize hits went %d -> %d within the TTL — the failure was not cached and every climb re-pays dead round-trips", hits, got)
	}
	if !strings.HasPrefix(res2.Meta.TierPack, "entry-inherited (tokenize (cached): ") {
		t.Fatalf("second run TierPack = %q, want the CACHED tokenize failure named", res2.Meta.TierPack)
	}
}

// Probe TTL expiry: a restarted llama-swap must not stay mis-degraded forever
// — after the TTL a fresh probe runs and the repack recovers.
func TestRunEscalationProbeTTLExpiryRecovers(t *testing.T) {
	const small, big = "tier-small", "tier-big"
	f := &repackFake{t: t,
		nCtx: map[string]int{}, // /props dead at first
		answers: map[string]string{
			small: `{"label":"billing","confidence":0.30}`,
			big:   `{"label":"billing","confidence":0.99}`,
		}}
	srv := f.server()
	defer srv.Close()

	cfg := repackCfg(srv, small, big, 600)
	p := New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, nil)
	base := time.Now()
	p.nowFn = func() time.Time { return base }
	req := core.Request{Task: core.TaskClassify, Input: repackInput("NEEDLE-AFTER-RECOVERY", 4000),
		Params: map[string]any{"labels": []string{"billing", "technical"}}}
	if res := p.Run(context.Background(), req); !strings.HasPrefix(res.Meta.TierPack, "entry-inherited (") {
		t.Fatalf("first run TierPack = %q, want the probe failure", res.Meta.TierPack)
	}
	// llama-swap comes back; the TTL elapses.
	f.mu.Lock()
	f.nCtx[big] = 8192
	f.mu.Unlock()
	p.nowFn = func() time.Time { return base.Add(probeTTL + time.Minute) }
	res := p.Run(context.Background(), req)
	if res.Meta.TierPack != "token-exact (full source)" {
		t.Fatalf("post-TTL TierPack = %q, want a fresh probe and a full-source repack (an inverted TTL check would stay degraded forever)", res.Meta.TierPack)
	}
	if !strings.Contains(f.lastPrompt(big), "NEEDLE-AFTER-RECOVERY") {
		t.Fatal("the recovered repack did not deliver the original source")
	}
}

// A degenerate callee window (allowance under the floor) is disclosed, and an
// under-entry-cap input never probes at all — with an HONEST label that does
// not claim measurement (round-1 review finding: the old "token-exact (full
// source)" label here was a false verification claim).
func TestRunEscalationDegenerateAndUnderCapDispositions(t *testing.T) {
	const small, big = "tier-small", "tier-big"
	f := &repackFake{t: t,
		// orig 4000 chars = 1000 tok, scaffold ~50 -> tokFull ~1050 > allowance
		// = 400-64-128 = 208; inputAllowance = 208-50 = 158 < 256 -> degenerate.
		nCtx: map[string]int{big: 400},
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
	if !strings.HasPrefix(res.Meta.TierPack, "entry-inherited (degenerate allowance ") {
		t.Fatalf("TierPack = %q, want the degenerate allowance named", res.Meta.TierPack)
	}
	if f.lastPrompt(big) != f.lastPrompt(small) {
		t.Fatal("degenerate allowance must fall back to the entry packing byte-identically")
	}

	// Under the entry cap: nothing was cut, nothing may be probed or claimed.
	f2 := &repackFake{t: t, nCtx: map[string]int{big: 8192},
		answers: map[string]string{
			small: `{"label":"billing","confidence":0.30}`,
			big:   `{"label":"billing","confidence":0.99}`,
		}}
	srv2 := f2.server()
	defer srv2.Close()
	cfg2 := repackCfg(srv2, small, big, 600)
	p2 := New(cfg2, llamaclient.New(srv2.URL, cfg2.CompletionPath, "", 10*time.Second), nil, nil)
	res2 := p2.Run(context.Background(), core.Request{Task: core.TaskClassify,
		Input:  "short input that fits the entry cap with room to spare, over the trivial floor",
		Params: map[string]any{"labels": []string{"billing", "technical"}}})
	if res2.Meta.TierPack != "full source (under entry cap)" {
		t.Fatalf("under-cap TierPack = %q, want the unverified-honest label", res2.Meta.TierPack)
	}
	f2.mu.Lock()
	props, toks := f2.propsHits, f2.tokenizeHits
	f2.mu.Unlock()
	if props != 0 || toks != 0 {
		t.Fatalf("under-cap path probed the endpoint (props=%d tokenize=%d) — it must be free", props, toks)
	}
}

// Three-model chain: EACH climbed tier repacks from the ORIGINAL, not its
// predecessor's cut — the mid tier's token-cut view must not become the big
// tier's source (round-1 review finding, rating 7).
func TestRunThreeTierChainRepacksEachFromOriginal(t *testing.T) {
	const entry, mid, big, needle = "tier-entry", "tier-mid", "tier-big", "NEEDLE-DEEP-IN-THE-MIDDLE"
	f := &repackFake{t: t,
		// mid: nCtx 1300 -> cut (needle in the elided middle); big: 8192 -> full.
		nCtx: map[string]int{mid: 1300, big: 8192},
		answers: map[string]string{
			entry: `{"label":"billing","confidence":0.30}`,
			mid:   `{"label":"billing","confidence":0.30}`,
			big:   `{"label":"billing","confidence":0.99}`,
		}}
	srv := f.server()
	defer srv.Close()

	cfg := repackCfg(srv, entry, big, 600)
	cfg.TriageModel = entry
	cfg.Model = mid
	p := New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, nil)
	req := core.Request{Task: core.TaskClassify, Input: repackInput(needle, 8000),
		Params: map[string]any{"labels": []string{"billing", "technical"}}}
	res := p.Run(context.Background(), req)
	if !res.OK {
		t.Fatalf("expected the final tier to answer, got defer: %s", res.Reason)
	}
	if got := f.lastPrompt(mid); !strings.Contains(got, repackMarker) || strings.Contains(got, needle) {
		t.Fatalf("fixture invalid: the mid tier's view must be a cut that elides the needle (marker=%v needle=%v)", strings.Contains(got, repackMarker), strings.Contains(got, needle))
	}
	if got := f.lastPrompt(big); !strings.Contains(got, needle) {
		t.Fatal("the big tier's view lacks the needle — it inherited the MID tier's cut instead of re-reading the original")
	}
	if res.Meta.TierPack != "token-exact (full source)" {
		t.Fatalf("final TierPack = %q, want the big tier's own full-source disposition", res.Meta.TierPack)
	}
}

// cutTokenExact's defensive over-allowance branch returns the text unchanged.
func TestCutTokenExactOverAllowanceReturnsUnchanged(t *testing.T) {
	f := &repackFake{t: t, nCtx: map[string]int{}}
	srv := f.server()
	defer srv.Close()
	text := strings.Repeat("word ", 100)
	tokc := tokclient.New(srv.URL, "m", 0)
	packed, kept, ok := cutTokenExact(context.Background(), tokc, text, 1<<20)
	if !ok || packed != text || kept != (len(text)+chunkTokBytes-1)/chunkTokBytes {
		t.Fatalf("over-allowance cut must return the text unchanged (ok=%v kept=%d)", ok, kept)
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
