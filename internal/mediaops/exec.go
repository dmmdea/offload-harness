package mediaops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Exec guards mirror internal/gpugen (timeout -> WHOLE process tree killed via
// taskkill on Windows; WaitDelay grace) minus everything GPU: no lock, no ComfyUI
// VRAM free, no COMFY env. mediaops children are pure CPU (python/PIL, ffmpeg,
// gimp-console) and run in parallel with renders by design (spec §decisions).

// runCapture executes exe args... with an optional stdin payload and returns
// stdout and stderr separately (probe needs the stderr banner; the PIL worker
// speaks JSON on stdout). Non-zero exit is returned as err with a stderr tail —
// EXCEPT when okExit reports the code as expected (ffmpeg -i exits 1 by design).
func runCapture(ctx context.Context, timeout time.Duration, stdin []byte, okExit func(int) bool, exe string, args ...string) (string, string, error) {
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, exe, args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	cmd.Cancel = func() error { return killTree(cmd.Process) }
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && okExit != nil && okExit(ee.ExitCode()) {
			return out.String(), errb.String(), nil
		}
		return out.String(), errb.String(), fmt.Errorf("%s failed: %w (%s)", filepath.Base(exe), err, tail(errb.String(), 400))
	}
	return out.String(), errb.String(), nil
}

func killTree(p *os.Process) error {
	if p == nil {
		return nil
	}
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid)).Run()
		return nil
	}
	return p.Kill()
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// ResolveEditPython resolves the PIL engine's python: the explicit config value
// when set, else <comfyDir>/.venv/{Scripts/python.exe|bin/python}. "" = absent
// (the caller defers). Existence-checked — a configured-but-missing engine is
// absent, not an error at call time.
func ResolveEditPython(editPython, comfyDir string) string {
	if editPython != "" {
		if _, err := os.Stat(editPython); err == nil {
			return editPython
		}
		return ""
	}
	if comfyDir == "" {
		return ""
	}
	for _, rel := range []string{
		filepath.Join(".venv", "Scripts", "python.exe"),
		filepath.Join(".venv", "bin", "python"),
	} {
		p := filepath.Join(comfyDir, rel)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
