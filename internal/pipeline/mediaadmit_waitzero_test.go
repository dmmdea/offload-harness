package pipeline

// G2 of the P0 plan: the guard's refusal sentence survives a call that cannot wait.
//
// A cluster placement (and `offload_*` with gpu_wait_ms 0) asks the node for ONE gated try. When the cards are
// free and only the host's memory is short, the grant refuses with the sentence the operator reads ("waiting for
// host RAM: needs N GiB, committed X of Y GiB physical, Z GiB headroom"). That sentence is the node's verdict:
// every queued answer that explains itself, and a placer that quotes the node, depends on it reaching the caller.
// On a call that had waited, hostRAMAnswer carried it; on a call that could not wait, the pinned and the auto plan
// fell through the queue step with held=nil and said "promised to callers ahead of this one", which is false and
// names nobody. Each shape of plan below is asked with a wait of 0.

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// waitZeroSentence is what the grant says for the krea2 lane (32.8 GiB declared on a 16 GiB card) on a
// 100 GiB host with 95 GiB committed and the default 8 GiB headroom.
const waitZeroSentence = "waiting for host RAM: needs 32.8 GiB, committed 95.0 of 100.0 GiB physical, 8.0 GiB headroom"

// noWait binds the krea2 lane and a wait of zero: one gated try, no queue.
func noWait(extra func(*config.Config)) func(*config.Config) {
	return func(c *config.Config) {
		krea2Binding(c)
		c.GPUWaitMs = 0
		if extra != nil {
			extra(c)
		}
	}
}

// grantSeesShortHost makes the GRANT see a host that cannot take the lane while the allocator's pre-filter
// sees a roomy one: the cards qualify, the lease grant is what refuses.
func grantSeesShortHost(t *testing.T) {
	t.Helper()
	restore := gpuprobe.UseHostMemoryReader(func() (gpuprobe.HostMemory, bool) { return shortHost(95), true })
	t.Cleanup(restore)
}

// wantSentenceQueued is the answer a door that can resume gets: a place in line whose reason is the guard's
// sentence, nothing started, and no claim of a card being held or promised to anyone.
func wantSentenceQueued(t *testing.T, f *admitFixture, res core.Result) {
	t.Helper()
	if res.OK || res.Meta.ErrClass != "gpu_queued" {
		t.Fatalf("want a queued answer, got ok=%v class=%q: %s", res.OK, res.Meta.ErrClass, res.Reason)
	}
	if !strings.Contains(res.Reason, waitZeroSentence) {
		t.Errorf("the reason must be the guard's sentence %q, got: %s", waitZeroSentence, res.Reason)
	}
	for _, wrong := range []string{"promised to callers ahead", "is in use", "held by"} {
		if strings.Contains(res.Reason, wrong) {
			t.Errorf("the cards are free; the answer must not say %q: %s", wrong, res.Reason)
		}
	}
	if n := len(f.started()); n != 0 {
		t.Errorf("%d runner(s) started on a host that cannot take them", n)
	}
}

// PINNED: the caller named the card (comfy_cuda_device), nothing to choose. The gated try is refused for host
// RAM, the call goes to the queue step with no time left, and answers.
func TestWaitZeroKeepsTheGuardsSentenceOnAPinnedPlan(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: noWait(func(c *config.Config) { c.ComfyCudaDevice = "2" })})
	grantSeesShortHost(t)
	res := f.await(f.image(nil))
	wantSentenceQueued(t, f, res)
	if p := wantQueued(t, res); len(p.Devices) != 1 || p.Devices[0] != leaseIDOf(admitUUIDC) {
		t.Errorf("the place is for the pinned card: %v", p.Devices)
	}
}

// AUTO, the allocator saw room and the grant did not: the same fall-through as the pinned plan.
func TestWaitZeroKeepsTheGuardsSentenceOnAnAutoPlanTheGrantRefuses(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: noWait(nil)})
	grantSeesShortHost(t)
	res := f.await(f.image(nil))
	wantSentenceQueued(t, f, res)
}

// AUTO, the allocator's own pre-filter already reads the host as short (one host, one rule): no card
// qualifies, and the sentence comes through the "no card can take this call" answer.
func TestWaitZeroKeepsTheGuardsSentenceOnAnAutoPlanTheAllocatorRefuses(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: noWait(nil)})
	lowHostRAM(t, f)
	res := f.await(f.image(nil))
	if res.OK || res.Meta.ErrClass != "gpu_queued" {
		t.Fatalf("want a queued answer, got ok=%v class=%q: %s", res.OK, res.Meta.ErrClass, res.Reason)
	}
	if !strings.Contains(res.Reason, waitZeroSentence) {
		t.Errorf("the reason must carry the guard's sentence %q, got: %s", waitZeroSentence, res.Reason)
	}
	if strings.Contains(res.Reason, "promised to callers ahead") {
		t.Errorf("nobody is ahead: %s", res.Reason)
	}
}

// WHOLE NODE: a host that leases no cards. The grant refuses, the call is the plain busy defer, and it
// carries the sentence.
func TestWaitZeroKeepsTheGuardsSentenceOnTheWholeNode(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, noAudit: true, mutate: noWait(nil)})
	grantSeesShortHost(t)
	res := f.await(f.image(nil))
	if res.OK || res.Meta.ErrClass != "gpu_busy" {
		t.Fatalf("want the busy defer, got ok=%v class=%q: %s", res.OK, res.Meta.ErrClass, res.Reason)
	}
	if !strings.Contains(res.Reason, waitZeroSentence) {
		t.Errorf("the reason must be the guard's sentence %q, got: %s", waitZeroSentence, res.Reason)
	}
	if n := len(f.started()); n != 0 {
		t.Errorf("%d runner(s) started on a host that cannot take them", n)
	}
}

// A door that cannot resume a place (no token to hand back) gets the plain busy defer, and it carries the
// sentence too, on the pinned and on the auto plan.
func TestWaitZeroKeepsTheGuardsSentenceForADoorThatCannotResume(t *testing.T) {
	for name, extra := range map[string]func(*config.Config){
		"pinned": func(c *config.Config) { c.ComfyCudaDevice = "2" },
		"auto":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: noWait(extra)})
			grantSeesShortHost(t)
			res := f.p.Run(context.Background(), core.Request{Task: core.TaskGenerateImage, Input: "a calm ocean at dawn",
				Params: map[string]any{"out": f.dir + "/out-cli.png"}, Resumable: false})
			if res.OK || res.Meta.ErrClass != "gpu_busy" {
				t.Fatalf("want the busy defer, got ok=%v class=%q: %s", res.OK, res.Meta.ErrClass, res.Reason)
			}
			if !strings.Contains(res.Reason, waitZeroSentence) {
				t.Errorf("the reason must be the guard's sentence %q, got: %s", waitZeroSentence, res.Reason)
			}
			if n := len(f.m.Tokens()); n != 0 {
				t.Errorf("a door that cannot resume must leave no place behind, found %d token(s)", n)
			}
		})
	}
}
