package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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
		"ampere-16":      "Qwen3.8-27B 3-bit GSQ, unit vllm-agent-seat, fp8_e5m2 KV (ADR 0049; the seat ADR 0035 declared as the 4B was restored after the 2026-09-16 A-07 deletion, then re-declared)",
		"blackwell-16":   "Qwen3.8-27B 3-bit GSQ, unit vllm-agent-seat, fp8 (e4m3) KV — measured on the RTX 5060 Ti 2026-09-17 (ADR 0048 amendment): fp8_e5m2 is NaN on this card under vLLM 0.29",
		"blackwell-2x16": "Qwen3.8-27B INT4 across the pair, unit vllm-agent-seat-27b",
		"blackwell-3x16": "Qwen3.8-27B INT4, unit vllm-agent-seat-27b",
	}

	// vllmSeatDebt: tiers that CANNOT yet seat a model under vLLM. Every entry is a
	// gap to close, not a settled exemption.
	vllmSeatDebt = map[string]string{
		"ampere-6":        "6 GB: needs a measured sub-6 GB w4a16 candidate",
		"ampere-8":        "8 GB: MEASURED, not a path for the 9B: text-only W4A16 weights alone exceed the card before KV (MiMo AutoRound 7.51 GiB, RedHatAI Qwen3.5-9B w4a16 9.35 GiB); owes a sub-8 GB candidate or an operator opt-out",
		"maxwell-3":       "3 GB sm_52: no vLLM wheel targets Maxwell (torch dropped sm_5x) and no sub-3 GB w4a16 candidate exists; the seat is llama.cpp CUDA through the sm_50 PTX",
		"amd-gcn":         "ROCm build path not established for this box class; the only vLLM run was on the CPU, MEASURED 2026-09-20: Qwen3.5-4B bf16 only, 3.1 tok/s against 9.27 for llama.cpp CPU (about 0.3x), not seated",
		"amd-rdna3":       "ROCm build path not established for this box class",
		"amd-rdna3-dgpu":  "ROCm build path not established for this box class",
		"blackwell-8":     "8 GB + accelerator tier: MEASURED, not a path for the 9B (same weights as ampere-8; vLLM 0.30 under WSL sees 6.86 GiB free on the 5060 8 GB); owes a sub-6.8 GB candidate or an operator opt-out",
		"blackwell-32":    "no box of this class online to measure on",
		"blackwell-48":    "no box of this class online to measure on",
		"blackwell-72":    "no box of this class online to measure on",
		"cpu":             "no GPU: vLLM CPU inference is forbidden by the RAM-is-overflow-only rule (and MEASURED 2026-09-20 at about 0.3x llama.cpp CPU, Qwen3.5-4B bf16 only, not seated)",
		"dual-gpu":        "generic two-card fallback profile; seat follows whichever tier it resolves to",
		"rockchip-rk3588": "vLLM has no Mali or RKNPU target: the GPU is served by llama.cpp on Vulkan (Mesa panvk) and the NPU by the Rockchip RKLLM runtime",
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

// vllmMeasuredNote matches the sentences a profile's own notes use to record a vLLM RESULT on that tier's
// silicon: "vLLM-CPU MEASURED <date>" (a run that seated or failed to seat a model) and "vLLM is NOT a path"
// (weights that cannot fit the card). A tier whose notes carry one has been measured, so a debt reason that
// still reads "never run" / "never measured" / "not established" is stale (register A-07, coverage).
var vllmMeasuredNote = regexp.MustCompile(`vLLM-CPU MEASURED|vLLM is NOT a path`)

// vllmReasonSaysMeasured is what a debt reason must carry once the tier's notes record a measurement: the
// house spelling of a result, upper case ("never measured" is the stale reason and must not satisfy it).
var vllmReasonSaysMeasured = regexp.MustCompile(`MEASURED`)

// checkVLLMDebtReasonsAreCurrent returns one finding per vllmSeatDebt entry whose profile text records a vLLM
// measurement that the entry's reason does not mention. It takes the profile text per tier so a fixture can hand
// it a tier that must be refused (the W0.1 pattern: a gate only ever shown clean data has not been seen to fail).
func checkVLLMDebtReasonsAreCurrent(profileText map[string]string, debt map[string]string) []string {
	var out []string
	for _, tier := range sortedKeys(debt) {
		if !vllmMeasuredNote.MatchString(profileText[tier]) {
			continue
		}
		if !vllmReasonSaysMeasured.MatchString(debt[tier]) {
			out = append(out, tier+": its profile notes record a vLLM measurement but its vllmSeatDebt reason ("+debt[tier]+") does not say it was measured")
		}
	}
	return out
}

func TestVLLMDebtReasonsAreCurrent(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]json.RawMessage `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	text := map[string]string{}
	measured := 0
	for tier, body := range doc.Profiles {
		text[tier] = string(body)
		if _, owes := vllmSeatDebt[tier]; owes && vllmMeasuredNote.MatchString(text[tier]) {
			measured++
		}
	}
	if measured == 0 {
		t.Fatal("no vllmSeatDebt tier's notes record a vLLM measurement any more; the matcher went blind or the notes moved")
	}
	for _, f := range checkVLLMDebtReasonsAreCurrent(text, vllmSeatDebt) {
		t.Error(f)
	}
}

// A debt reason that predates a measurement is refused; one that records it, and a tier with no measurement
// to record, are not.
func TestVLLMDebtReasonCheckRefusesAStaleReason(t *testing.T) {
	profiles := map[string]string{
		"measured-tier": `{"notes":"... vLLM is NOT a path for this 9B on 8GB ..."}`,
		"quiet-tier":    `{"notes":"no vLLM result recorded here"}`,
	}
	stale := map[string]string{"measured-tier": "vLLM arm never measured", "quiet-tier": "no box online"}
	got := checkVLLMDebtReasonsAreCurrent(profiles, stale)
	if len(got) != 1 || !strings.HasPrefix(got[0], "measured-tier:") {
		t.Fatalf("want exactly one finding, for measured-tier, got %q", got)
	}
	current := map[string]string{"measured-tier": "MEASURED, not a path: weights exceed the card", "quiet-tier": "no box online"}
	if got := checkVLLMDebtReasonsAreCurrent(profiles, current); len(got) != 0 {
		t.Fatalf("a reason that records the measurement must pass, got %q", got)
	}
}
