package delegate

// faultdemotion.go: ADR 0096. A card whose driver met NVSentinel's repeat rule (one fault class in 5 or more bursts
// within 24 h, bursts at a restart or sleep transition left out) is a card where something keeps resetting the
// engines, and a reset on such a card ends the contexts on it, whoever caused it. The node publishes the reading as
// gpu_faults (internal/gpuactivity/faultstore.go), and a repeat is LIVE while its last burst is within 24 h.
//
// DEMOTE, NEVER EXCLUDE, as a lease-busy node is demoted: a remote whose every seat for the contract sits on a card
// with a live repeat ranks below every node that has a seat off such cards (right after saturation in betterRemote
// and fitPickWith), and still takes work when nothing better is left. The local seat reads busy for the deal when
// its cards carry a live repeat and a remote could take the work (never for route=local, never with no remotes); the
// capacity wait reads the seat afresh, so an overflow subtask may still come back to it.
//
// Cause-agnostic: the reading does not ask whether the harness caused the faults. The reference box's repeat
// (2026-10-11, 5 bursts of nvlddmkm 153 on one card in 82 minutes) ran with no harness work on that card.

import (
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	placetable "github.com/dmmdea/offload-harness/internal/placement"
)

// faultRepeatLive is how long after its last burst a repeat demotes.
const faultRepeatLive = 24 * time.Hour

// Seams: the clock the live window is judged by, and the local summary reader.
var (
	faultNow              = time.Now
	readLocalFaultSummary = func(cfg config.Config) *gpuactivity.FaultSummary {
		path, err := gpuactivity.FaultSummaryPath(cfg.StateDir)
		if err != nil {
			return nil
		}
		s, _ := gpuactivity.ReadSummary(path)
		return s
	}
)

// liveRepeats are the summary's repeats whose last burst is within faultRepeatLive of now and that name a card.
func liveRepeats(s *gpuactivity.FaultSummary, now time.Time) []gpuactivity.FaultSummaryRepeat {
	if s == nil {
		return nil
	}
	var out []gpuactivity.FaultSummaryRepeat
	for _, r := range s.Repeats {
		if strings.TrimSpace(r.UUID) != "" && now.Sub(r.To) <= faultRepeatLive {
			out = append(out, r)
		}
	}
	return out
}

func repeatCards(rs []gpuactivity.FaultSummaryRepeat) []string {
	ids := make([]string, 0, len(rs))
	for _, r := range rs {
		ids = append(ids, strings.ToLower(strings.TrimSpace(r.UUID)))
	}
	return ids
}

// seatsOnFaultedCards says whether every seat of a chain sits on a card in live: false as soon as one seat whose
// cards are known sits on none of them (the placement table falls back to it). A seat whose cards are unknown is
// every card.
func seatsOnFaultedCards(seats [][]string, live []string) bool {
	for _, ids := range seats {
		if len(ids) > 0 && !sharesCard(ids, live) {
			return false
		}
	}
	return true
}

// faultDemoted is the ranking key: every seat of v the contract could run on sits on a card with a live repeat.
// Whole-node (any live repeat demotes) where nothing narrows: a nil st, a node that published no layer rows, a
// long-context contract, a layer the node does not declare. A node that publishes no gpu_faults is never demoted.
func faultDemoted(v NodeView, st *Subtask, now time.Time) bool {
	live := repeatCards(liveRepeats(v.GpuFaults, now))
	if len(live) == 0 {
		return false
	}
	if st == nil {
		return true
	}
	cards, _ := gpuprobe.BuildCards(v.Devices, "")
	seats, ok := placetable.RemoteSeatCards(v.Layers, cards, st.Contract, st.EstTokens)
	if !ok {
		return true
	}
	return seatsOnFaultedCards(seats, live)
}

// faultDetail names a node's live repeats for a placement reason: "nvlddmkm 153 x5 on 65:00, last burst 14:33Z".
func faultDetail(v NodeView, now time.Time) string {
	var parts []string
	for _, r := range liveRepeats(v.GpuFaults, now) {
		parts = append(parts, r.Class+" x"+strconv.Itoa(r.Bursts)+" on "+r.Bus+", last burst "+r.To.UTC().Format("01-02 15:04Z"))
	}
	return strings.Join(parts, "; ")
}

// localFaulted is the deal's reading of this box's own cards: true when some subtask's every local seat sits on a
// card with a live repeat in the summary fleet-serve wrote, and a remote could take the work. No summary, an
// unreadable one, or a box with no fleet-serve reads not faulted.
func (r *runner) localFaulted(subtasks []core.AgentContract) (bool, string) {
	if r.route == "local" || len(r.remotes) == 0 {
		return false, ""
	}
	s := readLocalFaultSummary(r.cfg)
	rs := liveRepeats(s, faultNow())
	if len(rs) == 0 {
		return false, ""
	}
	why := "a live driver-fault repeat on this box (ADR 0096): " + faultDetail(NodeView{GpuFaults: s}, faultNow())
	live := repeatCards(rs)
	cards := make([]gpuprobe.Card, 0, len(s.Cards))
	for _, c := range s.Cards {
		cards = append(cards, gpuprobe.Card{UUID: c.UUID, NvidiaIndex: c.Index})
	}
	if len(subtasks) == 0 {
		return true, why
	}
	for _, c := range subtasks {
		seats, ok := placetable.LocalSeatCards(r.cfg, cards, c)
		if !ok || seatsOnFaultedCards(seats, live) {
			return true, why
		}
	}
	return false, ""
}
