package vllmseat

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// wslTemplatesDir is the reference pattern for a Windows box whose engine runs in WSL.
func wslTemplatesDir() string {
	return filepath.Join("..", "..", "setup", "templates", "vllm-seat", "windows-wsl")
}

// flagship is the 3-card reference workstation's agent seat: the 27B on the 5060 Ti
// PAIR (devices 0 and 2 in PCI order), tensor parallel, fp8 KV, cache server on the
// Lenovo export. Device 1 is the 5070 Ti display card and is absent by law.
func flagship() Spec {
	return Spec{
		ID:                   "qwen3.8-27b-vllm",
		Aliases:              []string{"agent-pool", "27b-vllm"},
		Unit:                 "vllm-agent-seat-27b",
		Port:                 18797,
		MPPort:               18796,
		Launch:               LaunchWindowsWSL,
		Device:               "0,2",
		TensorParallel:       2,
		ModelRepo:            "hub/models--RedHatAI--Qwen3.8-27B-INT4",
		MaxModelLen:          163840,
		GPUMemoryUtilization: 0.9,
		MaxNumSeqs:           32,
		MaxBatchedTokens:     3135,
		KVCacheDtype:         "fp8",
		ToolCallParser:       "qwen3_xml",
		ReasoningParser:      "qwen3",
		TTLSeconds:           300,
		Fallback:             "qwen3.8-27b",
		FallbackCtx:          131072,
		AgentCtxTokens:       163840,
		CacheServer: &CacheServer{
			Store: "fs_native", Address: "/mnt/kvcache/lmcache-seat-tp2-fp8",
			L1StagingGB: 8, ChunkSize: 1568, KeyPrefix: "qube-seat-tp2-fp8",
			MaxCapacityGB: 45, PruneGB: 45, NumWorkers: 8,
			MountDir: "/mnt/kvcache", MinMBPS: 200,
		},
	}
}

// pinned sets an explicit snapshot so Artifacts exercises RENDERING rather than
// snapshot resolution, which has its own test and needs a real directory.
func pinned() Spec {
	s := flagship()
	s.ModelPath = "/hf/hub/models--RedHatAI--Qwen3.8-27B-INT4/snapshots/2fb0debc"
	return s
}

func wslRT() Runtime {
	return Runtime{
		User: "QUBE\\operator", ProxyHost: "127.0.0.1",
		StackDir: "C:/llama-swap", SeatDir: "C:/llama-swap/seat",
		VenvDir: "/root/g7/vllm-env", HFHome: "/hf",
		LMCacheOverlay: "/root/g7/lmcache-overlay",
		CacheMountSrc:  "//store.example/kvcache", CacheMountOpts: "vers=3.1.1",
		Distro: "freetoken", WSLSeatDir: "/root/g7",
	}
}

func TestFlagshipSeatValidates(t *testing.T) {
	if err := flagship().Validate("blackwell-3x16"); err != nil {
		t.Fatalf("the reference 3-card seat must validate: %v", err)
	}
}

// The device list and the parallel width are two statements of one fact. Neither
// direction may pass: too few cards and the engine refuses to start, too many and it
// loads the whole model onto the first card and silently ignores the rest.
func TestTensorParallelMustMatchTheDeviceList(t *testing.T) {
	for _, tc := range []struct {
		name, device string
		tp           int
		want         string
	}{
		{"two cards declared as one", "0,2", 1, "ignore the rest"},
		{"one card declared as two", "0", 2, "refuse to start"},
		{"three cards declared as two", "0,1,2", 2, "ignore the rest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := flagship()
			s.Device, s.TensorParallel = tc.device, tc.tp
			err := s.Validate("t")
			if err == nil {
				t.Fatalf("device %q with tensor_parallel %d must be refused", tc.device, tc.tp)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal should say what the engine would do (%q); got %v", tc.want, err)
			}
		})
	}
	// And the matching pair must still pass, or the check is just a blanket refusal.
	if err := flagship().Validate("t"); err != nil {
		t.Fatalf("the matching pair must pass: %v", err)
	}
}

// fp8 KV pages restore CORRUPT through stock LMCache and nothing reports it. The
// render must refuse the pair rather than emit a seat that serves wrong context while
// looking healthy.
func TestFp8CacheServerWithoutTheOverlayIsRefused(t *testing.T) {
	r := wslRT()
	r.LMCacheOverlay = ""
	_, err := pinned().Artifacts(wslTemplatesDir(), r)
	if err == nil {
		t.Fatal("fp8 KV + cache_server without an LMCache overlay must be refused")
	}
	for _, want := range []string{"4253", "corrupt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must name %q so the reader can act on it; got %v", want, err)
		}
	}
	// The same seat WITH the overlay renders — proving the guard is about the overlay
	// and not a seat that simply cannot render.
	if _, err := pinned().Artifacts(wslTemplatesDir(), wslRT()); err != nil {
		t.Fatalf("with the overlay the same seat must render: %v", err)
	}
	// And a seat with no cache server is unaffected: fp8 in VRAM is fine, it is the
	// STORE round-trip that corrupts.
	s := pinned()
	s.CacheServer = nil
	if _, err := s.Artifacts(wslTemplatesDir(), r); err != nil {
		t.Fatalf("fp8 without a cache server must still render: %v", err)
	}
}

func TestWindowsWSLArtifactsLeaveNoTokens(t *testing.T) {
	files, err := pinned().Artifacts(wslTemplatesDir(), wslRT())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"qwen3.8-27b-vllm.env", "seat-cmd.ps1", "seat-cmdstop.ps1", "hidden.vbs", "register-seat-tasks.ps1"}
	for _, n := range want {
		body, ok := files[n]
		if !ok {
			t.Fatalf("no %s rendered; got %v", n, keys(files))
		}
		if strings.Contains(body, "__") {
			t.Errorf("%s still holds a token", n)
		}
	}
	if len(files) != len(want) {
		t.Errorf("rendered %d files, want %d: %v", len(files), len(want), keys(files))
	}
}

// The rendered env must carry the MEASURED operating point, not a plausible one. Each
// value below is what the reference box actually serves.
func TestRenderedEnvCarriesTheMeasuredOperatingPoint(t *testing.T) {
	files, err := pinned().Artifacts(wslTemplatesDir(), wslRT())
	if err != nil {
		t.Fatal(err)
	}
	env := files["qwen3.8-27b-vllm.env"]
	for _, want := range []string{
		"SEAT_DEVICES=0,2",
		"SEAT_TP=2",
		"SEAT_MAX_LEN=163840",
		"SEAT_UTIL=0.9",
		"SEAT_SEQS=32",
		"SEAT_BATCHED=3135",
		"SEAT_L1_GB=8",
		"SEAT_CHUNK=1568",
		"SEAT_MP_PORT=18796",
		"SEAT_L2_MIN_MBPS=200",
		"SEAT_L2_PRUNE_GB=45",
		"SEAT_LMCACHE_PYTHONPATH=/root/g7/lmcache-overlay",
		`SEAT_EXTRA_ARGS="--enable-auto-tool-choice --tool-call-parser qwen3_xml --reasoning-parser qwen3 --kv-cache-dtype fp8"`,
	} {
		if !strings.Contains(env, want) {
			t.Errorf("rendered env is missing %q", want)
		}
	}
	// The L2 line must be parseable JSON inside single quotes, or the wrapper hands
	// LMCache a string it rejects at seat start.
	line := ""
	for _, l := range strings.Split(env, "\n") {
		if strings.HasPrefix(l, "SEAT_L2=") {
			line = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(l), "SEAT_L2='"), "'")
		}
	}
	if line == "" {
		t.Fatal("no SEAT_L2 line rendered")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("SEAT_L2 is not valid JSON (%v): %s", err, line)
	}
	if got["type"] != "fs_native" || got["base_path"] != "/mnt/kvcache/lmcache-seat-tp2-fp8" {
		t.Errorf("SEAT_L2 does not describe the measured store: %v", got)
	}
	if got["max_capacity_gb"] != float64(45) {
		t.Errorf("SEAT_L2 must carry the running server's cap; got %v", got["max_capacity_gb"])
	}
}

// A Windows seat's llama-swap entry must drive the PowerShell stubs, because
// llama-swap runs as SYSTEM and cannot start a per-user WSL distro itself.
func TestWindowsEntryDrivesTheScheduledTaskStubs(t *testing.T) {
	e := flagship().Entry(wslRT())
	for _, want := range []string{
		"powershell.exe", "seat-cmd.ps1 qwen3.8-27b-vllm", "seat-cmdstop.ps1 qwen3.8-27b-vllm",
		"ttl: 300", "concurrencyLimit: 32",
	} {
		if !strings.Contains(e, want) {
			t.Errorf("entry missing %q:\n%s", want, e)
		}
	}
	// The Linux wrappers must NOT appear — that pairing produced a seat that never started.
	if strings.Contains(e, "vllm-seat-cmd.sh") {
		t.Errorf("windows seat must not reference the Linux wrapper:\n%s", e)
	}
	// The Linux launch shape must still emit the shell wrappers, or this change broke it.
	l := ref().Entry(rt())
	if !strings.Contains(l, "vllm-seat-cmd.sh") || strings.Contains(l, "powershell") {
		t.Errorf("linux seat must keep the shell wrappers:\n%s", l)
	}
}

// The harness's kv_cache_server block and the seat env are two descriptions of one
// store. Deriving both from the seat is what keeps them from drifting.
func TestConfigBlockDerivesTheHarnessHalf(t *testing.T) {
	b := flagship().ConfigBlock()
	if b == nil {
		t.Fatal("a seat with a cache server must derive a config block")
	}
	for k, want := range map[string]any{
		"enabled": true, "store": "fs_native", "chunk_size": 1568,
		"key_prefix": "qube-seat-tp2-fp8", "seat": "qwen3.8-27b-vllm", "l1_staging_gb": 8,
	} {
		if b[k] != want {
			t.Errorf("config block %s = %v, want %v", k, b[k], want)
		}
	}
	if got := flagship().Bindings()["kv_cache_server"]; got == nil {
		t.Error("Bindings must carry the cache server so one declaration feeds both")
	}
	// A seat without a store derives no block, rather than an empty enabled one.
	s := flagship()
	s.CacheServer = nil
	if s.ConfigBlock() != nil {
		t.Error("a seat with no cache server must derive no block")
	}
	if _, ok := s.Bindings()["kv_cache_server"]; ok {
		t.Error("Bindings must not carry a cache server the seat does not have")
	}
}

func TestCacheServerValidateRefusesUnworkableBindings(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*CacheServer)
		want string
	}{
		{"host:port for fs_native", func(c *CacheServer) { c.Address = "192.0.2.10:18799" }, "absolute mount path"},
		{"no chunk size", func(c *CacheServer) { c.ChunkSize = 0 }, "unified block size"},
		{"no namespace", func(c *CacheServer) { c.KeyPrefix = "" }, "never a shared constant"},
		{"unknown store", func(c *CacheServer) { c.Store = "memcached" }, "not supported"},
		{"prune below the cap", func(c *CacheServer) { c.PruneGB = 10 }, "immediately exceeds"},
		{"store outside the mount", func(c *CacheServer) { c.Address = "/var/lib/kv" }, "not under mount_dir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := *flagship().CacheServer
			tc.mut(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("%s must be refused", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal should say %q; got %v", tc.want, err)
			}
		})
	}
	if err := flagship().CacheServer.Validate(); err != nil {
		t.Fatalf("the measured binding must pass: %v", err)
	}
}
