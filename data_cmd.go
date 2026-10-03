package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/dmmdea/offload-harness/internal/datahome"
	"github.com/dmmdea/offload-harness/internal/volumes"
)

// `local-offload data …` is the operator-facing side of the data-drive rule (register
// C-92: C: holds Windows and program installs, never data). `status` is the audit
// doctor summarises, in full; `migrate` is the way a node that already runs from the
// OS drive gets off it: a COPY, verified file by file, that never moves, deletes or
// rewrites the old tree and never puts a junction behind it.
func runData(args []string) error {
	// `local-offload --config X data migrate` reaches here with the hoisted flag in
	// front of the subcommand; put it behind, where the sub's own flag set reads it.
	if len(args) >= 3 && (args[0] == "--config" || args[0] == "-config") {
		args = append([]string{args[2], args[0], args[1]}, args[3:]...)
	}
	if len(args) == 0 {
		return errors.New("usage: local-offload data <status|migrate> [flags]")
	}
	switch args[0] {
	case "status":
		return runDataStatus(args[1:], os.Stdout)
	case "migrate":
		return runDataMigrate(args[1:], os.Stdout)
	default:
		return fmt.Errorf("unknown data subcommand %q (want status or migrate)", args[0])
	}
}

func runDataStatus(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("data status", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "emit the audit as JSON")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)

	locs := datahome.Locations(cfg)
	rep := liveDataReport(cfg)
	if rep == nil {
		rep = &datahome.Report{}
	}
	onOS := map[string]bool{}
	for _, l := range rep.OnOS {
		onOS[l.Key] = true
	}
	if *asJSON {
		b, _ := json.MarshalIndent(map[string]any{"locations": locs, "report": rep, "failing": rep.Failing()}, "", "  ")
		fmt.Fprintln(w, string(b))
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "KEY\tDRIVE\tPATH")
		for _, l := range locs {
			drive := volumes.DriveOf(l.Path)
			note := ""
			if onOS[l.Key] {
				note = "  <== OS drive"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s%s\n", l.Key, dash(drive), l.Path, note)
		}
		_ = tw.Flush()
		switch {
		case rep.OSDrive == "":
			fmt.Fprintln(w, "\nthis host has no OS drive letter: the data-drive rule does not apply")
		case len(rep.OnOS) == 0:
			fmt.Fprintf(w, "\nno data location is on the OS drive %s\n", rep.OSDrive)
		default:
			fmt.Fprintln(w)
			writeDataHomeSection(w, rep)
		}
	}
	if rep.Failing() {
		return fmt.Errorf("%d data location(s) on the OS drive %s while %s qualifies to hold them", len(rep.OnOS), rep.OSDrive, rep.Target.Volume.Root)
	}
	return nil
}

func runDataMigrate(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("data migrate", flag.ExitOnError)
	fs.String("config", "", "config file path")
	from := fs.String("from", "", "the tree the node runs from today (default: the loaded config's home / install root)")
	to := fs.String("to", "", "the new home (default: a fixed directory on the volume the install-volume rule picks; never the OS drive)")
	apply := fs.Bool("apply", false, "perform the copy; without it this only prints the plan")
	stopped := fs.Bool("stopped", false, "fleet-serve and every MCP door are stopped: copy the bbolt stores (*.db) too")
	asJSON := fs.Bool("json", false, "emit the result as JSON")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)

	osDrive := doctorOSDrive()
	src := *from
	if src == "" {
		src = cfg.BaseDir()
	}
	dst := *to
	if dst == "" {
		vols, err := doctorVolumes()
		if err != nil {
			return fmt.Errorf("enumerating volumes: %w (pass --to)", err)
		}
		choice, err := datahome.DataTarget(vols, osDrive)
		if err != nil {
			return fmt.Errorf("%w (pass --to to name the new home)", err)
		}
		dst = datahome.ProposedHome(choice.Volume.Root)
	}

	res, err := datahome.Run(datahome.Options{From: src, To: dst, OSDrive: osDrive, Apply: *apply, Stopped: *stopped})
	if err != nil {
		return err
	}
	if *asJSON {
		b, _ := json.MarshalIndent(map[string]any{"result": res, "incomplete": res.Incomplete(), "home": datahome.HomeValue(res.To)}, "", "  ")
		fmt.Fprintln(w, string(b))
	} else {
		printMigration(w, res, *apply)
	}
	// A plan always lists what it would hold back; only an apply that could not carry
	// everything is a failure a wrapper must stop on.
	if *apply && res.Incomplete() {
		return fmt.Errorf("%d file(s) still need attention (held, unreadable, changed or failed); fix them and re-run before switching home", res.Count(datahome.StatusHeld)+res.Count(datahome.StatusUnreadable)+res.Count(datahome.StatusChanged)+res.Count(datahome.StatusFailed))
	}
	return nil
}

// printMigration renders the plan or the outcome: counts first, then every file that is
// not a plain copy or an identical one (those are the ones that need a decision).
func printMigration(w io.Writer, res datahome.Result, applied bool) {
	verb := "would copy"
	if applied {
		verb = "copied"
	}
	fmt.Fprintf(w, "from %s\n  to %s\n", res.From, res.To)
	for _, it := range res.Items {
		switch it.Status {
		case datahome.StatusCopy, datahome.StatusSame:
			continue
		}
		fmt.Fprintf(w, "  %-10s %s — %s\n", it.Status, it.Rel, it.Why)
	}
	fmt.Fprintf(w, "%s %d file(s), %s; %d already identical; %d kept (destination newer); %d held; %d skipped; %d unreadable; %d changed; %d failed\n",
		verb, res.Count(datahome.StatusCopy), volumes.Human(uint64(res.Bytes(datahome.StatusCopy))), res.Count(datahome.StatusSame),
		res.Count(datahome.StatusKept), res.Count(datahome.StatusHeld), res.Count(datahome.StatusSkipped),
		res.Count(datahome.StatusUnreadable), res.Count(datahome.StatusChanged), res.Count(datahome.StatusFailed))
	switch {
	case !applied:
		fmt.Fprintln(w, "dry run: nothing was written. Re-run with --apply (and --stopped once fleet-serve and the MCP doors are stopped).")
	case res.Incomplete():
		fmt.Fprintln(w, "not complete: do not switch home yet.")
	default:
		fmt.Fprintf(w, "complete. Next: in config.json set  \"home\": \"%s\"  (an explicit path key you wrote by hand keeps its value: change it too), restart the doors, run `local-offload doctor`.\n", datahome.HomeValue(res.To))
		fmt.Fprintln(w, "The old tree was not touched; delete it yourself once the new home has run clean.")
	}
}
