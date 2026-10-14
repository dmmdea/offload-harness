package main

// The holders' side of the yield request (plan P11, ADR 0085): the shell that runs a job's own
// --on-yield line, the one place a holder builds its yield ticker, and what `gpu reserve --detach`
// prints about the request. The decision of WHEN to ask lives in internal/gpulease (yield.go); this
// file only starts the line the job recorded.
//
// Nothing here stops a command, signals a process or frees a claim. The line is started once, its
// output is discarded (the wrapper's own stdout is the job's, and a helper's chatter must not land in
// it), and it is never waited out or cut short: a helper that takes an hour is left to take it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/winexec"
)

// yieldShell is the runner the holders hand their yield ticker. A variable so a test substitutes it.
var yieldShell gpulease.YieldRunner = runYieldShell

// yieldShellCommand builds the platform shell around the job's own line: sh -c on Unix, and on
// Windows cmd.exe with the line as ONE argument that the OS command line carries unmangled (see
// setShellCmdLine). The child is hidden; it inherits the holder's working directory, which is the
// directory the line was recorded in.
func yieldShellCommand(line string) *exec.Cmd {
	name, args := "sh", []string{"-c", line}
	if runtime.GOOS == "windows" {
		name, args = "cmd.exe", []string{"/d", "/s", "/c", line}
	}
	cmd := winexec.Command(name, args...)
	setShellCmdLine(cmd, line)
	return cmd
}

// runYieldShell runs the line to its end with env added to the holder's own environment, and reports
// its exit status. An error is a failure to start it; a line that ran and failed is its exit status.
func runYieldShell(line string, env []string) (int, error) {
	cmd := yieldShellCommand(line)
	cmd.Env = append(os.Environ(), env...)
	// Stdin, Stdout and Stderr stay nil: the null device, never the wrapper's own streams.
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

// detachedYieldsForm is the form the hidden holder stamps on its record: it asks, so it says so, unless it
// releases at its deadline (the pre-terms ending), in which case it never ticks the request and stamps nothing.
func detachedYieldsForm(releaseAtExpiry bool) string {
	if releaseAtExpiry {
		return ""
	}
	return gpulease.YieldsDetached
}

// newHolderYieldTicker is the ticker a holder runs beside its term check. onYield is the holder's
// own in-memory --on-yield value, never read back from the record.
func newHolderYieldTicker(lease *gpulease.Lease, onYield, label string, out io.Writer) *gpulease.YieldTicker {
	return lease.NewYieldTicker(gpulease.YieldTickerOptions{OnYield: onYield, Run: yieldShell, Out: out, Label: label})
}

// detachedReport is what `gpu reserve --detach` prints once its holder has taken the lease. A detached
// job is not a child of the holder, so GPU_LEASE_YIELD_FILE is not in its environment: the report names
// the file a job can watch instead.
func detachedReport(info gpulease.Info, leaseDir string, asJSON bool) string {
	yieldFile := ""
	if info.Yields != "" {
		yieldFile = gpulease.YieldFilePath(leaseDir, info.Epoch)
	}
	if asJSON {
		out := map[string]any{
			"held": true, "class": info.Class, "epoch": info.Epoch,
			"pid": info.PID, "expires_at": info.ExpiresAt.Format(time.RFC3339),
		}
		if len(info.Devices) > 0 {
			out["devices"] = info.Devices
		}
		if yieldFile != "" {
			out["yield_file"] = yieldFile
		}
		b, _ := json.Marshal(out)
		return string(b) + "\n"
	}
	cardsText := ""
	if len(info.Devices) > 0 {
		cardsText = ", cards " + strings.Join(info.Devices, ", ")
	}
	s := fmt.Sprintf("reserved: %s epoch %d (pid %d%s) until %s\n  release with: local-offload gpu release --epoch %d\n",
		info.Class, info.Epoch, info.PID, cardsText, info.ExpiresAt.Format(time.Kitchen), info.Epoch)
	if yieldFile != "" {
		s += fmt.Sprintf("  yield request: when this lease is overdue and someone waits for its cards, the holder runs the --on-yield it was given; a job can watch %s, which exists only once the holder is asked\n", yieldFile)
	}
	return s
}
