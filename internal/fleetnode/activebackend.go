package fleetnode

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/backendchain"
	"github.com/dmmdea/offload-harness/internal/config"
)

// The per-lane and per-seat active_backend report rides /fleet/health as two additive keys,
// lane_backends and seat_backends (internal/backendchain.Entry: active_backend, backend_chain,
// backend_probe, backend_reason). They are NOT the existing `backends` key: that one is the hardware
// tag list of the install (ADR 0054) and is untouched.
//
// The report costs llama-swap reads (roster, running list, and a loaded seat's own health endpoint), so
// it follows the rule every other health read of llama-swap follows: the handler serves a cached answer
// and never waits. The first request after start-up answers without the keys (unknown, never "none"),
// kicks ONE background read, and the keys appear once it lands. A cached answer older than
// activeBackendTTL kicks the next read behind itself.
//
// The refresh is the cheap probe only. It sends no completion and never touches /upstream, because a
// completion loads an idle seat and can evict a vLLM seat; the 1-token completion probe exists only
// behind `doctor --probe-completion`, which a person runs.

// activeBackendTTL is how long one collection backs health: the same window as the residency answer.
const activeBackendTTL = agentResidencyTTL

// activeBackendBudget bounds one whole collection: the roster read, the running list, and each loaded
// seat's health GET are 3 s each at worst, and a node has a handful of lanes and seats.
const activeBackendBudget = 20 * time.Second

// backendCache is the last collection, with the single-flight latch that keeps one read in the air.
type backendCache struct {
	mu       sync.Mutex
	lanes    []backendchain.Entry
	seats    []backendchain.Entry
	at       time.Time
	inflight bool
}

// swapCollectBackends is the production collection: the cheap probe, over the client this package
// already builds for the node's own llama-swap (netguard's transport).
func swapCollectBackends(ctx context.Context, cfg config.Config) backendchain.Report {
	return backendchain.Collect(ctx, cfg, backendchain.Options{HTTP: seatRunningClient})
}

// activeBackends returns the cached report, kicking a background refresh when it is older than the TTL
// (or was never taken). It never waits. A node with no llama-swap endpoint publishes nothing.
func (s *Server) activeBackends() (lanes, seats []backendchain.Entry) {
	if strings.TrimSpace(s.opts.Cfg.Endpoint) == "" || s.collectBackends == nil {
		return nil, nil
	}
	c := &s.backends
	c.mu.Lock()
	lanes, seats = c.lanes, c.seats
	kick := (c.at.IsZero() || time.Since(c.at) > activeBackendTTL) && !c.inflight
	if kick {
		c.inflight = true
	}
	c.mu.Unlock()
	if kick {
		go s.refreshActiveBackends()
	}
	return lanes, seats
}

// refreshActiveBackends runs one collection and publishes it, releasing the latch the caller took. The
// collector reports its own failures as rows (an unreachable llama-swap is a "none" row with the reason),
// and a panic in it is not recovered: it is a bug, and it ends the process loudly like any other
// goroutine's would, instead of leaving a node that serves a stale report.
func (s *Server) refreshActiveBackends() {
	ctx, cancel := context.WithTimeout(context.Background(), activeBackendBudget)
	defer cancel()
	rep := s.collectBackends(ctx, s.opts.Cfg)
	c := &s.backends
	c.mu.Lock()
	c.lanes, c.seats = rep.Lanes, rep.Seats
	c.at = time.Now()
	c.inflight = false
	c.mu.Unlock()
}
