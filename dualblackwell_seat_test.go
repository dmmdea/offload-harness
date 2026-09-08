package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDualBlackwellSeedsThePairSeatWithTheCacheServer pins the operator's
// correction of 2026-09-08: blackwell-2x16 is the RTX 5060 Ti PAIR, and the
// tier seeds the same 27B vLLM TP2 seat blackwell-3x16 seeds — same operating
// point, same Lenovo cache server — with the device pair renumbered for a
// two-card box (0,1 instead of 0,2). Every Qube-class tier benefits from the
// cache server; a tier that seeds the llama.cpp fallback while its reference
// box serves the vLLM seat is the ADR 0035 defect, again.
//
// 0.114.1 briefly shipped the opposite (no seat), from an arm that spanned the
// 5070 Ti display card — a question this tier does not ask. This test makes
// the seat's presence, its pair, and its cache server load-bearing.
func TestDualBlackwellSeedsThePairSeatWithTheCacheServer(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	type seat struct {
		ID                   string   `json:"id"`
		Aliases              []string `json:"aliases"`
		Device               string   `json:"device"`
		TensorParallel       int      `json:"tensor_parallel"`
		ModelRepo            string   `json:"model_repo"`
		MaxModelLen          int      `json:"max_model_len"`
		GPUMemoryUtilization float64  `json:"gpu_memory_utilization"`
		KVCacheDtype         string   `json:"kv_cache_dtype"`
		TTLSeconds           int      `json:"ttl_seconds"`
		CacheServer          *struct {
			Store   string `json:"store"`
			Address string `json:"address"`
		} `json:"cache_server"`
		Measured string `json:"measured"`
	}
	var doc struct {
		Profiles map[string]struct {
			VLLMSeat *seat  `json:"vllm_seat"`
			Notes    string `json:"notes"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	two, ok := doc.Profiles["blackwell-2x16"]
	if !ok {
		t.Fatal("tier blackwell-2x16 not found — this gate went blind")
	}
	three, ok := doc.Profiles["blackwell-3x16"]
	if !ok || three.VLLMSeat == nil {
		t.Fatal("blackwell-3x16 or its vllm_seat not found — the pair seat has no reference to agree with")
	}
	s := two.VLLMSeat
	if s == nil {
		t.Fatal("blackwell-2x16 declares no vllm_seat. The tier IS the 5060 Ti pair the production " +
			"seat runs on; a fresh install must serve agent-pool with the Lenovo cache server, not the " +
			"llama.cpp fallback (operator correction 2026-09-08, overturning 0.114.1)")
	}
	if s.Device != "0,1" {
		t.Errorf("vllm_seat device = %q, want \"0,1\": on a two-card box the pair is devices 0 and 1", s.Device)
	}
	if s.TensorParallel != 2 {
		t.Errorf("vllm_seat tensor_parallel = %d, want 2", s.TensorParallel)
	}
	if s.CacheServer == nil || s.CacheServer.Store != "fs_native" || s.CacheServer.Address == "" {
		t.Errorf("vllm_seat has no fs_native cache server — every Qube-class tier benefits from the Lenovo store (got %+v)", s.CacheServer)
	}
	// Same operating point as the 3-card tier's pair seat: the numbers were measured on
	// this exact silicon, so a divergence is a typo, not a decision.
	r := three.VLLMSeat
	for _, c := range []struct{ name, got, want string }{
		{"id", s.ID, r.ID},
		{"model_repo", s.ModelRepo, r.ModelRepo},
		{"kv_cache_dtype", s.KVCacheDtype, r.KVCacheDtype},
		{"cache_server.address", s.CacheServer.Address, r.CacheServer.Address},
	} {
		if c.got != c.want {
			t.Errorf("vllm_seat.%s = %q, differs from blackwell-3x16's %q", c.name, c.got, c.want)
		}
	}
	if s.MaxModelLen != r.MaxModelLen || s.GPUMemoryUtilization != r.GPUMemoryUtilization || s.TTLSeconds != r.TTLSeconds {
		t.Errorf("vllm_seat operating point (max_model_len %d, util %.2f, ttl %d) differs from blackwell-3x16's (%d, %.2f, %d)",
			s.MaxModelLen, s.GPUMemoryUtilization, s.TTLSeconds, r.MaxModelLen, r.GPUMemoryUtilization, r.TTLSeconds)
	}
	if !strings.Contains(s.Aliases[0]+strings.Join(s.Aliases, ","), "agent-pool") {
		t.Errorf("vllm_seat aliases %v lack agent-pool — the harness binds to that alias", s.Aliases)
	}
	// The record must say the seat was overturned INTO existence, so a reader of the
	// tier table does not mistake 0.114.1's paragraph for the current verdict.
	for _, must := range []string{"0.114.2", "5060 Ti pair", "cache server"} {
		if !strings.Contains(two.Notes, must) {
			t.Errorf("blackwell-2x16 notes do not carry the correction (%q missing)", must)
		}
	}
	if !strings.Contains(s.Measured, "0,1") || !strings.Contains(s.Measured, "0,2") {
		t.Errorf("vllm_seat.measured must name both device numberings (2-card 0,1 / 3-card 0,2); got %.120s…", s.Measured)
	}
}
