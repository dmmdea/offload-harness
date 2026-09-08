package vllmseat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// templatesDir is the reference pattern the renderer substitutes into.
func templatesDir() string {
	return filepath.Join("..", "..", "setup", "templates", "vllm-seat", "linux-systemd")
}

// ref is the reference deployment: Qwen3.5-4B w4a16 on the NVIDIA A2 16 GB
// (`ampere-16`, harness 0.113.19, 2026-09-06) — 8/8 digests at a 45 s median against
// the llama.cpp 4B seat's 159 s, at 4x the window.
func ref() Spec {
	return Spec{
		ID:                   "qwen3.5-4b-vllm",
		Aliases:              []string{"a2-pool", "agent-pool-a2", "qwen35-4b-vllm"},
		Unit:                 "vllm-a2-seat",
		Port:                 18797,
		ModelRepo:            "hub/models--RedHatAI--Qwen3.5-4B-quantized.w4a16",
		MaxModelLen:          131072,
		GPUMemoryUtilization: 0.65,
		MaxNumSeqs:           32,
		MaxBatchedTokens:     4096,
		KVCacheDtype:         "fp8_e5m2",
		ToolCallParser:       "qwen3_xml",
		ReasoningParser:      "qwen3",
		Fallback:             "qwen3.5-4b-agent",
		FallbackCtx:          32768,
		AgentCtxTokens:       131072,
	}
}

func rt() Runtime {
	return Runtime{
		User: "svcuser", ProxyHost: "192.0.2.10", // RFC 5737 TEST-NET-1: a documentation address standing in for the box's tailnet IPv4
		StackDir: "/srv/offload-stack", SeatDir: "/srv/llama-swap/seat",
		VenvDir: "/srv/offload-stack/vllm-env", HFHome: "/hf",
	}
}

func TestReferenceSpecValidates(t *testing.T) {
	if err := ref().Validate("ampere-16"); err != nil {
		t.Fatalf("the reference deployment's own spec must validate: %v", err)
	}
	if err := rt().Validate(); err != nil {
		t.Fatalf("reference runtime: %v", err)
	}
}

// A half-specified seat must fail here, in a test over the committed tier table,
// rather than on someone's box where the symptom is a unit that will not start.
func TestValidateRefusesIncompleteSpecs(t *testing.T) {
	for name, mutate := range map[string]func(*Spec){
		"no id":              func(s *Spec) { s.ID = "" },
		"id with a comma":    func(s *Spec) { s.ID = "a,b" },
		"unit with suffix":   func(s *Spec) { s.Unit = "vllm-a2-seat.service" },
		"no tool parser":     func(s *Spec) { s.ToolCallParser = "" },
		"no reasoning parse": func(s *Spec) { s.ReasoningParser = "" },
		"no fallback":        func(s *Spec) { s.Fallback = "" },
		"fallback is self":   func(s *Spec) { s.Fallback = s.ID },
		"util out of range":  func(s *Spec) { s.GPUMemoryUtilization = 1.4 },
		"no concurrency":     func(s *Spec) { s.MaxNumSeqs = 0 },
		"absolute repo":      func(s *Spec) { s.ModelRepo = "/hf/hub/models--x--y" },
	} {
		t.Run(name, func(t *testing.T) {
			s := ref()
			mutate(&s)
			if err := s.Validate("ampere-16"); err == nil {
				t.Errorf("%s: accepted a spec that cannot render correctly", name)
			}
		})
	}
}

// A tier that declares this seat must never leave a token behind: a half-rendered
// unit starts and misbehaves instead of failing loudly.
func TestArtifactsLeaveNoTokens(t *testing.T) {
	s := ref()
	s.ModelPath = "/hf/hub/models--RedHatAI--Qwen3.5-4B-quantized.w4a16/snapshots/deadbeef"
	files, err := s.Artifacts(templatesDir(), rt())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"vllm-a2-seat.service", "vllm-seat-run.sh", "vllm-seat-cmd.sh",
		"vllm-seat-cmdstop.sh", "50-llama-swap-vllm-a2-seat.rules",
	}
	for _, w := range want {
		body, ok := files[w]
		if !ok {
			t.Errorf("no rendered %s (got %v)", w, keys(files))
			continue
		}
		if strings.Contains(body, "__") {
			t.Errorf("%s still holds a token", w)
		}
		for _, stray := range []string{"<user>", "<seat id>", "<hash>"} {
			if strings.Contains(body, stray) {
				t.Errorf("%s still holds the placeholder %s", w, stray)
			}
		}
	}
	// The measured launch line has to survive substitution intact — these are the
	// flags without which the seat is a different, slower, or broken thing.
	run := files["vllm-seat-run.sh"]
	for _, must := range []string{
		"--max-model-len 131072", "--gpu-memory-utilization 0.65", "--max-num-seqs 32",
		"--max-num-batched-tokens 4096", "--kv-cache-dtype fp8_e5m2",
		"--enable-auto-tool-choice", "--tool-call-parser qwen3_xml", "--reasoning-parser qwen3",
		`--limit-mm-per-prompt '{"image":0,"video":0}'`,
		"--served-model-name qwen3.5-4b-vllm a2-pool agent-pool-a2 qwen35-4b-vllm",
		"--port 18797", "VLLM_USE_FLASHINFER_SAMPLER=0",
		"/hf/hub/models--RedHatAI--Qwen3.5-4B-quantized.w4a16/snapshots/deadbeef",
	} {
		if !strings.Contains(run, must) {
			t.Errorf("the run script lost %q", must)
		}
	}
	// polkit must be scoped to THIS unit and THIS user, or it grants more than it should.
	rules := files["50-llama-swap-vllm-a2-seat.rules"]
	if !strings.Contains(rules, `action.lookup("unit") == "vllm-a2-seat.service"`) {
		t.Error("polkit rule is not scoped to the seat's unit")
	}
	if !strings.Contains(rules, `subject.user == "svcuser"`) {
		t.Error("polkit rule is not scoped to the llama-swap user")
	}
	// The unit must point at the rendered run script and log under the stack dir.
	unit := files["vllm-a2-seat.service"]
	for _, must := range []string{
		"ExecStart=/srv/llama-swap/seat/vllm-seat-run.sh",
		"User=svcuser", "WorkingDirectory=/srv/offload-stack",
		"append:/srv/offload-stack/logs/vllm-a2-seat.log",
	} {
		if !strings.Contains(unit, must) {
			t.Errorf("the unit lost %q", must)
		}
	}
	// The wrappers must drive the seat's OWN unit — a stale name here stops somebody
	// else's seat, or nothing at all.
	if !strings.Contains(files["vllm-seat-cmd.sh"], "U=vllm-a2-seat.service") {
		t.Error("cmd wrapper does not name the seat's unit")
	}
	if !strings.Contains(files["vllm-seat-cmdstop.sh"], "systemctl stop vllm-a2-seat.service") {
		t.Error("cmdStop wrapper does not stop the seat's unit")
	}
}

// Entry is written in Go while llama-swap-entry.yaml stays the hand-install
// reference, so the two can drift. Everything EXCEPT residency must agree: the
// reference expresses residency with a legacy `groups: {persistent: true}` block and
// the renderer uses a matrix membership instead (groups' persistent:true was measured
// FAILING on the Qube, silently degrading the memory stack to dense-only).
func TestEntryMatchesTheReferenceTemplate(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(templatesDir(), "llama-swap-entry.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := string(raw)
	entry := ref().Entry(rt())
	for _, field := range []string{
		"cmd:", "cmdStop:", "proxy:", "checkEndpoint:", "useModelName:",
		"ttl:", "unloadTimeout:", "concurrencyLimit:", "aliases:",
	} {
		if !strings.Contains(tmpl, field) {
			t.Errorf("the reference template no longer declares %s — update Entry to match", field)
		}
		if !strings.Contains(entry, field) {
			t.Errorf("Entry no longer renders %s, which the reference template declares", field)
		}
	}
	// Values that must be identical to the reference, not merely present.
	for _, must := range []string{
		"checkEndpoint: /health",
		`useModelName: "qwen3.5-4b-vllm"`,
		"ttl: 0",
		"unloadTimeout: 120",
		"concurrencyLimit: 32", // must equal --max-num-seqs or llama-swap 429s the streams
		"proxy: http://192.0.2.10:18797",
	} {
		if !strings.Contains(entry, must) {
			t.Errorf("Entry lost %q", must)
		}
	}
}

// Resolve must refuse rather than guess: picking one of several revisions silently is
// how a seat comes up serving weights nobody chose.
func TestResolveRefusesZeroOrManySnapshots(t *testing.T) {
	hf := t.TempDir()
	r := rt()
	r.HFHome = hf
	s := ref()
	snaps := filepath.Join(hf, s.ModelRepo, "snapshots")

	if _, err := s.Resolve(r); err == nil {
		t.Error("resolved with no snapshots directory at all")
	}
	if err := os.MkdirAll(snaps, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(r); err == nil {
		t.Error("resolved an empty snapshots directory — the weights were never fetched")
	}
	if err := os.Mkdir(filepath.Join(snaps, "aaaa"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := s.Resolve(r)
	if err != nil {
		t.Fatalf("one snapshot must resolve: %v", err)
	}
	if !strings.HasSuffix(got.ModelPath, "snapshots/aaaa") {
		t.Errorf("resolved to %q", got.ModelPath)
	}
	if err := os.Mkdir(filepath.Join(snaps, "bbbb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(r); err == nil {
		t.Error("resolved two snapshots instead of asking the tier to pin one")
	}
	// An explicit pin still wins.
	s.ModelPath = "/hf/pinned"
	if got, err := s.Resolve(r); err != nil || got.ModelPath != "/hf/pinned" {
		t.Errorf("an explicit model_path must win: %q %v", got.ModelPath, err)
	}
}

// Detect is what keeps a box WITHOUT the hand-built venv on the working llama.cpp
// fallback instead of a unit that fails at boot.
func TestDetectFallsBackWhenPrerequisitesAreMissing(t *testing.T) {
	root := t.TempDir()
	r := rt()
	r.VenvDir = filepath.Join(root, "vllm-env")
	r.HFHome = filepath.Join(root, "hf")
	s := ref()

	ok, why := s.Detect(r)
	if ok {
		t.Fatal("detected a seat on a box with no venv")
	}
	if !strings.Contains(why, "vllm entry point") {
		t.Errorf("the reason must name the missing prerequisite, got %q", why)
	}

	if err := os.MkdirAll(filepath.Join(r.VenvDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.VenvDir, "bin", "vllm"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, why = s.Detect(r); ok {
		t.Fatal("detected a seat with a venv but no weights")
	}
	if !strings.Contains(why, "snapshot") {
		t.Errorf("the reason must name the missing weights, got %q", why)
	}

	if err := os.MkdirAll(filepath.Join(r.HFHome, s.ModelRepo, "snapshots", "abc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, why = s.Detect(r); !ok {
		t.Errorf("a box with both prerequisites must detect: %s", why)
	}
}

func TestBindingsDeriveTheAgentLane(t *testing.T) {
	s := ref()
	b := s.Bindings()
	if b["agent_model"] != "qwen3.5-4b-vllm" || b["agent_ctx_tokens"] != 131072 {
		t.Errorf("vLLM bindings: %v", b)
	}
	f := s.FallbackBindings()
	if f["agent_model"] != "qwen3.5-4b-agent" || f["agent_ctx_tokens"] != 32768 {
		t.Errorf("fallback bindings: %v", f)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
