package volumes

import (
	"strings"
	"testing"
)

func gb(n uint64) uint64 { return n * GiB }

// TestPickPrefersTheRoomiestNonOSVolume is the whole policy in one test: the
// installer defaulted to $HOME (the OS drive) forever, which is how a laptop got a
// model tree on the same volume as Windows while a data drive sat empty.
func TestPickPrefersTheRoomiestNonOSVolume(t *testing.T) {
	got, err := Pick([]Volume{
		{Root: `C:\`, TotalBytes: gb(500), FreeBytes: gb(300), IsOS: true},
		{Root: `D:\`, TotalBytes: gb(1000), FreeBytes: gb(60)},
		{Root: `V:\`, TotalBytes: gb(2000), FreeBytes: gb(900)},
	}, PickOptions{})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.Volume.Root != `V:\` {
		t.Errorf("chose %s, want V:\\ (900 GiB free beats the 300 GiB OS volume and the 60 GiB D:)", got.Volume.Root)
	}
	if !strings.Contains(got.Because, "non-OS") || !strings.Contains(got.Because, "900.0 GiB") {
		t.Errorf("the reason must be recordable and specific, got %q", got.Because)
	}
	if !strings.Contains(got.Because, `D:\`) {
		t.Errorf("the runner-up belongs in the reason so a later operator can see the alternatives: %q", got.Because)
	}
}

// TestPickNeverSilentlyUsesTheOSVolume: filling the OS volume takes the MACHINE
// down, not just the harness. It must be an explicit decision.
func TestPickNeverSilentlyUsesTheOSVolume(t *testing.T) {
	vols := []Volume{{Root: "/", TotalBytes: gb(100), FreeBytes: gb(80), IsOS: true}}
	_, err := Pick(vols, PickOptions{})
	if err == nil {
		t.Fatal("an OS-volume-only box must refuse rather than quietly install onto /")
	}
	if !strings.Contains(err.Error(), "allow-os-volume") {
		t.Errorf("the refusal must name the override, got %q", err)
	}
	got, err := Pick(vols, PickOptions{AllowOSVolume: true})
	if err != nil {
		t.Fatalf("explicitly allowed: %v", err)
	}
	if got.Volume.Root != "/" || !strings.Contains(got.Because, "explicitly allowed") {
		t.Errorf("the recorded reason must show it was a deliberate choice, got %+v", got)
	}
}

// TestPickEnforcesTheMinimum: a volume that cannot hold a tier's model set is not
// a candidate, and the refusal says how much was short.
func TestPickEnforcesTheMinimum(t *testing.T) {
	_, err := Pick([]Volume{
		{Root: `D:\`, TotalBytes: gb(120), FreeBytes: gb(5)},
		{Root: `E:\`, TotalBytes: gb(120), FreeBytes: gb(9)},
	}, PickOptions{MinFreeBytes: gb(20)})
	if err == nil {
		t.Fatal("two undersized volumes must not yield a choice")
	}
	for _, want := range []string{`D:\`, "5.0 GiB", "need 20.0 GiB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must state the shortfall (%q missing): %v", want, err)
		}
	}
}

// TestPickRejectsVolumesThatCanVanish: an install on a USB stick or a dropped
// share is worse than no install.
func TestPickRejectsVolumesThatCanVanish(t *testing.T) {
	_, err := Pick([]Volume{
		{Root: `F:\`, TotalBytes: gb(2000), FreeBytes: gb(1900), Removable: true},
		{Root: `Z:\`, TotalBytes: gb(4000), FreeBytes: gb(3000), Network: true},
	}, PickOptions{})
	if err == nil {
		t.Fatal("removable and network volumes must never be selected, however large")
	}
	if !strings.Contains(err.Error(), "removable") || !strings.Contains(err.Error(), "network share") {
		t.Errorf("refusal must say why each was skipped: %v", err)
	}
}

// TestPickIsDeterministic: equal free space must not flip between runs, or two
// installs of the same machine land in different places.
func TestPickIsDeterministic(t *testing.T) {
	vols := []Volume{
		{Root: `E:\`, TotalBytes: gb(500), FreeBytes: gb(400)},
		{Root: `D:\`, TotalBytes: gb(500), FreeBytes: gb(400)},
	}
	first, err := Pick(vols, PickOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := Pick(vols, PickOptions{})
		if err != nil || again.Volume.Root != first.Volume.Root {
			t.Fatalf("tie-break is unstable: %v / %v", first.Volume.Root, again.Volume.Root)
		}
	}
	if first.Volume.Root != `D:\` {
		t.Errorf("ties break on root for a predictable answer, got %s", first.Volume.Root)
	}
}

// TestPickIgnoresZeroCapacityDrives: an empty card reader reports 0/0, which must
// not read as "a disk with no free space" in the refusal text.
func TestPickIgnoresZeroCapacityDrives(t *testing.T) {
	got, err := Pick([]Volume{
		{Root: `G:\`, TotalBytes: 0, FreeBytes: 0},
		{Root: `D:\`, TotalBytes: gb(500), FreeBytes: gb(400)},
	}, PickOptions{})
	if err != nil {
		t.Fatalf("a dead drive must not block a healthy one: %v", err)
	}
	if got.Volume.Root != `D:\` {
		t.Errorf("chose %s", got.Volume.Root)
	}
}

// TestListReturnsThisMachinesVolumes is the only test that touches the real
// platform: enumeration must find at least the OS volume, with a real capacity.
func TestListReturnsThisMachinesVolumes(t *testing.T) {
	vols, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(vols) == 0 {
		t.Fatal("no volumes enumerated — Pick would have nothing to choose from")
	}
	var sawOS bool
	for _, v := range vols {
		if v.TotalBytes == 0 {
			t.Errorf("%s reported zero capacity; such a volume must be skipped, not listed", v.Root)
		}
		if v.FreeBytes > v.TotalBytes {
			t.Errorf("%s free (%d) exceeds total (%d)", v.Root, v.FreeBytes, v.TotalBytes)
		}
		if v.IsOS {
			sawOS = true
		}
	}
	if !sawOS {
		t.Error("the OS volume must be identified — the policy's whole job is to avoid it")
	}
}

// TestPickPrefersThePoolRootOverItsDatasets: every ZFS dataset of one pool reports
// the SAME pool free space, so a name-only tie-break installs under whatever sorts
// first (`/srv/pool/apps/adventurelog` on the measured box). Depth-first tie-break
// picks the mount a human would.
func TestPickPrefersThePoolRootOverItsDatasets(t *testing.T) {
	got, err := Pick([]Volume{
		{Root: "/", FS: "ext4", TotalBytes: gb(98), FreeBytes: gb(33), IsOS: true},
		{Root: "/srv/pool/apps/adventurelog", FS: "zfs", TotalBytes: gb(338), FreeBytes: gb(246)},
		{Root: "/srv/pool/apps", FS: "zfs", TotalBytes: gb(338), FreeBytes: gb(246)},
		{Root: "/srv/pool", FS: "zfs", TotalBytes: gb(338), FreeBytes: gb(246)},
		{Root: "/srv/pool/apps/immich", FS: "zfs", TotalBytes: gb(338), FreeBytes: gb(246)},
	}, PickOptions{})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.Volume.Root != "/srv/pool" {
		t.Errorf("chose %s, want the pool root /srv/pool — all datasets report the same free space", got.Volume.Root)
	}
}
