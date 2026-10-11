package gpulease

// F58 (b): the grant asks WHICH CARDS first and the host's memory second, on both grant paths. Every test of the host-RAM
// term used a free card, so swapping the two checks left them all green; the order is the contract three things rest on:
// a refusal on a held card says the card is held (and not that memory is short, which sends the operator to --ram), the
// waiter on a held card is not marked "waiting for host RAM" (that mark is what lets a call that declares none pass it,
// BlocksArrival), and a refused grant spends nothing either way.

import (
	"errors"
	"testing"
	"time"
)

func TestAHeldCardIsReportedHeldNotShortOfHostRAM(t *testing.T) {
	t.Run("card-scoped", func(t *testing.T) {
		m, _ := ramScoped(t, 70) // 70 committed of 100: a 30 GiB declaration does not fit
		holder, err := m.TryAcquire(ClassMedia, Options{Reason: "holder", TTL: time.Hour, Devices: []string{card0}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Release() }()
		_, err = m.TryAcquire(ClassMedia, Options{Reason: "second", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 30})
		var held *ErrHeld
		var short *ErrHostRAM
		if !errors.As(err, &held) || errors.As(err, &short) {
			t.Fatalf("the card is held, so the refusal is ErrHeld and not a host-RAM wait, got %T: %v", err, err)
		}
		// Control: the same request on a free card IS the host's refusal, so the test can tell the two apart.
		_, err = m.TryAcquire(ClassMedia, Options{Reason: "other card", TTL: time.Hour, Devices: []string{card1}, HostRAMGiB: 30})
		asHostRAM(t, err)
	})
	// The cheap held-probe at the top of the grant answers a card that is held when the request arrives. The order INSIDE the
	// grant is reached only when the card is taken in the window between that probe and the epoch lock; a hook lands it there.
	t.Run("card-scoped, taken between the probe and the lock", func(t *testing.T) {
		m, _ := ramScoped(t, 70)
		var rival *Lease
		m.afterHostReadHook = func() {
			m.afterHostReadHook = nil
			rival, _ = m.TryAcquire(ClassMedia, Options{Reason: "rival", TTL: time.Hour, Devices: []string{card0}})
		}
		_, err := m.TryAcquire(ClassMedia, Options{Reason: "second", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 30})
		if rival == nil {
			t.Fatal("the rival never took the card")
		}
		defer func() { _ = rival.Release() }()
		var held *ErrHeld
		var short *ErrHostRAM
		if !errors.As(err, &held) || errors.As(err, &short) {
			t.Fatalf("the card was taken first, so the grant says it is held and not that memory is short, got %T: %v", err, err)
		}
	})
	t.Run("whole node", func(t *testing.T) {
		m, _ := ramScoped(t, 70)
		holder, err := m.TryAcquire(ClassMedia, Options{Reason: "holder", TTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Release() }()
		_, err = m.TryAcquire(ClassMedia, Options{Reason: "second", TTL: time.Hour, HostRAMGiB: 30})
		var held *ErrHeld
		var short *ErrHostRAM
		if !errors.As(err, &held) || errors.As(err, &short) {
			t.Fatalf("the node is held, so the refusal is ErrHeld and not a host-RAM wait, got %T: %v", err, err)
		}
	})
}

// A waiter on a card that is held is waiting for the card; it is marked as waiting for host RAM only once the card is free
// and the memory is what is short.
func TestAWaiterOnAHeldCardIsNotMarkedWaitingForHostRAM(t *testing.T) {
	m, _ := ramScoped(t, 70)
	holder, err := m.TryAcquire(ClassMedia, Options{Reason: "holder", TTL: time.Hour, Devices: []string{card0}})
	if err != nil {
		t.Fatal(err)
	}
	cancel := make(chan struct{})
	res := make(chan error, 1)
	go func() {
		l, aerr := m.Acquire(ClassMedia, Options{Reason: "waiter", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 30, Wait: time.Minute, WaitOut: true, Cancel: cancel})
		if aerr == nil {
			_ = l.Release()
		}
		res <- aerr
	}()
	waitAllRegistered(t, m, 1, 5*time.Second)
	time.Sleep(60 * time.Millisecond) // several polls of the held card
	if ws := m.Waiters(); len(ws) != 1 || ws[0].WaitingFor != "" {
		t.Fatalf("a waiter on a HELD card waits for the card, not for host RAM: %+v", ws)
	}
	_ = holder.Release()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ws := m.Waiters(); len(ws) == 1 && ws[0].WaitingFor == WaitHostRAM {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("once the card is free and the memory is short the waiter is marked waiting for host RAM: %+v", m.Waiters())
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(cancel)
	if aerr := <-res; !errors.Is(aerr, ErrCancelled) {
		t.Fatalf("cancelled: %v", aerr)
	}
}
