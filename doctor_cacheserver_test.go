package main

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// B-01: a vLLM seat with NO cache-server binding FAILS doctor, one line per seat and
// a non-zero exit. The rule it enforces is the operator directive of 2026-09-10 —
// the second device's store backs every vLLM seat while that device is online — and
// the failure it ends is a config that bound one store to one seat name, which made
// "this seat has no tier" look identical to "this seat was never considered".
func TestDoctorFailsAVLLMSeatWithNoCacheServerBinding(t *testing.T) {
	srv := fakeSwap(t, defaultAliasIDs())
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.VLLMSeats = []string{"qwen3.8-27b-vllm", "qwen3.8-27b-vllm-3card"}
	cfg.KVCacheServers = config.KVCacheServers{
		{Enabled: true, Store: "fs_native", Address: "/mnt/kvcache/lmcache-seat-tp2-fp8", ChunkSize: 1568,
			KeyPrefix: "qube-seat-tp2-fp8", Seat: "qwen3.8-27b-vllm", KVDtype: "fp8", TensorParallel: 2},
	}
	var out strings.Builder
	err := doctorRun(cfg, nil, &out)
	if err == nil {
		t.Fatalf("a vLLM seat with no binding must fail doctor\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "1 vLLM seat(s) with no kv_cache_server binding") {
		t.Errorf("the error must count the uncovered seats: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "qwen3.8-27b-vllm-3card:") || !strings.Contains(got, "FAIL  no kv_cache_server binding") {
		t.Errorf("expected a FAIL line naming the uncovered seat:\n%s", got)
	}
	// The covered seat is reported OK, with the store it actually got — the whole
	// point is that each seat carries its OWN directory and namespace.
	if !strings.Contains(got, "OK    fs_native /mnt/kvcache/lmcache-seat-tp2-fp8 key_prefix=qube-seat-tp2-fp8") {
		t.Errorf("the bound seat must report its own store and namespace:\n%s", got)
	}
	// The failure line must say how to opt out, or the gate is a wall.
	if !strings.Contains(got, `"storeless":true`) {
		t.Errorf("the FAIL line must name the explicit opt-out:\n%s", got)
	}
}

// The explicit opt-out PASSES: a tier that is optional by charter must let a box
// decline it — but only out loud. Silence in either of its two shapes (no binding,
// or a binding switched off with no reason) still fails.
func TestDoctorPassesAnExplicitStorelessOptOutAndFailsSilence(t *testing.T) {
	srv := fakeSwap(t, defaultAliasIDs())
	base := func() config.Config {
		c := config.Default()
		c.Endpoint = srv.URL
		c.VLLMSeats = []string{"qwen3.8-27b-vllm-3card"}
		return c
	}
	cfg := base()
	cfg.KVCacheServers = config.KVCacheServers{
		{Seat: "qwen3.8-27b-vllm-3card", Storeless: true,
			Reason: "three-stage pipeline seat: LMCache's L2 adapter has no working layout for it"},
	}
	var ok strings.Builder
	if err := doctorRun(cfg, nil, &ok); err != nil {
		t.Fatalf("an explained opt-out must pass doctor: %v\n%s", err, ok.String())
	}
	if !strings.Contains(ok.String(), "OK    storeless by declaration: three-stage pipeline seat") {
		t.Errorf("the opt-out must be reported WITH its reason:\n%s", ok.String())
	}

	// A binding that is merely switched off says nothing about the decision.
	quiet := base()
	quiet.KVCacheServers = config.KVCacheServers{{Seat: "qwen3.8-27b-vllm-3card"}}
	var qout strings.Builder
	if err := doctorRun(quiet, nil, &qout); err == nil {
		t.Fatalf("a disabled binding with no reason must fail doctor\n%s", qout.String())
	}
	if !strings.Contains(qout.String(), "enabled:false and no storeless reason") {
		t.Errorf("the FAIL must name what is missing:\n%s", qout.String())
	}
}

// A box default (a binding that names no seat) covers every vLLM seat that has no
// binding of its own — which is how one store backs a whole box without the operator
// repeating it per seat.
func TestDoctorAcceptsTheBoxDefaultBindingForEverySeat(t *testing.T) {
	srv := fakeSwap(t, defaultAliasIDs())
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.VLLMSeats = []string{"seat-a", "seat-b"}
	cfg.KVCacheServers = config.KVCacheServers{
		{Enabled: true, Store: "fs_native", Address: "/mnt/kvcache/box", ChunkSize: 1568, KeyPrefix: "box-gen1"},
	}
	var out strings.Builder
	if err := doctorRun(cfg, nil, &out); err != nil {
		t.Fatalf("the box default must cover every seat: %v\n%s", err, out.String())
	}
	if n := strings.Count(out.String(), "OK    fs_native /mnt/kvcache/box"); n != 2 {
		t.Errorf("both seats must resolve to the default store, got %d:\n%s", n, out.String())
	}
}

// A box that runs no vLLM seat must not gain a section or a failure: the tier is
// optional by charter, and every llama.cpp box in the fleet is that box.
func TestDoctorIsSilentOnABoxWithNoVLLMSeats(t *testing.T) {
	srv := fakeSwap(t, defaultAliasIDs())
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	var out strings.Builder
	if err := doctorRun(cfg, nil, &out); err != nil {
		t.Fatalf("a box with no vLLM seat must pass: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "cache server") {
		t.Errorf("no vLLM seat means no cache-server section:\n%s", out.String())
	}
}

// The cache-server verdicts are pure config, so a dead serving layer must never hide
// them — the same blind spot the media section was moved above the health probe for.
func TestDoctorCacheServerSectionSurvivesADeadEndpoint(t *testing.T) {
	srv := fakeSwap(t, nil)
	url := srv.URL
	srv.Close()
	cfg := config.Default()
	cfg.Endpoint = url
	cfg.VLLMSeats = []string{"qwen3.8-27b-vllm"}
	var out strings.Builder
	if err := doctorRun(cfg, nil, &out); err == nil {
		t.Fatal("a dead endpoint is still a failure")
	}
	if !strings.Contains(out.String(), "qwen3.8-27b-vllm:") {
		t.Errorf("the cache-server section must print before the health probe:\n%s", out.String())
	}
}

// A binding left behind after a seat was renamed is a store nothing reads. It is
// SURFACED and never fatal: refusing would break a box the moment it retired a seat,
// which is the wrong trade for a tier that is optional by charter.
func TestDoctorSurfacesABindingForAnUnlistedSeat(t *testing.T) {
	srv := fakeSwap(t, defaultAliasIDs())
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.VLLMSeats = []string{"seat-a"}
	cfg.KVCacheServers = config.KVCacheServers{
		{Enabled: true, Store: "fs_native", Address: "/mnt/kvcache/a", ChunkSize: 1568, KeyPrefix: "a", Seat: "seat-a"},
		{Enabled: true, Store: "fs_native", Address: "/mnt/kvcache/gone", ChunkSize: 1568, KeyPrefix: "gone", Seat: "seat-retired"},
	}
	var out strings.Builder
	if err := doctorRun(cfg, nil, &out); err != nil {
		t.Fatalf("a leftover binding must not fail the gate: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "seat-retired:") || !strings.Contains(out.String(), "vllm_seats does not list it") {
		t.Errorf("the leftover binding must be surfaced:\n%s", out.String())
	}
}
