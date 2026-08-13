// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// pp:data-source live

package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/lsconfig"
)

// seatDrift is the per-seat verdict of `config drift`.
type seatDrift struct {
	Model string `json:"model"`
	// Status is one of: match | drift | not-in-config | file-only-not-running.
	Status   string               `json:"status"`
	State    string               `json:"state,omitempty"`
	SeatKind lsconfig.SeatKind    `json:"seat_kind,omitempty"`
	LiveCmd  string               `json:"live_cmd,omitempty"`
	FileCmd  string               `json:"file_cmd,omitempty"`
	Deltas   []lsconfig.FlagDelta `json:"flag_deltas,omitempty"`
	Note     string               `json:"note,omitempty"`
}

type driftReport struct {
	SchemaVersion string      `json:"schema_version"`
	ConfigPath    string      `json:"config_path"`
	ConfigSha     string      `json:"config_sha256"`
	Endpoint      string      `json:"endpoint"`
	RunningSeats  int         `json:"running_seats"`
	Compared      int         `json:"compared"`
	Drifted       int         `json:"drifted"`
	Seats         []seatDrift `json:"seats"`
	// NotEvaluated names configured seats that are not currently loaded. Drift
	// is UNKNOWABLE for them without starting them, and starting a model to
	// check its flags would evict whatever is resident. Reported, never
	// silently counted as clean.
	NotEvaluated []string `json:"not_evaluated"`
}

const driftSchemaVersion = "drift/1"

func newConfigDriftCmd(flags *rootFlags) *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "drift",
		Short: "Compare every RUNNING seat's live command line against the config file, flag by flag.",
		Long: "Detect config drift: a process running with flags the file no longer says.\n\n" +
			"GET /running returns each loaded seat's actual argv — the ground truth for\n" +
			"what llama-swap spawned. This expands the file's cmd for the same seat and\n" +
			"diffs them flag by flag. A seat drifts when the file was edited without a\n" +
			"restart, when a restart picked up a different file, or when a change was\n" +
			"staged into a backup and never applied.\n\n" +
			"Only LOADED seats can be compared. Drift is unknowable for an unloaded seat\n" +
			"without starting it, and starting one would evict whatever is resident — so\n" +
			"unloaded seats are listed under not_evaluated, never counted as clean.\n\n" +
			"Exit 0 when every compared seat matches, " + fmt.Sprint(ExitDrift) + " when any diverges. Drift is a\n" +
			"finding, not a failure: the command worked.",
		Example: "  llamaswap-pp-cli config drift\n" +
			"  llamaswap-pp-cli config drift --json\n" +
			"  llamaswap-pp-cli config drift --config-file ./candidate.yaml",
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": fmt.Sprintf("0,2,%d,%d,%d", ExitServerUnreachable, ExitConfigInvalid, ExitDrift),
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			path, err := resolveConfigPath([]string{configPath}, 0)
			if err != nil {
				return err
			}
			if dryRunOK(flags) {
				return writeDryRun(out, flags, "config drift against "+path)
			}
			f, err := loadConfigFile(path)
			if err != nil {
				return err
			}
			running, err := fetchRunning(cmd.Context(), flags)
			if err != nil {
				return err
			}
			c, cerr := newLoopbackClient(flags)
			endpoint := ""
			if cerr == nil {
				endpoint = c.BaseURL
			}
			rep := buildDriftReport(f, running, endpoint)

			if wantsJSON(out, flags) {
				if err := printJSONFiltered(out, rep, flags); err != nil {
					return err
				}
			} else {
				printDriftHuman(cmd, rep)
			}
			if rep.Drifted > 0 {
				return errDrift(fmt.Errorf("%d of %d compared seat(s) diverge from %s", rep.Drifted, rep.Compared, path))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config-file", "", "llama-swap YAML to compare against (default: the live config)")
	return cmd
}

func buildDriftReport(f *lsconfig.File, running []runningSeat, endpoint string) *driftReport {
	rep := &driftReport{
		SchemaVersion: driftSchemaVersion,
		ConfigPath:    f.Path, ConfigSha: f.Sha256,
		Endpoint: endpoint, RunningSeats: len(running),
	}
	loaded := map[string]bool{}
	for _, r := range running {
		loaded[r.Model] = true
		m, ok := f.ModelIndex[r.Model]
		if !ok {
			rep.Seats = append(rep.Seats, seatDrift{
				Model: r.Model, Status: "not-in-config", State: r.State, LiveCmd: r.Cmd,
				Note: "a seat is running that this config file does not define — the service is running a DIFFERENT config than the one being compared",
			})
			continue
		}
		rep.Compared++
		deltas := lsconfig.DiffCmds(m.CmdExpanded, r.Cmd)
		sd := seatDrift{
			Model: r.Model, State: r.State, SeatKind: m.Seat,
			LiveCmd: r.Cmd, FileCmd: m.CmdExpanded, Deltas: deltas,
		}
		if len(deltas) == 0 {
			sd.Status = "match"
		} else {
			sd.Status = "drift"
			rep.Drifted++
		}
		rep.Seats = append(rep.Seats, sd)
	}
	for _, m := range f.Models {
		if !loaded[m.ID] {
			rep.NotEvaluated = append(rep.NotEvaluated, m.ID)
		}
	}
	sort.Strings(rep.NotEvaluated)
	sort.SliceStable(rep.Seats, func(i, j int) bool { return rep.Seats[i].Model < rep.Seats[j].Model })
	return rep
}

func printDriftHuman(cmd *cobra.Command, rep *driftReport) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s\n", bold("config drift"))
	fmt.Fprintf(out, "  config   %s (sha %s)\n", rep.ConfigPath, rep.ConfigSha[:16])
	fmt.Fprintf(out, "  endpoint %s\n", rep.Endpoint)
	fmt.Fprintf(out, "  running  %d seat(s), %d compared\n\n", rep.RunningSeats, rep.Compared)

	if len(rep.Seats) == 0 {
		fmt.Fprintf(out, "no seats are loaded — nothing to compare\n")
	}
	for _, s := range rep.Seats {
		switch s.Status {
		case "match":
			fmt.Fprintf(out, "%s  %s  (%s, %s)\n", green("MATCH"), bold(s.Model), s.State, s.SeatKind)
		case "not-in-config":
			fmt.Fprintf(out, "%s  %s\n    %s\n", red("ORPHAN"), bold(s.Model), s.Note)
			fmt.Fprintf(out, "    live: %s\n", s.LiveCmd)
		default:
			fmt.Fprintf(out, "%s  %s  (%s, %s)\n", red("DRIFT"), bold(s.Model), s.State, s.SeatKind)
			for _, d := range s.Deltas {
				fmt.Fprintf(out, "    %s\n", d.String())
			}
			fmt.Fprintf(out, "    file: %s\n    live: %s\n", s.FileCmd, s.LiveCmd)
		}
	}

	if len(rep.NotEvaluated) > 0 {
		fmt.Fprintf(out, "\n%s\n", bold("NOT EVALUATED (not currently loaded)"))
		fmt.Fprintf(out, "  %s\n", strings.Join(rep.NotEvaluated, ", "))
		fmt.Fprintf(out, "  Drift is unknowable for an unloaded seat without starting it, and starting\n")
		fmt.Fprintf(out, "  one evicts whatever is resident. These are NOT counted as clean.\n")
	}

	fmt.Fprintln(out)
	if rep.Drifted == 0 {
		fmt.Fprintf(out, "%s\n", green(fmt.Sprintf("no drift among the %d compared seat(s)", rep.Compared)))
	} else {
		fmt.Fprintf(out, "%s\n", red(fmt.Sprintf("%d of %d compared seat(s) diverge", rep.Drifted, rep.Compared)))
	}
}
