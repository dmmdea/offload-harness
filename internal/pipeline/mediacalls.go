package pipeline

// mediacalls.go — a render waits for the calls that are running on the models it is about to unload (ADR 0104).
//
// A media lease fences its cards the moment it is granted, and the runner then unloads the resident models (render/
// gpu-lock.mjs freeLlamaSwap, after a drain that reads each engine's own in-flight gauge for up to 60 s; a lease that
// compares its host RAM after the clear unloads them here, in clearForHostRAM, with llama-swap's own drain). Neither can
// see a transcription or a forecast: whisper-server and the forecast server publish no gauge. Those calls are marked in
// the run registry (modelaffinity.Call), and this is the wait that reads the marks, before either unload.
//
// Only those calls are waited for. An agent run or a vision call can be registered while it waits for this very lease,
// and the engines' own gauges are the runner's drain.

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/seatdrain"
)

// mediaCallsFloor is the least a render waits for such a call, however little of its own window is left: what the
// runner's own drain allows the engines that have a gauge.
const mediaCallsFloor = 60 * time.Second

// awaitCallsOn names the resident models this lease unloads on the lease itself, so a new call to one of them queues at
// the fence, and waits until no transcription or forecast is running on any of them: until the call's own window ends
// (gpu_wait_ms from start), and mediaCallsFloor at least. A call that is still running then is not cut: the error is the
// busy answer, and the caller gives the cards back.
func (p *Pipeline) awaitCallsOn(ctx context.Context, lease *gpulease.Lease, unload string, start time.Time) error {
	if unload == unloadEnvNone || strings.TrimSpace(p.cfg.Endpoint) == "" {
		return nil
	}
	victims, known := p.residentUnloadSet(ctx, unload)
	if !known || len(victims) == 0 {
		return nil
	}
	reg, err := gpuactivity.Open(p.cfg.GPULockPath, p.cfg.StateDir)
	if err != nil {
		log.Printf("media admission: lease epoch %d: the run registry is not readable (%v): a transcription on a model this lease unloads is not waited for", lease.Epoch(), err)
		return nil
	}
	names := seatdrain.Names(ctx, nil, p.cfg.Endpoint, victims)
	if err := lease.Restamp(func(m *gpulease.Meta) { m.Clearing = names }); err != nil {
		log.Printf("media admission: lease epoch %d: the models it unloads could not be named on it (%v): a call that starts on one now is not held back", lease.Epoch(), err)
	}
	deadline := start.Add(p.gpuWait())
	if floor := time.Now().Add(mediaCallsFloor); floor.After(deadline) {
		deadline = floor
	}
	err = seatdrain.Wait(ctx, seatdrain.Spec{
		Endpoint: p.cfg.Endpoint, Models: victims, Registry: reg, Counts: seatdrain.OnEngine, Deadline: deadline,
		Say: func(line string) { log.Printf("media admission: lease epoch %d: %s", lease.Epoch(), line) },
	})
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return &errGPUBusy{detail: "the cards are free, but " + err.Error()}
}
