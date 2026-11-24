package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/hostneed"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// writeSeatHostRAMSection prints one row for the host RAM a vLLM agent seat holds, which the node must declare in
// agent_seat_host_ram_gib (register B-56 / H-08). The seat's host memory is the VM that carries it, the pinned L1
// staging buffer (SEAT_L1_GB) and about 1.1 GiB of process overhead on top, and nothing bounded it: on 2026-10-18 a
// host's commit reached 168 of 171.7 GiB with the seat's share declared nowhere. The warm-back of a seat a lease
// unloaded is admitted against that declaration (internal/hostneed.SeatNeed), and unset it is a CHOSEN 21 GiB, not a
// measurement.
//
//	seat ram:   WARN  vLLM seat <seat>: the agent seat's host footprint is not configured (agent_seat_host_ram_gib), ...
//	seat ram:   OK    agent_seat_host_ram_gib 24 GiB is declared for vLLM seat <seat>; ...; no commit is stated, ...
//	seat ram:   OK    agent_seat_host_ram_gib 24 GiB and agent_seat_host_commit_gib 30 GiB are declared for vLLM seat <seat>; ...
//
// agent_seat_host_commit_gib is read only with the ram figure: set alone, the WARN row says it is ignored.
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
	// The harness measures every cold load of the seat it starts (internal/pipeline/seatmeasure.go). Once it holds one, the row
	// states the measured need and the run count, and the config's figures as the seed they are.
	if need, runs := pipeline.SeatNeedFor(cfg); runs > 0 && need.Source == hostneed.SourceMeasured {
		fmt.Fprintf(w, "seat ram:   OK    vLLM seat %s: %s; a cold load is judged by %.1f GiB touched and %.1f GiB of commit (agent_seat_host_ram_gib and agent_seat_host_commit_gib are the seed for a seat that has not been measured)\n", seat, need.Detail, need.GiB, need.CommitGiB)
		return
	}
	const operatorsWord = "it is the operator's statement and is not compared with a live reading here (the seat log's `host rss` line is the staging buffer's share at start, the loaded seat holds more)"
	if g := cfg.AgentSeatHostRAMGiB; g > 0 {
		if c := cfg.AgentSeatHostCommitGiB; c > 0 {
			fmt.Fprintf(w, "seat ram:   OK    agent_seat_host_ram_gib %g GiB and agent_seat_host_commit_gib %g GiB are declared for vLLM seat %s; the commit is judged against the OS commit limit where the OS enforces one (always on Windows; on Linux with vm.overcommit_memory=2), and in Linux mode 0 or 1 a stated commit is not judged, so only the touched set is; %s\n", g, c, seat, operatorsWord)
			return
		}
		fmt.Fprintf(w, "seat ram:   OK    agent_seat_host_ram_gib %g GiB is declared for vLLM seat %s; %s; no commit is stated (agent_seat_host_commit_gib), so the seat's commit is held to physical RAM\n", g, seat, operatorsWord)
		return
	}
	need := hostneed.SeatNeed(cfg)
	ignored := ""
	if cfg.AgentSeatHostCommitGiB > 0 {
		ignored = "; agent_seat_host_commit_gib is ignored until agent_seat_host_ram_gib is set"
	}
	fmt.Fprintf(w, "seat ram:   WARN  vLLM seat %s: %s; the seat log has a `host rss` line from every start (the pinned L1 staging buffer plus about 1.1 GiB), add the engine's resident set from the loaded seat and record the figure as `measured <date> <node>`%s\n", seat, need.Detail, ignored)
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
