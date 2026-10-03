//go:build linux

package gpulease

import (
	"os"
	"strconv"
	"strings"
)

// processTreeImpl reads /proc: the parent of each process from its stat line and its command
// line from cmdline (NUL-separated). A process that exits mid-walk is simply absent.
func processTreeImpl(root int) ([]TreeProc, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var table []TreeProc
	for _, e := range entries {
		pid, perr := strconv.Atoi(e.Name())
		if perr != nil {
			continue
		}
		ppid, ok := parentOf(pid)
		if !ok {
			continue
		}
		table = append(table, TreeProc{PID: pid, PPID: ppid})
	}
	out := descendantsOf(root, table, processStart)
	for i := range out {
		if v, ok := processStart(out[i].PID); ok {
			out[i].StartMs = v
		}
		out[i].Cmdline = cmdlineOf(out[i].PID)
	}
	return out, nil
}

// parentOf parses the ppid out of /proc/<pid>/stat. The command name is parenthesised and may
// itself contain spaces and parentheses, so the fields are counted from the LAST ')'.
func parentOf(pid int) (int, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(f[1])
	return ppid, err == nil
}

func cmdlineOf(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || len(b) == 0 {
		return ""
	}
	return strings.TrimRight(strings.ReplaceAll(string(b), "\x00", " "), " ")
}

// startUnixMs converts a TreeProc.StartMs, which on Linux is the process's start in clock ticks
// since boot (procstart_linux.go), to Unix milliseconds: the boot time from /proc/stat plus the
// ticks at USER_HZ (100 on every Linux this harness runs on). false when /proc/stat cannot say.
func startUnixMs(startMs int64) (int64, bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			boot, perr := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if perr != nil || boot <= 0 {
				return 0, false
			}
			return ticksToUnixMs(boot, startMs), true
		}
	}
	return 0, false
}
