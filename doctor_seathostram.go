package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/hostneed"
)

// writeSeatHostRAMSection prints one row for the host RAM a vLLM agent seat holds, which the node must declare in
// agent_seat_host_ram_gib (register B-56 / H-08). The seat's host memory is the VM that carries it, the pinned L1
// staging buffer (SEAT_L1_GB) and about 1.1 GiB of process overhead on top, and nothing bounded it: on 2026-10-18 a
// host's commit reached 168 of 171.7 GiB with the seat's share declared nowhere. The warm-back of a seat a lease
// unloaded is admitted against that declaration (internal/hostneed.SeatNeed), and unset it is a CHOSEN 21 GiB, not a
// measurement.
//
//	seat ram:   WARN  vLLM seat <seat>: the agent seat's host footprint is not configured (agent_seat_host_ram_gib), ...
//	seat ram:   OK    agent_seat_host_ram_gib 24 GiB is declared for vLLM seat <seat>; ...
//
// A box counts as running a vLLM seat when its vllm_seats names one, or when the tier table declares a vLLM seat and
// this box's agent_model is it: the same two statements the capacity row and the backends section read, and nothing
// is sniffed. A box with no such seat prints nothing, so config.Default() stays as short as it is.
//
// The row compares nothing with a live reading. The seat's resident set is read inside the distro that runs it
// (seat_fg.sh logs `host rss N GiB (L1 N GiB)` to the seat log at every start), and this process holds no cheap read
// of that distro's memory: on Windows the VM is the vmmem process, and the harness enumerates processes by parent, not
// by name. So the WARN is for the declaration that is missing, and a declaration that is present reads OK with the
// reminder that it is the operator's statement. It is informational, like the capacity and power rows: it never
// changes doctor's exit code, and it reads the config alone, so a serving layer that is down cannot hide it.
func writeSeatHostRAMSection(w io.Writer, cfg config.Config) {
	seat := vllmSeatOfBox(cfg)
	if seat == "" {
		return
	}
	if g := cfg.AgentSeatHostRAMGiB; g > 0 {
		fmt.Fprintf(w, "seat ram:   OK    agent_seat_host_ram_gib %g GiB is declared for vLLM seat %s; it is the operator's statement and is not compared with a live reading here (the seat log's `host rss` line is the staging buffer's share at start, the loaded seat holds more)\n", g, seat)
		return
	}
	need := hostneed.SeatNeed(cfg)
	fmt.Fprintf(w, "seat ram:   WARN  vLLM seat %s: %s; the seat log has a `host rss` line from every start (the pinned L1 staging buffer plus about 1.1 GiB), add the engine's resident set from the loaded seat and record the figure as `measured <date> <node>`\n", seat, need.Detail)
}

// vllmSeatOfBox names the vLLM agent seat this box runs, or "" when it runs none: the first seat of vllm_seats, else
// the tier table's vLLM seat when this box's agent_model is it. An empty agent_model matches nothing (vllmTierOf would
// read it as an empty fallback name).
func vllmSeatOfBox(cfg config.Config) string {
	for _, s := range cfg.VLLMSeats {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	agent := strings.TrimSpace(cfg.AgentModel)
	if agent == "" {
		return ""
	}
	if t := vllmTierOf(cfg, agent, ""); t.serving {
		return t.seat.ID
	}
	return ""
}
