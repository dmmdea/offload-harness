package gpuprobe

import (
	"math"
	"strings"
	"testing"
)

// host128 is the shape of the incident box with round binary numbers (so the arithmetic below is
// exact): 128 GiB physical, a 192 GiB commit limit.
func host128(commitUsed float64) HostMemory {
	return HostMemory{PhysicalGiB: 128, AvailableGiB: 128 - commitUsed/2, CommitUsedGiB: commitUsed, CommitLimitGiB: 192}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// The admission rule, every term: projected = commit used now + need + pending, admitted iff it is
// at or under physical RAM less the headroom (128 - 8 = 120 GiB here). The numbers are the
// incident's: a 56 GiB baseline and two 32.75 GiB krea2 lanes.
func TestHostRAMAdmitsTheRule(t *testing.T) {
	cases := []struct {
		name            string
		commit          float64
		need, pending   float64
		headroom        float64
		wantOK          bool
		wantImpossible  bool
		wantWhyContains string
	}{
		{"the first krea2 lane fits on the 56 GiB baseline", 56, 32.75, 0, 8, true, false, ""},
		{"the second lane is refused while the first is still loading", 56, 32.75, 32.75, 8, false, false, "waiting for host RAM"},
		{"the second lane is refused once the first has loaded", 88.75, 32.75, 0, 8, false, false, "waiting for host RAM"},
		{"exactly at the limit is admitted", 110, 10, 0, 8, true, false, ""},
		{"a quarter GiB over the limit is refused", 110.25, 10, 0, 8, false, false, "waiting for host RAM"},
		{"the part of a granted lease that has not loaded counts", 80, 10, 31, 8, false, false, "+31.0 GiB still to load"},
		{"the same lease fits when nothing granted before it is still loading", 80, 10, 0, 8, true, false, ""},
		{"a lease that declares nothing is admitted even when the box is over", 162.9, 0, 40, 8, true, false, ""},
		{"a need above physical less headroom is impossible, not queued", 10, 130, 0, 8, false, true, "can never admit"},
		{"zero headroom admits up to physical RAM itself", 100, 28, 0, 0, true, false, ""},
	}
	for _, c := range cases {
		got := hostRAMAdmits(host128(c.commit), true, true, c.need, c.pending, c.headroom)
		if got.OK != c.wantOK || got.Impossible != c.wantImpossible {
			t.Errorf("%s: ok=%v impossible=%v, want ok=%v impossible=%v (%s)", c.name, got.OK, got.Impossible, c.wantOK, c.wantImpossible, got.Why)
		}
		if !c.wantOK && got.Why == "" {
			t.Errorf("%s: a refusal must say why", c.name)
		}
		if c.wantWhyContains != "" && !strings.Contains(got.Why, c.wantWhyContains) {
			t.Errorf("%s: why = %q, want it to contain %q", c.name, got.Why, c.wantWhyContains)
		}
	}
}

// The refusal reads as the operator asked for it: what the lease needs, what is committed of what is
// physical, and the headroom, in one line a person can act on.
func TestHostRAMRefusalTextNamesTheNumbers(t *testing.T) {
	mem := HostMemory{PhysicalGiB: 127.7, AvailableGiB: 31, CommitUsedGiB: 96.4, CommitLimitGiB: 187.7}
	got := hostRAMAdmits(mem, true, true, 33.4, 0, 8)
	want := "waiting for host RAM: needs 33.4 GiB, committed 96.4 of 127.7 GiB physical, 8.0 GiB headroom"
	if got.Why != want {
		t.Fatalf("refusal text\n got: %s\nwant: %s", got.Why, want)
	}
	if got.OK || got.Impossible {
		t.Fatalf("a queueable refusal must be neither OK nor impossible: %+v", got)
	}
	if !near(got.ProjectedGiB, 96.4+33.4) || !near(got.LimitGiB, 127.7-8) {
		t.Fatalf("projected %.1f limit %.1f: the check must carry the arithmetic it decided on", got.ProjectedGiB, got.LimitGiB)
	}
}

// A platform with a reader that fails to read refuses (the guards fail closed on a number they never
// had); a platform with no reader at all admits, because a refusal there would be permanent.
func TestHostRAMUnreadableMemory(t *testing.T) {
	supported := hostRAMAdmits(HostMemory{}, false, true, 20, 0, 8)
	if supported.OK || !supported.Unreadable || supported.Impossible {
		t.Fatalf("an unreadable host on a platform with a reader must refuse as unreadable, got %+v", supported)
	}
	if !strings.Contains(supported.Why, "cannot be read") {
		t.Fatalf("the refusal must say the memory could not be read: %q", supported.Why)
	}
	unsupported := hostRAMAdmits(HostMemory{}, false, false, 20, 0, 8)
	if !unsupported.OK {
		t.Fatalf("a platform with no reader cannot judge and must admit, got %+v", unsupported)
	}
	if none := hostRAMAdmits(HostMemory{}, false, true, 0, 0, 8); !none.OK {
		t.Fatalf("an unreadable counter must not turn away a lease that declares no need, got %+v", none)
	}
}

func TestHostVerdict(t *testing.T) {
	cases := []struct {
		name     string
		mem      HostMemory
		headroom float64
		want     HostVerdict
	}{
		{"well under physical", HostMemory{PhysicalGiB: 128, CommitUsedGiB: 56}, 8, HostOK},
		{"exactly headroom below physical is still OK", HostMemory{PhysicalGiB: 100, CommitUsedGiB: 92}, 8, HostOK},
		{"inside the headroom is NEAR", HostMemory{PhysicalGiB: 100, CommitUsedGiB: 92.5}, 8, HostNear},
		{"exactly physical is NEAR, not OVER", HostMemory{PhysicalGiB: 100, CommitUsedGiB: 100}, 8, HostNear},
		{"above physical is OVER", HostMemory{PhysicalGiB: 127.7, CommitUsedGiB: 162.9}, 8, HostOver},
		{"a hair above physical is OVER", HostMemory{PhysicalGiB: 100, CommitUsedGiB: 100.01}, 8, HostOver},
		{"no reading is unknown", HostMemory{}, 8, HostUnknown},
	}
	for _, c := range cases {
		if got := c.mem.Verdict(c.headroom); got != c.want {
			t.Errorf("%s: verdict %s, want %s", c.name, got, c.want)
		}
	}
}

const sampleMeminfo = `MemTotal:       134217728 kB
MemFree:         2097152 kB
MemAvailable:   33554432 kB
Buffers:          102400 kB
Committed_AS:   104857600 kB
CommitLimit:    201326592 kB
SwapTotal:       4194304 kB
`

func TestParseMeminfo(t *testing.T) {
	m, ok := parseMeminfo(sampleMeminfo)
	if !ok {
		t.Fatal("a complete /proc/meminfo must parse")
	}
	want := HostMemory{PhysicalGiB: 128, AvailableGiB: 32, CommitUsedGiB: 100, CommitLimitGiB: 192}
	if m != want {
		t.Fatalf("parsed %+v, want %+v (kB to GiB)", m, want)
	}
	without := func(drop string) string {
		var kept []string
		for _, line := range strings.Split(sampleMeminfo, "\n") {
			if !strings.HasPrefix(line, drop) {
				kept = append(kept, line)
			}
		}
		return strings.Join(kept, "\n")
	}
	for _, drop := range []string{"MemTotal:", "MemAvailable:"} {
		if _, ok := parseMeminfo(without(drop)); ok {
			t.Errorf("a meminfo without %s must not be read as a reading", drop)
		}
	}
	// A sandbox that hides the commit accounting still yields a reading, from what the box visibly
	// uses: the alternative is every lease that declares host RAM waiting forever.
	for _, drop := range []string{"Committed_AS:", "CommitLimit:"} {
		got, ok := parseMeminfo(without(drop))
		want := HostMemory{PhysicalGiB: 128, AvailableGiB: 32, CommitUsedGiB: 96, CommitLimitGiB: 128}
		if !ok || got != want {
			t.Errorf("without %s: %+v ok=%v, want the used-memory fallback %+v", drop, got, ok, want)
		}
	}
	if _, ok := parseMeminfo("MemTotal: banana kB\n"); ok {
		t.Error("a non-numeric counter must not parse")
	}
}

// TestReadHostMemoryIsPlausibleWhereSupported: where this platform has a reader the reading is
// internally consistent; where it has none the bool says so.
func TestReadHostMemoryIsPlausibleWhereSupported(t *testing.T) {
	m, ok := ReadHostMemory()
	if !ok {
		if HostMemorySupported {
			t.Fatal("this platform has a reader but it reported no reading")
		}
		return
	}
	if m.PhysicalGiB <= 0 || m.PhysicalGiB > 65536 {
		t.Fatalf("physical RAM = %v GiB, not plausible", m.PhysicalGiB)
	}
	if m.AvailableGiB <= 0 || m.AvailableGiB > m.PhysicalGiB+0.5 {
		t.Fatalf("available %v GiB of %v physical, not plausible", m.AvailableGiB, m.PhysicalGiB)
	}
	t.Logf("host memory: %.1f GiB physical, %.1f available, commit %.1f of %.1f GiB (%s at the default headroom)",
		m.PhysicalGiB, m.AvailableGiB, m.CommitUsedGiB, m.CommitLimitGiB, m.Verdict(DefaultHostRAMHeadroomGiB))
	if m.CommitUsedGiB <= 0 || m.CommitLimitGiB < m.CommitUsedGiB {
		t.Fatalf("commit %v of limit %v GiB, not plausible", m.CommitUsedGiB, m.CommitLimitGiB)
	}
}

// The seam lets the packages that take a lease run against a known host, and puts the real reader
// back; the available-RAM figure the placement guard reads comes from the same reading.
func TestUseHostMemoryReaderStandsAHostInAndRestores(t *testing.T) {
	fake := HostMemory{PhysicalGiB: 64, AvailableGiB: 12.5, CommitUsedGiB: 40, CommitLimitGiB: 100}
	restore := UseHostMemoryReader(func() (HostMemory, bool) { return fake, true })
	got, ok := ReadHostMemory()
	if !ok || got != fake {
		t.Fatalf("seam reading = %+v ok=%v, want the stand-in", got, ok)
	}
	if free, fok := HostFreeRAMGiB(); !fok || free != 12.5 {
		t.Fatalf("HostFreeRAMGiB = %v ok=%v, want the stand-in's available 12.5", free, fok)
	}
	restore()
	if again, _ := ReadHostMemory(); again == fake {
		t.Fatal("restore left the stand-in installed")
	}
}
