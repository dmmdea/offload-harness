package gpuprobe

import (
	"math"
	"testing"
)

// The incident, with its own figures (the reference Windows box, one morning of refusals): 36 agent sessions held the commit
// charge at 125.2 GiB against a 152.7 GiB limit while 68.2 GiB of RAM was available, and the agent seat (36 GiB touched, 61 GiB
// of commit, stated) was refused on every contract. The page file could grow from 33.08 to 116.30 GiB on a volume with 338.4 of
// 930.4 GiB free, so the commit the OS could grant reached 244 GiB.
func TestACommitIsHeldToTheCeilingThePageFileCanReachNotToTheCurrentLimit(t *testing.T) {
	const gib = 1 << 30
	g := func(x float64) uint64 { return uint64(x * gib) }
	volume := func(free, total float64) func(string) (uint64, uint64, bool) {
		return func(string) (uint64, uint64, bool) { return g(free), g(total), true }
	}
	file := PageFile{Path: `C:\pagefile.sys`, Volume: `C:\`, SizeBytes: g(33.08), MaxBytes: g(116.30)}

	growth := float64(commitGrowthBytes([]PageFile{file}, volume(338.4, 930.4))) / gib
	if math.Abs(growth-83.22) > 0.01 {
		t.Fatalf("growth %.2f GiB, want 83.22: the file's maximum less its size, which the volume can give", growth)
	}
	// A volume that cannot give it bounds the growth: free space less the tenth of the volume the rule never plans on taking.
	if tight := float64(commitGrowthBytes([]PageFile{file}, volume(120, 930.4))) / gib; math.Abs(tight-(120-93.04)) > 0.01 {
		t.Errorf("growth on a volume with 120 of 930.4 GiB free is %.2f GiB, want %.2f", tight, 120-93.04)
	}
	// Two files on one volume share its free space; a fixed file and an unreadable volume add nothing.
	two := []PageFile{file, {Path: `C:\second.sys`, Volume: `C:\`, SizeBytes: g(1), MaxBytes: g(100)}}
	if shared := float64(commitGrowthBytes(two, volume(200, 930.4))) / gib; math.Abs(shared-(200-93.04)) > 0.01 {
		t.Errorf("two files on one volume grew %.2f GiB between them, want the volume's %.2f once", shared, 200-93.04)
	}
	fixed := PageFile{Path: `D:\pagefile.sys`, Volume: `D:\`, SizeBytes: g(16), MaxBytes: g(16)}
	if n := commitGrowthBytes([]PageFile{fixed}, volume(500, 1000)); n != 0 {
		t.Errorf("a file at its maximum grew %d bytes", n)
	}
	if n := commitGrowthBytes([]PageFile{file}, func(string) (uint64, uint64, bool) { return 0, 0, false }); n != 0 {
		t.Errorf("a volume that cannot be read gave %d bytes of growth", n)
	}

	seat := HostLoad{TouchGiB: 36, CommitGiB: 61, CommitStated: true}
	asRead := HostMemory{PhysicalGiB: 127.7, AvailableGiB: 68.2, CommitUsedGiB: 125.2, CommitLimitGiB: 152.7}
	if c := HostRAMAdmitsLoad(asRead, true, seat, HostLoad{}, 8); c.OK || !c.CommitShort {
		t.Fatalf("a reading whose limit cannot grow keeps the current limit as its line: %+v", c)
	}
	growable := asRead
	growable.CommitCeilingGiB = asRead.CommitLimitGiB + growth
	c := HostRAMAdmitsLoad(growable, true, seat, HostLoad{}, 8)
	if !c.OK {
		t.Fatalf("the seat fits the host (68.2 GiB available for 36 touched, 125.2 + 61 GiB of commit under a 235.9 GiB ceiling) and was refused: %s", c.Why)
	}
	if math.Abs(c.CommitBoundGiB-(152.7+growth-8)) > 0.01 {
		t.Errorf("the commit line is %.1f GiB, want the ceiling less the headroom", c.CommitBoundGiB)
	}
	// The same lease with no commit stated is taken at the factor and judged on the same line, not on physical RAM, which the
	// charge already exceeds.
	if u := HostRAMAdmitsLoad(growable, true, HostLoad{TouchGiB: 36, CommitGiB: 36}, HostLoad{}, 8); !u.OK || !u.CommitEstimated || math.Abs(u.CommitNeedGiB-36*UnstatedCommitFactor) > 0.01 {
		t.Errorf("an unstated commit on a host whose limit can grow: %+v", u)
	}
	// What pages is still refused: the touched set against the memory that is available.
	short := growable
	short.AvailableGiB = 40
	if c := HostRAMAdmitsLoad(short, true, seat, HostLoad{}, 8); c.OK || !c.TouchShort || c.CommitShort {
		t.Errorf("36 GiB touched with 40 available leaves 4 of the 8 GiB headroom and must wait on the touched set alone: %+v", c)
	}
	// And the ceiling is a line: a charge that would pass it waits.
	full := growable
	full.CommitUsedGiB = 190
	if c := HostRAMAdmitsLoad(full, true, seat, HostLoad{}, 8); c.OK || !c.CommitShort {
		t.Errorf("190 + 61 GiB of commit is past the %.1f GiB ceiling less the headroom and was admitted: %+v", growable.CommitCeilingGiB, c)
	}
	if v := growable.Verdict(8); v != HostOK {
		t.Errorf("verdict %s for a host with 68.2 GiB available and 110 GiB under its ceiling, want OK", v)
	}
}
