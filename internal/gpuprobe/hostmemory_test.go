package gpuprobe

import (
	"math"
	"strings"
	"testing"
)

// host128 is the shape of the incident box with round binary numbers (so the arithmetic below is
// exact): 128 GiB physical, a 192 GiB commit limit, and the memory that is available stated outright
// (it is not a function of the commit charge: a charge is promised memory, the available figure is what is free).
func host128(commitUsed, available float64) HostMemory {
	return HostMemory{PhysicalGiB: 128, AvailableGiB: available, CommitUsedGiB: commitUsed, CommitLimitGiB: 192}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// The single-figure form of the rule, every term: a figure declared once is taken for both quantities and states no commit, and an
// admission needs available - need - pending >= headroom AND commit used + need + pending <= physical RAM - headroom (here 128 - 8 =
// 120 GiB). The numbers are the incident's: a 56 GiB baseline (so 72 GiB of 128 available) and two 32.75 GiB krea2 lanes.
func TestHostRAMAdmitsTheRule(t *testing.T) {
	cases := []struct {
		name            string
		commit, avail   float64
		need, pending   float64
		headroom        float64
		wantOK          bool
		wantImpossible  bool
		wantWhyContains string
	}{
		{"the first krea2 lane fits on the 56 GiB baseline", 56, 72, 32.75, 0, 8, true, false, ""},
		{"the second lane is refused while the first is still loading", 56, 72, 32.75, 32.75, 8, false, false, "only 72.0 GiB of 128.0 GiB physical is available"},
		{"the second lane is refused once the first has loaded", 88.5, 39.5, 32.75, 0, 8, false, false, "only 39.5 GiB of 128.0 GiB physical is available"},
		{"exactly at the available floor is admitted", 50, 18, 10, 0, 8, true, false, ""},
		{"a quarter GiB under the available floor is refused", 50, 17.75, 10, 0, 8, false, false, "waiting for host RAM"},
		{"the part of a granted lease that has not loaded counts", 80, 60, 10, 43, 8, false, false, "+43.0 GiB still to load"},
		{"the same lease fits when nothing granted before it is still loading", 80, 60, 10, 0, 8, true, false, ""},
		{"the commit charge refuses a lease the available memory would admit", 170, 100, 20, 0, 8, false, false, "committed 170.0 of 128.0 GiB physical"},
		{"a lease that declares nothing is admitted even when the box is over", 190, 0, 0, 40, 8, true, false, ""},
		{"a need above physical less headroom is impossible, not queued", 10, 100, 130, 0, 8, false, true, "can never admit"},
		{"zero headroom admits up to the available memory itself", 20, 100, 100, 0, 0, true, false, ""},
	}
	for _, c := range cases {
		got := hostRAMAdmits(host128(c.commit, c.avail), true, true, c.need, c.pending, c.headroom)
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

// The refusal reads as the operator asked for it: what the lease needs, what is available of what is physical, and the
// headroom, in one line a person can act on; the commit sentence carries the commit limit it was judged against.
func TestHostRAMRefusalTextNamesTheNumbers(t *testing.T) {
	mem := HostMemory{PhysicalGiB: 127.7, AvailableGiB: 31, CommitUsedGiB: 40, CommitLimitGiB: 187.7}
	got := hostRAMAdmits(mem, true, true, 33.4, 0, 8)
	want := "waiting for host RAM: needs 33.4 GiB, only 31.0 GiB of 127.7 GiB physical is available, 8.0 GiB headroom"
	if got.Why != want {
		t.Fatalf("refusal text\n got: %s\nwant: %s", got.Why, want)
	}
	if got.OK || got.Impossible || !got.TouchShort || got.CommitShort {
		t.Fatalf("a queueable refusal must be neither OK nor impossible, and name the touched condition only: %+v", got)
	}
	if !near(got.AvailableAfterGiB, 31-33.4) || !near(got.LimitGiB, 127.7-8) {
		t.Fatalf("available after %.1f limit %.1f: the check must carry the arithmetic it decided on", got.AvailableAfterGiB, got.LimitGiB)
	}
	mem = HostMemory{PhysicalGiB: 127.7, AvailableGiB: 90, CommitUsedGiB: 160, CommitLimitGiB: 171.7}
	got = statedAdmits(mem, true, true, 33.4, 0, 8)
	want = "waiting for host RAM: needs 33.4 GiB of commit, committed 160.0 of 171.7 GiB commit limit, 8.0 GiB headroom"
	if got.Why != want {
		t.Fatalf("commit refusal text\n got: %s\nwant: %s", got.Why, want)
	}
	if !near(got.CommitProjectedGiB, 160+33.4) || !near(got.CommitBoundGiB, 171.7-8) {
		t.Fatalf("projected %.1f bound %.1f: the check must carry the arithmetic it decided on", got.CommitProjectedGiB, got.CommitBoundGiB)
	}
	// The same figure with no commit stated names the physical line it was held to, and says what states one.
	got = hostRAMAdmits(mem, true, true, 33.4, 0, 8)
	want = "waiting for host RAM: needs 33.4 GiB, committed 160.0 of 127.7 GiB physical, 8.0 GiB headroom (no commit stated, so it is held to physical RAM; a stated commit is held to the 171.7 GiB commit limit)"
	if got.Why != want {
		t.Fatalf("unstated commit refusal text\n got: %s\nwant: %s", got.Why, want)
	}
	if !near(got.CommitProjectedGiB, 160+33.4) || !near(got.CommitBoundGiB, 127.7-8) || got.CommitStated {
		t.Fatalf("projected %.1f bound %.1f stated %v: the unstated line is physical RAM less the headroom", got.CommitProjectedGiB, got.CommitBoundGiB, got.CommitStated)
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

// The verdict is the same two lines the grant applies: the available memory, and the commit limit where the OS enforces one.
func TestHostVerdict(t *testing.T) {
	cases := []struct {
		name     string
		mem      HostMemory
		headroom float64
		want     HostVerdict
	}{
		{"room on both lines", HostMemory{PhysicalGiB: 128, AvailableGiB: 72, CommitUsedGiB: 56, CommitLimitGiB: 192}, 8, HostOK},
		{"the reference reading: commit 70.5 of 171.7 with 75.6 available is OK although commit is far above half of RAM", HostMemory{PhysicalGiB: 127.7, AvailableGiB: 75.6, CommitUsedGiB: 70.5, CommitLimitGiB: 171.7}, 8, HostOK},
		{"commit above physical RAM is OVER although it is under the limit and memory is available (the line a caller that declares one figure is held to)", HostMemory{PhysicalGiB: 127.7, AvailableGiB: 40, CommitUsedGiB: 150, CommitLimitGiB: 187.7}, 8, HostOver},
		{"the incident reading of 2026-10-09 (3.2 GiB available, 162.9 GiB committed on 127.7 GiB of RAM) is OVER, as it was", HostMemory{PhysicalGiB: 127.7, AvailableGiB: 3.2, CommitUsedGiB: 162.9, CommitLimitGiB: 187.7}, 8, HostOver},
		{"commit inside the headroom of physical RAM is NEAR", HostMemory{PhysicalGiB: 100, AvailableGiB: 60, CommitUsedGiB: 95, CommitLimitGiB: 160}, 8, HostNear},
		{"exactly physical RAM less the headroom is still OK", HostMemory{PhysicalGiB: 100, AvailableGiB: 60, CommitUsedGiB: 92, CommitLimitGiB: 160}, 8, HostOK},
		{"commit just above physical RAM is OVER", HostMemory{PhysicalGiB: 100, AvailableGiB: 60, CommitUsedGiB: 100.5, CommitLimitGiB: 160}, 8, HostOver},
		{"exactly the headroom available is still OK", HostMemory{PhysicalGiB: 100, AvailableGiB: 8, CommitUsedGiB: 20, CommitLimitGiB: 160}, 8, HostOK},
		{"available inside the headroom is NEAR", HostMemory{PhysicalGiB: 100, AvailableGiB: 7.5, CommitUsedGiB: 20, CommitLimitGiB: 160}, 8, HostNear},
		{"a limit below physical RAM (strict Linux): commit inside the headroom of the limit is NEAR", HostMemory{PhysicalGiB: 200, AvailableGiB: 60, CommitUsedGiB: 152.5, CommitLimitGiB: 160}, 8, HostNear},
		{"a limit below physical RAM: exactly the limit less the headroom is still OK", HostMemory{PhysicalGiB: 200, AvailableGiB: 60, CommitUsedGiB: 152, CommitLimitGiB: 160}, 8, HostOK},
		{"commit at the limit is OVER", HostMemory{PhysicalGiB: 100, AvailableGiB: 60, CommitUsedGiB: 160, CommitLimitGiB: 160}, 8, HostOver},
		{"nothing available is OVER", HostMemory{PhysicalGiB: 100, AvailableGiB: 0, CommitUsedGiB: 20, CommitLimitGiB: 160}, 8, HostOver},
		{"a soft commit limit is not a line: commit between it and physical RAM reads OK", HostMemory{PhysicalGiB: 200, AvailableGiB: 60, CommitUsedGiB: 170, CommitLimitGiB: 160, CommitLimitSoft: true}, 8, HostOK},
		{"a soft commit limit leaves physical RAM a line: commit above it is OVER", HostMemory{PhysicalGiB: 100, AvailableGiB: 60, CommitUsedGiB: 200, CommitLimitGiB: 160, CommitLimitSoft: true}, 8, HostOver},
		{"a soft commit limit leaves the available line", HostMemory{PhysicalGiB: 100, AvailableGiB: 5, CommitUsedGiB: 50, CommitLimitGiB: 160, CommitLimitSoft: true}, 8, HostNear},
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
	// uses: the alternative is every lease that declares host RAM waiting forever. Its limit is no limit anyone enforces.
	for _, drop := range []string{"Committed_AS:", "CommitLimit:"} {
		got, ok := parseMeminfo(without(drop))
		want := HostMemory{PhysicalGiB: 128, AvailableGiB: 32, CommitUsedGiB: 96, CommitLimitGiB: 128, CommitLimitSoft: true}
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
	t.Logf("host memory: %.1f GiB physical, %.1f available, commit %.1f of %.1f GiB (soft limit %v; %s at the default headroom)",
		m.PhysicalGiB, m.AvailableGiB, m.CommitUsedGiB, m.CommitLimitGiB, m.CommitLimitSoft, m.Verdict(DefaultHostRAMHeadroomGiB))
	if m.CommitUsedGiB <= 0 || m.CommitLimitGiB < m.CommitUsedGiB {
		// Under a soft limit the charge may pass the limit; a hard one never does.
		if !m.CommitLimitSoft {
			t.Fatalf("commit %v of limit %v GiB, not plausible", m.CommitUsedGiB, m.CommitLimitGiB)
		}
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

// The two conditions are independent: what can be handed out without paging, less the need and
// the part of the running leases still to load, must stay at or above the headroom, and the commit must fit the commit limit.
// If WDDM GPU allocations inflate a process's commit, the commit condition sees them and the available one is untouched; if
// some resident memory is not in commit, the available condition catches it. Either way an admission needs BOTH.
func TestHostRAMAdmitsAlsoRequiresTheAvailableFloor(t *testing.T) {
	cases := []struct {
		name            string
		mem             HostMemory
		need, pending   float64
		wantOK          bool
		wantWhyContains string
	}{
		{"commit reads low but little is available: the available condition refuses",
			HostMemory{PhysicalGiB: 128, AvailableGiB: 20, CommitUsedGiB: 50, CommitLimitGiB: 192}, 15, 0, false, "only 20.0 GiB of 128.0 GiB physical is available"},
		{"what the running leases have yet to load comes off the available figure too",
			HostMemory{PhysicalGiB: 128, AvailableGiB: 40, CommitUsedGiB: 50, CommitLimitGiB: 192}, 15, 20, false, "+20.0 GiB still to load"},
		{"exactly at the floor is admitted",
			HostMemory{PhysicalGiB: 128, AvailableGiB: 23, CommitUsedGiB: 50, CommitLimitGiB: 192}, 15, 0, true, ""},
		{"commit near the limit with plenty available: the commit condition refuses, with its own sentence",
			HostMemory{PhysicalGiB: 128, AvailableGiB: 90, CommitUsedGiB: 180, CommitLimitGiB: 192}, 10, 0, false, "committed 180.0 of 192.0 GiB commit limit"},
		{"commit above physical RAM but under the limit is admitted",
			HostMemory{PhysicalGiB: 128, AvailableGiB: 90, CommitUsedGiB: 125, CommitLimitGiB: 192}, 10, 0, true, ""},
		{"both conditions satisfied",
			HostMemory{PhysicalGiB: 128, AvailableGiB: 80, CommitUsedGiB: 60, CommitLimitGiB: 192}, 20, 0, true, ""},
		{"a lease that declares nothing is admitted whatever is available",
			HostMemory{PhysicalGiB: 128, AvailableGiB: 0, CommitUsedGiB: 128, CommitLimitGiB: 192}, 0, 0, true, ""},
	}
	for _, c := range cases {
		got := statedAdmits(c.mem, true, true, c.need, c.pending, 8) // a STATED commit, judged against the commit limit
		if got.OK != c.wantOK || got.Impossible {
			t.Errorf("%s: ok=%v impossible=%v, want ok=%v (%s)", c.name, got.OK, got.Impossible, c.wantOK, got.Why)
		}
		if !c.wantOK && (got.Why == "" || !strings.Contains(got.Why, c.wantWhyContains)) {
			t.Errorf("%s: why = %q, want it to contain %q", c.name, got.Why, c.wantWhyContains)
		}
		if got.AvailableGiB != c.mem.AvailableGiB && c.need > 0 {
			t.Errorf("%s: the check must carry the available figure it decided on, got %v want %v", c.name, got.AvailableGiB, c.mem.AvailableGiB)
		}
	}
}
