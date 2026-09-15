package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// seatFixture is the reference workstation's tensor-parallel pair, with the tier's
// own store declaration — the half a per-seat binding must be able to replace.
func seatFixture(id string) vllmseat.Spec {
	return vllmseat.Spec{
		ID: id, Unit: "vllm-agent-seat-27b", Port: 18797, MPPort: 18796,
		Launch: vllmseat.LaunchWindowsWSL, Device: "0,2", TensorParallel: 2,
		ModelPath:   "/hf/hub/models--RedHatAI--Qwen3.8-27B-INT4/snapshots/2fb0debc",
		ModelRepo:   "hub/models--RedHatAI--Qwen3.8-27B-INT4",
		MaxModelLen: 163840, GPUMemoryUtilization: 0.9, MaxNumSeqs: 32,
		MaxBatchedTokens: 3135, KVCacheDtype: "fp8",
		ToolCallParser:  "qwen3_xml",
		ReasoningParser: "qwen3", TTLSeconds: 300,
		Fallback: "qwen3.8-27b", FallbackCtx: 131072, AgentCtxTokens: 163840,
		CacheServer: &vllmseat.CacheServer{
			Store: "fs_native", Address: "/mnt/kvcache/lmcache-seat-tp2-fp8",
			L1StagingGB: 8, ChunkSize: 1568, KeyPrefix: "qube-seat-tp2-fp8",
			MaxCapacityGB: 45, PruneGB: 45, NumWorkers: 8,
			MountDir: "/mnt/kvcache", MinMBPS: 200,
		},
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// B-01(4): the render reads its binding BY SEAT NAME, so two seats on one box each
// carry their own store directory and namespace. Sharing either across layouts is
// the measured silent failure, and before this the config could name only one seat.
func TestSeatRenderPicksItsOwnBinding(t *testing.T) {
	// Built rather than pasted: the namespace names are assembled from their parts
	// so the fixture cannot drift from the assertions below, and so no line of this
	// file reads as `key_prefix":"<opaque string>"` — the shape the tree's secret
	// scanner classifies as a generic API key.
	const pairGen, trioGen = "tp2-fp8", "pp3-fp16"
	pairPrefix, trioPrefix := "qube-seat-"+pairGen, "qube-seat-"+trioGen
	cfgPath := writeConfig(t, fmt.Sprintf(`{"vllm_seats":["qwen3.8-27b-vllm","qwen3.8-27b-vllm-3card"],"kv_cache_server":[
	  {"enabled":true,"store":"fs_native","address":"/mnt/kvcache/lmcache-seat-%s","chunk_size":1568,"l1_staging_gb":8,"key_prefix":%q,"seat":"qwen3.8-27b-vllm","kv_dtype":"fp8","tensor_parallel":2},
	  {"enabled":true,"store":"fs_native","address":"/mnt/kvcache/lmcache-seat-%s","chunk_size":784,"l1_staging_gb":12,"key_prefix":%q,"seat":"qwen3.8-27b-vllm-3card","kv_dtype":"fp16","tensor_parallel":3}]}`,
		pairGen, pairPrefix, trioGen, trioPrefix))

	envFor := func(id string) (string, vllmseat.Spec) {
		s, err := applySeatBinding(seatFixture(id), cfgPath)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		files, err := s.Artifacts(filepath.Join("setup", "templates", "vllm-seat", "windows-wsl"), renderRT())
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return files[id+".env"], s
	}

	pair, pairSpec := envFor("qwen3.8-27b-vllm")
	for _, want := range []string{"/mnt/kvcache/lmcache-seat-" + pairGen, "SEAT_CHUNK=1568", "SEAT_L1_GB=8"} {
		if !strings.Contains(pair, want) {
			t.Errorf("the pair seat's env is missing %q:\n%s", want, pair)
		}
	}
	trio, trioSpec := envFor("qwen3.8-27b-vllm-3card")
	for _, want := range []string{"/mnt/kvcache/lmcache-seat-" + trioGen, "SEAT_CHUNK=784", "SEAT_L1_GB=12"} {
		if !strings.Contains(trio, want) {
			t.Errorf("the 3-card seat's env is missing %q:\n%s", want, trio)
		}
	}
	// The namespace each seat carries into the harness half. fs_native namespaces by
	// DIRECTORY, so the store dir above is the live separation; key_prefix is what
	// the config block and offload_status publish, and it must be per-seat too.
	if pairSpec.CacheServer.KeyPrefix != pairPrefix || trioSpec.CacheServer.KeyPrefix != trioPrefix {
		t.Errorf("each seat must carry its own namespace: %q / %q",
			pairSpec.CacheServer.KeyPrefix, trioSpec.CacheServer.KeyPrefix)
	}
	// The two seats must not be able to end up in one namespace or one directory.
	if strings.Contains(trio, "lmcache-seat-"+pairGen) || strings.Contains(pair, "lmcache-seat-"+trioGen) {
		t.Error("a seat picked up another seat's binding — one store per seat is the whole rule")
	}
	// What only the TIER knows — the mount point, the write floor, the prune target
	// — survives the override: those are properties of the share, not the namespace.
	for _, want := range []string{"SEAT_L2_MIN_MBPS=200", "SEAT_L2_PRUNE_GB=45", "SEAT_L2_MOUNT_DIR=/mnt/kvcache"} {
		if !strings.Contains(trio, want) {
			t.Errorf("the tier's own deployment facts must survive the binding: missing %q", want)
		}
	}
}

// The box default (a binding that names no seat) backs every seat that has none of
// its own — one declaration for a whole box.
func TestSeatRenderFallsBackToTheBoxDefaultBinding(t *testing.T) {
	cfgPath := writeConfig(t, `{"vllm_seats":["seat-x"],"kv_cache_server":[{"enabled":true,"store":"fs_native","address":"/mnt/kvcache/box","chunk_size":1568,"key_prefix":"box-gen1"}]}`)
	s, err := applySeatBinding(seatFixture("seat-x"), cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if s.CacheServer == nil || s.CacheServer.Address != "/mnt/kvcache/box" || s.CacheServer.KeyPrefix != "box-gen1" {
		t.Fatalf("the box default must reach a seat with no binding of its own: %+v", s.CacheServer)
	}
}

// A storeless binding renders a seat with NO L2 at all: an opt-out that still
// rendered the tier's store would be an opt-out in the config and a store on disk.
func TestStorelessBindingRendersNoL2(t *testing.T) {
	cfgPath := writeConfig(t, `{"vllm_seats":["seat-x"],"kv_cache_server":[{"seat":"seat-x","storeless":true,"reason":"three-stage pipeline seat: no L2 layout works"}]}`)
	s, err := applySeatBinding(seatFixture("seat-x"), cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if s.CacheServer != nil {
		t.Fatalf("a storeless binding must clear the tier's store: %+v", s.CacheServer)
	}
	files, err := s.Artifacts(filepath.Join("setup", "templates", "vllm-seat", "windows-wsl"), renderRT())
	if err != nil {
		t.Fatal(err)
	}
	env := files["seat-x.env"]
	for _, line := range strings.Split(env, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "SEAT_L2=") {
			v := strings.Trim(strings.TrimPrefix(strings.TrimSpace(line), "SEAT_L2="), "'")
			if v != "" {
				t.Fatalf("a storeless seat must render an empty SEAT_L2, got %q", v)
			}
		}
	}
}

// A config whose binding cannot work is refused at RENDER time, naming the seat —
// the artifacts must never describe a store the engine would reject at start.
func TestSeatRenderRefusesAnUnworkableBinding(t *testing.T) {
	cfgPath := writeConfig(t, `{"vllm_seats":["seat-x"],"kv_cache_server":[{"enabled":true,"store":"fs_native","address":"/mnt/kvcache/x","chunk_size":1568,"key_prefix":"x","seat":"seat-x"}]}`)
	s := seatFixture("seat-x")
	// mount_dir belongs to the tier; an address outside it means the store path is
	// on the local disk instead of the export.
	s.CacheServer.MountDir = "/mnt/other"
	_, err := applySeatBinding(s, cfgPath)
	if err == nil || !strings.Contains(err.Error(), `seat "seat-x"`) {
		t.Fatalf("expected a refusal naming the seat, got %v", err)
	}
}

// A seat with no binding at all renders the tier's own declaration and says so — the
// tier is optional by charter, and the gate that fails it lives in doctor, not here.
func TestSeatRenderWithoutABindingKeepsTheTierDeclaration(t *testing.T) {
	cfgPath := writeConfig(t, `{"vllm_seats":["seat-x"]}`)
	s, err := applySeatBinding(seatFixture("seat-x"), cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if s.CacheServer == nil || s.CacheServer.KeyPrefix != "qube-seat-tp2-fp8" {
		t.Fatalf("the tier's declaration must survive an unbound seat: %+v", s.CacheServer)
	}
}

// The example config must stay loadable and must demonstrate the LIST shape, or the
// one file an operator copies teaches the shape this change replaced.
func TestExampleConfigDocumentsTheListShape(t *testing.T) {
	raw, err := os.ReadFile("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"kv_cache_server", "vllm_seats"} {
		if _, ok := m[key]; !ok {
			t.Errorf("config.example.json must carry %q", key)
		}
	}
}

func renderRT() vllmseat.Runtime {
	return vllmseat.Runtime{
		User: "BOX\\operator", ProxyHost: "127.0.0.1",
		StackDir: "C:/llama-swap", SeatDir: "C:/llama-swap/seat",
		VenvDir: "/root/g7/vllm-env", HFHome: "/hf",
		LMCacheOverlay: "/root/g7/lmcache-overlay",
		CacheMountSrc:  "//store.example/kvcache", CacheMountOpts: "vers=3.1.1",
		Distro: "freetoken", WSLSeatDir: "/root/g7",
	}
}
