// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Novel command (wave A spine).
// pp:data-source auto
// Supported strategies: auto, local, live, or computed.

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/cliutil"
	"llamaswap-pp-cli/internal/mirror"
)

func newNovelKeepsetCmd(flags *rootFlags) *cobra.Command {

	cmd := &cobra.Command{
		Use:   "keepset",
		Short: "The models that must stay resident: current status, and a sampled custody audit.",
		Long: strings.Trim(`
The keep-set is the set of seats whose absence is an outage rather than a
slow request. On this box that is the mem0 memory stack.

SOURCE (binding): the keep-set is parsed from the llama-swap YAML (seats
with ttl:-1 or ttl:0) unioned with a keep_set list in this CLI's own
config. It is NEVER derived from the server's ttl field — GET /running
reports ttl:0 for a seat configured ttl:-1, which is verified and wrong
for this purpose. Membership matches aliases as well as canonical ids.`, "\n"),
		Example: "  llamaswap-pp-cli keepset status\n" +
			"  llamaswap-pp-cli keepset status --json\n" +
			"  llamaswap-pp-cli keepset audit --since 7d",
		Annotations: map[string]string{"mcp:read-only": "true", "pp:parent-group": "true"},
		RunE:        parentNoSubcommandRunE(flags),
	}
	addNovelCommandIfAbsent(cmd, newNovelKeepsetStatusCmd(flags))
	addNovelCommandIfAbsent(cmd, newNovelKeepsetAuditCmd(flags))
	return cmd
}

// spineKeepsetStatusRow is one member's live state.
type spineKeepsetStatusRow struct {
	Model    string   `json:"model"`
	Aliases  []string `json:"aliases,omitempty"`
	Origin   string   `json:"origin"`
	InRoster bool     `json:"in_roster"`
	Resident bool     `json:"resident"`
	// Answering is nil when no probe was sent. A non-resident member is NOT
	// probed: any /upstream request auto-starts the model, so probing to find
	// out whether it is down would load it and destroy the finding.
	Answering    *bool  `json:"answering"`
	ProbeStatus  int    `json:"probe_status,omitempty"`
	ProbeSkipped string `json:"probe_skipped,omitempty"`
	Note         string `json:"note,omitempty"`
}

type spineKeepsetStatusReport struct {
	SchemaVersion int                     `json:"schema_version"`
	BaseURL       string                  `json:"base_url"`
	Sources       []string                `json:"keep_set_sources"`
	Members       []spineKeepsetStatusRow `json:"members"`
	AllResident   bool                    `json:"all_resident"`
	Warnings      []string                `json:"warnings,omitempty"`
}

func newNovelKeepsetStatusCmd(flags *rootFlags) *cobra.Command {
	var flagKeepset []string
	var flagNoProbe bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Is each keep-set model resident, and is it actually answering?",
		Long: strings.Trim(`
"Listed in the roster" and "resident" and "answering" are three different
things, and only the third one means the memory stack works. This command
reports all three:

  RESIDENT comes from /running.
  ANSWERING comes from one cheap read-only probe of the model's own
  /health through the upstream passthrough — but ONLY for models already
  resident. Probing a stopped model would auto-start it, converting the
  finding into a multi-GB load.

Exit code 25 when a configured keep-set member is not resident: that is a
finding, not a crash, and unattended callers should notice it.`, "\n"),
		Example: strings.Trim(`
  # Is the mem0 stack up?
  llamaswap-pp-cli keepset status

  # Machine-readable, for a post-restart check
  llamaswap-pp-cli keepset status --json

  # Residency only, no upstream probe at all
  llamaswap-pp-cli keepset status --no-probe
`, "\n"),
		Annotations: map[string]string{"mcp:read-only": "true", "pp:typed-exit-codes": "25"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "keepset status")
			}
			if cliutil.IsVerifyEnv() {
				return spineWriteWouldSync(cmd, flags, "PRINTING_PRESS_VERIFY=1: no network reads", "read /running and probe keep-set residency")
			}
			ctx := cmd.Context()
			keep := mirror.LoadKeepSet(mirror.KeepSetOptions{Extra: flagKeepset})
			c, err := spineClient(flags)
			if err != nil {
				return err
			}
			rep := &spineKeepsetStatusReport{
				SchemaVersion: spineSchemaVersion,
				BaseURL:       c.BaseURL,
				Sources:       keep.Sources,
				Warnings:      keep.Warnings,
			}
			if keep.Empty() {
				rep.Warnings = append(rep.Warnings,
					"keep-set is EMPTY: nothing is structurally protected from 'unload --all'. Point "+mirror.EnvYAMLPath+" at the llama-swap config or add keep_set to the CLI config.")
				return spineWriteKeepsetStatus(cmd, flags, rep)
			}

			running, err := c.Running(ctx)
			if err != nil {
				return spineExitErr(ExitServerUnreachable, fmt.Errorf("read /running: %w", err))
			}
			residentIDs := map[string]bool{}
			for _, r := range running {
				residentIDs[strings.ToLower(r.Model)] = true
			}
			roster, rerr := spineLoadRoster(ctx, c)
			if rerr != nil {
				return rerr
			}

			allResident := true
			for _, m := range keep.Members {
				row := spineKeepsetStatusRow{Model: m.ID, Aliases: m.Aliases, Origin: m.Origin}
				entry, inRoster := roster.Resolve(m.ID)
				row.InRoster = inRoster
				id := m.ID
				if inRoster {
					id = entry.ID
					if len(row.Aliases) == 0 {
						row.Aliases = entry.Aliases
					}
				} else {
					row.Note = "not present in /v1/models: the keep-set names a seat this server does not serve"
				}
				row.Resident = residentIDs[strings.ToLower(id)]
				if !row.Resident {
					allResident = false
					row.ProbeSkipped = "not resident; probing would auto-start the model"
					rep.Members = append(rep.Members, row)
					continue
				}
				if flagNoProbe {
					row.ProbeSkipped = "--no-probe"
					rep.Members = append(rep.Members, row)
					continue
				}
				status, perr := c.UpstreamHealth(ctx, id)
				row.ProbeStatus = status
				answering := perr == nil && status >= 200 && status < 300
				row.Answering = &answering
				if !answering {
					allResident = false
					if perr != nil {
						row.Note = "resident but the health probe failed: " + perr.Error()
					} else {
						row.Note = fmt.Sprintf("resident but /upstream/%s/health answered HTTP %d — listed is not answering", id, status)
					}
				}
				rep.Members = append(rep.Members, row)
			}
			rep.AllResident = allResident
			if err := spineWriteKeepsetStatus(cmd, flags, rep); err != nil {
				return err
			}
			if !allResident {
				return spineExitErr(ExitDrift, fmt.Errorf("one or more keep-set members are not resident-and-answering"))
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&flagKeepset, "keepset", nil, "Extra keep-set names for this invocation.")
	cmd.Flags().BoolVar(&flagNoProbe, "no-probe", false, "Report residency only; send no upstream health probe.")
	return cmd
}

func spineWriteKeepsetStatus(cmd *cobra.Command, flags *rootFlags, rep *spineKeepsetStatusReport) error {
	if flags != nil && (flags.asJSON || !isTerminal(cmd.OutOrStdout())) {
		if err := printJSONFiltered(cmd.OutOrStdout(), rep, flags); err != nil {
			return err
		}
	} else {
		w := cmd.OutOrStdout()
		if len(rep.Members) == 0 {
			fmt.Fprintln(w, "keep-set is empty")
		} else {
			tw := newTabWriter(w)
			fmt.Fprintln(tw, "MODEL\tALIASES\tRESIDENT\tANSWERING\tORIGIN\tNOTE")
			for _, m := range rep.Members {
				fmt.Fprintf(tw, "%s\t%s\t%v\t%s\t%s\t%s\n",
					m.Model, strings.Join(m.Aliases, ","), m.Resident,
					spineAnsweringText(m.Answering, m.ProbeSkipped), m.Origin, m.Note)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
		}
	}
	for _, warn := range rep.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warn)
	}
	return nil
}

func spineAnsweringText(b *bool, skipped string) string {
	if b == nil {
		if skipped != "" {
			return "not probed"
		}
		return "-"
	}
	if *b {
		return "yes"
	}
	return "NO"
}
