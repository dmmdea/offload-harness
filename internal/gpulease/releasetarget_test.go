package gpulease

// ReleaseTarget names the lease ReleaseByEpoch would end, WITHOUT ending it. A caller that has to
// do something destructive on the lease's behalf first (`gpu release` stops the ComfyUI instances
// kept under it) asks here and acts only on the epoch this returns: if the release is going to be
// refused, nothing destructive has run. The rule is ReleaseByEpoch's own, so these tests compare
// the two across every shape of lease directory instead of restating the rule.

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// liveEpochSet is the epochs ReleaseByEpoch could act on right now.
func liveEpochSet(m *Manager) map[uint64]bool {
	out := map[uint64]bool{}
	for _, i := range m.Leases() {
		out[i.Epoch] = true
	}
	return out
}

func TestReleaseTargetAgreesWithReleaseByEpoch(t *testing.T) {
	type shape struct {
		name  string
		build func(t *testing.T) (*Manager, []uint64) // the manager and the epochs it holds
	}
	shapes := []shape{
		{"nothing held", func(t *testing.T) (*Manager, []uint64) { m, _ := scopedManager(t); return m, nil }},
		{"one whole-node lease", func(t *testing.T) (*Manager, []uint64) {
			m, _ := scopedManager(t)
			l, err := m.TryAcquire(ClassMedia, Options{Reason: "whole", TTL: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			return m, []uint64{l.Epoch()}
		}},
		{"one card lease", func(t *testing.T) (*Manager, []uint64) {
			m, _ := scopedManager(t)
			return m, []uint64{mustDevices(t, m, card0).Epoch()}
		}},
		{"two card leases", func(t *testing.T) (*Manager, []uint64) {
			m, _ := scopedManager(t)
			a, b := mustDevices(t, m, card0), mustDevices(t, m, card1)
			return m, []uint64{a.Epoch(), b.Epoch()}
		}},
	}
	for _, sh := range shapes {
		sh := sh
		// 0 = "whatever is held"; each held epoch by name; an epoch nobody holds.
		args := func(held []uint64) []uint64 { return append(append([]uint64{0}, held...), 9999) }
		_, held := sh.build(t)
		for _, arg := range args(held) {
			arg := arg
			t.Run(sh.name+"/epoch="+strconv.FormatUint(arg, 10), func(t *testing.T) {
				m, _ := sh.build(t)
				before := liveEpochSet(m)
				target, terr := m.ReleaseTarget(arg)
				if got := liveEpochSet(m); len(got) != len(before) {
					t.Fatalf("ReleaseTarget changed the lease directory: before %v after %v", before, got)
				}
				released, rerr := m.ReleaseByEpoch(arg)
				switch {
				case rerr != nil:
					if terr == nil || terr.Error() != rerr.Error() {
						t.Fatalf("ReleaseByEpoch refused with %q but ReleaseTarget said (%d, %v)", rerr, target, terr)
					}
					if target != 0 {
						t.Fatalf("a refused release has no target, got %d", target)
					}
				case released:
					if terr != nil {
						t.Fatalf("ReleaseByEpoch released but ReleaseTarget refused: %v", terr)
					}
					after := liveEpochSet(m)
					var gone []uint64
					for e := range before {
						if !after[e] {
							gone = append(gone, e)
						}
					}
					if len(gone) != 1 || gone[0] != target {
						t.Fatalf("ReleaseTarget named %d, ReleaseByEpoch ended %v", target, gone)
					}
				default:
					if terr != nil || target != 0 {
						t.Fatalf("nothing was released and no error, so there is no target; got (%d, %v)", target, terr)
					}
				}
			})
		}
	}
}

// Several card leases and no epoch is the operator's slip the refusal exists for; the target
// lookup must refuse it in the same words, before anything is touched.
func TestReleaseTargetRefusesAmbiguousEpochZero(t *testing.T) {
	m, _ := scopedManager(t)
	mustDevices(t, m, card0)
	mustDevices(t, m, card1)
	target, err := m.ReleaseTarget(0)
	if err == nil || !strings.Contains(err.Error(), "pass --epoch N") {
		t.Fatalf("want the ambiguity refusal, got (%d, %v)", target, err)
	}
	if target != 0 {
		t.Fatalf("a refusal names no target, got %d", target)
	}
}
