package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/servingtmpl"
	"github.com/dmmdea/offload-harness/internal/tierseed"
)

// A node publishes fleet_max_concurrent_jobs workers (default 4) and the delegator deals it as many jobs as that
// leaves free. When the node's agent seat is a single-slot llama.cpp seat (--parallel 1) the engine runs one
// request at a time, so four published workers are one worker and three jobs waiting inside the seat while the
// delegator counts them as parallel (2026-10-08, a 12-page research call: nodes publishing 4 held 3 running and
// 4 running plus 1 queued over --parallel 1 seats). The rule below is the data-side fix, derived from the
// shipped serving templates and not from a list of tiers, so a tier added later cannot drift from it.
//
// THE RULE. A tier seeds fleet_max_concurrent_jobs 1 when
//   - it seeds an agent seat (agent_model): the rule is about the agent lane. amd-rdna3 and cpu seed none, so their
//     agent lane would fall back to the workhorse (offload-e4b, also --parallel 1); whether they should seed a
//     one-worker cap too is a separate decision, and this rule leaves them as they are;
//   - it declares no vllm_seat: a tier that does is served by a vLLM seat when the box runs it (many slots),
//     and its llama-swap agent entry is only the fallback, so a one-worker cap there is the opposite defect;
//   - the agent seat, resolved through its aliases, is a llama.cpp entry of the tier's rendered template that
//     serves ONE slot (--parallel 1, --parallel=1, -np 1 or LLAMA_ARG_N_PARALLEL=1) on every OS the tier is
//     rendered for.
//
// The key is also the delegator's run-cap line (localRunCapRoom, atRunCap, dealParallelism's local count): on
// such a box the local seat takes one run at a time and the rest of a call goes to the fleet. That is intended,
// because a single-slot seat serves one request at a time.

// seededConfigFor is effectiveConfig with the RAM tier the installer was given.
func seededConfigFor(t *testing.T, prof tierseed.Profile, id, goos, ram string) config.Config {
	t.Helper()
	cfg := config.Default()
	seed, err := tierseed.Resolve(prof, id, tierseed.Options{Home: "/opt/offload", GOOS: goos, RAMTier: ram})
	if err != nil {
		t.Fatalf("tier %s: resolving the seed: %v", id, err)
	}
	if len(seed) == 0 {
		return cfg
	}
	b, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("tier %s: applying the seed the way config.Load does: %v", id, err)
	}
	return cfg
}

// fleetCapVerdict is the rule applied to one tier.
type fleetCapVerdict struct {
	selected bool   // the rule seeds a one-worker node
	slots    int    // the fewest slots the agent seat serves on any OS the tier renders for (0 = not a known llama.cpp seat)
	seat     string // the agent seat, as the tier's config names it
	reason   string // why a tier is not selected
	configs  []fleetCapConfig
}

// fleetCapConfig is one resolved seed of the tier, per OS and RAM tier.
type fleetCapConfig struct {
	goos, ram string
	cfg       config.Config
}

// fleetCapRuleFor derives the verdict from the tier table and the rendered serving templates.
func fleetCapRuleFor(t *testing.T, id string, sp servingProfile, prof tierseed.Profile) fleetCapVerdict {
	t.Helper()
	v := fleetCapVerdict{}
	for _, goos := range []string{"linux", "windows"} {
		for _, ram := range []string{"", "mid", "high"} {
			v.configs = append(v.configs, fleetCapConfig{goos, ram, seededConfigFor(t, prof, id, goos, ram)})
		}
	}
	for _, c := range v.configs {
		if c.cfg.AgentModel == "" {
			v.reason = "no agent seat is seeded (the rule is about the agent lane; the workhorse fallback is a separate decision)"
			return v
		}
		v.seat = c.cfg.AgentPlannerModel("")
	}
	if prof.VLLMSeat != nil {
		v.reason = "declares a vllm_seat: when the box runs it, that is the agent seat (many slots) and the llama-swap entry is the fallback"
		return v
	}
	for _, goos := range []string{"linux", "windows"} {
		tmpl, err := templateFor(goos, sp.Backend)
		if err != nil {
			continue // this tier has no template for this OS
		}
		rendered, err := servingtmpl.Render(tmpl, renderParams(sp, goos))
		if err != nil {
			t.Errorf("tier %s (%s/%s): rendering: %v", id, goos, sp.Backend, err)
			v.reason = "does not render"
			return v
		}
		for _, c := range v.configs {
			if c.goos != goos {
				continue
			}
			n, why := servingtmpl.SeatParallelWhy(rendered, c.cfg.AgentPlannerModel(""))
			if why != "" {
				t.Errorf("tier %s (%s/%s, ram %q): the slots of agent seat %q are unknown: %s. State --parallel in the "+
					"template entry (or the seed rule cannot say what cap the tier needs)", id, goos, sp.Backend, c.ram, c.cfg.AgentPlannerModel(""), why)
				v.reason = "slots unknown"
				return v
			}
			if v.slots == 0 || n < v.slots {
				v.slots = n
			}
		}
	}
	if v.slots == 0 {
		v.reason = "no serving template renders the tier"
		return v
	}
	v.selected = v.slots == 1
	if !v.selected {
		v.reason = "its agent seat serves more than one slot"
	}
	return v
}

func fleetCapProfiles(t *testing.T) (map[string]servingProfile, map[string]tierseed.Profile, []string) {
	t.Helper()
	seedProfiles, err := tierseed.Parse(embeddedProfiles)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]servingProfile `json:"profiles"`
	}
	if err := json.Unmarshal(embeddedProfiles, &doc); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(doc.Profiles))
	for id := range doc.Profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return doc.Profiles, seedProfiles, ids
}

// TestTheSingleSlotLlamaCppTiersSeedAOneWorkerFleetNode: every tier the rule selects resolves to ONE worker and
// the default backlog (twice the workers: one running, one waiting), and does not seed fleet_max_queue_depth,
// which is the backlog bound and stays at its default (the rk3588 precedent).
func TestTheSingleSlotLlamaCppTiersSeedAOneWorkerFleetNode(t *testing.T) {
	profiles, seedProfiles, ids := fleetCapProfiles(t)
	var selected []string
	for _, id := range ids {
		v := fleetCapRuleFor(t, id, profiles[id], seedProfiles[id])
		if !v.selected {
			t.Logf("tier %-16s not selected: %s", id, v.reason)
			if seedProfiles[id].VLLMSeat != nil {
				if raw, seeded := seedProfiles[id].ConfigSeed["fleet_max_concurrent_jobs"]; seeded && raw == float64(1) {
					t.Errorf("tier %s declares a vllm_seat and seeds fleet_max_concurrent_jobs 1: a one-worker node over a many-slot engine is the opposite defect", id)
				}
			}
			continue
		}
		selected = append(selected, id)
		t.Logf("tier %-16s SELECTED: agent seat %q serves %d slot", id, v.seat, v.slots)
		var badCap, badQueue []string
		for _, c := range v.configs {
			if got := c.cfg.FleetConcurrencyLimit(); got != 1 {
				badCap = append(badCap, fmt.Sprintf("%s/ram %q = %d", c.goos, c.ram, got))
			}
			if got := c.cfg.FleetQueueLimit(); got != 2 {
				badQueue = append(badQueue, fmt.Sprintf("%s/ram %q = %d", c.goos, c.ram, got))
			}
		}
		if len(badCap) > 0 {
			t.Errorf("tier %s: fleet concurrency is not 1 (%s): agent seat %q is a single-slot llama.cpp seat, so a node that publishes more "+
				"workers holds jobs that only wait inside the seat while the delegator counts them as parallel. Seed fleet_max_concurrent_jobs 1 "+
				"in the tier's config_seed", id, strings.Join(badCap, ", "), v.seat)
		}
		if len(badQueue) > 0 {
			t.Errorf("tier %s: fleet queue depth is not 2, the default (twice the concurrency): %s", id, strings.Join(badQueue, ", "))
		}
		if _, set := seedProfiles[id].ConfigSeed["fleet_max_queue_depth"]; set {
			t.Errorf("tier %s seeds fleet_max_queue_depth: leave the backlog at its default (twice the concurrency)", id)
		}
		if _, set := seedProfiles[id].ConfigSeedMidHigh["fleet_max_queue_depth"]; set {
			t.Errorf("tier %s seeds fleet_max_queue_depth in its mid/high RAM overlay: leave the backlog at its default", id)
		}
	}
	if len(selected) == 0 {
		t.Fatal("the rule selected no tier: it went blind (SeatParallel reads nothing from the rendered templates?)")
	}
	// The three the plan named are single-slot llama.cpp tiers by construction; if the rule stops selecting one of
	// them it has lost its eyes, not found a reason.
	for _, id := range []string{"ampere-6", "ampere-8", "amd-gcn"} {
		if v := fleetCapRuleFor(t, id, profiles[id], seedProfiles[id]); !v.selected {
			t.Errorf("tier %s is not selected (%s): the rule no longer sees its single-slot agent seat", id, v.reason)
		}
	}
	t.Logf("selected tiers (%d): %v", len(selected), selected)
}

// TestEverySeededCapFitsTheSlotsOfTheSeatItsTierServes: whatever a tier seeds, the worker count it resolves to is
// at least one and never above the slots of the agent seat the shipped template serves, on every OS and RAM tier.
// Unlimited (a negative setting, which resolves to 0) is above any slot count.
func TestEverySeededCapFitsTheSlotsOfTheSeatItsTierServes(t *testing.T) {
	profiles, seedProfiles, ids := fleetCapProfiles(t)
	checked := 0
	for _, id := range ids {
		v := fleetCapRuleFor(t, id, profiles[id], seedProfiles[id])
		if v.slots == 0 {
			continue // no llama.cpp agent seat to compare with
		}
		checked++
		var bad []string
		for _, c := range v.configs {
			if limit := c.cfg.FleetConcurrencyLimit(); limit < 1 || limit > v.slots {
				bad = append(bad, fmt.Sprintf("%s/ram %q = %d", c.goos, c.ram, limit))
			}
		}
		if len(bad) > 0 {
			t.Errorf("tier %s: fleet_max_concurrent_jobs resolves outside 1..%d (0 = unlimited): %s. Agent seat %q serves %d slot(s), so any worker "+
				"above that waits inside the seat while the delegator counts it as a parallel worker", id, v.slots, strings.Join(bad, ", "), v.seat, v.slots)
		}
	}
	if checked == 0 {
		t.Fatal("no tier had a llama.cpp agent seat to compare with: the check went blind")
	}
}
