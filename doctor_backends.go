package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/backendchain"
	"github.com/dmmdea/offload-harness/internal/config"
)

// The active-backend section of `doctor` (EXP-1): for every lane and every seat, which serving engine
// is answering, the ordered chain it was picked from, how deep the probe went and why.
//
//	backends:   active_backend per lane and seat ...
//	  lane agent         llama.cpp  chain vllm > llama.cpp  probe process   vllm seat X unavailable: not in the llama-swap roster; fell to llama.cpp seat Y: ...
//	  seat gemma4-e2b    llama.cpp  chain llama.cpp         probe process   llama.cpp seat gemma4-e2b: in the roster, not loaded; ...
//
// The default probe LOADS NOTHING (internal/backendchain): llama-swap's roster and running list, and a
// loaded seat's own health endpoint. `doctor --probe-completion` adds a 1-token completion per seat, and
// that one can load an idle seat (and is skipped where it would evict a vLLM seat), so it exists only
// behind the flag. The section is informational and never changes doctor's exit code, like the
// capacity and fleet-version rows.
//
// It runs AFTER the checks of doctorRunChecked, whatever they returned: "why is nothing serving" is
// exactly what an operator reads this section for when the endpoint is down, so a failed health probe
// must not hide it (the file's own rule for the pure-config rows, kept from the other side).

// runDoctorBackends is the `doctor` verb: the unchanged doctor, then the backends section. It lives
// beside the verb and not inside doctorRunChecked so that main.go carries one changed line, and so the
// new flag is parsed here and never reaches runDoctor's own flag set.
func runDoctorBackends(args []string) error {
	rest, probe := splitProbeCompletion(args)
	err := runDoctor(rest)
	// runDoctor has already accepted `rest` (its flag set exits on a bad flag), so this parse only
	// reads --config the way runDoctor read it, quietly: the loader's warnings were printed once.
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("config", "", "config file path")
	if fs.Parse(rest) != nil {
		return err
	}
	cfg, _ := config.LoadWithSource(*cfgPath)
	writeBackendsSection(os.Stdout, cfg, probe)
	return err
}

// splitProbeCompletion takes --probe-completion (also -probe-completion, and =true / =false) out of
// args and says whether it asked for the probe. Everything else, in order, goes on to runDoctor.
func splitProbeCompletion(args []string) (rest []string, probe bool) {
	for i, a := range args {
		if a == "--" {
			return append(rest, args[i:]...), probe
		}
		name := strings.TrimLeft(a, "-")
		if len(a)-len(name) > 0 && len(a)-len(name) <= 2 {
			if name == "probe-completion" {
				probe = true
				continue
			}
			if v, ok := strings.CutPrefix(name, "probe-completion="); ok {
				b, err := strconv.ParseBool(v)
				if err != nil {
					rest = append(rest, a) // let runDoctor's flag set refuse it by name
					continue
				}
				probe = b
				continue
			}
		}
		rest = append(rest, a)
	}
	return rest, probe
}

// writeBackendsSection collects and prints the section. A box with no local model (a delegation client)
// has no lane or seat of its own to report and prints nothing.
func writeBackendsSection(w io.Writer, cfg config.Config, probeCompletion bool) {
	if !hasLocalModel(cfg) {
		return
	}
	if probeCompletion {
		fmt.Fprintln(w, "backends:   running a 1-token completion per seat (--probe-completion): this can load idle seats, and a cold load takes minutes ...")
	}
	rep := backendchain.Collect(context.Background(), cfg, backendchain.Options{HTTP: maintenanceClient, ProbeCompletion: probeCompletion})
	writeBackendRows(w, rep, probeCompletion)
}

// writeBackendRows prints a collected report.
func writeBackendRows(w io.Writer, rep backendchain.Report, probeCompletion bool) {
	if len(rep.Lanes) == 0 && len(rep.Seats) == 0 {
		return
	}
	how := "the default probe loads nothing; --probe-completion adds a 1-token completion that CAN load a model"
	if probeCompletion {
		how = "completion probe ON: idle seats may have been loaded by this run"
	}
	fmt.Fprintf(w, "backends:   active_backend per lane and seat (%s)\n", how)
	width := 0
	for _, e := range rep.Lanes {
		width = max(width, len(e.Lane))
	}
	for _, e := range rep.Seats {
		width = max(width, len(e.Seat))
	}
	row := func(kind, name string, e backendchain.Entry) {
		fmt.Fprintf(w, "  %-4s %-*s  %-10s chain %-24s probe %-10s %s\n",
			kind, width, name, e.ActiveBackend, strings.Join(e.BackendChain, " > "), e.BackendProbe, e.BackendReason)
	}
	for _, e := range rep.Lanes {
		row("lane", e.Lane, e)
	}
	for _, e := range rep.Seats {
		row("seat", e.Seat, e)
	}
}

// tierVLLMSeat is the tier-table reader backendchain's lane plan uses: when the tier this box is
// installed as declares a vLLM agent seat and agentSeat is that seat or its llama.cpp fallback, the seat
// and its fallback. It is the same reading the capacity row makes (vllmTierOf), so doctor, the capacity
// row and /fleet/health agree on which tier a box is.
func tierVLLMSeat(cfg config.Config, agentSeat string) (backendchain.TierSeat, bool) {
	t := vllmTierOf(cfg, agentSeat)
	if !t.serving && !t.fallback {
		return backendchain.TierSeat{}, false
	}
	return backendchain.TierSeat{ID: t.seat.ID, Aliases: t.seat.Aliases, Fallback: t.seat.Fallback}, true
}

// init installs the reader: the tier table is embedded in this package, and /fleet/health
// (internal/fleetnode) plans its lanes from the same place doctor does. A hook and not an Options field,
// so wiring it costs main.go nothing.
func init() { backendchain.SetTierLookup(tierVLLMSeat) }
