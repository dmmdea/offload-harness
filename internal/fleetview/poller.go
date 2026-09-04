package fleetview

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/netguard"
)

const (
	probeTimeout = 5 * time.Second
	maxBody      = 1 << 20
	jobsPerNode  = 50
	maxErrors    = 200
)

// Poller periodically probes every configured base's /fleet/health and
// /fleet/jobs, folds the delegation-log corpus, and exposes the result
// through Snapshot(). All mutable state lives behind mu.
type Poller struct {
	cfg      config.Config
	bases    []string
	interval time.Duration
	history  int
	client   *http.Client

	mu          sync.RWMutex
	nodes       map[string]*Node // by base
	errors      []Error
	delegations []map[string]any // last 100 delegation-log rows, newest last
	// seenErr dedupes error emission: "probe|<base>" for probe failures
	// (cleared when the node answers again, so the NEXT failure streak
	// reports once more), "<node_id>|<job_id>" for job errors, and
	// "deleg|<job_id>" for delegation-log rows.
	seenErr map[string]bool
}

// NewPoller builds a Poller for bases (already-resolved node base URLs). It
// does not start probing — call Run.
func NewPoller(cfg config.Config, bases []string, interval time.Duration, history int) *Poller {
	p := &Poller{
		cfg: cfg, bases: bases, interval: interval, history: history,
		client:  &http.Client{Transport: netguard.SafeTransport(nil), Timeout: probeTimeout},
		nodes:   map[string]*Node{},
		seenErr: map[string]bool{},
	}
	for _, b := range bases {
		b = strings.TrimRight(b, "/")
		p.nodes[b] = &Node{Base: b}
	}
	return p
}

// Run probes immediately, then on every tick, until ctx is done.
func (p *Poller) Run(ctx context.Context) {
	p.tick(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick(ctx)
		}
	}
}

// tick probes every base in parallel (the mcpserver status probe's pattern:
// per-node budgets, never one shared deadline), then folds the delegation
// log corpus.
func (p *Poller) tick(ctx context.Context) {
	var wg sync.WaitGroup
	for _, base := range p.bases {
		base = strings.TrimRight(base, "/")
		wg.Add(1)
		go func(base string) {
			defer wg.Done()
			h, herr := p.getJSON(ctx, base+"/fleet/health")
			// jerr covers both a pre-0.113.0 node's permanent 404 and a
			// transient timeout/5xx on an otherwise-current node — fold
			// tells those apart from jobsOK, not from the jobs payload.
			j, jerr := p.getJSON(ctx, fmt.Sprintf("%s/fleet/jobs?limit=%d", base, jobsPerNode))
			p.fold(base, h, herr, j, jerr == nil)
		}(base)
	}
	wg.Wait()
	p.foldDelegationLog()

	p.mu.Lock()
	p.pruneSeenErrLocked()
	p.mu.Unlock()
}

// pruneSeenErrLocked bounds seenErr: a job key ("<nodeLabel>|<jobID>") is
// kept only while that job id is still present in that node's current Jobs
// listing, and a "deleg|<job_id>" key only while that job id is still inside
// the retained Delegations window (last maxDelegations rows). "probe|<base>"
// keys are left alone — fold already clears those the moment a node answers.
// With both windows bounded, seenErr is bounded by roughly
// (nodes*jobsPerNode + maxDelegations + len(bases)). Caller must hold p.mu.
func (p *Poller) pruneSeenErrLocked() {
	validJob := map[string]bool{}
	for _, n := range p.nodes {
		nodeLabel := n.NodeID
		if nodeLabel == "" {
			nodeLabel = n.Base
		}
		for _, job := range n.Jobs {
			validJob[nodeLabel+"|"+str(job, "id")] = true
		}
	}
	validDeleg := map[string]bool{}
	for _, row := range p.delegations {
		validDeleg["deleg|"+str(row, "job_id")] = true
	}
	for k := range p.seenErr {
		switch {
		case strings.HasPrefix(k, "probe|"):
			// left alone
		case strings.HasPrefix(k, "deleg|"):
			if !validDeleg[k] {
				delete(p.seenErr, k)
			}
		default:
			if !validJob[k] {
				delete(p.seenErr, k)
			}
		}
	}
}

func (p *Poller) getJSON(ctx context.Context, u string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if p.cfg.FleetAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.FleetAuthToken)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %.200s", resp.StatusCode, body)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	return m, nil
}

// fold applies one tick's probe result for base into p.nodes and appends any
// new errors. Locks p.mu for the duration. jobsOK distinguishes "the jobs
// fetch failed this tick" from "it succeeded and returned an empty list": on
// failure n.Jobs is left exactly as the last successful fetch set it, so a
// transient jobs-endpoint timeout/5xx never wipes a still-ongoing job error
// out of the node's list — and pruneSeenErrLocked, which derives validity
// from n.Jobs, then leaves that job's dedupe key alone too.
func (p *Poller) fold(base string, h map[string]any, herr error, j map[string]any, jobsOK bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	n, ok := p.nodes[base]
	if !ok {
		n = &Node{Base: base}
		p.nodes[base] = n
	}

	probeKey := "probe|" + base
	if herr != nil {
		n.Reachable = false
		n.ProbeError = herr.Error()
		if !p.seenErr[probeKey] {
			p.seenErr[probeKey] = true
			nodeLabel := n.NodeID
			if nodeLabel == "" {
				nodeLabel = base
			}
			p.appendErrorLocked(Error{
				At: time.Now().Unix(), Severity: "error", Node: nodeLabel,
				Source: "probe", Message: herr.Error(),
			})
		}
		return
	}
	// The node answered: clear the probe dedupe key so the NEXT failure
	// streak reports once more.
	delete(p.seenErr, probeKey)

	n.Reachable = true
	n.ProbeError = ""
	n.LastSeen = time.Now().Unix()

	n.NodeID = str(h, "node_id")
	n.Version = str(h, "harness_version")
	n.GpuVendor = str(h, "gpu_vendor")
	n.GpuArch = str(h, "gpu_arch")
	n.VramTotal = num(h, "vram_total_gb")
	n.VramFree = num(h, "vram_free_gb")
	n.Devices = maps(h, "gpu_devices")
	n.GpuUtil = int(num(h, "gpu_util_pct"))
	n.GpuUtilKnown = boolv(h, "gpu_util_known")
	n.HostCPU = int(num(h, "host_cpu_pct"))
	n.RamUsed = num(h, "host_ram_used_gb")
	n.RamTotal = num(h, "host_ram_total_gb")
	n.AgentEnabled = boolv(h, "agent_enabled")
	n.AgentResident = boolv(h, "agent_seat_resident")
	n.AgentSeat = str(h, "agent_seat")
	n.AgentCtx = int(num(h, "agent_ctx_tokens"))
	n.ServedModels = strs(h, "served_models")
	n.QueueDepth = int(num(h, "queue_depth"))
	n.JobsRunning = int(num(h, "jobs_running"))
	n.JobsQueued = int(num(h, "jobs_queued"))
	n.MaxConcurrent = int(num(h, "max_concurrent_jobs"))
	n.MaxQueue = int(num(h, "max_queue_depth"))

	n.History = append(n.History, Point{
		At: time.Now().Unix(), GpuUtil: n.GpuUtil, CPU: n.HostCPU,
		VramFree: n.VramFree, RamUsed: n.RamUsed,
	})
	if len(n.History) > p.history {
		n.History = n.History[len(n.History)-p.history:]
	}

	if !jobsOK {
		// Fetch failed this tick (timeout, 5xx, a permanently-404 pre-
		// 0.113.0 node, ...): keep whatever n.Jobs already held rather than
		// overwriting it with an empty decode of a failed response. Do NOT
		// touch seenErr here — pruneSeenErrLocked recomputes validity from
		// n.Jobs, which is now unchanged from last tick, so it naturally
		// keeps this node's existing job keys instead of pruning them as
		// "no longer present".
		return
	}

	jobs := maps(j, "jobs")
	n.Jobs = jobs
	nodeLabel := n.NodeID
	if nodeLabel == "" {
		nodeLabel = base
	}
	for _, job := range jobs {
		if str(job, "state") != "error" {
			continue
		}
		jobID := str(job, "id")
		key := nodeLabel + "|" + jobID
		if p.seenErr[key] {
			continue
		}
		p.seenErr[key] = true
		p.appendErrorLocked(Error{
			At: time.Now().Unix(), Severity: "error", Node: nodeLabel,
			Source: "job", Message: str(job, "task") + ": " + str(job, "error"),
		})
	}
}

// appendErrorLocked appends to p.errors and trims to maxErrors, newest last.
// Caller must hold p.mu.
func (p *Poller) appendErrorLocked(e Error) {
	p.errors = append(p.errors, e)
	if len(p.errors) > maxErrors {
		p.errors = p.errors[len(p.errors)-maxErrors:]
	}
}

// Snapshot returns a deep copy of the current state: Nodes in bases order.
func (p *Poller) Snapshot() Overview {
	p.mu.RLock()
	defer p.mu.RUnlock()

	ov := Overview{
		At:                time.Now().Unix(),
		DelegationEnabled: p.cfg.AgentDelegationEnabled,
	}
	for _, base := range p.bases {
		base = strings.TrimRight(base, "/")
		n, ok := p.nodes[base]
		if !ok {
			continue
		}
		ov.Nodes = append(ov.Nodes, copyNode(n))
	}
	if len(p.errors) > 0 {
		ov.Errors = make([]Error, len(p.errors))
		copy(ov.Errors, p.errors)
	}
	if len(p.delegations) > 0 {
		ov.Delegations = cloneMaps(p.delegations)
	}
	return ov
}

func copyNode(n *Node) Node {
	cp := *n
	if n.Devices != nil {
		cp.Devices = cloneMaps(n.Devices)
	}
	if n.ServedModels != nil {
		cp.ServedModels = append([]string(nil), n.ServedModels...)
	}
	if n.History != nil {
		cp.History = append([]Point(nil), n.History...)
	}
	if n.Jobs != nil {
		cp.Jobs = cloneMaps(n.Jobs)
	}
	return cp
}

// cloneMaps deep-copies a []map[string]any so a caller mutating the returned
// Snapshot can never race with the poller's own goroutine mutating the same
// maps on the next tick. gpu_devices/jobs/delegation rows are JSON-decoded
// (map[string]any, []any, or scalars all the way down), so a generic
// recursive clone handles whatever shape shows up — even though the health
// payload's gpu_devices rows are flat in practice.
func cloneMaps(in []map[string]any) []map[string]any {
	if in == nil {
		return nil
	}
	out := make([]map[string]any, len(in))
	for i, m := range in {
		out[i] = cloneAnyMap(m)
	}
	return out
}

func cloneAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneAnyValue(v)
	}
	return out
}

func cloneAnyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return cloneAnyMap(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = cloneAnyValue(e)
		}
		return out
	default:
		// Scalars (string, float64, bool, nil, json.Number) are copied by
		// value already — nothing further to clone.
		return v
	}
}

// ---- loose-decode helpers for map[string]any health/jobs payloads ----

func num(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return 0
}

func str(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func boolv(m map[string]any, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}

func strs(m map[string]any, key string) []string {
	v, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(v))
	for _, e := range v {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func maps(m map[string]any, key string) []map[string]any {
	v, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(v))
	for _, e := range v {
		if mm, ok := e.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}
