// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored (wave D final glue): bulk capture export to JSONL.
// Attaches to the GENERATED `captures` command as a subcommand; that file is
// not modified.
// pp:data-source live

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/mirror"
)

// glueCaptureRecord is one JSONL line: the activity metadata joined to the
// capture body. Both halves are kept because neither is sufficient alone —
// the activity row carries timings and status, the capture carries the bodies.
type glueCaptureRecord struct {
	// ID is the activity/capture id, the join key.
	ID int64 `json:"id"`
	// Timestamp is the activity row's timestamp, as the server reported it.
	Timestamp string `json:"timestamp"`
	// Model is the model that served the request.
	Model string `json:"model"`
	// ReqPath is the endpoint the request hit.
	ReqPath string `json:"req_path"`
	// Status is the response status code.
	Status int `json:"status"`
	// DurationMS is the server-measured wall time.
	DurationMS int64 `json:"duration_ms"`
	// Tokens is the activity row's token block, verbatim.
	Tokens mirror.ActivityTokens `json:"tokens"`
	// Capture is the /api/captures/{id} body, verbatim. Request and response
	// bodies inside it are base64 as the server encodes them; this command
	// does not re-encode or "helpfully" decode them.
	Capture json.RawMessage `json:"capture"`
}

// glueExportReport is the honest summary. Three separate numbers, because
// "exported 40" alone hides whether 10 were silently missing.
type glueExportReport struct {
	SchemaVersion int    `json:"schema_version"`
	Action        string `json:"action"`
	BaseURL       string `json:"base_url"`
	Out           string `json:"out"`
	// Scanned is how many activity rows were examined.
	Scanned int `json:"scanned"`
	// Eligible is how many of those declared has_capture.
	Eligible int `json:"eligible"`
	// Fetched is how many capture bodies were written.
	Fetched int `json:"fetched"`
	// Missing404 counts rows that claimed a capture the server no longer has —
	// the capture buffer is smaller than the activity ring, so this is normal
	// on an old row and is reported rather than treated as failure.
	Missing404 int `json:"missing_404"`
	// Errors counts capture fetches that failed for any other reason.
	Errors int `json:"errors"`
	// SkippedNoCapture counts rows with has_capture false.
	SkippedNoCapture int      `json:"skipped_no_capture"`
	Notes            []string `json:"notes,omitempty"`
}

func newGlueCapturesExportCmd(flags *rootFlags) *cobra.Command {
	var flagSince time.Duration
	var flagLast int
	var flagStatus int
	var flagModel string
	var flagOut string
	var flagMaxPages int

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Bulk-export request/response captures to JSONL — the eval/regression dataset the UI cannot produce.",
		Long: strings.Trim(`
The web UI shows captures one at a time. This exports them in bulk, joined to
their activity metadata, as JSONL — one self-contained JSON object per line,
which is the format every eval and replay pipeline already reads.

What it does: walks /api/metrics/activity, keeps the rows that declare
has_capture, fetches /api/captures/{id} for each, and writes
{activity metadata + capture} per line.

The summary reports four numbers separately, on purpose:

  fetched              captures actually written
  missing_404          rows that claimed a capture the server no longer holds.
                       The capture buffer is SMALLER than the activity ring, so
                       older rows routinely lose their bodies first. Normal, not
                       an error — but it must be visible, or an export that
                       silently lost half its rows looks complete.
  errors               fetches that failed for any other reason
  skipped_no_capture   rows that never had one

Captures must be enabled server-side for any of this to exist. When nothing is
eligible, the command says so and writes an empty file rather than inventing
rows or failing.

Bodies are passed through exactly as the server encodes them (base64 for
request/response payloads). Decoding is the consumer's decision, not this
command's.

Exit codes: 2 usage, 4 server unreachable.`, "\n"),
		Example: strings.Trim(`
  # The last 200 captured requests
  llamaswap-pp-cli captures export --last 200 --out captures.jsonl

  # Only failures, for a regression corpus
  llamaswap-pp-cli captures export --status 500 --out failures.jsonl

  # Everything the embedder served in the last 6 hours
  llamaswap-pp-cli captures export --since 6h --model embeddinggemma --out embed.jsonl
`, "\n"),
		Annotations: map[string]string{
			"pp:data-source":      "live",
			"pp:typed-exit-codes": "0,2,4",
			// Reads the API; the only write is the operator's own --out file.
			"mcp:read-only": "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return glueUsageErrf("%s takes no positional arguments (got %q)", cmd.CommandPath(), args[0])
			}
			out := strings.TrimSpace(flagOut)
			if out == "" {
				return glueUsageErrf("%s requires --out <file.jsonl>", cmd.CommandPath())
			}
			plan := []string{
				"read /api/metrics/activity, keep rows with has_capture",
				"fetch /api/captures/{id} for each",
				"write JSONL to " + out,
			}
			// Both gates: the verifier must never write into an operator's
			// filesystem, and --dry-run is the operator-facing equivalent.
			if verifyPlan(cmd.OutOrStdout(), flags, "export captures", plan) {
				return nil
			}
			return glueRunCapturesExport(cmd, flags, glueExportOpts{
				Since: flagSince, Last: flagLast, Status: flagStatus,
				Model: flagModel, Out: out, MaxPages: flagMaxPages,
			})
		},
	}
	cmd.Flags().DurationVar(&flagSince, "since", 0, "Only rows newer than this (e.g. 6h).")
	cmd.Flags().IntVar(&flagLast, "last", 0, "Stop after scanning this many activity rows, newest first.")
	cmd.Flags().IntVar(&flagStatus, "status", 0, "Only rows with this response status code.")
	cmd.Flags().StringVar(&flagModel, "model", "", "Only rows served by this model (id or alias).")
	cmd.Flags().StringVar(&flagOut, "out", "", "Destination JSONL file. Required.")
	cmd.Flags().IntVar(&flagMaxPages, "max-pages", 50, "Safety bound on activity pagination.")
	return cmd
}

type glueExportOpts struct {
	Since    time.Duration
	Last     int
	Status   int
	Model    string
	Out      string
	MaxPages int
}

func glueRunCapturesExport(cmd *cobra.Command, flags *rootFlags, opts glueExportOpts) error {
	ctx := cmd.Context()
	base, _ := spineBaseURL(flags)
	rep := &glueExportReport{SchemaVersion: glueSchemaVersion, Action: "captures export", BaseURL: base, Out: opts.Out}

	c, err := glueClient(flags)
	if err != nil {
		return err
	}
	model := ""
	if strings.TrimSpace(opts.Model) != "" {
		entry, rerr := glueResolve(ctx, c, opts.Model)
		if rerr != nil {
			return rerr
		}
		model = entry.ID
	}

	if dir := filepath.Dir(opts.Out); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create output directory %s: %w", dir, err)
		}
	}
	f, err := os.Create(opts.Out)
	if err != nil {
		return fmt.Errorf("create %s: %w", opts.Out, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)

	cutoff := time.Time{}
	if opts.Since > 0 {
		cutoff = time.Now().Add(-opts.Since)
	}
	maxPages := opts.MaxPages
	if maxPages <= 0 {
		maxPages = 50
	}

	stopped := false
	for page := 1; page <= maxPages && !stopped; page++ {
		p, perr := c.Activity(ctx, mirror.ActivityOpts{Model: model, Page: page, Limit: 100, Sort: "id", Order: "desc"})
		if perr != nil {
			return spineExitErr(ExitServerUnreachable, fmt.Errorf("read activity page %d: %w", page, perr))
		}
		if len(p.Data) == 0 {
			break
		}
		for _, row := range p.Data {
			if opts.Last > 0 && rep.Scanned >= opts.Last {
				stopped = true
				break
			}
			rep.Scanned++
			if !cutoff.IsZero() {
				if t, terr := time.Parse(time.RFC3339, row.Timestamp); terr == nil && t.Before(cutoff) {
					// Rows arrive newest-first, so the first row older than the
					// cutoff ends the walk.
					stopped = true
					break
				}
			}
			if opts.Status != 0 && row.RespStatusCode != opts.Status {
				continue
			}
			if !row.HasCapture {
				rep.SkippedNoCapture++
				continue
			}
			rep.Eligible++
			body, status, cerr := glueFetchCapture(ctx, flags, row.ID)
			switch {
			case status == 404:
				rep.Missing404++
				continue
			case cerr != nil:
				rep.Errors++
				continue
			}
			rec := glueCaptureRecord{
				ID: row.ID, Timestamp: row.Timestamp, Model: row.Model,
				ReqPath: row.ReqPath, Status: row.RespStatusCode,
				DurationMS: row.DurationMS, Tokens: row.Tokens, Capture: body,
			}
			if werr := enc.Encode(rec); werr != nil {
				return fmt.Errorf("write %s: %w", opts.Out, werr)
			}
			rep.Fetched++
		}
		if page >= p.TotalPages {
			break
		}
	}

	if rep.Eligible == 0 {
		rep.Notes = append(rep.Notes,
			"no activity row declared has_capture; either request capture is disabled server-side or the buffer has rolled. An empty file was written so a pipeline sees zero rows rather than a missing input.")
	}
	if rep.Missing404 > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"%d row(s) claimed a capture the server no longer holds — the capture buffer is smaller than the activity ring, so the oldest bodies expire first", rep.Missing404))
	}
	return mcEmit(cmd, flags, rep, func(w io.Writer) {
		fmt.Fprintf(w, "wrote %s\n", rep.Out)
		fmt.Fprintf(w, "  scanned:            %d\n", rep.Scanned)
		fmt.Fprintf(w, "  eligible:           %d\n", rep.Eligible)
		fmt.Fprintf(w, "  fetched:            %d\n", rep.Fetched)
		fmt.Fprintf(w, "  missing (404):      %d\n", rep.Missing404)
		fmt.Fprintf(w, "  errors:             %d\n", rep.Errors)
		fmt.Fprintf(w, "  skipped (no capture): %d\n", rep.SkippedNoCapture)
		for _, n := range rep.Notes {
			fmt.Fprintf(w, "  - %s\n", n)
		}
	})
}

// glueFetchCapture reads one capture body, returning the HTTP status alongside
// the error so a 404 (body expired) is distinguishable from a real failure.
func glueFetchCapture(ctx context.Context, flags *rootFlags, id int64) (json.RawMessage, int, error) {
	timeout := 30 * time.Second
	if flags != nil && flags.timeout > 0 {
		timeout = flags.timeout
	}
	data, status, err := mcDo(ctx, flags, "GET", "/api/captures/"+strconv.FormatInt(id, 10), nil, timeout)
	if err != nil {
		return nil, status, err
	}
	return json.RawMessage(data), status, nil
}
