// Package fleetview is the DELEGATOR-side operator overview: it polls every
// configured node's /fleet/health and /fleet/jobs, keeps a bounded per-node
// history for sparklines, folds in the delegation-log corpus, and exposes a
// single Overview snapshot for Task 6's embedded page to serve as JSON.
//
// This package intentionally does NOT reuse delegate.FetchNodeView — that
// decoder discards the graph fields (gpu_devices, host_cpu_pct, ...) this
// view needs, and pulling in internal/delegate would drag the whole
// delegator (placement gate, dispatch, ledger) into a read-only viewer.
package fleetview

// Point is one bounded history sample for a node's sparklines.
type Point struct {
	At       int64   `json:"at"`
	GpuUtil  int     `json:"gpu_util"`
	CPU      int     `json:"cpu"`
	VramFree float64 `json:"vram_free_gb"`
	RamUsed  float64 `json:"ram_used_gb"`
}

// Node is one polled node's current state plus its bounded history and
// recent jobs. JSON tags are exact — the embedded overview page (Task 6)
// reads them verbatim.
type Node struct {
	Base       string `json:"base"`
	NodeID     string `json:"node_id"`
	Reachable  bool   `json:"reachable"`
	ProbeError string `json:"probe_error,omitempty"`
	Version    string `json:"harness_version,omitempty"`
	GpuVendor  string `json:"gpu_vendor,omitempty"`
	GpuArch    string `json:"gpu_arch,omitempty"`

	VramTotal float64          `json:"vram_total_gb"`
	VramFree  float64          `json:"vram_free_gb"`
	Devices   []map[string]any `json:"gpu_devices,omitempty"`

	GpuUtil      int     `json:"gpu_util_pct"`
	GpuUtilKnown bool    `json:"gpu_util_known"`
	HostCPU      int     `json:"host_cpu_pct"`
	RamUsed      float64 `json:"host_ram_used_gb"`
	RamTotal     float64 `json:"host_ram_total_gb"`

	AgentEnabled  bool     `json:"agent_enabled"`
	AgentResident bool     `json:"agent_seat_resident"`
	AgentSeat     string   `json:"agent_seat,omitempty"`
	AgentCtx      int      `json:"agent_ctx_tokens"`
	ServedModels  []string `json:"served_models,omitempty"`

	QueueDepth    int `json:"queue_depth"`
	JobsRunning   int `json:"jobs_running"`
	JobsQueued    int `json:"jobs_queued"`
	MaxConcurrent int `json:"max_concurrent_jobs"`
	MaxQueue      int `json:"max_queue_depth"`

	History []Point          `json:"history"`
	Jobs    []map[string]any `json:"jobs"`

	LastSeen int64 `json:"last_seen"`
}

// Error is one operator-facing event: a probe failure, a job that finished
// in state "error", or a deferred/failed delegation-log row.
type Error struct {
	At       int64  `json:"at"`
	Severity string `json:"severity"`
	Node     string `json:"node"`
	Source   string `json:"source"`
	Message  string `json:"message"`
}

// Overview is the full snapshot Task 6 serves as JSON.
//
// Delegations carries only errors.go's delegationRowFields — operational
// telemetry (where a contract landed, whether it passed, how long it took),
// never contract CONTENT. This whole endpoint is unauthenticated, so that
// exclusion is load-bearing: see delegationRowFields's doc comment.
type Overview struct {
	At                int64            `json:"at"`
	DelegationEnabled bool             `json:"delegation_enabled"`
	Nodes             []Node           `json:"nodes"`
	Errors            []Error          `json:"errors"`
	Delegations       []map[string]any `json:"delegations"`
}
