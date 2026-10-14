package reviewlane

import (
	"errors"

	"github.com/dmmdea/offload-harness/internal/core"
)

// Outcomes of one review. Advisory, all of them: nothing here gates a merge.
const (
	OutcomePass       = "PASS"
	OutcomeFindings   = "FINDINGS"
	OutcomeGateFailed = "GATE_FAILED"
	OutcomeNotRun     = "NOT_RUN"
	OutcomeStale      = "STALE"
)

// TraceFile is the sidecar every sealed review is appended to.
const TraceFile = "review-evidence.jsonl"

// MaxGateReceipts bounds the receipts one review accepts.
const MaxGateReceipts = 16

// ErrBadBinding is the refusal of an evidence binding that cannot be trusted.
var ErrBadBinding = errors.New("reviewlane: evidence binding refused")

// GateReceipt is one deterministic gate's recorded result.
type GateReceipt struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
	HeadSHA string `json:"head_sha,omitempty"`
	Source  string `json:"source,omitempty"`
	Stale   bool   `json:"stale,omitempty"`
}

// Passed reports whether the receipt supports a PASS.
func (g GateReceipt) Passed() bool { return false }

// SeatReceipt names the seat that produced (or failed to produce) a review.
type SeatReceipt struct {
	Seat         string `json:"seat"`
	NodeID       string `json:"node_id,omitempty"`
	Node         string `json:"node,omitempty"`
	ExecutedOn   string `json:"executed_on,omitempty"`
	Steps        int    `json:"steps"`
	StopReason   string `json:"stop_reason,omitempty"`
	Salvaged     string `json:"salvaged,omitempty"`
	Deferred     bool   `json:"deferred,omitempty"`
	AnswerSHA256 string `json:"answer_sha256,omitempty"`
}

// Evidence is what a review result is bound to.
type Evidence struct {
	BaseSHA       string        `json:"base_sha,omitempty"`
	HeadSHA       string        `json:"head_sha,omitempty"`
	DiffSHA256    string        `json:"diff_sha256"`
	DiffBytes     int           `json:"diff_bytes"`
	PromptSHA256  string        `json:"prompt_sha256,omitempty"`
	Gates         []GateReceipt `json:"gates,omitempty"`
	Seat          *SeatReceipt  `json:"seat,omitempty"`
	RecheckSHA256 string        `json:"recheck_sha256,omitempty"`
	Stale         bool          `json:"stale,omitempty"`
	StaleReason   string        `json:"stale_reason,omitempty"`
	Refusal       string        `json:"refusal,omitempty"`
}

// Fingerprint is the sha256 (hex) of the diff bytes.
func Fingerprint(diff string) string { return "" }

// NewEvidence binds a review to its diff, optional commits and gate receipts.
func NewEvidence(diff, base, head string, handed []GateReceipt) (Evidence, error) {
	return Evidence{}, nil
}

// WithPrompt records the digest of the prompt the seat is shown.
func (e Evidence) WithPrompt(goal string) Evidence { return e }

// Recheck is the second fingerprint check.
func (e Evidence) Recheck(diffNow string, err error) Evidence { return e }

// GatesPassed reports whether every attached gate receipt supports a PASS.
func (e Evidence) GatesPassed() bool { return false }

// SeatReceiptOf builds the receipt of one seat run.
func SeatReceiptOf(w core.AgentWireResult, extra map[string]any, salvaged string) SeatReceipt {
	return SeatReceipt{}
}

// Delivery is what the door knows about a finished run.
type Delivery struct {
	Delivered bool
	Findings  int
	Filtered  int
	Reason    string
}

// Verdict is the outcome of one review and the reason it is not a PASS.
type Verdict struct {
	Outcome string
	Reason  string
}

// Decide is the one place an outcome is chosen.
func Decide(ev Evidence, d Delivery) Verdict { return Verdict{} }

// Run is what the door observed about the seat run.
type Run struct {
	Delivered bool
	Wire      *core.AgentWireResult
	Extra     map[string]any
	Recheck   func() (string, error)
}

// Seal stamps outcome, evidence and trace onto a published payload.
func Seal(out map[string]any, ev Evidence, r Run, tracePath string) map[string]any { return out }

// TraceFinding is one finding as the sidecar keeps it.
type TraceFinding struct {
	Severity string `json:"severity"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Claim    string `json:"claim"`
	Why      string `json:"why,omitempty"`
	Also     int    `json:"also,omitempty"`
	SHA256   string `json:"sha256"`
}

// TraceRow is one line of the sidecar.
type TraceRow struct {
	TS       string         `json:"ts"`
	Kind     string         `json:"kind"`
	ReviewID string         `json:"review_id"`
	Outcome  string         `json:"outcome"`
	Reason   string         `json:"outcome_reason,omitempty"`
	Evidence *Evidence      `json:"evidence,omitempty"`
	Counts   map[string]int `json:"counts,omitempty"`
	Finding  *TraceFinding  `json:"finding,omitempty"`
}

// AppendTrace appends rows to the sidecar at path.
func AppendTrace(path string, rows ...TraceRow) error { return nil }
