// Package comfyinst stops the ComfyUI instances a GPU lease's runners kept alive (plan P13).
//
// A runner told to keep the ComfyUI it launched (render/comfy-lifecycle.mjs, keep:true) leaves
// it running after the runner exits: spawned detached, console in a log file, so a batch of
// items under one lease pays the model load once. That instance must live no longer than the
// lease it was launched under, which is what keeps the "no idle model stays loaded" rule
// without a watcher: the launch marker (.offload-launch-<key>.json) records the lease epoch,
// and the HOLDER of that lease stops the instance when it releases it (`gpu reserve` when its
// wrapped command ends, the pipeline when a media lease is released).
//
// WHAT IT WILL AND WILL NOT STOP. Only a keyed instance whose marker names exactly this lease
// epoch. A marker with no epoch (the default instance, an instance launched outside a lease) is
// never touched: it belongs to whoever kept it. Before anything is signalled the instance is
// shown to be the harness's own, the same proof the launcher applies before it reuses one: its
// pid is alive, the process did not begin after the marker was written (a recycled pid), and
// the endpoint on the marker's port reports exactly the argv the launcher recorded. Anything
// short of that is left running and reported, never killed: a foreign process on the port is
// not ours to stop. Nothing here is a timer or a watcher; it runs once, in the releasing
// process, at release.
package comfyinst

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

const (
	markerPrefix = ".offload-launch-"
	markerSuffix = ".json"
	// startSlackMs is how much later than the process start the marker may have been written
	// (the launcher writes it right after spawning). A process that began after the marker
	// plus this slack cannot be the instance the marker describes.
	startSlackMs = 10_000
)

// Marker is the launch record render/comfy-ownership.mjs writes for a keyed instance.
type Marker struct {
	StartedAt  int64    `json:"startedAt"`
	PID        int      `json:"pid"`
	OwnerPID   int      `json:"ownerPid"`
	Args       []string `json:"args"`
	Key        string   `json:"key"`
	Port       int      `json:"port"`
	LeaseEpoch *uint64  `json:"leaseEpoch"`
}

// Deps are the process and network seams.
type Deps struct {
	// Alive reports whether pid is a live process.
	Alive func(pid int) bool
	// Start reports when pid began, in Unix milliseconds; false when the platform cannot say.
	Start func(pid int) (int64, bool)
	// Kill stops pid.
	Kill func(pid int) error
	// Sleep pauses between the checks that the instance is gone after a kill.
	Sleep func(time.Duration)
	// QuietPolls is how many times the process is re-checked after a kill before it is
	// reported as still running.
	QuietPolls int
	// HTTPTimeout bounds each request to the instance.
	HTTPTimeout time.Duration
	// Client is the HTTP client; nil means one with HTTPTimeout.
	Client *http.Client
}

// RealDeps is the production wiring.
func RealDeps() Deps {
	return Deps{
		Alive:       gpulease.PIDAlive,
		Start:       processStartUnixMs,
		Kill:        terminate,
		Sleep:       time.Sleep,
		QuietPolls:  20, // 20 x 500 ms
		HTTPTimeout: 3 * time.Second,
	}
}

// Outcome is what happened to one instance of the lease.
type Outcome struct {
	Key     string `json:"key"`
	PID     int    `json:"pid"`
	Port    int    `json:"port"`
	Stopped bool   `json:"stopped"`
	// Why is empty when the instance was stopped; otherwise it says why it was left, or why its
	// marker was only cleared.
	Why string `json:"why,omitempty"`
}

// StopForLease stops every keyed instance in comfyDir whose launch marker records lease epoch
// `epoch`, and reports one Outcome per such instance, in key order. An empty comfyDir, an
// epoch of 0 and a directory that cannot be read are no-ops.
func StopForLease(ctx context.Context, comfyDir string, epoch uint64, d Deps) []Outcome {
	if strings.TrimSpace(comfyDir) == "" || epoch == 0 {
		return nil
	}
	entries, err := os.ReadDir(comfyDir)
	if err != nil {
		return nil
	}
	var out []Outcome
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, markerPrefix) || !strings.HasSuffix(name, markerSuffix) {
			continue
		}
		path := filepath.Join(comfyDir, name)
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var m Marker
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		// A complete keyed marker for THIS lease, in the file its key names, or not ours at all.
		if m.Key == "" || m.Port <= 0 || m.PID <= 0 || m.LeaseEpoch == nil || *m.LeaseEpoch != epoch ||
			name != markerPrefix+m.Key+markerSuffix {
			continue
		}
		out = append(out, stopOne(ctx, path, m, epoch, d))
	}
	return out
}

func stopOne(ctx context.Context, path string, m Marker, epoch uint64, d Deps) Outcome {
	o := Outcome{Key: m.Key, PID: m.PID, Port: m.Port}
	if !d.Alive(m.PID) {
		_ = os.Remove(path)
		o.Why = fmt.Sprintf("already gone (process %d is not running); marker cleared", m.PID)
		return o
	}
	if m.StartedAt > 0 && d.Start != nil {
		if began, ok := d.Start(m.PID); ok && began > m.StartedAt+startSlackMs {
			_ = os.Remove(path)
			o.Why = fmt.Sprintf("process %d began after the launch marker was written, so the pid was recycled and is not the instance; marker cleared, nothing killed", m.PID)
			return o
		}
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", m.Port)
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: d.HTTPTimeout}
	}
	argv, err := systemArgv(ctx, client, base)
	switch {
	case err != nil:
		o.Why = fmt.Sprintf("the instance on port %d did not answer /system_stats (%v), so it is not shown to be the harness's own; left running", m.Port, err)
		return o
	case !sameArgv(m.Args, argv):
		o.Why = fmt.Sprintf("the argv the instance on port %d reports differs from the launch marker's, so it is not shown to be the harness's own; left running", m.Port)
		return o
	}
	// The proof above took a round trip to the instance. A lease that reuses a kept instance
	// re-stamps its marker (render/comfy-lifecycle.mjs), so an instance that changed hands in that
	// window is the next lease's and in use: read the marker again at the moment of the stop.
	if why := stillThisLeases(path, m.PID, epoch); why != "" {
		o.Why = why
		return o
	}
	// Drop its models first: if the kill does not take, the card is at least empty.
	postFree(ctx, client, base)
	if err := d.Kill(m.PID); err != nil {
		o.Why = fmt.Sprintf("could not stop process %d: %v; still running", m.PID, err)
		return o
	}
	for i := 0; i < d.QuietPolls; i++ {
		if !d.Alive(m.PID) {
			_ = os.Remove(path)
			o.Stopped = true
			return o
		}
		if d.Sleep != nil {
			d.Sleep(500 * time.Millisecond)
		}
	}
	if !d.Alive(m.PID) {
		_ = os.Remove(path)
		o.Stopped = true
		return o
	}
	o.Why = fmt.Sprintf("stop sent to process %d but it is still running", m.PID)
	return o
}

// stillThisLeases re-reads the marker at path and says why the instance is no longer shown to be
// this lease's ("" = it still is): the marker is gone or unreadable, names another process, or
// names another lease (the next lease re-stamped it when it reused the instance).
func stillThisLeases(path string, pid int, epoch uint64) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "its launch marker vanished while the instance was being checked, so it is not shown to still be this lease's; left running"
	}
	var now Marker
	if json.Unmarshal(b, &now) != nil {
		return "its launch marker became unreadable while the instance was being checked, so it is not shown to still be this lease's; left running"
	}
	if now.PID != pid {
		return fmt.Sprintf("its launch marker now names process %d, not %d: the instance was relaunched while it was being checked; left running", now.PID, pid)
	}
	if now.LeaseEpoch == nil {
		return "its launch marker no longer names any lease: the instance changed hands while it was being checked; left running"
	}
	if *now.LeaseEpoch != epoch {
		return fmt.Sprintf("the instance changed hands while it was being checked (its marker now names lease epoch %d, not %d): it is in use by that lease; left running", *now.LeaseEpoch, epoch)
	}
	return ""
}

// systemArgv reads GET /system_stats system.argv, the launch fingerprint.
func systemArgv(ctx context.Context, c *http.Client, base string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/system_stats", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		System struct {
			Argv []any `json:"argv"`
		} `json:"system"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	if body.System.Argv == nil {
		return nil, fmt.Errorf("it reports no launch argv")
	}
	argv := make([]string, 0, len(body.System.Argv))
	for _, a := range body.System.Argv {
		argv = append(argv, fmt.Sprint(a))
	}
	return argv, nil
}

// sameArgv compares the recorded spawn argv with the one the server reports, element for
// element (render/comfy-ownership.mjs sameArgv).
func sameArgv(a, b []string) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// postFree asks the instance to drop its models and free memory. Best effort.
func postFree(ctx context.Context, c *http.Client, base string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/free", bytes.NewReader([]byte(`{"unload_models":true,"free_memory":true}`)))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := c.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
	}
}
