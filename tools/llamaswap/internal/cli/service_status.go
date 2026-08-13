// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored (wave D final glue): read-only service awareness.
// pp:data-source live

package cli

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/cliutil"
	"llamaswap-pp-cli/internal/lsconfig"
)

// glueProcessInfo is what could be learned about the llama-swap process.
//
// Every field is optional on purpose: llama-swap runs here as a SYSTEM-principal
// scheduled task, and an unelevated reader can legitimately see the process
// exists while being denied its start time. Reporting "SYSTEM-owned, details
// need elevation" is the correct answer; erroring out is not.
type glueProcessInfo struct {
	// Found reports whether a llama-swap process was located at all.
	Found bool `json:"found"`
	// PID is the process id, when readable.
	PID int `json:"pid,omitempty"`
	// StartTime is when the process started, when readable.
	StartTime string `json:"start_time,omitempty"`
	// Uptime is derived from StartTime, when available.
	Uptime string `json:"uptime,omitempty"`
	// Elevated reports whether full details were readable. False with
	// Found=true means the process is there but owned by a principal this
	// session cannot inspect.
	DetailsReadable bool `json:"details_readable"`
	// Note explains any gap in the fields above.
	Note string `json:"note,omitempty"`
}

// glueListenerInfo is the socket-level check.
type glueListenerInfo struct {
	// Address is what was probed.
	Address string `json:"address"`
	// Listening reports whether a TCP connect succeeded.
	Listening bool `json:"listening"`
	// LatencyMS is how long the connect took.
	LatencyMS int64 `json:"latency_ms"`
	// Error carries the dial failure when there was one.
	Error string `json:"error,omitempty"`
}

// glueGuardLine is one entry from the launcher's own log.
type glueGuardLine struct {
	// Timestamp is the launcher's stamp, verbatim.
	Timestamp string `json:"timestamp"`
	// Text is the line, trimmed.
	Text string `json:"text"`
}

// glueServiceReport is the command envelope.
type glueServiceReport struct {
	SchemaVersion int              `json:"schema_version"`
	Action        string           `json:"action"`
	BaseURL       string           `json:"base_url"`
	Listener      glueListenerInfo `json:"listener"`
	Process       glueProcessInfo  `json:"process"`
	// Version is the proxy's own /api/version answer.
	Version map[string]any `json:"version,omitempty"`
	// GuardLog is the tail of the launcher's start/decline lines — the
	// idempotence guard's record of what it did and did not start.
	GuardLog []glueGuardLine `json:"guard_log,omitempty"`
	// GuardLogPath is where those lines came from.
	GuardLogPath string `json:"guard_log_path,omitempty"`
	// RestartCommands are SURFACED, NEVER EXECUTED. There is deliberately no
	// start/stop/restart verb in this CLI.
	RestartCommands []string `json:"restart_commands,omitempty"`
	// RestartSource says whether the commands were read from the operator's
	// own registration script or assumed.
	RestartSource string   `json:"restart_source,omitempty"`
	Notes         []string `json:"notes,omitempty"`
}

func newGlueServiceCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Read-only awareness of the llama-swap service: listener, process, version, launcher log.",
		Long: "Read-only awareness of the llama-swap service.\n\n" +
			"There is no start, stop, or restart verb here, and there will not be one. " +
			"llama-swap runs as a SYSTEM-principal scheduled task; a CLI that stops it would " +
			"need elevation it should not have, and an automatic restart is exactly the kind of " +
			"unattended recovery that turns one bad config into a boot loop nobody is watching. " +
			"The restart commands are PRINTED for a human to run.",
		Example: "  llamaswap-pp-cli service status\n" +
			"  llamaswap-pp-cli service status --json",
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:parent-group":     "true",
			"pp:typed-exit-codes": "0,2",
		},
		RunE: parentNoSubcommandRunE(flags),
	}
	cmd.AddCommand(newGlueServiceStatusCmd(flags))
	return cmd
}

func newGlueServiceStatusCmd(flags *rootFlags) *cobra.Command {
	var flagTail int

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Is llama-swap up, which process is it, and what did the launcher last do.",
		Long: strings.Trim(`
Four independent facts about the service, gathered without touching it:

  listener    a TCP connect to the configured port. Answers "is the socket
              open" independently of whether the HTTP layer is healthy.
  process     the llama-swap process id and start time. On this deployment the
              service is a SYSTEM-principal scheduled task, so an unelevated
              session can see that the process exists while being refused its
              start time. That case is reported as "SYSTEM-owned, details need
              elevation" — never as an error, and never as "not running", which
              is the misreading that has sent people chasing a phantom outage.
  version     GET /api/version, i.e. the proxy answering for itself.
  guard log   the tail of the launcher's own start/decline lines. This is what
              tells you whether the last boot actually started a new instance or
              the idempotence guard declined because one was already up.

READ-ONLY. The restart commands are printed, never run — see 'service --help'.

Exit codes: 2 usage. A down service is reported in the output, not as a
non-zero exit: "the service is down" is a successful answer to this question.`, "\n"),
		Example: strings.Trim(`
  llamaswap-pp-cli service status
  llamaswap-pp-cli service status --json
  llamaswap-pp-cli service status --tail 20
`, "\n"),
		Annotations: map[string]string{
			"pp:data-source":      "live",
			"pp:typed-exit-codes": "0,2",
			"mcp:read-only":       "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return glueUsageErrf("%s takes no positional arguments (got %q)", cmd.CommandPath(), args[0])
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "service status")
			}
			return glueRunServiceStatus(cmd, flags, flagTail)
		},
	}
	cmd.Flags().IntVar(&flagTail, "tail", 6, "How many launcher start/decline lines to show.")
	return cmd
}

func glueRunServiceStatus(cmd *cobra.Command, flags *rootFlags, tail int) error {
	ctx := cmd.Context()
	base, _ := spineBaseURL(flags)
	rep := &glueServiceReport{SchemaVersion: glueSchemaVersion, Action: "service status", BaseURL: base}

	rep.Listener = glueProbeListener(base)

	if cliutil.IsVerifyEnv() {
		rep.Notes = append(rep.Notes, "PRINTING_PRESS_VERIFY=1: no process probe and no version read")
	} else {
		rep.Process = glueProbeProcess(ctx)
		var ver map[string]any
		if err := mcGetJSON(ctx, flags, "/api/version", 10*time.Second, &ver); err != nil {
			rep.Notes = append(rep.Notes, "could not read /api/version: "+err.Error())
		} else {
			rep.Version = ver
		}
	}

	configPath, cfgErr := lsconfig.DefaultConfigPath()
	if cfgErr == nil {
		rep.GuardLogPath = filepath.Join(filepath.Dir(configPath), "llama-swap.log")
		lines, note := glueGuardTail(rep.GuardLogPath, tail)
		rep.GuardLog = lines
		if note != "" {
			rep.Notes = append(rep.Notes, note)
		}
		rep.RestartCommands, rep.RestartSource = restartCommand(configPath)
	} else {
		rep.Notes = append(rep.Notes, "llama-swap config not found ("+cfgErr.Error()+"); launcher log and restart commands unavailable")
	}
	rep.Notes = append(rep.Notes,
		"restart commands are SURFACED, NEVER EXECUTED: this CLI has no start/stop/restart verb by design")

	return mcEmit(cmd, flags, rep, func(w io.Writer) {
		fmt.Fprintf(w, "listener:  %s %s (%dms)\n", rep.Listener.Address, glueUpDown(rep.Listener.Listening), rep.Listener.LatencyMS)
		switch {
		case !rep.Process.Found:
			fmt.Fprintf(w, "process:   not found%s\n", glueParen(rep.Process.Note))
		case rep.Process.DetailsReadable:
			fmt.Fprintf(w, "process:   pid %d, started %s (up %s)\n", rep.Process.PID, rep.Process.StartTime, rep.Process.Uptime)
		default:
			fmt.Fprintf(w, "process:   found%s%s\n", gluePid(rep.Process.PID), glueParen(rep.Process.Note))
		}
		if rep.Version != nil {
			fmt.Fprintf(w, "version:   %v (commit %v, built %v)\n", rep.Version["version"], rep.Version["commit"], rep.Version["build_date"])
		}
		if len(rep.GuardLog) > 0 {
			fmt.Fprintf(w, "\nlauncher log (%s):\n", rep.GuardLogPath)
			for _, l := range rep.GuardLog {
				fmt.Fprintf(w, "  %s\n", l.Text)
			}
		}
		if len(rep.RestartCommands) > 0 {
			fmt.Fprintf(w, "\nrestart (%s) — surfaced, never executed:\n", rep.RestartSource)
			for _, c := range rep.RestartCommands {
				fmt.Fprintf(w, "  %s\n", c)
			}
		}
		for _, n := range rep.Notes {
			fmt.Fprintf(w, "  - %s\n", n)
		}
	})
}

func glueUpDown(ok bool) string {
	if ok {
		return "LISTENING"
	}
	return "DOWN"
}

func gluePid(pid int) string {
	if pid == 0 {
		return ""
	}
	return fmt.Sprintf(", pid %d", pid)
}

func glueParen(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return " (" + s + ")"
}

// glueProbeListener does a plain TCP connect. Deliberately separate from the
// HTTP version read: a socket that accepts while the HTTP layer is wedged is a
// real state, and collapsing the two hides it.
func glueProbeListener(base string) glueListenerInfo {
	info := glueListenerInfo{}
	u, err := url.Parse(base)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "11436"
	}
	info.Address = net.JoinHostPort(host, port)
	start := time.Now()
	conn, derr := net.DialTimeout("tcp", info.Address, 3*time.Second)
	info.LatencyMS = time.Since(start).Milliseconds()
	if derr != nil {
		info.Error = derr.Error()
		return info
	}
	_ = conn.Close()
	info.Listening = true
	return info
}

// glueGuardPattern matches the launcher's own stamped lines, which is where the
// idempotence guard records both a start and a declined start.
var glueGuardPattern = regexp.MustCompile(`^\[([^\]]+)\]\s*(start-llama-swap:.*)$`)

// glueGuardTail returns the last n launcher lines from the log.
//
// The log is a 250k-line append-only file holding the proxy's own output as
// well; only the launcher-stamped lines are extracted, and the file is read
// from the end so a large log costs a bounded read.
func glueGuardTail(path string, n int) ([]glueGuardLine, string) {
	if n <= 0 {
		n = 6
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "launcher log not readable (" + err.Error() + "); start/decline history unavailable"
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, "launcher log not readable (" + err.Error() + ")"
	}
	// A generous tail window: launcher lines are rare relative to request
	// lines, so the window must cover many requests to catch a few starts.
	const window = 4 << 20
	size := fi.Size()
	offset := int64(0)
	if size > window {
		offset = size - window
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, "launcher log not seekable (" + err.Error() + ")"
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, "launcher log not readable (" + err.Error() + ")"
	}
	var out []glueGuardLine
	for _, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		m := glueGuardPattern.FindStringSubmatch(strings.TrimSpace(raw))
		if m == nil {
			continue
		}
		out = append(out, glueGuardLine{Timestamp: strings.TrimSpace(m[1]), Text: strings.TrimSpace(raw)})
	}
	if len(out) == 0 {
		return nil, fmt.Sprintf("no launcher start/decline lines in the last %dMB of %s", window>>20, filepath.Base(path))
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out, ""
}
