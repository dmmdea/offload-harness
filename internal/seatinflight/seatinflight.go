// Package seatinflight is the machine-wide register of the harness's OWN
// requests to this box's llama-swap seats: one small marker file per request,
// written when the request is admitted and removed when it is released.
//
// WHY IT EXISTS. PAIR's Jobs list shows harness work because the harness
// reports it (internal/pairworkloads). Traffic that reaches a seat without the
// harness — a curl soak, opencode's own chat model, codex pointed at
// llama-swap — was invisible there, so fleet-serve now watches the seats'
// live request counts and reports that traffic too (pairworkloads.SeatWatcher).
// A seat's count cannot tell a harness request from a direct one, and a harness
// job already has its own card, so the watcher subtracts what this register
// holds: a harness request is never shown twice.
//
// WHY FILES. The harness runs as several processes on one box (an MCP server
// per editor session, CLI runs, fleet-serve serving another box's delegation);
// modelaffinity's gate is per-process by design and cannot count them. A
// marker is one create and one remove per request, never a lock, never a read
// on the request path — against a seat call measured in seconds.
//
// SAFE BY CONSTRUCTION. Nothing here can fail or slow a request: every error
// is swallowed, and an unarmed register is a no-op. A marker left behind by a
// process that died is ignored (and removed) once its pid is gone, and any
// marker older than MaxAge is ignored regardless, so a leak can only ever hide
// direct traffic for a while — never invent a card.
package seatinflight

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// dirName is the register's directory under the machine-wide state root.
const dirName = "seat-inflight"

// MaxAge bounds how long a marker is believed. The longest harness request is
// a delegation's 900 s cap; an agent_run step or a proxied chat is shorter.
// Past this a marker is a leak whatever its pid says (a recycled pid reads as
// alive).
const MaxAge = time.Hour

var (
	mu  sync.RWMutex
	dir string // "" = unarmed
	seq atomic.Int64
)

// Arm points the register at the machine-wide state root the operator's
// state_dir resolves to (the same root the GPU lease uses), and is called from
// config.Load beside modelaffinity.SetGPULease. A root that cannot be resolved
// disarms and returns the reason.
//
// enabled is the box's pair_seat_activity_enabled: with the feature off NOTHING
// reads this register, so nothing should pay for it. modelaffinity.Admit is the
// gate EVERY text call passes, and a file create plus a remove per request for a
// consumer that does not exist is exactly the cost that gate refuses to carry.
func Arm(stateDir string, enabled bool) error {
	if !enabled {
		Disarm()
		return nil
	}
	root, err := gpulease.ResolveStateRoot(stateDir)
	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		dir = ""
		return err
	}
	dir = filepath.Join(root, dirName)
	return nil
}

// ArmAt points the register at an explicit directory (tests).
func ArmAt(d string) {
	mu.Lock()
	dir = d
	mu.Unlock()
}

// Disarm turns the register off: a process whose endpoint is another box's
// llama-swap sends nothing to this box's seats and must not mark them.
func Disarm() { ArmAt("") }

// Dir is the armed directory, "" when unarmed.
func Dir() string {
	mu.RLock()
	defer mu.RUnlock()
	return dir
}

type marker struct {
	Model   string `json:"model"`
	PID     int    `json:"pid"`
	Started int64  `json:"started_ms"`
}

// Begin records one request naming model (the name the request carries; an
// alias is fine, the reader resolves it) and returns the function that ends
// it. The returned function is safe to call more than once. An unarmed
// register or an empty model returns a no-op.
func Begin(model string) (end func()) {
	d := Dir()
	model = strings.TrimSpace(model)
	if d == "" || model == "" {
		return func() {}
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		return func() {}
	}
	now := time.Now()
	path := filepath.Join(d, fmt.Sprintf("%d-%d-%d.json", os.Getpid(), now.UnixNano(), seq.Add(1)))
	body, _ := json.Marshal(marker{Model: model, PID: os.Getpid(), Started: now.UnixMilli()})
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return func() {}
	}
	var once sync.Once
	return func() { once.Do(func() { remove(path) }) }
}

// removeRetries and removeBackoff bound the retry below.
const (
	removeRetries = 6
	removeBackoff = 50 * time.Millisecond
)

// removeFn is os.Remove, a var so the retry can be tested without provoking a
// real sharing violation.
var removeFn = os.Remove

// remove deletes a marker, retrying in the background when the first attempt
// fails.
//
// WINDOWS, AND IT IS NOT THEORETICAL. os.ReadFile opens without
// FILE_SHARE_DELETE and os.Remove is one DeleteFile with no retry, so a marker
// being read by the watcher's Count at the instant its owner ends the request
// fails to delete. Dropping that error would leave a marker claiming a harness
// request is in flight until MaxAge — an hour of direct traffic hidden from
// PAIR, caused by the watcher's own 2 s read loop rather than by any crash. The
// collision window is sub-millisecond, so a few backed-off retries close it.
// The retry is off the caller's path: Release is on every text request.
func remove(path string) {
	if err := removeFn(path); err == nil || os.IsNotExist(err) {
		return
	}
	go func() {
		for i := 0; i < removeRetries; i++ {
			time.Sleep(removeBackoff)
			if err := removeFn(path); err == nil || os.IsNotExist(err) {
				return
			}
		}
	}()
}

// Count returns the live markers in d per lower-cased model name. A marker
// whose process is gone is removed; one older than MaxAge is ignored and
// removed; an unreadable one (being written right now) is skipped. alive is
// the liveness test (gpulease.PIDAlive in production).
func Count(d string, now time.Time, alive func(pid int) bool) map[string]int {
	out := map[string]int{}
	if d == "" {
		return out
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(d, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var m marker
		if json.Unmarshal(raw, &m) != nil || m.Model == "" {
			continue
		}
		if now.Sub(time.UnixMilli(m.Started)) > MaxAge || (alive != nil && !alive(m.PID)) {
			_ = os.Remove(path)
			continue
		}
		out[strings.ToLower(m.Model)]++
	}
	return out
}

// CountLive is Count on the armed directory with the lease's liveness rule.
func CountLive() map[string]int { return Count(Dir(), time.Now(), gpulease.PIDAlive) }
