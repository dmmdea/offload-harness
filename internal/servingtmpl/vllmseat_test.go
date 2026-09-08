package servingtmpl

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

func vllmSpec() *vllmseat.Spec {
	return &vllmseat.Spec{
		ID:                   "qwen3.5-4b-vllm",
		Aliases:              []string{"a2-pool", "agent-pool-a2"},
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
	}
}

func vllmRuntime() vllmseat.Runtime {
	return vllmseat.Runtime{
		User: "svcuser", ProxyHost: "192.0.2.10", // RFC 5737 TEST-NET-1: a documentation address standing in for the box's tailnet IPv4
		StackDir: "/srv/offload-stack", SeatDir: "/srv/llama-swap/seat",
		VenvDir: "/srv/offload-stack/vllm-env", HFHome: "/hf",
	}
}

// The overwhelming majority of tiers declare no vLLM seat, and their output must be
// byte-identical to a build that had no vLLM support at all. This is the same line
// TestNoSeatsChangesNothing holds for media seats.
func TestNoVLLMSeatChangesNothing(t *testing.T) {
	base, err := Render(linuxCUDA(t), params())
	if err != nil {
		t.Fatal(err)
	}
	p := params()
	p.VLLMSeat = nil
	p.VLLMRuntime = vllmRuntime() // set but unused: a runtime alone must change nothing
	withRuntime, err := Render(linuxCUDA(t), p)
	if err != nil {
		t.Fatal(err)
	}
	if base != withRuntime {
		t.Error("a VLLMRuntime with no VLLMSeat changed the rendered config")
	}
	for _, unwanted := range []string{"vllm", "cmdStop", "useModelName", "concurrencyLimit"} {
		if strings.Contains(strings.ToLower(base), strings.ToLower(unwanted)) {
			t.Errorf("a seatless render leaked %q into the config", unwanted)
		}
	}
}

// The seat must land as a RESIDENT matrix member: it IS the agent lane, so an
// ordinary chat request must never be able to evict it and pay its 125-250 s reload.
func TestVLLMSeatRendersAsAResidentMatrixMember(t *testing.T) {
	p := params()
	p.VLLMSeat = vllmSpec()
	p.VLLMRuntime = vllmRuntime()
	out, err := Render(linuxCUDA(t), p)
	if err != nil {
		t.Fatal(err)
	}

	// It must parse — a seat block that breaks the document is worse than no seat.
	var doc struct {
		HealthCheckTimeout int `yaml:"healthCheckTimeout"`
		Models             map[string]struct {
			Cmd              string   `yaml:"cmd"`
			CmdStop          string   `yaml:"cmdStop"`
			Proxy            string   `yaml:"proxy"`
			UseModelName     string   `yaml:"useModelName"`
			ConcurrencyLimit int      `yaml:"concurrencyLimit"`
			Aliases          []string `yaml:"aliases"`
			TTL              *int     `yaml:"ttl"`
		} `yaml:"models"`
		Matrix struct {
			Vars map[string]string `yaml:"vars"`
			Sets map[string]string `yaml:"sets"`
		} `yaml:"matrix"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("a vLLM seat made the config unparseable: %v", err)
	}

	m, ok := doc.Models["qwen3.5-4b-vllm"]
	if !ok {
		t.Fatalf("no vLLM seat in the rendered models (got %v)", modelIDs(doc.Models))
	}
	if m.Cmd != "/srv/llama-swap/seat/vllm-seat-cmd.sh" || m.CmdStop != "/srv/llama-swap/seat/vllm-seat-cmdstop.sh" {
		t.Errorf("the seat must drive the unit through the wrappers, got cmd=%q cmdStop=%q", m.Cmd, m.CmdStop)
	}
	if m.Proxy != "http://192.0.2.10:18797" {
		t.Errorf("proxy must be the LITERAL address the engine binds, got %q", m.Proxy)
	}
	if m.UseModelName != "qwen3.5-4b-vllm" {
		t.Errorf("useModelName must rewrite every alias to the served id (vLLM 404s the rest), got %q", m.UseModelName)
	}
	if m.ConcurrencyLimit != 32 {
		t.Errorf("concurrencyLimit must equal --max-num-seqs or llama-swap's default 10 would 429 the streams, got %d", m.ConcurrencyLimit)
	}
	if m.TTL == nil || *m.TTL != 0 {
		t.Errorf("ttl must be 0 — the agent lane must not idle out and pay a cold reload, got %v", m.TTL)
	}

	// Residency: a var pointing at the seat, joined into the residents set.
	var id string
	for k, v := range doc.Matrix.Vars {
		if v == "qwen3.5-4b-vllm" {
			id = k
		}
	}
	if id == "" {
		t.Fatalf("the seat has no matrix var (vars: %v)", doc.Matrix.Vars)
	}
	if len(id) > 8 {
		t.Errorf("matrix var %q exceeds llama-swap's 8-character key limit", id)
	}
	residents, ok := doc.Matrix.Sets["residents"]
	if !ok {
		t.Fatalf("no residents set (sets: %v)", doc.Matrix.Sets)
	}
	if !strings.Contains(residents, id) {
		t.Errorf("the seat is not in the residents set (%q) — something could evict the agent lane", residents)
	}
	// And it must NOT have been joined as a swappable alternative.
	for name, expr := range doc.Matrix.Sets {
		if name == "residents" {
			continue
		}
		if strings.Contains(expr, "| "+id) {
			t.Errorf("set %q joins the agent seat as an ALTERNATIVE (%q) — an ordinary request could evict it", name, expr)
		}
	}

	// The vLLM cold load is 125-250 s; the default 120 kills the attach mid-load.
	if doc.HealthCheckTimeout < 480 {
		t.Errorf("healthCheckTimeout is %d — the vLLM cold load (125-250 s) needs 480", doc.HealthCheckTimeout)
	}
}

// A template that cannot place a resident cannot host the agent lane, and must say so
// by name rather than quietly rendering the seat where anything can unload it.
func TestVLLMSeatRefusesATemplateThatPlacesNoResident(t *testing.T) {
	tmpl := strings.Replace(linuxCUDA(t), "# offload-seats: swappable resident", "# offload-seats: swappable", 1)
	p := params()
	p.VLLMSeat = vllmSpec()
	p.VLLMRuntime = vllmRuntime()
	_, err := Render(tmpl, p)
	if err == nil {
		t.Fatal("rendered an agent seat into a template that cannot place a resident")
	}
	if !strings.Contains(err.Error(), "RESIDENT") {
		t.Errorf("the refusal must say why, got %v", err)
	}
}

func modelIDs(m map[string]struct {
	Cmd              string   `yaml:"cmd"`
	CmdStop          string   `yaml:"cmdStop"`
	Proxy            string   `yaml:"proxy"`
	UseModelName     string   `yaml:"useModelName"`
	ConcurrencyLimit int      `yaml:"concurrencyLimit"`
	Aliases          []string `yaml:"aliases"`
	TTL              *int     `yaml:"ttl"`
}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
