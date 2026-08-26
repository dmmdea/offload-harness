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
	// JobsRunning / JobsQueued split QueueDepth, and MaxConcurrentJobs is the
	// node's execution limit (0 = unlimited, or "not advertised" by a node too
	// old to publish it — both decode to 0 here, so a consumer must treat 0 as
	// "no usable number" rather than as a limit). Read-only reporting: nothing
	// in gate.go consults them, so placement is unchanged. They exist because
	// a `queue deadline` failure sends an operator straight to "is that node
	// saturated?", and depth alone cannot answer it.
	JobsRunning       int
	JobsQueued        int
	MaxConcurrentJobs int
	Local             bool
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
