package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// smokeRow is one node's outcome from a fleet-smoke run: where the contract
// landed, what it cost, and the verdict a human reads at a glance.
type smokeRow struct {
	Base      string `json:"base"`
	Node      string `json:"node,omitempty"`
	Seat      string `json:"seat,omitempty"`
	Placement string `json:"placement,omitempty"`
	JobID     string `json:"job_id,omitempty"`
	WallMs    int64  `json:"wall_ms,omitempty"`
	Verdict   string `json:"verdict"` // PASS | FAIL | DEFER
	Detail    string `json:"detail,omitempty"`
}

// smokeContract is the harness's "Test traffic" button. "Cheap" is the 60 s
// wall bound (TimeoutSec), not a hand-picked step cap: MaxSteps is left at 0
// so PrepareContract fills in the harness default (core.AgentMaxStepsDefault)
// — a hard-coded small budget strangled legitimate smokes (the Lenovo seat
// needed exactly 3 steps to answer; a smaller cap starves any seat that plans
// or tool-calls before replying) and the real cost control is the timeout,
// not an artificially tight step count. The acceptance is anchored on a
// token that lives ONLY in the context doc, so a seat that echoes the goal
// cannot pass (the delegation skill's parrot rule).
func smokeContract(nodeHint string) delegate.SubtaskSpec {
	token := "PONG-" + nodeHint
	return delegate.SubtaskSpec{AgentContract: core.AgentContract{
		Goal:         "Read the provided document and reply with the exact token it contains in the `reply` field. Nothing else.",
		Context:      []core.ContextDoc{{Name: "smoke.txt", Text: "The token is: " + token}},
		OutputSchema: json.RawMessage(`{"properties":{"reply":{"type":"string"}}}`),
		Acceptance:   []string{"contains:" + token},
		MaxSteps:     0,
		TimeoutSec:   60,
	}}
}

// renderSmokeTable is fleet-smoke's human-readable output: one row per node,
// wall time rendered in seconds so an operator can eyeball latency at a glance.
func renderSmokeTable(rows []smokeRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-32s %-18s %-20s %-10s %8s  %s\n", "node", "seat", "placement", "verdict", "wall", "detail")
	for _, r := range rows {
		node := r.Node
		if node == "" {
			node = r.Base
		}
		wall := ""
		if r.WallMs > 0 {
			wall = fmt.Sprintf("%.1fs", float64(r.WallMs)/1000)
		}
		fmt.Fprintf(&b, "%-32s %-18s %-20s %-10s %8s  %s\n", node, r.Seat, r.Placement, r.Verdict, wall, r.Detail)
	}
	return b.String()
}

// countNot counts rows whose verdict is NOT the given one — used to size the
// non-zero exit message.
func countNot(rows []smokeRow, verdict string) int {
	n := 0
	for _, r := range rows {
		if r.Verdict != verdict {
			n++
		}
	}
	return n
}

// exitError builds runFleetSmoke's non-zero-exit error for rows, or nil when
// every row PASSed. Extracted so both output modes (table and --json) run the
// SAME verdict check — the bug this guards against shipped once already:
// --json returned right after encoding the rows, before the failed check
// below ever ran, so a real fleet failure (a DEFER or FAIL row) still exited
// 0 as long as --json was passed, which is exactly the mode a script checks
// the exit code from.
func exitError(rows []smokeRow) error {
	if n := countNot(rows, "PASS"); n > 0 {
		return fmt.Errorf("fleet-smoke: %d node(s) did not PASS", n)
	}
	return nil
}

// runFleetSmoke is the harness's version of PAIR's "Test traffic" button: one
// grounded contract (harness default step budget, 60 s cap) dispatched to
// EVERY configured fleet node with route=remote forced (so it proves the
// node, never a local fallback), then a table of where each landed. Non-zero
// exit unless every row is PASS — a DEFER or FAIL here is real fleet signal,
// not a soft warning to swallow, in EITHER output mode (table or --json).
func runFleetSmoke(args []string) error {
	fs := flag.NewFlagSet("fleet-smoke", flag.ExitOnError)
	fs.String("config", "", "config file path")
	var remotes multiFlag
	fs.Var(&remotes, "remote", "node base URL (repeatable; default delegate_remotes)")
	timeout := fs.Int("timeout", 120, "overall seconds per node")
	asJSON := fs.Bool("json", false, "machine-readable rows")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg := loadCfg(fs)
	if !cfg.AgentDelegationEnabled {
		return fmt.Errorf("agent delegation is disabled on this box — set \"agent_delegation_enabled\": true in the config")
	}
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	bases := []string(remotes)
	if len(bases) == 0 {
		bases = cfg.DelegateRemotes
	}
	if len(bases) == 0 {
		return fmt.Errorf("fleet-smoke: no nodes — set delegate_remotes or pass --remote")
	}

	rows := make([]smokeRow, 0, len(bases))
	for _, base := range bases {
		hint := strings.TrimPrefix(strings.TrimPrefix(base, "http://"), "https://")
		hint = strings.Split(hint, ":")[0]
		c, perr := delegate.PrepareContract(smokeContract(hint), "")
		if perr != nil {
			return perr
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeout)*time.Second)
		results, _, rerr := delegate.Run(ctx, cfg, p.RunAgentContract, []core.AgentContract{c}, "remote", []string{base})
		cancel()
		row := smokeRow{Base: base}
		switch {
		case rerr != nil:
			row.Verdict, row.Detail = "FAIL", rerr.Error()
		case len(results) == 0:
			row.Verdict, row.Detail = "FAIL", "no result row"
		default:
			r := results[0]
			row.Node, row.Seat, row.Placement, row.JobID = r.Node, r.Seat, r.PlacementReason, r.JobID
			row.WallMs = r.Result.WallMs
			switch {
			case r.Err != "":
				row.Verdict, row.Detail = "FAIL", r.Err
			case r.Result.Deferred:
				row.Verdict, row.Detail = "DEFER", r.Result.Reason
			case len(r.AcceptanceFailures) > 0:
				row.Verdict, row.Detail = "FAIL", strings.Join(r.AcceptanceFailures, "; ")
			case !strings.HasPrefix(r.PlacementReason, "route=remote"):
				row.Verdict, row.Detail = "FAIL", "did not land on the node: "+r.PlacementReason
			default:
				row.Verdict = "PASS"
			}
		}
		rows = append(rows, row)
	}

	if *asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(rows); err != nil {
			return err
		}
		return exitError(rows)
	}
	fmt.Print(renderSmokeTable(rows))
	return exitError(rows)
}
