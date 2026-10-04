package main

import (
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// The lease reader a fleet node publishes its health from (GPU routing P7) reads the same
// inspection the delegator makes of the local box (modelaffinity.PeekLease): the effective
// cards of every live lease, declared or inferred, and nothing written. It used to be a bare
// InspectDir, which never fills the inferred scope, so a legacy lease the harness had scoped by
// evidence was published as the whole node while the box's own gates treated it as scoped.
func TestFleetLeaseReaderPublishesTheLeasesTheBoxGatesOn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lease")
	cfg := config.Config{GPULockPath: dir}
	read := fleetLeaseReader(cfg)
	if info := read(); info.Held {
		t.Fatalf("an empty lease directory reads %+v, want nothing held", info)
	}

	m, err := gpulease.OpenAt(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	a, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render A", Devices: []string{"gpu-aaaa0000"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Release() }()
	c, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "bench C", Devices: []string{"gpu-cccc0000"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Release() }()

	info := read()
	if !info.Held || len(info.Each()) != 2 {
		t.Fatalf("two disjoint leases read %+v, want both live", info)
	}
	got := map[string]string{}
	for _, l := range info.Each() {
		eff := l.EffectiveDevices()
		if len(eff) != 1 {
			t.Fatalf("lease %d effective devices = %v, want its one declared card", l.Epoch, eff)
		}
		got[eff[0]] = string(l.Class)
		if l.ScopeKind() != gpulease.ScopeDeclared {
			t.Fatalf("lease %d scope = %s, want declared", l.Epoch, l.ScopeKind())
		}
	}
	if got["gpu-aaaa0000"] != "media" || got["gpu-cccc0000"] != "text" {
		t.Fatalf("cards by class = %v", got)
	}
}

// The difference from a bare InspectDir: a legacy whole-node lease the harness has scoped by
// evidence is published with the cards it was scoped to, the same reading the box's own gates make.
func TestFleetLeaseReaderPublishesTheInferredScopeOfALegacyLease(t *testing.T) {
	_, m := legacyLeaseWithSidecar(t, `{"epoch":EPOCH,"devices":["gpu-cccc0000-x"],"source":"command line"}`)
	modelaffinity.SetLegacyInference(true)
	t.Cleanup(func() { modelaffinity.SetLegacyInference(false) })
	info := fleetLeaseReader(config.Config{StateDir: m.Root()})()
	// The display card rides along while the operator may be at the desk (plan I6), so the set
	// is the inferred card plus that one, and it must contain the card the evidence named.
	eff := info.EffectiveDevices()
	named := false
	for _, id := range eff {
		named = named || id == "gpu-cccc0000-x"
	}
	if !info.Held || !named || len(eff) > 2 || info.ScopeKind() != gpulease.ScopeInferred {
		t.Fatalf("effective devices = %v scope = %s: a scoped legacy lease must be published with its inferred card", eff, info.ScopeKind())
	}
	if len(info.Devices) != 0 {
		t.Fatalf("the record itself still declares no cards: %v", info.Devices)
	}
}

// An unresolvable lease directory reads as nothing held: the node fails toward serving, as it
// always did.
func TestFleetLeaseReaderFailsTowardServing(t *testing.T) {
	read := fleetLeaseReader(config.Config{GPULockPath: string([]byte{0})})
	if info := read(); info.Held {
		t.Fatalf("an unreadable lease directory reads %+v, want nothing held", info)
	}
}
