package update

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// The restart mechanism of the llama-swap daemon is probed before anything is drained or stopped: a daemon that is
// stopped and then cannot be started again (the default task name is not the one this node uses, the unit does not
// exist, the configured command is not there) is down with no way back, and the rollback would fail the same way.

const (
	defaultWindowsTask = "llama-swap-native"
	defaultSystemdUnit = "llama-swap.service"
)

// psSingle quotes a value for a PowerShell command string.
func psSingle(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// prepareRestart probes the restart mechanism of def and remembers the one it settled on (restartSpec returns it
// from then on). It returns "" when the mechanism is there, else a description of what was not found.
func (e *Env) prepareRestart(ctx context.Context, def *compDef, live string) (missing string) {
	spec := e.restartSpec(def)
	if spec.None {
		return ""
	}
	run := e.Deps.RunCommand
	if run == nil {
		return "" // no way to ask: nothing to probe with
	}
	_, configured := e.Config.Restart[def.Name]
	probe := func(cmd string) bool {
		_, err := run(ctx, 30*time.Second, cmd)
		return err == nil
	}

	switch {
	case spec.Command == "" && spec.Task != "":
		if runtime.GOOS != "windows" {
			return "scheduled task " + spec.Task + " (scheduled tasks exist on Windows only)"
		}
		if probe(fmt.Sprintf("Get-ScheduledTask -TaskName %s -ErrorAction Stop | Out-Null", psSingle(spec.Task))) {
			return ""
		}
		if !configured {
			// The default name is not this node's: a single scheduled task that runs the live llama-swap is the one.
			if name := e.taskRunning(ctx, live); name != "" {
				e.logf("%s: no scheduled task %s; using %s, the one task that runs %s (set update.restart.%s to pin it)",
					def.Name, spec.Task, name, filepath.Base(live), def.Name)
				e.setRestart(def.Name, RestartSpec{Task: name})
				return ""
			}
		}
		return "scheduled task " + spec.Task
	case spec.Command != "":
		return probeCommand(ctx, spec.Command, run)
	}
	return ""
}

func (e *Env) setRestart(name string, s RestartSpec) {
	if e.restart == nil {
		e.restart = map[string]RestartSpec{}
	}
	e.restart[name] = s
}

// taskRunning is the name of the one scheduled task whose action runs the live llama-swap executable (as the link
// names it or as it resolves), "" when there is none or more than one.
func (e *Env) taskRunning(ctx context.Context, live string) string {
	want := []string{live}
	if r, err := filepath.EvalSymlinks(live); err == nil && r != live {
		want = append(want, r)
	}
	quoted := make([]string, 0, len(want))
	for _, w := range want {
		quoted = append(quoted, psSingle(filepath.Clean(w)))
	}
	script := "$w=@(" + strings.Join(quoted, ",") + "); " +
		"Get-ScheduledTask | Where-Object { $_.Actions | Where-Object { $_.Execute -and ($w -contains [Environment]::ExpandEnvironmentVariables($_.Execute).Trim('\"')) } } | " +
		"ForEach-Object { $_.TaskName }"
	out, err := e.Deps.RunCommand(ctx, 60*time.Second, script)
	if err != nil {
		return ""
	}
	var names []string
	for _, ln := range strings.Split(out, "\n") {
		if n := strings.TrimSpace(ln); n != "" {
			names = append(names, n)
		}
	}
	if len(names) != 1 {
		return ""
	}
	return names[0]
}

// probeCommand checks a restart command without running it: `systemctl ... restart <unit>` by `systemctl cat <unit>`,
// any other command by its first token resolving on PATH or as a path (on Windows also as a PowerShell command).
func probeCommand(ctx context.Context, command string, run func(context.Context, time.Duration, string) (string, error)) string {
	fields := strings.Fields(command)
	if len(fields) >= 3 && fields[0] == "systemctl" {
		for _, f := range fields[1 : len(fields)-1] {
			if f == "restart" || f == "start" || f == "reload-or-restart" || f == "try-restart" {
				cat := []string{"systemctl"}
				for _, g := range fields[1 : len(fields)-1] {
					if strings.HasPrefix(g, "-") {
						cat = append(cat, g)
					}
				}
				unit := fields[len(fields)-1]
				if _, err := run(ctx, 30*time.Second, strings.Join(append(cat, "cat", unit), " ")); err != nil {
					return "systemd unit " + unit
				}
				return ""
			}
		}
	}
	tok := firstToken(command)
	if tok == "" {
		return "command (empty)"
	}
	if _, err := exec.LookPath(tok); err == nil {
		return ""
	}
	if _, err := os.Stat(tok); err == nil {
		return ""
	}
	if runtime.GOOS == "windows" {
		if _, err := run(ctx, 30*time.Second, fmt.Sprintf("Get-Command %s -ErrorAction Stop | Out-Null", psSingle(tok))); err == nil {
			return ""
		}
	} else if _, err := run(ctx, 30*time.Second, "command -v '"+strings.ReplaceAll(tok, "'", `'\''`)+"'"); err == nil {
		return ""
	}
	return "command " + tok
}

// firstToken is the program a command line starts with: a quoted first word whole, a leading call operator skipped.
func firstToken(command string) string {
	s := strings.TrimSpace(command)
	s = strings.TrimSpace(strings.TrimPrefix(s, "&"))
	if s == "" {
		return ""
	}
	if q := s[0]; q == '"' || q == '\'' {
		if i := strings.IndexByte(s[1:], q); i >= 0 {
			return s[1 : 1+i]
		}
		return s[1:]
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

// skipped is the result of a component left alone because its restart mechanism is not there: nothing was drained,
// stopped or changed, and it is not a rejection of the build.
func skipped(component, missing string) result {
	return result{action: "skipped: restart mechanism " + missing + " not found; set update.restart." + component, exit: ExitOK}
}
