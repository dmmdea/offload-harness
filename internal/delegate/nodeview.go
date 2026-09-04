// Package delegate is the DELEGATOR side of multi-node agent delegation
// (docs/specs/2026-08-16-multi-node-agent-delegation.md, §S3): it turns
// /fleet/health answers into NodeViews and decides PLACEMENT — which node
// runs a delegation contract — through the hard capability gate in gate.go.
// It performs no dispatch itself; the Phase T surfaces (MCP agent_delegate,
// the CLI verb) consume Place's decision and drive the fleet wire.
package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/netguard"
)

// NodeView is one node's placement-relevant state, built from /fleet/health
// (FetchNodeView) for remotes and from local knowledge for the local node.
type NodeView struct {
	NodeID         string
	AgentEnabled   bool
	AgentSeat      string
	AgentResident  bool
	AgentCtxTokens int
	QueueDepth     int
	// JobsRunning / JobsQueued split QueueDepth. MaxConcurrentJobs is the node's
	// execution limit and MaxQueueDepth its ADMISSION ceiling on QueueDepth —
	// the one and only thing that produces a `503 queue full`.
	//
	// For BOTH limits, 0 means "no usable number": the node publishes 0 for
	// unlimited, and a node too old to publish the field at all also decodes to
	// 0, and this decoder cannot tell those apart. So 0 is read as UNKNOWN
	// everywhere — never as a limit, never as unlimited. Same house rule
	// AgentCtxTokens already follows: unknown is not a capacity.
	//
	// gate.go DOES consult these (capacity-aware placement, 0.101.0): a node
	// whose published ceiling is already met loses to any node that is not
	// provably full, and a node with a provably free execution slot outranks
	// one whose workers are all busy. QueueDepth keeps its own meaning and its
	// own role as the ordering key — this is added on top, not in place of it.
	JobsRunning       int
	JobsQueued        int
	MaxConcurrentJobs int
	MaxQueueDepth     int
	// ServedModels is the node's advertised roster (health served_models).
	// nil/empty = UNKNOWN (a pre-0.113.0 node): never a refusal. When
	// published, the agent seat must be in it — a stronger check than the
	// cached residency flag, and the capability gate PAIR calls "the exact
	// requested model is present".
	ServedModels []string
	// GpuUtilPct is the busiest device's utilization; GpuUtilKnown is false
	// when the node did not publish it. Read by betterRemote as the LAST key —
	// a tie-breaker, never a primary signal (operator decision 2026-09-03).
	GpuUtilPct   int
	GpuUtilKnown bool
	Local        bool
}

// fetchNodeViewTimeout is the transport-level backstop for one health GET —
// a caller-supplied ctx deadline still wins when shorter. Health is a cached
// read on the node side (never blocks on llama-swap), so a node that cannot
// answer inside this is down, not busy.
const fetchNodeViewTimeout = 15 * time.Second

// maxHealthBody bounds the decoded health response. A real payload is a few
// KiB; the cap only exists so a misconfigured base pointing at something
// pathological cannot balloon the delegator.
const maxHealthBody = 1 << 20

// healthClient rides netguard.SafeTransport: the delegation lane may only
// ever reach loopback or the operator's tailnet (never-cloud, ADR 0001), and
// the dial-time gate holds even when a MagicDNS name's resolution drifts —
// the URL-shape check alone cannot promise that.
var healthClient = &http.Client{
	Transport: netguard.SafeTransport(nil),
	Timeout:   fetchNodeViewTimeout,
}

// healthWire is the LOOSE decode of GET /fleet/health — only the fields the
// placement gate consumes. Tolerant on purpose, twice over: unknown health
// fields (VRAM, footprints, future keys) are ignored so staggered node
// deploys never flag-day the delegator, and ABSENT agent fields decode to
// zero values, which the gate reads as ineligible — a pre-delegation node is
// automatically never placed on.
type healthWire struct {
	NodeID         string `json:"node_id"`
	QueueDepth     int    `json:"queue_depth"`
	AgentSeat      string `json:"agent_seat"`
	AgentCtxTokens int    `json:"agent_ctx_tokens"`
	AgentResident  bool   `json:"agent_seat_resident"`
	AgentEnabled   bool   `json:"agent_enabled"`
	// Additive (0.100.0). Absent on a pre-0.100.0 node, which decodes to 0 —
	// the same tolerance every other field here relies on.
	JobsRunning       int `json:"jobs_running"`
	JobsQueued        int `json:"jobs_queued"`
	MaxConcurrentJobs int `json:"max_concurrent_jobs"`
	MaxQueueDepth     int `json:"max_queue_depth"`
	// Additive (0.113.0). Absent on a pre-0.113.0 node, which decodes to the
	// zero value — nil/false, both read as UNKNOWN by the gate.
	ServedModels []string `json:"served_models"`
	GpuUtilPct   int      `json:"gpu_util_pct"`
	GpuUtilKnown bool     `json:"gpu_util_known"`
}

// FetchNodeView reads one node's /fleet/health into a NodeView (Local=false —
// a fetched view is by definition a remote node). token, when non-empty, is
// sent as a bearer credential; health is open today (capability advertisement
// is not sensitive, and the deployed media dispatcher must keep decoding it
// tokenless), so the parameter is RESERVED for a future health-auth posture
// rather than required now — callers thread the fleet token through so they
// need no signature change on that day.
func FetchNodeView(ctx context.Context, base, token string) (NodeView, error) {
	u := strings.TrimRight(strings.TrimSpace(base), "/") + "/fleet/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return NodeView{}, fmt.Errorf("delegate: health request for %q: %w", base, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := healthClient.Do(req)
	if err != nil {
		return NodeView{}, fmt.Errorf("delegate: health GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHealthBody))
	if err != nil {
		return NodeView{}, fmt.Errorf("delegate: reading health from %s: %w", u, err)
	}
	if resp.StatusCode != http.StatusOK {
		// The node's error taxonomy is a small JSON envelope — carry it: "503
		// vram snapshot stale" routes very differently from a connection error.
		return NodeView{}, fmt.Errorf("delegate: health GET %s: status %d: %s", u, resp.StatusCode, truncate(body, 256))
	}
	var w healthWire
	if err := json.Unmarshal(body, &w); err != nil {
		return NodeView{}, fmt.Errorf("delegate: health from %s is not JSON: %w", u, err)
	}
	return NodeView{
		NodeID:         w.NodeID,
		AgentEnabled:   w.AgentEnabled,
		AgentSeat:      w.AgentSeat,
		AgentResident:  w.AgentResident,
		AgentCtxTokens: w.AgentCtxTokens,
		QueueDepth:     w.QueueDepth,

		JobsRunning:       w.JobsRunning,
		JobsQueued:        w.JobsQueued,
		MaxConcurrentJobs: w.MaxConcurrentJobs,
		MaxQueueDepth:     w.MaxQueueDepth,
		ServedModels:      w.ServedModels,
		GpuUtilPct:        w.GpuUtilPct,
		GpuUtilKnown:      w.GpuUtilKnown,
		Local:             false,
	}, nil
}

// truncate bounds an error-path body excerpt.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
