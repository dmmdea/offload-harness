package comfyinst

import (
	"fmt"
	"strings"
)

// The proof that a ComfyUI which does not answer is still the harness's own, from the operating system alone.
//
// stopInstance proves an instance ours by asking it for its launch argv over HTTP, which a hung instance
// (a CUDA illegal-memory-access leaves the process up and mute) can never answer: it was left running with the
// lease it was launched under, and the next job on its port was refused at once. wedgedProof is the proof that
// needs no answer: the process's command line is ComfyUI's main.py with this instance's --port and
// --output-directory, and the listener of the port is that pid, a process it started, or an owner the system
// cannot name (pid 0). Together with the start-time guard stopInstance has already applied, that is the same
// identity the marker records. Any lookup that fails is a refusal, never a kill.

// wedgedProof reports whether the instance a marker names is shown to be the harness's own without its help;
// when it is not, why is the reason ("" when the platform cannot ask at all: Deps.Listener or Deps.CmdLine unset).
func (d Deps) wedgedProof(m Marker) (ok bool, why string) {
	if d.Listener == nil || d.CmdLine == nil {
		return false, ""
	}
	cmd, err := d.CmdLine(m.PID)
	if err != nil {
		return false, fmt.Sprintf("its command line could not be read (%v)", err)
	}
	if why := commandLineGap(cmd, m); why != "" {
		return false, why
	}
	pids, err := d.Listener(m.Port)
	if err != nil {
		return false, fmt.Sprintf("the listener of port %d could not be found (%v)", m.Port, err)
	}
	for _, p := range pids {
		if p == 0 || p == m.PID {
			continue
		}
		if !d.descendantOf(p, m.PID) {
			return false, fmt.Sprintf("port %d is held by process %d, which is neither process %d nor one it started", m.Port, p, m.PID)
		}
	}
	return true, ""
}

// descendantOf walks up from pid by parent and reports whether it reaches root.
func (d Deps) descendantOf(pid, root int) bool {
	if d.Parent == nil {
		return false
	}
	for depth := 0; depth < 8 && pid > 0; depth++ {
		parent, err := d.Parent(pid)
		if err != nil {
			return false
		}
		if parent == root {
			return true
		}
		pid = parent
	}
	return false
}

func normPath(s string) string { return strings.ToLower(strings.ReplaceAll(s, `\`, "/")) }

// commandLineGap says why a command line is not the one a marker's launch would give ("" = it is): main.py, the
// marker's port and the --output-directory its argv records.
func commandLineGap(cmd string, m Marker) string {
	c := normPath(cmd)
	if !hasToken(c, "main.py") {
		return fmt.Sprintf("process %d is not running main.py", m.PID)
	}
	port := fmt.Sprint(m.Port)
	if !hasFlagValue(c, "--port", port) {
		return fmt.Sprintf("process %d does not run with --port %s", m.PID, port)
	}
	out := argFlagValue(m.Args, "--output-directory")
	if out == "" {
		return "the launch marker records no --output-directory to match against"
	}
	if !strings.Contains(c, normPath(out)) {
		return fmt.Sprintf("process %d does not run with --output-directory %s", m.PID, out)
	}
	return ""
}

// hasToken reports whether tok stands alone in cmd (after a space, quote or slash; before a space, quote or the end).
func hasToken(cmd, tok string) bool {
	for i := 0; ; {
		j := strings.Index(cmd[i:], tok)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(tok)
		beforeOK := start == 0 || strings.ContainsRune(" \t\"'/", rune(cmd[start-1]))
		afterOK := end == len(cmd) || strings.ContainsRune(" \t\"'", rune(cmd[end]))
		if beforeOK && afterOK {
			return true
		}
		i = end
	}
}

// hasFlagValue reports whether cmd carries `flag value` or `flag=value`, the value optionally quoted.
func hasFlagValue(cmd, flag, value string) bool {
	for _, sep := range []string{" ", "="} {
		for _, q := range []string{"", `"`, "'"} {
			want := flag + sep + q + value
			for i := 0; ; {
				j := strings.Index(cmd[i:], want)
				if j < 0 {
					break
				}
				end := i + j + len(want)
				if end == len(cmd) || strings.ContainsRune(" \t\"'", rune(cmd[end])) {
					return true
				}
				i = end
			}
		}
	}
	return false
}

// argFlagValue is the value of the last `flag v` / `flag=v` in an argv (argparse semantics); "" when absent.
func argFlagValue(args []string, flag string) string {
	val := ""
	for i, a := range args {
		switch {
		case a == flag && i+1 < len(args):
			val = args[i+1]
		case strings.HasPrefix(a, flag+"="):
			val = strings.TrimPrefix(a, flag+"=")
		}
	}
	return val
}
