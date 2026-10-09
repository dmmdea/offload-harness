package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/servingtmpl"
	"github.com/dmmdea/offload-harness/internal/tierseed"
	"github.com/dmmdea/offload-harness/internal/vllmseat"
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
// peak WITHOUT it (--ubatch-size 2048 in both). A row that names no peaks of its own uses these.
const (
	eg2ProjectorPeakMiB = 1536
	eg2TextOnlyPeakMiB  = 482
)

// eg2HeadroomMiB is H, the MiB both arms of the budget keep free on the card. It is zero because the
// repo states no headroom for these tiers: the suite already accepts ampere-6 with 89 MiB to spare. An
// operator decision may set one; it then applies to every tier alike, and
// TestTheEmbeddingGemma2HeadroomTermIsInBothArms must be updated in the same change.
const eg2HeadroomMiB = 0

type eg2Part struct {
	what string
	mib  int
}

// eg2VLLMShare is a row's record for the second arm: the tier's vLLM agent seat takes
// ceil(gpu_memory_utilization x card) of the card the memory-stack residents are pinned to, and what is
// left is the residents' and the entry's. It is measured, on that card, with the template's flags.
type eg2VLLMShare struct {
	residents                 []eg2Part // the memory stack's other residents on the card
	textOnlyMiB, projectorMiB int       // the entry's footprint on that card, without and with the projector
}

func (s eg2VLLMShare) residentsMiB() int {
	n := 0
	for _, p := range s.residents {
		n += p.mib
	}
	return n
}

type eg2BudgetRow struct {
	tier    string
	card    string
	cardMiB int
	beside  []eg2Part
	// textOnlyPeakMiB and projectorPeakMiB are the entry's peaks on this row's card for the sum arm;
	// zero means the reference 6 GB node's figures above.
	textOnlyPeakMiB, projectorPeakMiB int
	// vllmShare is set exactly on the rows whose tier has a vLLM seat on the residents' card.
	vllmShare *eg2VLLMShare
}

func (r eg2BudgetRow) textOnlyPeak() int {
	if r.textOnlyPeakMiB > 0 {
		return r.textOnlyPeakMiB
	}
	return eg2TextOnlyPeakMiB
}

func (r eg2BudgetRow) projectorPeak() int {
	if r.projectorPeakMiB > 0 {
		return r.projectorPeakMiB
	}
	return eg2ProjectorPeakMiB
}

// sum is everything the records put on the card beside the entry, plus the entry at entryMiB.
func (r eg2BudgetRow) sum(entryMiB int) int {
	s := entryMiB
	for _, p := range r.beside {
		s += p.mib
	}
	return s
}

// eg2ShareMiB is what a vLLM seat at gpu_memory_utilization util claims of one card: ceil(util x card).
// The 1e-6 keeps a product that is a whole number in exact arithmetic from rounding up one MiB.
func eg2ShareMiB(util float64, cardMiB int) int {
	return int(math.Ceil(util*float64(cardMiB) - 1e-6))
}

// shareRoom is what the seat's share and the residents leave for the entry, less the headroom h:
// card - ceil(util x card) - residents - h. The row must carry a vllmShare.
func (r eg2BudgetRow) shareRoom(util float64, h int) int {
	return r.cardMiB - eg2ShareMiB(util, r.cardMiB) - r.vllmShare.residentsMiB() - h
}

// eg2UtilityCardResidents are the memory stack's embedder and reranker as they sit on the 3-card tier's
// utility card, measured on the card (RTX 5060 Ti 16 GB, 16,311 MiB; llama.cpp b11490; the template's
// flags; card-total deltas, because per-process reads are N/A under WDDM; 2026-10-09).
var eg2UtilityCardResidents = []eg2Part{
	{"embeddinggemma 300M, card-total delta on the utility card", 505},
	{"bge-reranker-v2-m3, card-total delta on the utility card", 439},
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
// A row has up to two arms. The SUM arm adds every footprint the records put on the card beside the
// entry (the figures the tier's shipped notes cite). The VLLM-SHARE arm exists on the rows whose tier
// declares a vLLM seat on the card the residents are pinned to: the seat claims its utilization share
// of the card first, and the residents and the entry must fit in what is left (eg2TierProblemsAt). The
// share arm carries the figures measured on that card, so a text-only tier's reason does not rest on
// the sum arm's margin alone.
var eg2CardBudget = []eg2BudgetRow{
	{tier: "ampere-6", card: "RTX 3050 6 GB, nominal 6 GiB", cardMiB: 6144, beside: []eg2Part{
		{"qwen3.5-4b-agent, ctx 32768 q8_0 KV (win-cuda.yaml)", 3681},
		{"embeddinggemma 300M (ampere16_coresidency_test.go)", 460},
		{"bge-reranker-v2-m3, up to (ampere16_coresidency_test.go)", 378},
	}},
	// The Windows template renders no reranker, so it is left out here: with the projector the sum is over
	// the card even without it, and text-only it still fits with the Linux template's 378 MiB reranker added.
	{tier: "ampere-8", card: "RTX 3070 Laptop 8 GB, nominal 8 GiB", cardMiB: 8192, beside: []eg2Part{
		{"mimo-9b-agent, ctx 65536 q8_0 KV, peak on the 3070 Laptop (win-cuda.yaml)", 6707},
		{"embeddinggemma 300M (ampere16_coresidency_test.go)", 460},
	}},
	// The sum arm keeps the declared whisper (TestTheBlackwellTripleRowKeepsTheDeclaredWhisperOnPurpose)
	// and the entry peaks of the reference node: the utility card's image and video peaks were not
	// measured, so its 1,237 MiB (loaded, after a text embed) is no peak to set here. The share arm
	// carries the measured figures: the seat's 0.90 share leaves 687 MiB beside the residents. The two
	// residents below therefore stay at the figures the tier's notes in profiles.json cite (458 / 378,
	// measured on the 16 GB co-residency tier); the utility card's own 505 / 439 live in the share arm.
	{tier: "blackwell-3x16", card: "RTX 5060 Ti 16 GB, card 2, 16,311 MiB (docs/FLEET-NODE.md)", cardMiB: 16311, beside: []eg2Part{
		{"vl-8b OCR seat measured alone on card 2 (win-triple-blackwell.yaml)", 11751},
		{"whisper-stt, 2.2 GiB as declared in the tier's layers", 2252},
		{"embeddinggemma 300M (ampere16_coresidency_test.go)", 458},
		{"bge-reranker-v2-m3, up to (ampere16_coresidency_test.go)", 378},
	}, vllmShare: &eg2VLLMShare{residents: eg2UtilityCardResidents, textOnlyMiB: 501, projectorMiB: 1237}},
}

// eg2SeatClaim is what a tier's vLLM seats take of the card the memory-stack residents are pinned to:
// the largest gpu_memory_utilization among the seats whose device list contains that card.
type eg2SeatClaim struct {
	util  float64
	pin   string   // the residents' CUDA_VISIBLE_DEVICES pin, as the tier's own render gives it
	seats []string // ids of the seats on that card
}

func eg2DeviceSet(list string) map[string]bool {
	out := map[string]bool{}
	for _, d := range strings.Split(list, ",") {
		if d = strings.TrimSpace(d); d != "" {
			out[d] = true
		}
	}
	return out
}

// eg2SharesACard says whether an engine on seatDevice (CUDA_VISIBLE_DEVICES) and residents pinned to
// residentPin sit on a common card. An empty list means unpinned, which sees every card, so it shares.
func eg2SharesACard(seatDevice, residentPin string) bool {
	seat, res := eg2DeviceSet(seatDevice), eg2DeviceSet(residentPin)
	if len(seat) == 0 || len(res) == 0 {
		return true
	}
	for d := range res {
		if seat[d] {
			return true
		}
	}
	return false
}

var eg2PinRe = regexp.MustCompile(`CUDA_VISIBLE_DEVICES=(\d+(?:,\d+)*)`)

// eg2ResidentPin is the one CUDA_VISIBLE_DEVICES pin the memory stack's residents (the 300M embedder,
// EmbeddingGemma-2 and the reranker) carry in the tier's own render on every OS it has a template for.
// The share arm sums the residents on one card, so a render that spreads them is a failure here.
func eg2ResidentPin(t *testing.T, id string, p servingProfile) string {
	t.Helper()
	pins := map[string]bool{}
	for _, goos := range []string{"linux", "windows"} {
		if _, err := templateFor(goos, p.Backend); err != nil {
			continue
		}
		res, err := deriveRender(embeddedProfiles, renderReq(id, goos, &pinnedVLLM{}))
		if err != nil {
			t.Fatalf("%s %s: %v", id, goos, err)
		}
		body := nonCommentConfig(res.Config)
		for _, key := range []string{"embeddinggemma", "embeddinggemma2", "bge-reranker-v2-m3"} {
			block := eg2EntryBlock(body, key)
			if block == "" {
				continue // a template without that resident (the Windows 8 GB one has no reranker)
			}
			pin := ""
			if m := eg2PinRe.FindStringSubmatch(block); m != nil {
				pin = m[1]
			}
			pins[pin] = true
		}
	}
	if len(pins) != 1 {
		t.Fatalf("tier %s: the memory stack's residents carry pins %v in its render, want exactly one (the share arm sums them on one card)", id, pins)
	}
	for pin := range pins {
		return pin
	}
	return ""
}

// eg2SeatClaims decides, per tier that carries the entry, whether a vLLM seat shares the residents'
// card, by reading the profile's vllm_seat (and extra_vllm_seats) device lists against the pin of the
// tier's own render. A tier with no vLLM seat, or whose seats sit on other cards, has no entry in the
// map and the share arm is skipped for it, logged.
func eg2SeatClaims(t *testing.T) map[string]*eg2SeatClaim {
	t.Helper()
	profiles, seedProfiles, _ := fleetCapProfiles(t)
	claims := map[string]*eg2SeatClaim{}
	for _, id := range eg2Tiers {
		var seats []vllmseat.Spec
		if s := seedProfiles[id].VLLMSeat; s != nil {
			seats = append(seats, *s)
		}
		seats = append(seats, seedProfiles[id].ExtraVLLMSeats...)
		if len(seats) == 0 {
			t.Logf("tier %s declares no vllm_seat: the vLLM-share arm is skipped", id)
			continue
		}
		pin := eg2ResidentPin(t, id, profiles[id])
		claim := &eg2SeatClaim{pin: pin}
		for _, s := range seats {
			if !eg2SharesACard(s.Device, pin) {
				continue
			}
			claim.seats = append(claim.seats, s.ID)
			if s.GPUMemoryUtilization > claim.util {
				claim.util = s.GPUMemoryUtilization
			}
		}
		if len(claim.seats) == 0 {
			t.Logf("tier %s: its vLLM seat(s) are on other cards than the residents' pin %q: the vLLM-share arm is skipped", id, pin)
			continue
		}
		// the device lists and the pins are comparable only if both count cards in PCI order
		pci := false
		for _, e := range profiles[id].GPUEnv {
			if e == "CUDA_DEVICE_ORDER=PCI_BUS_ID" {
				pci = true
			}
		}
		if !pci {
			t.Errorf("tier %s: the seat's device list and the residents' pin are compared as card indexes, but the tier's gpu_env does not pin CUDA_DEVICE_ORDER=PCI_BUS_ID", id)
		}
		claims[id] = claim
	}
	return claims
}

// eg2TierProblems is the budget verdict for one tier row against the tier's own profile, with the
// repo's headroom (eg2HeadroomMiB). claim is the tier's vLLM seat on the residents' card, nil when it has
// none (eg2SeatClaims).
func eg2TierProblems(row eg2BudgetRow, p servingProfile, claim *eg2SeatClaim) []string {
	return eg2TierProblemsAt(row, p, claim, eg2HeadroomMiB)
}

// eg2TierProblemsAt is the verdict at headroom h, exact in both directions and over both arms. A tier
// that carries the projector needs the projector to fit in EVERY arm. A tier that is text-only needs the
// text-only entry to fit in every arm AND the projector to be refused by at least one, so a text-only tier
// always has a recorded reason, and a measurement that changes the figures forces the projector to be
// turned on (or the tier's row to be corrected) on purpose rather than leaving a stale reason behind.
func eg2TierProblemsAt(row eg2BudgetRow, p servingProfile, claim *eg2SeatClaim, h int) []string {
	var out []string
	if !p.IncludeEmbeddingGemma2 {
		return []string{fmt.Sprintf("tier %s has a row in eg2CardBudget but does not carry include_embeddinggemma2", row.tier)}
	}
	textPeak, projectorPeak := row.textOnlyPeak(), row.projectorPeak()
	withProjector, textOnly := row.sum(projectorPeak), row.sum(textPeak)
	sumFitsProjector := withProjector+h <= row.cardMiB

	// the vLLM-share arm exists only for a tier with a seat on the residents' card, and its record only for that
	haveShare, room, shareText, shareProjector := false, 0, 0, 0
	switch {
	case claim != nil && row.vllmShare == nil:
		out = append(out, fmt.Sprintf("tier %s has a vLLM seat (%s, util %v) on the card its residents are pinned to but eg2CardBudget records no vLLM-share arithmetic for it: "+
			"measure the residents and the entry beside the seat's share on that card and record them in the row's vllmShare",
			row.tier, strings.Join(claim.seats, ", "), claim.util))
	case claim == nil && row.vllmShare != nil:
		out = append(out, fmt.Sprintf("tier %s records a vLLM share in eg2CardBudget but has no vLLM seat on the residents' card: the record is stale, remove its vllmShare", row.tier))
	case claim != nil:
		haveShare, room = true, row.shareRoom(claim.util, h)
		shareText, shareProjector = row.vllmShare.textOnlyMiB, row.vllmShare.projectorMiB
	}
	shareFits := func(entryMiB int) bool { return !haveShare || room >= entryMiB }
	shareNote := func(entryMiB int) string {
		return fmt.Sprintf("the vLLM share (util %v of %d MiB = %d MiB) and the residents (%d MiB) leave %d MiB for an entry of %d MiB",
			claim.util, row.cardMiB, eg2ShareMiB(claim.util, row.cardMiB), row.vllmShare.residentsMiB(), room, entryMiB)
	}

	if !p.eg2TextOnly() {
		if !sumFitsProjector {
			out = append(out, fmt.Sprintf("tier %s carries the entry WITH its projector but the recorded footprints sum to %d MiB (entry %d + %d beside it) on a %s card of %d MiB: "+
				"the residency set would declare a combination the card cannot hold. Measure the co-residency on the box, record it in the tier's notes and in eg2CardBudget, or set embeddinggemma2_projector false",
				row.tier, withProjector, projectorPeak, withProjector-projectorPeak, row.card, row.cardMiB))
		}
		if !shareFits(shareProjector) {
			out = append(out, fmt.Sprintf("tier %s carries the entry WITH its projector but %s: the seat's share leaves the projector no room. "+
				"Measure the co-residency on the box, record it in the tier's notes and in eg2CardBudget, or set embeddinggemma2_projector false",
				row.tier, shareNote(shareProjector)))
		}
		return out
	}
	if textOnly+h > row.cardMiB {
		out = append(out, fmt.Sprintf("tier %s carries the text-only entry but even that does not fit: %d MiB (entry %d + %d beside it) on a %s card of %d MiB",
			row.tier, textOnly, textPeak, textOnly-textPeak, row.card, row.cardMiB))
	}
	if !shareFits(shareText) {
		out = append(out, fmt.Sprintf("tier %s carries the text-only entry but even that does not fit beside the vLLM seat: %s", row.tier, shareNote(shareText)))
	}
	if sumFitsProjector && shareFits(shareProjector) {
		out = append(out, fmt.Sprintf("tier %s is text-only but the recorded footprints now fit the projector too (%d MiB on %d MiB%s): the text-only state has no recorded reason left. "+
			"Turn the projector on deliberately: set embeddinggemma2_projector true, move the tier from eg2TextOnlyTiers to eg2ProjectorTiers and update the tier's notes",
			row.tier, withProjector, row.cardMiB, map[bool]string{true: "; the vLLM share leaves room for it too", false: ""}[haveShare]))
	}
	return out
}

// TestTheEmbeddingGemma2FlagIsOnTheTiersWhoseRecordedFootprintsFitTheCardAndOnlyThem is exact in both
// directions (see eg2TierProblems) and covers every tier that carries the flag: a tier that carries it
// without a row has no recorded arithmetic, which is how a card gets promised a combination it cannot
// hold.
func TestTheEmbeddingGemma2FlagIsOnTheTiersWhoseRecordedFootprintsFitTheCardAndOnlyThem(t *testing.T) {
	profiles, _, ids := fleetCapProfiles(t)
	claims := eg2SeatClaims(t)
	seen := map[string]bool{}
	for _, row := range eg2CardBudget {
		seen[row.tier] = true
		p, ok := profiles[row.tier]
		if !ok {
			t.Errorf("no tier %s: this gate went blind for it", row.tier)
			continue
		}
		for _, problem := range eg2TierProblems(row, p, claims[row.tier]) {
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
// 15,321 on 16,311) and the projector does not (8,703 on 8,192; 16,375 on 16,311), every row at the
// reference 6 GB node's entry peaks (482 / 1,536) because no row names its own. Then it flips the real
// profiles in memory, projector on, and requires the very check the suite runs to refuse them, once per
// arm that has a witness (the 3-card tier is also refused by the vLLM-share arm): this is what stops a
// tier being switched to the projector without a measurement.
func TestTheEmbeddingGemma2ProjectorIsRefusedOnTheTextOnlyTiers(t *testing.T) {
	profiles, _, _ := fleetCapProfiles(t)
	claims := eg2SeatClaims(t)
	byTier := map[string]eg2BudgetRow{}
	for _, r := range eg2CardBudget {
		byTier[r.tier] = r
	}
	for _, want := range []struct {
		tier                    string
		textPeak, projectorPeak int
		textOnly, withProjector int
		cardMiB                 int
		arms                    int // the arms that must each refuse the projector once it is on
	}{
		{"ampere-8", 482, 1536, 7649, 8703, 8192, 1},
		{"blackwell-3x16", 482, 1536, 15321, 16375, 16311, 2},
	} {
		row := byTier[want.tier]
		if row.textOnlyPeak() != want.textPeak || row.projectorPeak() != want.projectorPeak {
			t.Errorf("%s: entry peaks = %d / %d, want the reference node's %d / %d", want.tier, row.textOnlyPeak(), row.projectorPeak(), want.textPeak, want.projectorPeak)
		}
		if got := row.sum(row.textOnlyPeak()); got != want.textOnly || got > row.cardMiB {
			t.Errorf("%s: text-only sum = %d (card %d), want %d and under the card", want.tier, got, row.cardMiB, want.textOnly)
		}
		if got := row.sum(row.projectorPeak()); got != want.withProjector || got <= row.cardMiB || row.cardMiB != want.cardMiB {
			t.Errorf("%s: projector sum = %d on card %d, want %d and over the card %d", want.tier, got, row.cardMiB, want.withProjector, want.cardMiB)
		}
		on := true
		p := profiles[want.tier]
		p.EmbeddingGemma2Projector = &on
		problems := eg2TierProblems(row, p, claims[want.tier])
		if len(problems) != want.arms {
			t.Errorf("%s: switching the projector on must be refused by %d arm(s) of the budget, got %v", want.tier, want.arms, problems)
		}
		for _, problem := range problems {
			if !strings.Contains(problem, "WITH its projector") {
				t.Errorf("%s: switching the projector on must be refused as a projector tier over the card, got %q", want.tier, problem)
			}
		}
	}
	// and the other direction: a tier whose projector fits has no business being text-only
	ampere6 := profiles["ampere-6"]
	off := false
	ampere6.EmbeddingGemma2Projector = &off
	if problems := eg2TierProblems(byTier["ampere-6"], ampere6, claims["ampere-6"]); len(problems) != 1 || !strings.Contains(problems[0], "no recorded reason left") {
		t.Errorf("a text-only ampere-6 must be flagged as having no recorded reason, got %v", problems)
	}
}

// TestEG2BudgetRowsCarryTheirOwnEntryPeaks: the entry's peaks are a property of the card they were
// measured on, so a row may name its own (a measurement on that card) and a row that names none uses
// the reference 6 GB node's 482 / 1,536 MiB. The verdict must read them off the row: with the node's
// 1,536 the projector sum below is over the card (9,736 on 9,600) and the tier is rightly text-only; with
// a row's own measured 1,237 it fits (9,437) and the same text-only tier has lost its reason.
func TestEG2BudgetRowsCarryTheirOwnEntryPeaks(t *testing.T) {
	plain := eg2BudgetRow{tier: "node-x", card: "9,600 MiB", cardMiB: 9600, beside: []eg2Part{{"the agent seat", 8200}}}
	if plain.textOnlyPeak() != 482 || plain.projectorPeak() != 1536 {
		t.Errorf("a row that names no peaks must use the reference 6 GB node's 482 / 1536, got %d / %d", plain.textOnlyPeak(), plain.projectorPeak())
	}
	own := plain
	own.textOnlyPeakMiB, own.projectorPeakMiB = 501, 1237
	if own.textOnlyPeak() != 501 || own.projectorPeak() != 1237 {
		t.Errorf("a row's own peaks must win, got %d / %d", own.textOnlyPeak(), own.projectorPeak())
	}
	if got := plain.sum(plain.projectorPeak()); got != 9736 {
		t.Errorf("default projector sum = %d, want 9736", got)
	}
	if got := own.sum(own.projectorPeak()); got != 9437 {
		t.Errorf("own-peak projector sum = %d, want 9437", got)
	}
	off, on := false, true
	textOnly := servingProfile{IncludeEmbeddingGemma2: true, EmbeddingGemma2Projector: &off}
	withProjector := servingProfile{IncludeEmbeddingGemma2: true, EmbeddingGemma2Projector: &on}
	if p := eg2TierProblems(plain, textOnly, nil); len(p) != 0 {
		t.Errorf("default peaks: the text-only tier is justified, got %v", p)
	}
	if p := eg2TierProblems(plain, withProjector, nil); len(p) != 1 || !strings.Contains(p[0], "9736") {
		t.Errorf("default peaks: the projector must be refused at 9736 MiB, got %v", p)
	}
	if p := eg2TierProblems(own, textOnly, nil); len(p) != 1 || !strings.Contains(p[0], "no recorded reason left") {
		t.Errorf("own peaks: the projector fits, so text-only has no reason left, got %v", p)
	}
	if p := eg2TierProblems(own, withProjector, nil); len(p) != 0 {
		t.Errorf("own peaks: the projector fits and must be accepted, got %v", p)
	}
}

// TestTheSeatShareIsDecidedByTheDevicePinsNotAssumed: whether a tier's vLLM seat shares the card the
// memory-stack residents sit on is read from the profile's vllm_seat device list and from the pin the
// tier's own render gives the three residents, never typed into the row. The table is the rule; the
// real tiers below it are read through the renderer (the vLLM seat itself renders only on a box that
// has the engine's venv, which is why the profile is read, not a render of the seat).
func TestTheSeatShareIsDecidedByTheDevicePinsNotAssumed(t *testing.T) {
	for _, c := range []struct {
		seatDevice, residentPin string
		want                    bool
	}{
		{"0,2", "2", true},
		{"0,2", "0", true},
		{"0,2", "1", false}, // the display card is outside the pair
		{"0", "2", false},
		{"2", "2", true},
		{"0,2", "0,2", true},
		{" 0 , 2 ", "2", true}, // the list is split and trimmed
		{"", "2", true},        // an unpinned engine sees every card
		{"0,2", "", true},      // unpinned residents may land on any card
	} {
		if got := eg2SharesACard(c.seatDevice, c.residentPin); got != c.want {
			t.Errorf("eg2SharesACard(%q, %q) = %v, want %v", c.seatDevice, c.residentPin, got, c.want)
		}
	}
	_, seedProfiles, _ := fleetCapProfiles(t)
	claims := eg2SeatClaims(t)
	var arm, skipped []string
	for _, id := range eg2Tiers {
		if claims[id] != nil {
			arm = append(arm, id)
		} else {
			skipped = append(skipped, id)
		}
	}
	sort.Strings(skipped)
	if strings.Join(arm, ",") != "blackwell-3x16" || strings.Join(skipped, ",") != "ampere-6,ampere-8" {
		t.Errorf("vLLM-share arm on %v, skipped on %v; want it on blackwell-3x16 and skipped (no vllm_seat) on ampere-6, ampere-8", arm, skipped)
	}
	seat := seedProfiles["blackwell-3x16"].VLLMSeat
	if seat == nil || seat.Device != "0,2" {
		t.Fatalf("blackwell-3x16 vllm_seat = %+v, want a seat on devices 0,2", seat)
	}
	if claim := claims["blackwell-3x16"]; claim == nil || claim.pin != "2" || claim.util != seat.GPUMemoryUtilization {
		t.Errorf("blackwell-3x16 claim = %+v, want the residents' pin 2 and the seat's util %v", claim, seat.GPUMemoryUtilization)
	}
	for _, id := range []string{"ampere-6", "ampere-8"} {
		if seedProfiles[id].VLLMSeat != nil || len(seedProfiles[id].ExtraVLLMSeats) != 0 {
			t.Errorf("tier %s now declares a vLLM seat: record its share arithmetic in eg2CardBudget (vllmShare) before this test lets it through", id)
		}
	}
}

// TestTheEmbeddingGemma2EntryFitsBesideTheAgentSeatUtilShare is the second arm of the budget. The
// tier's vLLM agent seat takes ceil(gpu_memory_utilization x card) of each card in its device list, and
// the memory stack's residents sit on one of those cards (the pins above), so what is left for the
// entry beside them is card - ceil(util x card) - residents. Measured on the 3-card tier's utility card
// (RTX 5060 Ti 16 GB, 16,311 MiB; card-total deltas, since per-process reads are N/A under WDDM):
// embeddinggemma 505, bge-reranker-v2-m3 439 -> 944 MiB of residents; at util 0.90 the seat takes 14,680,
// leaving 687. The text-only entry (501 MiB) fits with 186 to spare; the projector (1,237 loaded and
// after a text embed, image and video peaks not measured on that card) is 550 over, and its two GGUFs
// alone are 825 MiB. The same arithmetic at util 0.95 leaves -129 MiB: the profile's own record that
// 0.95 cannot initialise beside the memory-stack embedder. Each deliberate mutation below must be
// refused by the very check the suite runs.
func TestTheEmbeddingGemma2EntryFitsBesideTheAgentSeatUtilShare(t *testing.T) {
	profiles, _, _ := fleetCapProfiles(t)
	claims := eg2SeatClaims(t)
	byTier := map[string]eg2BudgetRow{}
	for _, r := range eg2CardBudget {
		byTier[r.tier] = r
	}
	for _, id := range []string{"ampere-6", "ampere-8"} {
		if claims[id] != nil || byTier[id].vllmShare != nil {
			t.Errorf("tier %s has no vllm_seat: it must carry no share arm (claim %v, row share %v)", id, claims[id], byTier[id].vllmShare)
		}
	}
	const id = "blackwell-3x16"
	row, claim, p := byTier[id], claims[id], profiles[id]
	if claim == nil || row.vllmShare == nil {
		t.Fatalf("%s: claim %v, row share %v: the seat shares the residents' card, so both must exist", id, claim, row.vllmShare)
	}
	if claim.util != 0.9 {
		t.Errorf("%s: seat util = %v, want 0.9", id, claim.util)
	}
	if got := eg2ShareMiB(claim.util, row.cardMiB); got != 14680 {
		t.Errorf("%s: ceil(0.9 x %d) = %d, want 14680", id, row.cardMiB, got)
	}
	if got := row.vllmShare.residentsMiB(); got != 944 {
		t.Errorf("%s: residents = %d MiB, want 944 (505 + 439)", id, got)
	}
	if got := row.shareRoom(claim.util, 0); got != 687 {
		t.Errorf("%s: room beside the seat = %d MiB, want 687", id, got)
	}
	if row.vllmShare.textOnlyMiB != 501 || row.vllmShare.projectorMiB != 1237 {
		t.Errorf("%s: measured entry = %d / %d MiB, want 501 / 1237", id, row.vllmShare.textOnlyMiB, row.vllmShare.projectorMiB)
	}
	if room := row.shareRoom(claim.util, 0); room < row.vllmShare.textOnlyMiB || room >= row.vllmShare.projectorMiB {
		t.Errorf("%s: room %d MiB must hold the text-only entry (%d) and not the projector (%d)", id, room, row.vllmShare.textOnlyMiB, row.vllmShare.projectorMiB)
	}
	if got := eg2TierProblems(row, p, claim); len(got) != 0 {
		t.Errorf("%s as shipped: %v", id, got)
	}
	// mutate runs the verdict on a deep copy of the row, claim and profile after f changed them.
	mutate := func(f func(*eg2BudgetRow, *eg2SeatClaim, *servingProfile)) string {
		r, c, q := row, *claim, p
		share := *row.vllmShare
		share.residents = append([]eg2Part(nil), share.residents...)
		r.vllmShare = &share
		r.beside = append([]eg2Part(nil), row.beside...)
		f(&r, &c, &q)
		return strings.Join(eg2TierProblems(r, q, &c), "\n")
	}
	if got := mutate(func(_ *eg2BudgetRow, _ *eg2SeatClaim, q *servingProfile) {
		on := true
		q.EmbeddingGemma2Projector = &on
	}); !strings.Contains(got, "WITH its projector") || !strings.Contains(got, "vLLM share") {
		t.Errorf("projector on: the vLLM-share arm must refuse it, got %q", got)
	}
	if got := mutate(func(_ *eg2BudgetRow, c *eg2SeatClaim, _ *servingProfile) { c.util = 0.95 }); !strings.Contains(got, "text-only entry") || !strings.Contains(got, "vLLM share") || !strings.Contains(got, "-129") {
		t.Errorf("util raised to 0.95 leaves -129 MiB and must refuse even the text-only entry, got %q", got)
	}
	if got := mutate(func(_ *eg2BudgetRow, c *eg2SeatClaim, _ *servingProfile) { c.util = 0.8 }); got != "" {
		t.Errorf("util lowered to 0.8 frees the projector in this arm but the sum arm still refuses it, the tier stays valid, got %q", got)
	}
	if got := mutate(func(r *eg2BudgetRow, c *eg2SeatClaim, _ *servingProfile) {
		c.util = 0.8
		r.beside = nil // and the sum arm quiet too: the projector now fits everywhere
	}); !strings.Contains(got, "no recorded reason left") {
		t.Errorf("util 0.8 and an empty sum arm: the text-only state has no reason left, got %q", got)
	}
	if got := mutate(func(r *eg2BudgetRow, _ *eg2SeatClaim, _ *servingProfile) { r.vllmShare.residents[0].mib = 700 }); !strings.Contains(got, "text-only entry") || !strings.Contains(got, "vLLM share") {
		t.Errorf("a resident measured at 700 MiB leaves 492 beside the seat, under the text-only entry's 501, got %q", got)
	}
	if got := mutate(func(r *eg2BudgetRow, _ *eg2SeatClaim, _ *servingProfile) { r.vllmShare = nil }); !strings.Contains(got, "records no vLLM-share arithmetic") {
		t.Errorf("a seat on the residents' card with no recorded share must be refused, got %q", got)
	}
	// and the converse: a row that records a share for a tier whose seat is not on the residents' card.
	if got := strings.Join(eg2TierProblems(row, p, nil), "\n"); !strings.Contains(got, "no vLLM seat on the residents' card") {
		t.Errorf("a recorded share with no seat on the card must be refused, got %q", got)
	}
}

// TestTheBlackwellTripleRowKeepsTheDeclaredWhisperOnPurpose: the sum arm adds whisper at the 2.2 GiB the
// tier's layers declare (2,252 MiB), not the ~2,182 MiB measured alone, because the row records what the
// residency set promises is co-resident. That choice is load-bearing in one direction: at the measured
// figure the projector sum is 16,305 MiB, 6 under the card, so the sum arm alone would hand the verdict
// to a 6 MiB hairline. The vLLM-share arm is what keeps the reason when it does: with the measured
// whisper the tier is still valid text-only, and a projector switched on is still refused, by the share
// arm alone.
func TestTheBlackwellTripleRowKeepsTheDeclaredWhisperOnPurpose(t *testing.T) {
	profiles, seedProfiles, _ := fleetCapProfiles(t)
	claims := eg2SeatClaims(t)
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
	if got := row.sum(row.projectorPeak()); got != 16375 || got <= row.cardMiB {
		t.Errorf("declared whisper: projector sum = %d, want 16375 and over the card %d", got, row.cardMiB)
	}
	measured := row
	measured.beside = append([]eg2Part(nil), row.beside...)
	measured.beside[at].mib = 2182
	if got := measured.sum(measured.projectorPeak()); got != 16305 || got > measured.cardMiB {
		t.Errorf("measured whisper: projector sum = %d, want 16305 and under the card %d (the hairline this test records)", got, measured.cardMiB)
	}
	if got := eg2TierProblems(measured, profiles[id], claims[id]); len(got) != 0 {
		t.Errorf("measured whisper, text-only: the share arm keeps the reason, so no problem, got %v", got)
	}
	on := true
	withProjector := profiles[id]
	withProjector.EmbeddingGemma2Projector = &on
	if got := eg2TierProblems(measured, withProjector, claims[id]); len(got) != 1 || !strings.Contains(got[0], "vLLM share") {
		t.Errorf("measured whisper, projector on: exactly the share arm must refuse it, got %v", got)
	}
}

// TestTheEmbeddingGemma2HeadroomTermIsInBothArms: H (eg2HeadroomMiB) is zero because the repo states no
// headroom for these tiers, and the suite accepts ampere-6 with 89 MiB to spare. An operator decision may
// set one; it must then reach both arms. ampere-6 is the sum arm's witness (89 MiB spare, projector
// tier), the 3-card tier the share arm's (186 MiB spare for the text-only entry).
func TestTheEmbeddingGemma2HeadroomTermIsInBothArms(t *testing.T) {
	if eg2HeadroomMiB != 0 {
		t.Errorf("eg2HeadroomMiB = %d: the repo states no headroom for these tiers; an operator decision that sets one must update this test and the budget's comment together", eg2HeadroomMiB)
	}
	profiles, _, _ := fleetCapProfiles(t)
	claims := eg2SeatClaims(t)
	byTier := map[string]eg2BudgetRow{}
	for _, r := range eg2CardBudget {
		byTier[r.tier] = r
	}
	for _, c := range []struct {
		tier         string
		h            int
		wantProblems bool
		wantIn       string
	}{
		{"ampere-6", 89, false, ""},
		{"ampere-6", 90, true, "WITH its projector"},
		{"blackwell-3x16", 186, false, ""},
		{"blackwell-3x16", 187, true, "vLLM share"},
	} {
		got := strings.Join(eg2TierProblemsAt(byTier[c.tier], profiles[c.tier], claims[c.tier], c.h), "\n")
		if (got != "") != c.wantProblems || !strings.Contains(got, c.wantIn) {
			t.Errorf("%s at H = %d MiB: problems %q, want problems = %v naming %q", c.tier, c.h, got, c.wantProblems, c.wantIn)
		}
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

// eg2EntryBlock returns the entry named key of a rendered config: from its key to the next key at the
// same indent, "" when the config defines none.
func eg2EntryBlock(config, key string) string {
	var b strings.Builder
	in := false
	for _, ln := range strings.Split(config, "\n") {
		isKey := strings.HasPrefix(ln, "  ") && len(ln) > 2 && ln[2] != ' '
		if in && isKey {
			break
		}
		if isKey && strings.HasPrefix(strings.TrimSpace(ln), key+":") {
			in = true
		}
		if in {
			b.WriteString(ln)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// eg2Block returns the embeddinggemma2 entry of a rendered config.
func eg2Block(config string) string { return eg2EntryBlock(config, "embeddinggemma2") }

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
