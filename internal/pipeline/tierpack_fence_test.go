package pipeline

// tierpack_fence_test.go: the cascade's per-tier window probe and tokenizer under
// a GPU-lease fence (2026-09-22). Both reach llama-swap's /upstream/<tier>/…,
// which starts a tier that is not loaded. Under a render they must not be sent,
// must not wait (the tier's generation waits at modelaffinity.Admit), and must
// not cache the fenced answer as "this tier has no window / no tokenizer".

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

func TestTierWindowProbeUnderAFenceIsNotSentOrCached(t *testing.T) {
	const small, big = "tier-small", "tier-big"
	f := &repackFake{t: t, nCtx: map[string]int{big: 8192}, answers: map[string]string{}}
	srv := f.server()
	defer srv.Close()
	cfg := repackCfg(srv, small, big, 600)
	p := New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, nil)

	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	if err := modelaffinity.SetGPULease("", root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = modelaffinity.SetGPULease("", filepath.Join(t.TempDir(), "My Drive", "x")) })
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "video render", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	n, why := p.tierNCtx(context.Background(), big)
	if n != 0 || !strings.Contains(why, "GPU lease") {
		t.Fatalf("tierNCtx under the fence = (%d, %q), want no window and the lease named", n, why)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("the tier probe waited %s under the fence; the tier's generation is the request that waits", el)
	}
	tok := p.tierTok(big)
	if _, ok := tok.Count(context.Background(), "some text"); ok {
		t.Fatal("the tier tokenizer answered under the fence over a cold tier")
	}
	if note := p.noteTokFail(big, tok); !strings.Contains(note, "GPU lease") {
		t.Fatalf("noteTokFail = %q, want the lease named", note)
	}
	if _, cached := p.tokFailFresh(big); cached {
		t.Fatal("a fenced tokenize was cached as a tokenizer failure")
	}
	f.mu.Lock()
	props, toks := f.propsHits, f.tokenizeHits
	f.mu.Unlock()
	if props != 0 || toks != 0 {
		t.Fatalf("/props hits %d, /tokenize hits %d under the fence — each one loads the tier", props, toks)
	}

	// The render ends: the next escalation probes for real (nothing was cached).
	_ = l.Release()
	if n, why := p.tierNCtx(context.Background(), big); n != 8192 {
		t.Fatalf("after the lease, tierNCtx = (%d, %q), want the live 8192: the fenced answer must not have been cached", n, why)
	}
}
