package pipeline

// The contract of MediaLaneFree over COMBINATIONS of lane states. laneCases() lists the states one at a time; a review
// drove the real admission over random combinations of them and found a state the table could not have: a call that
// resumes its own place in line, on the whole-node plan, with a caller that joined the line after that place (the plan
// queues by arrival time, the prober counted the directory). Combinations are where a relaxation built from several
// readers breaks, so this keeps the random differential: fixed seeds, so a failure names a state that can be replayed
// (go test -run 'TestMediaLaneFreeNeverRefusesAGrantOverRandomStates/state-NNN'; the check that the states as a whole
// include both outcomes runs only when every state ran, so a replay of one reports that one and nothing else).

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// randomLaneIterations is how many random states the differential drives. The review ran 1,500 in a scratch copy; this
// is the share a unit run can afford (each state builds a fixture and drives the real admission once).
const randomLaneIterations = 150

func TestMediaLaneFreeNeverRefusesAGrantOverRandomStates(t *testing.T) {
	A, C := leaseIDOf(admitUUIDA), leaseIDOf(admitUUIDC)
	specs := []struct {
		name string
		spec admitSpec
	}{
		{"auto", admitSpec{order: admitOrder}},
		{"pinned", admitSpec{order: admitOrder, mutate: pinA}},
		{"krea2-auto", admitSpec{order: admitOrder, mutate: krea2Binding}},
		{"krea2-pinned", admitSpec{order: admitOrder, mutate: pinAKrea2}},
		{"whole (no card leases)", admitSpec{order: admitOrder, noAudit: true}},
		{"whole (a pin that names several cards)", admitSpec{order: admitOrder, mutate: pinSeveral}},
		{"whole-krea2 (a pin that names several cards)", admitSpec{order: admitOrder, mutate: func(c *config.Config) { krea2Binding(c); pinSeveral(c) }}},
	}
	var granted, busy, free, ran int
	for i := 0; i < randomLaneIterations; i++ {
		i := i
		t.Run(fmt.Sprintf("state-%03d", i), func(t *testing.T) {
			ran++
			rng := rand.New(rand.NewSource(int64(52000 + i)))
			pick := func(p float64) bool { return rng.Float64() < p }
			sp := specs[rng.Intn(len(specs))]
			f := newAdmitFixtureWith(t, sp.spec)
			f.away = pick(0.3)
			var desc []string

			switch rng.Intn(4) { // 0 roomy, 1 short(95), 2 impossible, 3 short(40)
			case 1:
				desc = append(desc, "host short(95)")
				f.useHost(t, func() gpuprobe.HostMemory { return shortHost(95) })
			case 2:
				desc = append(desc, "host impossible")
				f.useHost(t, func() gpuprobe.HostMemory {
					return gpuprobe.HostMemory{PhysicalGiB: 20, AvailableGiB: 15, CommitUsedGiB: 5, CommitLimitGiB: 40}
				})
			case 3:
				desc = append(desc, "host short(40)")
				f.useHost(t, func() gpuprobe.HostMemory { return shortHost(40) })
			default:
				if pick(0.35) {
					desc = append(desc, "host-RAM waiter on A")
					startHostRAMWaiter(t, f, admitUUIDA)
				}
			}
			wholeLease := false
			if pick(0.12) {
				desc = append(desc, "whole-node lease")
				if l, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "whole", Origin: "test", TTL: time.Hour}); err == nil {
					wholeLease = true
					t.Cleanup(func() { _ = l.Release() })
				}
			}
			if !wholeLease {
				if pick(0.30) {
					desc = append(desc, "lease A")
					if l, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "lease A", Origin: "test", TTL: time.Hour, Devices: []string{A}}); err == nil {
						t.Cleanup(func() { _ = l.Release() })
					}
				}
				if pick(0.30) {
					desc = append(desc, "lease C")
					cls := gpulease.ClassMedia
					if pick(0.3) {
						cls = gpulease.ClassText
					}
					opts := gpulease.Options{Reason: "lease C", Origin: "test", TTL: time.Hour, Devices: []string{C}}
					if pick(0.4) {
						opts.HostRAMGiB = 32.8
						desc = append(desc, "(declares 32.8)")
					}
					if l, err := f.m.TryAcquire(cls, opts); err == nil {
						t.Cleanup(func() { _ = l.Release() })
					}
				}
			}
			if pick(0.2) {
				desc = append(desc, "slot A")
				if mediaSlots.tryTake([]string{A}) {
					t.Cleanup(func() { mediaSlots.release([]string{A}) })
				}
			}
			if pick(0.2) {
				desc = append(desc, "slot C")
				if mediaSlots.tryTake([]string{C}) {
					t.Cleanup(func() { mediaSlots.release([]string{C}) })
				}
			}
			leave := func(devices ...string) {
				_, _ = f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: devices}, time.Now())
			}
			if pick(0.2) {
				desc = append(desc, "token A")
				leave(A)
			}
			if pick(0.15) {
				desc = append(desc, "token C")
				leave(C)
			}
			if pick(0.08) {
				desc = append(desc, "whole-node token")
				leave()
			}
			if pick(0.2) {
				desc = append(desc, "seat waiter A")
				seatWaiterAt(t, f.root, "transcribe a.wav", A)
			}
			if pick(0.15) {
				desc = append(desc, "seat waiter C")
				seatWaiterAt(t, f.root, "transcribe c.wav", C)
			}
			if pick(0.06) {
				desc = append(desc, "whole-node seat waiter")
				seatWaiterAt(t, f.root, "transcribe all.wav")
			}
			params := map[string]any{}
			if pick(0.25) {
				// The call's own place, left a moment ago: every caller above that joined the line after it is behind it on a
				// plan that queues by arrival time, and a caller above that is older than it is ahead.
				devices := []string{A}
				if pick(0.5) {
					devices = nil
				}
				age := 5 * time.Second
				if pick(0.3) {
					age = 0
				}
				if tok, err := f.m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: devices}, time.Now().Add(-age)); err == nil {
					params["waiter_token"] = tok.ID
					desc = append(desc, fmt.Sprintf("own place (devices %v, %s old)", devices, age))
				}
			}

			time.Sleep(6 * time.Millisecond) // millisecond-resolution arrival stamps: the setup strictly precedes the call
			req := laneReq(params)
			v := f.p.MediaLaneFree(context.Background(), req)
			cfg, _, err := f.p.cfg.ResolveImageFamily("")
			if err != nil {
				t.Fatal(err)
			}
			g, gerr := f.p.acquireMediaLease(context.Background(), "image-gen", time.Hour, 0, imageNeed(cfg, paramStr(params, "waiter_token")).resumableBy(req))
			ok := gerr == nil
			if ok {
				g.Release()
				granted++
			}
			if v.Free {
				free++
			} else {
				busy++
			}
			if ok && !v.Free {
				t.Errorf("THE CONTRACT IS BROKEN: %s, operator away=%v, state %v: the probe said busy (%s) and the real wait-0 admission GRANTED", sp.name, f.away, desc, v.Why)
			}
		})
	}
	t.Logf("%d states: the real admission granted %d; the probe called %d busy and %d free", ran, granted, busy, free)
	// Under a -run filter only some states ran, and a handful of states need not hold both outcomes.
	if ran == randomLaneIterations && (granted == 0 || busy == 0) {
		t.Errorf("the random states must include both outcomes or they test nothing: granted %d, busy %d", granted, busy)
	}
}
