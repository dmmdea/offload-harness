// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored novel command.
// pp:data-source live

package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type buildSeat struct {
	Model     string `json:"model"`
	Binary    string `json:"binary"`
	BuildInfo string `json:"build_info,omitempty"`
	ModelPath string `json:"model_path,omitempty"`
	NCtx      int    `json:"n_ctx,omitempty"`
	Probed    bool   `json:"probed"`
	Skipped   string `json:"skipped_reason,omitempty"`
}

type buildReport struct {
	SchemaVersion int    `json:"schema_version"`
	CheckedAt     string `json:"checked_at"`

	ProxyVersion   string `json:"llamaswap_version"`
	ProxyCommit    string `json:"llamaswap_commit,omitempty"`
	ProxyBuildDate string `json:"llamaswap_build_date,omitempty"`

	LoadedSeats []buildSeat `json:"loaded_seats"`
	Builds      []string    `json:"distinct_build_info"`
	Binaries    []string    `json:"distinct_binaries"`
	Drift       bool        `json:"drift"`
	Verdict     string      `json:"verdict"`
	Notes       []string    `json:"notes,omitempty"`
}

func newMeasureBuildCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "build",
		Short: "build subcommands: check",
		Example: `  llamaswap-pp-cli build check
  llamaswap-pp-cli build check --json`,
		Annotations: map[string]string{"mcp:read-only": "true", "pp:measurement-owner": "wave-c"},
		RunE:        parentNoSubcommandRunE(flags),
	}
	addNovelCommandIfAbsent(cmd, newMeasureBuildCheckCmd(flags))
	return cmd
}

func newMeasureBuildCheckCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Proxy version plus the llama.cpp build actually serving each loaded seat",
		Long: `The post-upgrade sweep, done by hand after every llama.cpp rebuild until now:

  * /api/version for the proxy itself;
  * /upstream/{model}/props for every LOADED seat, which reports the llama.cpp
    build_info of the process serving it.

Only loaded seats are probed. /upstream is an auto-start endpoint, so sweeping
the whole roster would load every model on it - the sweep would cause the
outage it is meant to detect.

Exits 25 (drift) when loaded seats are not all on the same build: that is a
finding, not an error.`,
		Example: `  llamaswap-pp-cli build check
  llamaswap-pp-cli build check --json`,
		Annotations: map[string]string{
			"mcp:read-only":        "true",
			"pp:typed-exit-codes":  "4=proxy unreachable, 25=build drift between loaded seats",
			"pp:measurement-owner": "wave-c",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "build check")
			}
			if err := validateDataSourceStrategy(flags, "live"); err != nil {
				return usageErr(err)
			}
			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			timeout := mcTimeout(cmd, flags, 30*time.Second)

			report := &buildReport{SchemaVersion: 1, CheckedAt: mcNow()}
			var version struct {
				Version   string `json:"version"`
				Commit    string `json:"commit"`
				BuildDate string `json:"build_date"`
			}
			if err := mcGetJSON(ctx, flags, "/api/version", timeout, &version); err != nil {
				return mcClassify(err)
			}
			report.ProxyVersion, report.ProxyCommit, report.ProxyBuildDate = version.Version, version.Commit, version.BuildDate

			seats, err := mcRunning(ctx, flags, timeout)
			if err != nil {
				return mcClassify(err)
			}
			if len(seats) == 0 {
				report.Verdict = "no seats loaded: nothing to probe (build check never starts a model to inspect it)"
				return mcEmit(cmd, flags, report, func(w io.Writer) { buildPrint(w, report) })
			}

			builds := map[string]bool{}
			binaries := map[string]bool{}
			for _, seat := range seats {
				row := buildSeat{Model: seat.Model, Binary: buildBinaryOf(seat.Cmd)}
				binaries[row.Binary] = true
				if !mcIsLlamaServer(seat.Cmd) {
					row.Skipped = "non-llama-server seat (no llama.cpp build_info to report)"
					report.LoadedSeats = append(report.LoadedSeats, row)
					continue
				}
				var props struct {
					BuildInfo                 string `json:"build_info"`
					ModelPath                 string `json:"model_path"`
					DefaultGenerationSettings struct {
						NCtx int `json:"n_ctx"`
					} `json:"default_generation_settings"`
				}
				if err := mcGetJSON(ctx, flags, "/upstream/"+seat.Model+"/props", timeout, &props); err != nil {
					row.Skipped = "props unreadable: " + err.Error()
					report.LoadedSeats = append(report.LoadedSeats, row)
					continue
				}
				row.Probed = true
				row.BuildInfo, row.ModelPath, row.NCtx = props.BuildInfo, props.ModelPath, props.DefaultGenerationSettings.NCtx
				if row.BuildInfo != "" {
					builds[row.BuildInfo] = true
				}
				report.LoadedSeats = append(report.LoadedSeats, row)
			}
			report.Builds = sortedKeys(builds)
			report.Binaries = sortedKeys(binaries)
			report.Drift = len(report.Builds) > 1 || len(report.Binaries) > 1
			switch {
			case report.Drift:
				report.Verdict = fmt.Sprintf("DRIFT: %d distinct llama.cpp builds and %d distinct binaries across %d loaded seats",
					len(report.Builds), len(report.Binaries), len(report.LoadedSeats))
			case len(report.Builds) == 1:
				report.Verdict = "all loaded seats are on " + report.Builds[0]
			default:
				report.Verdict = "no build_info reported by any loaded seat"
			}
			report.Notes = append(report.Notes, "unloaded seats are not probed: /upstream is an auto-start endpoint and a sweep would load the entire roster")

			if err := mcEmit(cmd, flags, report, func(w io.Writer) { buildPrint(w, report) }); err != nil {
				return err
			}
			if report.Drift {
				return &cliError{code: ExitDrift, err: fmt.Errorf("%s", report.Verdict)}
			}
			return nil
		},
	}
	return cmd
}

func buildBinaryOf(cmd string) string {
	tokens := mcSplitCmd(cmd)
	if len(tokens) == 0 {
		return ""
	}
	return filepath.ToSlash(tokens[0])
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if k != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func buildPrint(w io.Writer, r *buildReport) {
	fmt.Fprintf(w, "%s\n", bold("build check"))
	fmt.Fprintf(w, "  llama-swap      %s (commit %s, built %s)\n", r.ProxyVersion, r.ProxyCommit, r.ProxyBuildDate)
	if len(r.LoadedSeats) == 0 {
		fmt.Fprintf(w, "  %s\n", r.Verdict)
		return
	}
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "SEAT\tBUILD_INFO\tN_CTX\tBINARY")
	for _, s := range r.LoadedSeats {
		info := s.BuildInfo
		if !s.Probed {
			info = "(" + s.Skipped + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", s.Model, info, s.NCtx, s.Binary)
	}
	tw.Flush()
	verdict := green(r.Verdict)
	if r.Drift {
		verdict = yellow(r.Verdict)
	}
	fmt.Fprintf(w, "  verdict         %s\n", verdict)
	for _, n := range r.Notes {
		fmt.Fprintf(w, "  note            %s\n", strings.TrimSpace(n))
	}
}
