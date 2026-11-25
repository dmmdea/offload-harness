package modelaffinity

// callmark.go — one single-shot call on an engine that publishes no in-flight gauge (2026-11-25, ADR 0104).
//
// whisper-server and the forecast server answer neither /metrics nor /slots, so the only thing that can say a call
// is running on them is the process that sent it. Call.Run therefore marks the call in the machine-wide run registry
// (internal/gpuactivity) for exactly as long as the request is on the engine. A lease grant that unloads other models
// (internal/seatdrain) and the last transcription of a burst in another process (sttclient.UnloadIfIdle) both read
// these marks before they take the model down.
//
// THE ORDER IS THE PROTOCOL. A call marks itself AFTER the fence let it through and then looks at the fence once more.
// A holder names the models it is clearing on its lease (gpulease.Meta.Clearing) and then reads the marks. Whichever
// comes second sees the other, so a call is either waited for or sent back to the fence, and never both unseen and on
// the engine. Marking before the fence would be a deadlock: the lease would wait for a call that waits for the lease.

import (
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
)

// Call is one single-shot request to an engine that publishes no in-flight gauge, on its way through llama-swap's
// /upstream passthrough.
type Call struct {
	Kind     string        // gpuactivity.KindSTT or gpuactivity.KindForecast
	Endpoint string        // llama-swap's root, or any URL under it
	Model    string        // the model, by the name the caller was configured with
	Path     string        // the engine's own route
	Fence    time.Duration // how long the call may still wait for a fenced card; what a wait took is taken off it
	Slot     sync.Locker   // the engine's single slot, held from before the mark until send returns; nil for none
}

// Run waits at the fence, takes the slot, marks the call, looks at the fence once more and calls send with the URL. A
// lease that fenced the model between the wait and the mark sends the call back to wait, with what is left of Fence.
// The error is send's, or the fence's *LeaseError.
func (c *Call) Run(ctx context.Context, send func(url string) error) error {
	for {
		queued := time.Now()
		u, err := AwaitUpstream(ctx, c.Endpoint, c.Model, c.Path, queued.Add(c.Fence))
		c.Fence = max(0, c.Fence-time.Since(queued))
		if err != nil {
			return err
		}
		if sent, err := c.onEngine(ctx, u, send); sent {
			return err
		}
	}
}

// onEngine is Run past the fence; sent is false when the fence went up before the mark did.
func (c *Call) onEngine(ctx context.Context, url string, send func(url string) error) (sent bool, err error) {
	if c.Slot != nil {
		c.Slot.Lock()
		defer c.Slot.Unlock()
	}
	mark := beginCall(c.Kind, c.Model)
	defer mark.End()
	if WouldBlockUpstream(ctx, c.Endpoint, c.Model) {
		return false, nil
	}
	return true, send(url)
}

var callMarkLog sync.Once

// beginCall marks one call as running until the returned handle's End. The handle is nil, and every method on it a
// no-op, when the lease gate is not armed or the mark could not be written: a call is never failed over its own
// bookkeeping.
func beginCall(kind, model string) *gpuactivity.Handle {
	dir := gpuLeaseDir()
	if dir == "" {
		return nil
	}
	h, err := gpuactivity.OpenAt(gpuactivity.DirBeside(dir)).Begin(gpuactivity.Run{Seat: model, Kind: kind, Phase: gpuactivity.PhaseRunning})
	if err != nil {
		callMarkLog.Do(func() {
			log.Printf("modelaffinity: a %s on %s could not be marked as running (%v): a GPU lease that clears the cards cannot see it (said once per process)", kind, model, err)
		})
		return nil
	}
	return h
}

// CallsElsewhere counts the calls of kind that OTHER processes on this machine have marked as running on model.
func CallsElsewhere(kind, model string) int {
	dir := gpuLeaseDir()
	if dir == "" {
		return 0
	}
	pid := os.Getpid()
	return len(gpuactivity.OpenAt(gpuactivity.DirBeside(dir)).Where(time.Now(), func(r gpuactivity.Run) bool {
		return r.PID != pid && r.Kind == kind && strings.EqualFold(r.Seat, model)
	}))
}
