package main

import (
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dmmdea/offload-harness/internal/hwdetect"
	"github.com/dmmdea/offload-harness/internal/mediaseat"
	"github.com/dmmdea/offload-harness/internal/servingtmpl"
	"github.com/dmmdea/offload-harness/internal/tierseed"
	"github.com/dmmdea/offload-harness/internal/visionremote"
)

// The rockchip-rk3588 tier is a board that also runs something else: an SoC whose GPU and
// NPU draw on the same 7.7 GiB as everything on it, so it has a rule no card-backed tier
// has — nothing runs on the CPU, and nothing may be rendered that does not fit. These are
// the gates that keep that true after someone edits the tier, the template or the seat.

const rk3588Tier = "rockchip-rk3588"

// rk3588Render renders the tier the way install.sh does on the board, from the embedded
// table, so the gate reads what actually ships.
func rk3588Render(t *testing.T, mut func(*renderRequest)) (renderResult, error) {
	t.Helper()
	req := renderRequest{
		TierID: rk3588Tier, RAMTier: "min", GOOS: "linux",
		LlamaBin: "/opt/offload/build/llama.cpp/build/bin", ModelsDir: "/opt/offload/models",
		Listen: "127.0.0.1:11436", Home: "/opt/offload", Threads: 4,
	}
	if mut != nil {
		mut(&req)
	}
	return deriveRender(embeddedProfiles, req)
}

// rk3588Seat is the tier's one NPU seat, read from the shipped table.
func rk3588Seat(t *testing.T) mediaseat.Seat {
	t.Helper()
	res, err := rk3588Render(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	var seats []mediaseat.Seat
	for _, s := range res.Profile.MediaSeats {
		if s.Kind == mediaseat.KindRKLLM {
			seats = append(seats, s)
		}
	}
	if len(seats) != 1 {
		t.Fatalf("the tier declares %d rkllm seats, want exactly 1 (the integrator adjusts the list after benchmarks)", len(seats))
	}
	return seats[0]
}

// TestRK3588TierRendersNoCPUInference is the operator's rule, checked on the rendered
// config rather than on the template: every model llama-swap could start is either a
// llama-server that offloads every layer to the GPU and pins the Vulkan device, or the NPU
// seat. It fails when a CPU seat, a non-offloading entry or an unpinned device appears.
func TestRK3588TierRendersNoCPUInference(t *testing.T) {
	res, err := rk3588Render(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Macros map[string]string `yaml:"macros"`
		Models map[string]struct {
			Cmd string   `yaml:"cmd"`
			Env []string `yaml:"env"`
			TTL int      `yaml:"ttl"`
		} `yaml:"models"`
	}
	if err := yaml.Unmarshal([]byte(res.Config), &cfg); err != nil {
		t.Fatalf("the rendered config is not YAML: %v", err)
	}
	seat := rk3588Seat(t)
	var ids []string
	for id, m := range cfg.Models {
		ids = append(ids, id)
		if m.TTL != 300 {
			t.Errorf("%s: ttl %d, want 300 (every model unloads after five idle minutes)", id, m.TTL)
		}
		switch {
		case strings.Contains(m.Cmd, "llama-server"):
			if !strings.Contains(m.Cmd, "--n-gpu-layers 999") {
				t.Errorf("%s: a llama-server entry that does not offload every layer runs on the CPU: %q", id, m.Cmd)
			}
			if !hasEnv(m.Env, "${vk}") {
				t.Errorf("%s: no Vulkan device pin in its env %v", id, m.Env)
			}
		case id == seat.Name:
			if !strings.Contains(m.Cmd, "--cpu-mask "+seat.EffectiveCPUMask()+" ") {
				t.Errorf("%s: the NPU seat is not started with its cpu mask: %q", id, m.Cmd)
			}
		default:
			t.Errorf("%s: an entry that is neither a GPU llama-server nor the NPU seat: %q", id, m.Cmd)
		}
	}
	sort.Strings(ids)
	if got, want := strings.Join(ids, ","), seat.Name; got != want {
		t.Errorf("rendered models = %s, want %s: nothing else fits the budget (offload-e4b, the 26B, an embedder and a reranker are not on this board)", got, want)
	}
	if got := cfg.Macros["vk"]; got != "GGML_VK_VISIBLE_DEVICES=0" {
		t.Errorf("the vk macro = %q, want the single-device Vulkan pin", got)
	}
	if vs := servingtmpl.Audit(res.Config); len(vs) != 0 {
		t.Errorf("the rendered config breaks the serving-config rules:\n%s", servingtmpl.Violations(vs))
	}
	if len(res.Profile.AltBackends) != 0 {
		t.Errorf("the tier declares alt_backends %v: a CPU seat family is withdrawn by operator order (ADR 0054 amendment)", res.Profile.AltBackends)
	}
	// And the door to a CPU family stays shut: --llama-bin-cpu must be refused by name.
	_, err = rk3588Render(t, func(r *renderRequest) { r.AltLlamaBinCPU = "/opt/offload/build/llama.cpp-cpu/bin" })
	if err == nil || !strings.Contains(err.Error(), "alt_backends") {
		t.Errorf("--llama-bin-cpu against this tier must be refused, got %v", err)
	}
}

// TestRK3588SeedBindsTheNPUSeatAndBlanksWhatItDoesNotServe pins what the tier ships to the
// harness config: the vision route belongs to the NPU seat's vision encoder (a config_seed
// may not write it, so it can only come from the seat), every cascade rung this board cannot
// serve is blanked, and the host's RAM reserve and the agent lane's off switch are seeded.
func TestRK3588SeedBindsTheNPUSeatAndBlanksWhatItDoesNotServe(t *testing.T) {
	profiles, err := tierseed.Parse(embeddedProfiles)
	if err != nil {
		t.Fatal(err)
	}
	seat := rk3588Seat(t)
	cfg := effectiveConfig(t, profiles[rk3588Tier], rk3588Tier, "linux")
	if cfg.VisionModel != seat.Name {
		t.Errorf("vision_model = %q, want the NPU seat %q (it has a vision encoder %q)", cfg.VisionModel, seat.Name, seat.VisionEncoder)
	}
	if cfg.Model != "qwen3.5-2b-npu" || cfg.TriageModel != "qwen3.5-2b-npu" {
		t.Errorf("model/triage_model = %q/%q, want the NPU seat qwen3.5-2b-npu for both: the only chat model the tier serves", cfg.Model, cfg.TriageModel)
	}
	if cfg.EscalationModel != "" || cfg.ReasoningModel != "" {
		t.Errorf("escalation/reasoning = %q/%q, want both blank: the 26B is not on this board", cfg.EscalationModel, cfg.ReasoningModel)
	}
	if cfg.STTModel != "" || cfg.OCRModel != "" {
		t.Errorf("stt/ocr = %q/%q, want unbound: whisper on the CPU is forbidden and no OCR seat exists here", cfg.STTModel, cfg.OCRModel)
	}
	if cfg.UMAReserveGiB != 3 {
		t.Errorf("uma_reserve_gib = %v, want 3: the RAM the home-automation stack keeps", cfg.UMAReserveGiB)
	}
	if cfg.FleetAgentEnabled {
		t.Error("fleet_agent_enabled is true: no agent seat has been chosen or measured on this tier")
	}
	if got := cfg.AgentPlannerModel(""); got != "qwen3.5-2b-npu" {
		t.Errorf("the agent planner falls back to %q, want the served workhorse qwen3.5-2b-npu", got)
	}
	// The seat's own settings: an 8192 window (the 16384 the model was converted for is not
	// yet measured on this board's RAM budget) and the strict CPU reservation (the A55
	// cluster) until the operator chooses otherwise.
	if seat.CtxSize != 8192 || seat.EffectiveCPUMask() != "0x0f" || seat.Residency != mediaseat.Swappable {
		t.Errorf("the NPU seat is %+v, want ctx 8192, cpu_mask 0x0f, swappable", seat)
	}
}

// TestDetectedRK3588RendersItsOwnTier is the chain the installer walks on the board, once per
// boot entry: the device tree names the SoC, the classifier names the tier, the tier renders.
// The two spellings are the vendor 6.1 kernel's and mainline's, on the same physical board.
func TestDetectedRK3588RendersItsOwnTier(t *testing.T) {
	for name, compat := range map[string]string{
		"vendor 6.1 kernel": "rockchip,rk3588s-exampleboard-5\x00rockchip,rk3588\x00",
		"mainline 7.0":      "vendor,exampleboard-5\x00rockchip,rk3588s\x00",
	} {
		facts, ok := hwdetect.DetectSoC(func(string) ([]byte, error) { return []byte(compat), nil })
		if !ok {
			t.Fatalf("%s: the device tree was not recognised", name)
		}
		facts.RAMGb = 7 // 7.7 GiB reads as 7
		v := hwdetect.Classify(facts)
		if v.Profile != rk3588Tier || v.RAMTier != "min" {
			t.Fatalf("%s: classified %s / ram tier %s, want %s / min", name, v.Profile, v.RAMTier, rk3588Tier)
		}
		res, err := rk3588Render(t, func(r *renderRequest) { r.TierID, r.RAMTier = v.Profile, v.RAMTier })
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// 1024 MiB is the min RAM tier's host prompt cache: the tier keeps the house convention.
		if !strings.Contains(res.Config, "--cache-ram 1024") {
			t.Errorf("%s: the ram tier's prompt cache did not reach the render", name)
		}
	}
}

func hasEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// TestRK3588RknpuHomeSurvivesTheProvenanceReplay: the NPU seat's launcher follows --rknpu-home,
// and the stamp records it, so auditing a config rendered with a non-default RKNPU home against
// this binary reads MATCH instead of re-rendering the default home and calling it STALE.
func TestRK3588RknpuHomeSurvivesTheProvenanceReplay(t *testing.T) {
	res, err := rk3588Render(t, func(r *renderRequest) { r.RknpuHome = "/srv/npu-elsewhere" })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Config, "/srv/npu-elsewhere/rkllm-serve.sh --model") {
		t.Fatalf("the rendered seat does not launch from the RKNPU home:\n%s", res.Config)
	}
	stamped, err := servingtmpl.Stamp(res.Config, res.Basis, stampedAtFixed())
	if err != nil {
		t.Fatal(err)
	}
	if rep := provenanceOf(stamped); rep.State != servingtmpl.StateMatch {
		t.Fatalf("a config rendered with a non-default RKNPU home does not verify: %s -- %s (keys %v)", rep.State, rep.Detail, rep.Keys)
	}
}

// TestRK3588VisionLaneIsLimitedToWhatTheRuntimeCanDo pins the 0.153.0 vision decisions on the
// SHIPPED table: the seat is started with a repeat penalty (greedy decoding at the runtime's 1.0
// looped to the token cap on VQA), declares vqa and ocr only (the runtime cannot constrain
// sampling and assess_image always sends a grammar), and the seed binds exactly that as
// vision_tasks. Each of those is one edit away from silently reverting.
func TestRK3588VisionLaneIsLimitedToWhatTheRuntimeCanDo(t *testing.T) {
	seat := rk3588Seat(t)
	if seat.RepeatPenalty == nil || *seat.RepeatPenalty != 1.1 {
		t.Errorf("the NPU seat's repeat_penalty = %v, want 1.1 (measured: 1.0 loops on VQA to the token cap)", seat.RepeatPenalty)
	}
	if got, want := strings.Join(seat.Tasks, ","), "vqa,ocr"; got != want {
		t.Errorf("the NPU seat declares tasks %q, want %q: the runtime refuses the grammar assess_image always sends", got, want)
	}
	res, err := rk3588Render(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Config, "--cpu-mask 0x0f --repeat-penalty 1.1 --served-name "+seat.Name+" ") {
		t.Errorf("the rendered NPU seat is not started with --repeat-penalty 1.1:\n%s", res.Config)
	}
	profiles, err := tierseed.Parse(embeddedProfiles)
	if err != nil {
		t.Fatal(err)
	}
	cfg := effectiveConfig(t, profiles[rk3588Tier], rk3588Tier, "linux")
	if got, want := strings.Join(cfg.VisionTasks, ","), "vqa,ocr"; got != want {
		t.Errorf("the seed's vision_tasks = %q, want %q (bound from the seat)", got, want)
	}
}

// TestRK3588SeedLimitsFitAOneGenerationNPU: the node default of four concurrent jobs would queue
// three behind the NPU's single generation, where their wait counts against the delegator's wall;
// the timeout must stay below the delegator's fleet vision budget or the delegator gives up first.
func TestRK3588SeedLimitsFitAOneGenerationNPU(t *testing.T) {
	profiles, err := tierseed.Parse(embeddedProfiles)
	if err != nil {
		t.Fatal(err)
	}
	cfg := effectiveConfig(t, profiles[rk3588Tier], rk3588Tier, "linux")
	if got := cfg.FleetConcurrencyLimit(); got != 1 {
		t.Errorf("fleet concurrency = %d, want 1: the NPU runs one generation at a time", got)
	}
	if got := cfg.FleetQueueLimit(); got != 2 {
		t.Errorf("fleet queue depth = %d, want 2 (the default, twice the concurrency; one running, one waiting): the seed must not set fleet_max_queue_depth", got)
	}
	if got, budget := time.Duration(cfg.RequestTimeoutSec)*time.Second, visionremote.Budget; got != 240*time.Second || got >= budget {
		t.Errorf("request_timeout_sec = %v, want 240s and below the delegator's %v fleet vision budget", got, budget)
	}
	if cfg.MaxInputChars != 8000 || cfg.OCRMaxTokens != 512 {
		t.Errorf("max_input_chars/ocr_max_tokens = %d/%d, want 8000/512", cfg.MaxInputChars, cfg.OCRMaxTokens)
	}
	if _, set := profiles[rk3588Tier].ConfigSeed["fleet_max_queue_depth"]; set {
		t.Error("the tier sets fleet_max_queue_depth: leave it at its default (twice the concurrency)")
	}
}

// TestRK3588TextDoorShipsDark pins the 0.154.0 text-door decisions on the SHIPPED table: the seat
// is written into unconstrained_seats (so the node's own pipeline stops sending a grammar its
// runtime refuses) while NO text task is declared, so text_tasks is empty and the fleet text lane is
// not advertised. A later data-only change that adds classify/extract to the seat's tasks is the
// one edit that opens the lane, and it must be made on purpose, after measured data passes.
func TestRK3588TextDoorShipsDark(t *testing.T) {
	seat := rk3588Seat(t)
	for _, task := range seat.Tasks {
		if task == "classify" || task == "extract" || task == "summarize" || task == "triage" {
			t.Errorf("the NPU seat declares text task %q: the text lane ships dark until measured data passes (>=30 cases/lane, >=90%% correct, zero off-schema accepted)", task)
		}
	}
	profiles, err := tierseed.Parse(embeddedProfiles)
	if err != nil {
		t.Fatal(err)
	}
	cfg := effectiveConfig(t, profiles[rk3588Tier], rk3588Tier, "linux")
	if !cfg.DeclaresUnconstrainedSeat(seat.Name) {
		t.Errorf("unconstrained_seats = %v, want it to name the NPU seat %q (bound from the seat)", cfg.UnconstrainedSeats, seat.Name)
	}
	// What stops the node's own pipeline sending the seat a grammar is that the cascade rungs it
	// actually calls are declared, not merely that the seat name is listed somewhere.
	if !cfg.DeclaresUnconstrainedSeat(cfg.Model) || !cfg.DeclaresUnconstrainedSeat(cfg.TriageModel) {
		t.Errorf("the cascade rungs model %q and triage_model %q must both be declared unconstrained (unconstrained_seats %v): otherwise the node's pipeline sends the NPU a grammar it refuses",
			cfg.Model, cfg.TriageModel, cfg.UnconstrainedSeats)
	}
	if len(cfg.TextTasks) != 0 {
		t.Errorf("text_tasks = %v, want none: the fleet text lane is dark", cfg.TextTasks)
	}
}
