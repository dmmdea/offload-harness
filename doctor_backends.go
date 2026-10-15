package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/backendchain"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/servingtmpl"
)

// The active-backend section of `doctor`: for every lane and every seat, which serving engine
// is answering, the ordered chain it was picked from, how deep the probe went and why.
//
//	backends:   active_backend per lane and seat ...
//	  lane agent         llama.cpp  chain vllm > llama.cpp  probe process   vllm seat X unavailable: not in the llama-swap roster; fell to llama.cpp seat Y: ...
//	  seat gemma4-e2b    llama.cpp  chain llama.cpp         probe process   llama.cpp seat gemma4-e2b: in the roster, not loaded; ...
//
// The default probe LOADS NOTHING (internal/backendchain): llama-swap's roster and running list, and a
// loaded seat's own health endpoint. `doctor --probe-completion` adds a 1-token completion to each seat
// that is already running and, on a box where nothing is running, to one idle seat, which it loads; it
// never loads a seat over a running one, so it exists only behind the flag. The section is informational
// and never changes doctor's exit code, like the capacity and fleet-version rows.
//
// It runs AFTER the checks of doctorRunChecked, whatever they returned: "why is nothing serving" is
// exactly what an operator reads this section for when the endpoint is down, so a failed health probe
// must not hide it (the file's own rule for the pure-config rows, kept from the other side).

// runDoctorBackends is the `doctor` verb: the unchanged doctor, then the backends section. It lives
// beside the verb and not inside doctorRunChecked so that main.go carries one changed dispatch line (and
// one usage line), and so the new flag is parsed here and never reaches runDoctor's own flag set.
//
// The flags are checked here too, before runDoctor runs. Its flag set knows only --config and exits the
// process on anything else after printing a usage that omits --probe-completion, which is the flag a
// person most needs to see after a typo (it is the only door to a completion, and a completion can load
// an idle seat). A flag this verb does not accept is refused with the verb's own usage instead.
func runDoctorBackends(args []string) error {
	if doctorHelp(args, os.Stderr) {
		return nil
	}
	rest, probe := splitProbeCompletion(args)
	fs := doctorFlags()
	if perr := fs.Parse(rest); perr != nil {
		return doctorUsageError(perr, fs)
	}
	err := runDoctor(rest)
	// runDoctor read --config the way this parse did; reading it quietly again means the loader's
	// warnings were printed once, by runDoctor.
	cfg, _ := config.LoadWithSource(fs.Lookup("config").Value.String())
	writeBackendsSection(os.Stdout, cfg, probe)
	return err
}

// doctorFlags is the flag set of the doctor verb, both flags, parsed quietly: it prints nothing itself, so
// a caller decides what a person sees. It is the one place the flags are declared, and the help and the
// refusal of a mistyped flag print its declarations, so neither can drift from what the verb accepts.
func doctorFlags() *flag.FlagSet {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("config", "", "config file path")
	fs.Bool("probe-completion", false, "also send a 1-token completion to each seat that is already running and, on a box where nothing is running, to one idle seat: this CAN LOAD one idle seat (a cold load takes minutes), so it is for a person to run; it never loads a seat over a running one")
	return fs
}

// writeDoctorUsage prints the usage of the doctor verb: the usage line, what it does, and each flag as
// the flag package prints it.
func writeDoctorUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintln(w, "usage: local-offload doctor [--config FILE] [--probe-completion]")
	fmt.Fprintln(w, "  checks endpoint health and config, then prints which engine answers for each lane and seat")
	fs.SetOutput(w)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
}

// doctorUsageError is the refusal of a flag the verb does not accept: the flag package's own error line,
// then the usage, with exit status 2 as the flag package's ExitOnError ends the process.
func doctorUsageError(parseErr error, fs *flag.FlagSet) error {
	var b strings.Builder
	fmt.Fprintln(&b, parseErr)
	writeDoctorUsage(&b, fs)
	return &exitCodeError{code: 2, msg: strings.TrimSuffix(b.String(), "\n")}
}

// doctorHelp prints the usage of the doctor verb, both flags, and reports true when args ask for help
// (-h, -help or --help, read the way the flag package reads them). runDoctor's own flag set knows only
// --config and exits the process on help, so help is answered here, before it runs: `doctor -h` is where
// an operator learns that --probe-completion exists and that it can load an idle seat.
func doctorHelp(args []string, w io.Writer) bool {
	fs := doctorFlags()
	if err := fs.Parse(args); !errors.Is(err, flag.ErrHelp) {
		return false
	}
	writeDoctorUsage(w, fs)
	return true
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
		fmt.Fprintln(w, "backends:   running a 1-token completion on the seats that are running (--probe-completion): on a box where nothing is running this loads one idle seat, and a cold load takes minutes ...")
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
		how = "completion probe ON: this run loaded at most one idle seat, and only where nothing was running"
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
	// The capacity row breaks a tie between two tiers that both explain agentSeat by the serving config's
	// own entry for it; read the same file here, so both readers settle on the same tier. An unset or
	// unreadable file leaves the entry empty, and the tier_profile-then-manifest order decides.
	entry := ""
	if path := strings.TrimSpace(cfg.ServingConfigPath); path != "" {
		if text, err := os.ReadFile(path); err == nil {
			entry = servingtmpl.SeatEntryName(string(text), agentSeat)
		}
	}
	t := vllmTierOf(cfg, agentSeat, entry)
	if !t.serving && !t.fallback {
		return backendchain.TierSeat{}, false
	}
	return backendchain.TierSeat{ID: t.seat.ID, Aliases: t.seat.Aliases, Fallback: t.seat.Fallback}, true
}

// init installs the reader: the tier table is embedded in this package, and /fleet/health
// (internal/fleetnode) plans its lanes from the same place doctor does. A hook and not an Options field,
// so wiring it costs main.go nothing.
func init() { backendchain.SetTierLookup(tierVLLMSeat) }
