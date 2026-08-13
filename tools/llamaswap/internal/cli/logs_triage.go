// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored (wave D final glue): the error-class triage table over the
// buffered proxy log. Attaches to the GENERATED `logs` command as a subcommand;
// that file is not modified.
// pp:data-source live

package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/cliutil"
)

// glueLogClass is one taxonomy entry: a name, what it means, and how it is
// recognized in the log text.
//
// Every class here corresponds to a failure that actually happened on this
// deployment and cost real debugging time. The regexes are deliberately
// anchored to llama-swap's own log grammar rather than to loose keywords, so a
// model that happens to emit the word "aborted" in a completion does not
// register as a proxy failure.
type glueLogClass struct {
	Name    string
	Meaning string
	Re      *regexp.Regexp
}

// glueLogTaxonomy is evaluated in order; each line is assigned to the FIRST
// class it matches, so the classes are disjoint by construction.
var glueLogTaxonomy = []glueLogClass{
	{
		Name:    "429-embed-burst",
		Meaning: "embedding requests rejected for rate/queue pressure — a client burst outran the seat's --parallel",
		Re:      regexp.MustCompile(`POST /v1/embeddings HTTP/[\d.]+" 429|status=429, path=/v1/embeddings`),
	},
	{
		Name:    "500-embed-toolarge",
		Meaning: "embedding request failed upstream — classically an input longer than the seat's --ctx-size/--ubatch-size",
		Re:      regexp.MustCompile(`POST /v1/embeddings HTTP/[\d.]+" 500|status=500, path=/v1/embeddings`),
	},
	{
		Name:    "502-unload-midflight",
		Meaning: "the upstream vanished mid-request — the classic signature of a model unloaded or swapped while serving",
		Re:      regexp.MustCompile(`HTTP/[\d.]+" 502|status=502`),
	},
	{
		Name:    "400-ctx-overflow",
		Meaning: "request rejected before reaching the model — prompt over the context window is the common cause",
		Re:      regexp.MustCompile(`HTTP/[\d.]+" 400|status=400`),
	},
	{
		Name:    "proxy-dial-swap-window",
		Meaning: "the proxy could not reach the upstream port — a request that landed inside a swap window",
		Re:      regexp.MustCompile(`http: proxy error: dial tcp`),
	},
	{
		Name:    "gpuCh-monitor",
		Meaning: "the GPU monitor channel dropped — telemetry is degraded, inference is not",
		Re:      regexp.MustCompile(`failed reading from gpuCh`),
	},
	{
		Name:    "aborted-start",
		Meaning: "a model start was aborted, usually by a newer request preempting the swap",
		Re:      regexp.MustCompile(`starting .+ failed: aborted|failed: aborted`),
	},
	{
		Name:    "premature-exit",
		Meaning: "the upstream llama-server process exited during startup — a bad flag or a model that would not fit",
		Re:      regexp.MustCompile(`upstream command exited prematurely`),
	},
}

// glueAccessLine matches llama-swap's access-log line, so a class can report
// how many distinct REQUESTS it saw rather than only how many lines mention it
// (one failed request typically writes both an access line and a WARN line).
var glueAccessLine = regexp.MustCompile(`Request \S+ "(?:GET|POST|PUT|DELETE|PATCH|HEAD) [^"]+ HTTP/[\d.]+" (\d{3})`)

// glueLogTimestamp matches the leading timestamp formats a llama-swap build may
// emit. Current builds emit none; the parser is here so --since works the day
// one does, instead of silently mis-filtering.
var glueLogTimestamp = regexp.MustCompile(`^\[?(\d{4}[-/]\d{2}[-/]\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?)\]?`)

// glueTriageFinding is one class's tally.
type glueTriageFinding struct {
	// Class is the taxonomy name.
	Class string `json:"class"`
	// Meaning is the one-line explanation of what this class indicates.
	Meaning string `json:"meaning"`
	// Lines is how many buffered log lines matched.
	Lines int `json:"lines"`
	// Requests is how many of those were access-log lines, i.e. distinct
	// requests. Zero for classes that are not per-request events.
	Requests int `json:"requests"`
	// FirstLine and LastLine are 1-based positions in the buffer. They are the
	// only ordering available when the build stamps no timestamps.
	FirstLine int `json:"first_line,omitempty"`
	LastLine  int `json:"last_line,omitempty"`
	// FirstTS and LastTS are populated only when the log carries timestamps.
	FirstTS string `json:"first_ts,omitempty"`
	LastTS  string `json:"last_ts,omitempty"`
	// Sample is the first matching line, truncated, as evidence.
	Sample string `json:"sample,omitempty"`
}

// glueTriageReport is the command envelope.
type glueTriageReport struct {
	SchemaVersion int    `json:"schema_version"`
	Action        string `json:"action"`
	BaseURL       string `json:"base_url"`
	// BufferLines is the size of the buffer that was classified. llama-swap
	// serves a ring buffer, so this is "what is still there", not "everything
	// that ever happened" — a restart or a busy hour drops the rest.
	BufferLines int `json:"buffer_lines"`
	// Timestamped reports whether the buffer carried parseable timestamps.
	// When false, --since could not be applied and says so rather than
	// pretending to have filtered.
	Timestamped bool `json:"timestamped"`
	// SinceApplied reports whether the --since window was actually honored.
	SinceApplied bool                `json:"since_applied"`
	Since        string              `json:"since,omitempty"`
	Findings     []glueTriageFinding `json:"findings"`
	// Clean is true when no class matched at all.
	Clean bool     `json:"clean"`
	Notes []string `json:"notes,omitempty"`
}

func newGlueLogsTriageCmd(flags *rootFlags) *cobra.Command {
	var flagSince time.Duration
	var flagAll bool

	cmd := &cobra.Command{
		Use:   "triage",
		Short: "Classify the buffered proxy log into the error taxonomy, with counts and buffer positions.",
		Long: strings.Trim(`
'logs' hands you a wall of text. 'logs triage' answers the question you actually
had: what is going wrong, how often, and is it one class or several.

Eight classes, each one a failure mode that has really happened on this class of
deployment:

  429-embed-burst          a client burst outran the embedding seat's --parallel
  500-embed-toolarge       an embedding input exceeded the seat's context/batch
  502-unload-midflight     an upstream vanished mid-request (unload or swap)
  400-ctx-overflow         a request was rejected before reaching the model
  proxy-dial-swap-window   a request landed while the upstream port was down
  gpuCh-monitor            GPU telemetry dropped (inference unaffected)
  aborted-start            a model start was preempted by a newer request
  premature-exit           an upstream process died during startup

Counting is deliberate. A single failed request usually writes TWO lines (the
access line and a WARN partial-metrics line), so each class reports both 'lines'
(how much log matched) and 'requests' (how many distinct requests) instead of
quietly double-counting.

Two honesty properties worth knowing before you trust a zero:

  The buffer is a RING. What is not in it was dropped, by a restart or by
  volume. A count of zero means "not in the current buffer", never "never
  happened". 'sync' is what preserves history past the ring.

  Current llama-swap builds write NO timestamps into this buffer. When that is
  the case --since cannot be honored, and the report says so
  (timestamped:false, since_applied:false) rather than returning a filtered-
  looking result that was not filtered. Positions are reported as buffer line
  numbers instead, which is the only ordering that exists.

Exit codes: 2 usage, 4 server unreachable.`, "\n"),
		Example: strings.Trim(`
  # What is failing right now
  llamaswap-pp-cli logs triage

  # Machine-readable, for a nightly check
  llamaswap-pp-cli logs triage --json

  # Include classes with a zero count, so a dashboard has stable keys
  llamaswap-pp-cli logs triage --all --json
`, "\n"),
		Annotations: map[string]string{
			"pp:data-source":      "live",
			"pp:typed-exit-codes": "0,2,4",
			"mcp:read-only":       "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "logs triage")
			}
			if len(args) > 0 {
				return glueUsageErrf("%s takes no positional arguments (got %q)", cmd.CommandPath(), args[0])
			}
			if cliutil.IsVerifyEnv() {
				return printJSONFiltered(cmd.OutOrStdout(), glueTriageReport{
					SchemaVersion: glueSchemaVersion,
					Action:        "would_triage",
					Notes:         []string{"PRINTING_PRESS_VERIFY=1: the log buffer was not fetched"},
				}, flags)
			}
			return glueRunLogsTriage(cmd, flags, flagSince, flagAll)
		},
	}
	cmd.Flags().DurationVar(&flagSince, "since", 0, "Only classify entries newer than this (e.g. 2h). Honored only when the build stamps timestamps; the report says whether it was applied.")
	cmd.Flags().BoolVar(&flagAll, "all", false, "Include classes with a zero count, so machine consumers get stable keys.")
	return cmd
}

func glueRunLogsTriage(cmd *cobra.Command, flags *rootFlags, since time.Duration, includeZero bool) error {
	base, _ := spineBaseURL(flags)
	text, err := glueFetchLogs(cmd.Context(), flags, base)
	if err != nil {
		return spineExitErr(ExitServerUnreachable, fmt.Errorf("read /logs from %s: %w", base, err))
	}
	rep := glueClassifyLogs(text, since, includeZero)
	rep.BaseURL = base

	return mcEmit(cmd, flags, rep, func(w io.Writer) {
		fmt.Fprintf(w, "buffer: %d lines from %s\n", rep.BufferLines, rep.BaseURL)
		if rep.Clean {
			fmt.Fprintln(w, "no taxonomy class matched the current buffer.")
		} else {
			tw := newTabWriter(w)
			fmt.Fprintln(tw, "CLASS\tLINES\tREQUESTS\tFIRST\tLAST\tMEANING")
			for _, f := range rep.Findings {
				fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\n",
					f.Class, f.Lines, f.Requests,
					glueLinePos(f.FirstLine, f.FirstTS), glueLinePos(f.LastLine, f.LastTS), f.Meaning)
			}
			_ = tw.Flush()
		}
		for _, n := range rep.Notes {
			fmt.Fprintf(w, "  - %s\n", n)
		}
	})
}

// glueLinePos renders a position as a timestamp when one exists, otherwise as a
// buffer line number prefixed with L so it cannot be mistaken for a time.
func glueLinePos(line int, ts string) string {
	if ts != "" {
		return ts
	}
	if line == 0 {
		return "-"
	}
	return fmt.Sprintf("L%d", line)
}

// glueFetchLogs reads the buffered log as plain text. Deliberately not the
// generated JSON read path: /logs answers text/plain, and decoding it as JSON
// would fail or mangle it.
func glueFetchLogs(ctx context.Context, flags *rootFlags, base string) (string, error) {
	timeout := 30 * time.Second
	if flags != nil && flags.timeout > 0 {
		timeout = flags.timeout
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/logs", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/plain")
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return string(body), nil
}

// glueClassifyLogs is the pure function behind the command: log text in,
// taxonomy report out. Kept free of IO so it is directly testable against a
// fixture.
func glueClassifyLogs(text string, since time.Duration, includeZero bool) glueTriageReport {
	rep := glueTriageReport{SchemaVersion: glueSchemaVersion, Action: "triage"}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	type acc struct {
		lines, requests     int
		first, last         int
		firstTS, lastTS     string
		sample              string
		sawAnyTimestampHere bool
	}
	tally := map[string]*acc{}

	cutoff := time.Time{}
	if since > 0 {
		cutoff = time.Now().Add(-since)
	}
	sawTimestamp := false
	skippedForSince := 0

	counted := 0
	for i, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		if strings.TrimSpace(line) == "" {
			continue
		}
		counted++

		var lineTS string
		if m := glueLogTimestamp.FindStringSubmatch(line); m != nil {
			lineTS = m[1]
			sawTimestamp = true
			if since > 0 {
				if t, perr := glueParseLogTime(lineTS); perr == nil && t.Before(cutoff) {
					skippedForSince++
					continue
				}
			}
		}

		for _, class := range glueLogTaxonomy {
			if !class.Re.MatchString(line) {
				continue
			}
			a := tally[class.Name]
			if a == nil {
				a = &acc{first: i + 1, firstTS: lineTS, sample: truncate(strings.TrimSpace(line), 200)}
				tally[class.Name] = a
			}
			a.lines++
			a.last = i + 1
			if lineTS != "" {
				a.lastTS = lineTS
			}
			if glueAccessLine.MatchString(line) {
				a.requests++
			}
			break // first match wins; classes are disjoint
		}
	}

	rep.BufferLines = counted
	rep.Timestamped = sawTimestamp
	if since > 0 {
		rep.Since = since.String()
		rep.SinceApplied = sawTimestamp
		if !sawTimestamp {
			rep.Notes = append(rep.Notes,
				"--since was NOT applied: this llama-swap build writes no timestamps into the /logs buffer, so there is nothing to filter on. Every line in the buffer was classified.")
		} else {
			rep.Notes = append(rep.Notes, fmt.Sprintf("--since %s excluded %d older line(s)", since, skippedForSince))
		}
	}

	for _, class := range glueLogTaxonomy {
		a := tally[class.Name]
		if a == nil {
			if includeZero {
				rep.Findings = append(rep.Findings, glueTriageFinding{Class: class.Name, Meaning: class.Meaning})
			}
			continue
		}
		rep.Findings = append(rep.Findings, glueTriageFinding{
			Class:     class.Name,
			Meaning:   class.Meaning,
			Lines:     a.lines,
			Requests:  a.requests,
			FirstLine: a.first,
			LastLine:  a.last,
			FirstTS:   a.firstTS,
			LastTS:    a.lastTS,
			Sample:    a.sample,
		})
	}
	rep.Clean = len(tally) == 0
	rep.Notes = append(rep.Notes,
		"the /logs buffer is a RING: a zero count means 'not in the current buffer', never 'never happened'. Run 'sync' to keep history past it.")
	if !sawTimestamp {
		rep.Notes = append(rep.Notes,
			"positions are buffer line numbers (L<n>) because this build stamps no timestamps in /logs")
	}
	return rep
}

// glueParseLogTime accepts the timestamp layouts a llama-swap build might emit.
func glueParseLogTime(s string) (time.Time, error) {
	layouts := []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02T15:04:05", "2006-01-02 15:04:05",
		"2006/01/02 15:04:05",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized log timestamp %q", s)
}
