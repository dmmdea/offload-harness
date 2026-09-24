package main

// `local-offload node-swap` — the one reusable engine every future Windows
// fleet-node binary swap calls, replacing the per-deploy aorus-swap-<sha>.ps1
// / deploy-node-exe.ps1 / fleet-node-restart.ps1 stitching (docs/systems/
// node-swap.md has the full story, including the 2026-09-24 Aorus outage
// this tool exists to prevent).
//
//	local-offload node-swap --staged path --target path --sha256 HEX
//	  [--restart-task NAME | --restart-command "..."] [--health-url URL]
//	  [--render-tarball path --render-dir path]
//	  [--dry-run] [--result out.json] [--log out.log]
//
// This command runs the swap SYNCHRONOUSLY and returns its own exit code —
// it is the engine, not the detached launcher. To survive an SSH session
// ending mid-run (the actual defect behind the 2026-09-24 outage), invoke it
// through setup/windows-node-swap-launch.ps1, which starts this exact
// command via WMI/CIM (Win32_Process.Create) so it keeps running, keeps
// writing --log/--result, and still reaches its own rollback path after the
// calling session is gone.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/nodeswap"
)

// nodeSwapOutput is the output-related flags (--result/--log/--json) kept
// separate from nodeswap.Plan, which carries only what Run itself needs.
type nodeSwapOutput struct {
	resultPath string
	logPath    string
	asJSON     bool
}

// parseNodeSwapFlags is the testable seam: pure flag parsing into a
// nodeswap.Plan + nodeSwapOutput, no I/O, no pipeline, no real Deps — the
// same shape as runGraphParams elsewhere in this file for the same reason
// (a CLI's argument contract should be unit-testable without standing up
// everything the command eventually does).
func parseNodeSwapFlags(args []string) (nodeswap.Plan, nodeSwapOutput, error) {
	fs := flag.NewFlagSet("node-swap", flag.ContinueOnError)
	fs.String("config", "", "unused by node-swap; accepted so the global --config hoist never misroutes this subcommand")

	staged := fs.String("staged", "", "path to the staged new binary (required)")
	target := fs.String("target", "", "path to the live binary to replace (required)")
	sha256Want := fs.String("sha256", "", "expected sha256 of the staged binary, hex (required unless --skip-hash-check)")
	skipHash := fs.Bool("skip-hash-check", false, "TESTING ONLY: skip verifying the staged binary's hash")
	backupSuffix := fs.String("backup-suffix", "", "backup filename suffix: <target>.bak-<suffix> (default: a timestamp)")

	healthURL := fs.String("health-url", "", "this node's GET /fleet/health URL — waits for queue=0 before swapping and verifies health after restart; omit for a standalone node with no fleet-serve endpoint (e.g. the OptiPlex)")
	waitIdleTimeout := fs.Duration("wait-idle-timeout", 10*time.Minute, "max time to wait for the node to go idle before swapping")

	restartTask := fs.String("restart-task", "", "Windows scheduled task to Stop/Start around the swap (e.g. offload-fleet-node); mutually exclusive with --restart-command")
	restartCommand := fs.String("restart-command", "", "a command that stops+relaunches the node itself (e.g. the Qube's WMI-based fleet-node-restart.ps1); mutually exclusive with --restart-task. Neither flag = standalone binary-only swap, nothing restarted")
	restartTimeout := fs.Duration("restart-timeout", 60*time.Second, "max time to let --restart-command/-task run")
	verifyTimeout := fs.Duration("verify-timeout", 90*time.Second, "max time to wait for a verified new process (PID + image sha256 + health) after restart")

	renderTarball := fs.String("render-tarball", "", "optional: path to a render tree tarball (tar.gz) to also swap")
	renderDir := fs.String("render-dir", "", "target render directory (required with --render-tarball)")

	processMatch := fs.String("process-match", "fleet-serve", "substring a live node's command line must contain")
	mcpMatch := fs.String("mcp-match", " mcp", "substring identifying an idle MCP-helper process sharing --target's exe path; only this class may be stopped to clear a rename")

	dryRun := fs.Bool("dry-run", false, "verify the hash (and, with --health-url, read health once) and stop — no file or process is touched")
	noRollback := fs.Bool("no-rollback", false, "TESTING ONLY: disable the automatic rollback on failure")

	resultPath := fs.String("result", "", "write the full JSON Outcome here (poll this after a detached launch)")
	logPath := fs.String("log", "", "append a timestamped text log here as the swap progresses")
	asJSON := fs.Bool("json", false, "print the Outcome as JSON on stdout too")

	if err := fs.Parse(args); err != nil {
		return nodeswap.Plan{}, nodeSwapOutput{}, err
	}

	plan := nodeswap.Plan{
		Staged:          *staged,
		Target:          *target,
		ExpectedSHA256:  *sha256Want,
		SkipHashCheck:   *skipHash,
		BackupSuffix:    *backupSuffix,
		HealthURL:       *healthURL,
		WaitIdleTimeout: *waitIdleTimeout,
		RestartTaskName: *restartTask,
		RestartCommand:  *restartCommand,
		RestartTimeout:  *restartTimeout,
		VerifyTimeout:   *verifyTimeout,
		RenderTarball:   *renderTarball,
		RenderDir:       *renderDir,
		ProcessMatch:    *processMatch,
		MCPMatch:        *mcpMatch,
		DryRun:          *dryRun,
		NoRollback:      *noRollback,
	}
	out := nodeSwapOutput{resultPath: *resultPath, logPath: *logPath, asJSON: *asJSON}
	return plan, out, nil
}

func runNodeSwap(args []string) error {
	// Find --result/--log BEFORE the real flag parse can fail (PR #476
	// review): this command is normally launched DETACHED via
	// setup/windows-node-swap-launch.ps1 with no attached console, so a
	// failure before either file exists is indistinguishable to a poller
	// from "still running". A bad flag combination or a --log path the
	// process cannot create must still leave a result a poller can read.
	earlyResultPath, earlyLogPath := scanEarlyOutputFlags(args)

	plan, out, err := parseNodeSwapFlags(args)
	if err != nil {
		writeEarlyFailure(earlyResultPath, earlyLogPath, "parsing flags: "+err.Error())
		return err
	}
	resultPath, logPath, asJSON := out.resultPath, out.logPath, out.asJSON

	logf, closeLog, err := openSwapLog(logPath)
	if err != nil {
		writeEarlyFailure(resultPath, "", fmt.Sprintf("opening --log %s: %v", logPath, err))
		return fmt.Errorf("opening --log %s: %w", logPath, err)
	}
	defer closeLog()
	logger := nodeswap.NewLogger(logf)

	outcome := nodeswap.Run(context.Background(), plan, nodeswap.DefaultDeps(), logger)

	if resultPath != "" {
		if werr := writeSwapResult(resultPath, outcome); werr != nil {
			// The swap's own outcome still matters even if we failed to
			// persist it — report both rather than losing the swap result
			// behind a write error.
			fmt.Fprintf(os.Stderr, "node-swap: writing --result %s: %v\n", resultPath, werr)
		}
	}
	if asJSON {
		b, _ := json.MarshalIndent(outcome, "", "  ")
		fmt.Println(string(b))
	} else {
		printSwapOutcomeHuman(outcome)
	}

	if !outcome.OK {
		return fmt.Errorf("node-swap failed: %s", outcome.Error)
	}
	return nil
}

// scanEarlyOutputFlags does a tolerant, best-effort pre-scan for -result/
// --result and -log/--log (space or = form) BEFORE the real flag.Parse runs.
// It never errors: a malformed flag set here just means the corresponding
// path stays empty, which is exactly the situation the real parse is about
// to report properly. This exists ONLY so a parse failure still has
// somewhere to write news of itself.
func scanEarlyOutputFlags(args []string) (resultPath, logPath string) {
	get := func(name string) string {
		flagEq := "-" + name + "="
		for i, a := range args {
			switch {
			case strings.HasPrefix(a, "--"+name+"="):
				return strings.TrimPrefix(a, "--"+name+"=")
			case strings.HasPrefix(a, flagEq):
				return strings.TrimPrefix(a, flagEq)
			case a == "-"+name || a == "--"+name:
				if i+1 < len(args) {
					return args[i+1]
				}
			}
		}
		return ""
	}
	return get("result"), get("log")
}

// writeEarlyFailure records a minimal nodeswap.Outcome-shaped failure to
// whatever output paths were found — best-effort on every step, since this
// runs precisely when something has already gone wrong and it must never
// itself panic or mask the real error being returned to the caller.
func writeEarlyFailure(resultPath, logPath, reason string) {
	now := time.Now()
	if resultPath != "" {
		outcome := nodeswap.Outcome{
			OK: false, Error: reason, StartedAt: now, FinishedAt: now,
			Steps: []nodeswap.StepResult{{Name: "startup", OK: false, Detail: reason, At: now}},
		}
		_ = writeSwapResult(resultPath, outcome)
	}
	if logPath != "" {
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			fmt.Fprintf(f, "%s [startup] ok=false %s\n", now.Format(time.RFC3339), reason)
			_ = f.Close()
		}
	}
}

func openSwapLog(path string) (write func(string), closeFn func(), err error) {
	if path == "" {
		return func(string) {}, func() {}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return func(line string) {
		fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), line)
		// Also echo to stderr: a caller running this attached (dry-run
		// testing, or the launcher's own foreground smoke) sees progress
		// without tailing the log file separately.
		fmt.Fprintln(os.Stderr, line)
	}, func() { _ = f.Close() }, nil
}

func writeSwapResult(path string, outcome nodeswap.Outcome) error {
	b, err := json.MarshalIndent(outcome, "", "  ")
	if err != nil {
		return err
	}
	// Write to a temp file then rename into place: a poller reading --result
	// over a fresh connection after an SSH drop must never observe a
	// partially-written file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func printSwapOutcomeHuman(o nodeswap.Outcome) {
	status := "OK"
	if !o.OK {
		status = "FAILED"
	}
	fmt.Printf("node-swap: %s (%.1fs)\n", status, o.DurationSec)
	for _, s := range o.Steps {
		mark := "ok"
		if !s.OK {
			mark = "FAIL"
		}
		line := fmt.Sprintf("  [%-5s] %-20s %s", mark, s.Name, s.Detail)
		fmt.Println(strings.TrimRight(line, " "))
	}
	if o.OK {
		fmt.Printf("  old=%s new=%s backup=%s\n", short(o.OldSHA256), short(o.NewSHA256), o.BackupPath)
		if o.FinalPID != 0 {
			fmt.Printf("  running pid=%d image_sha256=%s health_version=%s\n", o.FinalPID, short(o.FinalImageSHA256), o.HealthVersion)
		}
		return
	}
	fmt.Printf("  error: %s\n", o.Error)
	if o.RolledBack {
		fmt.Printf("  rolled back: %v\n", o.RollbackOK)
	}
}

func short(sha string) string {
	if len(sha) <= 16 {
		return sha
	}
	return sha[:16] + "..."
}
