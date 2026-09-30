package vllmseat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// extraSeat is a tier's SECOND vLLM seat: an on-demand digest seat that shares the
// card with the tier's agent-lane seat but never binds `agent_model`. The numbers are
// the ampere-16 reference box's measured 35B-A3B seat (register A-100): 32,768 at
// util 0.90, eight in flight, the Qwen3.6 tool parser.
func extraSeat() Spec {
	return Spec{
		ID:                   "qwen36-35b-a3b-gsq-vllm",
		Aliases:              []string{"a2-pool-35b", "qwen36-35b-gsq"},
		Unit:                 "vllm-35b-seat",
		Port:                 18797,
		Device:               "0",
		ModelRepo:            "hub/models--ISTA-DASLab--Qwen3.6-35B-A3B-2Bit-GSQ",
		MaxModelLen:          32768,
		GPUMemoryUtilization: 0.9,
		MaxNumSeqs:           8,
		MaxBatchedTokens:     4096,
		KVCacheDtype:         "fp8_e5m2",
		ToolCallParser:       "qwen3_coder",
		ReasoningParser:      "qwen3",
	}
}

// A seat the tier gave no store derives an explicit storeless opt-out, and until now
// that opt-out carried ONE generic sentence. The measured reason (why LMCache's MP
// tier cannot run beside the resident embedder on this card) lived only in the
// reference box's hand-edited config, so a fresh install lost it. The reason is seat
// data now, and it must reach the binding verbatim.
func TestStorelessReasonRidesTheDerivedBinding(t *testing.T) {
	s := ref()
	generic := s.ConfigBinding()["reason"]
	if generic == "" || generic == nil {
		t.Fatal("the derived storeless binding carries no reason at all")
	}
	s.StorelessReason = "MEASURED 2026-09-18 (register B-01): the MP tier's own CUDA context OOMs the engine at util 0.90"
	b := s.ConfigBinding()
	if b["storeless"] != true || b["seat"] != s.ID {
		t.Fatalf("binding = %v, want a storeless opt-out naming the seat", b)
	}
	if b["reason"] != s.StorelessReason {
		t.Fatalf("binding reason = %q, want the seat's measured reason %q", b["reason"], s.StorelessReason)
	}
	if b["reason"] == generic {
		t.Fatal("the measured reason did not replace the generic sentence")
	}
	// Whitespace around a measured sentence is not part of the sentence.
	s.StorelessReason = "  " + s.StorelessReason + "\n"
	if got := s.ConfigBinding()["reason"]; got != strings.TrimSpace(s.StorelessReason) {
		t.Fatalf("reason %q is not trimmed", got)
	}
}

// A seat is bound to a store or it is storeless; declaring both is a config that
// reads as a deliberate opt-out while the render still builds a store on disk.
func TestStorelessReasonAndACacheServerContradict(t *testing.T) {
	s := ref()
	s.CacheServer = &CacheServer{Address: "/mnt/kvcache/x", ChunkSize: 1568, KeyPrefix: "x-gen1"}
	s.StorelessReason = "measured: no store"
	for name, err := range map[string]error{"lane": s.Validate("t"), "extra": func() error {
		e := extraSeat()
		e.CacheServer, e.StorelessReason = s.CacheServer, s.StorelessReason
		return e.ValidateExtra("t")
	}()} {
		if err == nil || !strings.Contains(err.Error(), "storeless_reason") {
			t.Errorf("%s seat: a cache_server AND a storeless_reason must be refused naming storeless_reason, got %v", name, err)
		}
	}
}

// Validate was written for THE agent lane: it demands a llama.cpp fallback so a box
// without the venv still has an agent seat. An extra seat backs a layer that is
// simply absent when the seat is, so it has no fallback to name — and inventing one
// would seed an `agent_model` binding that this seat must never own.
func TestExtraSeatValidatesWithoutALaneFallback(t *testing.T) {
	s := extraSeat()
	if err := s.ValidateExtra("ampere-16"); err != nil {
		t.Fatalf("an extra seat with no fallback must validate: %v", err)
	}
	if err := s.Validate("ampere-16"); err == nil || !strings.Contains(err.Error(), "fallback_agent_model") {
		t.Fatalf("the LANE validator must still demand a fallback, got %v", err)
	}
	// Everything else is shared: an extra seat is refused for the same authoring mistakes.
	for name, mutate := range map[string]func(*Spec){
		"no id":             func(s *Spec) { s.ID = "" },
		"no tool parser":    func(s *Spec) { s.ToolCallParser = "" },
		"util out of range": func(s *Spec) { s.GPUMemoryUtilization = 1.4 },
		"no concurrency":    func(s *Spec) { s.MaxNumSeqs = 0 },
		"absolute repo":     func(s *Spec) { s.ModelRepo = "/hf/hub/models--x--y" },
		"ttl never unloads": func(s *Spec) { s.TTLSeconds = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			e := extraSeat()
			mutate(&e)
			if err := e.ValidateExtra("ampere-16"); err == nil {
				t.Errorf("%s: accepted an extra seat that cannot render correctly", name)
			}
		})
	}
}

// A lane field on an extra seat would be written and never read: the seat never
// binds agent_model, so an agent_max_tokens or a fallback on it is a setting that
// reads as a decision and does nothing.
func TestExtraSeatRefusesLaneFields(t *testing.T) {
	temp := 0.7
	for field, mutate := range map[string]func(*Spec){
		"fallback_agent_model":      func(s *Spec) { s.Fallback = "qwen3.5-4b-agent" },
		"fallback_agent_ctx_tokens": func(s *Spec) { s.FallbackCtx = 131072 },
		"fallback_agent_device":     func(s *Spec) { s.FallbackDevice = "0" },
		"agent_ctx_tokens":          func(s *Spec) { s.AgentCtxTokens = 32768 },
		"agent_max_tokens":          func(s *Spec) { s.AgentMaxTokens = 4096 },
		"agent_thinking":            func(s *Spec) { s.AgentThinking = "off" },
		"agent_sampling":            func(s *Spec) { s.AgentSampling = &core.AgentSampling{Temperature: &temp} },
		"agent_timeout_sec":         func(s *Spec) { s.AgentTimeoutSec = 900 },
		"agent_seat_tok_s":          func(s *Spec) { s.AgentSeatTokS = 7.17 },
	} {
		t.Run(field, func(t *testing.T) {
			e := extraSeat()
			mutate(&e)
			err := e.ValidateExtra("ampere-16")
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Errorf("a %s on an extra seat must be refused naming it, got %v", field, err)
			}
		})
	}
}

// Two seats rendered into one seat directory would overwrite each other's wrappers:
// the primary's `vllm-seat-cmd.sh` has ITS unit baked in, so a second entry pointing
// at the same path would start the wrong engine. An extra seat's entry names wrappers
// after its own unit (the reference box's own convention), and the primary's path —
// which every deployed config already carries — does not move.
func TestExtraEntryUsesPerUnitWrappers(t *testing.T) {
	r := rt()
	primary := ref().Entry(r)
	if !strings.Contains(primary, "cmd: /srv/llama-swap/seat/vllm-seat-cmd.sh\n") ||
		!strings.Contains(primary, "cmdStop: /srv/llama-swap/seat/vllm-seat-cmdstop.sh\n") {
		t.Fatalf("the primary seat's wrapper paths moved:\n%s", primary)
	}
	e := extraSeat()
	got := e.ExtraEntry(r)
	for _, want := range []string{
		"  qwen36-35b-a3b-gsq-vllm:\n",
		"aliases: [a2-pool-35b, qwen36-35b-gsq]\n",
		"cmd: /srv/llama-swap/seat/vllm-35b-seat-cmd.sh\n",
		"cmdStop: /srv/llama-swap/seat/vllm-35b-seat-cmdstop.sh\n",
		"proxy: http://192.0.2.10:18797\n",
		"useModelName: \"qwen36-35b-a3b-gsq-vllm\"\n",
		"ttl: 300\n",
		"concurrencyLimit: 8\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the extra seat's entry is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "vllm-seat-cmd.sh") {
		t.Errorf("the extra seat's entry names the primary's wrapper:\n%s", got)
	}
	// Same shape as the primary everywhere the wrappers do not enter: swap the two
	// wrapper lines out and the entries are the same template.
	norm := func(s string) string {
		var keep []string
		for _, l := range strings.Split(s, "\n") {
			if !strings.Contains(l, "cmd:") && !strings.Contains(l, "cmdStop:") {
				keep = append(keep, l)
			}
		}
		return strings.Join(keep, "\n")
	}
	if !strings.Contains(norm(got), "checkEndpoint: /health") || !strings.Contains(norm(primary), "checkEndpoint: /health") {
		t.Error("an entry lost its health check")
	}
}

func writeScript(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// An extra seat's wrapper scripts are not installer output: its launch line carries flags the
// shared unit template cannot express, so its unit, wrappers and polkit rule are the operator's
// step. llama-swap does not check that an entry's `cmd` exists when it loads its config, so a
// seat advertised without them is listed and fails only when a contract asks for it. DetectExtra
// is Detect plus the two files the seat's own entry runs: a box advertises the seat (rosters it,
// binds it, seeds the layer that names it) only once the operator has put them there.
func TestDetectExtraNeedsTheWrappersTheInstallerDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	slash := filepath.ToSlash(root)
	r := rt()
	r.VenvDir, r.HFHome, r.SeatDir = slash+"/vllm-env", slash+"/hf", slash+"/seat"
	s := extraSeat()

	if ok, _ := s.DetectExtra(r); ok {
		t.Fatal("a box with nothing installed runs the seat")
	}
	// The venv and the weights: everything Detect asks for.
	writeScript(t, filepath.Join(root, "vllm-env", "bin", "vllm"))
	if err := os.MkdirAll(filepath.Join(root, "hf", s.ModelRepo, "snapshots", "abc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, why := s.Detect(r); !ok {
		t.Fatalf("the control box must pass Detect: %s", why)
	}
	cmd, stop := slash+"/seat/vllm-35b-seat-cmd.sh", slash+"/seat/vllm-35b-seat-cmdstop.sh"

	ok, why := s.DetectExtra(r)
	if ok || !strings.Contains(why, cmd) || !strings.Contains(why, "by hand") {
		t.Fatalf("the venv and the weights without the wrappers must not run the seat, and the reason must name the missing file and say what to do; got %v %q", ok, why)
	}
	writeScript(t, filepath.Join(root, "seat", "vllm-35b-seat-cmd.sh"))
	if ok, why := s.DetectExtra(r); ok || !strings.Contains(why, stop) {
		t.Fatalf("one of the two wrappers is not enough; got %v %q", ok, why)
	}
	// A directory of that name is not a script.
	if err := os.MkdirAll(filepath.Join(root, "seat", "vllm-35b-seat-cmdstop.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.DetectExtra(r); ok {
		t.Fatal("a directory stood in for a wrapper script")
	}
	if err := os.Remove(filepath.Join(root, "seat", "vllm-35b-seat-cmdstop.sh")); err != nil {
		t.Fatal(err)
	}
	writeScript(t, filepath.Join(root, "seat", "vllm-35b-seat-cmdstop.sh"))
	if ok, why := s.DetectExtra(r); !ok {
		t.Fatalf("both wrappers on the box: %s", why)
	}
	// No seat directory to look in is a refusal that says so, not a stat of the filesystem root.
	noDir := r
	noDir.SeatDir = ""
	if ok, why := s.DetectExtra(noDir); ok || !strings.Contains(why, "seat directory") {
		t.Fatalf("an empty seat directory must be refused, saying so; got %v %q", ok, why)
	}
	// The wrappers alone are not the seat: the prerequisites Detect checks still apply.
	if err := os.RemoveAll(filepath.Join(root, "hf")); err != nil {
		t.Fatal(err)
	}
	if ok, why := s.DetectExtra(r); ok || !strings.Contains(why, "snapshot") {
		t.Fatalf("wrappers without the weights must not run the seat and must say the weights are missing; got %v %q", ok, why)
	}
}

// ExtraWrapperPaths is what DetectExtra looks for, and ExtraEntry is what llama-swap runs: the two
// must name the same files, or the box checks one pair of scripts and starts another.
func TestExtraWrapperPathsAreTheFilesTheEntryRuns(t *testing.T) {
	r := rt()
	s := extraSeat()
	paths := s.ExtraWrapperPaths(r)
	entry := s.ExtraEntry(r)
	if len(paths) != 2 || !strings.Contains(entry, "cmd: "+paths[0]+"\n") || !strings.Contains(entry, "cmdStop: "+paths[1]+"\n") {
		t.Fatalf("ExtraWrapperPaths = %v, but the entry runs:\n%s", paths, entry)
	}
	// The WSL launch drives one shared PowerShell stub named by the seat id: no per-seat script
	// exists for an operator to install, so nothing is required and Detect alone decides.
	w := s
	w.Launch = LaunchWindowsWSL
	if got := w.ExtraWrapperPaths(r); got != nil {
		t.Errorf("a WSL-launched seat has no per-seat wrapper, got %v", got)
	}
}
