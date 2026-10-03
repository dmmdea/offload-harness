package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
)

// runAgentAudit is `local-offload agent-audit verify [--file PATH] [--json]` (register
// SF-08): it checks every hash-chained run on the broker audit trail and exits non-zero
// when a run's chain is broken, naming the run and the seq. (The audit-yaml,
// audit-config and audit-sample verbs are unrelated config-drift tools.)
func runAgentAudit(args []string) error {
	if len(args) == 0 || args[0] != "verify" {
		return errors.New("usage: local-offload agent-audit verify [--file PATH] [--json] [--strict]")
	}
	fs := flag.NewFlagSet("agent-audit verify", flag.ContinueOnError)
	file := fs.String("file", "", "the broker audit trail to verify (default: agent-audit.jsonl under the install root, config `home`, else <user home>/.local-offload)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	strict := fs.Bool("strict", false, "also fail an OPEN run (no run_end: a crash, or a cut tail; the chain alone cannot tell them apart)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *file == "" {
		cfg, _ := config.LoadWithSource("")
		*file = agent.DefaultAuditPath(cfg.BaseDir())
	}
	return agentAuditVerifyMode(*file, *asJSON, *strict, os.Stdout)
}

// agentAuditVerify verifies one trail and writes the report to w. A broken run fails;
// an open run (no run_end: a crash, or a removed tail) and unchained rows (written
// with audit_chain off) are reported, not failed.
func agentAuditVerify(path string, asJSON bool, w io.Writer) error {
	return agentAuditVerifyMode(path, asJSON, false, w)
}

// agentAuditVerifyMode is agentAuditVerify with --strict: an open run fails too.
func agentAuditVerifyMode(path string, asJSON, strict bool, w io.Writer) error {
	if path == "" {
		return errors.New("agent-audit verify: no trail path resolves (the home directory is unknown); pass --file")
	}
	rep, err := agent.VerifyAuditFile(path)
	if err != nil {
		return fmt.Errorf("agent-audit verify: %w", err)
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(w, "agent-audit verify: %s: %d chained run(s), %d chained row(s), %d unchained row(s), %d open run(s), %d late, %d broken\n",
			path, rep.Runs, rep.Chained, rep.Unchained, len(rep.Open), len(rep.Late), len(rep.Breaks))
		for _, id := range rep.Open {
			fmt.Fprintf(w, "  OPEN   run %s: no run_end (the run crashed, or its tail was cut; --strict fails this)\n", id)
		}
		for _, id := range rep.Late {
			fmt.Fprintf(w, "  LATE   run %s: chained rows after its run_end (a tool that kept running after the run closed)\n", id)
		}
		for _, b := range rep.Breaks {
			if b.Line > 0 {
				fmt.Fprintf(w, "  BROKEN line %d: %s\n", b.Line, b.Problem)
				continue
			}
			fmt.Fprintf(w, "  BROKEN run %s seq %d: %s\n", b.RunID, b.Seq, b.Problem)
		}
	}
	if len(rep.Breaks) > 0 {
		return fmt.Errorf("agent-audit verify: %d break(s) in the trail", len(rep.Breaks))
	}
	if strict && len(rep.Open) > 0 {
		return fmt.Errorf("agent-audit verify --strict: %d open run(s)", len(rep.Open))
	}
	return nil
}
