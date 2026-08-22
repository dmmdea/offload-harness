package hailoclient

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// ErrNoSidecarCmd: the sidecar is down and this box has no way to start it.
var ErrNoSidecarCmd = errors.New("hailo sidecar not running and no hailo_sidecar_cmd configured")

// Sidecar starts the HTTP sidecar ON DEMAND (operator decision 2026-08-22: no
// scheduler, no always-on service — the sidecar self-exits idle, the harness
// brings it back when needed). Ensure is the single entry point every NPU tool
// calls first; concurrent callers share one spawn.
type Sidecar struct {
	c            *Client
	spawn        func() error
	startTimeout time.Duration
	mu           sync.Mutex
}

// NewSidecar wires a client to a spawn function (nil = cannot spawn).
func NewSidecar(c *Client, spawn func() error, startTimeout time.Duration) *Sidecar {
	return &Sidecar{c: c, spawn: spawn, startTimeout: startTimeout}
}

// Client returns the wired client, for callers that hold only the Sidecar.
func (s *Sidecar) Client() *Client { return s.c }

// Ensure returns nil once /health answers. Down + spawnable → spawn once and
// poll until startTimeout; down + not spawnable → ErrNoSidecarCmd.
func (s *Sidecar) Ensure(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.c.Health(ctx); err == nil {
		return nil
	}
	if s.spawn == nil {
		return ErrNoSidecarCmd
	}
	if err := s.spawn(); err != nil {
		return fmt.Errorf("starting hailo sidecar: %w", err)
	}
	deadline := time.Now().Add(s.startTimeout)
	for time.Now().Before(deadline) {
		if _, err := s.c.Health(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("hailo sidecar did not become healthy within %s", s.startTimeout)
}

// SpawnCmd launches the configured launcher DETACHED (Start, not Run — the
// sidecar outlives this call and exits on its own idle timer) with no console
// window. The idle window is passed through so config is the single source.
func SpawnCmd(cmdPath string, idleSec int) func() error {
	return func() error {
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("cmd.exe", "/c", cmdPath, "--idle-sec", strconv.Itoa(idleSec))
			hideWindow(cmd)
		} else {
			cmd = exec.Command("sh", "-c", cmdPath+" --idle-sec "+strconv.Itoa(idleSec))
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		go cmd.Wait() // reap; never block the caller
		return nil
	}
}
