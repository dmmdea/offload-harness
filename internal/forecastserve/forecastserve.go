// Package forecastserve places and runs the Timestral forecast server: the python file it ships
// (embedded, so a binary drop carries the server that matches it), the venv `install forecast`
// builds, and the launcher llama-swap's seat runs (`local-offload forecast-serve`). The launcher
// holds the python child for the whole of its own life: when llama-swap stops the seat, or the
// launcher dies, the child goes with it, so a stop never orphans a process that holds the card.
package forecastserve

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// ScriptName is the file EnsureScript writes under the forecast directory.
const ScriptName = "timestral_server.py"

//go:embed timestral_server.py
var serverScript []byte

// Script is the embedded server source.
func Script() []byte { return serverScript }

// EnsureScript writes <dir>/timestral_server.py when it is missing or differs from the embedded
// source, and returns its path and whether it wrote. The write is atomic (temp file + rename), so a
// launcher that reads the file while an upgrade replaces it never sees half of it.
func EnsureScript(dir string) (path string, wrote bool, err error) {
	path = filepath.Join(dir, ScriptName)
	if cur, rerr := os.ReadFile(path); rerr == nil && bytes.Equal(cur, serverScript) {
		return path, false, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return path, false, fmt.Errorf("forecastserve: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ScriptName+".*.tmp")
	if err != nil {
		return path, false, fmt.Errorf("forecastserve: write %s: %w", path, err)
	}
	_, werr := tmp.Write(serverScript)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return path, false, fmt.Errorf("forecastserve: write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return path, false, fmt.Errorf("forecastserve: replace %s: %w", path, err)
	}
	return path, true, nil
}

// VenvPython is the interpreter of the venv `install forecast` builds under dir.
func VenvPython(dir string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(dir, "venv", "Scripts", "python.exe")
	}
	return filepath.Join(dir, "venv", "bin", "python")
}

// Options is one launch of the server.
type Options struct {
	Dir           string // the forecast directory: <dir>/venv and the script
	Model         string // the checkpoint directory (config.json + model.safetensors)
	Host          string // loopback only
	Port          int
	Device        string
	MaxRowPatches int // 0 leaves the server's default
	Stdout        io.Writer
	Stderr        io.Writer
}

// IsLoopbackHost reports whether host names only this machine: an IP literal in the loopback range
// or "localhost". The server has no authentication, so nothing else is accepted.
func IsLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Args is the python command line after the interpreter.
func (o Options) Args(script string) []string {
	args := []string{"-I", script, "--model", o.Model, "--port", strconv.Itoa(o.Port), "--host", o.Host, "--device", o.Device}
	if o.MaxRowPatches > 0 {
		args = append(args, "--max-row-patches", strconv.Itoa(o.MaxRowPatches))
	}
	return args
}

// Check refuses a launch that cannot work, naming what is missing and the verb that fixes it.
func (o Options) Check() error {
	if !IsLoopbackHost(o.Host) {
		return fmt.Errorf("forecast-serve: --host %q is not loopback; the forecast server has no authentication and binds 127.0.0.1 only", o.Host)
	}
	if o.Port < 1 || o.Port > 65535 {
		return fmt.Errorf("forecast-serve: --port %d is not a port", o.Port)
	}
	py := VenvPython(o.Dir)
	if fi, err := os.Stat(py); err != nil || fi.IsDir() {
		return fmt.Errorf("forecast-serve: no python at %s: the forecast venv is not installed; run `local-offload install forecast`", py)
	}
	if fi, err := os.Stat(filepath.Join(o.Model, "model.safetensors")); err != nil || fi.IsDir() {
		return fmt.Errorf("forecast-serve: %s holds no model.safetensors: the Timestral weights are not installed; run `local-offload install forecast`", o.Model)
	}
	return nil
}

// Serve refreshes the embedded script, starts the server under the venv python and waits for it.
// The child is bound to this process's life (see startBound); SIGINT and SIGTERM are forwarded
// as a stop. It returns the child's exit code; err is non-nil only when the launch itself failed.
func Serve(ctx context.Context, o Options) (int, error) {
	if err := o.Check(); err != nil {
		return 1, err
	}
	script, _, err := EnsureScript(o.Dir)
	if err != nil {
		return 1, err
	}
	cmd := winexec.Command(VenvPython(o.Dir), o.Args(script)...)
	cmd.Dir = o.Dir
	cmd.Stdout, cmd.Stderr = o.Stdout, o.Stderr
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	// The weights are local; the server must never reach for the hub.
	cmd.Env = append(os.Environ(), "HF_HUB_OFFLINE=1")

	release, stop, err := startBound(cmd)
	if err != nil {
		return 1, fmt.Errorf("forecast-serve: start %s: %w", VenvPython(o.Dir), err)
	}
	defer release()

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case <-sigs:
			stop()
		case <-ctx.Done():
			stop()
			ctx = context.Background()
		case werr := <-done:
			var ee *exec.ExitError
			switch {
			case werr == nil:
				return 0, nil
			case errors.As(werr, &ee):
				if c := ee.ExitCode(); c > 0 {
					return c, nil
				}
				return 1, nil
			default:
				return 1, werr
			}
		}
	}
}
