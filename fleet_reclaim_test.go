package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"llamaswap-pp-cli/pkg/llamaswap"
)

// noneProtected is the keep-set answer for a box whose config protects nothing.
func noneProtected(string) bool { return false }

// protect returns an IsProtected-shaped predicate over a fixed name set.
func protect(names ...string) func(string) bool {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return func(id string) bool { return set[id] }
}

// TestReclaimableUsesConfigTruthNotTheServerTTL is the regression for the bug this
// call site carried: GET /running publishes `ttl: 0` for a seat CONFIGURED
// `ttl: -1` (verified live on llama-swap v249 — both mem0-stack seats report 0
// there). The old rule read residency off that field.
//
// Both rows below arrive as ttl:0. Only one of them is actually resident in the
// config; the other is a swapping seat the server also happens to report as 0.
// The ttl rule calls the whole snapshot idle and folds a live workhorse into the
// idle baseline; the keep-set rule sees the reclaimable seat.
func TestReclaimableUsesConfigTruthNotTheServerTTL(t *testing.T) {
	running := []llamaswap.RunningModel{
		{ID: "embeddinggemma", State: "ready", TTL: 0}, // configured ttl:-1, misreported as 0
		{ID: "gemma-4-26b", State: "ready", TTL: 0},    // a swapping seat, also reported as 0
	}
	if got := anyReclaimable(running, protect("embeddinggemma"), true, nil); !got {
		t.Error("a loaded swapping seat must be reclaimable even when the server reports ttl:0 for it")
	}
	// The same snapshot under the old server-ttl rule: everything looks resident.
	if got := anyReclaimable(running, noneProtected, false, nil); got {
		t.Error("the documented fallback must still read the ttl field when no config is available")
	}
}

// TestReclaimableProtectsTheKeepSet: a resident support seat is baseline, not
// capacity. Unloading the embedder and reranker defeats the reason they are
// co-resident (one RAG query paid three full model loads without them).
func TestReclaimableProtectsTheKeepSet(t *testing.T) {
	running := []llamaswap.RunningModel{
		{ID: "embeddinggemma", State: "ready", TTL: 0},
		{ID: "bge-reranker-v2-m3", State: "ready", TTL: 0},
	}
	if anyReclaimable(running, protect("embeddinggemma", "bge-reranker-v2-m3"), true, nil) {
		t.Error("a snapshot of nothing but keep-set members holds no reclaimable VRAM")
	}
}

// TestReclaimableCountsASupportSeatWithARealTTL: the old rule's other half. A
// support seat given a real TTL instead of 0 was counted as reclaimable, which
// over-stated capacity by the size of an embedder. Config truth files it under
// the keep-set regardless of what ttl says.
func TestReclaimableCountsASupportSeatWithARealTTL(t *testing.T) {
	running := []llamaswap.RunningModel{{ID: "embeddinggemma", State: "ready", TTL: 900}}
	if anyReclaimable(running, protect("embeddinggemma"), true, nil) {
		t.Error("a keep-set member with a non-zero ttl is still baseline, not capacity")
	}
	if !anyReclaimable(running, noneProtected, false, nil) {
		t.Error("the ttl fallback keeps its old verdict for a seat with a real ttl")
	}
}

// TestReclaimableIgnoresStoppedSeats: a stopped/stopping row holds no VRAM, so it
// is neither baseline nor capacity. Unchanged by this refactor, and asserted so a
// future edit to the classifier cannot drop it silently.
func TestReclaimableIgnoresStoppedSeats(t *testing.T) {
	for _, state := range []string{"stopped", "stopping", "STOPPED", "Stopping"} {
		running := []llamaswap.RunningModel{{ID: "gemma-4-26b", State: state, TTL: 300}}
		if anyReclaimable(running, noneProtected, true, nil) {
			t.Errorf("state %q holds no VRAM and must not count as reclaimable", state)
		}
	}
	if anyReclaimable(nil, noneProtected, true, nil) {
		t.Error("an empty /running holds nothing")
	}
}

// reclaimBox stands up a llama-swap that reports `running` on /running and points
// the keep-set loader at a YAML that is entirely under the test's control, so the
// answer never depends on whatever llama-swap config this machine has. residentYAML
// names a ttl:-1 seat to give the keep-set a member (keepSetKnown=true); empty gives
// a YAML with no resident seat at all (keepSetKnown=false, the ttl fallback).
func reclaimBox(t *testing.T, running string, residentYAML string) config.Config {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/running" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"running":` + running + `}`))
	}))
	t.Cleanup(srv.Close)

	yaml := "models:\n  \"swapper\":\n    cmd: \"x\"\n    ttl: 300\n"
	if residentYAML != "" {
		yaml += "  \"" + residentYAML + "\":\n    cmd: \"x\"\n    ttl: -1\n"
	}
	path := filepath.Join(t.TempDir(), "llama-swap.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLAMASWAP_YAML", path)
	t.Setenv("LLAMASWAP_CONFIG", "")
	t.Setenv("LLAMASWAP_KEEP_SET", "")

	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.StateDir = t.TempDir() // no lease held
	cfg.GPULockPath = ""
	cfg.MemoryStack = nil // an unset memory_stack: the default set applies
	return cfg
}

// TestFleetReclaimNeverReclaimsAMemoryStackMember is the register C-94 regression.
// The house rule gives EVERY model a 300 s idle ttl, so a mem0 stack member is not
// "ttl -1/0" and the keep-set (which reads ttl -1 seats) does not hold it. The
// reclaim path therefore called a resident embedder reclaimable capacity and a job
// could take the memory authority's embedder off the card. `gpu reserve
// --unload-seat` and render/gpu-lock.mjs both keep cfg.MemoryStack (or its default
// when empty); reclaim has to keep the same set, in both keep-set modes.
func TestFleetReclaimNeverReclaimsAMemoryStackMember(t *testing.T) {
	for _, mode := range []struct{ name, resident string }{
		{"keep-set known", "some-pinned-seat"},
		{"keep-set unknown (ttl fallback)", ""},
	} {
		t.Run(mode.name, func(t *testing.T) {
			// Every default member, each at the house-rule ttl of 300.
			for _, member := range config.Default().MemoryStack {
				cfg := reclaimBox(t, `[{"model":"`+member+`","state":"ready","ttl":300}]`, mode.resident)
				if loaded, ok := oursLoaded(cfg); !ok || loaded {
					t.Errorf("default stack member %q at ttl 300 = (loaded=%v, ok=%v); it is baseline, never reclaimable", member, loaded, ok)
				}
			}
			// A non-member at the same ttl is still capacity a job can take.
			cfg := reclaimBox(t, `[{"model":"gemma-4-26b","state":"ready","ttl":300}]`, mode.resident)
			if loaded, ok := oursLoaded(cfg); !ok || !loaded {
				t.Errorf("a non-member at ttl 300 = (loaded=%v, ok=%v); it must still count as reclaimable", loaded, ok)
			}
			// A member beside a non-member: the non-member alone keeps it reclaimable.
			cfg = reclaimBox(t, `[{"model":"embeddinggemma-ams","state":"ready","ttl":300},{"model":"gemma-4-26b","state":"ready","ttl":300}]`, mode.resident)
			if loaded, ok := oursLoaded(cfg); !ok || !loaded {
				t.Errorf("member + non-member = (loaded=%v, ok=%v); the non-member must still count", loaded, ok)
			}
		})
	}
}

// TestFleetReclaimHonoursAConfiguredMemoryStack: a non-empty memory_stack REPLACES
// the default (gpu_drain does the same), so a configured name is protected and a
// default name the config left out is not. Matching ignores case and padding, the
// way otherResidentModels does.
func TestFleetReclaimHonoursAConfiguredMemoryStack(t *testing.T) {
	cfg := reclaimBox(t, `[{"model":"custom-embed","state":"ready","ttl":300}]`, "some-pinned-seat")
	cfg.MemoryStack = []string{"  Custom-Embed "}
	if loaded, ok := oursLoaded(cfg); !ok || loaded {
		t.Errorf("configured stack member = (loaded=%v, ok=%v), want protected", loaded, ok)
	}
	// /running's id differs from the configured name only by case and padding.
	cfg = reclaimBox(t, `[{"model":" CUSTOM-embed ","state":"ready","ttl":300}]`, "some-pinned-seat")
	cfg.MemoryStack = []string{"custom-embed"}
	if loaded, ok := oursLoaded(cfg); !ok || loaded {
		t.Errorf("a /running id differing only by case and padding = (loaded=%v, ok=%v), want protected", loaded, ok)
	}
	cfg = reclaimBox(t, `[{"model":"embeddinggemma","state":"ready","ttl":300}]`, "some-pinned-seat")
	cfg.MemoryStack = []string{"custom-embed"}
	if loaded, ok := oursLoaded(cfg); !ok || !loaded {
		t.Errorf("a default name the configured stack omits = (loaded=%v, ok=%v); the list replaces the default, so it is reclaimable", loaded, ok)
	}
}
