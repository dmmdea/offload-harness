package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/tierseed"
	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// The capacity row compares the workers a box publishes (fleet_max_concurrent_jobs) with the slots its agent
// seat serves in the LIVE serving config. It is informational: never red, never an exit code.

// capacityYAML is a serving config whose agent seat serves `parallel` slots through an alias, with the flag
// arriving through a macro like the shipped templates.
func capacityYAML(parallel string) string {
	return `
macros:
  common: >-
    --jinja --parallel ` + parallel + ` --port ${PORT}
models:
  mimo-9b-agent:
    aliases: [mimo-9b, agent-seat]
    ttl: 300
    cmd: /bin/llama-server --model m.gguf ${common}
`
}

// capacityCfg is a delegating box that seeds an agent seat and points at a serving config holding yaml.
func capacityCfg(t *testing.T, yaml string, seat string, cap int) config.Config {
	t.Helper()
	// The row reads the installer's manifest ($OFFLOAD_HOME/installed.json) for the tier: point it at an empty
	// directory so no test reads the machine it runs on.
	t.Setenv("OFFLOAD_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "llama-swap.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AgentModel = seat
	cfg.ServingConfigPath = path
	cfg.FleetMaxConcurrentJobs = cap
	return cfg
}

func capacityRow(cfg config.Config) string {
	var b strings.Builder
	writeFleetCapacitySection(&b, cfg)
	return b.String()
}

// A node that publishes 4 workers over a --parallel 1 seat holds 3 jobs that only wait inside the seat, charged
// to the job's wall: the row says so and names the value to set.
func TestDoctorWarnsWhenTheFleetCapExceedsTheAgentSeatsSlots(t *testing.T) {
	cfg := capacityCfg(t, capacityYAML("1"), "mimo-9b-agent", 0) // 0 = the built-in default, 4
	got := capacityRow(cfg)
	for _, want := range []string{
		"capacity:   WARN",
		"fleet_max_concurrent_jobs 4 exceeds mimo-9b-agent --parallel 1",
		cfg.ServingConfigPath,
		"3 of 4 workers wait behind one generation slot",
		"counts against the delegator's wall",
		"set fleet_max_concurrent_jobs 1 to match the seat this box serves now",
		"change it again whenever the seat changes (to max_num_seqs on a vLLM seat)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the warn row lacks %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "\n") != 1 {
		t.Errorf("the capacity verdict is one row, got:\n%s", got)
	}
	// An explicit 3 over two slots is the same finding in the plural.
	got = capacityRow(capacityCfg(t, capacityYAML("2"), "mimo-9b-agent", 3))
	if !strings.Contains(got, "WARN  fleet_max_concurrent_jobs 3 exceeds mimo-9b-agent --parallel 2") ||
		!strings.Contains(got, "1 of 3 workers wait behind 2 generation slots") || !strings.Contains(got, "set fleet_max_concurrent_jobs 2") {
		t.Errorf("a cap of 3 over two slots:\n%s", got)
	}
}

// The boundary: a cap that equals the slots is the right answer, not a warning.
func TestDoctorCapacityRowIsOKWhenTheCapEqualsTheSeatsSlots(t *testing.T) {
	cfg := capacityCfg(t, capacityYAML("1"), "mimo-9b-agent", 1)
	got := capacityRow(cfg)
	want := "capacity:   OK    fleet_max_concurrent_jobs 1 = mimo-9b-agent --parallel 1 (" + cfg.ServingConfigPath + ")\n"
	if got != want {
		t.Fatalf("the OK row =\n%q\nwant\n%q", got, want)
	}
	// The same through the seat's alias: the agent_model a box seeds may be the alias, not the entry's key.
	cfg = capacityCfg(t, capacityYAML("1"), "agent-seat", 1)
	if got := capacityRow(cfg); !strings.Contains(got, "OK    fleet_max_concurrent_jobs 1 = agent-seat --parallel 1") {
		t.Errorf("an agent_model that is an alias of the entry must read its slots:\n%s", got)
	}
}

// A cap below the slots is safe (the seat has slots the workers never fill), so it is an OK row that says so.
func TestDoctorCapacityRowIsOKWhenTheCapIsBelowTheSeatsSlots(t *testing.T) {
	got := capacityRow(capacityCfg(t, capacityYAML("4"), "mimo-9b-agent", 1))
	if !strings.Contains(got, "capacity:   OK    fleet_max_concurrent_jobs 1 is below mimo-9b-agent --parallel 4") ||
		!strings.Contains(got, "slots the workers never fill") || strings.Contains(got, "WARN") {
		t.Fatalf("a cap below the slots:\n%s", got)
	}
}

// An unlimited cap (a negative setting) is above any slot count.
func TestDoctorCapacityRowWarnsOnAnUnlimitedCap(t *testing.T) {
	got := capacityRow(capacityCfg(t, capacityYAML("2"), "mimo-9b-agent", -1))
	for _, want := range []string{
		"capacity:   WARN  fleet_max_concurrent_jobs is unlimited (negative) over mimo-9b-agent --parallel 2",
		"set fleet_max_concurrent_jobs 2 to match the seat this box serves now",
		"whenever the seat changes (to max_num_seqs on a vLLM seat)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the unlimited-cap row lacks %q:\n%s", want, got)
		}
	}
}

// UNKNOWN is a row of its own, and it says which unknown it is.
func TestDoctorCapacityRowSaysUnknownWhenTheServingConfigIsUnsetOrHoldsNoSuchSeat(t *testing.T) {
	unset := config.Default()
	unset.AgentModel = "mimo-9b-agent"
	missing := config.Default()
	missing.AgentModel = "mimo-9b-agent"
	missing.ServingConfigPath = filepath.Join(t.TempDir(), "absent.yaml")
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want []string
	}{
		{"serving_config_path unset", unset, []string{"capacity:   UNKNOWN fleet_max_concurrent_jobs 4", "serving_config_path is unset", `"mimo-9b-agent"`}},
		{"serving_config_path unreadable", missing, []string{"capacity:   UNKNOWN", "cannot be read", missing.ServingConfigPath}},
		{"no such entry", capacityCfg(t, capacityYAML("1"), "another-seat", 0), []string{"UNKNOWN", `no entry of the serving config answers to "another-seat"`}},
		{"not a llama.cpp entry", capacityCfg(t, "models:\n  mimo-9b-agent:\n    cmd: /usr/bin/vllm serve /m\n", "mimo-9b-agent", 0), []string{"UNKNOWN", "not a llama.cpp entry", "states no concurrencyLimit"}},
		{"no slot flag", capacityCfg(t, "models:\n  mimo-9b-agent:\n    cmd: /bin/llama-server --model m.gguf\n", "mimo-9b-agent", 0), []string{"UNKNOWN", "states no --parallel"}},
		{"not yaml", capacityCfg(t, "models: [", "mimo-9b-agent", 0), []string{"UNKNOWN", "not parseable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := capacityRow(tc.cfg)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("the UNKNOWN row lacks %q:\n%s", w, got)
				}
			}
			if strings.Contains(got, "WARN") || strings.Contains(got, "FAIL") {
				t.Errorf("an unknown must not read as a finding:\n%s", got)
			}
		})
	}
}

// The row is informational: a WARN does not make doctor exit non-zero, and it reaches the real output in the
// pure-config band.
func TestDoctorCapacityRowNeverChangesTheExitCode(t *testing.T) {
	cfg := capacityCfg(t, capacityYAML("1"), "mimo-9b-agent", 0)
	var ids []string
	for _, a := range modelAliases(cfg) {
		if a.Alias != "" {
			ids = append(ids, a.Alias)
		}
	}
	srv := fakeSwap(t, ids)
	cfg.Endpoint = srv.URL
	var out strings.Builder
	if err := doctorRun(cfg, nil, &out); err != nil {
		t.Fatalf("a capacity WARN must not fail doctor: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "capacity:   WARN  fleet_max_concurrent_jobs 4 exceeds mimo-9b-agent --parallel 1") {
		t.Fatalf("doctor output lacks the capacity row:\n%s", out.String())
	}
	// An UNKNOWN is not red either.
	cfg.ServingConfigPath = ""
	out.Reset()
	if err := doctorRun(cfg, nil, &out); err != nil {
		t.Fatalf("a capacity UNKNOWN must not fail doctor: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "capacity:   UNKNOWN") {
		t.Fatalf("doctor output lacks the UNKNOWN row:\n%s", out.String())
	}
}

// A default config has no agent seat: doctor says nothing about capacity, so a green doctor stays as short as it is.
func TestDoctorStaysSilentAboutCapacityOnADefaultConfig(t *testing.T) {
	if got := capacityRow(config.Default()); got != "" {
		t.Fatalf("a default config must print no capacity row, got %q", got)
	}
	// Even with a serving config set: with no agent seat there is nothing to compare.
	cfg := capacityCfg(t, capacityYAML("1"), "", 0)
	if got := capacityRow(cfg); got != "" {
		t.Fatalf("a config with no agent seat must print no capacity row, got %q", got)
	}
	srv := fakeSwap(t, defaultAliasIDs())
	def := config.Default()
	def.Endpoint = srv.URL
	var out strings.Builder
	if err := doctorRun(def, nil, &out); err != nil {
		t.Fatalf("a default config must pass doctor: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "capacity:") {
		t.Fatalf("doctor on a default config printed a capacity row:\n%s", out.String())
	}
}

// A stated --parallel -1 or auto is a slot count, not a missing one: llama-server serves 4 slots for it. The row used
// to call it "states no --parallel" and go UNKNOWN.
func TestDoctorCapacityRowReadsAStatedAutoParallelAsFourSlots(t *testing.T) {
	for _, parallel := range []string{"-1", "auto"} {
		cfg := capacityCfg(t, capacityYAML(parallel), "mimo-9b-agent", 0) // the default cap, 4
		got := capacityRow(cfg)
		want := "capacity:   OK    fleet_max_concurrent_jobs 4 = mimo-9b-agent --parallel auto, 4 slots (" + cfg.ServingConfigPath + ")\n"
		if got != want || strings.Contains(got, "states no") || strings.Contains(got, "UNKNOWN") {
			t.Fatalf("--parallel %s at a cap of 4:\n%q\nwant\n%q", parallel, got, want)
		}
		got = capacityRow(capacityCfg(t, capacityYAML(parallel), "mimo-9b-agent", 8))
		for _, w := range []string{"WARN  fleet_max_concurrent_jobs 8 exceeds mimo-9b-agent --parallel auto, 4 slots", "4 of 8 workers wait behind 4 generation slots", "set fleet_max_concurrent_jobs 4 to match"} {
			if !strings.Contains(got, w) {
				t.Errorf("--parallel %s at a cap of 8 lacks %q:\n%s", parallel, w, got)
			}
		}
		got = capacityRow(capacityCfg(t, capacityYAML(parallel), "mimo-9b-agent", 2))
		if !strings.Contains(got, "OK    fleet_max_concurrent_jobs 2 is below mimo-9b-agent --parallel auto, 4 slots") {
			t.Errorf("--parallel %s at a cap of 2:\n%s", parallel, got)
		}
	}
}

// llamaEntryYAML is a serving config with one llama.cpp entry; vllmEntryYAML is how a vLLM seat is rendered: a wrapper
// script as the command and a llama-swap concurrencyLimit equal to the engine's max_num_seqs.
func llamaEntryYAML(name, parallel string) string {
	return "models:\n  " + name + ":\n    cmd: /bin/llama-server --model m.gguf --parallel " + parallel + "\n"
}

func vllmEntryYAML(id string, aliases []string, limit int) string {
	return fmt.Sprintf("models:\n  %s:\n    aliases: [%s]\n    cmd: /opt/seat/%s-cmd.sh\n    proxy: http://127.0.0.1:18797\n    concurrencyLimit: %d\n",
		id, strings.Join(aliases, ", "), id, limit)
}

// tierSeat is the vLLM seat the embedded tier table declares for a tier, so a test pins the row to the table and not
// to a number it copied.
func tierSeat(t *testing.T, tier string) vllmseat.Spec {
	t.Helper()
	profiles, err := tierseed.Parse(embeddedProfiles)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := profiles[tier]
	if !ok || p.VLLMSeat == nil {
		t.Fatalf("fixture bug: tier %q declares no vLLM seat", tier)
	}
	return *p.VLLMSeat
}

// installedAs writes the installer's manifest for the tier the way the box would have it.
func installedAs(t *testing.T, tier string) {
	t.Helper()
	dir := os.Getenv("OFFLOAD_HOME")
	if dir == "" {
		t.Fatal("fixture bug: capacityCfg points OFFLOAD_HOME at a directory first")
	}
	if err := os.WriteFile(filepath.Join(dir, "installed.json"), []byte(`{"profile":"`+tier+`","backend":"cuda"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A vLLM seat's slots are in the file: its entry carries a concurrencyLimit equal to its max_num_seqs. The row used to
// say they were "not in" the file.
func TestDoctorCapacityRowReadsAVLLMSeatsConcurrencyLimit(t *testing.T) {
	cfg := capacityCfg(t, vllmEntryYAML("some-vllm-seat", nil, 32), "some-vllm-seat", 0) // the default cap, 4
	got := capacityRow(cfg)
	want := "capacity:   OK    fleet_max_concurrent_jobs 4 is below some-vllm-seat concurrencyLimit 32 (" + cfg.ServingConfigPath + "): the seat has slots the workers never fill\n"
	if got != want {
		t.Fatalf("a vLLM entry with a limit:\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(got, "not in") || strings.Contains(got, "UNKNOWN") {
		t.Fatalf("the slots of a vLLM seat are in the file:\n%s", got)
	}
	// Above the limit, llama-swap answers 429: that is the finding, not a wait behind a slot.
	got = capacityRow(capacityCfg(t, vllmEntryYAML("some-vllm-seat", nil, 32), "some-vllm-seat", 40))
	for _, w := range []string{"WARN  fleet_max_concurrent_jobs 40 exceeds some-vllm-seat concurrencyLimit 32", "8 of 40 workers are past the entry's concurrencyLimit, where llama-swap answers 429", "set fleet_max_concurrent_jobs 32 to match"} {
		if !strings.Contains(got, w) {
			t.Errorf("a cap of 40 over a limit of 32 lacks %q:\n%s", w, got)
		}
	}
	got = capacityRow(capacityCfg(t, vllmEntryYAML("some-vllm-seat", nil, 32), "some-vllm-seat", -1))
	if !strings.Contains(got, "WARN  fleet_max_concurrent_jobs is unlimited (negative) over some-vllm-seat concurrencyLimit 32") || !strings.Contains(got, "answered 429 by llama-swap") {
		t.Errorf("an unlimited cap over a limit:\n%s", got)
	}
}

// A box of a vLLM tier whose venv is not installed serves the tier's llama.cpp fallback at --parallel 1. The row warns
// (cap 4 over one slot) and must not leave the operator at a cap of 1 when the vLLM seat is installed later: it says the
// cap follows the seat served, names the vLLM seat the tier means, and gives the cap to go back to.
func TestDoctorCapacityRowNamesTheVLLMSeatOfATierThatServesItsFallback(t *testing.T) {
	spec := tierSeat(t, "blackwell-2x16")
	yaml := llamaEntryYAML(spec.Fallback, "1")

	// The tier from the installer's manifest.
	cfg := capacityCfg(t, yaml, spec.Fallback, 0)
	installedAs(t, "blackwell-2x16")
	got := capacityRow(cfg)
	for _, w := range []string{
		"capacity:   WARN  fleet_max_concurrent_jobs 4 exceeds " + spec.Fallback + " --parallel 1",
		"set fleet_max_concurrent_jobs 1 to match the seat this box serves now",
		"change it again whenever the seat changes (to max_num_seqs on a vLLM seat)",
		"tier blackwell-2x16 declares the vLLM agent seat " + spec.ID + fmt.Sprintf(" (max_num_seqs %d)", spec.MaxNumSeqs),
		"serves its llama.cpp fallback " + spec.Fallback + " until the vLLM venv is installed",
		fmt.Sprintf("raise fleet_max_concurrent_jobs to %d when %s takes over", spec.MaxNumSeqs, spec.ID),
	} {
		if !strings.Contains(got, w) {
			t.Errorf("the fallback row lacks %q:\n%s", w, got)
		}
	}
	if strings.Count(got, "\n") != 1 {
		t.Errorf("the capacity verdict is one row, got:\n%s", got)
	}

	// The same box after the operator followed it: an OK row that still tells them where the cap goes.
	cfg = capacityCfg(t, yaml, spec.Fallback, 1)
	installedAs(t, "blackwell-2x16")
	got = capacityRow(cfg)
	if !strings.HasPrefix(got, "capacity:   OK    fleet_max_concurrent_jobs 1 = "+spec.Fallback+" --parallel 1") ||
		!strings.Contains(got, fmt.Sprintf("raise fleet_max_concurrent_jobs to %d when %s takes over", spec.MaxNumSeqs, spec.ID)) {
		t.Errorf("a cap of 1 over the fallback:\n%s", got)
	}

	// The tier from the config alone (tierseed seeds tier_profile for composite tiers), when there is no manifest.
	ampere := tierSeat(t, "ampere-16")
	cfg = capacityCfg(t, llamaEntryYAML(ampere.Fallback, "1"), ampere.Fallback, 0)
	cfg.TierProfile = "ampere-16"
	got = capacityRow(cfg)
	if !strings.Contains(got, "tier ampere-16 declares the vLLM agent seat "+ampere.ID+fmt.Sprintf(" (max_num_seqs %d)", ampere.MaxNumSeqs)) {
		t.Errorf("the tier from the config:\n%s", got)
	}
	// And it never changes the exit code: the WARN rows above are informational (TestDoctorCapacityRowNeverChangesTheExitCode).
}

// When the box DOES serve the tier's vLLM seat, the row reads the seat's limit as its max_num_seqs, and a cap below it
// says the cap can follow.
func TestDoctorCapacityRowOnTheVLLMSeatItselfNamesMaxNumSeqs(t *testing.T) {
	spec := tierSeat(t, "blackwell-2x16")
	for _, agent := range []string{spec.ID, spec.Aliases[0]} {
		cfg := capacityCfg(t, vllmEntryYAML(spec.ID, spec.Aliases, spec.MaxNumSeqs), agent, 1)
		installedAs(t, "blackwell-2x16")
		got := capacityRow(cfg)
		for _, w := range []string{
			"capacity:   OK    fleet_max_concurrent_jobs 1 is below " + agent + fmt.Sprintf(" max_num_seqs %d", spec.MaxNumSeqs),
			"slots the workers never fill",
			spec.ID + " is tier blackwell-2x16's vLLM seat",
			fmt.Sprintf("raise fleet_max_concurrent_jobs toward its max_num_seqs %d", spec.MaxNumSeqs),
		} {
			if !strings.Contains(got, w) {
				t.Errorf("agent_model %q: the vLLM-seat row lacks %q:\n%s", agent, w, got)
			}
		}
		if strings.Contains(got, "takes over") || strings.Contains(got, "fallback") {
			t.Errorf("agent_model %q: a box serving the vLLM seat is not on its fallback:\n%s", agent, got)
		}
	}
	// A cap equal to the seat's slots is the right answer and says nothing more.
	cfg := capacityCfg(t, vllmEntryYAML(spec.ID, spec.Aliases, spec.MaxNumSeqs), spec.ID, spec.MaxNumSeqs)
	installedAs(t, "blackwell-2x16")
	want := fmt.Sprintf("capacity:   OK    fleet_max_concurrent_jobs %d = %s max_num_seqs %d (%s)\n", spec.MaxNumSeqs, spec.ID, spec.MaxNumSeqs, cfg.ServingConfigPath)
	if got := capacityRow(cfg); got != want {
		t.Errorf("a cap equal to max_num_seqs:\n%q\nwant\n%q", got, want)
	}
}

// The row names a tier only when it can prove one: no manifest and no tier_profile, a tier that declares no vLLM seat,
// or an agent_model that is neither the vLLM seat nor its fallback (the operator chose another) say nothing of vLLM.
func TestDoctorCapacityRowNamesNoVLLMTierItCannotProve(t *testing.T) {
	spec := tierSeat(t, "blackwell-2x16")
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) config.Config
	}{
		{"no manifest and no tier_profile", func(t *testing.T) config.Config {
			return capacityCfg(t, llamaEntryYAML(spec.Fallback, "1"), spec.Fallback, 0)
		}},
		{"a tier that declares no vLLM seat", func(t *testing.T) config.Config {
			cfg := capacityCfg(t, llamaEntryYAML(spec.Fallback, "1"), spec.Fallback, 0)
			installedAs(t, "blackwell-8")
			return cfg
		}},
		{"an agent_model that is neither the vLLM seat nor its fallback", func(t *testing.T) config.Config {
			cfg := capacityCfg(t, llamaEntryYAML("custom-agent", "1"), "custom-agent", 0)
			installedAs(t, "blackwell-2x16")
			return cfg
		}},
		{"a manifest that does not parse falls back to a config with no tier", func(t *testing.T) config.Config {
			cfg := capacityCfg(t, llamaEntryYAML(spec.Fallback, "1"), spec.Fallback, 0)
			if err := os.WriteFile(filepath.Join(os.Getenv("OFFLOAD_HOME"), "installed.json"), []byte("{"), 0o644); err != nil {
				t.Fatal(err)
			}
			return cfg
		}},
		{"a tier id the table does not know", func(t *testing.T) config.Config {
			cfg := capacityCfg(t, llamaEntryYAML(spec.Fallback, "1"), spec.Fallback, 0)
			installedAs(t, "no-such-tier")
			return cfg
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := capacityRow(tc.setup(t))
			if !strings.Contains(got, "capacity:   WARN") || !strings.Contains(got, "whenever the seat changes") {
				t.Fatalf("the generic WARN is still printed:\n%s", got)
			}
			for _, w := range []string{"declares the vLLM agent seat", "takes over", spec.ID} {
				if strings.Contains(got, w) {
					t.Errorf("the row names a tier it cannot prove (%q):\n%s", w, got)
				}
			}
		})
	}
}
