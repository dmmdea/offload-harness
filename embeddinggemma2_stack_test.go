package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/servingtmpl"
	"github.com/dmmdea/offload-harness/internal/tierseed"
)

// embeddinggemma2 as a memory-stack member, tier by tier. The entry is shipped in every template that
// renders the stack, but a tier carries it only by its own flag: ampere-6, the memory authority's card
// where it was measured (WITH its multimodal projector), and ampere-8 and blackwell-3x16 as TEXT-ONLY
// replicas (embeddinggemma2_projector false: the same entry without --mmproj). The sets below are the
// whole point of the test: adding a tier to either is a decision someone makes on purpose, in a diff
// that names this file, and eg2CardBudget below refuses the decision when the footprints on record
// cannot fit the card.

var eg2Tiers = []string{"ampere-6", "ampere-8", "blackwell-3x16"}

// eg2ProjectorTiers carry the entry with its projector: the authority, the card it was measured on.
var eg2ProjectorTiers = []string{"ampere-6"}

// eg2TextOnlyTiers carry the entry WITHOUT the projector, because the recorded footprints leave the
// projector no room on their card (eg2CardBudget): a replica only embeds text (media adds go to the
// authority) and text vectors are identical with and without the projector, so nothing it serves is
// lost. They are a named state with a way out: turning the projector on means measuring the
// co-residency, recording the figures in the tier's notes and in eg2CardBudget, then moving the tier
// into eg2ProjectorTiers.
var eg2TextOnlyTiers = []string{"ampere-8", "blackwell-3x16"}

// The entry's footprint on the reference 6 GB node, the only place it was measured: 1,196 MiB loaded,
// 1,466 after image embeds and 1,536 after a short video WITH the projector; 460 loaded and 482 at
// peak WITHOUT it (--ubatch-size 2048 in both).
const (
	eg2ProjectorPeakMiB = 1536
	eg2TextOnlyPeakMiB  = 482
)

type eg2Part struct {
	what string
	mib  int
}

type eg2BudgetRow struct {
	tier    string
	card    string
	cardMiB int
	beside  []eg2Part
}

// sum is everything the records put on the card beside the entry, plus the entry at entryMiB.
func (r eg2BudgetRow) sum(entryMiB int) int {
	s := entryMiB
	for _, p := range r.beside {
		s += p.mib
	}
	return s
}

// eg2CardBudget is the recorded co-residency arithmetic for every tier that carries the entry: the card
// the stack lives on, and every footprint the repo's own records put on that card beside the entry. It
// is a NECESSARY condition, not a proof of fit: the card figures are the largest the records give
// (nominal GiB for the 6 and 8 GB cards, the reported MiB for the 16 GB one), the parts are peaks
// measured one at a time, and a sum under the card says nothing about headroom or the CUDA context.
// What it does catch is a render that declares co-resident a set the recorded numbers cannot hold: the
// residency matrix says the combination is valid and llama-swap does not check VRAM, so the failure would
// be a memory-stack embed request that cannot load beside the loaded agent seat.
//
// It models no headroom and no vLLM seat: the sum is over the llama.cpp footprints the records give, and
// the vLLM agent seat that shares the 3-card tier's utility card is not in it (the reference 3-card box's
// launcher keeps 0.5 GiB for its pinned KV pool, which is not here either). What residents do to such a
// seat is a measured record, not arithmetic in this table (TestTheTripleBlackwellTierStaysTextOnlyOnTheMeasuredRecord).
var eg2CardBudget = []eg2BudgetRow{
	{"ampere-6", "RTX 3050 6 GB, nominal 6 GiB", 6144, []eg2Part{
		{"qwen3.5-4b-agent, ctx 32768 q8_0 KV (win-cuda.yaml)", 3681},
		{"embeddinggemma 300M (ampere16_coresidency_test.go)", 460},
		{"bge-reranker-v2-m3, up to (ampere16_coresidency_test.go)", 378},
	}},
	// The Windows template renders no reranker, so it is left out here: with the projector the sum is over
	// the card even without it, and text-only it still fits with the Linux template's 378 MiB reranker added.
	{"ampere-8", "RTX 3070 Laptop 8 GB, nominal 8 GiB", 8192, []eg2Part{
		{"mimo-9b-agent, ctx 65536 q8_0 KV, peak on the 3070 Laptop (win-cuda.yaml)", 6707},
		{"embeddinggemma 300M (ampere16_coresidency_test.go)", 460},
	}},
	// The whisper is the 2.2 GiB the tier's layers declare (TestTheBlackwellTripleRowKeepsTheDeclaredWhisperOnPurpose),
	// the residents are the co-residency tier's figures and the entry is at the reference 6 GB node's peaks: a
	// necessary condition, not this tier's reason for staying text-only. On the utility card's own measured
	// deltas (505 + 439 for the residents, 1,237 for the entry with its projector) the same sum is 16,184 MiB,
	// under the card; the reason is the measured cold-start record.
	{"blackwell-3x16", "RTX 5060 Ti 16 GB, card 2, 16,311 MiB (docs/FLEET-NODE.md)", 16311, []eg2Part{
		{"vl-8b OCR seat measured alone on card 2 (win-triple-blackwell.yaml)", 11751},
		{"whisper-stt, 2.2 GiB as declared in the tier's layers", 2252},
		{"embeddinggemma 300M (ampere16_coresidency_test.go)", 458},
		{"bge-reranker-v2-m3, up to (ampere16_coresidency_test.go)", 378},
	}},
}

// eg2TierProblems is the budget verdict for one tier row against the tier's own profile, exact in both
// directions. A tier that carries the projector needs the projector sum to fit the card. A tier that is
// text-only needs the text-only sum to fit AND the projector sum NOT to fit, so a text-only tier always
// has a recorded reason, and a measurement that changes the figures forces the projector to be turned on
// (or the tier's row to be corrected) on purpose rather than leaving a stale reason behind.
func eg2TierProblems(row eg2BudgetRow, p servingProfile) []string {
	var out []string
	if !p.IncludeEmbeddingGemma2 {
		return []string{fmt.Sprintf("tier %s has a row in eg2CardBudget but does not carry include_embeddinggemma2", row.tier)}
	}
	withProjector, textOnly := row.sum(eg2ProjectorPeakMiB), row.sum(eg2TextOnlyPeakMiB)
	if !p.eg2TextOnly() {
		if withProjector > row.cardMiB {
			out = append(out, fmt.Sprintf("tier %s carries the entry WITH its projector but the recorded footprints sum to %d MiB (entry %d + %d beside it) on a %s card of %d MiB: "+
				"the residency set would declare a combination the card cannot hold. Measure the co-residency on the box, record it in the tier's notes and in eg2CardBudget, or set embeddinggemma2_projector false",
				row.tier, withProjector, eg2ProjectorPeakMiB, withProjector-eg2ProjectorPeakMiB, row.card, row.cardMiB))
		}
		return out
	}
	if textOnly > row.cardMiB {
		out = append(out, fmt.Sprintf("tier %s carries the text-only entry but even that does not fit: %d MiB (entry %d + %d beside it) on a %s card of %d MiB",
			row.tier, textOnly, eg2TextOnlyPeakMiB, textOnly-eg2TextOnlyPeakMiB, row.card, row.cardMiB))
	}
	if withProjector <= row.cardMiB {
		out = append(out, fmt.Sprintf("tier %s is text-only but the recorded footprints now fit the projector too (%d MiB on %d MiB): the text-only state has no recorded reason left. "+
			"Turn the projector on deliberately: set embeddinggemma2_projector true, move the tier from eg2TextOnlyTiers to eg2ProjectorTiers and update the tier's notes",
			row.tier, withProjector, row.cardMiB))
	}
	return out
}

// TestTheEmbeddingGemma2FlagIsOnTheTiersWhoseRecordedFootprintsFitTheCardAndOnlyThem is exact in both
// directions (see eg2TierProblems) and covers every tier that carries the flag: a tier that carries it
// without a row has no recorded arithmetic, which is how a card gets promised a combination it cannot
// hold.
func TestTheEmbeddingGemma2FlagIsOnTheTiersWhoseRecordedFootprintsFitTheCardAndOnlyThem(t *testing.T) {
	profiles, _, ids := fleetCapProfiles(t)
	seen := map[string]bool{}
	for _, row := range eg2CardBudget {
		seen[row.tier] = true
		p, ok := profiles[row.tier]
		if !ok {
			t.Errorf("no tier %s: this gate went blind for it", row.tier)
			continue
		}
		for _, problem := range eg2TierProblems(row, p) {
			t.Error(problem)
		}
	}
	for _, id := range ids {
		if profiles[id].IncludeEmbeddingGemma2 && !seen[id] {
			t.Errorf("tier %s carries include_embeddinggemma2 but has no row in eg2CardBudget: its fit has no recorded arithmetic", id)
		}
	}
	for _, id := range eg2Tiers {
		if !seen[id] {
			t.Errorf("tier %s is listed in eg2Tiers but has no row in eg2CardBudget", id)
		}
	}
}

// TestTheEmbeddingGemma2ProjectorIsRefusedOnTheTextOnlyTiers states the arithmetic that keeps the two
// replicas text-only, with the figures: text-only fits (6,707 + 460 + 482 = 7,649 on 8,192; 14,839 + 482 =
// 15,321 on 16,311) and the projector does not (8,703 on 8,192; 16,375 on 16,311). Then it flips the real
// profiles in memory, projector on, and requires the very check the suite runs to refuse them: this is
// what stops a tier being switched to the projector without a measurement.
func TestTheEmbeddingGemma2ProjectorIsRefusedOnTheTextOnlyTiers(t *testing.T) {
	profiles, _, _ := fleetCapProfiles(t)
	byTier := map[string]eg2BudgetRow{}
	for _, r := range eg2CardBudget {
		byTier[r.tier] = r
	}
	for _, want := range []struct {
		tier                    string
		textOnly, withProjector int
		cardMiB                 int
	}{
		{"ampere-8", 7649, 8703, 8192},
		{"blackwell-3x16", 15321, 16375, 16311},
	} {
		row := byTier[want.tier]
		if got := row.sum(eg2TextOnlyPeakMiB); got != want.textOnly || got > row.cardMiB {
			t.Errorf("%s: text-only sum = %d (card %d), want %d and under the card", want.tier, got, row.cardMiB, want.textOnly)
		}
		if got := row.sum(eg2ProjectorPeakMiB); got != want.withProjector || got <= row.cardMiB || row.cardMiB != want.cardMiB {
			t.Errorf("%s: projector sum = %d on card %d, want %d and over the card %d", want.tier, got, row.cardMiB, want.withProjector, want.cardMiB)
		}
		on := true
		p := profiles[want.tier]
		p.EmbeddingGemma2Projector = &on
		problems := eg2TierProblems(row, p)
		if len(problems) != 1 || !strings.Contains(problems[0], "WITH its projector") {
			t.Errorf("%s: switching the projector on must be refused by the budget, got %v", want.tier, problems)
		}
	}
	// and the other direction: a tier whose projector fits has no business being text-only
	ampere6 := profiles["ampere-6"]
	off := false
	ampere6.EmbeddingGemma2Projector = &off
	if problems := eg2TierProblems(byTier["ampere-6"], ampere6); len(problems) != 1 || !strings.Contains(problems[0], "no recorded reason left") {
		t.Errorf("a text-only ampere-6 must be flagged as having no recorded reason, got %v", problems)
	}
}

// TestTheBlackwellTripleRowKeepsTheDeclaredWhisperOnPurpose: the sum adds whisper at the 2.2 GiB the
// tier's layers declare (2,252 MiB), not the ~2,182 MiB measured alone, because the row records what the
// residency set promises is co-resident. The choice is load-bearing: at the measured figure the projector
// sum is 16,305 MiB, 6 under the card, and the sum alone would call the text-only state unfounded, which
// is why the tier's reason is the measured record and not this margin.
func TestTheBlackwellTripleRowKeepsTheDeclaredWhisperOnPurpose(t *testing.T) {
	profiles, seedProfiles, _ := fleetCapProfiles(t)
	const id = "blackwell-3x16"
	var row eg2BudgetRow
	for _, r := range eg2CardBudget {
		if r.tier == id {
			row = r
		}
	}
	declared := 0
	for _, l := range seedProfiles[id].Layers {
		for _, s := range l.Seats {
			if s.Role == "stt" && s.FootprintGiB > 0 {
				declared = int(s.FootprintGiB * 1024)
			}
		}
	}
	at := -1
	for i, part := range row.beside {
		if strings.HasPrefix(part.what, "whisper-stt") {
			at = i
		}
	}
	if at < 0 || declared != 2252 || row.beside[at].mib != declared {
		t.Fatalf("whisper in the row = part %d of %v, layers declare %d MiB: the row must carry the declared 2252", at, row.beside, declared)
	}
	if got := row.sum(eg2ProjectorPeakMiB); got != 16375 || got <= row.cardMiB {
		t.Errorf("declared whisper: projector sum = %d, want 16375 and over the card %d", got, row.cardMiB)
	}
	measured := row
	measured.beside = append([]eg2Part(nil), row.beside...)
	measured.beside[at].mib = 2182
	if got := measured.sum(eg2ProjectorPeakMiB); got != 16305 || got > measured.cardMiB {
		t.Errorf("measured whisper: projector sum = %d, want 16305 and under the card %d (the hairline this test records)", got, measured.cardMiB)
	}
	if problems := eg2TierProblems(measured, profiles[id]); len(problems) != 1 || !strings.Contains(problems[0], "no recorded reason left") {
		t.Errorf("measured whisper: the sum alone must call the text-only state unfounded, got %v", problems)
	}
}

// TestTheTripleBlackwellTierStaysTextOnlyOnTheMeasuredRecord: blackwell-3x16 carries the entry without its
// projector because of a measurement, not because of the sum in eg2CardBudget. Measured 2026-10-09 on the
// reference 3-card box, with that box's local pinned-pool launcher (the seat's --kv-cache-memory-bytes is
// the smaller free memory of the seat's cards less 11.19 GiB non-KV and 0.5 GiB headroom, floor 2.0, cap
// 3.4; NOT the shipped seat.env, which sizes by utilization), the agent seat tensor-parallel 2 over the two
// 5060 Ti cards at max_model_len 163,840:
//
//	(a) embeddinggemma, bge-reranker-v2-m3 and the TEXT-ONLY embeddinggemma2 all on ONE seat card,
//	    1,648 MiB used on it: the agent seat's cold start FAILED after ~208 s, vLLM
//	    _check_enough_kv_cache_memory: 2.66 GiB of KV needed for 163,840 tokens, 2.49 GiB available.
//	(b) embeddinggemma2 moved to the seat's OTHER card (1,049 and 600 MiB on the two cards with the stack
//	    loaded): cold start OK in 255 s, all four models ready afterwards.
//	(c) card-total deltas on the utility card (WDDM, llama.cpp b11490): embeddinggemma +505 MiB, text-only
//	    embeddinggemma2 +501, reranker +439; embeddinggemma2 WITH its projector 1,237 (projector +736).
//
// What it supports: on a tier whose vLLM seat spans the residents' card, the residents must not all sit
// on one seat card at the declared window; the projector adds 736 MiB to the shape that already failed,
// so it stays off. The template still pins all three residents to one card, a known gap recorded in the
// tier's notes and in docs/systems/setup-installer.md.
func TestTheTripleBlackwellTierStaysTextOnlyOnTheMeasuredRecord(t *testing.T) {
	profiles, _, _ := fleetCapProfiles(t)
	p := profiles["blackwell-3x16"]
	if !p.IncludeEmbeddingGemma2 || !p.eg2TextOnly() {
		t.Errorf("blackwell-3x16 must carry the entry text-only (include_embeddinggemma2 %v, text-only %v): the 2026-10-09 measurement "+
			"failed the agent seat's cold start (2.49 of 2.66 GiB of KV) with the three residents on one seat card, and the projector adds 736 MiB to that shape. "+
			"Turning it on takes a co-residency measurement on the real launcher with the residents placed, recorded in the tier's notes",
			p.IncludeEmbeddingGemma2, p.eg2TextOnly())
	}
}

var eg2StackSetRe = regexp.MustCompile(`(?m)^\s{4}residents?:\s*"([^"]*)"`)

// TestTheEmbeddingGemma2FlagAndProjectorAreSetOnExactlyTheirTiers: three tiers carry the entry, one of
// them with the projector, and each of the three says so in its own entry (an explicit
// embeddinggemma2_projector), while every other tier says nothing: the field means something only with
// the entry, and a silent default on a tier that carries it is how a replica would grow a projector.
func TestTheEmbeddingGemma2FlagAndProjectorAreSetOnExactlyTheirTiers(t *testing.T) {
	profiles, _, ids := fleetCapProfiles(t)
	var got, gotProjector, gotTextOnly []string
	for _, id := range ids {
		p := profiles[id]
		if p.IncludeEmbeddingGemma2 {
			got = append(got, id)
			if p.EmbeddingGemma2Projector == nil {
				t.Errorf("tier %s carries include_embeddinggemma2 without stating embeddinggemma2_projector: say true or false on purpose", id)
			}
			if p.eg2TextOnly() {
				gotTextOnly = append(gotTextOnly, id)
			} else {
				gotProjector = append(gotProjector, id)
			}
		} else if p.EmbeddingGemma2Projector != nil {
			t.Errorf("tier %s sets embeddinggemma2_projector without include_embeddinggemma2: the field means nothing there", id)
		}
	}
	for _, c := range []struct {
		what      string
		got, want []string
	}{
		{"carrying include_embeddinggemma2", got, eg2Tiers},
		{"with the projector", gotProjector, eg2ProjectorTiers},
		{"text-only", gotTextOnly, eg2TextOnlyTiers},
	} {
		sort.Strings(c.got)
		want := append([]string{}, c.want...)
		sort.Strings(want)
		if strings.Join(c.got, ",") != strings.Join(want, ",") {
			t.Errorf("tiers %s = %v, want exactly %v (the authority's card carries the projector; the replicas are text-only until a measurement says otherwise)", c.what, c.got, want)
		}
	}
}

// eg2Block returns the embeddinggemma2 entry of a rendered config: from its key to the next key at the
// same indent.
func eg2Block(config string) string {
	var b strings.Builder
	in := false
	for _, ln := range strings.Split(config, "\n") {
		isKey := strings.HasPrefix(ln, "  ") && len(ln) > 2 && ln[2] != ' '
		if in && isKey {
			break
		}
		if isKey && strings.HasPrefix(strings.TrimSpace(ln), "embeddinggemma2:") {
			in = true
		}
		if in {
			b.WriteString(ln)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// eg2FoldedCmd is the command line of the embeddinggemma2 entry as llama-swap reads its `cmd: >-`
// folded scalar: the lines between `cmd: >-` and the next entry key, joined by single spaces.
func eg2FoldedCmd(t *testing.T, config string) string {
	t.Helper()
	var lines []string
	in := false
	for _, ln := range strings.Split(eg2Block(config), "\n") {
		switch {
		case strings.TrimSpace(ln) == "cmd: >-":
			in = true
		case in && strings.HasPrefix(ln, "      "):
			lines = append(lines, strings.TrimSpace(ln))
		case in:
			in = false
		}
	}
	if len(lines) == 0 {
		t.Fatalf("no cmd scalar in the embeddinggemma2 entry:\n%s", eg2Block(config))
	}
	return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
}

// TestEmbeddingGemma2RendersOnExactlyTheTiersThatCarryItOnEveryOSTheyRenderOn: for every tier, every
// OS it has a template for, and two RAM tiers (there is no RAM gate: it is VRAM-resident), the entry,
// its matrix var and its place in the stack's residency set are present iff the tier carries the flag,
// the entry carries --mmproj iff the tier carries the projector, the 300M embedder is always there, and
// the write gate accepts the render.
func TestEmbeddingGemma2RendersOnExactlyTheTiersThatCarryItOnEveryOSTheyRenderOn(t *testing.T) {
	profiles, _, ids := fleetCapProfiles(t)
	rendered := map[string]int{}
	for _, id := range ids {
		for _, goos := range []string{"linux", "windows"} {
			if _, err := templateFor(goos, profiles[id].Backend); err != nil {
				continue
			}
			for _, ram := range []string{"min", "high"} {
				req := renderReq(id, goos, &pinnedVLLM{})
				req.RAMTier = ram
				res, err := deriveRender(embeddedProfiles, req)
				if err != nil {
					continue // a pair that refuses to render is some other test's business
				}
				body := nonCommentConfig(res.Config)
				carries := profiles[id].IncludeEmbeddingGemma2
				has := strings.Contains(body, "\n  embeddinggemma2:")
				if has != carries {
					t.Errorf("%s %s ram %s: the roster defines embeddinggemma2 = %v, want %v (include_embeddinggemma2 = %v)", id, goos, ram, has, carries, carries)
				}
				if res.Params.IncludeEG2 != carries {
					t.Errorf("%s %s ram %s: Params.IncludeEG2 = %v, want %v", id, goos, ram, res.Params.IncludeEG2, carries)
				}
				if res.Params.EG2TextOnly != profiles[id].eg2TextOnly() {
					t.Errorf("%s %s ram %s: Params.EG2TextOnly = %v, want %v (embeddinggemma2_projector)", id, goos, ram, res.Params.EG2TextOnly, profiles[id].eg2TextOnly())
				}
				if hasProjector, want := strings.Contains(eg2Block(body), "--mmproj"), carries && !profiles[id].eg2TextOnly(); hasProjector != want {
					t.Errorf("%s %s ram %s: the embeddinggemma2 entry carries --mmproj = %v, want %v", id, goos, ram, hasProjector, want)
				}
				set := ""
				if m := eg2StackSetRe.FindStringSubmatch(body); m != nil {
					set = m[1]
				}
				if inSet := regexp.MustCompile(`\beg2\b`).MatchString(set); inSet != carries {
					t.Errorf("%s %s ram %s: eg2 in the stack residency set = %v, want %v (set %q)", id, goos, ram, inSet, carries, set)
				}
				if regexp.MustCompile(`(?m)^\s{4}eg2:`).MatchString(body) != carries {
					t.Errorf("%s %s ram %s: the eg2 var / evict row presence does not follow the flag", id, goos, ram)
				}
				// (rk3588 serves from the NPU and renders no embedder of either kind.)
				if profiles[id].Backend != "rk3588" && !servedAliases(res.Config)["embeddinggemma"] {
					t.Errorf("%s %s ram %s: the embeddinggemma (300M) entry must stay on every tier", id, goos, ram)
				}
				if err := renderGate(res); err != nil {
					t.Errorf("%s %s ram %s: the write gate refuses the render: %v", id, goos, ram, err)
				}
				if carries {
					rendered[id]++
				}
			}
		}
	}
	for _, id := range eg2Tiers {
		if rendered[id] == 0 {
			t.Errorf("tier %s carries the flag but never rendered on any OS: the check went blind for it", id)
		}
	}
}

// TestTheTextOnlyEntryOfEachReplicaTierIsTheAuthoritysEntryMinusTheProjector: on ampere-8 and
// blackwell-3x16 the rendered entry carries no --mmproj and every other flag of the authority's entry
// byte for byte. Two references: the same tier rendered with the projector (the whole config differs by
// exactly that one argument), and the authority tier itself, ampere-6, on the same OS (the entry's
// command line differs by exactly that argument). It also proves the reference render is the render:
// re-rendering the tier's own Params through servingtmpl gives back the bytes `install render` wrote.
func TestTheTextOnlyEntryOfEachReplicaTierIsTheAuthoritysEntryMinusTheProjector(t *testing.T) {
	profiles, _, _ := fleetCapProfiles(t)
	rendered := map[string]int{}
	for _, id := range eg2TextOnlyTiers {
		for _, goos := range []string{"linux", "windows"} {
			tmpl, err := templateFor(goos, profiles[id].Backend)
			if err != nil {
				continue
			}
			for _, ram := range []string{"min", "high"} {
				req := renderReq(id, goos, &pinnedVLLM{})
				req.RAMTier = ram
				res, err := deriveRender(embeddedProfiles, req)
				if err != nil {
					t.Errorf("%s %s ram %s: %v", id, goos, ram, err)
					continue
				}
				again, err := servingtmpl.Render(tmpl, res.Params)
				if err != nil || again != res.Config {
					t.Fatalf("%s %s ram %s: re-rendering the tier's Params did not reproduce its config (err %v): the reference below would prove nothing", id, goos, ram, err)
				}
				block := eg2Block(res.Config)
				if strings.TrimSpace(block) == "" {
					t.Errorf("%s %s ram %s: no embeddinggemma2 entry", id, goos, ram)
					continue
				}
				if strings.Contains(block, "mmproj") {
					t.Errorf("%s %s ram %s: the text-only entry names a projector:\n%s", id, goos, ram, block)
				}
				arg := "--mmproj " + req.ModelsDir + "/mmproj-embeddinggemma-2-Q8_0.gguf "
				withProjector := res.Params
				withProjector.EG2TextOnly = false
				with, err := servingtmpl.Render(tmpl, withProjector)
				if err != nil {
					t.Fatalf("%s %s ram %s: %v", id, goos, ram, err)
				}
				if strings.Count(eg2Block(with), arg) != 1 {
					t.Fatalf("%s %s ram %s: the with-projector entry does not carry %q exactly once:\n%s", id, goos, ram, arg, eg2Block(with))
				}
				if want := strings.Replace(with, arg, "", 1); res.Config != want {
					t.Errorf("%s %s ram %s: the text-only config differs from the with-projector config by more than %q", id, goos, ram, arg)
				}
				// the memory authority's own entry on this OS
				authority, err := deriveRender(embeddedProfiles, func() renderRequest {
					r := renderReq("ampere-6", goos, &pinnedVLLM{})
					r.RAMTier = ram
					return r
				}())
				if err != nil {
					t.Fatalf("%s %s ram %s: rendering the authority tier: %v", id, goos, ram, err)
				}
				wantCmd := strings.Replace(eg2FoldedCmd(t, authority.Config), strings.TrimSpace(arg)+" ", "", 1)
				if gotCmd := eg2FoldedCmd(t, res.Config); gotCmd != wantCmd {
					t.Errorf("%s %s ram %s: the replica's command line is not the authority's minus the projector.\n got: %s\nwant: %s", id, goos, ram, gotCmd, wantCmd)
				}
				for _, flag := range []string{"--embeddings --pooling mean", "--ctx-size 4096", "--batch-size 4096", "--ubatch-size 2048", "--flash-attn on"} {
					if !strings.Contains(eg2FoldedCmd(t, res.Config), flag) {
						t.Errorf("%s %s ram %s: the replica's entry lost %q (ubatch 2048 is load-bearing: a smaller one returns HTTP 500 on long memories)", id, goos, ram, flag)
					}
				}
				rendered[id]++
			}
		}
	}
	for _, id := range eg2TextOnlyTiers {
		if rendered[id] == 0 {
			t.Errorf("tier %s never rendered on any OS: the check went blind for it", id)
		}
	}
	// ampere-6 keeps its projector
	for _, goos := range []string{"linux", "windows"} {
		res, err := deriveRender(embeddedProfiles, renderReq("ampere-6", goos, &pinnedVLLM{}))
		if err != nil {
			t.Fatalf("ampere-6 %s: %v", goos, err)
		}
		if !strings.Contains(eg2FoldedCmd(t, res.Config), "--mmproj /opt/offload/models/mmproj-embeddinggemma-2-Q8_0.gguf --embeddings") {
			t.Errorf("ampere-6 %s: the authority's entry lost its projector", goos)
		}
	}
}

// TestTheInstallRenderCommandWarnsForExactlyTheEmbeddingGemma2FilesTheTierDownloads drives the command
// the installers call. With an EMPTY models dir the missing-weight warning names the model AND its
// projector for the tier that carries the projector, the model alone for a text-only replica (it is
// started without --mmproj and the installer never fetches the projector), and nothing for one that does
// not carry the entry. The warning speaks only for a render of the host OS (a miss on another machine
// means nothing: install_render.go), so each tier renders on the host OS and a tier with no template for
// it (the 3-card Blackwell tier on a Linux host) is skipped, never rendered cross-OS with host paths.
func TestTheInstallRenderCommandWarnsForExactlyTheEmbeddingGemma2FilesTheTierDownloads(t *testing.T) {
	profiles, _, _ := fleetCapProfiles(t)
	goos := runtime.GOOS
	checked := 0
	for _, c := range []struct {
		tier          string
		model, mmproj bool
	}{
		{"ampere-6", true, true},
		{"ampere-8", true, false},
		{"blackwell-3x16", true, false},
		{"blackwell-16", false, false},
	} {
		if _, err := templateFor(goos, profiles[c.tier].Backend); err != nil {
			t.Logf("%s: no %s template, skipped on this host", c.tier, goos)
			continue
		}
		home, models := t.TempDir(), t.TempDir()
		_, notes, err := renderCommand(t, "-profile", c.tier, "-os", goos, "-root", ".", "-home", home,
			"-models", models, "-llama-bin", filepath.Join(home, "llama"), "-ram-tier", "high")
		if err != nil {
			t.Fatalf("%s: install render: %v", c.tier, err)
		}
		checked++
		for name, want := range map[string]bool{"embeddinggemma-2-Q8_0.gguf": c.model, "mmproj-embeddinggemma-2-Q8_0.gguf": c.mmproj} {
			if warned := strings.Contains(notes, name); warned != want {
				t.Errorf("%s: the missing-weight warning names %s = %v, want %v. stderr:\n%s", c.tier, name, warned, want, notes)
			}
		}
	}
	if checked < 3 {
		t.Errorf("only %d tier(s) rendered on %s: the projector tier, a text-only tier and a tier without the entry must all be checked", checked, goos)
	}
}

// installedMemoryStack is the memory_stack a fresh install writes for a tier: the installer's template
// config.json with the tier's resolved seed laid over it, the way both installers merge a seed. Starting
// from config.Default() instead would hide the defect this guards (the default already names
// embeddinggemma2; the template does not).
func installedMemoryStack(t *testing.T, id, goos, ram string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("setup/templates/config.json: %v", err)
	}
	_, seedProfiles, _ := fleetCapProfiles(t)
	seed, err := tierseed.Resolve(seedProfiles[id], id, tierseed.Options{Home: "/opt/offload", GOOS: goos, RAMTier: ram})
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range seed {
		cfg[k] = v
	}
	var out []string
	switch l := cfg["memory_stack"].(type) {
	case []string:
		out = l
	case []any:
		for _, e := range l {
			out = append(out, e.(string))
		}
	}
	return out
}

// TestAFreshInstallOfATierThatRendersEmbeddingGemma2KeepsItResident: a configured memory_stack REPLACES the
// default, and the installer template names only embeddinggemma and the reranker, so the tiers that render
// the entry must seed a list that names it, or a fresh install lets `gpu reserve --unload-seat`, the render
// helper and fleet reclaim unload the memory stack's embedder ("mem0 never yields"). The list keeps
// embeddinggemma FIRST (EmbedModel() falls back to MemoryStack[0]). It covers the text-only replicas too:
// a projector-off entry is as much a stack member as the authority's.
func TestAFreshInstallOfATierThatRendersEmbeddingGemma2KeepsItResident(t *testing.T) {
	for _, id := range eg2Tiers {
		for _, goos := range []string{"linux", "windows"} {
			for _, ram := range []string{"min", "high"} {
				got := installedMemoryStack(t, id, goos, ram)
				if strings.Join(got, ",") != "embeddinggemma,bge-reranker-v2-m3,embeddinggemma2" {
					t.Errorf("%s %s ram %s: a fresh install's memory_stack = %v, want [embeddinggemma bge-reranker-v2-m3 embeddinggemma2]", id, goos, ram, got)
				}
			}
		}
	}
}

// TestATextOnlyTierStillSeedsTheKeepSet names the projector-off tiers on their own: dropping the
// projector must never drop the entry from the keep-set, because the keep-set is what stops a lease's
// --unload-seat from unloading the memory stack's embedder, text-only or not.
func TestATextOnlyTierStillSeedsTheKeepSet(t *testing.T) {
	profiles, seedProfiles, _ := fleetCapProfiles(t)
	for _, id := range eg2TextOnlyTiers {
		if !profiles[id].eg2TextOnly() {
			t.Fatalf("tier %s is listed text-only but its profile carries the projector", id)
		}
		ms, ok := seedProfiles[id].ConfigSeed["memory_stack"].([]any)
		if !ok {
			t.Errorf("text-only tier %s seeds no memory_stack keep-set (config_seed.memory_stack = %v)", id, seedProfiles[id].ConfigSeed["memory_stack"])
			continue
		}
		var names []string
		for _, e := range ms {
			names = append(names, fmt.Sprint(e))
		}
		if strings.Join(names, ",") != "embeddinggemma,bge-reranker-v2-m3,embeddinggemma2" {
			t.Errorf("text-only tier %s seeds memory_stack = %v, want [embeddinggemma bge-reranker-v2-m3 embeddinggemma2]", id, names)
		}
	}
}

// TestNoOtherTierSeedsAMemoryStackThatNamesWhatItDoesNotServe: the shared template stays at two entries and
// no other tier seeds a list, because `llamaswap bind check` reports a listed name the roster does not
// serve as a dangling binding; naming embeddinggemma2 on a tier that renders no such entry would flag it.
func TestNoOtherTierSeedsAMemoryStackThatNamesWhatItDoesNotServe(t *testing.T) {
	_, seedProfiles, ids := fleetCapProfiles(t)
	for _, id := range ids {
		carries := false
		for _, e := range eg2Tiers {
			if e == id {
				carries = true
			}
		}
		if _, seeded := seedProfiles[id].ConfigSeed["memory_stack"]; seeded != carries {
			t.Errorf("tier %s seeds a memory_stack = %v, want %v (only the tiers that render embeddinggemma2 do)", id, seeded, carries)
		}
		for _, k := range []string{"memory_stack"} {
			if _, set := seedProfiles[id].ConfigSeedLowUp[k]; set {
				t.Errorf("tier %s seeds %s in a RAM overlay: the keep-set does not depend on RAM", id, k)
			}
			if _, set := seedProfiles[id].ConfigSeedMidHigh[k]; set {
				t.Errorf("tier %s seeds %s in a RAM overlay: the keep-set does not depend on RAM", id, k)
			}
		}
	}
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "embeddinggemma2") {
		t.Error("setup/templates/config.json names embeddinggemma2: every tier that does not render it would carry a dangling memory_stack member")
	}
}

// TestAnAbsentEmbeddingGemma2ProjectorMeansTheProjectorAndOnlyAnExplicitFalseIsTextOnly pins the
// default the field documents, which no shipped tier exercises (all three state the field): absent
// is true, only an explicit false makes a tier text-only, and the field means nothing on a tier that
// does not carry the entry. Then it proves the default end to end through the renderer: with the field
// deleted from the two replicas' entries they render the entry WITH its projector, the authority's.
func TestAnAbsentEmbeddingGemma2ProjectorMeansTheProjectorAndOnlyAnExplicitFalseIsTextOnly(t *testing.T) {
	for _, c := range []struct {
		doc      string
		textOnly bool
	}{
		{`{"include_embeddinggemma2": true}`, false},
		{`{"include_embeddinggemma2": true, "embeddinggemma2_projector": true}`, false},
		{`{"include_embeddinggemma2": true, "embeddinggemma2_projector": false}`, true},
		{`{"embeddinggemma2_projector": false}`, false},
		{`{}`, false},
	} {
		var p servingProfile
		if err := json.Unmarshal([]byte(c.doc), &p); err != nil {
			t.Fatal(err)
		}
		if got := p.eg2TextOnly(); got != c.textOnly {
			t.Errorf("%s: eg2TextOnly() = %v, want %v", c.doc, got, c.textOnly)
		}
	}
	var doc map[string]any
	if err := json.Unmarshal(embeddedProfiles, &doc); err != nil {
		t.Fatal(err)
	}
	tiers := doc["profiles"].(map[string]any)
	for _, id := range eg2Tiers {
		delete(tiers[id].(map[string]any), "embeddinggemma2_projector")
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range eg2Tiers {
		res, err := deriveRender(raw, renderReq(id, "windows", &pinnedVLLM{}))
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if res.Params.EG2TextOnly || !strings.Contains(eg2Block(res.Config), "--mmproj") {
			t.Errorf("%s: with embeddinggemma2_projector absent the entry must carry its projector (text-only = %v):\n%s", id, res.Params.EG2TextOnly, eg2Block(res.Config))
		}
	}
}
