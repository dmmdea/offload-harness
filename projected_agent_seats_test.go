package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// A-20: no tier may seed a PROJECTED agent seat that a measurement elsewhere already contradicts, without the
// contradiction being on the record.
//
// The ampere-16 profile once seeded the 26B agent seat; on the tier's reference card (a 16 GB A2) that seat scored
// 1/8 on the blind digest, and the seed was fixed in 0.113.15. The same family is still seeded, with no agent
// contract ever run, on volta-16, dual-gpu, amd-rdna3-dgpu and blackwell-32, and amd-rdna3 still derives its seat
// from offload-e4b, the model that FAILED the agent contract on amd-gcn (31 s, 2026-09-20). Whether the A2 finding
// generalises to a card of another size is a measurement nobody has taken. This file does not decide it. It makes
// the state countable: every tier is either MEASURED or carries a PROJECTED label naming the seat it seeds and,
// where the seat is one a measurement failed, the failure. Changing a projected seed, adding a tier, or seeding a
// seat that is known to have failed, each fails here until a person writes the new state down.
//
// Tier-level MEASURED status is the generator's EVIDENCE dict (register I-13: eight GPU tiers). Their agent seats
// are pinned by their own tests (ampere16_agent_seat_test.go, TestAmdGcnSeedsTheSeatItWasMeasuredOn, ...), so this
// file does not pin them a second time.

// measuredAgentTiers are the tiers whose agent seat was run on the tier's own silicon; their seat tests own them.
var measuredAgentTiers = map[string]string{
	"amd-gcn":        "fleet-smoke grounded contract, 2026-09-20; 8GB agent-seat bake, 2026-09-24",
	"ampere-16":      "blind re-audit 2026-09-16 and the digest-8 gate on the GSQ seat",
	"ampere-6":       "tier MEASURED 2026-07-26 on an RTX 3050 6GB (docs/tiers/ampere-6.md, register I-13); the reference box ran it again from 2026-10-06 (CT-51 rescope note)",
	"ampere-8":       "8GB agent-seat bake, 2026-09-24",
	"blackwell-16":   "ADR 0048 Amendment 1: the GSQ seat on one 5060 Ti, digest-8 8/8",
	"blackwell-2x16": "the pair seat, digest-8 gate",
	"blackwell-3x16": "the pair seat, digest-8 gate, and the three-card seat",
	"blackwell-8":    "8GB agent-seat bake, 2026-09-24",
}

// agentSeatLabel is one PROJECTED tier's record.
type agentSeatLabel struct {
	Seat string // config_seed.agent_model as the profile seeds it; "" when it seeds none
	Why  string // why the seed stands without a measurement on this class
	// FailedElsewhere is required when the seat the tier ends up with (agent_model, else the resident tier it
	// derives from) is one agentSeatsMeasuredToFail names, and refused when it is not.
	FailedElsewhere string
}

var projectedAgentSeats = map[string]agentSeatLabel{
	"amd-rdna3": {
		Seat:            "",
		Why:             "derives its seat from resident_tier offload-e4b (the workhorse equals the resident, so the derive rule exempts it); no agent contract has run on this class",
		FailedElsewhere: "offload-e4b",
	},
	"amd-rdna3-dgpu": {
		Seat:            "gemma-4-26b-agent",
		Why:             "projected from the discrete-card tiers; no agent contract has run on this class",
		FailedElsewhere: "gemma-4-26b-agent",
	},
	"blackwell-32": {
		Seat:            "gemma4-26b-a4b",
		Why:             "projected: the 26B resident doubles as the agent seat on a 32 GB card; no box of this class is online",
		FailedElsewhere: "gemma4-26b-a4b",
	},
	"blackwell-48": {
		Seat: "qwen3.8-27b",
		Why:  "projected from the 27B that is the measured seat of the two-card and three-card Blackwell tiers; no box of this class is online",
	},
	"blackwell-72": {
		Seat: "qwen3.8-27b",
		Why:  "projected from the 27B that is the measured seat of the two-card and three-card Blackwell tiers; no box of this class is online",
	},
	"cpu": {
		Seat:            "",
		Why:             "derives its seat from resident_tier offload-e4b; the CPU backend has no agent measurement",
		FailedElsewhere: "offload-e4b",
	},
	"dual-gpu": {
		Seat:            "gemma-4-26b-agent",
		Why:             "generic two-card fallback; the seat follows whichever tier it resolves to and none has run on it",
		FailedElsewhere: "gemma-4-26b-agent",
	},
	"maxwell-3": {
		Seat: "",
		Why:  "binds no seat: the workhorse is the resident E2B (config_seed.model, so the derive rule exempts it) and the agent lane falls back to it until the <node-g> bake (shapes B/C of the 2026-08-17 ampere-6 instrument) picks a seat that fits the 3 GB card beside the embedder",
	},
	"rockchip-rk3588": {
		Seat: "",
		Why:  "no agent seat chosen: fleet_agent_enabled is false and the NPU seat serves vision and constrained text only",
	},
	"volta-16": {
		Seat:            "gemma-4-26b-agent",
		Why:             "projected; no box of this class is online (sm70)",
		FailedElsewhere: "gemma-4-26b-agent",
	},
}

// agentSeatsMeasuredToFail are the agent seats a measurement showed failing, and where.
var agentSeatsMeasuredToFail = map[string]string{
	"gemma-4-26b-agent": "the 26B agent seat scored 1/8 on the ampere-16 reference card (a 16 GB A2; register A-20, ampere-16 reseeded in 0.113.15)",
	"gemma4-26b-a4b":    "the 26B scored 1/8 on the ampere-16 reference card (a 16 GB A2; register A-20)",
	"offload-e4b":       "offload-e4b FAILED the agent contract on amd-gcn in 31 s, 2026-09-20: it never made the read tool call",
}

// checkAgentSeatLabels is the body of TestEveryTierIsMeasuredOrLabelledProjectedForItsAgentSeat over a decoded
// document, one finding per defect. It is a function of its document and its three tables so a fixture can hand
// it a state that must be refused.
func checkAgentSeatLabels(doc seedRulesDoc, measured map[string]string, projected map[string]agentSeatLabel, failed map[string]string) []string {
	var out []string
	for _, name := range sortedTiers(doc) {
		p := doc.Profiles[name]
		_, isMeasured := measured[name]
		label, isProjected := projected[name]
		switch {
		case isMeasured && isProjected:
			out = append(out, fmt.Sprintf("%s is in both measuredAgentTiers and projectedAgentSeats: one tier, one state", name))
			continue
		case !isMeasured && !isProjected:
			out = append(out, fmt.Sprintf("%s is in neither measuredAgentTiers nor projectedAgentSeats: say whether its agent seat was run on its own silicon, or label what it seeds", name))
			continue
		case isMeasured:
			continue
		}
		seeded := ""
		if raw, ok := p.ConfigSeed["agent_model"]; ok {
			seeded = strings.Trim(string(raw), `"`)
		}
		if seeded != label.Seat {
			out = append(out, fmt.Sprintf("%s: the projected agent seed changed from %q to %q; re-label it (and say what measured it, or that nothing did)", name, label.Seat, seeded))
		}
		if strings.TrimSpace(label.Why) == "" {
			out = append(out, fmt.Sprintf("%s: its projected label carries no reason", name))
		}
		effective := seeded
		if effective == "" {
			effective = p.ResidentTier
		}
		why, known := failed[effective]
		switch {
		case known && label.FailedElsewhere != effective:
			out = append(out, fmt.Sprintf("%s ends up with the agent seat %q, which a measurement failed (%s); the label must record it as FailedElsewhere", name, effective, why))
		case !known && label.FailedElsewhere != "":
			out = append(out, fmt.Sprintf("%s: the label records %q as failed elsewhere, but the seat it ends up with (%q) is not in agentSeatsMeasuredToFail", name, label.FailedElsewhere, effective))
		}
	}
	for _, name := range sortedKeys(projected) {
		if _, ok := doc.Profiles[name]; !ok {
			out = append(out, fmt.Sprintf("projectedAgentSeats names %s, which profiles.json no longer has", name))
		}
	}
	for _, name := range sortedKeys(measured) {
		if _, ok := doc.Profiles[name]; !ok {
			out = append(out, fmt.Sprintf("measuredAgentTiers names %s, which profiles.json no longer has", name))
		}
	}
	sort.Strings(out)
	return out
}

func TestEveryTierIsMeasuredOrLabelledProjectedForItsAgentSeat(t *testing.T) {
	for _, finding := range checkAgentSeatLabels(loadSeedRulesDoc(t), measuredAgentTiers, projectedAgentSeats, agentSeatsMeasuredToFail) {
		t.Error(finding)
	}
}

func TestAgentSeatLabelCheckRefusesEachDefect(t *testing.T) {
	doc := seedDoc(t, `{"profiles":{
		"labelled":     {"resident_tier":"r","config_seed":{"agent_model":"seat-a"}},
		"seed-moved":   {"resident_tier":"r","config_seed":{"agent_model":"seat-b"}},
		"unlabelled":   {"resident_tier":"r","config_seed":{"agent_model":"seat-a"}},
		"failed-seed":  {"resident_tier":"r","config_seed":{"agent_model":"bad-seat"}},
		"failed-derive":{"resident_tier":"bad-resident"},
		"stale-note":   {"resident_tier":"r","config_seed":{"agent_model":"seat-a"}},
		"both":         {"resident_tier":"r","config_seed":{"agent_model":"seat-a"}}
	}}`)
	measured := map[string]string{"both": "x", "gone-measured": "x"}
	projected := map[string]agentSeatLabel{
		"labelled":       {Seat: "seat-a", Why: "w"},
		"seed-moved":     {Seat: "seat-a", Why: "w"},
		"failed-seed":    {Seat: "bad-seat", Why: "w"},
		"failed-derive":  {Seat: "", Why: "w"},
		"stale-note":     {Seat: "seat-a", Why: "w", FailedElsewhere: "seat-a"},
		"both":           {Seat: "seat-a", Why: "w"},
		"gone-projected": {Seat: "", Why: "w"},
	}
	failed := map[string]string{"bad-seat": "scored 1/8", "bad-resident": "failed in 31 s"}

	got := strings.Join(checkAgentSeatLabels(doc, measured, projected, failed), "\n")
	for _, want := range []string{
		"seed-moved: the projected agent seed changed",
		"unlabelled is in neither",
		"failed-seed ends up with the agent seat \"bad-seat\"",
		"failed-derive ends up with the agent seat \"bad-resident\"",
		"stale-note: the label records \"seat-a\" as failed elsewhere",
		"both is in both",
		"projectedAgentSeats names gone-projected",
		"measuredAgentTiers names gone-measured",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the check did not refuse %q; it said:\n%s", want, got)
		}
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "labelled") {
			t.Errorf("the clean projected tier was refused: %s", line)
		}
	}
}
