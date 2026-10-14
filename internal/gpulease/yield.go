package gpulease

// yield.go — an overdue lease's HOLDER is asked to yield; it is never taken from (plan P11, ADR 0085).
//
// SEAM. This file is the behaviour-free shape of the feature: the types, the names and the
// signatures the tests are written against, every function returning its zero value. The behaviour
// follows in the commit after the tests.

import "io"

// YieldFileEnv names the variable the wrapper form hands its command: the path of the lease's
// yield sidecar, which exists only once the holder has been asked.
const YieldFileEnv = "GPU_LEASE_YIELD_FILE"

// yieldPrefix names the sidecar yield.<epoch>, a sidecar like orphan.<epoch>, removed with the lease.
const yieldPrefix = "yield."

// The forms of holder that run the request, as Meta.Yields records them.
const (
	YieldsWrapper  = "wrapper"
	YieldsDetached = "detached"
)

// OnOverdue is the policy for a lease that outlived its window while someone waits for its cards
// (config gpu_on_overdue).
type OnOverdue string

const (
	// OnOverdueUnset: the key is not set. The harness behaves as it did before the request existed:
	// nobody is asked.
	OnOverdueUnset OnOverdue = ""
	// OnOverdueHold: the holder is never asked; the lease is held until its holder ends it.
	OnOverdueHold OnOverdue = "hold"
	// OnOverdueRequest: the holder is asked to yield and runs the job's own recorded on_yield.
	OnOverdueRequest OnOverdue = "request"
)

// ParseOnOverdue reads a configured policy.
func ParseOnOverdue(raw string) (OnOverdue, error) { return OnOverdueUnset, nil }

// SetDefaultOnOverdue installs the configured policy for every holder tick of this process.
func SetDefaultOnOverdue(raw string) {}

// InstalledOnOverdue is the policy in force.
func InstalledOnOverdue() OnOverdue { return OnOverdueUnset }

// OnOverdueNote is the sentence a holder says once when the installed value was not recognised.
func OnOverdueNote() string { return "" }

// Yield actions, the words the sidecar's action field carries.
const (
	YieldActionRunning = "on_yield running"
	YieldActionRan     = "ran on_yield"
	YieldActionFailed  = "on_yield failed"
	YieldActionNone    = "no on_yield recorded"
)

// YieldWaiter is one waiter that was queued for the lease's cards when its holder was asked.
type YieldWaiter struct {
	PID    int    `json:"pid"`
	Class  Class  `json:"class"`
	Reason string `json:"reason,omitempty"`
}

// YieldRequest is the content of the yield.<epoch> sidecar.
type YieldRequest struct {
	Epoch   uint64        `json:"epoch"`
	AskedMs int64         `json:"asked_ms"`
	ByPID   int           `json:"by_pid"`
	Basis   string        `json:"basis"`
	Waiters []YieldWaiter `json:"waiters,omitempty"`
	Policy  string        `json:"policy"`
	GraceMs int64         `json:"grace_ms,omitempty"`
	ActedMs int64         `json:"acted_ms,omitempty"`
	Action  string        `json:"action,omitempty"`
	Exit    int           `json:"exit,omitempty"`
}

// YieldFilePath is where the sidecar of one epoch is (or will be) in a lease directory.
func YieldFilePath(leaseDir string, epoch uint64) string { return "" }

// ReadYield reads the sidecar of one epoch; false when the holder was never asked.
func ReadYield(leaseDir string, epoch uint64) (YieldRequest, bool) { return YieldRequest{}, false }

// YieldOf is ReadYield on a Manager.
func (m *Manager) YieldOf(epoch uint64) (YieldRequest, bool) { return YieldRequest{}, false }

// yieldBeforeLockHook is a test seam, called by an ask that has passed every check made outside the epoch
// lock and is about to take it. nil in production.
var yieldBeforeLockHook func()

// YieldAsker decides, on a holder's tick, whether the holder of one lease is to be asked to yield.
type YieldAsker struct{}

// NewYieldAsker binds the decision to one lease of this directory, held by this process.
func (m *Manager) NewYieldAsker(epoch uint64) *YieldAsker { return &YieldAsker{} }

// Tick makes the decision. It reports the request it wrote and true on the one tick that asked.
func (a *YieldAsker) Tick() (YieldRequest, bool, error) { return YieldRequest{}, false, nil }

// RecordYieldAction updates the sidecar of an epoch that was asked with what the holder did about it.
func (m *Manager) RecordYieldAction(epoch uint64, action string, exit int) error { return nil }

// YieldRunner runs the job's own on_yield line to its end and reports the exit status.
type YieldRunner func(line string, env []string) (exit int, err error)

// YieldTickerOptions configure a holder's yield ticker.
type YieldTickerOptions struct {
	// OnYield is the holder's OWN in-memory --on-yield value; the record is never re-read for it.
	OnYield string
	Run     YieldRunner
	Out     io.Writer
	// Label leads the holder's stderr lines ("gpu reserve", "gpu hold").
	Label string
}

// YieldTicker is the holder side of the request: ask when due, then run the job's own on_yield once.
type YieldTicker struct{}

// NewYieldTicker binds a yield ticker to a lease this process holds.
func (l *Lease) NewYieldTicker(o YieldTickerOptions) *YieldTicker { return &YieldTicker{} }

// Tick is called on every holder tick, after the term check.
func (t *YieldTicker) Tick() {}

// Wait blocks until the on_yield this ticker started has ended.
func (t *YieldTicker) Wait() {}

// YieldView is what a read-only surface says about the request on one lease.
type YieldView struct {
	// Capable: the holder's record says it runs the request.
	Capable bool
	// File is the sidecar path; "" for a holder that cannot be asked.
	File string
	// Asked: the sidecar exists.
	Asked      bool
	Req        YieldRequest
	Unanswered bool
	// Sentence is the wording for a waiter or a status line; "" when there is nothing to say.
	Sentence string
}

// YieldViewOf reads the request state of one lease without writing anything.
func (m *Manager) YieldViewOf(info Info) YieldView { return YieldView{} }
