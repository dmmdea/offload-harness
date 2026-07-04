package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/local-offload/internal/sandbox"
)

// BuildConfig declares everything a drive mode needs to construct the agent loop
// identically. The caller supplies the local planner endpoint, the read scope, an
// in-process offload closure (record=false — e.g. pipeline.NewRecordlessOffload),
// and which capabilities are opt-in enabled. Build wires the tools + the single
// deny→ask→allow broker + the loop the SAME way for every mode (CLI, MCP front
// door, standalone), so the three modes cannot drift.
type BuildConfig struct {
	PlannerBase string        // local model base URL (no /v1); required
	Model       string        // planner model id (must support tool-calling); required
	Timeout     time.Duration // per planner-call timeout; default 180s
	MaxSteps    int           // hard step budget; default 12
	MaxTokens   int           // planner max tokens per call; default 1024

	ReadRoot string      // directory the agent may read (P0 scope); required
	Offload  OffloadFunc // in-process offload (record=false); nil => no offload tools

	// SystemPromptOverride, when set, replaces the capability-aware system prompt
	// (P6 flywheel replay evaluates a CANDIDATE planner prompt). Empty => the normal
	// SystemPrompt. The tool set is still capability-gated as usual, so a read-only
	// build stays side-effect-free regardless of the prompt.
	SystemPromptOverride string

	Unattended   bool   // true => every broker "ask" deny-and-queues (no human in the loop)
	AuditPath    string // append-only broker audit JSONL; must live OUTSIDE the worktree
	AskQueuePath string // P5b: reviewable queue of asks deferred on an unattended run (optional)

	AllowWrite  bool     // P2: write_file/delete_file in the worktree
	AllowFetch  bool     // P3: web_fetch behind the egress allowlist
	AllowShell  bool     // P4.6: run_shell in the OS cage (granted only if sandbox.Available)
	Worktree    string   // RW worktree for write/shell; default = ReadRoot
	EgressHosts []string // web_fetch allowlist (AllowFetch)

	Memory Memory // optional mem0 layer; nil => no memory
}

// BuildResult is the assembled loop plus what was actually granted. ShellGranted
// is false when --allow-shell was requested but the OS cage is unavailable here
// (fail-closed); Notes are human-readable capability lines the caller can log.
type BuildResult struct {
	Loop         *Loop
	Tools        []Tool
	Policy       *Policy
	Worktree     string // resolved RW worktree (empty if no write/shell)
	ShellGranted bool
	Notes        []string
}

// Build assembles the agent loop for any drive mode. It is the SINGLE place the
// tool set + broker + loop are constructed, so the CLI, the MCP front door, and
// the standalone runner stay at parity by construction. Operational failures
// (bad paths, audit-inside-worktree, worktree creation) are returned as errors,
// never os.Exit — the caller decides how to surface them.
func Build(cfg BuildConfig) (*BuildResult, error) {
	if cfg.PlannerBase == "" || cfg.Model == "" {
		return nil, fmt.Errorf("agent.Build: PlannerBase and Model are required")
	}
	if cfg.ReadRoot == "" {
		return nil, fmt.Errorf("agent.Build: ReadRoot is required")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 12
	}
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1024
	}

	absRoot, err := filepath.Abs(cfg.ReadRoot)
	if err != nil {
		return nil, fmt.Errorf("bad ReadRoot %q: %w", cfg.ReadRoot, err)
	}
	tools, err := ReadOnlyTools(absRoot, cfg.Offload)
	if err != nil {
		return nil, fmt.Errorf("building read tools: %w", err)
	}

	res := &BuildResult{}

	// The single broker governs write+fetch+shell. The egress allowlist is built
	// only when fetch is enabled (else the zero value is default-deny).
	var allow Allowlist
	if cfg.AllowFetch {
		a, aerr := NewAllowlist(cfg.EgressHosts)
		if aerr != nil {
			return nil, fmt.Errorf("bad egress host: %w", aerr)
		}
		allow = a
		if len(cfg.EgressHosts) == 0 {
			res.Notes = append(res.Notes, "egress allowlist EMPTY — web_fetch will refuse every URL")
		}
	}
	var audit *AuditLog
	if cfg.AuditPath != "" {
		audit = NewAuditLog(cfg.AuditPath)
	}
	pol := NewPolicyWithEgress(cfg.Unattended, audit, allow)
	if cfg.AskQueuePath != "" {
		pol.WithAskQueue(NewAuditLog(cfg.AskQueuePath))
	}
	res.Policy = pol

	// The RW worktree is shared by write_file/delete_file (P2) and run_shell (P4.6).
	var absWt string
	if cfg.AllowWrite || cfg.AllowShell {
		wt := cfg.Worktree
		if wt == "" {
			wt = absRoot
		}
		if absWt, err = filepath.Abs(wt); err != nil {
			return nil, fmt.Errorf("bad Worktree %q: %w", wt, err)
		}
		if err := os.MkdirAll(absWt, 0o755); err != nil {
			return nil, fmt.Errorf("creating worktree %q: %w", absWt, err)
		}
		// The audit trail AND the ask-queue must live OUTSIDE the worktree, or
		// write_file / run_shell could clobber the integrity records via the cage path.
		for _, p := range []struct{ name, path string }{{"audit", cfg.AuditPath}, {"ask-queue", cfg.AskQueuePath}} {
			if p.path == "" {
				continue
			}
			if apAbs, e := filepath.Abs(p.path); e == nil {
				if rel, e2 := filepath.Rel(absWt, apAbs); e2 == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return nil, fmt.Errorf("%s path %q is inside the worktree %q (the agent could clobber it); choose a path outside it", p.name, p.path, absWt)
				}
			}
		}
		res.Worktree = absWt
	}

	if cfg.AllowWrite {
		wtools, terr := WriteTools(absWt, pol)
		if terr != nil {
			return nil, fmt.Errorf("building write tools: %w", terr)
		}
		tools = append(tools, wtools...)
		res.Notes = append(res.Notes, fmt.Sprintf("write ON — worktree=%s (unattended: new files only; overwrite/delete refused)", absWt))
	}
	if cfg.AllowFetch {
		tools = append(tools, FetchTools(pol)...)
		res.Notes = append(res.Notes, fmt.Sprintf("egress ON — allowlist=%v (only allowlisted hosts; loopback/private/redirect-escape blocked)", cfg.EgressHosts))
	}
	if cfg.AllowShell {
		// Fail-closed: grant the shell ONLY if the OS cage can actually be enforced
		// here (Linux + Landlock + seccomp + user namespaces). Else refuse.
		if ok, why := sandbox.Available(); ok {
			pol.WithShell(true)
			scratch := filepath.Join(absWt, ".agent-scratch")
			tools = append(tools, ShellTools(pol, absWt, scratch)...)
			res.ShellGranted = true
			res.Notes = append(res.Notes, fmt.Sprintf("shell ON — OS sandbox (%s); worktree=%s (no network, FS-confined, syscall-limited)", why, absWt))
		} else {
			res.Notes = append(res.Notes, fmt.Sprintf("shell requested but OS sandbox unavailable (%s) — NOT granted (fail-closed)", why))
		}
	}

	client := NewLLMClient(cfg.PlannerBase, cfg.Model, "", timeout) // local planner, keyless
	// The system prompt advertises only what was actually granted — ShellGranted,
	// not the raw flag, so a cage-refused shell is never advertised to the model. A
	// SystemPromptOverride (P6 flywheel replay of a candidate prompt) replaces it.
	sys := SystemPrompt(cfg.AllowWrite, cfg.AllowFetch, res.ShellGranted)
	if cfg.SystemPromptOverride != "" {
		sys = cfg.SystemPromptOverride
	}
	loop := NewLoop(client, tools, maxSteps).WithSystem(sys).WithMaxTokens(maxTokens)
	if cfg.Memory != nil {
		loop = loop.WithMemory(cfg.Memory)
	}
	res.Loop = loop
	res.Tools = tools
	return res, nil
}
