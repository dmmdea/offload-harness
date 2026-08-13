// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored (wave A spine). Registered through the novel-command hook so a
// reprint of root.go keeps the wiring. See REGISTRATIONS-A.md.
// pp:data-source live

package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/cliutil"
	"llamaswap-pp-cli/internal/mirror"
	"llamaswap-pp-cli/internal/store"
)

func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		addNovelCommandIfAbsent(root, newSpinePsCmd(flags))
	})
}

// spinePsRow is one loaded model.
type spinePsRow struct {
	Name string `json:"name"`
	// State is the proxy's own word for the seat ("ready", "starting", ...).
	State string `json:"state"`
	// Ctx is the context size the seat was STARTED with, parsed from the live
	// cmd line, falling back to the roster's meta.n_ctx on llama-swap builds
	// that expose it. Nil when neither says (the server default applies and
	// this command will not guess it).
	Ctx *int `json:"ctx"`
	// CtxSource names where Ctx came from, because "-c on the command line"
	// and "meta.n_ctx from the roster" are different claims: the first is what
	// the process was told, the second is what the config declares.
	CtxSource string `json:"ctx_source,omitempty"`
	// TTL comes from the llama-swap YAML, never from /running: the API reports
	// ttl:0 for a ttl:-1 seat (verified live), so echoing it would launder a
	// known-wrong value into an operator-facing table.
	TTL       string `json:"ttl"`
	TTLSource string `json:"ttl_source"`
	Port      int    `json:"port,omitempty"`
	// Uptime is derived from mirrored swap events. "unknown" when the mirror has
	// not yet recorded this seat becoming ready — /running carries no start
	// time, so there is nothing else to derive it from.
	Uptime       string   `json:"uptime"`
	UptimeSource string   `json:"uptime_source"`
	NGL          *int     `json:"n_gpu_layers,omitempty"`
	Aliases      []string `json:"aliases,omitempty"`
	KeepSet      bool     `json:"keep_set"`
}

// spineInflightRow is one request currently without a terminal status.
type spineInflightRow struct {
	ActivityID int64  `json:"activity_id"`
	Model      string `json:"model"`
	ReqPath    string `json:"req_path"`
	Since      string `json:"since"`
}

type spinePsReport struct {
	SchemaVersion int                `json:"schema_version"`
	BaseURL       string             `json:"base_url"`
	Running       []spinePsRow       `json:"running"`
	Inflight      []spineInflightRow `json:"inflight,omitempty"`
	Notes         []string           `json:"notes,omitempty"`
}

func newSpinePsCmd(flags *rootFlags) *cobra.Command {
	var flagInflight bool
	var flagWatch bool
	var flagInterval time.Duration
	var flagDB string

	cmd := &cobra.Command{
		Use:   "ps",
		Short: "Models currently holding VRAM: NAME, STATE, CTX, TTL, PORT, UPTIME.",
		Long: strings.Trim(`
Joins /running with /v1/models roster metadata into the table an operator
actually wants. Two columns are deliberately NOT taken from the obvious
place:

  TTL is read from the llama-swap YAML, not from /running. The API reports
  ttl:0 for a seat configured ttl:-1 (verified live on v249), so the field
  is unusable for the one question it looks like it answers.

  UPTIME is derived from the local mirror's swap events. /running carries
  no start timestamp, so a seat this CLI has not observed loading shows
  "unknown" rather than a fabricated duration. Run 'sync' to populate it.

VRAM and GPU utilisation are NOT shown: reading them means shelling out to
nvidia-smi, which is neither cheap nor this command's job. Use the measure
commands for that.`, "\n"),
		Example: strings.Trim(`
  # What is loaded right now
  llamaswap-pp-cli ps

  # Include requests that have not reached a terminal status
  llamaswap-pp-cli ps --inflight --json

  # Refresh in the foreground every 5s (Ctrl-C to stop; no daemon)
  llamaswap-pp-cli ps --watch --interval 5s
`, "\n"),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "ps")
			}
			if cliutil.IsVerifyEnv() {
				return spineWriteWouldSync(cmd, flags, "PRINTING_PRESS_VERIFY=1: no network reads", "read /running and /v1/models")
			}
			if flagWatch {
				interval := flagInterval
				if interval <= 0 {
					interval = 5 * time.Second
				}
				ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
				defer stop()
				for {
					if err := spinePsOnce(ctx, cmd, flags, flagInflight, flagDB); err != nil {
						if ctx.Err() != nil {
							return nil
						}
						fmt.Fprintf(cmd.ErrOrStderr(), "ps: %v\n", err)
					}
					select {
					case <-ctx.Done():
						fmt.Fprintln(cmd.ErrOrStderr(), "ps --watch: interrupted; stopping cleanly")
						return nil
					case <-time.After(interval):
					}
				}
			}
			return spinePsOnce(cmd.Context(), cmd, flags, flagInflight, flagDB)
		},
	}
	cmd.Flags().BoolVar(&flagInflight, "inflight", false, "Also list activity rows that have not reached a terminal HTTP status.")
	cmd.Flags().BoolVar(&flagWatch, "watch", false, "Refresh in the foreground until Ctrl-C. No daemon, no background process.")
	cmd.Flags().DurationVar(&flagInterval, "interval", 5*time.Second, "Refresh interval for --watch.")
	cmd.Flags().StringVar(&flagDB, "db", "", "SQLite database file path used for UPTIME (default: resolved data directory data.db).")
	return cmd
}

func spinePsOnce(ctx context.Context, cmd *cobra.Command, flags *rootFlags, inflight bool, dbPath string) error {
	c, err := spineClient(flags)
	if err != nil {
		return err
	}
	running, err := c.Running(ctx)
	if err != nil {
		return spineExitErr(ExitServerUnreachable, fmt.Errorf("read /running: %w", err))
	}
	roster, err := spineLoadRoster(ctx, c)
	if err != nil {
		return err
	}
	keep := mirror.LoadKeepSet(mirror.KeepSetOptions{})
	seats := spineYAMLSeatIndex()

	rep := &spinePsReport{SchemaVersion: spineSchemaVersion, BaseURL: c.BaseURL}
	if len(keep.Warnings) > 0 {
		rep.Notes = append(rep.Notes, keep.Warnings...)
	}

	// UPTIME needs the mirror; a missing store is a note, not a failure.
	readyAt := map[string]time.Time{}
	if db, derr := spineOpenDB(ctx, dbPath); derr == nil {
		defer db.Close()
		readyAt = spineReadyTimes(ctx, db)
	} else {
		rep.Notes = append(rep.Notes, "local store unavailable ("+derr.Error()+"); UPTIME is unknown for every seat")
	}

	// Backend identity. A llama.cpp router-mode server answers a similarly
	// shaped roster, and every llama-swap-specific column below (TTL from the
	// YAML, keep-set, mirrored uptime) is meaningless against one. Reported as
	// a note rather than an error: the loaded-model table itself is still true.
	if probe := probeBackend(ctx, flags, 10*time.Second); probe.Kind != backendLlamaSwap && probe.Warning != "" {
		rep.Notes = append(rep.Notes, probe.Warning)
	}

	for _, r := range running {
		entry, _ := roster.Resolve(r.Model)
		psCtx, psCtxSource := spineParseCtxSize(r.Cmd), ""
		if psCtx != nil {
			psCtxSource = "seat -c/--ctx-size"
		} else if n, ok := rosterNCtx(ctx, flags, r.Model, 10*time.Second); ok {
			psCtx, psCtxSource = &n, "roster meta.n_ctx (configured, not per-slot live)"
		}
		row := spinePsRow{
			Name:         r.Model,
			State:        r.State,
			Ctx:          psCtx,
			CtxSource:    psCtxSource,
			NGL:          spineParseNGL(r.Cmd),
			Port:         spineParsePort(r.Proxy, r.Cmd),
			Aliases:      entry.Aliases,
			TTL:          "unknown",
			TTLSource:    "no llama-swap YAML readable",
			Uptime:       "unknown",
			UptimeSource: "no mirrored load event (run 'sync')",
		}
		if seat, ok := seats[r.Model]; ok && seat.TTLSet {
			row.TTL = seat.TTLText
			row.TTLSource = "llama-swap YAML"
			if seat.TTL == -1 {
				row.TTL = "-1 (resident)"
			}
		}
		if _, protected := keep.MatchAny(append(spineNamesOf(entry), r.Model)...); protected {
			row.KeepSet = true
		}
		if t, ok := readyAt[r.Model]; ok {
			row.Uptime = spineHumanDuration(time.Since(t))
			row.UptimeSource = "mirrored swap event (ready at " + t.UTC().Format(time.RFC3339) + ")"
		}
		rep.Running = append(rep.Running, row)
	}

	if inflight {
		for _, r := range running {
			page, aerr := c.Activity(ctx, mirror.ActivityOpts{Model: r.Model, Limit: 25, Sort: "id", Order: "desc"})
			if aerr != nil {
				rep.Notes = append(rep.Notes, "in-flight scan failed for "+r.Model+": "+aerr.Error())
				continue
			}
			for _, row := range page.Data {
				if row.Terminal() {
					continue
				}
				rep.Inflight = append(rep.Inflight, spineInflightRow{
					ActivityID: row.ID, Model: row.Model, ReqPath: row.ReqPath, Since: row.Timestamp,
				})
			}
		}
	}

	if flags != nil && (flags.asJSON || !isTerminal(cmd.OutOrStdout())) {
		return printJSONFiltered(cmd.OutOrStdout(), rep, flags)
	}
	w := cmd.OutOrStdout()
	if len(rep.Running) == 0 {
		fmt.Fprintln(w, "no models loaded")
	} else {
		tw := newTabWriter(w)
		fmt.Fprintln(tw, "NAME\tSTATE\tCTX\tTTL\tPORT\tUPTIME\tKEEP-SET")
		for _, r := range rep.Running {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%v\n",
				r.Name, r.State, intPtrText(r.Ctx), r.TTL, intText(r.Port), r.Uptime, r.KeepSet)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if len(rep.Inflight) > 0 {
		fmt.Fprintln(w)
		tw := newTabWriter(w)
		fmt.Fprintln(tw, "IN-FLIGHT\tMODEL\tPATH\tSINCE")
		for _, r := range rep.Inflight {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", r.ActivityID, r.Model, r.ReqPath, r.Since)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(cmd.ErrOrStderr(), "note: %s\n", n)
	}
	return nil
}

// spineReadyTimes returns the most recent mirrored 'ready' time per model,
// ignoring seats whose latest event says they were unloaded again.
func spineReadyTimes(ctx context.Context, db *store.Store) map[string]time.Time {
	out := map[string]time.Time{}
	rows, err := db.DB().QueryContext(ctx, `
		SELECT model, event, ts FROM swap_events ORDER BY id`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var model, event, ts string
		if err := rows.Scan(&model, &event, &ts); err != nil {
			return out
		}
		t, perr := time.Parse(time.RFC3339, ts)
		if perr != nil {
			continue
		}
		switch event {
		case "ready":
			out[model] = t
		case "unloaded", "failed":
			delete(out, model)
		}
	}
	return out
}

var (
	spineCtxRE  = regexp.MustCompile(`(?:--ctx-size|-c)\s+(\d+)`)
	spineNGLRE  = regexp.MustCompile(`(?:--n-gpu-layers|-ngl)\s+(-?\d+)`)
	spinePortRE = regexp.MustCompile(`--port\s+(\d+)`)
)

func spineParseCtxSize(cmdLine string) *int {
	m := spineCtxRE.FindStringSubmatch(cmdLine)
	if m == nil {
		return nil
	}
	if n, err := strconv.Atoi(m[1]); err == nil {
		return &n
	}
	return nil
}

func spineParseNGL(cmdLine string) *int {
	m := spineNGLRE.FindStringSubmatch(cmdLine)
	if m == nil {
		return nil
	}
	if n, err := strconv.Atoi(m[1]); err == nil {
		return &n
	}
	return nil
}

func spineParsePort(proxy, cmdLine string) int {
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			if p, cerr := strconv.Atoi(u.Port()); cerr == nil {
				return p
			}
		}
	}
	if m := spinePortRE.FindStringSubmatch(cmdLine); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n
		}
	}
	return 0
}

// spineYAMLSeatIndex reads the llama-swap YAML READ-ONLY. An unreadable config
// yields an empty index; callers render "unknown", never a default.
func spineYAMLSeatIndex() map[string]mirror.YAMLSeat {
	out := map[string]mirror.YAMLSeat{}
	path := os.Getenv(mirror.EnvYAMLPath)
	if path == "" {
		if _, err := os.Stat(mirror.DefaultYAMLPath); err == nil {
			path = mirror.DefaultYAMLPath
		}
	}
	if path == "" {
		return out
	}
	seats, err := mirror.ParseYAMLSeats(path)
	if err != nil {
		return out
	}
	for _, s := range seats {
		out[s.ID] = s
	}
	return out
}

func spineHumanDuration(d time.Duration) string {
	if d < 0 {
		return "unknown"
	}
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func intPtrText(v *int) string {
	if v == nil {
		return "-"
	}
	return strconv.Itoa(*v)
}

func intText(v int) string {
	if v == 0 {
		return "-"
	}
	return strconv.Itoa(v)
}
