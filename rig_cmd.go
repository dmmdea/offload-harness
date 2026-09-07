package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/rig"
)

// runRig is `local-offload rig --seat <alias> [--since 7d] [--node <id>]
// [--out file.json] [--json]`: the seat rigger's classifier over this box's
// delegation-log corpus (rig package, ADR 0036 P3a). Prints the markdown
// triage report (or the JSON with --json) and writes the JSON to --out. It
// proposes nothing and applies nothing.
func runRig(args []string) error {
	fs := flag.NewFlagSet("rig", flag.ExitOnError)
	fs.String("config", "", "config file path")
	seat := fs.String("seat", "", "seat alias exactly as the corpus rows name it (e.g. qwen3.5-4b-vllm, agent-pool); required")
	since := fs.String("since", "7d", "window: <N>d or <N>h back from now, or an RFC3339 instant")
	node := fs.String("node", "", "only rows that ran on this node id (optional)")
	out := fs.String("out", "", "write the JSON report here (optional)")
	verdicts := fs.String("verdicts", "", "write EVERY bad row's verdict ({job_id, axis, sub, evidence}) here — the per-row companion of the report, for an agreement check against independent labels (optional)")
	asJSON := fs.Bool("json", false, "print the JSON report instead of the markdown")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*seat) == "" {
		return fmt.Errorf("rig: --seat is required (the seat alias as the corpus names it)")
	}
	until := time.Now()
	start, err := rig.ParseSince(*since, until)
	if err != nil {
		return err
	}
	cfg := loadCfg(fs)
	dir := filepath.Join(cfg.BaseDir(), "delegation-log")
	rows, skipped, err := rig.ReadShards(dir, start, until)
	if err != nil {
		return err
	}
	rep, err := rig.Build(rows, *seat, *node, start, until)
	if err != nil {
		return err
	}
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "[rig] %d unparseable corpus line(s) skipped\n", skipped)
	}
	if *verdicts != "" {
		vb, _ := json.MarshalIndent(rig.Verdicts(rows, *seat, *node), "", "  ")
		if err := os.WriteFile(*verdicts, vb, 0o644); err != nil {
			return fmt.Errorf("rig: writing %s: %w", *verdicts, err)
		}
		fmt.Fprintf(os.Stderr, "[rig] verdicts written: %s\n", *verdicts)
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	if *out != "" {
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			return fmt.Errorf("rig: writing %s: %w", *out, err)
		}
		fmt.Fprintf(os.Stderr, "[rig] report written: %s\n", *out)
	}
	if *asJSON {
		fmt.Println(string(b))
	} else {
		fmt.Print(rig.Markdown(rep))
	}
	return nil
}
