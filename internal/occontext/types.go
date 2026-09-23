// Package occontext is a read-only context instrument for opencode sessions. It reads a
// COPY of opencode's session database and reports, per session and agent, what each LLM
// call cost in prompt tokens, how much of it the server's prefix cache served, what made
// the context grow from one call to the next, where compaction fired, and how long the
// first token took. It is the before/after gate for changes to what opencode sends a
// local seat. See docs/systems/opencode-integration.md ("Measuring context").
package occontext

import "time"

// Cutoff marks the moment a server began reporting cached prompt tokens. Calls created
// before Since report cache figures that are artifacts (a seat that does not return
// prompt_tokens_details logs cached 0 even on a hit), so they leave every cache ratio.
// An empty Model applies to every call; otherwise Model matches the call's model id or
// its provider/model id.
type Cutoff struct {
	Model string    `json:"model,omitempty"`
	Since time.Time `json:"since"`
}

// Options selects sessions and sets the parameters of the analysis.
type Options struct {
	// Last keeps the N most recent primary sessions, each with its child sessions.
	// 0 keeps every session.
	Last int
	// Since keeps sessions created at or after it (zero: no bound).
	Since time.Time
	// Sessions keeps these session ids and their children (empty: no filter).
	Sessions []string
	// Model keeps calls whose model id or provider/model id contains it (empty: all).
	Model string
	// CacheValidSince lists the cache-reporting cutoffs.
	CacheValidSince []Cutoff
	// CacheBlock is the server's prefix-cache block in tokens (0: no block check).
	// Cached counts come in whole blocks; a remainder means the block is wrong.
	CacheBlock int
	// ToolBytesPerToken and TextBytesPerToken convert stored bytes to estimated tokens
	// for the two growth categories opencode stores no token count for.
	ToolBytesPerToken float64
	TextBytesPerToken float64
	// Titles includes session titles. Off by default so a report carries no session
	// content and is safe to paste.
	Titles bool
}

// Store is what Load reads from the db copy.
type Store struct {
	Sessions []Session
	// Messages per session id, ordered as opencode orders them (time_created, id).
	Messages map[string][]Message
	// Unparsed counts message and part rows whose data column did not parse.
	Unparsed int
}

// Session is one opencode session row.
type Session struct {
	ID       string
	ParentID string
	Title    string
	Created  time.Time
}

// Message is one message row; an assistant message is one LLM call.
type Message struct {
	ID        string
	SessionID string
	Role      string
	Agent     string
	Provider  string
	Model     string
	Created   time.Time
	Completed time.Time
	Summary   bool
	Tokens    Tokens
	Parts     []Part
}

// Tokens is opencode's per-call accounting. Output excludes Reasoning; Input excludes
// the cached tokens for OpenAI-compatible providers, so the prompt is
// Input + CacheRead + CacheWrite.
type Tokens struct {
	Input      int
	Output     int
	Reasoning  int
	CacheRead  int
	CacheWrite int
	Total      int
}

// Prompt is the call's prompt size in tokens.
func (t Tokens) Prompt() int { return t.Input + t.CacheRead + t.CacheWrite }

// Part is the subset of a part row the analysis needs. Text and tool output are held
// only to be measured in bytes; nothing in a Report carries them.
type Part struct {
	ID        string
	Type      string
	Created   time.Time // the row's time_created
	Start     time.Time // data.time.start (reasoning, text)
	Text      string
	Synthetic bool
	Tool      string
	ToolOut   string // completed output or error text
	Auto      bool   // compaction part
	Overflow  bool   // compaction part
}

// Report is the analysis result; it marshals to the --json output.
type Report struct {
	Source   SourceInfo      `json:"source"`
	Params   Params          `json:"params"`
	Sessions []SessionReport `json:"sessions"`
	Groups   []GroupReport   `json:"groups"`
	Cache    CacheStats      `json:"cache"`
	Hints    []string        `json:"hints,omitempty"`
}

// SourceInfo describes the copy the report was read from.
type SourceInfo struct {
	DB           string   `json:"db"`
	Copied       []string `json:"copied"`
	Attempts     int      `json:"attempts"`
	QuickCheckOK bool     `json:"quick_check_ok"`
	UnparsedRows int      `json:"unparsed_rows,omitempty"`
}

// Params echoes the parameters that shape the figures.
type Params struct {
	Last              int      `json:"last,omitempty"`
	Since             string   `json:"since,omitempty"`
	Sessions          []string `json:"sessions,omitempty"`
	Model             string   `json:"model,omitempty"`
	CacheValidSince   []Cutoff `json:"cache_valid_since,omitempty"`
	CacheBlock        int      `json:"cache_block,omitempty"`
	ToolBytesPerToken float64  `json:"tool_bytes_per_token"`
	TextBytesPerToken float64  `json:"text_bytes_per_token"`
}

// SessionReport is one session: its calls in order, its compactions, its growth and
// cache totals.
type SessionReport struct {
	ID          string       `json:"id"`
	ParentID    string       `json:"parent_id,omitempty"`
	Kind        string       `json:"kind"` // "primary" or "child" (session.parent_id set)
	Title       string       `json:"title,omitempty"`
	Created     time.Time    `json:"created"`
	Models      []string     `json:"models"`
	Calls       []CallReport `json:"calls"`
	Stubs       int          `json:"stubs"` // assistant messages with no recorded tokens
	Compactions []Compaction `json:"compactions,omitempty"`
	Growth      GrowthTotals `json:"growth"`
	Cache       CacheStats   `json:"cache"`
}

// CallReport is one LLM call.
type CallReport struct {
	N          int       `json:"n"`
	MessageID  string    `json:"message_id"`
	Agent      string    `json:"agent"`
	Model      string    `json:"model"` // provider/model
	Created    time.Time `json:"created"`
	Prompt     int       `json:"prompt"`
	Cached     int       `json:"cached"`
	CacheWrite int       `json:"cache_write,omitempty"`
	Output     int       `json:"output"`
	Reasoning  int       `json:"reasoning"`
	// ReasoningEst is set when the server reported reasoning 0 but reasoning text was
	// stored (llama.cpp counts reasoning inside output): the text's byte estimate.
	ReasoningEst   *int     `json:"reasoning_est,omitempty"`
	CachedBlocks   *int     `json:"cached_blocks,omitempty"`
	BlockRemainder int      `json:"block_remainder,omitempty"`
	CacheValid     bool     `json:"cache_valid"`
	First          bool     `json:"first"` // the session's first call
	Compaction     bool     `json:"compaction,omitempty"`
	TTFTSec        *float64 `json:"ttft_s,omitempty"`
	WallSec        *float64 `json:"wall_s,omitempty"`
	Tools          []string `json:"tools,omitempty"`
	Growth         *Growth  `json:"growth,omitempty"` // growth INTO this call from the previous one
}

// Growth splits the prompt growth from the previous call into this one. Reasoning and
// Output are the previous call's server-reported counts (replayed into history when the
// chat template preserves thinking); ToolOutput and UserText are byte estimates; Residual
// is what is left (template wrapping, attachments, estimate error). A negative Residual
// larger than Reasoning means the template did not replay the reasoning.
type Growth struct {
	Delta              int  `json:"delta"`
	Reasoning          int  `json:"reasoning"`
	ReasoningEstimated bool `json:"reasoning_estimated,omitempty"`
	Output             int  `json:"output"`
	ToolOutput         int  `json:"tool_output_est"`
	UserText           int  `json:"user_text_est"`
	Residual           int  `json:"residual"`
}

// GrowthTotals sums Growth over consecutive-call pairs, with each category's share of
// the summed Delta in percent.
type GrowthTotals struct {
	Pairs              int      `json:"pairs"`
	NegativePairs      int      `json:"negative_pairs,omitempty"`
	SkippedModelSwitch int      `json:"skipped_model_switch,omitempty"`
	Delta              int      `json:"delta"`
	Reasoning          int      `json:"reasoning"`
	Output             int      `json:"output"`
	ToolOutput         int      `json:"tool_output_est"`
	UserText           int      `json:"user_text_est"`
	Residual           int      `json:"residual"`
	ReasoningShare     *float64 `json:"reasoning_share_pct,omitempty"`
	OutputShare        *float64 `json:"output_share_pct,omitempty"`
	ToolOutputShare    *float64 `json:"tool_output_share_pct,omitempty"`
	UserTextShare      *float64 `json:"user_text_share_pct,omitempty"`
	ResidualShare      *float64 `json:"residual_share_pct,omitempty"`
}

// Compaction is one compaction event: the summary call and the prompt sizes around it.
type Compaction struct {
	MessageID    string    `json:"message_id"`
	At           time.Time `json:"at"`
	Auto         bool      `json:"auto"`
	Overflow     bool      `json:"overflow"`
	BeforePrompt int       `json:"before_prompt"`
	BeforeTotal  int       `json:"before_total"` // what opencode compares with the usable window
	Prompt       int       `json:"prompt"`
	Output       int       `json:"output"`
	Reasoning    int       `json:"reasoning"`
	// ReasoningEstimated: Reasoning is a byte estimate (the server reported 0).
	ReasoningEstimated bool    `json:"reasoning_estimated,omitempty"`
	WallSec            float64 `json:"wall_s"`
	AfterPrompt        *int    `json:"after_prompt,omitempty"`
}

// CacheStats is the prefix-cache read ratio over calls whose cache figures are valid,
// overall, excluding each session's first call, and excluding only cold session-first
// calls (cached 0) — a warm first call is a cross-session prefix hit and stays in.
type CacheStats struct {
	ValidCalls          int      `json:"valid_calls"`
	ExcludedCalls       int      `json:"excluded_calls"` // before a cutoff
	Prompt              int      `json:"prompt"`
	Cached              int      `json:"cached"`
	Pct                 *float64 `json:"pct,omitempty"`
	ExclFirstPrompt     int      `json:"excl_first_prompt"`
	ExclFirstCached     int      `json:"excl_first_cached"`
	ExclFirstPct        *float64 `json:"excl_first_pct,omitempty"`
	ExclColdFirstPrompt int      `json:"excl_cold_first_prompt"`
	ExclColdFirstCached int      `json:"excl_cold_first_cached"`
	ExclColdFirstPct    *float64 `json:"excl_cold_first_pct,omitempty"`
	BlockMisaligned     int      `json:"block_misaligned,omitempty"`
}

// GroupReport rolls calls up by session kind, agent and model.
type GroupReport struct {
	Kind              string       `json:"kind"`
	Agent             string       `json:"agent"`
	Model             string       `json:"model"`
	Sessions          int          `json:"sessions"`
	Calls             int          `json:"calls"`
	FirstCallPrompts  []int        `json:"first_call_prompts"` // per session, oldest first
	Cache             CacheStats   `json:"cache"`
	Growth            GrowthTotals `json:"growth"`
	Compactions       int          `json:"compactions"`
	TTFTFirstSec      []float64    `json:"ttft_first_s,omitempty"`
	TTFTWarmMedianSec *float64     `json:"ttft_warm_median_s,omitempty"`
}
