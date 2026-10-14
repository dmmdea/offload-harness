// trace_corpus.go — the `trace-corpus` verb: a read-only readout over RECORDED agent
// runs, shaped like compaction-eval (harvest, freeze, check) and kept apart from it.
//
//	trace-corpus harvest  [--traces DIR] [--delegation-log DIR] --out corpus.jsonl [filters]
//	trace-corpus readout  --corpus C [--out FILE]
//	trace-corpus freeze   --corpus C --out baseline.json
//	trace-corpus check    --corpus C --baseline B
//
// Two kinds of record hold agent runs, and harvest reads both without touching the ledger:
//
//   - agent traces: the per-goal JSON files the standalone queue drainer writes (the files
//     compaction-eval harvest reads). They hold the final transcript and, from the release that
//     added this verb, the run's pager and prefill reports.
//   - the delegation log: one JSONL row per agent subtask, written by the delegator. It holds the
//     wire result (step trace, prefill accounting and, from the same release, the pager report),
//     not the transcript.
//
// The readout answers four questions: how many runs are trace-bearing, how often evicted content
// was fetched back (the pager re-fetch rate against the gate agent.PagerGate), how often the
// compaction ladder ran, and how many tokens were prefilled. A run recorded before a field existed
// has no value for it, and that is reported as UNRECORDED: every rate is a pointer that
// serializes as null when nothing was recorded, so a missing measurement can never read as 0.
//
// Like compaction-eval, harvest redacts at harvest (the same Redactor and vet table), stamps the
// corpus with its sha256, and the corpus is machine-local eval data, never committed. readout,
// freeze and check are deterministic and model-free; the report has no timestamps, and a frozen
// baseline must be reproduced byte for byte by check.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/compeval"
)

const (
	tcSchema           = "trace-corpus/1"
	tcSourceAgentTrace = "agent-trace"
	tcSourceDelegation = "delegation-log"
	// tcSeatUnrecorded is the by_seat key of the runs whose record names no seat (agent traces
	// do not), and the pool_alias_by_node key of the alias runs whose row names no node.
	tcSeatUnrecorded = "(unrecorded)"

	// tcSkipNeverRan is the harvest's reason for a delegation row whose agent never executed.
	tcSkipNeverRan = "delegation row never ran (zero steps, nothing recorded)"

	// tcSkipNotATrace is the harvest's reason for a traces-directory file that is valid JSON but
	// is not shaped like an agent trace: no transcript, or neither steps nor a stop reason.
	tcSkipNotATrace = "trace record has no transcript, or neither steps nor a stop reason"

	tcBasisMeasured     = "measured"
	tcBasisInsufficient = "insufficient_data"

	tcVerdictBelow  = "BELOW_GATE"
	tcVerdictAbove  = "ABOVE_GATE"
	tcVerdictNoData = "NO_DATA"
)

// tcDayRe is the shard name of a delegation-log file: the writer's local date.
var tcDayRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// ---- the corpus: one tcRun per recorded agent run ----

// tcPager is the pager report a run recorded. Present only when the run recorded one; a run from
// before the field has none, and nil is how the corpus says so.
type tcPager struct {
	Basis            string `json:"basis"`
	Evictions        int    `json:"evictions"`
	EvictedBytes     int64  `json:"evicted_bytes"`
	DistinctEvicted  int    `json:"distinct_evicted"`
	Refetched        int    `json:"refetched_distinct"`
	CompactionPasses int    `json:"compaction_passes"`
}

// tcPrefill is the server's prefill accounting for a run. Present only when the backend reported
// timings on at least one step; nil is "unmeasured", never zero prefill.
type tcPrefill struct {
	ObservedSteps int   `json:"observed_steps"`
	PrefillTokens int64 `json:"prefill_tokens"`
	CacheTokens   int64 `json:"cache_tokens"`
}

// tcRun is one recorded agent run, reduced to numbers and enums. No goal, output, transcript or
// tool text is kept: the corpus carries facts about runs, so the redaction has little to find
// and the readout needs nothing else.
type tcRun struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	// Seat is the seat the record names. A pool placement names the pool's ALIAS here, which is
	// not a seat (see tcIsPoolAlias); Node is the one further identifier the row carries.
	Seat string `json:"seat"`
	// Node is the node id the delegation row names; "" when the record has none (agent traces).
	Node string `json:"node,omitempty"`
	Day  string `json:"day,omitempty"`
	// TraceBearing: an agent trace with a transcript, or a delegation row whose step trace is
	// not empty.
	TraceBearing bool       `json:"trace_bearing"`
	Steps        int        `json:"steps"`
	StopReason   string     `json:"stop_reason,omitempty"`
	Pager        *tcPager   `json:"pager"`
	Prefill      *tcPrefill `json:"prefill"`
	// SeatTokensIn is the prompt-token count the seat processed over the whole run, cache hits
	// included (the wire's seat_tokens_in). It is the one input-volume figure an engine that
	// reports no prefill timings still gives. nil = not reported.
	SeatTokensIn *int64 `json:"seat_tokens_in"`
	// LadderArtifacts counts tool turns in the final transcript that are products of the
	// compaction ladder (elision markers, skeletons). It exists only for agent traces, which keep
	// a transcript; nil on a delegation row. It is a LOWER BOUND on whether the ladder fired.
	LadderArtifacts *int `json:"ladder_artifacts"`
}

// ---- reading the records ----

// tcPagerIn is a pager object as either record writes it: the loop report in an agent trace
// and the wire object in a delegation row share their key names. CompactionPasses is a pointer so
// a report that never recorded the pass count (null, or a key from before it existed) is told
// from a recorded zero.
type tcPagerIn struct {
	Basis            string `json:"basis"`
	Evictions        int    `json:"evictions"`
	EvictedBytes     int64  `json:"evicted_bytes"`
	DistinctEvicted  int    `json:"distinct_evicted"`
	Refetched        int    `json:"refetched_distinct"`
	CompactionPasses *int   `json:"compaction_passes"`
}

// run converts a recorded pager object to the corpus form. ok is false for an object that cannot
// be used (no pass count, an unknown basis, or counts that contradict each other): it is then
// treated as UNRECORDED rather than trusted, and the caller counts the skip.
func (in *tcPagerIn) run() (p *tcPager, ok bool) {
	if in == nil {
		return nil, true // no object: a plain absence, not a skip
	}
	switch {
	case in.CompactionPasses == nil, *in.CompactionPasses < 0:
		return nil, false
	case in.Evictions < 0, in.EvictedBytes < 0, in.DistinctEvicted < 0, in.Refetched < 0:
		return nil, false
	case in.Refetched > in.DistinctEvicted:
		return nil, false
	case in.Basis == tcBasisMeasured && in.DistinctEvicted == 0:
		return nil, false
	case in.Basis == tcBasisInsufficient && (in.DistinctEvicted != 0 || in.Evictions != 0):
		return nil, false
	case in.Basis != tcBasisMeasured && in.Basis != tcBasisInsufficient:
		return nil, false
	}
	return &tcPager{Basis: in.Basis, Evictions: in.Evictions, EvictedBytes: in.EvictedBytes,
		DistinctEvicted: in.DistinctEvicted, Refetched: in.Refetched, CompactionPasses: *in.CompactionPasses}, true
}

// tcTraceIn is the part of a standalone trace file the harvest reads.
type tcTraceIn struct {
	StopReason string     `json:"stop_reason"`
	Steps      int        `json:"steps"`
	Pager      *tcPagerIn `json:"pager"`
	Prefill    *struct {
		Basis         string `json:"basis"`
		ObservedSteps int    `json:"observed_steps"`
		PrefillTokens int64  `json:"prefill_tokens"`
		CacheTokens   int64  `json:"cache_tokens"`
	} `json:"prefill"`
	Transcript []agent.Msg `json:"transcript"`
}

// tcDelegationIn is the part of a delegation-log row the harvest reads.
type tcDelegationIn struct {
	Seat    string `json:"seat"`
	Node    string `json:"node"`
	Refusal *struct {
		Status int `json:"status"`
	} `json:"refusal"`
	Result *tcDelegationResult `json:"result"`
}

// tcDelegationResult is the wire result inside a delegation-log row, as far as the harvest reads it.
type tcDelegationResult struct {
	Steps         int        `json:"steps"`
	StopReason    string     `json:"stop_reason"`
	Trace         []struct{} `json:"trace"`
	PrefillSteps  int        `json:"prefill_steps"`
	PrefillTokens int64      `json:"prefill_tokens"`
	CacheTokens   int64      `json:"cache_tokens"`
	SeatTokensIn  int64      `json:"seat_tokens_in"`
	Pager         *tcPagerIn `json:"pager"`
}

// neverRan reports a logged subtask whose agent never executed: a deferral (capacity,
// infrastructure, budget, config, contract) is written with zero steps and nothing else. Anything
// recorded at all means an agent ran: a step trace, prefill or seat-token accounting, or a pager
// object, which a run the loop returned from carries (zeros included) once the recording ships.
// A row with none of those and no step is read as never having got there; for a row written
// before the recording that is an inference, because a deferral raised before the loop ran and a
// run the loop returned from in zero steps with nothing else wrote the same row.
//
// A row with zero steps but a step trace is KEPT, and that is deliberate. Its trace is built from
// the loop's own effect ledger, which exists only inside the loop's run: the contract's setup
// actions execute there, before the first model turn and without spending a step. Such a row is a
// deferred run (budget or infrastructure) that stopped before any model turn finished, and it is a
// run for the three questions this readout answers: a budget or infrastructure deferral takes the
// loop's pager report, so after the recording ships the row carries a pager object and its
// unrecorded count reaches zero like any other run's; and the compaction ladder runs before the
// first model call and again when that call is rejected for context overflow, so a first prompt
// too large for the seat can end a run at zero steps with passes recorded.
//
// A write-class deferral (the write door did not close cleanly) is kept the same way, by its
// trace, and carries a pager object like any other run the loop returned from: the object is
// attached ahead of the write door.
func (r *tcDelegationResult) neverRan() bool {
	return r.Steps == 0 && len(r.Trace) == 0 && r.PrefillSteps == 0 && r.SeatTokensIn == 0 && r.Pager == nil
}

// tcHarvestOpts selects what the harvest reads.
type tcHarvestOpts struct {
	TracesDir     string
	DelegationDir string
	// Seats, when non-empty, keeps only the runs whose record names one of these seats. An agent
	// trace names none, so it is skipped under a seat filter.
	Seats        map[string]bool
	ExcludeSeats map[string]bool
	// Since / Until bound the delegation shards by their day (inclusive, YYYY-MM-DD, "" = open).
	Since, Until string
}

// tcHarvestStats is the harvest's accounting: every input is a run or a counted skip.
type tcHarvestStats struct {
	TraceFiles       int            `json:"trace_files"`
	DelegationShards int            `json:"delegation_shards"`
	DelegationRows   int            `json:"delegation_rows"`
	Runs             int            `json:"runs"`
	BySource         map[string]int `json:"by_source"`
	// Skipped counts inputs that did not become a run, by reason. Notes counts runs that were
	// kept with one field dropped (an unusable pager object is read as unrecorded).
	Skipped    map[string]int `json:"skipped,omitempty"`
	Notes      map[string]int `json:"notes,omitempty"`
	Redactions map[string]int `json:"redactions,omitempty"`
}

func (o tcHarvestOpts) seatAllowed(seat string) (bool, string) {
	if len(o.Seats) > 0 {
		if seat == "" {
			return false, "seat filter: record names no seat"
		}
		if !o.Seats[seat] {
			return false, "seat filter: seat not selected"
		}
	}
	if o.ExcludeSeats[seat] {
		return false, "seat excluded"
	}
	return true, ""
}

func tcHarvest(o tcHarvestOpts) ([]tcRun, tcHarvestStats, error) {
	stats := tcHarvestStats{BySource: map[string]int{}, Skipped: map[string]int{}, Notes: map[string]int{}}
	var runs []tcRun
	if o.TracesDir != "" {
		r, err := tcHarvestTraces(o, &stats)
		if err != nil {
			return nil, stats, err
		}
		runs = append(runs, r...)
	}
	if o.DelegationDir != "" {
		r, err := tcHarvestDelegation(o, &stats)
		if err != nil {
			return nil, stats, err
		}
		runs = append(runs, r...)
	}
	// Redact every string that will be written, then gate on the vet: a residual hit means the
	// redactor and the vet have drifted, and nothing is written.
	red := compeval.NewRedactor()
	paths := tcPathRedactor{}
	for i := range runs {
		r := &runs[i]
		// A path-shaped value is replaced whole before the vet classes are applied: the vet table
		// has no path class, and a seat or stop reason that names a file would otherwise become a
		// by_seat key in every readout, and a node id that names one a pool_alias_by_node key.
		r.ID, r.Seat, r.StopReason = red.Redact(r.ID), red.Redact(paths.redact(r.Seat)), red.Redact(paths.redact(r.StopReason))
		r.Node = red.Redact(paths.redact(r.Node))
	}
	for _, r := range runs {
		for _, s := range []string{r.ID, r.Source, r.Seat, r.Node, r.Day, r.StopReason} {
			if classes := compeval.PIIClasses(s); len(classes) > 0 {
				return nil, stats, fmt.Errorf("harvest refused: a run still matches %s after redaction (redactor/vet drift, fix before harvesting; the value is not echoed)", strings.Join(classes, ", "))
			}
		}
	}
	hits := red.Hits()
	if paths.hits > 0 {
		hits["path"] = paths.hits
	}
	if len(hits) > 0 {
		stats.Redactions = hits
	}
	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].Source != runs[j].Source {
			return runs[i].Source < runs[j].Source
		}
		return runs[i].ID < runs[j].ID
	})
	for _, r := range runs {
		stats.BySource[r.Source]++
	}
	stats.Runs = len(runs)
	if len(stats.Skipped) == 0 {
		stats.Skipped = nil
	}
	if len(stats.Notes) == 0 {
		stats.Notes = nil
	}
	return runs, stats, nil
}

// tcPathRe matches a value that is a local filesystem path: a drive path, a rooted path, or
// anything holding a backslash. A bare name or an org/model id with no leading slash is not one.
// It vets the three strings a readout can key or print as the record wrote them: the seat, the
// node id and the stop reason.
var tcPathRe = regexp.MustCompile(`^[A-Za-z]:[\\/]|^/|\\`)

// tcPathRedactor replaces path-shaped values with stable placeholders, numbered by first
// appearance, so the same path is the same placeholder and a re-harvest is byte-identical.
type tcPathRedactor struct {
	seen map[string]string
	hits int
}

func (p *tcPathRedactor) redact(s string) string {
	if !tcPathRe.MatchString(s) {
		return s
	}
	if p.seen == nil {
		p.seen = map[string]string{}
	}
	ph, ok := p.seen[s]
	if !ok {
		ph = fmt.Sprintf("[redacted-path-%d]", len(p.seen)+1)
		p.seen[s] = ph
	}
	p.hits++
	return ph
}

func tcHarvestTraces(o tcHarvestOpts, stats *tcHarvestStats) ([]tcRun, error) {
	paths, err := filepath.Glob(filepath.Join(o.TracesDir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var runs []tcRun
	for _, p := range paths {
		stats.TraceFiles++
		if ok, why := o.seatAllowed(""); !ok {
			stats.Skipped[why]++
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			stats.Skipped["trace file unreadable"]++
			continue
		}
		var in tcTraceIn
		if err := json.Unmarshal(raw, &in); err != nil {
			stats.Skipped["trace file not a trace record"]++
			continue
		}
		run := tcRun{
			ID:           "tr-" + strings.TrimSuffix(filepath.Base(p), ".json"),
			Source:       tcSourceAgentTrace,
			Seat:         "",
			TraceBearing: len(in.Transcript) > 0,
			Steps:        in.Steps,
			StopReason:   in.StopReason,
		}
		if in.Steps < 0 {
			stats.Skipped["trace record has negative steps"]++
			continue
		}
		// Any JSON object decodes into tcTraceIn, so the shape is checked: an agent trace holds
		// the final transcript and either spent a step or says why it stopped. A file that has
		// neither (a checkpoint, a queue file, a goal list) is not a run, and counting it would
		// add a zero-step run with nothing recorded to every denominator.
		if len(in.Transcript) == 0 || (in.Steps == 0 && in.StopReason == "") {
			stats.Skipped[tcSkipNotATrace]++
			continue
		}
		pg, ok := in.Pager.run()
		if !ok {
			stats.Notes["pager object unusable (read as unrecorded)"]++
		}
		run.Pager = pg
		if pf := in.Prefill; pf != nil && pf.Basis == tcBasisMeasured && pf.ObservedSteps > 0 && pf.PrefillTokens >= 0 && pf.CacheTokens >= 0 {
			run.Prefill = &tcPrefill{ObservedSteps: pf.ObservedSteps, PrefillTokens: pf.PrefillTokens, CacheTokens: pf.CacheTokens}
		}
		if run.TraceBearing {
			n := 0
			for _, m := range in.Transcript {
				if m.Role == "tool" && agent.IsCompactionArtifact(m.Content) {
					n++
				}
			}
			run.LadderArtifacts = &n
		}
		runs = append(runs, run)
	}
	return runs, nil
}

func tcHarvestDelegation(o tcHarvestOpts, stats *tcHarvestStats) ([]tcRun, error) {
	paths, err := filepath.Glob(filepath.Join(o.DelegationDir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var runs []tcRun
	for _, p := range paths {
		day := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		if !tcDayRe.MatchString(day) {
			stats.Skipped["delegation shard name is not a date"]++
			continue
		}
		if (o.Since != "" && day < o.Since) || (o.Until != "" && day > o.Until) {
			stats.Skipped["delegation shard outside the day window"]++
			continue
		}
		stats.DelegationShards++
		f, err := os.Open(p)
		if err != nil {
			return nil, fmt.Errorf("delegation shard %s: %w", filepath.Base(p), err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
		line := 0
		for sc.Scan() {
			line++
			b := bytes.TrimSpace(sc.Bytes())
			if len(b) == 0 {
				continue
			}
			stats.DelegationRows++
			var in tcDelegationIn
			if err := json.Unmarshal(b, &in); err != nil {
				stats.Skipped["delegation row not JSON"]++
				continue
			}
			if in.Refusal != nil {
				stats.Skipped["delegation row is a capacity refusal, not a run"]++
				continue
			}
			if in.Result == nil {
				stats.Skipped["delegation row has no result (dispatch error)"]++
				continue
			}
			if ok, why := o.seatAllowed(in.Seat); !ok {
				stats.Skipped[why]++
				continue
			}
			res := in.Result
			if res.Steps < 0 {
				stats.Skipped["delegation row has negative steps"]++
				continue
			}
			if res.neverRan() {
				stats.Skipped[tcSkipNeverRan]++
				continue
			}
			run := tcRun{
				ID:           fmt.Sprintf("dl-%s-%06d", day, line),
				Source:       tcSourceDelegation,
				Seat:         in.Seat,
				Node:         in.Node,
				Day:          day,
				TraceBearing: len(res.Trace) > 0,
				Steps:        res.Steps,
				StopReason:   res.StopReason,
			}
			pg, ok := res.Pager.run()
			if !ok {
				stats.Notes["pager object unusable (read as unrecorded)"]++
			}
			run.Pager = pg
			if res.PrefillSteps > 0 && res.PrefillTokens >= 0 && res.CacheTokens >= 0 {
				run.Prefill = &tcPrefill{ObservedSteps: res.PrefillSteps, PrefillTokens: res.PrefillTokens, CacheTokens: res.CacheTokens}
			}
			if res.SeatTokensIn > 0 {
				n := res.SeatTokensIn
				run.SeatTokensIn = &n
			}
			runs = append(runs, run)
		}
		scanErr := sc.Err()
		f.Close()
		if scanErr != nil {
			return nil, fmt.Errorf("delegation shard %s: %w", filepath.Base(p), scanErr)
		}
	}
	return runs, nil
}

// ---- writing and loading the corpus ----

// tcEncodeCorpus is the corpus bytes for runs: one JSON object per line, in the given order.
func tcEncodeCorpus(runs []tcRun) ([]byte, error) {
	var buf bytes.Buffer
	for _, r := range runs {
		line, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// tcWriteCorpus writes the corpus atomically and returns its hash, obtained by RE-LOADING the
// bytes through the strict loader, so a written corpus is round-trip-proven loadable and the hash
// is the one every artifact is stamped with. A corpus the loader refuses never exists at path.
func tcWriteCorpus(path string, runs []tcRun) (string, error) {
	if len(runs) == 0 {
		return "", fmt.Errorf("write corpus: no runs")
	}
	b, err := tcEncodeCorpus(runs)
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return "", err
	}
	loaded, hash, err := tcLoadCorpus(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("corpus failed the strict re-load (nothing written to %s): %w", path, err)
	}
	if len(loaded) != len(runs) {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("corpus re-loaded %d runs, expected %d (nothing written to %s)", len(loaded), len(runs), path)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return hash, nil
}

// tcLoadCorpus reads a corpus strictly: an unknown key is an error (a typo must not become a
// silently missing measurement), ids are unique, and a vet-class string refuses the corpus.
// The hash is the sha256 of the file bytes with any CR removed, so a corpus checked out with
// CRLF line endings still matches the hash stamped when it was written.
func tcLoadCorpus(path string) ([]tcRun, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	var runs []tcRun
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<24)
	line := 0
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		var r tcRun
		if err := dec.Decode(&r); err != nil {
			return nil, "", fmt.Errorf("corpus %s line %d: %w", path, line, err)
		}
		if err := tcValidateRun(r); err != nil {
			// The id is named only when it is clean: a refused value is never echoed back.
			if len(compeval.PIIClasses(r.ID)) > 0 {
				return nil, "", fmt.Errorf("corpus %s line %d: %w", path, line, err)
			}
			return nil, "", fmt.Errorf("corpus %s line %d (%s): %w", path, line, r.ID, err)
		}
		if seen[r.ID] {
			return nil, "", fmt.Errorf("corpus %s line %d: duplicate id %q", path, line, r.ID)
		}
		seen[r.ID] = true
		runs = append(runs, r)
	}
	if err := sc.Err(); err != nil {
		return nil, "", err
	}
	if len(runs) == 0 {
		return nil, "", fmt.Errorf("corpus %s holds no runs", path)
	}
	return runs, hash, nil
}

func tcValidateRun(r tcRun) error {
	if r.ID == "" {
		return fmt.Errorf("missing id")
	}
	if r.Source != tcSourceAgentTrace && r.Source != tcSourceDelegation {
		return fmt.Errorf("unknown source %q", r.Source)
	}
	if r.Steps < 0 {
		return fmt.Errorf("negative steps")
	}
	if r.Day != "" && !tcDayRe.MatchString(r.Day) {
		return fmt.Errorf("day %q is not YYYY-MM-DD", r.Day)
	}
	if r.Pager != nil {
		in := tcPagerIn{Basis: r.Pager.Basis, Evictions: r.Pager.Evictions, EvictedBytes: r.Pager.EvictedBytes,
			DistinctEvicted: r.Pager.DistinctEvicted, Refetched: r.Pager.Refetched, CompactionPasses: &r.Pager.CompactionPasses}
		if _, ok := in.run(); !ok {
			return fmt.Errorf("pager object is inconsistent")
		}
	}
	if r.Prefill != nil && (r.Prefill.ObservedSteps <= 0 || r.Prefill.PrefillTokens < 0 || r.Prefill.CacheTokens < 0) {
		return fmt.Errorf("prefill object is inconsistent (observed_steps must be > 0: an unmeasured run has no prefill object)")
	}
	if r.SeatTokensIn != nil && *r.SeatTokensIn <= 0 {
		return fmt.Errorf("seat_tokens_in must be positive when present (an unreported figure is null)")
	}
	if r.LadderArtifacts != nil && *r.LadderArtifacts < 0 {
		return fmt.Errorf("negative ladder_artifacts")
	}
	for _, s := range []string{r.ID, r.Source, r.Seat, r.Node, r.Day, r.StopReason} {
		if classes := compeval.PIIClasses(s); len(classes) > 0 {
			return fmt.Errorf("corpus refused: %s (a corpus must be vetted, never scrubbed in place)", strings.Join(classes, ", "))
		}
	}
	if tcPathRe.MatchString(r.Seat) || tcPathRe.MatchString(r.Node) || tcPathRe.MatchString(r.StopReason) {
		return fmt.Errorf("corpus refused: a seat, node id or stop reason is a local path (harvest replaces those with a placeholder)")
	}
	return nil
}

// ---- the readout ----

// tcReport is the readout. It has no timestamps and no paths, so the same corpus always yields
// the same bytes. Every rate is a pointer: null means the data to compute it was not recorded.
//
// By seat: by_seat holds the runs a seat can be named for, keyed by the seat the record names, and
// the runs whose record names no seat (every agent trace) under the key "(unrecorded)". A pool
// placement logs the pool's ALIAS as the seat (the node's real seat is in neither the row nor its
// result), so a run on one cannot be attributed to a seat, and it is not in by_seat: it is reported
// apart as pool_alias (the whole bucket) and pool_alias_by_node (the same runs keyed by the node the
// row names, "(unrecorded)" when it names none). attribution counts the three classes and says how
// much of the corpus a per-seat reading cannot reach.
type tcReport struct {
	Schema          string             `json:"schema"`
	CorpusHash      string             `json:"corpus_hash"`
	PagerGate       float64            `json:"pager_gate"`
	Attribution     tcAttribution      `json:"attribution"`
	All             tcStats            `json:"all"`
	BySource        map[string]tcStats `json:"by_source"`
	BySeat          map[string]tcStats `json:"by_seat"`
	PoolAlias       tcStats            `json:"pool_alias"`
	PoolAliasByNode map[string]tcStats `json:"pool_alias_by_node"`
}

// tcAttribution splits the runs by how far a seat can be named for them: the record names a seat,
// the record names a pool alias (a placement, not a seat), or the record names nothing. The
// unattributed share is the last two over all runs, nil for no runs.
type tcAttribution struct {
	Runs               int      `json:"runs"`
	SeatNamedRuns      int      `json:"seat_named_runs"`
	PoolAliasRuns      int      `json:"pool_alias_runs"`
	SeatUnrecordedRuns int      `json:"seat_unrecorded_runs"`
	UnattributedShare  *float64 `json:"unattributed_share"`
}

// tcPoolAliasRe: a pool alias is a seat name in which "pool" is a whole hyphen-delimited token
// (agent-pool, arm-pool, agent-pool-3card). "spool" and "carpool-a" are not aliases.
var tcPoolAliasRe = regexp.MustCompile(`(^|-)pool(-|$)`)

func tcIsPoolAlias(seat string) bool { return tcPoolAliasRe.MatchString(seat) }

type tcStats struct {
	Runs             int            `json:"runs"`
	TraceBearingRuns int            `json:"trace_bearing_runs"`
	Steps            int64          `json:"steps"`
	Pager            tcPagerStats   `json:"pager"`
	Compaction       tcCompaction   `json:"compaction"`
	Prefill          tcPrefillStats `json:"prefill"`
}

// tcPagerStats pools the runs that recorded a pager object. The rate is distinct payloads
// re-fetched over distinct payloads evicted, summed over the runs that evicted something.
type tcPagerStats struct {
	RecordedRuns      int      `json:"recorded_runs"`
	MeasuredRuns      int      `json:"measured_runs"`
	UnexercisedRuns   int      `json:"unexercised_runs"`
	UnrecordedRuns    int      `json:"unrecorded_runs"`
	Evictions         int64    `json:"evictions"`
	DistinctEvicted   int64    `json:"distinct_evicted"`
	RefetchedDistinct int64    `json:"refetched_distinct"`
	RefetchRate       *float64 `json:"refetch_rate"`
	Gate              float64  `json:"gate"`
	Basis             string   `json:"basis"`
	Verdict           string   `json:"verdict"`
}

// tcCompaction: recorded_runs are the runs whose pager object carried the pass count. The
// transcript block is the evidence an agent trace gives without it: a lower bound.
type tcCompaction struct {
	RecordedRuns        int      `json:"recorded_runs"`
	UnrecordedRuns      int      `json:"unrecorded_runs"`
	RunsWithPass        int      `json:"runs_with_pass"`
	FireRate            *float64 `json:"fire_rate"`
	Passes              int64    `json:"passes"`
	PassesPerRun        *float64 `json:"passes_per_run"`
	StepsInRecordedRuns int64    `json:"steps_in_recorded_runs"`
	PassesPerStep       *float64 `json:"passes_per_step"`
	TranscriptRuns      int      `json:"transcript_runs"`
	RunsWithArtifacts   int      `json:"transcript_runs_with_ladder_artifacts"`
	LowerBoundFireRate  *float64 `json:"transcript_lower_bound_fire_rate"`
}

// tcPrefillStats: a run is MEASURED when the backend reported token counts for its steps. A
// prompt has at least one token, so a run that observed steps and reported zero prefilled AND
// zero cached tokens came from an engine that gives no token timings (the step counter ticks
// because a stats block arrived, not because tokens were counted): it is counted apart as
// no_token_counts and contributes nothing to the token sums, never a measured zero.
type tcPrefillStats struct {
	MeasuredRuns            int      `json:"measured_runs"`
	NoTokenCountsRuns       int      `json:"no_token_counts_runs"`
	UnmeasuredRuns          int      `json:"unmeasured_runs"`
	ObservedSteps           int64    `json:"observed_steps"`
	PrefillTokens           int64    `json:"prefill_tokens"`
	CacheTokens             int64    `json:"cache_tokens"`
	KVReusePct              *float64 `json:"kv_reuse_pct"`
	AvgPrefillTokensPerStep *float64 `json:"avg_prefill_tokens_per_step"`
	AvgPrefillTokensPerRun  *float64 `json:"avg_prefill_tokens_per_run"`
	// Seat input tokens: the prompt tokens the seat processed per run, cache hits included,
	// over the runs that reported it. Reported by engines whose prefill timings are absent.
	SeatInputRuns             int      `json:"seat_input_runs"`
	SeatInputTokens           int64    `json:"seat_input_tokens"`
	SeatInputSteps            int64    `json:"seat_input_steps"`
	AvgSeatInputTokensPerStep *float64 `json:"avg_seat_input_tokens_per_step"`
	// The per-run spread (an average hides the long runs): prefilled tokens over the measured
	// runs, and the mean prompt size per step (seat input tokens over steps) over the runs that
	// reported seat input tokens and ran at least one step.
	PrefillTokensPerRun          tcDist `json:"prefill_tokens_per_run"`
	SeatInputTokensPerStepPerRun tcDist `json:"seat_input_tokens_per_step_per_run"`
}

// tcDist is a nearest-rank summary of one value per run: every figure is a value some run had,
// and an empty set has runs 0 and null figures, never zeros.
type tcDist struct {
	Runs int    `json:"runs"`
	P50  *int64 `json:"p50"`
	P90  *int64 `json:"p90"`
	Max  *int64 `json:"max"`
}

// tcSummarize sorts vals in place and returns its nearest-rank summary.
func tcSummarize(vals []int64) tcDist {
	d := tcDist{Runs: len(vals)}
	if len(vals) == 0 {
		return d
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	at := func(q float64) *int64 {
		v := vals[int(math.Ceil(q*float64(len(vals))))-1]
		return &v
	}
	d.P50, d.P90 = at(0.5), at(0.9)
	top := vals[len(vals)-1]
	d.Max = &top
	return d
}

// tcRound keeps six decimals: a rate in a baseline file reads, and the rounding is the same
// arithmetic on every platform.
func tcRound(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// tcRate is num/den as a pointer, nil when den is 0: no denominator is not a zero rate.
func tcRate(num, den float64) *float64 {
	if den == 0 {
		return nil
	}
	v := tcRound(num / den)
	return &v
}

func tcStatsFor(runs []tcRun) tcStats {
	var s tcStats
	s.Pager.Gate = agent.PagerGate
	var prefillPerRun, promptPerStep []int64
	for _, r := range runs {
		s.Runs++
		if r.TraceBearing {
			s.TraceBearingRuns++
		}
		s.Steps += int64(r.Steps)

		if r.Pager == nil {
			s.Pager.UnrecordedRuns++
			s.Compaction.UnrecordedRuns++
		} else {
			s.Pager.RecordedRuns++
			s.Pager.Evictions += int64(r.Pager.Evictions)
			if r.Pager.Basis == tcBasisMeasured {
				s.Pager.MeasuredRuns++
				s.Pager.DistinctEvicted += int64(r.Pager.DistinctEvicted)
				s.Pager.RefetchedDistinct += int64(r.Pager.Refetched)
			} else {
				s.Pager.UnexercisedRuns++
			}
			s.Compaction.RecordedRuns++
			s.Compaction.Passes += int64(r.Pager.CompactionPasses)
			s.Compaction.StepsInRecordedRuns += int64(r.Steps)
			if r.Pager.CompactionPasses > 0 {
				s.Compaction.RunsWithPass++
			}
		}
		if r.LadderArtifacts != nil {
			s.Compaction.TranscriptRuns++
			if *r.LadderArtifacts > 0 {
				s.Compaction.RunsWithArtifacts++
			}
		}
		if r.SeatTokensIn != nil {
			s.Prefill.SeatInputRuns++
			s.Prefill.SeatInputTokens += *r.SeatTokensIn
			s.Prefill.SeatInputSteps += int64(r.Steps)
			if r.Steps > 0 {
				promptPerStep = append(promptPerStep, *r.SeatTokensIn/int64(r.Steps))
			}
		}
		switch {
		case r.Prefill == nil:
			s.Prefill.UnmeasuredRuns++
		case r.Prefill.PrefillTokens+r.Prefill.CacheTokens == 0:
			s.Prefill.NoTokenCountsRuns++
		default:
			s.Prefill.MeasuredRuns++
			s.Prefill.ObservedSteps += int64(r.Prefill.ObservedSteps)
			s.Prefill.PrefillTokens += r.Prefill.PrefillTokens
			s.Prefill.CacheTokens += r.Prefill.CacheTokens
			prefillPerRun = append(prefillPerRun, r.Prefill.PrefillTokens)
		}
	}

	p := &s.Pager
	p.Basis = tcBasisInsufficient
	p.Verdict = tcVerdictNoData
	if p.MeasuredRuns > 0 {
		p.Basis = tcBasisMeasured
		p.RefetchRate = tcRate(float64(p.RefetchedDistinct), float64(p.DistinctEvicted))
		if p.RefetchRate != nil {
			if *p.RefetchRate < agent.PagerGate {
				p.Verdict = tcVerdictBelow
			} else {
				p.Verdict = tcVerdictAbove
			}
		}
	}

	c := &s.Compaction
	c.FireRate = tcRate(float64(c.RunsWithPass), float64(c.RecordedRuns))
	c.PassesPerRun = tcRate(float64(c.Passes), float64(c.RecordedRuns))
	c.PassesPerStep = tcRate(float64(c.Passes), float64(c.StepsInRecordedRuns))
	c.LowerBoundFireRate = tcRate(float64(c.RunsWithArtifacts), float64(c.TranscriptRuns))

	f := &s.Prefill
	f.KVReusePct = tcRate(float64(f.CacheTokens)*100, float64(f.CacheTokens+f.PrefillTokens))
	f.AvgPrefillTokensPerStep = tcRate(float64(f.PrefillTokens), float64(f.ObservedSteps))
	f.AvgPrefillTokensPerRun = tcRate(float64(f.PrefillTokens), float64(f.MeasuredRuns))
	f.AvgSeatInputTokensPerStep = tcRate(float64(f.SeatInputTokens), float64(f.SeatInputSteps))
	f.PrefillTokensPerRun = tcSummarize(prefillPerRun)
	f.SeatInputTokensPerStepPerRun = tcSummarize(promptPerStep)
	return s
}

func tcBuildReport(runs []tcRun, hash string) tcReport {
	rep := tcReport{Schema: tcSchema, CorpusHash: hash, PagerGate: agent.PagerGate,
		All: tcStatsFor(runs), BySource: map[string]tcStats{}, BySeat: map[string]tcStats{}, PoolAliasByNode: map[string]tcStats{}}
	bySource, bySeat, byNode := map[string][]tcRun{}, map[string][]tcRun{}, map[string][]tcRun{}
	var pool []tcRun
	for _, r := range runs {
		bySource[r.Source] = append(bySource[r.Source], r)
		switch {
		case r.Seat == "":
			rep.Attribution.SeatUnrecordedRuns++
			bySeat[tcSeatUnrecorded] = append(bySeat[tcSeatUnrecorded], r)
		case tcIsPoolAlias(r.Seat):
			rep.Attribution.PoolAliasRuns++
			pool = append(pool, r)
			node := r.Node
			if node == "" {
				node = tcSeatUnrecorded
			}
			byNode[node] = append(byNode[node], r)
		default:
			rep.Attribution.SeatNamedRuns++
			bySeat[r.Seat] = append(bySeat[r.Seat], r)
		}
	}
	rep.Attribution.Runs = len(runs)
	rep.Attribution.UnattributedShare = tcRate(float64(rep.Attribution.PoolAliasRuns+rep.Attribution.SeatUnrecordedRuns), float64(len(runs)))
	rep.PoolAlias = tcStatsFor(pool)
	for k, v := range bySource {
		rep.BySource[k] = tcStatsFor(v)
	}
	for k, v := range bySeat {
		rep.BySeat[k] = tcStatsFor(v)
	}
	for k, v := range byNode {
		rep.PoolAliasByNode[k] = tcStatsFor(v)
	}
	return rep
}

func tcMarshalReport(rep tcReport) ([]byte, error) {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// tcCheck compares the report for the corpus against a frozen baseline, byte for byte. A
// baseline from a different corpus is refused rather than diffed.
func tcCheck(baseline []byte, rep tcReport) error {
	baseline = bytes.ReplaceAll(baseline, []byte("\r\n"), []byte("\n"))
	var head struct {
		Schema     string `json:"schema"`
		CorpusHash string `json:"corpus_hash"`
	}
	if err := json.Unmarshal(baseline, &head); err != nil {
		return fmt.Errorf("baseline is not a trace-corpus report: %w", err)
	}
	if head.Schema != tcSchema {
		return fmt.Errorf("baseline schema %q != %q", head.Schema, tcSchema)
	}
	if head.CorpusHash != rep.CorpusHash {
		return fmt.Errorf("check refused: baseline corpus %.12s != corpus %.12s (re-freeze on the new corpus instead)", head.CorpusHash, rep.CorpusHash)
	}
	cur, err := tcMarshalReport(rep)
	if err != nil {
		return err
	}
	if bytes.Equal(baseline, cur) {
		return nil
	}
	bl, cl := strings.Split(string(baseline), "\n"), strings.Split(string(cur), "\n")
	for i := 0; i < len(bl) || i < len(cl); i++ {
		var a, b string
		if i < len(bl) {
			a = bl[i]
		}
		if i < len(cl) {
			b = cl[i]
		}
		if a != b {
			return fmt.Errorf("check BREACHED: the report no longer reproduces the baseline (first difference at line %d: baseline %q, now %q)", i+1, strings.TrimSpace(a), strings.TrimSpace(b))
		}
	}
	return fmt.Errorf("check BREACHED: the report no longer reproduces the baseline")
}

// ---- the verb ----

func runTraceCorpus(args []string) error {
	return runTraceCorpusTo(args, os.Stdout, os.Stderr)
}

func tcSet(csv string) map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out[s] = true
		}
	}
	return out
}

func runTraceCorpusTo(args []string, stdout, stderr io.Writer) error {
	const usage = "usage: local-offload trace-corpus <harvest|readout|freeze|check> [flags] (harvest takes --traces/--delegation-log/--out; the others take --corpus)"
	if len(args) < 1 {
		return fmt.Errorf("%s", usage)
	}
	mode := args[0]
	fs := flag.NewFlagSet("trace-corpus "+mode, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.String("config", "", "config file path (only used by harvest, to find the default source directories)")
	corpusPath := fs.String("corpus", "", "readout/freeze/check: the harvested corpus (JSONL)")
	baselinePath := fs.String("baseline", "", "check: the frozen report")
	outPath := fs.String("out", "", "harvest: corpus file to write; readout/freeze: file to write the report to")
	tracesDir := fs.String("traces", "", "harvest: directory of standalone agent trace files (*.json)")
	delegationDir := fs.String("delegation-log", "", "harvest: delegation-log directory (YYYY-MM-DD.jsonl shards)")
	seats := fs.String("seat", "", "harvest: keep only these seats (comma list); agent traces name no seat and are then skipped")
	excludeSeats := fs.String("exclude-seat", "fake-seat", "harvest: drop these seats (comma list; the default drops the harness's own test seat)")
	since := fs.String("since", "", "harvest: first delegation shard day to read (YYYY-MM-DD)")
	until := fs.String("until", "", "harvest: last delegation shard day to read (YYYY-MM-DD)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("trace-corpus %s: unexpected argument %q", mode, fs.Arg(0))
	}

	switch mode {
	case "harvest":
		return tcRunHarvest(fs, stdout, stderr, tcHarvestOpts{
			TracesDir: *tracesDir, DelegationDir: *delegationDir,
			Seats: tcSet(*seats), ExcludeSeats: tcSet(*excludeSeats), Since: *since, Until: *until,
		}, *outPath)
	case "readout", "freeze", "check":
		if *corpusPath == "" {
			return fmt.Errorf("trace-corpus %s: --corpus is required", mode)
		}
		runs, hash, err := tcLoadCorpus(*corpusPath)
		if err != nil {
			return err
		}
		rep := tcBuildReport(runs, hash)
		switch mode {
		case "readout":
			b, err := tcMarshalReport(rep)
			if err != nil {
				return err
			}
			if *outPath != "" {
				return os.WriteFile(*outPath, b, 0o644)
			}
			_, err = stdout.Write(b)
			return err
		case "freeze":
			if *outPath == "" {
				return fmt.Errorf("trace-corpus freeze: --out is required")
			}
			b, err := tcMarshalReport(rep)
			if err != nil {
				return err
			}
			if err := os.WriteFile(*outPath, b, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(stderr, "[trace-corpus] baseline frozen: %s (corpus %.12s, %d runs)\n", *outPath, hash, len(runs))
			return nil
		default:
			if *baselinePath == "" {
				return fmt.Errorf("trace-corpus check: --baseline is required")
			}
			base, err := os.ReadFile(*baselinePath)
			if err != nil {
				return err
			}
			if err := tcCheck(base, rep); err != nil {
				return err
			}
			fmt.Fprintf(stderr, "[trace-corpus] baseline reproduced (corpus %.12s, %d runs)\n", hash, len(runs))
			return nil
		}
	default:
		return fmt.Errorf("trace-corpus: unknown mode %q (harvest|readout|freeze|check)", mode)
	}
}

// tcRunHarvest resolves the source directories (explicit flags, else the install root's
// defaults), harvests, and writes the corpus.
func tcRunHarvest(fs *flag.FlagSet, stdout, stderr io.Writer, o tcHarvestOpts, out string) error {
	if out == "" {
		return fmt.Errorf("trace-corpus harvest: --out is required")
	}
	if o.TracesDir == "" && o.DelegationDir == "" {
		cfg := loadCfg(fs)
		base := cfg.BaseDir()
		if d := agent.StateFile(base, "agent-traces"); d != "" && tcIsDir(d) {
			o.TracesDir = d
		}
		if d := filepath.Join(base, "delegation-log"); tcIsDir(d) {
			o.DelegationDir = d
		}
		if o.TracesDir == "" && o.DelegationDir == "" {
			return fmt.Errorf("trace-corpus harvest: no source given and neither agent-traces nor delegation-log exists under %s (pass --traces and/or --delegation-log)", base)
		}
		fmt.Fprintf(stderr, "[trace-corpus] sources from the install root: traces=%t delegation-log=%t\n", o.TracesDir != "", o.DelegationDir != "")
	}
	for _, d := range []struct{ flag, path string }{{"--traces", o.TracesDir}, {"--delegation-log", o.DelegationDir}} {
		if d.path != "" && !tcIsDir(d.path) {
			return fmt.Errorf("trace-corpus harvest: %s %q is not a directory", d.flag, d.path)
		}
	}
	runs, stats, err := tcHarvest(o)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		return fmt.Errorf("trace-corpus harvest: no usable runs (%d trace file(s), %d delegation row(s) read, all skipped)", stats.TraceFiles, stats.DelegationRows)
	}
	hash, err := tcWriteCorpus(out, runs)
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "[trace-corpus] harvested %d run(s) → %s (corpus %.12s)\n", len(runs), out, hash)
	b, err := json.MarshalIndent(struct {
		CorpusHash string         `json:"corpus_hash"`
		Stats      tcHarvestStats `json:"stats"`
	}{hash, stats}, "", "  ")
	if err != nil {
		return err
	}
	_, err = stdout.Write(append(b, '\n'))
	return err
}

func tcIsDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
