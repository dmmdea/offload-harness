package main

import (
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/tierseed"
)

// The Qwen3.6-35B-A3B RAM-spill agent seat on ampere-6 (ADR 0080). The gate has two halves written in
// two places: the serving render (install_render.go spillSeatIncluded) and the seed overlay that binds
// agent_model (config_seed_ram_low_up). A box where they disagree is the "phantom capability" the seat
// closure gates exist to end: a config naming a seat the roster dropped, or a roster serving 12 GB of
// weights nothing routes to. Every test here renders through deriveRender, the production derivation.

const spillSeat = "qwen3.6-35b-a3b-agent"

// spillRender renders a tier the way `install render` does for one OS and RAM tier.
func spillRender(t *testing.T, tier, goos, ram string) renderResult {
	t.Helper()
	req := renderReq(tier, goos, &pinnedVLLM{})
	req.RAMTier = ram
	res, err := deriveRender(embeddedProfiles, req)
	if err != nil {
		t.Fatalf("%s %s ram %q: %v", tier, goos, ram, err)
	}
	return res
}

var agentSeatAliasRe = regexp.MustCompile(`(?m)^\s+aliases:\s*\[[^\]]*\bagent-seat\b[^\]]*\]`)

// nonCommentConfig is a rendered config without its `#` lines (a dropped entry's comment stays).
func nonCommentConfig(rendered string) string {
	var b strings.Builder
	for _, ln := range strings.Split(rendered, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(ln), "#") {
			b.WriteString(ln)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// TestTheAgentSeatEachRAMTierBindsIsTheOneItsRenderServes: for every RAM tier and both operating
// systems, ampere-6's seeded agent_model is a seat its own rendered roster defines, the spill seat is
// bound exactly where it is rendered (low, mid, high), and a `min` box renders and binds the 4B as
// before. The agent-seat alias is held by exactly one entry on every box.
func TestTheAgentSeatEachRAMTierBindsIsTheOneItsRenderServes(t *testing.T) {
	_, seedProfiles, _ := fleetCapProfiles(t)
	for _, goos := range []string{"linux", "windows"} {
		for _, tc := range []struct {
			ram       string
			wantSeat  string
			spillSeat bool
		}{
			{"min", "qwen3.5-4b-agent", false},
			{"low", spillSeat, true},
			{"mid", spillSeat, true},
			{"high", spillSeat, true},
		} {
			res := spillRender(t, "ampere-6", goos, tc.ram)
			cfg := seededConfigFor(t, seedProfiles["ampere-6"], "ampere-6", goos, tc.ram)
			agent := cfg.AgentPlannerModel("")
			if agent != tc.wantSeat {
				t.Errorf("%s ram %s: the seeded agent seat is %q, want %q", goos, tc.ram, agent, tc.wantSeat)
			}
			served := servedAliases(res.Config)
			if !served[agent] {
				t.Errorf("%s ram %s: the config binds agent_model=%q but the rendered roster does not serve it (phantom capability)", goos, tc.ram, agent)
			}
			if got := res.Params.IncludeQ3635B; got != tc.spillSeat {
				t.Errorf("%s ram %s: Params.IncludeQ3635B = %v, want %v", goos, tc.ram, got, tc.spillSeat)
			}
			hasEntry := strings.Contains(nonCommentConfig(res.Config), "\n  "+spillSeat+":")
			if hasEntry != tc.spillSeat {
				t.Errorf("%s ram %s: the rendered roster defines the spill seat = %v, want %v", goos, tc.ram, hasEntry, tc.spillSeat)
			}
			// The 4B is always rendered: the only agent seat on a min box, the opt-in rollback elsewhere.
			if !served["qwen3.5-4b-agent"] {
				t.Errorf("%s ram %s: the 4B entry must stay rendered", goos, tc.ram)
			}
			if n := len(agentSeatAliasRe.FindAllString(res.Config, -1)); n != 1 {
				t.Errorf("%s ram %s: %d entries claim the agent-seat alias, want exactly 1", goos, tc.ram, n)
			}
			// `--n-cpu-moe` exists in the rendered config exactly when the seat does.
			if has := strings.Contains(nonCommentConfig(res.Config), "--n-cpu-moe"); has != tc.spillSeat {
				t.Errorf("%s ram %s: --n-cpu-moe present = %v, want %v", goos, tc.ram, has, tc.spillSeat)
			}
			// The write gate (INV-1) accepts every one of these: the tier declares the spill it measured.
			if err := renderGate(res); err != nil {
				t.Errorf("%s ram %s: the write gate refuses the render: %v", goos, tc.ram, err)
			}
		}
	}
}

// TestNoOtherTierRendersTheSpillSeatOrAnNCPUMoESpill: the seat is ampere-6's alone tonight. Any other
// tier, at any RAM tier, renders no spill seat and no `--n-cpu-moe` at all, so no other tier's roster,
// seed or spec hash moved with this change.
func TestNoOtherTierRendersTheSpillSeatOrAnNCPUMoESpill(t *testing.T) {
	profiles, _, ids := fleetCapProfiles(t)
	checked := 0
	for _, id := range ids {
		if id == "ampere-6" {
			continue
		}
		if profiles[id].IncludeQwen3635B {
			t.Errorf("tier %s sets include_qwen36_35b: the seat is measured on ampere-6 only", id)
		}
		for _, goos := range []string{"linux", "windows"} {
			if _, err := templateFor(goos, profiles[id].Backend); err != nil {
				continue
			}
			req := renderReq(id, goos, &pinnedVLLM{})
			req.RAMTier = "high"
			res, err := deriveRender(embeddedProfiles, req)
			if err != nil {
				continue // a pair that refuses to render is some other test's business
			}
			checked++
			body := nonCommentConfig(res.Config)
			if strings.Contains(body, spillSeat) || strings.Contains(body, "--n-cpu-moe") {
				t.Errorf("tier %s (%s) renders the spill seat or an --n-cpu-moe spill, which only ampere-6 may", id, goos)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no tier rendered: the check went blind")
	}
}

// TestTheSpillSeatIsSanctionedByTheTiersMeasuredSpillAndNothingElse: ampere-6 declares n_cpu_moe_max 40,
// the number its seat's `--n-cpu-moe 40` was measured at. Lower it and the write gate refuses the render
// on exactly the boxes that render the seat; on a `min` box (no seat, no spill) the same table passes.
func TestTheSpillSeatIsSanctionedByTheTiersMeasuredSpillAndNothingElse(t *testing.T) {
	render := func(ram string, max int) (renderResult, error) {
		doc := tableDoc(t)
		tierEntry(doc, "ampere-6")["n_cpu_moe_max"] = max
		req := renderReq("ampere-6", "linux", &pinnedVLLM{})
		req.RAMTier = ram
		return deriveRender(marshalTable(t, doc), req)
	}
	res, err := render("low", 40)
	if err != nil {
		t.Fatal(err)
	}
	if err := renderGate(res); err != nil {
		t.Fatalf("a measured spill of 40 must sanction the seat's --n-cpu-moe 40: %v", err)
	}
	for _, max := range []int{39, 0} {
		res, err := render("low", max)
		if err != nil {
			t.Fatal(err)
		}
		err = renderGate(res)
		if err == nil {
			t.Fatalf("n_cpu_moe_max %d must refuse the seat's --n-cpu-moe 40", max)
		}
		for _, want := range []string{"ampere-6", "--n-cpu-moe 40", "INV-1", "not written"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("n_cpu_moe_max %d: the refusal must carry %q, got: %v", max, want, err)
			}
		}
	}
	// A min box renders no seat, so there is no spill to sanction and the render passes at any max.
	res, err = render("min", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := renderGate(res); err != nil {
		t.Fatalf("a min box renders no spill, so n_cpu_moe_max 0 must pass: %v", err)
	}
}

// TestTheSeedOverlayAndTheRenderShareOneRAMPredicate: spillSeatIncluded must be exactly the set of RAM
// tiers config_seed_ram_low_up applies on, for every tier name the installers can hand over, and an
// UNKNOWN tier ("", the caller does not know its RAM) is below the floor on both sides, never a wildcard.
// Two predicates that drift apart is how a binding outlives its seat.
func TestTheSeedOverlayAndTheRenderShareOneRAMPredicate(t *testing.T) {
	p := servingProfile{IncludeQwen3635B: true}
	for ram, explicit := range map[string]bool{
		"": false, " ": false, "min": false, "none": false, "huge": false,
		"low": true, "mid": true, "high": true, "LOW": true, " low ": true, "Mid": true,
	} {
		want := tierseed.RAMLowUp(ram)
		if want != explicit {
			t.Errorf("tierseed.RAMLowUp(%q) = %v, want %v", ram, want, explicit)
		}
		if got := spillSeatIncluded(p, ram); got != want {
			t.Errorf("spillSeatIncluded(ram %q) = %v, but the seed overlay says %v", ram, got, want)
		}
		if spillSeatIncluded(servingProfile{}, ram) {
			t.Errorf("a tier that does not carry the seat must never include it (ram %q)", ram)
		}
	}
}

// TestTheLowUpOverlayNeverSetsTheFleetCap: a single-slot seat keeps the default backlog (twice the
// workers); the seed rule in fleet_cap_seed_test.go forbids fleet_max_queue_depth in the base and
// mid/high layers, and the fleet cap is one number that must hold on every RAM tier, so the low-and-up
// layer may set neither.
func TestTheLowUpOverlayNeverSetsTheFleetCap(t *testing.T) {
	_, seedProfiles, ids := fleetCapProfiles(t)
	for _, id := range ids {
		for _, k := range []string{"fleet_max_queue_depth", "fleet_max_concurrent_jobs"} {
			if _, set := seedProfiles[id].ConfigSeedLowUp[k]; set {
				t.Errorf("tier %s seeds %s in its low-and-up RAM overlay: the fleet cap is the base seed's decision, "+
					"one number that holds on every RAM tier", id, k)
			}
		}
	}
}

// TestTheInstallCommandsSeatTheSpillSeatAndWarnForItsWeightOnlyWhereItIsRendered drives the two
// commands the installers actually call, `install render` and `install seed`, for the OS this suite
// runs on. The roster and the binding agree per RAM tier, and the missing-weight warning (which is
// handed the POST-gate flag) names the 12.3 GiB GGUF exactly where the entry is rendered: a `min` box
// is not told to fetch a file for a seat it does not serve.
func TestTheInstallCommandsSeatTheSpillSeatAndWarnForItsWeightOnlyWhereItIsRendered(t *testing.T) {
	goos := runtime.GOOS
	for _, tc := range []struct {
		ram       string // "" = the flag is not passed at all (a caller that does not know its RAM)
		wantSeat  string
		spillSeat bool
	}{
		{"", "qwen3.5-4b-agent", false},
		{"min", "qwen3.5-4b-agent", false},
		{"low", spillSeat, true},
		{"high", spillSeat, true},
		// The flag spelled with other case or padding reaches BOTH commands the same way.
		{"LOW", spillSeat, true},
		{" Low ", spillSeat, true},
		{"MIN", "qwen3.5-4b-agent", false},
	} {
		home, models := t.TempDir(), t.TempDir() // the models dir is EMPTY: every gated weight is absent
		var ramArgs []string
		if tc.ram != "" {
			ramArgs = []string{"-ram-tier", tc.ram}
		}
		cfg, notes, err := renderCommand(t, append([]string{"-profile", "ampere-6", "-os", goos, "-root", ".", "-home", home,
			"-models", models, "-llama-bin", filepath.Join(home, "llama")}, ramArgs...)...)
		if err != nil {
			t.Fatalf("ram %q: install render: %v", tc.ram, err)
		}
		seed, _ := seedCommand(t, append([]string{"-profile", "ampere-6", "-os", goos, "-root", ".", "-home", home}, ramArgs...)...)
		if got := seed["agent_model"]; got != tc.wantSeat {
			t.Errorf("ram %s: install seed binds agent_model=%v, want %q", tc.ram, got, tc.wantSeat)
		}
		if !servedAliases(cfg)[tc.wantSeat] {
			t.Errorf("ram %s: install render does not serve the seat install seed binds (%q)", tc.ram, tc.wantSeat)
		}
		if served := strings.Contains(nonCommentConfig(cfg), "\n  "+spillSeat+":"); served != tc.spillSeat {
			t.Errorf("ram %q: install render serves the spill seat entry = %v, want %v", tc.ram, served, tc.spillSeat)
		}
		warned := strings.Contains(notes, "Qwen3.6-35B-A3B")
		if warned != tc.spillSeat {
			t.Errorf("ram %s: the missing-weight warning names the spill GGUF = %v, want %v. stderr:\n%s", tc.ram, warned, tc.spillSeat, notes)
		}
		if !strings.Contains(notes, "Qwen3.5-4B-UD-Q4_K_XL.gguf") {
			t.Errorf("ram %s: the 4B stays rendered, so its absent weight is still named. stderr:\n%s", tc.ram, notes)
		}
	}
}
