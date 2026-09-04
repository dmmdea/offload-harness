package hostsample

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestCPUPctFromTwoReadings(t *testing.T) {
	// idle 100→180 (+80), total 200→300 (+100) ⇒ busy 20 %
	got := cpuPct(cpuTimes{idle: 100, total: 200}, cpuTimes{idle: 180, total: 300})
	if got != 20 {
		t.Fatalf("got %d want 20", got)
	}
}

func TestCPUPctNoElapsedIsUnknown(t *testing.T) {
	if got := cpuPct(cpuTimes{idle: 5, total: 9}, cpuTimes{idle: 5, total: 9}); got != -1 {
		t.Fatalf("got %d want -1 (unknown)", got)
	}
}

func TestStartPublishesWithinTwoIntervals(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skipf("readCPU/readRAM have no implementation on %s; skipping a live sampler test that would just report Known=false forever", runtime.GOOS)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := Start(ctx, 50*time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if smp, ok := s.Load(); ok {
			if smp.RAMTotalGiB <= 0 {
				t.Fatalf("RAM total must be positive, got %v", smp.RAMTotalGiB)
			}
			if smp.CPUPct < 0 || smp.CPUPct > 100 {
				t.Fatalf("cpu %% out of range: %d", smp.CPUPct)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no sample published")
}
