package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// vLLM is a FIRST-CLASS engine in this harness, equal to llama.cpp: every tier must be
// able to TEST and SEAT a model under it (operator hard rule, 2026-09-16). Two gates
// already validate the seats that are declared — but both of them skip on a nil
// declaration, so on 2026-09-16 the A-07 change deleted ampere-16's entire `vllm_seat`
// object to swap a model and BOTH went green: the tier fell out of their iteration
// instead of failing them. A guard written as `if X == nil { continue }` over a
// declared capability is blind by construction.
//
// This gate closes that hole by asserting the SET of tiers rather than iterating
// whatever happens to be declared. Removing a seat now names the tier and fails.
//
// It also keeps the remaining coverage debt COUNTABLE instead of silent: a tier that
// cannot yet seat a model under vLLM belongs in vllmSeatDebt with the reason, and the
// moment it gains a seat this gate tells you to promote it. The debt list is meant to
// shrink to empty — that is the operator's stated target state, not an aspiration.
var (
	// vllmSeatTiers: tiers that declare a `vllm_seat` today. REGRESSION FLOOR —
	// entries are added as tiers gain a seat and removed only on an explicit
	// operator instruction, never as a side effect of changing a model.
	vllmSeatTiers = map[string]string{
		"ampere-16":      "Qwen3.5-4B w4a16, unit vllm-agent-seat, fp8_e5m2 KV (ADR 0035; restored after the 2026-09-16 A-07 deletion)",
		"blackwell-2x16": "Qwen3.8-27B INT4 across the pair, unit vllm-agent-seat-27b",
		"blackwell-3x16": "Qwen3.8-27B INT4, unit vllm-agent-seat-27b",
	}

	// vllmSeatDebt: tiers that CANNOT yet seat a model under vLLM. Every entry is a
	// gap to close, not a settled exemption.
	vllmSeatDebt = map[string]string{
		"ampere-6":        "6 GB: needs a measured sub-6 GB w4a16 candidate",
		"ampere-8":        "8 GB: 9B w4a16 arm never run against the llama.cpp seat",
		"amd-gcn":         "ROCm build path not established for this box class",
		"amd-rdna3":       "ROCm build path not established for this box class",
		"amd-rdna3-dgpu":  "ROCm build path not established for this box class",
		"blackwell-8":     "8 GB + accelerator tier: vLLM arm never measured",
		"blackwell-16":    "twin-arch sibling of ampere-16; owes the same seat (tier-doctrine parity)",
		"blackwell-32":    "no box of this class online to measure on",
		"blackwell-48":    "no box of this class online to measure on",
		"blackwell-72":    "no box of this class online to measure on",
		"cpu":             "no GPU: vLLM CPU inference is forbidden by the RAM-is-overflow-only rule",
		"dual-gpu":        "generic two-card fallback profile; seat follows whichever tier it resolves to",
		"volta-16":        "sm70: needs a kv_cache_dtype that Volta backends accept",
	}
)

func TestEveryTierCanSeatAModelUnderVLLM(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			VLLMSeat *struct {
				ID string `json:"id"`
			} `json:"vllm_seat"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	if len(doc.Profiles) == 0 {
		t.Fatal("no profiles parsed — the schema moved and this gate went blind")
	}

	for _, tier := range sortedKeys(doc.Profiles) {
		p := doc.Profiles[tier]
		_, mustHave := vllmSeatTiers[tier]
		_, owes := vllmSeatDebt[tier]

		switch {
		case mustHave && owes:
			t.Errorf("tier %s is in BOTH vllmSeatTiers and vllmSeatDebt — one tier, one state", tier)

		case !mustHave && !owes:
			t.Errorf("tier %s is tracked by neither vllmSeatTiers nor vllmSeatDebt. A new tier must declare "+
				"where it stands on vLLM: give it a seat and add it to vllmSeatTiers, or record why it cannot "+
				"yet and add it to vllmSeatDebt. Silence is how universal support rots.", tier)

		case mustHave && p.VLLMSeat == nil:
			t.Errorf("REGRESSION: tier %s declared a vllm_seat (%s) and no longer does. vLLM is first-class "+
				"infrastructure and a tier never loses it as a side effect — to change the MODEL, edit the "+
				"fields inside vllm_seat; deleting the object also deletes this tier's LMCache cache-server "+
				"capability, because a cache-server binding is defined per vLLM seat. If the removal is "+
				"genuinely intended, the operator says so and this entry moves to vllmSeatDebt with the reason.",
				tier, vllmSeatTiers[tier])

		case owes && p.VLLMSeat != nil:
			t.Errorf("tier %s now declares a vllm_seat (%s) but is still listed in vllmSeatDebt (%q). Promote it "+
				"to vllmSeatTiers so the seat is protected from here on.", tier, p.VLLMSeat.ID, vllmSeatDebt[tier])
		}
	}

	have, owe := len(vllmSeatTiers), len(vllmSeatDebt)
	t.Logf("vLLM coverage: %d/%d tiers can seat a model under vLLM; %d still owe one", have, have+owe, owe)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
