// Package ledger records per-call telemetry to an append-only JSONL file and
// reports estimated tokens (and dollars) kept out of Opus — how the harness
// proves it earns its keep.
//
// JSONL, not a locked key-value store, on purpose: the long-running MCP server
// and an occasional `local-offload ledger` invocation must both touch the
// ledger at once. Writers append with O_APPEND (atomic for the small one-line
// records on both POSIX and Windows); the reader takes no lock and tolerates a
// not-yet-complete trailing line. This is what lets the savings report run while
// the MCP server is live (the bbolt version could not — exclusive file lock).
package ledger

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"unicode/utf8"
)

// Entry is one offload call. The self-learning fields (margin..feat) are written
// by the pipeline from core.Meta; old lines without them parse fine (zero values).
type Entry struct {
	TS        int64  `json:"ts"`
	Task      string `json:"task"`
	TokensIn  int    `json:"tokens_in"`
	TokensOut int    `json:"tokens_out"`
	// SeatTokensIn is prompt work a seat did (agent rows), NOT tokens saved:
	// the summary never adds it to TokensSaved. Absent on pre-0.115.5 rows.
	SeatTokensIn int     `json:"seat_tokens_in,omitempty"`
	LatencyMs    int64   `json:"latency_ms"`
	TokPerSec    float64 `json:"tok_per_s"`
	CacheHit     bool    `json:"cache_hit"`
	Deferred     bool    `json:"deferred"`
	// --- self-learning signals (Phase 0 enrichment) ---
	Margin          float64            `json:"margin,omitempty"`
	ModelTier       string             `json:"model_tier,omitempty"`
	Escalations     int                `json:"escalations,omitempty"`
	Reasoning       bool               `json:"reasoning,omitempty"` // produced by the terminal reasoning tier (a reclaimed deferral)
	Retries         int                `json:"retries,omitempty"`
	Truncated       bool               `json:"truncated,omitempty"`
	Grounded        *bool              `json:"grounded,omitempty"`
	EscalatedAgreed *bool              `json:"escalated_agreed,omitempty"`
	ErrClass        string             `json:"err_class,omitempty"`
	InputChars      int                `json:"input_chars,omitempty"`
	Feat            map[string]float64 `json:"feat,omitempty"`
	// Reason is the human-readable defer reason (LO-8), set only on deferred
	// entries and truncated to maxReasonLen on write. Old ledger lines without
	// the field parse fine (empty string).
	Reason string `json:"reason,omitempty"`
	// EscSource names WHICH gate sent the call up a tier, from core's closed
	// EscalationSource set. Unlike Reason it is written on SUCCESSFUL
	// escalations too — that was the gap: a call that escalated and then
	// succeeded recorded nothing, so the split between the one self-declared
	// gate (self_confidence) and the structural ones (margin/confhead/schema/
	// grounding/verifier/retries) could not be recovered from telemetry.
	// Closed set = groupable; the free-text Reason is not.
	EscSource string `json:"esc_source,omitempty"`
	// TierPack records how a climbed-to tier's input was packed (TO-3, see
	// core.Meta.TierPack). Empty on entry-tier rows; old ledger lines without
	// the field parse fine.
	TierPack string `json:"tier_pack,omitempty"`
	// Layer names the device layer a composite box served this call on (ADR
	// 0039: single | pair | triple | display), copied from core.Meta.Placed so
	// the utilization scoreboard can later sum work per layer (council R8 held
	// the by_layer summary; the column is what makes it summable). Empty — and
	// omitted — on a plain box, so its rows stay byte-identical; old lines
	// without the field parse fine.
	Layer string `json:"layer,omitempty"`
	// Oracle names a NON-LOCAL counterfactual oracle that produced this label
	// row's judgment (e.g. "nim" for shadow-label --oracle nim). Empty = the
	// local escalation tier (the default), so historic rows parse unchanged and
	// remote-judged labels stay auditable/filterable in the sidecars.
	Oracle string `json:"oracle,omitempty"`
	// --- call identity (memory-frontier Phase 0.1) ---------------------------
	// Hashes, never content. The ledger recorded THAT a call happened but not
	// WHAT it was about, so duplicate-rate, prefix-reuse and counterfactual-replay
	// questions could not be answered from telemetry at all. Storing full prompts
	// was rejected (privacy + multi-brand isolation); these fingerprints give the
	// grouping power without the liability. All omitempty: historic lines parse
	// unchanged, and the fields simply accrue going forward.
	InputSHA256        string   `json:"input_sha256,omitempty"`
	PromptPrefixSHA256 string   `json:"prompt_prefix_sha256,omitempty"`
	ContextHash        string   `json:"context_hash,omitempty"`
	ExemplarIDs        []string `json:"exemplar_ids,omitempty"`
	// CacheBypass names why the result cache was skipped entirely, so
	// "permanently uncacheable input" is one query away instead of being
	// indistinguishable from a cold miss.
	CacheBypass string `json:"cache_bypass,omitempty"`
	// CacheHitInLoop marks a cache hit served from an entry the agent loop wrote
	// (T2-D). In-loop generations run with a nil ledger and are never costed, so
	// without this the share of the cache-hit rate the harness produced for
	// itself is unrecoverable — and that rate is a gate.
	CacheHitInLoop bool `json:"cache_hit_in_loop,omitempty"`
	// Agent-loop prefill (T2-B). PrefillSteps present (>0) is what marks the row as
	// MEASURED; a row without it made no observation, which is not the same as a row
	// that observed zero prefill.
	PrefillSteps int `json:"prefill_steps,omitempty"`
	// AgentProfile: which agent profile the run resolved to. Absent on non-agent rows.
	AgentProfile string `json:"agent_profile,omitempty"`
	// LabelSource names WHICH WRITER produced a confhead-label row.
	//
	// confhead-labels.jsonl has two writers with OPPOSITE populations, and nothing
	// previously distinguished them:
	//   "live-escalation"       - pipeline.labelAgreement, only ESCALATED calls, judged by
	//                             answersAgree (classify/triage only).
	//   "shadow-counterfactual" - shadow.LabelQueue, only NON-escalated calls (captureShadow
	//                             skips anything with Escalations != 0), and it judges
	//                             summarize by embedding cosine rather than by answersAgree.
	//
	// Pooling those yields a rate that is conditional on escalation and unconditional at the
	// same time, which is no rate at all. Absent on pre-existing rows, which is why readers
	// must treat "" as unknown-provenance rather than as either source.
	LabelSource   string  `json:"label_source,omitempty"`
	PrefillTokens int64   `json:"prefill_tokens,omitempty"`
	CacheTokens   int64   `json:"cache_tokens,omitempty"`
	PrefillMS     float64 `json:"prefill_ms,omitempty"`
	// Arm labels which experimental arm this row was recorded under — the
	// LEDGER twin of the delegation log's field of the same name (see
	// delegate.delegationLogLine.Arm for the full argument: once rows from an
	// experiment and ordinary traffic interleave unlabelled in one file, no
	// timestamp separates them afterwards). Set from OFFLOAD_DELEGATE_ARM at
	// record time, process-wide: every row a tagged process records (agent,
	// agent_delegate, cascade offloads it triggers) belongs to that arm's run.
	// Without this, the prefill columns above are measured under an arm and
	// recorded without it — the exact "computed then discarded" defect class
	// the 0.79.0 instrument-honesty round was about, one file over.
	Arm string `json:"arm,omitempty"`
	// --- row provenance (0.124.0, register D-101) ---------------------------
	// OriginSession / OriginPID / OriginPPID name the PROCESS that recorded
	// the row and the session it served (see Origin). Record stamps them on
	// every row that carries none, so every writer in this binary — cascade,
	// agent, delegate, media — is attributed by one rule. Rows from a service
	// or a fleet node carry pids and no session; pre-0.124.0 rows carry
	// neither, and a reader must treat that as UNATTRIBUTED, never as "some
	// other session".
	OriginSession string `json:"origin_session,omitempty"`
	OriginPID     int    `json:"origin_pid,omitempty"`
	OriginPPID    int    `json:"origin_ppid,omitempty"`
	// Door names the SURFACE that admitted the call (register A-102): an MCP
	// tool name ("offload_summarize"), a CLI command ("cli:summarize"), or
	// "fleet" for a request a fleet node ran for a delegator. Origin* above
	// names the process; nothing named the door, so all 558 cascade rows in the
	// live ledger were indistinguishable between an MCP tool call, a hand-run
	// CLI command and fleet dispatch. Copied from core.Meta.Door; absent on
	// pre-0.129.x rows and on any writer that stamps none, which a reader must
	// treat as UNKNOWN DOOR, never as one of the values above.
	Door string `json:"door,omitempty"`
	// CardsTokens is the ONE token figure a share reader wants: the tokens the
	// cards processed for this row — the seat's prompt work (SeatTokensIn on
	// agent rows, TokensIn on cascade rows: the same measurement under two
	// names, kept apart only because TokensIn doubles as the savings column)
	// plus what the seat generated. A cache hit did no card work and records
	// 0. Always written, never omitted: its PRESENCE marks a row that carries
	// this schema, so a reader can stop reconstructing the figure from three
	// columns and an input_chars/4 guess the moment it sees the key.
	CardsTokens int `json:"cards_tokens"`
	// --- the job behind the row (0.124.0, D-101 / F15) ----------------------
	// What the delegate result and the agent wire already knew, so the ledger
	// can answer "which session, which job, how many steps, why it stopped"
	// without opening the delegation-log corpus. All omitempty: a cascade row
	// has no job. Placement is the delegator's placement note, capped like
	// Reason; AcceptanceResult is "pass" | "fail" | "" (nothing evaluated: the
	// run deferred or the wire failed).
	JobID            string `json:"job_id,omitempty"`
	Route            string `json:"route,omitempty"`
	Placement        string `json:"placement,omitempty"`
	Steps            int    `json:"steps,omitempty"`
	StopReason       string `json:"stop_reason,omitempty"`
	RepackMs         int64  `json:"repack_ms,omitempty"`
	AcceptanceResult string `json:"acceptance_result,omitempty"`
}

// Label provenance values for Entry.LabelSource. Constants rather than string literals so
// a reader that filters on one cannot silently miss rows because of a typo at the writer.
const (
	LabelSourceLiveEscalation       = "live-escalation"
	LabelSourceShadowCounterfactual = "shadow-counterfactual"
)

// maxReasonLen bounds a recorded defer reason so a long upstream error can't
// bloat the one-line ledger records (they must stay O_APPEND-atomic-small).
const maxReasonLen = 120

// truncateReason caps s at maxReasonLen bytes, backing off to a rune boundary
// so a multibyte character is never split.
func truncateReason(s string) string {
	if len(s) <= maxReasonLen {
		return s
	}
	cut := s[:maxReasonLen]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// Ledger appends entries to a JSONL file. The mutex serializes in-process
// writes; cross-process safety relies on O_APPEND atomicity for small lines.
type Ledger struct {
	mu   sync.Mutex
	f    *os.File
	path string
	// observer, when set, sees every entry AFTER it is durably written. It
	// runs on the recording goroutine and must not block: the one consumer
	// (internal/pairworkloads) hands the entry to a background sender.
	observer func(Entry)
}

// Observe registers fn to be called with every entry Record writes, after
// the write and its fsync succeed. One observer; a later call replaces it.
func (l *Ledger) Observe(fn func(Entry)) {
	l.mu.Lock()
	l.observer = fn
	l.mu.Unlock()
}

// Open opens (creating if needed) the JSONL ledger for appending. It does NOT
// take an exclusive lock, so multiple processes can append concurrently.
func Open(path string) (*Ledger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Ledger{f: f, path: path}, nil
}

// Close releases the append handle.
func (l *Ledger) Close() error {
	if l.f == nil {
		return nil
	}
	return l.f.Close()
}

// Record appends one entry as a single JSON line.
func (l *Ledger) Record(e Entry) error {
	if e.TS == 0 {
		e.TS = time.Now().Unix()
	}
	e.Reason = truncateReason(e.Reason)
	e.Placement = truncateReason(e.Placement)
	// Provenance and the one token figure are stamped HERE, on the one path
	// every writer takes, so no record site can forget them (D-101).
	stampOrigin(&e)
	if e.CardsTokens == 0 {
		e.CardsTokens = cardsTokens(e)
	}
	val, err := json.Marshal(e)
	if err != nil {
		return err
	}
	val = append(val, '\n')
	l.mu.Lock()
	obs := l.observer
	if _, err = l.f.Write(val); err != nil {
		l.mu.Unlock()
		return err
	}
	// fsync each entry so a crash can't lose recorded savings — sub-ms on NVMe,
	// negligible against multi-second model inference on the call path.
	err = l.f.Sync()
	l.mu.Unlock()
	if err == nil && obs != nil {
		// Outside the lock: an observer that recorded (or read) the ledger
		// re-entrantly would otherwise deadlock.
		obs(e)
	}
	return err
}

// labelMu serializes sidecar appends across concurrent in-process callers (the
// long-running MCP server may label several escalations at once). Cross-process
// safety relies on O_APPEND atomicity for the small one-line records.
var labelMu sync.Mutex

// AppendLabel appends e as ONE JSON line to a correctness-label sidecar file
// (NOT the main ledger — kept separate so the router/calibration/savings report
// stay pristine). Only the confhead reads it. It creates the parent dir and
// stamps TS if unset; concurrent callers are serialized by labelMu.
func AppendLabel(path string, e Entry) error {
	if e.TS == 0 {
		e.TS = time.Now().Unix()
	}
	val, err := json.Marshal(e)
	if err != nil {
		return err
	}
	val = append(val, '\n')
	labelMu.Lock()
	defer labelMu.Unlock()
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(val)
	return err
}

// ReadLabelFile loads every parseable entry from a correctness-label sidecar
// (mirrors ReadAll). A missing file returns (nil, nil); blank/malformed lines
// are skipped.
func ReadLabelFile(path string) ([]Entry, error) {
	return ReadAll(path)
}

// Summary aggregates the ledger.
type Summary struct {
	Calls             int `json:"calls"`
	CacheHits         int `json:"cache_hits"`
	Deferred          int `json:"deferred"`
	Completed         int `json:"completed"`
	ReasoningReclaims int `json:"reasoning_reclaims"` // completed via the terminal reasoning tier (deferrals it reclaimed before Opus)
	TokensSaved       int `json:"tokens_saved"`       // input tokens kept out of Opus on completed/cache calls
	TokensOut         int `json:"tokens_out"`
	// EstValueKeptLocal is the estimated Opus value of the work kept local:
	// tokens_saved x opus_input_price_per_mtok + tokens_out x
	// opus_output_price_per_mtok, both per 1M. BOTH halves are priced — the
	// input tokens the cloud never received AND the output tokens a local seat
	// generated in its place (0.117.7; before that output was priced at zero,
	// which is the expensive half on Opus). It is an estimate of avoided cloud
	// pricing, NOT literal billed dollars saved (LO-12: the old est_dollar_saved
	// name presented it as money in the bank).
	EstValueKeptLocal float64 `json:"est_value_kept_local"`
	// Deprecated: the same number under the old, misleading name — kept
	// emitted for one release for consumers of the JSON; remove in v0.7.
	EstDollarSaved float64        `json:"est_dollar_saved"`
	ByTask         map[string]int `json:"by_task"`
}

// Prices values what a local seat produced, in Opus dollars per 1M tokens.
//
// TWO rates, not one, because the cloud bills two: on Opus an output token
// costs FIVE TIMES an input token, and this package's whole claim is that a
// local seat did work the cloud would otherwise have billed. One rate cannot
// express that, and the single rate this package took priced output at zero.
type Prices struct {
	// InputPerMTok values prompt tokens kept out of the cloud (config
	// `opus_input_price_per_mtok`).
	InputPerMTok float64
	// OutputPerMTok values tokens the local seat GENERATED (config
	// `opus_output_price_per_mtok`). A caller with no configured value passes
	// DefaultPrices.OutputPerMTok - never 0, which is the defect this field
	// exists to close.
	OutputPerMTok float64
}

// DefaultPrices are the shipped Opus list rates ($/1M tokens) and the fallback
// an absent/zero config key resolves to, so an un-updated config under-reports
// by a KNOWN factor instead of silently zeroing half the accounting.
var DefaultPrices = Prices{InputPerMTok: 15.0, OutputPerMTok: 75.0}

// PricesFrom resolves a config's two price keys against DefaultPrices. A
// non-positive key falls back to the shipped rate INDEPENDENTLY of the other,
// which is the whole point: every config written before `opus_output_price_per_mtok`
// existed carries an input price and no output price, and the fallback is what
// keeps those files from re-creating the priced-at-zero defect on upgrade.
// Floats, not a config struct, so the ledger keeps depending on nothing.
func PricesFrom(inputPerMTok, outputPerMTok float64) Prices {
	p := DefaultPrices
	if inputPerMTok > 0 {
		p.InputPerMTok = inputPerMTok
	}
	if outputPerMTok > 0 {
		p.OutputPerMTok = outputPerMTok
	}
	return p
}

// Summarize aggregates this ledger's file since `since` (unix; 0 = all).
func (l *Ledger) Summarize(since int64, prices Prices) (Summary, error) {
	return SummarizeFile(l.path, since, prices)
}

// ReadAll loads every parseable entry from a JSONL ledger (lock-free; skips
// malformed/partial lines). Missing file => empty slice, no error.
func ReadAll(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if json.Unmarshal(line, &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// ReasonCount is one aggregated defer reason for the ledger report.
type ReasonCount struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// TopDeferReasons aggregates the defer reasons of entries since `since`
// (unix; 0 = all) and returns the topN most frequent, ties broken
// alphabetically for deterministic output. Deferred entries written before
// the reason field existed count under "(unrecorded)" so the denominator
// stays honest. Lock-free like SummarizeFile; a missing file returns nil.
func TopDeferReasons(path string, since int64, topN int) ([]ReasonCount, error) {
	entries, err := ReadAll(path)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, e := range entries {
		if !e.Deferred || e.CacheHit {
			continue
		}
		if since > 0 && e.TS < since {
			continue
		}
		r := e.Reason
		if r == "" {
			r = "(unrecorded)"
		}
		counts[r]++
	}
	if len(counts) == 0 {
		return nil, nil // nil (not an empty slice) so the JSON field stays omitempty
	}
	out := make([]ReasonCount, 0, len(counts))
	for r, n := range counts {
		out = append(out, ReasonCount{Reason: r, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Reason < out[j].Reason
	})
	if topN > 0 && len(out) > topN {
		out = out[:topN]
	}
	return out, nil
}

// SummarizeFile reads a JSONL ledger without any lock — safe to call while
// another process is appending (a partial final line is skipped). A missing
// file reports an empty summary (nothing offloaded yet), not an error.
func SummarizeFile(path string, since int64, prices Prices) (Summary, error) {
	s := Summary{ByTask: map[string]int{}}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if json.Unmarshal(line, &e) != nil {
			continue // malformed or a not-yet-complete trailing line
		}
		if since > 0 && e.TS < since {
			continue
		}
		s.Calls++
		s.ByTask[e.Task]++
		switch {
		case e.CacheHit:
			s.CacheHits++
			s.TokensSaved += e.TokensIn // a cache hit also saved Opus those tokens
		case e.Deferred:
			s.Deferred++
		default:
			s.Completed++
			s.TokensSaved += e.TokensIn
			s.TokensOut += e.TokensOut
			if e.Reasoning {
				s.ReasoningReclaims++ // a completed reasoning entry = a deferral reclaimed before Opus
			}
		}
	}
	// The est. Opus value of what stayed local, BOTH HALVES priced at their own
	// rate. Until 0.117.7 this was TokensSaved x the input rate and nothing else,
	// so every output token a seat generated was multiplied by zero — and output
	// is the half that bills at 5x. The CLAIM is unchanged (LO-12): an estimate of
	// avoided cloud pricing, never billed savings.
	s.EstValueKeptLocal = float64(s.TokensSaved)/1_000_000*prices.InputPerMTok +
		float64(s.TokensOut)/1_000_000*prices.OutputPerMTok
	s.EstDollarSaved = s.EstValueKeptLocal // Deprecated alias; remove in v0.7
	return s, sc.Err()
}
