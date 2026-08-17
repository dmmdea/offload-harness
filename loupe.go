package main

// `local-offload loupe` — the Ledger Loupe (memory-frontier Phase 0.1b).
//
// WHY THIS EXISTS
//   The plan this implements is built almost entirely of items GATED on a
//   measurement ("build X only if the duplicate rate clears N%", "ship the prefix
//   change only if reuse actually fires", "prune exemplars only if the histogram
//   is power-law"). Every one of those gates previously needed its own throwaway
//   script, and several could not be written at all because the ledger recorded no
//   call identity. This is the single place those questions get answered.
//
// DESIGN CONSTRAINTS, all deliberate:
//   - READ-ONLY. It never writes, never mutates, never reaches the network.
//   - Reads the JSONL ledger, NOT the bbolt cache. bbolt takes a file-level lock
//     for the lifetime of a read-write handle, so opening it from a second process
//     would contend with a live MCP server; the append-only JSONL is lock-free to
//     read by design (see the package comment on internal/ledger).
//   - Everything is computed in RAM over a slice of rows. Even at 100x the current
//     ledger size this is single-digit MB.
//   - NO cgo, no DuckDB, no parquet: the fleet binary must stay cross-compilable
//     for the laptop, the 6GB box and the 780M box. `--json` exists so anything
//     fancier can consume the output without linking anything in here.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/embedmemo"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// statRow is one aggregated bucket in a report.
type statRow struct {
	Key   string  `json:"key"`
	Count int     `json:"count"`
	Share float64 `json:"share"`
}

// statsReport is the full read-only view. Every section answers a specific gate
// in the memory-frontier plan; the field comments name which.
type statsReport struct {
	LedgerPath  string  `json:"ledger_path"`
	Rows        int     `json:"rows"`
	WindowDays  int     `json:"window_days,omitempty"`
	FirstTS     string  `json:"first_ts,omitempty"`
	LastTS      string  `json:"last_ts,omitempty"`
	SpanDays    float64 `json:"span_days,omitempty"`
	CallsPerDay float64 `json:"calls_per_day,omitempty"`

	// --- gate: is an exact-result cache worth building/keeping? ---
	CacheHits    int     `json:"cache_hits"`
	CacheHitRate float64 `json:"cache_hit_rate"`
	// DuplicateInputRate is the share of rows whose input fingerprint was seen
	// before — the CEILING on any exact-cache hit rate.
	//
	// ⚠ POINTERS ON PURPOSE (review finding; the most important lines in this file).
	// rate() collapses a zero denominator to the value 0. The TEXT report had an
	// explicit "n/a" branch but the JSON path did not — so running this against a
	// ledger where no row carries an identity published "duplicate_input_rate": 0.
	// This file exists to serve gates of the form "build X only if the duplicate
	// rate clears N%", and --json exists to be machine-read: a consumer would have
	// read a measured 0 and CLOSED A GATE THAT WAS NEVER MEASURED. The tool built to
	// end a silent failure was emitting one, in the exact number decisions turn on.
	// nil serialises as JSON null — unambiguous. 0 was a lie.
	//
	// Coverage is reported beside the rate and is NOT purely a "backlog washes out"
	// figure: identity is computed on the text cascade only, so on a vision/media
	// box coverage plateaus below 100% structurally and forever.
	RowsWithIdentity   int      `json:"rows_with_identity"`
	IdentityCoverage   *float64 `json:"identity_coverage"`
	DuplicateInputRate *float64 `json:"duplicate_input_rate"`
	// DuplicateRateBasis is the machine-readable discriminator a consumer should
	// branch on before reading the rate: "measured" | "insufficient_data".
	DuplicateRateBasis string    `json:"duplicate_rate_basis"`
	TopRepeatedInputs  []statRow `json:"top_repeated_inputs,omitempty"`

	// --- gate: does prefix reuse actually have anything to reuse? ---
	// A prefix seen many times is a prefix llama.cpp could have kept warm. If
	// nothing recurs, the prefix-stability work is measured-negative and closes
	// itself for free — which is a legitimate and cheap outcome.
	//
	// RowsWithPrefix is tracked separately from RowsWithIdentity because they are
	// different populations, and because "7 distinct prefixes" over 10 rows versus
	// over 10,000 rows are opposite conclusions that previously printed identically.
	RowsWithPrefix   int       `json:"rows_with_prefix"`
	DistinctPrefixes int       `json:"distinct_prefixes"`
	TopPrefixes      []statRow `json:"top_prefixes,omitempty"`

	// --- gate: which exemplars actually fire? ---
	// Nobody could answer this before. A power-law answer means the selection set
	// can be pruned by hand once.
	ExemplarInjections int       `json:"exemplar_injections"`
	TopExemplars       []statRow `json:"top_exemplars,omitempty"`

	// --- attribution: which decision-artifact set produced these rows? ---
	// Each context_hash bucket is a free A/B arm: compare defer/escalation rates
	// across buckets to attribute the effect of a hot-reloaded artifact.
	ContextBuckets []contextBucket `json:"context_buckets,omitempty"`

	// --- gate 0.4: is the embed memo earning its keep? ---
	// Always present so a consumer can tell "memo unavailable" from "memo idle".
	EmbedMemo embedMemoReport `json:"embed_memo"`

	// --- routing health ---
	ByTask       []statRow `json:"by_task,omitempty"`
	ByTier       []statRow `json:"by_tier,omitempty"`
	Deferred     int       `json:"deferred"`
	DeferRate    float64   `json:"defer_rate"`
	TopDefers    []statRow `json:"top_defer_reasons,omitempty"`
	Escalated    int       `json:"escalated"`
	EscalateRate float64   `json:"escalate_rate"`
}

// embedMemoReport answers Phase 0.4 ("embed distinct/total") from the memo's own
// bookkeeping rather than from a bolted-on counter that would later need removing.
//
// Unavailable is a FIRST-CLASS state, not an absence. The memo lives in a bbolt
// file the MCP server holds open while it runs, so a loupe run alongside the
// server legitimately cannot read it. Reporting zeros in that case would state a
// measured "the memo never hits" where nothing was measured at all — the exact
// silent-zero defect the duplicate-rate basis field exists to prevent.
type embedMemoReport struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"` // why not, when Available is false
	Path      string `json:"path,omitempty"`
	Distinct  int    `json:"distinct,omitempty"`
	Hits      int64  `json:"lifetime_hits,omitempty"`
	Misses    int64  `json:"lifetime_misses,omitempty"`
	// HitRate is nil when nothing has been looked up yet — see the Stats doc in
	// internal/embedmemo for why this is a pointer.
	HitRate *float64 `json:"hit_rate"`
}

// contextBucket is one artifact-set arm, with the outcome rates that make it
// comparable against the others.
type contextBucket struct {
	ContextHash  string  `json:"context_hash"`
	Rows         int     `json:"rows"`
	DeferRate    float64 `json:"defer_rate"`
	EscalateRate float64 `json:"escalate_rate"`
	FirstTS      string  `json:"first_ts,omitempty"`
	LastTS       string  `json:"last_ts,omitempty"`
}

func rate(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// topN turns a count map into the N largest buckets, shares computed against
// total. Ties break on key so output is deterministic (a report that reorders
// between identical runs is unreadable as a diff).
func topN(counts map[string]int, total, n int) []statRow {
	rows := make([]statRow, 0, len(counts))
	for k, c := range counts {
		rows = append(rows, statRow{Key: k, Count: c, Share: rate(c, total)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Key < rows[j].Key
	})
	if n > 0 && len(rows) > n {
		rows = rows[:n]
	}
	return rows
}

func runLoupe(args []string) error {
	fs := flag.NewFlagSet("loupe", flag.ExitOnError)
	fs.String("config", "", "config file path")
	since := fs.Int("since", 0, "only include entries from the last N days (0 = all)")
	top := fs.Int("top", 10, "how many rows per ranked section")
	asJSON := fs.Bool("json", false, "emit JSON instead of the text report")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)

	var sinceTS int64
	if *since > 0 {
		sinceTS = time.Now().AddDate(0, 0, -*since).Unix()
	}

	// Lock-free read: safe while the MCP server is appending.
	all, err := ledger.ReadAll(cfg.LedgerPath)
	if err != nil {
		return err
	}
	rows := make([]ledger.Entry, 0, len(all))
	for _, e := range all {
		if sinceTS == 0 || e.TS >= sinceTS {
			rows = append(rows, e)
		}
	}

	rep := statsReport{LedgerPath: cfg.LedgerPath, Rows: len(rows), WindowDays: *since}
	rep.EmbedMemo = readEmbedMemoReport(cfg)
	if len(rows) == 0 {
		return emitStats(rep, *asJSON)
	}

	inputs := map[string]int{}
	prefixes := map[string]int{}
	exemplars := map[string]int{}
	tasks := map[string]int{}
	tiers := map[string]int{}
	defers := map[string]int{}
	type ctxAgg struct {
		rows, def, esc int
		first, last    int64
	}
	ctxs := map[string]*ctxAgg{}

	minTS, maxTS := rows[0].TS, rows[0].TS
	for _, e := range rows {
		if e.TS < minTS {
			minTS = e.TS
		}
		if e.TS > maxTS {
			maxTS = e.TS
		}
		if e.CacheHit {
			rep.CacheHits++
		}
		if e.Deferred {
			rep.Deferred++
			if e.Reason != "" {
				defers[e.Reason]++
			}
		}
		if e.Escalations > 0 {
			rep.Escalated++
		}
		if e.Task != "" {
			tasks[e.Task]++
		}
		if e.ModelTier != "" {
			tiers[e.ModelTier]++
		}
		if e.InputSHA256 != "" {
			rep.RowsWithIdentity++
			inputs[e.InputSHA256]++
		}
		if e.PromptPrefixSHA256 != "" {
			rep.RowsWithPrefix++
			prefixes[e.PromptPrefixSHA256]++
		}
		for _, id := range e.ExemplarIDs {
			exemplars[id]++
			rep.ExemplarInjections++
		}
		if e.ContextHash != "" {
			a := ctxs[e.ContextHash]
			if a == nil {
				a = &ctxAgg{first: e.TS, last: e.TS}
				ctxs[e.ContextHash] = a
			}
			a.rows++
			if e.Deferred {
				a.def++
			}
			if e.Escalations > 0 {
				a.esc++
			}
			if e.TS < a.first {
				a.first = e.TS
			}
			if e.TS > a.last {
				a.last = e.TS
			}
		}
	}

	rep.FirstTS = time.Unix(minTS, 0).Format(time.RFC3339)
	rep.LastTS = time.Unix(maxTS, 0).Format(time.RFC3339)
	if span := float64(maxTS-minTS) / 86400.0; span > 0 {
		rep.SpanDays = float64(int(span*10)) / 10
		rep.CallsPerDay = float64(int(float64(len(rows))/span*10)) / 10
	}
	rep.CacheHitRate = rate(rep.CacheHits, len(rows))
	rep.DeferRate = rate(rep.Deferred, len(rows))
	rep.EscalateRate = rate(rep.Escalated, len(rows))

	// Duplicate rate is computed ONLY over rows that carry an identity, so the
	// pre-Phase-0.1 backlog cannot silently dilute it toward zero and make a real
	// signal look absent. IdentityCoverage is reported beside it so a reader can
	// see how much of the ledger the number actually speaks for.
	dupRows := 0
	for _, c := range inputs {
		if c > 1 {
			dupRows += c - 1 // every occurrence after the first is a repeat
		}
	}
	// Only publish a rate when there is something to divide by. "insufficient_data"
	// is a first-class answer here, not a zero — see the field comments.
	if rep.RowsWithIdentity > 0 {
		cov := rate(rep.RowsWithIdentity, len(rows))
		dup := rate(dupRows, rep.RowsWithIdentity)
		rep.IdentityCoverage = &cov
		rep.DuplicateInputRate = &dup
		rep.DuplicateRateBasis = "measured"
	} else {
		rep.DuplicateRateBasis = "insufficient_data"
	}
	rep.DistinctPrefixes = len(prefixes)

	rep.TopRepeatedInputs = topN(inputs, rep.RowsWithIdentity, *top)
	// Only repeats are interesting here; a list of once-seen inputs is noise.
	rep.TopRepeatedInputs = filterRepeats(rep.TopRepeatedInputs)
	// Prefix shares divide by the PREFIX population, not the identity population —
	// they are different sets (a task with no system block yields no prefix), and
	// mixing them silently under-reports every share.
	rep.TopPrefixes = topN(prefixes, rep.RowsWithPrefix, *top)
	rep.TopExemplars = topN(exemplars, rep.ExemplarInjections, *top)
	rep.ByTask = topN(tasks, len(rows), *top)
	rep.ByTier = topN(tiers, len(rows), *top)
	rep.TopDefers = topN(defers, rep.Deferred, *top)

	for h, a := range ctxs {
		rep.ContextBuckets = append(rep.ContextBuckets, contextBucket{
			ContextHash: h, Rows: a.rows,
			DeferRate: rate(a.def, a.rows), EscalateRate: rate(a.esc, a.rows),
			FirstTS: time.Unix(a.first, 0).Format(time.RFC3339),
			LastTS:  time.Unix(a.last, 0).Format(time.RFC3339),
		})
	}
	sort.Slice(rep.ContextBuckets, func(i, j int) bool {
		if rep.ContextBuckets[i].Rows != rep.ContextBuckets[j].Rows {
			return rep.ContextBuckets[i].Rows > rep.ContextBuckets[j].Rows
		}
		return rep.ContextBuckets[i].ContextHash < rep.ContextBuckets[j].ContextHash
	})

	return emitStats(rep, *asJSON)
}

// filterRepeats drops single-occurrence rows: "seen once" is not a repeat, and
// listing them buries the signal.
// readEmbedMemoReport opens the embed memo read-only for its counters.
//
// Every failure is reported WITH ITS CAUSE rather than folded into zeros: a
// disabled memo, a memo held by the running MCP server, and a memo that exists
// but has never been consulted are three different answers to "is it earning its
// keep?", and only the third is a measurement.
func readEmbedMemoReport(cfg config.Config) embedMemoReport {
	if !cfg.EmbedMemoOn() {
		return embedMemoReport{Available: false, Reason: "disabled (embed_memo_enabled=false or embed_memo_path empty)"}
	}
	// READ-ONLY, short timeout. This command's contract is that it never contends
	// with a live MCP server (see the file header); a read-write open here would
	// take an exclusive bolt lock and stall for its full timeout on every run
	// while the server is up.
	m, err := embedmemo.OpenReadOnly(cfg.EmbedMemoPath, cfg.EmbedModel(), cfg.EmbedMemoEpoch)
	if err != nil {
		reason := "cannot read: " + err.Error()
		switch {
		case errors.Is(err, embedmemo.ErrNoStore):
			reason = "no store yet — nothing has been embedded on this machine"
		case errors.Is(err, bolt.ErrTimeout):
			reason = "held by a running local-offload process — ask it directly via offload_status, or stop it and re-run"
		}
		return embedMemoReport{Available: false, Path: cfg.EmbedMemoPath, Reason: reason}
	}
	defer m.Close()
	st := m.Stats()
	return embedMemoReport{
		Available: true,
		Path:      cfg.EmbedMemoPath,
		Distinct:  st.Distinct,
		Hits:      st.LifetimeHits,
		Misses:    st.LifetimeMisses,
		HitRate:   st.HitRate,
	}
}

func filterRepeats(rows []statRow) []statRow {
	out := rows[:0]
	for _, r := range rows {
		if r.Count > 1 {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func emitStats(rep statsReport, asJSON bool) error {
	if asJSON {
		// Do NOT swallow the encode error: printing a bare newline and exiting 0
		// gives a consumer piping to jq a parse error with no cause and a success
		// status — the same silent-failure shape this command exists to remove.
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return fmt.Errorf("loupe: encode report (rows=%d): %w", rep.Rows, err)
		}
		fmt.Println(string(b))
		return nil
	}

	p := func(f string, a ...any) { fmt.Printf(f+"\n", a...) }
	p("ledger: %s", rep.LedgerPath)
	if rep.Rows == 0 {
		p("no rows in window — nothing to report")
		return nil
	}
	win := "all time"
	if rep.WindowDays > 0 {
		win = fmt.Sprintf("last %dd", rep.WindowDays)
	}
	p("rows:   %d (%s, %s .. %s, %.1f days, %.1f calls/day)",
		rep.Rows, win, rep.FirstTS[:10], rep.LastTS[:10], rep.SpanDays, rep.CallsPerDay)
	p("")
	p("REUSE")
	p("  cache hits          %d (%.1f%%)", rep.CacheHits, rep.CacheHitRate*100)
	if rep.DuplicateInputRate == nil {
		p("  duplicate inputs    n/a — no row carries input_sha256 yet")
		p("                      (identity accrues on the TEXT cascade going forward;")
		p("                       vision/media rows never carry it, so coverage plateaus below 100%%)")
	} else {
		p("  duplicate inputs    %.1f%% of %d identified rows (identity coverage %.1f%%)",
			*rep.DuplicateInputRate*100, rep.RowsWithIdentity, *rep.IdentityCoverage*100)
		p("                      ^ CEILING on an exact-cache hit rate over the SAME rows;")
		p("                        the cache-hit %% above is over ALL rows — different denominators")
	}
	if rep.RowsWithPrefix == 0 {
		p("  distinct prefixes   n/a — no row carries prompt_prefix_sha256 yet")
	} else {
		p("  distinct prefixes   %d distinct over %d rows carrying a prefix", rep.DistinctPrefixes, rep.RowsWithPrefix)
	}
	printRows(p, "  most-reused prefixes", rep.TopPrefixes)
	printRows(p, "  most-repeated inputs", rep.TopRepeatedInputs)
	em := rep.EmbedMemo
	switch {
	case !em.Available:
		p("  embed memo          n/a — %s", em.Reason)
	case em.HitRate == nil:
		p("  embed memo          %d vectors stored, never consulted yet", em.Distinct)
	default:
		p("  embed memo          %.1f%% hit (%d hit / %d miss) over %d stored vectors",
			*em.HitRate*100, em.Hits, em.Misses, em.Distinct)
		p("                      ^ each hit also skips a possible ~1-2s cold-embedder load (ttl=300)")
	}
	p("")
	p("ROUTING")
	p("  deferred            %d (%.1f%%)", rep.Deferred, rep.DeferRate*100)
	p("  escalated           %d (%.1f%%)", rep.Escalated, rep.EscalateRate*100)
	printRows(p, "  by task", rep.ByTask)
	printRows(p, "  by tier", rep.ByTier)
	printRows(p, "  defer reasons", rep.TopDefers)
	if rep.ExemplarInjections > 0 {
		p("")
		p("EXEMPLARS  (%d injections)", rep.ExemplarInjections)
		printRows(p, "  most-injected", rep.TopExemplars)
	}
	if len(rep.ContextBuckets) > 0 {
		p("")
		p("ARTIFACT SETS  (each row is a free A/B arm — compare rates across them)")
		for _, b := range rep.ContextBuckets {
			p("  %s  rows=%-5d defer=%.1f%%  escalate=%.1f%%  %s..%s",
				b.ContextHash, b.Rows, b.DeferRate*100, b.EscalateRate*100, b.FirstTS[:10], b.LastTS[:10])
		}
	}
	return nil
}

func printRows(p func(string, ...any), label string, rows []statRow) {
	if len(rows) == 0 {
		return
	}
	p("%s:", label)
	for _, r := range rows {
		key := r.Key
		if len(key) > 44 {
			key = key[:41] + "..."
		}
		p("    %-46s %5d  %5.1f%%", key, r.Count, r.Share*100)
	}
}
