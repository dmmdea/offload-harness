package seatload

// Room for one more agent run on a loaded vLLM seat.
//
// The local seat's line admits a fixed number of runs (fleet_max_concurrent_jobs, 4 when nothing is set). The number says
// nothing about the engine: on the reference 3-card box the agent seat serves 32 sequences and a KV pool of 115k tokens, so a
// fan-out of eight small contracts waited in line at four with a third of the pool in use, while four contracts of 40k tokens
// each already fill it. ReadRoom asks the engine instead, and the line lets one run past the fixed number while the answer is
// yes (modelaffinity.SeatLine).
//
// What was measured there (27B on two 16 GB cards, steady decode, small prompts):
//
//	running   tok/s in all   tok/s each   KV pool
//	      1             28           28        8 %
//	      4             99           25       30 %
//	      6            125           21       45 %
//	     14            137           10       96 %
//
// and during a long prompt the engine decodes about 0.5 tok/s for every run, because each 1,600-token slice of the prompt is one
// step of 1.8 s. Two readings a second apart tell the three states apart (fast, full, reading a prompt) without knowing the
// model, the cards or the prompt: the rate per run is what a run already there loses.

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

const (
	// roomWindow is the time between the two readings the rate is taken from.
	roomWindow = time.Second
	// roomKVCeiling is the share of the KV pool the seat may be expected to use with the run admitted. Chosen: vLLM preempts and
	// recomputes when the pool is full, and runs grow after they are admitted.
	roomKVCeiling = 0.85
	// roomRunKV is the least share of the pool one run is counted at. Chosen: a run read in its first step holds a few hundred
	// tokens and grows by every tool result it reads, so its own current share is not what it will use.
	roomRunKV = 0.10
	// roomMinTokS is the decode rate per running request below which no run is added. 10 tok/s is what each of 14 runs got on the
	// reference box with the pool 96 % full: a thousand-token answer in 100 s.
	roomMinTokS = 10.0
)

// Room is one answer of ReadRoom: whether the engine takes one more run, and what that rests on, in words.
type Room struct {
	OK  bool
	Why string
}

// ReadRoom reads the seat's engine twice, roomWindow apart, and says whether it takes one more run while held runs hold a slot on
// the seat. Yes only on evidence: a loaded vLLM engine with nothing queued, nothing preempted between the readings, requests
// running at roomMinTokS each or better, and a KV pool that stays under roomKVCeiling with every held run and the new one counted
// at their present average, or at roomRunKV where that is more. An engine that cannot be read, is loading, is not vLLM, or has
// nothing running (no rate to judge by) is not room.
func ReadRoom(ctx context.Context, client *http.Client, endpoint, seat string, held int) Room {
	a, why := readServing(ctx, client, endpoint, seat)
	if why != "" {
		return Room{Why: why}
	}
	runs := max(held, a.Running)
	per := max(a.KVUsage/float64(a.Running), roomRunKV)
	if want := max(a.KVUsage, per*float64(runs)) + per; want > roomKVCeiling {
		return Room{Why: fmt.Sprintf("the engine's KV pool is %.0f%% used by %d run(s), and one more would take it to %.0f%%", 100*a.KVUsage, runs, 100*want)}
	}
	t := time.NewTimer(roomWindow)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return Room{Why: "the engine was not read twice in time"}
	case <-t.C:
	}
	b, why := readServing(ctx, client, endpoint, seat)
	if why != "" {
		return Room{Why: why}
	}
	if a.Preemptions >= 0 && b.Preemptions > a.Preemptions {
		return Room{Why: "the engine preempted a request between two readings"}
	}
	gen, _, ok := LiveRates(a, b)
	if !ok {
		return Room{Why: "the engine's token counters gave no rate"}
	}
	each := gen / float64(max(a.Running, b.Running))
	if each < roomMinTokS {
		return Room{Why: fmt.Sprintf("the engine gives its %d running request(s) %.1f tok/s each, under %.0f", b.Running, each, roomMinTokS)}
	}
	return Room{OK: true, Why: fmt.Sprintf("the engine takes one more: %d running at %.0f tok/s each, none queued, KV pool %.0f%% used", b.Running, each, 100*b.KVUsage)}
}

// readServing is one reading of a vLLM engine that is loaded and serving with nothing queued, or why it is not.
func readServing(ctx context.Context, client *http.Client, endpoint, seat string) (Activity, string) {
	act, err := ReadActivity(ctx, client, endpoint, seat)
	switch {
	case err != nil:
		return act, "the engine could not be read: " + err.Error()
	case !act.Loaded:
		return act, "the seat is not loaded"
	case act.Starting:
		return act, act.Summary()
	case act.ActSource != "vllm-metrics":
		return act, "the engine (" + act.ActSource + ") reports no KV pool to judge by"
	case act.KVUsage < 0:
		return act, "the engine reports no KV pool usage"
	case act.Waiting > 0:
		return act, fmt.Sprintf("the engine already queues %d request(s)", act.Waiting)
	case act.Running == 0:
		return act, "the engine has no request running, so there is no rate to judge by"
	}
	return act, ""
}
