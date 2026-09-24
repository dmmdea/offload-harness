package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

// A PAIR card for a job run under `gpu reserve -- <cmd>` (0.140.6).
//
// The lease queue is where the house runs every bench, render and measurement
// (CLAUDE.md: any GPU job on any node goes through `gpu reserve`), and none of
// it reached PAIR: on 2026-09-23 the Aorus ran a seat bench and a ComfyUI
// diagnostic and the Lenovo a Wan 2.2 smoke render, each at 84-100 % on its
// card, and the Jobs list showed only the Qube's delegations. The card is
// queued while the job waits in the lease queue and drains the seat, running
// once the command starts, and closed with the command's exit. A one-shot
// harness verb run directly under the lease gets pairworkloads.UnderLeaseEnv,
// so it adds no card of its own (silencesWrapped); anything else keeps its own
// reporting.
type leaseCard struct {
	pair      *pairworkloads.Emitter
	jobID     string
	model     string
	engine    string
	requester string
	created   int64

	mu      sync.Mutex
	started int64
	closed  bool
}

// harnessExes are the names the harness binary goes by on the fleet.
var harnessExes = map[string]bool{"local-offload": true, "offload-harness": true, "local-offload-fleet": true}

// longRunningVerbs are harness verbs that serve for hours: an MCP session or a
// fleet node wrapped by `gpu reserve … -- <session>` (the documented way to
// hold a lease around a session) must keep reporting its own calls, so the
// lease never silences them (review finding, 0.140.6).
var longRunningVerbs = map[string]bool{"mcp": true, "fleet-serve": true, "fleet-ui": true, "rig": true, "top": true, "loupe": true}

// silencesWrapped reports whether the lease card stands for the wrapped
// command's own card: only a ONE-SHOT harness verb run directly (`gpu reserve
// -- local-offload generate-video …`). A shell, an interpreter or a
// long-running harness process keeps its own reporting — an env flag set on
// those would be inherited by everything they start, for as long as they run.
func silencesWrapped(args []string) bool {
	if len(args) == 0 {
		return false
	}
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(strings.ReplaceAll(args[0], `\`, "/"))), ".exe")
	if !harnessExes[base] {
		return false
	}
	// The harness dispatches on its first argument (main.go): that is the verb.
	return len(args) > 1 && args[1] != "" && !strings.HasPrefix(args[1], "-") && !longRunningVerbs[args[1]]
}

// scriptExts mark the argument that names what an interpreter runs.
var scriptExts = []string{".ps1", ".py", ".mjs", ".js", ".sh", ".bat", ".cmd"}

// leaseCardIdentity names the card for a wrapped command: a harness verb
// (`generate-video`) on the engine that verb runs, else the script an
// interpreter runs (`seatbench.ps1`) or the program itself, on the engine
// "gpu-lease" (PAIR keeps an engine it does not know and shows it generically).
func leaseCardIdentity(args []string) (model, engine string) {
	if len(args) == 0 {
		return "gpu-lease", "gpu-lease"
	}
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(args[0], `\`, "/")))
	base = strings.TrimSuffix(base, ".exe")
	if harnessExes[base] {
		// The harness dispatches on its first argument (main.go), so that is the verb.
		if len(args) > 1 && args[1] != "" && !strings.HasPrefix(args[1], "-") {
			return args[1], pairworkloads.EngineFor(strings.ReplaceAll(args[1], "-", "_"), "")
		}
		return base, "gpu-lease"
	}
	for _, a := range args[1:] {
		la := strings.ToLower(a)
		for _, ext := range scriptExts {
			if strings.HasSuffix(la, ext) {
				return filepath.Base(strings.ReplaceAll(a, `\`, "/")), "gpu-lease"
			}
		}
	}
	return base, "gpu-lease"
}

// newLeaseCard opens the queued card. It returns nil (every method is then a
// no-op) when PAIR reporting is off on this box.
func newLeaseCard(cfg config.Config, args []string, origin string) *leaseCard {
	pair := pairworkloads.New(pairworkloads.FromConfig(cfg))
	if !pair.Enabled() {
		return nil
	}
	model, engine := leaseCardIdentity(args)
	who := strings.TrimSpace(origin)
	if who == "" {
		who = ledger.ProcessOrigin().Session
	}
	now := time.Now().UnixMilli()
	c := &leaseCard{pair: pair, jobID: fmt.Sprintf("lease-%d-%d", now, os.Getpid()),
		model: model, engine: engine, requester: pairworkloads.Requester(who), created: now}
	c.pair.Emit(pairworkloads.Event{JobID: c.jobID, Model: c.model, Engine: c.engine, State: "queued",
		Requester: c.requester, CreatedAt: c.created})
	return c
}

// running marks the wrapped command started.
func (c *leaseCard) running() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.closed || c.started != 0 {
		c.mu.Unlock()
		return
	}
	c.started = time.Now().UnixMilli()
	started := c.started
	c.mu.Unlock()
	c.pair.Emit(pairworkloads.Event{JobID: c.jobID, Model: c.model, Engine: c.engine, State: "running",
		Requester: c.requester, CreatedAt: c.created, StartedAt: started})
}

// finish closes the card, once: err nil = completed, else failed with err's
// text. It waits for the frames to leave, since the process may exit next.
func (c *leaseCard) finish(err error) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	started := c.started
	c.mu.Unlock()
	state, errText := "completed", ""
	if err != nil {
		state, errText = "failed", err.Error()
	}
	c.pair.Emit(pairworkloads.Event{JobID: c.jobID, Model: c.model, Engine: c.engine, State: state, Error: errText,
		Requester: c.requester, CreatedAt: c.created, StartedAt: started, CompletedAt: time.Now().UnixMilli()})
	c.pair.Wait()
}
