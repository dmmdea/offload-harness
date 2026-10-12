package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpualloc"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// A request queued on the display card was decided against the card's cap and the VRAM in use at
// ENQUEUE. FIFO can be hours long; the grant has to put the same rule again, from fresh readings, or the
// queue hands the screen's card to a job the cap would now refuse.

// grantBox is a two-card box whose card 0 is the operator's screen (the lower id, so it is the first
// card a request queues on), with the display card's cap and its free VRAM as variables the test moves
// while the request waits (the operator closing the card, a game growing on it). The cap is moved the way
// the operator moves it: by writing the config file. The Config the request holds was read when it started
// and is never touched, as in the reserve verb.
type grantBox struct {
	cards   []gpuprobe.Card
	m       *gpulease.Manager
	cfg     config.Config
	cfgPath string
	mu      sync.Mutex
	free0   float64
}

// setCap writes gpu_display_cap_pct to the config file, as the operator does.
func (b *grantBox) setCap(t *testing.T, pct int) {
	t.Helper()
	if err := os.WriteFile(b.cfgPath, []byte(`{"gpu_display_cap_pct": `+strconv.Itoa(pct)+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newGrantBox(t *testing.T) *grantBox {
	t.Helper()
	_, m := scopedLeaseFixture(t)
	cards, _ := gpuprobe.BuildCards([]gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16, DisplayActive: true},
		{Index: 1, UUID: "GPU-bbbb0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
	}, "")
	b := &grantBox{cards: cards, m: m, free0: 16, cfgPath: filepath.Join(t.TempDir(), "config.json")}
	b.setCap(t, 60)
	b.cfg = config.Config{}.WithSource(b.cfgPath) // what the verb's loadCfg holds: values and the file they came from
	return b
}

// build reads the box as the reserve verb does, through gpualloc.BuildInput: the leases that are live, the cap
// the config file says now and the VRAM in use now.
func (b *grantBox) build() (gpulease.AllocInput, error) {
	deps := gpualloc.Deps{
		Cards: func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
			b.mu.Lock()
			defer b.mu.Unlock()
			cards := append([]gpuprobe.Card(nil), b.cards...)
			cards[0].VRAMFreeGiB = b.free0
			return cards, "", nil
		},
		HostMemory: func() (gpuprobe.HostMemory, bool) { return roomyTestHost, true },
	}
	return gpualloc.BuildInput(context.Background(), b.m, b.cfg, gpualloc.Need{VRAMGiB: 2}, deps)
}

// fakeClock starts at the real time (the arrival time it hands the lease queue is compared with the real
// clock there) and advances only when the caller sleeps, so a request that polls for a card never waits.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *fakeClock) sleep(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

func holdCard(t *testing.T, m *gpulease.Manager, id string) *gpulease.Lease {
	t.Helper()
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "holder " + id, Devices: []string{id}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func waitForWaiter(t *testing.T, m *gpulease.Manager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(m.Waiters()) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the request never joined the line")
}

type reserveResult struct {
	lease *gpulease.Lease
	err   error
}

// queueOn starts a one-card auto request against the box and returns the channel its outcome lands on.
func (b *grantBox) queueOn(out *bytes.Buffer, clock *fakeClock) chan reserveResult {
	done := make(chan reserveResult, 1)
	go func() {
		l, err := acquireAutoCards(b.m, gpulease.ClassMedia, gpulease.Options{Reason: "queued render", TTL: time.Hour},
			devicePlan{Auto: true, Min: 1, Max: 1, Source: "--cards 1"}, time.Minute, b.build, out, clock.sleep, clock.now)
		done <- reserveResult{l, err}
	}()
	return done
}

// Open at enqueue, closed at the grant (the operator says he needs the card: gpu_display_cap_pct 0), and no
// other card to take: the display card is not granted, and the request keeps waiting for what qualifies
// (here its window runs out) instead of getting it.
func TestAQueuedLeaseIsNotGrantedTheDisplayCardOnceTheOperatorClosesIt(t *testing.T) {
	b := newGrantBox(t)
	b.cards = b.cards[:1] // a one-card box: the screen's card is the only one
	holder := holdCard(t, b.m, "gpu-aaaa0000-x")
	clock := &fakeClock{at: time.Now()}
	var out bytes.Buffer
	done := b.queueOn(&out, clock)
	waitForWaiter(t, b.m)
	b.setCap(t, 0) // the operator closes the card while the request waits
	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.lease != nil {
			devs := r.lease.Devices()
			_ = r.lease.Release()
			t.Fatalf("the screen's card was granted to a queued request after the operator closed it: %v", devs)
		}
		var none *gpulease.NoCardsError
		if !errors.As(r.err, &none) || !strings.Contains(r.err.Error(), "display") {
			t.Fatalf("the request is turned away for the display card, not granted: %v", r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the request neither got a card nor gave up")
	}
	if !strings.Contains(out.String(), "queueing") {
		t.Errorf("setup: with the cap open the display card is queued on: %q", out.String())
	}
	if !strings.Contains(out.String(), "no longer qualify") {
		t.Errorf("the refused grant is said once on entry to the second choice: %q", out.String())
	}
	if ls := b.m.Leases(); len(ls) != 0 {
		t.Fatalf("no lease may be left on the display card: %+v", ls)
	}
}

// The same, with a second card the request still fits: it is given that card, and never the screen's.
func TestAQueuedLeaseMovesToAFreeCardWhenTheDisplayCardNoLongerQualifies(t *testing.T) {
	b := newGrantBox(t)
	holdDisplay := holdCard(t, b.m, "gpu-aaaa0000-x")
	holdOther := holdCard(t, b.m, "gpu-bbbb0000-x") // both busy: the request queues on the first, the screen's
	clock := &fakeClock{at: time.Now()}
	var out bytes.Buffer
	done := b.queueOn(&out, clock)
	waitForWaiter(t, b.m)
	b.setCap(t, 0)
	if err := holdOther.Release(); err != nil { // the other card frees up first...
		t.Fatal(err)
	}
	if err := holdDisplay.Release(); err != nil { // ...and then the card the request is queued on
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("a card the request fits is free: it must be given that one: %v (%s)", r.err, out.String())
		}
		defer func() { _ = r.lease.Release() }()
		if got := strings.Join(r.lease.Devices(), ","); got != "gpu-bbbb0000-x" {
			t.Fatalf("the operator closed the screen's card, so it is not the one: got %s", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the request neither got a card nor gave up")
	}
}

// Open at both ends and under the cap: the grant is the plain one it always was.
func TestAQueuedLeaseStillGetsTheDisplayCardWhileTheCapStaysOpen(t *testing.T) {
	b := newGrantBox(t)
	b.cards = b.cards[:1]
	holder := holdCard(t, b.m, "gpu-aaaa0000-x")
	clock := &fakeClock{at: time.Now()}
	var out bytes.Buffer
	done := b.queueOn(&out, clock)
	waitForWaiter(t, b.m)
	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("cap open and under it: the queued request gets its card: %v", r.err)
		}
		_ = r.lease.Release()
	case <-time.After(10 * time.Second):
		t.Fatal("never granted")
	}
}

// The VRAM in use is re-read at the grant as well: a game that took the display card's memory while the
// request waited refuses the grant even though the cap is still open.
func TestAQueuedLeaseIsNotGrantedTheDisplayCardOnceTheCapIsPassed(t *testing.T) {
	b := newGrantBox(t)
	b.cards = b.cards[:1]
	holder := holdCard(t, b.m, "gpu-aaaa0000-x")
	clock := &fakeClock{at: time.Now()}
	var out bytes.Buffer
	done := b.queueOn(&out, clock)
	waitForWaiter(t, b.m)
	b.mu.Lock()
	b.free0 = 5 // 11 GiB in use + 2 needed = 13, over the 9.6 GiB a 60 percent cap allows on 16
	b.mu.Unlock()
	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.lease != nil {
			_ = r.lease.Release()
			t.Fatal("the display card is over its cap at the grant and must not be given")
		}
		if r.err == nil {
			t.Fatal("no lease and no error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("never returned")
	}
}

// The second choice keeps the place in line: the request that was refused the screen's card and queues
// on another busy card carries the arrival time it first joined with, not a new one.
func TestARefusedGrantKeepsTheArrivalTimeOfThePlaceInLine(t *testing.T) {
	b := newGrantBox(t)
	holdDisplay := holdCard(t, b.m, "gpu-aaaa0000-x")
	holdOther := holdCard(t, b.m, "gpu-bbbb0000-x")
	clock := &fakeClock{at: time.Now().Add(-time.Minute)} // joined a minute ago, by the clock the queue is handed
	var out bytes.Buffer
	done := b.queueOn(&out, clock)
	waitForWaiter(t, b.m)
	first := b.m.Waiters()[0].SinceMs
	if want := clock.now().UnixMilli(); first != want {
		t.Fatalf("setup: the first place in line carries the arrival time it was handed: %d, want %d", first, want)
	}
	b.setCap(t, 0)
	if err := holdDisplay.Release(); err != nil { // the card it queued on frees: the grant is refused
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var second int64
	for time.Now().Before(deadline) && second == 0 {
		// the refusal puts it back in line, now on the other (still busy) card
		for _, w := range b.m.Waiters() {
			if len(w.Devices) == 1 && w.Devices[0] == "gpu-bbbb0000-x" {
				second = w.SinceMs
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if second == 0 {
		t.Fatalf("the refused request never queued again on the other card: %s", out.String())
	}
	if second != first {
		t.Errorf("the place in line must keep its arrival time across the second choice: first %d, second %d", first, second)
	}
	if err := holdOther.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		_ = r.lease.Release()
	case <-time.After(10 * time.Second):
		t.Fatal("never granted")
	}
}

// A card that is free the moment the request asks is granted by acquireQueued's first try, which never
// reaches the queue: the grant check is made there as well.
func TestAnImmediateGrantIsVettedToo(t *testing.T) {
	b := newGrantBox(t)
	opts := gpulease.Options{Reason: "r", TTL: time.Hour, Devices: []string{"gpu-aaaa0000-x"},
		GrantCheck: func() error { return errors.New("the card no longer qualifies") }}
	lease, err := acquireQueued(b.m, gpulease.ClassMedia, opts, time.Second)
	var refused *gpulease.ErrGrantRefused
	if !errors.As(err, &refused) || lease != nil {
		t.Fatalf("a free card whose check refuses is not granted: lease=%v err=%v", lease, err)
	}
	if ls := b.m.Leases(); len(ls) != 0 {
		t.Fatalf("the refused grant is given back: %+v", ls)
	}
	opts.GrantCheck = func() error { return nil }
	lease, err = acquireQueued(b.m, gpulease.ClassMedia, opts, time.Second)
	if err != nil {
		t.Fatalf("a passing check is the plain grant: %v", err)
	}
	_ = lease.Release()
}
