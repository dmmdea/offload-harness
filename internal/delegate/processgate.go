// processgate.go is the delegator's PROCESS-WIDE overload policy (ADR 0063,
// register C-70 / PR-11): the two things every bound in run.go was missing
// because each of them lived in one Run and a delegating session is not one Run.
//
// On 2026-09-29 one MCP server process issued 648 of the day's 836 delegated
// attempts, with up to 44 open at once, into about 13 execution slots. Every
// bound it met was per Run - runConcurrency (4), the deal's headroom count, the
// re-placement bound - so a dozen concurrent Runs each stayed inside their own
// and together buried the fleet. The same pages were also issued again and again
// (301 distinct pages took 713 jobs; the worst page took 24 attempts) because
// nothing remembered that the last three issues of a page had failed.
//
//   - inflightGate counts, per dial base, the jobs THIS PROCESS has dispatched and
//     not finished with (a terminal answer, or the delegator giving up on it), across
//     every concurrent RunWith. A dispatch that would take a node past its published
//     admission ceiling is not sent: the subtask goes to the capacity wait, which
//     places it on the first node that frees. It never refuses work - a full node is
//     a place in line - and it never replaces the node's own admission (foreign
//     tenants' load is the node's health to report, read fresh by every wait tick).
//   - pageRetryCap remembers, per page, how many consecutive issues failed, and
//     backs a page off after the original issue and two re-issues so a page no seat
//     can digest stops consuming the fleet. A success forgets it, and so does time.

package delegate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// inflightGate is the process-wide count of open dispatches per dial base.
type inflightGate struct {
	mu   sync.Mutex
	open map[string]int
}

// processGate is THE gate: one per process, shared by every RunWith.
var processGate = &inflightGate{open: map[string]int{}}

// admissionCeiling is the most jobs a node will hold at once - its published
// max_queue_depth, which counts running AND queued (the number a `503 queue full`
// is decided on). 0 = the node published none: unknown is never a limit.
func admissionCeiling(v NodeView) int { return v.MaxQueueDepth }

// tryAcquire takes one slot on base unless the process already holds `limit` of
// them open (limit <= 0 = no limit, the slot is only counted). release gives the
// slot back and is safe to call more than once.
func (g *inflightGate) tryAcquire(base string, limit int) (release func(), ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if limit > 0 && g.open[base] >= limit {
		return nil, false
	}
	g.open[base]++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.open[base] > 0 {
				g.open[base]--
			}
			if g.open[base] == 0 {
				delete(g.open, base)
			}
		})
	}, true
}

// available reports whether a dispatch to base would be admitted right now.
func (g *inflightGate) available(base string, limit int) bool {
	if limit <= 0 {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open[base] < limit
}

// load is how many dispatches this process holds open on base.
func (g *inflightGate) load(base string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open[base]
}

// gateFullReason is the sentence a subtask carries into the capacity wait when
// the process gate turned its dispatch away.
func gateFullReason(base string, v NodeView) string {
	return fmt.Sprintf("process gate: this process already holds %d dispatch(es) open on %s, at its admission ceiling (max_queue_depth %d) — waiting in line for one to finish",
		processGate.load(base), laneID(v), admissionCeiling(v))
}

// ---- per-page retry cap ---------------------------------------------------

// pageMaxIssues is how many consecutive FAILED issues of one page the delegator
// runs before backing it off: the original and two re-issues. pageBackoff is how
// long a backed-off page stays off; a var so tests can move it.
const pageMaxIssues = 3

var pageBackoff = 15 * time.Minute

type pageRecord struct {
	failures int
	last     time.Time
}

// pageRetryCap is the process-wide memory of pages that keep failing.
type pageRetryCap struct {
	mu    sync.Mutex
	pages map[string]*pageRecord
	now   func() time.Time
}

// pageRetries is THE cap: one per process, shared by every RunWith.
var pageRetries = &pageRetryCap{pages: map[string]*pageRecord{}, now: time.Now}

// pageKeyFor is the identity of the page a research contract digests: the
// content hash of its context documents. The document NAME is no key - it is
// "<index>-<host>.txt", so the same page fetched as source 3 of one call and
// source 1 of the next has two names - and neither is the goal: a page that
// keeps failing keeps failing whatever it is asked. ok=false for a contract that
// is not a research digest (nothing else is capped: an agent_delegate contract
// re-issued on purpose is the caller's business).
func pageKeyFor(c core.AgentContract) (string, bool) {
	if !researchDoors[c.Door] || len(c.Context) == 0 {
		return "", false
	}
	h := sha256.New()
	for _, d := range c.Context {
		_, _ = h.Write([]byte(d.Text))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// admit reports whether a page may be issued now. A page whose last
// pageMaxIssues issues all failed is backed off until pageBackoff has passed
// since the last failure; why says how many failed and when it may run again.
func (c *pageRetryCap) admit(key string) (ok bool, why string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec := c.pages[key]
	if rec == nil {
		return true, ""
	}
	now := c.now()
	if now.Sub(rec.last) >= pageBackoff {
		delete(c.pages, key)
		return true, ""
	}
	if rec.failures < pageMaxIssues {
		return true, ""
	}
	return false, fmt.Sprintf("page retry cap: this page was issued %d times in a row and none of them produced a verified digest — issuing it again would repeat the failure and spend the fleet on it; it may be issued again %s (or narrow the ask, or split the page)",
		rec.failures, pageBackoffUntil(rec.last, now))
}

// pageBackoffUntil renders when a backed-off page opens up again.
func pageBackoffUntil(last, now time.Time) string {
	left := pageBackoff - now.Sub(last)
	if left < time.Second {
		left = time.Second
	}
	return "in " + left.Round(time.Second).String()
}

// record files one finished issue of a page: a failure counts toward the cap, a
// success forgets the page. Stale records are pruned as the map grows, so a
// long-lived server never accumulates pages it will not see again.
func (c *pageRetryCap) record(key string, failed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if !failed {
		delete(c.pages, key)
		return
	}
	rec := c.pages[key]
	if rec == nil || now.Sub(rec.last) >= pageBackoff {
		rec = &pageRecord{}
		c.pages[key] = rec
	}
	rec.failures++
	rec.last = now
	if len(c.pages) > 512 {
		for k, r := range c.pages {
			if now.Sub(r.last) >= pageBackoff {
				delete(c.pages, k)
			}
		}
	}
}

// pageIssueFailed reports whether a finished issue counts against its page: a
// seat RAN it and it did not produce a verified digest. Nothing that never ran
// counts - a capacity defer, a shed, "placement refused", a queue deadline, a
// caller's cancel are the fleet's busy day, not the page's fault - and neither
// does a contract-class defer.
func pageIssueFailed(pr PlacedResult) bool {
	switch {
	case pr.Unplaced || pr.shed:
		return false
	case pr.Err != "":
		for _, p := range []string{replacementExhaustedPrefix, "queue deadline", "canceled"} {
			if strings.HasPrefix(pr.Err, p) {
				return false
			}
		}
		return true
	case pr.Result.Deferred:
		return pr.Result.DeferClass != core.DeferClassCapacity && pr.Result.DeferClass != core.DeferClassContract
	}
	return len(pr.AcceptanceFailures) > 0
}

// runCapped is runOne behind the per-page retry cap: a research page that has
// failed pageMaxIssues times in a row is not issued again until it has cooled;
// every other contract runs exactly as before.
func (r *runner) runCapped(ctx context.Context, i int, contract core.AgentContract) PlacedResult {
	key, capped := pageKeyFor(contract)
	if !capped {
		return r.runOne(ctx, i, contract)
	}
	start := time.Now()
	if ok, why := pageRetries.admit(key); !ok {
		local := r.localView()
		pr := PlacedResult{
			Node: local.NodeID, Seat: local.AgentSeat, Unplaced: true, PlacementReason: why,
			Result: core.AgentWireResult{
				SchemaVersion: core.AgentWireSchemaVersion, NodeID: local.NodeID, Seat: local.AgentSeat,
				Deferred: true, DeferClass: core.DeferClassContract, Reason: why,
			},
		}
		return r.settle(contract, pr, newPlacements(), start)
	}
	pr := r.runOne(ctx, i, contract)
	pageRetries.record(key, pageIssueFailed(pr))
	return pr
}
