package core

import "context"

// The media lane probe (P0 plan, S1).
//
// A media call on a box that has the lane waits for it: the lease line, the card, the host's memory. When another
// node of the fleet stands idle with the same recipe, that wait is the wrong answer, and to know it the router
// (internal/mediaremote) needs ONE read-only question of the pipeline that owns the lane: would this call take its
// lane right now? The pipeline answers it from the same state its own admission reads. It lives in core for the
// reason RemoteAttributor does: the router receives only a small Runner interface and must not import the pipeline.
//
// The contract is a RELAXATION, and the pipeline's differential test is its guard: Free is false only when a
// condition that makes the real admission refuse is present, so a prober never calls a lane busy that the grant would
// have served. It may call a lane free that the grant then refuses (a quarantined card, a race, a table it could not
// read): the call then runs locally as it did before, after that one bounded read. Every doubt degrades toward today's behaviour, never
// toward a placement made on a guess.

// LaneVerdict is a prober's answer for one request.
type LaneVerdict struct {
	// Free: the lane would take the call now, OR the prober cannot say (a task it does not model, a lease this process
	// inherited, a card table that would not read). A router acts on a false Free only.
	Free bool `json:"free"`
	// Why is the sentence for a person: what is in the way when Free is false, or why the prober could not judge when
	// Free is true and the answer is not a plain yes. Empty for a plain yes.
	Why string `json:"why,omitempty"`
	// Holders are the live leases in the way (the cards the call would use, or any lease when it needs the whole node).
	Holders []LaneHolder `json:"holders,omitempty"`
	// Ahead is how many callers hold a place in line that the call would queue behind (live waiters and tokens).
	Ahead int `json:"ahead,omitempty"`
	// HostRAM is the host-RAM guard's own sentence when the memory, not the card, is what is short.
	HostRAM string `json:"host_ram,omitempty"`
}

// LaneHolder is one live lease in the way, as `gpu status` would list it.
type LaneHolder struct {
	Epoch        uint64   `json:"epoch"`
	Class        string   `json:"class"`
	Reason       string   `json:"reason,omitempty"`
	RemainingSec int      `json:"remaining_sec,omitempty"`
	Devices      []string `json:"devices,omitempty"`
}

// LaneProber is implemented by a Runner that can answer the question above. It reads and never writes: no lease, no
// place in line, no ledger row, no card of any kind.
type LaneProber interface {
	MediaLaneFree(ctx context.Context, req Request) LaneVerdict
}

// ProbeLane asks runner whether req would take its local lane now. A runner that cannot say (a test double, a node's
// own runner, nil) reports a free lane, so the call is the one that existed before the probe did.
func ProbeLane(ctx context.Context, runner any, req Request) LaneVerdict {
	if p, ok := runner.(LaneProber); ok {
		return p.MediaLaneFree(ctx, req)
	}
	return LaneVerdict{Free: true}
}
