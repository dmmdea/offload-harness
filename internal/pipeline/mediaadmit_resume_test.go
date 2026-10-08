package pipeline

// A place in line is only worth keeping for a caller that can come back for it. The queued answer
// carries a waiter_token the caller must hand back, which only an MCP door can do; the CLI verb,
// the fleet-node dispatch (the delegator re-places, it never resumes) and the image batch have no
// such path. A token left for one of those is a ghost: it holds a card back from every later
// caller for the 30 s grace, and a node a delegator retries every few seconds would keep newcomers
// behind a rolling set of them. So a call that did not come through a door that can resume
// (core.Request.Resumable, set by the MCP server) gets the answer it always got, "gpu busy", and
// leaves nothing behind.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// plain starts a call as the CLI or the fleet dispatch would: a request no door can resume.
func (f *admitFixture) plain(task core.TaskType, input string, params map[string]any) <-chan core.Result {
	f.t.Helper()
	ch := make(chan core.Result, 1)
	go func() {
		ch <- f.p.Run(context.Background(), core.Request{Task: task, Input: input, Params: params, Resumable: false})
	}()
	return ch
}

func (f *admitFixture) plainImage() <-chan core.Result {
	return f.plain(core.TaskGenerateImage, "a calm ocean at dawn", map[string]any{"out": filepath.Join(f.dir, "plain.png")})
}

func busyNotQueued(t *testing.T, res core.Result) {
	t.Helper()
	if res.OK || res.Meta.ErrClass != "gpu_busy" {
		t.Fatalf("want the legacy busy answer, got ok=%v class=%q: %s", res.OK, res.Meta.ErrClass, res.Reason)
	}
	if res.Data != nil {
		t.Errorf("a busy answer carries no place in line: %s", res.Data)
	}
}

// A single-card call from a door that cannot resume.
func TestACallFromADoorThatCannotResumeGetsBusyAndLeavesNoPlace(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.ComfyCudaDevice = "2" }})
	holder, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "its card", TTL: time.Hour, Devices: []string{leaseIDOf(admitUUIDC)}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()

	busyNotQueued(t, f.await(f.plainImage()))
	if n := len(f.m.Tokens()); n != 0 {
		t.Fatalf("%d token(s) left by a call that can never resume one", n)
	}
	if n := len(f.m.Waiters()); n != 0 {
		t.Errorf("%d waiter record(s) left behind", n)
	}
}

// A whole-node call from a door that cannot resume.
func TestAWholeNodeCallFromADoorThatCannotResumeGetsBusyAndLeavesNoPlace(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	holder, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "another job", TTL: time.Hour, Devices: []string{leaseIDOf(admitUUIDA)}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()
	f.cfg.RunGraphScript = f.cfg.ImageGenScript
	f.p.cfg.RunGraphScript = f.cfg.ImageGenScript
	gp := filepath.Join(f.dir, "g.json")
	if err := os.WriteFile(gp, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	busyNotQueued(t, f.await(f.plain(core.TaskRunGraph, "", map[string]any{"graph_path": gp, "out_dir": f.dir})))
	if n := len(f.m.Tokens()); n != 0 {
		t.Fatalf("%d token(s) left by a call that can never resume one", n)
	}
}

// The scenario the ghosts cause: a delegator retries a busy node every few seconds. Each refused
// attempt used to leave a token that read as a live place for 30 s, so a newcomer arriving just
// after the card freed queued behind a caller who was never coming back.
func TestAGhostFromACallThatCannotResumeDoesNotHoldBackTheNextCaller(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.ComfyCudaDevice = "2" }})
	holder, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "its card", TTL: time.Hour, Devices: []string{leaseIDOf(admitUUIDC)}})
	if err != nil {
		t.Fatal(err)
	}
	busyNotQueued(t, f.await(f.plainImage())) // the delegator's attempt, refused
	_ = holder.Release()                      // the card frees

	f.letRunnersGo()
	if r := f.await(f.image(nil)); !r.OK { // a newcomer through a door that can resume
		t.Fatalf("the newcomer found the card free, yet it queued behind an absent caller: class=%q %s", r.Meta.ErrClass, r.Reason)
	}
}

// A door that can resume is unchanged: the place is kept and the token returned.
func TestACallFromADoorThatCanResumeStillGetsAPlace(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.ComfyCudaDevice = "2" }})
	holder, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "its card", TTL: time.Hour, Devices: []string{leaseIDOf(admitUUIDC)}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()
	res := f.await(f.image(nil))
	if res.Meta.ErrClass != "gpu_queued" || len(f.m.Tokens()) != 1 {
		t.Fatalf("want a queued answer and one place in line, got class=%q tokens=%d", res.Meta.ErrClass, len(f.m.Tokens()))
	}
}

// RunImageBatch (the CLI's batch verb) is such a caller: a busy card is a clean defer it can
// report, not a place it can never come back for.
func TestAnImageBatchOnABusyCardLeavesNoPlace(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) { c.ComfyCudaDevice = "2" }})
	holder, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "its card", TTL: time.Hour, Devices: []string{leaseIDOf(admitUUIDC)}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()

	_, err = f.p.RunImageBatch(context.Background(), []ImageBatchJob{{Prompt: "a calm ocean at dawn", Out: filepath.Join(f.dir, "b.png")}})
	if err == nil || !IsGPUBusy(err) {
		t.Fatalf("want a busy refusal, got %v", err)
	}
	var queued *errGPUQueued
	if errors.As(err, &queued) {
		t.Fatalf("a batch cannot resume a token, but was handed one: %v", err)
	}
	if n := len(f.m.Tokens()); n != 0 {
		t.Fatalf("%d token(s) left by a batch", n)
	}
}

// Every media door builds its need from the request's waiter_token, and the same line must carry
// the request's resumability: a door that forgot would silently stop keeping places for the MCP
// callers (or, built the other way, start leaving ghosts). The behavioural tests above
// reach image generation and run-graph; inpaint, upscale, edit, video, animate, audio and sd.cpp
// are pinned at the source: no line may read the token without also reading the door. The iGPU
// lanes (igpumedia.go) take their lease in one place, runIGPU, which TestAnIGPULaneKeepsAPlaceInLineAndResumesIt
// also pins by behaviour; the scan below keeps that file honest too, and a file that stops reading the
// token at all fails its minimum instead of passing with nothing to check.
func TestEveryMediaDoorThreadsTheRequestsResumability(t *testing.T) {
	for _, file := range []struct {
		name string
		min  int
	}{{"pipeline.go", 10}, {"igpumedia.go", 1}} {
		src, err := os.ReadFile(file.name)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for i, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, `paramStr(req.Params, "waiter_token")`) {
				continue
			}
			n++
			if !strings.Contains(line, ".resumableBy(req)") {
				t.Errorf("%s:%d reads the waiter_token but not the request's resumability:\n\t%s", file.name, i+1, strings.TrimSpace(line))
			}
		}
		if n < file.min {
			t.Errorf("found %d door(s) in %s reading the waiter_token, expected at least %d: the scan is looking for the wrong text, or a lane stopped threading the token", n, file.name, file.min)
		}
	}
}
