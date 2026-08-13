// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored (wave D final glue): consume the /api/events SSE stream.
// pp:data-source live

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/cliutil"
	"llamaswap-pp-cli/internal/mirror"
)

// glueEventLine is one decoded SSE frame, rendered as one NDJSON line.
type glueEventLine struct {
	// ReceivedAt is when this process saw the frame, not when the server
	// emitted it — the stream carries no server timestamp.
	ReceivedAt string `json:"received_at"`
	// Type is the frame's type field ("logData", "modelStatus", ...).
	Type string `json:"type"`
	// Text is the decoded payload for frames whose data is a JSON string.
	Text string `json:"text,omitempty"`
	// Data is the raw payload for frames whose data is an object.
	Data json.RawMessage `json:"data,omitempty"`
}

// glueEventsSummary closes a --once drain with what was actually seen.
type glueEventsSummary struct {
	SchemaVersion int            `json:"schema_version"`
	Action        string         `json:"action"`
	BaseURL       string         `json:"base_url"`
	WindowMS      int64          `json:"window_ms"`
	Frames        int            `json:"frames"`
	ByType        map[string]int `json:"by_type,omitempty"`
	Filter        string         `json:"filter,omitempty"`
	Notes         []string       `json:"notes,omitempty"`
}

func newGlueEventsCmd(flags *rootFlags) *cobra.Command {
	var flagFollow bool
	var flagFilter string
	var flagWindow time.Duration

	cmd := &cobra.Command{
		Use:   "events",
		Short: "Consume the /api/events SSE stream as NDJSON — drain briefly by default, follow with -f.",
		Long: strings.Trim(`
llama-swap publishes a Server-Sent Events stream at /api/events carrying log
data and model lifecycle frames. Nothing on this box consumes it today; the
web UI is the only reader, and it forgets everything on reload.

Two modes, and the DEFAULT is the bounded one:

  events            drain for a short window, print what arrived, exit. Safe in
                    a script: it always terminates.
  events -f         follow until Ctrl-C. Foreground only — this CLI never
                    daemonizes a listener.

Output is NDJSON: one JSON object per line, so it pipes straight into jq, a
file, or a line-oriented consumer without a streaming parser. --filter narrows
by frame type.

Note on the stream's shape: llama-swap replays the whole buffered log as the
first logData frame on connect, so the first line of a fresh drain is large and
historical rather than live. That is the server's behavior, reported here rather
than hidden.

Exit codes: 2 usage, 4 server unreachable.`, "\n"),
		Example: strings.Trim(`
  # Drain the stream briefly and exit
  llamaswap-pp-cli events

  # Follow live until Ctrl-C
  llamaswap-pp-cli events -f

  # Only model lifecycle frames
  llamaswap-pp-cli events --filter type=modelStatus --window 5s
`, "\n"),
		Annotations: map[string]string{
			"pp:data-source":      "live",
			"pp:typed-exit-codes": "0,2,4",
			"mcp:read-only":       "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "events")
			}
			filterType, err := glueParseEventFilter(flagFilter)
			if err != nil {
				return err
			}
			if cliutil.IsVerifyEnv() {
				return printJSONFiltered(cmd.OutOrStdout(), glueEventsSummary{
					SchemaVersion: glueSchemaVersion,
					Action:        "would_drain",
					Filter:        flagFilter,
					Notes:         []string{"PRINTING_PRESS_VERIFY=1: the SSE stream was not opened"},
				}, flags)
			}
			if flagFollow {
				return glueRunEventsFollow(cmd, flags, filterType)
			}
			return glueRunEventsOnce(cmd, flags, filterType, flagWindow)
		},
	}
	cmd.Flags().BoolVarP(&flagFollow, "follow", "f", false, "Follow the stream until Ctrl-C instead of draining a bounded window.")
	cmd.Flags().StringVar(&flagFilter, "filter", "", "Narrow to one frame type, as type=<name> (e.g. type=logData).")
	cmd.Flags().DurationVar(&flagWindow, "window", 3*time.Second, "How long the default (non-follow) drain listens before exiting.")
	return cmd
}

// glueParseEventFilter accepts the documented `type=X` form only. A silently
// ignored filter would be worse than a usage error: the caller would believe
// the output was narrowed.
func glueParseEventFilter(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	key, value, ok := strings.Cut(v, "=")
	if !ok || !strings.EqualFold(strings.TrimSpace(key), "type") || strings.TrimSpace(value) == "" {
		return "", glueUsageErrf("--filter %q is not supported; the only form is type=<name> (e.g. --filter type=logData)", raw)
	}
	return strings.TrimSpace(value), nil
}

func glueRunEventsOnce(cmd *cobra.Command, flags *rootFlags, filterType string, window time.Duration) error {
	if window <= 0 {
		window = 3 * time.Second
	}
	base, _ := spineBaseURL(flags)
	c, err := glueClient(flags)
	if err != nil {
		return err
	}
	start := time.Now()
	frames, err := c.DrainEvents(cmd.Context(), window)
	if err != nil {
		return spineExitErr(ExitServerUnreachable, fmt.Errorf("read /api/events from %s: %w", base, err))
	}

	w := cmd.OutOrStdout()
	summary := glueEventsSummary{
		SchemaVersion: glueSchemaVersion,
		Action:        "drain",
		BaseURL:       base,
		WindowMS:      time.Since(start).Milliseconds(),
		ByType:        map[string]int{},
	}
	if filterType != "" {
		summary.Filter = "type=" + filterType
	}
	for _, ev := range frames {
		if filterType != "" && !strings.EqualFold(ev.Type, filterType) {
			continue
		}
		summary.Frames++
		summary.ByType[ev.Type]++
		glueWriteEventLine(w, ev)
	}
	if summary.Frames == 0 {
		summary.Notes = append(summary.Notes,
			"no frames in the window; llama-swap emits on activity, so an idle proxy is silent (use -f to wait, or --window to listen longer)")
	}
	// The summary goes to stderr so stdout stays pure NDJSON for a pipe.
	return printJSONFiltered(cmd.ErrOrStderr(), summary, flags)
}

func glueRunEventsFollow(cmd *cobra.Command, flags *rootFlags, filterType string) error {
	base, _ := spineBaseURL(flags)
	c, err := glueClient(flags)
	if err != nil {
		return err
	}
	// Foreground only, and Ctrl-C is the documented exit. No daemon, no
	// scheduled task, no background watcher — house rule.
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()

	w := cmd.OutOrStdout()
	seen := 0
	// DrainEvents is window-bounded by construction; following is repeated
	// bounded drains, which also reconnects for free when the server restarts.
	for {
		frames, derr := c.DrainEvents(ctx, 5*time.Second)
		if ctx.Err() != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "events: interrupted after %d frame(s); stopping cleanly\n", seen)
			return nil
		}
		if derr != nil {
			return spineExitErr(ExitServerUnreachable, fmt.Errorf("read /api/events from %s: %w", base, derr))
		}
		for _, ev := range frames {
			if filterType != "" && !strings.EqualFold(ev.Type, filterType) {
				continue
			}
			seen++
			glueWriteEventLine(w, ev)
		}
	}
}

// glueWriteEventLine emits one NDJSON line.
func glueWriteEventLine(w io.Writer, ev mirror.RawEvent) {
	line := glueEventLine{
		ReceivedAt: ev.ReceivedAt.UTC().Format(time.RFC3339Nano),
		Type:       ev.Type,
		Text:       ev.Text,
	}
	// Text and Data are mutually exclusive: when the payload decoded as a
	// string, echoing the raw form too would double every log frame.
	if ev.Text == "" {
		line.Data = ev.Data
	}
	spineWriteJSONLine(w, line)
}
