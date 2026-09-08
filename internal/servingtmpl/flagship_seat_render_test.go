package servingtmpl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// This gate exists because the tier table and the render tests could BOTH be green while
// the flagship tier's seat never actually landed in its own template.
//
// The seat renders only when Detect finds the hand-built venv and the weights, which is
// false on CI and false on every box but one — so setup/render.tests.ps1 exercises the
// FALLBACK path for blackwell-3x16 and says nothing about the seat. Meanwhile
// TestEveryDeclaredVLLMSeatValidates checks the tier's JSON in isolation, never rendering
// it. The one thing neither covers is the join: does the declared seat, put through the
// template that tier actually renders, produce a config llama-swap can load?
//
// insertVLLMSeat refuses a template whose `# offload-seats:` directive does not place a
// resident, and blackwell-3x16 renders llama-swap.win-triple-blackwell.yaml rather than the
// linux template every other seat test uses. Nothing before this asserted that pairing
// works, so a tier could ship a seat that fails to render on the only box that can run it.
func TestFlagshipSeatRendersIntoItsOwnTemplate(t *testing.T) {
	tmplPath := filepath.Join("..", "..", "setup", "templates", "llama-swap.win-triple-blackwell.yaml")
	raw, err := os.ReadFile(tmplPath)
	if err != nil {
		t.Fatal(err)
	}

	// The seat under test is the COMMITTED one, read from the tier table — not a fixture.
	// A fixture would pass while the shipped declaration was broken, which is the exact
	// class of failure this file exists to catch.
	profiles, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			VLLMSeat *vllmseat.Spec `json:"vllm_seat"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(profiles, &doc); err != nil {
		t.Fatal(err)
	}
	spec := doc.Profiles["blackwell-3x16"].VLLMSeat
	if spec == nil {
		t.Fatal("blackwell-3x16 declares no vllm_seat — the tier whose reference box IS the " +
			"3-card workstation must seed the seat that box serves")
	}

	p := params()
	p.GOOS = "windows"
	p.VLLMSeat = spec
	p.VLLMRuntime = vllmseat.Runtime{
		User: "BOX\\operator", ProxyHost: "127.0.0.1",
		StackDir: "C:/llama-swap", SeatDir: "C:/llama-swap/seat",
		VenvDir: "/root/g7/vllm-env", HFHome: "/hf",
		LMCacheOverlay: "/root/g7/lmcache-overlay",
		Distro:         "freetoken", WSLSeatDir: "/root/g7",
	}

	out, err := Render(string(raw), p)
	if err != nil {
		t.Fatalf("the flagship tier's own template refused its own seat: %v", err)
	}
	if strings.Contains(out, "__") {
		t.Error("rendered config still holds a token")
	}

	var cfg struct {
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
	if err := yaml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("the seat made the flagship config unparseable: %v", err)
	}

	m, ok := cfg.Models[spec.ID]
	if !ok {
		t.Fatalf("the seat %q is not in the rendered models", spec.ID)
	}
	// llama-swap runs as SYSTEM on this box and cannot start a per-user WSL distro, so
	// the entry must drive the PowerShell stubs, never a bare wsl.exe.
	for name, got := range map[string]string{"cmd": m.Cmd, "cmdStop": m.CmdStop} {
		if !strings.Contains(got, "powershell.exe") || !strings.Contains(got, spec.ID) {
			t.Errorf("%s must drive the PowerShell stub with the seat name, got %q", name, got)
		}
	}
	if m.UseModelName != spec.ID {
		t.Errorf("useModelName must rewrite every alias to the served id (vLLM 404s the rest), got %q", m.UseModelName)
	}
	if m.ConcurrencyLimit != spec.MaxNumSeqs {
		t.Errorf("concurrencyLimit %d must equal max_num_seqs %d, or llama-swap 429s the streams "+
			"the seat was provisioned for", m.ConcurrencyLimit, spec.MaxNumSeqs)
	}
	// The check above compares the render against the DECLARATION, so it cannot fail when
	// the declaration itself moves — a mutation run proved exactly that: changing
	// max_num_seqs in the tier left it green. These are the MEASURED absolutes, which a
	// change has to justify rather than silently carry along.
	if m.ConcurrencyLimit != 32 {
		t.Errorf("the seat's measured concurrency is 32 streams (fan-out 305 tok/s at c32); "+
			"got %d — if the operating point genuinely moved, move this number with the evidence",
			m.ConcurrencyLimit)
	}
	if spec.MaxModelLen != 163840 {
		t.Errorf("the seat's measured window is 163,840 (fp8 KV, pool 177,766 tokens, ~1.08x "+
			"margin); got %d", spec.MaxModelLen)
	}
	// `agent-pool` is not decoration: it is the name the harness config binds
	// (`agent_model: agent-pool`), so dropping it from the tier silently unbinds the agent
	// lane on every fresh install while every other check stays green.
	hasPool := false
	for _, a := range spec.Aliases {
		if a == "agent-pool" {
			hasPool = true
		}
	}
	if !hasPool {
		t.Errorf("the seat must declare the alias \"agent-pool\" — it is what the harness "+
			"binds agent_model to; got %v", spec.Aliases)
	}
	if m.TTL == nil || *m.TTL != 300 {
		t.Errorf("the seat must idle out like every other seat; got ttl %v", m.TTL)
	}
	// The seat's cold load is 125-250 s. A healthCheckTimeout below that kills the attach
	// mid-load and cmdStop then stops an engine that was starting normally.
	if cfg.HealthCheckTimeout < spec.MaxModelLen/1000 && cfg.HealthCheckTimeout < 300 {
		t.Errorf("healthCheckTimeout %d is below the seat's cold load", cfg.HealthCheckTimeout)
	}

	// It must join the RESIDENT set: the seat IS the agent lane, and an ordinary chat
	// request must never evict it and pay the reload.
	resident := cfg.Matrix.Sets["resident"] + cfg.Matrix.Sets["residents"]
	var v string
	for name, model := range cfg.Matrix.Vars {
		if model == spec.ID {
			v = name
		}
	}
	if v == "" {
		t.Fatalf("the seat got no matrix var, so nothing can reference it (vars: %v)", cfg.Matrix.Vars)
	}
	if !strings.Contains(resident, v) && !strings.Contains(out, v) {
		t.Errorf("the seat's matrix var %q is not referenced anywhere in the rendered config", v)
	}

	// Every alias must reach it. An alias llama-swap does not know is a 404 at the exact
	// moment the agent lane needs the seat.
	for _, a := range spec.Aliases {
		found := false
		for _, got := range m.Aliases {
			if got == a {
				found = true
			}
		}
		if !found {
			t.Errorf("alias %q is declared by the tier but absent from the rendered entry %v", a, m.Aliases)
		}
	}
}
