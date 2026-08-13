// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored novel command.
// pp:data-source live

package cli

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		addNovelCommandIfAbsent(root, newMetricsCmd(flags))
	})
}

// llama-server's Prometheus field names, transcribed from
// tools/server/README.md "GET /metrics" on ggml-org/llama.cpp master
// (fetched 2026-08-13). The `llamacpp:` prefix is part of the wire name.
//
// kv_cache_usage_ratio is DELIBERATELY ABSENT: it was removed upstream and is
// no longer exported. Older guidance still names it, so a reader that expects
// it must be told the absence is correct rather than left assuming the seat is
// broken - which is what metricsLegacyNames below is for.
const (
	mxPromptTokensTotal     = "llamacpp:prompt_tokens_total"
	mxPromptSecondsTotal    = "llamacpp:prompt_seconds_total"
	mxPromptTokensSeconds   = "llamacpp:prompt_tokens_seconds"
	mxPredictedTokensTotal  = "llamacpp:tokens_predicted_total"
	mxPredictedSecondsTotal = "llamacpp:tokens_predicted_seconds_total"
	mxPredictedTokensSecond = "llamacpp:predicted_tokens_seconds"
	mxRequestsProcessing    = "llamacpp:requests_processing"
	mxRequestsDeferred      = "llamacpp:requests_deferred"
	mxNTokensMax            = "llamacpp:n_tokens_max"
	mxNDecodeTotal          = "llamacpp:n_decode_total"
	mxBusySlotsPerDecode    = "llamacpp:n_busy_slots_per_decode"
	mxSpecDraftTokens       = "llamacpp:spec_decode_num_draft_tokens_total"
	mxSpecAcceptedTokens    = "llamacpp:spec_decode_num_accepted_tokens_total"
	mxSpecDrafts            = "llamacpp:spec_decode_num_drafts_total"
)

// metricsCounters are cumulative-since-start. A single scrape of a counter is
// a lifetime total, not a rate; only the difference between two scrapes
// describes what the seat is doing NOW.
var metricsCounters = []string{
	mxPromptTokensTotal, mxPromptSecondsTotal, mxPredictedTokensTotal,
	mxPredictedSecondsTotal, mxNTokensMax, mxNDecodeTotal,
	mxSpecDraftTokens, mxSpecAcceptedTokens, mxSpecDrafts,
}

// metricsGauges are point-in-time.
var metricsGauges = []string{
	mxPromptTokensSeconds, mxPredictedTokensSecond,
	mxRequestsProcessing, mxRequestsDeferred, mxBusySlotsPerDecode,
}

// metricsLegacyNames are fields that older documentation names but current
// llama.cpp no longer exports. Their absence is expected and is reported as
// such, so nobody goes looking for a fault that is a removal.
var metricsLegacyNames = map[string]string{
	"llamacpp:kv_cache_usage_ratio": "removed upstream; the server no longer exports KV-cache utilisation. Use `ctx <model>` for the live n_ctx and `fit` for the cache footprint",
	"llamacpp:kv_cache_tokens":      "removed upstream alongside kv_cache_usage_ratio",
}

type metricSample struct {
	Name   string             `json:"name"`
	Labels map[string]string  `json:"labels,omitempty"`
	Value  float64            `json:"value"`
	Kind   string             `json:"kind"`
	Rate   *metricCounterRate `json:"rate,omitempty"`
}

// metricCounterRate is a counter's change across the two scrapes.
type metricCounterRate struct {
	Delta     float64 `json:"delta"`
	WindowSec float64 `json:"window_seconds"`
	PerSecond float64 `json:"per_second"`
	Note      string  `json:"note,omitempty"`
}

type metricsFinding struct {
	Severity    string `json:"severity"`
	Code        string `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

type metricsReport struct {
	SchemaVersion  int    `json:"schema_version"`
	Model          string `json:"model"`
	RequestedAs    string `json:"requested_as,omitempty"`
	MetricsEnabled bool   `json:"metrics_enabled"`
	Reason         string `json:"reason,omitempty"`
	HTTPStatus     int    `json:"http_status"`

	SampledAt  string  `json:"sampled_at"`
	WindowSec  float64 `json:"delta_window_seconds,omitempty"`
	SampleMode string  `json:"sample_mode"`

	Samples []metricSample `json:"metrics"`

	// Derived facts that need two fields joined.
	PromptRate    *float64 `json:"prompt_tokens_per_second_window,omitempty"`
	PredictedRate *float64 `json:"predicted_tokens_per_second_window,omitempty"`
	SpecAccept    *float64 `json:"speculative_acceptance_rate,omitempty"`

	Findings []metricsFinding `json:"findings,omitempty"`
	Notes    []string         `json:"notes,omitempty"`
	Unknown  []string         `json:"unrecognized_metric_names,omitempty"`
}

func newMetricsCmd(flags *rootFlags) *cobra.Command {
	var (
		flagDelta     time.Duration
		flagAllowLoad bool
		flagAll       bool
	)

	cmd := &cobra.Command{
		Use:   "metrics <model>",
		Short: "Parse a seat's llama-server Prometheus metrics into typed telemetry and findings",
		Long: `Scrapes GET /upstream/{model}/metrics and parses llama.cpp's Prometheus
exposition into typed fields, rather than handing back the raw text that
'upstream metrics' returns.

Counters are cumulative since the process started. A single scrape of
prompt_tokens_total is a lifetime figure that says nothing about what the seat
is doing now, so --delta scrapes TWICE and reports the change across the
window alongside the raw totals. The gauges (prompt_tokens_seconds,
predicted_tokens_seconds) are all-time averages for the same reason - the
windowed rate is the one that describes the present.

requests_deferred > 0 is surfaced as a finding: it means requests arrived with
every slot busy and were queued, which is a --parallel/--threads-http capacity
fact about the seat, not a load average.

kv_cache_usage_ratio NO LONGER EXISTS upstream. Its absence is reported as a
removal so nobody hunts for a fault that is a deleted field.

/metrics is served only when the seat's llama-server was started with
--metrics; without it the server answers 501 (some builds 404), which is
reported as metrics_enabled:false and EXITS 0 - an absent seat flag is a fact
about the configuration, not a failure to retry.

/upstream/{model}/* is an AUTO-START route: scraping an unloaded model would
make llama-swap load it. This command refuses unless the model is already
running; --allow-load opts into that cost explicitly.`,
		Example: `  llamaswap-pp-cli metrics embeddinggemma
  llamaswap-pp-cli metrics gemma-4-e2b --delta 5s --json
  llamaswap-pp-cli metrics gemma-4-e2b --all`,
		Annotations: map[string]string{
			"mcp:read-only":        "true",
			"pp:happy-args":        "embeddinggemma",
			"pp:typed-exit-codes":  "0=success (including metrics-not-enabled), 2=usage, 3=model not loaded or not in roster, 4=proxy unreachable, 27=upstream 5xx",
			"pp:measurement-owner": "wave-ls1",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				if wantsAgentErrorEnvelope(flags) {
					return usageEnvelopeErr(flags, fmt.Errorf("%q requires a model name", cmd.CommandPath()))
				}
				return cmd.Help()
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "metrics "+args[0])
			}
			if err := validateDataSourceStrategy(flags, "live"); err != nil {
				return usageErr(err)
			}
			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			timeout := mcTimeout(cmd, flags, 30*time.Second)

			requested := args[0]
			report := &metricsReport{SchemaVersion: 1, SampledAt: mcNow(), SampleMode: "single scrape"}

			roster, err := mcRoster(ctx, flags, timeout)
			if err != nil {
				return mcClassify(err)
			}
			model, known := mcResolveAlias(roster, requested)
			if !known {
				return mcModelNotFound(requested, roster)
			}
			report.Model = model
			if model != requested {
				report.RequestedAs = requested
			}

			seats, err := mcRunning(ctx, flags, timeout)
			if err != nil {
				return mcClassify(err)
			}
			if _, loaded := mcFindSeat(seats, model); !loaded && !flagAllowLoad {
				return &cliError{code: ExitModelNotFound, err: fmt.Errorf(
					"%q is not loaded, and /upstream/{model}/metrics is an AUTO-START route: scraping it would make llama-swap load the model "+
						"(multi-GB, evicting whatever is resident). Loaded right now: %s.\n"+
						"Re-run with --allow-load to accept that cost deliberately",
					model, mcJoinOrNone(mcLoadedNames(seats)))}
			}

			path := "/upstream/" + model + "/metrics"
			first, ferr := rawTextGet(ctx, flags, path)
			if ferr != nil {
				return spineExitErr(ExitServerUnreachable, fmt.Errorf("GET %s%s: %w", first.BaseURL, path, ferr))
			}
			report.HTTPStatus = first.Status
			switch {
			case first.Status == http.StatusOK:
			case first.Status == http.StatusNotImplemented || first.Status == http.StatusNotFound:
				report.MetricsEnabled = false
				report.Reason = rawTextUpstreamMetricsOffReason
				return mcEmit(cmd, flags, report, func(w io.Writer) {
					fmt.Fprintf(w, "no metrics for %s: %s\n", model, report.Reason)
				})
			case first.Status >= 500:
				return spineExitErr(ExitUpstream5xx, fmt.Errorf("GET %s%s returned HTTP %d", first.BaseURL, path, first.Status))
			default:
				return apiErr(fmt.Errorf("GET %s%s returned HTTP %d", first.BaseURL, path, first.Status))
			}
			report.MetricsEnabled = true

			samples, unknown := parsePrometheus(first.Body)
			if flagDelta > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(flagDelta):
				}
				second, serr := rawTextGet(ctx, flags, path)
				if serr != nil || second.Status != http.StatusOK {
					report.Notes = append(report.Notes, "the second scrape failed; counters below are lifetime totals with no windowed rate")
				} else {
					later, _ := parsePrometheus(second.Body)
					report.WindowSec = flagDelta.Seconds()
					report.SampleMode = fmt.Sprintf("two scrapes %s apart (counters reported as a windowed delta)", flagDelta)
					samples = applyCounterDeltas(samples, later, flagDelta.Seconds())
				}
			}
			report.Samples = metricsSelect(samples, flagAll)
			report.Unknown = unknown
			metricsDerive(report, samples)
			metricsFindingsFor(report, samples)

			return mcEmit(cmd, flags, report, func(w io.Writer) { metricsPrint(w, report) })
		},
	}
	cmd.Flags().DurationVar(&flagDelta, "delta", 0, "Scrape twice this far apart and report counters as a windowed rate (e.g. 5s)")
	cmd.Flags().BoolVar(&flagAllowLoad, "allow-load", false, "Allow scraping an unloaded model, accepting that it will be LOADED (multi-GB, evicts residents)")
	cmd.Flags().BoolVar(&flagAll, "all", false, "Report every exported series, not just the known llamacpp: fields")
	return cmd
}

// parsePrometheus reads the text exposition format: comment lines starting
// with '#', then `name{labels} value [timestamp]`. Unrecognized names are
// returned separately rather than dropped, so a field this CLI has not caught
// up with is visible instead of invisible.
func parsePrometheus(body string) ([]metricSample, []string) {
	var out []metricSample
	unknownSet := map[string]bool{}
	known := map[string]string{}
	for _, n := range metricsCounters {
		known[n] = "counter"
	}
	for _, n := range metricsGauges {
		known[n] = "gauge"
	}
	for _, raw := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, rest := splitMetricLine(line)
		if name == "" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		kind, ok := known[name]
		if !ok {
			kind = "unrecognized"
			unknownSet[name] = true
		}
		out = append(out, metricSample{Name: name, Labels: labels, Value: v, Kind: kind})
	}
	unknown := make([]string, 0, len(unknownSet))
	for n := range unknownSet {
		unknown = append(unknown, n)
	}
	sort.Strings(unknown)
	return out, unknown
}

// splitMetricLine separates `name{a="b",c="d"} value` into its parts.
func splitMetricLine(line string) (string, map[string]string, string) {
	open := strings.IndexByte(line, '{')
	if open < 0 {
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			return "", nil, ""
		}
		return parts[0], nil, parts[1]
	}
	close := strings.IndexByte(line[open:], '}')
	if close < 0 {
		return "", nil, ""
	}
	close += open
	name := line[:open]
	labels := map[string]string{}
	for _, pair := range strings.Split(line[open+1:close], ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			continue
		}
		labels[strings.TrimSpace(kv[0])] = strings.Trim(strings.TrimSpace(kv[1]), `"`)
	}
	return name, labels, strings.TrimSpace(line[close+1:])
}

// applyCounterDeltas joins two scrapes. A counter that went DOWN means the
// process restarted between scrapes, which is said out loud rather than
// reported as a negative rate.
func applyCounterDeltas(first, second []metricSample, window float64) []metricSample {
	index := map[string]float64{}
	for _, s := range second {
		index[metricKey(s)] = s.Value
	}
	for i := range first {
		if first[i].Kind != "counter" {
			continue
		}
		later, ok := index[metricKey(first[i])]
		if !ok {
			continue
		}
		delta := later - first[i].Value
		rate := &metricCounterRate{Delta: delta, WindowSec: window}
		switch {
		case delta < 0:
			rate.Note = "counter DECREASED between scrapes: the seat's llama-server restarted mid-window, so no rate can be derived from this pair"
		case window > 0:
			rate.PerSecond = delta / window
		}
		first[i].Rate = rate
	}
	return first
}

func metricKey(s metricSample) string {
	if len(s.Labels) == 0 {
		return s.Name
	}
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(s.Name)
	for _, k := range keys {
		b.WriteString("|" + k + "=" + s.Labels[k])
	}
	return b.String()
}

// metricsSelect keeps the known llamacpp: fields unless --all was passed.
func metricsSelect(samples []metricSample, all bool) []metricSample {
	if all {
		return samples
	}
	out := make([]metricSample, 0, len(samples))
	for _, s := range samples {
		if s.Kind != "unrecognized" {
			out = append(out, s)
		}
	}
	return out
}

func metricValue(samples []metricSample, name string) (metricSample, bool) {
	for _, s := range samples {
		if s.Name == name && len(s.Labels) == 0 {
			return s, true
		}
	}
	return metricSample{}, false
}

// metricsDerive computes the facts that need two fields joined. Windowed
// throughput uses the counter pair (tokens / seconds) rather than the
// all-time gauge, because the gauge answers a different question.
func metricsDerive(rep *metricsReport, samples []metricSample) {
	windowRate := func(tokens, seconds string) *float64 {
		tk, ok1 := metricValue(samples, tokens)
		sc, ok2 := metricValue(samples, seconds)
		if !ok1 || !ok2 || tk.Rate == nil || sc.Rate == nil || sc.Rate.Delta <= 0 {
			return nil
		}
		v := tk.Rate.Delta / sc.Rate.Delta
		return &v
	}
	rep.PromptRate = windowRate(mxPromptTokensTotal, mxPromptSecondsTotal)
	rep.PredictedRate = windowRate(mxPredictedTokensTotal, mxPredictedSecondsTotal)

	draft, ok1 := metricValue(samples, mxSpecDraftTokens)
	accepted, ok2 := metricValue(samples, mxSpecAcceptedTokens)
	if ok1 && ok2 && draft.Value > 0 {
		v := accepted.Value / draft.Value
		rep.SpecAccept = &v
	} else if ok1 && draft.Value == 0 {
		rep.Notes = append(rep.Notes, "speculative decoding is off on this seat (spec_decode_num_draft_tokens_total is 0), so there is no acceptance rate")
	}

	for legacy, why := range metricsLegacyNames {
		if _, present := metricValue(samples, legacy); present {
			rep.Notes = append(rep.Notes, fmt.Sprintf("%s IS exported by this build; upstream has since removed it, so this seat is running an older llama.cpp than the field list this CLI was written against", legacy))
		} else {
			rep.Notes = append(rep.Notes, fmt.Sprintf("%s absent: %s", legacy, why))
		}
	}
}

// metricsFindingsFor turns the numbers into the two statements an operator
// acts on.
func metricsFindingsFor(rep *metricsReport, samples []metricSample) {
	if deferred, ok := metricValue(samples, mxRequestsDeferred); ok && deferred.Value > 0 {
		slots := "the seat's slot count"
		if busy, ok := metricValue(samples, mxBusySlotsPerDecode); ok {
			slots = fmt.Sprintf("the seat's slot count (%.2f busy slots per decode)", busy.Value)
		}
		rep.Findings = append(rep.Findings, metricsFinding{
			Severity: "warning",
			Code:     "slots_too_low",
			Message: fmt.Sprintf(
				"requests_deferred = %.0f: requests arrived while every slot was busy and were QUEUED. Latency on this seat now includes queue time that no per-request timing reports",
				deferred.Value),
			Remediation: "raise the seat's --parallel (slot count) in the llama-swap YAML, or reduce concurrent callers. Note each slot divides n_ctx: doubling --parallel halves the per-slot context, so check `ctx " + rep.Model + "` before raising it. Reported by " + slots,
		})
	}
	if processing, ok := metricValue(samples, mxRequestsProcessing); ok && processing.Value > 0 {
		rep.Findings = append(rep.Findings, metricsFinding{
			Severity: "info",
			Code:     "seat_busy_during_scrape",
			Message: fmt.Sprintf(
				"requests_processing = %.0f during the scrape: this seat was serving live traffic, so any benchmark taken in this window is contended",
				processing.Value),
			Remediation: "wait for the seat to idle before benching, or treat the numbers as loaded-seat figures",
		})
	}
	if spec := rep.SpecAccept; spec != nil && *spec < 0.4 {
		rep.Findings = append(rep.Findings, metricsFinding{
			Severity:    "warning",
			Code:        "low_speculative_acceptance",
			Message:     fmt.Sprintf("speculative acceptance rate is %.1f%%: the draft model is being rejected more often than it helps", *spec*100),
			Remediation: "a draft model below roughly 40% acceptance usually costs more than it saves; try a closer-matched drafter or drop -md",
		})
	}
}

func metricsPrint(w io.Writer, r *metricsReport) {
	fmt.Fprintf(w, "%s  %s\n", bold("metrics"), r.Model)
	if !r.MetricsEnabled {
		fmt.Fprintf(w, "  %s %s (HTTP %d)\n", yellow("metrics off:"), r.Reason, r.HTTPStatus)
		return
	}
	fmt.Fprintf(w, "  sampled         %s  [%s]\n", r.SampledAt, r.SampleMode)
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "METRIC\tKIND\tVALUE\tWINDOW DELTA\tPER SECOND")
	for _, s := range r.Samples {
		name := strings.TrimPrefix(s.Name, "llamacpp:")
		if len(s.Labels) > 0 {
			name += fmt.Sprintf("%v", s.Labels)
		}
		delta, per := "-", "-"
		if s.Rate != nil {
			delta = fmt.Sprintf("%+.3g", s.Rate.Delta)
			if s.Rate.PerSecond != 0 {
				per = fmt.Sprintf("%.3g/s", s.Rate.PerSecond)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%.4g\t%s\t%s\n", name, s.Kind, s.Value, delta, per)
	}
	tw.Flush()
	if r.PromptRate != nil {
		fmt.Fprintf(w, "  prompt rate     %.1f tok/s over the window (counter pair, not the all-time gauge)\n", *r.PromptRate)
	}
	if r.PredictedRate != nil {
		fmt.Fprintf(w, "  predict rate    %.1f tok/s over the window (counter pair, not the all-time gauge)\n", *r.PredictedRate)
	}
	if r.SpecAccept != nil {
		fmt.Fprintf(w, "  spec accept     %.1f%%\n", *r.SpecAccept*100)
	}
	for _, f := range r.Findings {
		marker := yellow("finding:")
		if f.Severity == "info" {
			marker = "note:    "
		}
		fmt.Fprintf(w, "  %s [%s] %s\n", marker, f.Code, f.Message)
		if f.Remediation != "" {
			fmt.Fprintf(w, "           -> %s\n", f.Remediation)
		}
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "  note            %s\n", n)
	}
	if len(r.Unknown) > 0 {
		fmt.Fprintf(w, "  unrecognized    %s (pass --all to include them)\n", strings.Join(r.Unknown, ", "))
	}
}
