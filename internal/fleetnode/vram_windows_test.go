package fleetnode

import (
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// TestPidFromInstance locks the PDH instance-name parse for
// \GPU Process Memory(pid_*_luid_*)\Dedicated Usage instances.
func TestPidFromInstance(t *testing.T) {
	cases := []struct {
		in   string
		want uint32
	}{
		{"pid_12345_luid_0x00000000_0x0000E7C2_phys_0", 12345},
		{"pid_4_luid_0x0_phys_0", 4},
		{"pid_999", 999},
		{"pid_", 0},
		{"pid_abc_luid_0", 0},
		{"12345_luid_0", 0},
		{"", 0},
		{"_Total", 0},
		{"pid_4294967295", 4294967295}, // max uint32
		{"pid_99999999999", 0},         // overflows uint32 -> rejected
	}
	for _, tc := range cases {
		if got := pidFromInstance(tc.in); got != tc.want {
			t.Errorf("pidFromInstance(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestLivePDH is the live smoke for the PDH per-process source. It needs a real
// NVIDIA/WDDM box with GPU Process Memory counters, so it only runs when
// OFFLOAD_LIVE_PDH=1 (acceptance-step gate). It dumps every pid instance and
// its dedicated bytes (evidence for the bring-up validation vs Afterburner),
// checks the all-instances sum is plausible (>0: dwm.exe et al always hold
// dedicated memory on a desktop box), and smokes TreeDedicatedGiB on our own
// process tree (must not error; the test binary itself may legitimately be 0).
func TestLivePDH(t *testing.T) {
	if os.Getenv("OFFLOAD_LIVE_PDH") != "1" {
		t.Skip("set OFFLOAD_LIVE_PDH=1 to run the live PDH smoke")
	}

	// Raw enumeration through the same plumbing TreeDedicatedGiB uses, without
	// the tree filter, so the evidence shows exactly what PDH returned.
	var q uintptr
	if r, _, _ := pdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&q))); r != 0 {
		t.Fatalf("PdhOpenQuery: 0x%x", r)
	}
	defer pdhCloseQuery.Call(q)
	path, _ := syscall.UTF16PtrFromString(`\GPU Process Memory(*)\Dedicated Usage`)
	var c uintptr
	if r, _, _ := pdhAddEnglishCounter.Call(q, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&c))); r != 0 {
		t.Fatalf("PdhAddEnglishCounter: 0x%x", r)
	}
	if r, _, _ := pdhCollectQueryData.Call(q); r != 0 {
		t.Fatalf("PdhCollectQueryData: 0x%x", r)
	}
	var bufLen, itemCount uint32
	pdhGetFormattedCounterArr.Call(c, pdhFmtDouble, uintptr(unsafe.Pointer(&bufLen)), uintptr(unsafe.Pointer(&itemCount)), 0)
	if bufLen == 0 {
		t.Fatal("no GPU Process Memory instances — counter set unavailable on this box")
	}
	buf := make([]byte, bufLen)
	if r, _, _ := pdhGetFormattedCounterArr.Call(c, pdhFmtDouble, uintptr(unsafe.Pointer(&bufLen)), uintptr(unsafe.Pointer(&itemCount)), uintptr(unsafe.Pointer(&buf[0]))); r != 0 {
		t.Fatalf("PdhGetFormattedCounterArray: 0x%x", r)
	}
	items := (*[1 << 16]pdhFmtCountervalueItemDouble)(unsafe.Pointer(&buf[0]))[:itemCount:itemCount]
	var sumBytes float64
	badStatus := 0
	for i, it := range items {
		name := syscall.UTF16ToString((*[256]uint16)(unsafe.Pointer(it.Name))[:])
		t.Logf("instance[%d] %q CStatus=0x%x bytes=%.0f (%.3f GiB)", i, name, it.Val.CStatus, it.Val.Double, it.Val.Double/(1<<30))
		if it.Val.CStatus != 0 {
			badStatus++
			continue
		}
		if pidFromInstance(name) != 0 {
			sumBytes += it.Val.Double
		}
	}
	t.Logf("all-instances sum: %.3f GiB across %d instances (%d bad CStatus)", sumBytes/(1<<30), itemCount, badStatus)
	if sumBytes <= 0 {
		t.Errorf("all-pid dedicated sum = %.0f bytes, want > 0 on a live desktop (struct layout or counter problem)", sumBytes)
	}
	if badStatus > int(itemCount)/2 {
		t.Errorf("%d/%d instances have non-zero CStatus — layout misalignment suspected", badStatus, itemCount)
	}

	// Tree path: our own test process tree. Must not error; value may be 0
	// (the test binary does not necessarily touch the GPU).
	gib, err := TreeDedicatedGiB(os.Getpid())
	if err != nil {
		t.Fatalf("TreeDedicatedGiB(self): %v", err)
	}
	t.Logf("TreeDedicatedGiB(self pid %d) = %.3f GiB", os.Getpid(), gib)
	if gib < 0 {
		t.Errorf("TreeDedicatedGiB(self) = %v, want >= 0", gib)
	}
}
