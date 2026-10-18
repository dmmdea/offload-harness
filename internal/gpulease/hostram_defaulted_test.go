package gpulease

// A lease that states no commit (it declared one figure, as every caller before --ram-commit did) is judged on its commit against
// physical RAM less the headroom, the line it always had. A lease that STATES a commit is judged against the OS commit limit,
// even when the figure it states equals the one it touches. The host these tests stand in for is commitHostMem's: 100 GiB of
// RAM, a 160 GiB commit limit, 8 GiB of headroom, so the unstated line is 92 and the stated one 152.

import (
	"os"
	"strings"
	"testing"
	"time"
)

// recordText is the lease's record file as written: Info reads an absent commit as the touched figure, so only the file says
// whether a commit was stated.
func recordText(t *testing.T, m *Manager, epoch uint64) string {
	t.Helper()
	raw, err := os.ReadFile(epochRecordPath(m.leaseDir(), epoch))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAGrantThatStatesNoCommitIsJudgedAgainstPhysicalRAMLessTheHeadroom(t *testing.T) {
	m, _ := commitScoped(t, commitHostMem(80, 70))
	l0, err := m.TryAcquire(ClassMedia, Options{Reason: "one figure", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 30})
	if err == nil {
		_ = l0.Release()
		t.Fatalf("70 committed + 30 declared once (no commit stated) is over the 92 GiB line (physical less headroom): want refused, got admitted")
	}
	short := asHostRAM(t, err)
	if !short.CommitShort || short.TouchShort || short.Impossible {
		t.Fatalf("70 committed + 30 declared once is over the 92 GiB line (physical less headroom): want a refusal on commit alone, got commitShort=%v touchShort=%v impossible=%v (%s)",
			short.CommitShort, short.TouchShort, short.Impossible, err)
	}
	if !strings.Contains(err.Error(), "committed 70.0 of 100.0 GiB physical") {
		t.Fatalf("the refusal names the line it was held to: %s", err)
	}

	// The same figure, the commit STATED (equal to the touched set): judged against the 152 GiB the commit limit leaves.
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "stated", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 30, HostRAMCommitGiB: 30})
	if err != nil {
		t.Fatalf("a stated commit of 30 fits the commit limit (70 + 30 <= 152): %v", err)
	}
	defer func() { _ = l.Release() }()
	if raw := recordText(t, m, l.Epoch()); !strings.Contains(raw, `"host_ram_commit_gib":30`) {
		t.Fatalf("a stated commit is on the record even when it equals the touched figure, or a reader would take it for the default: %s", raw)
	}
}

// A lease that waits for the host after it has cleared the seat keeps the same distinction through the await.
func TestADeferredCommitKeepsBeingStatedOrNotThroughTheAwait(t *testing.T) {
	m, f := commitScoped(t, commitHostMem(80, 70))
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "one figure", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	aerr := l.AwaitHostRAM(AwaitHostRAMOptions{})
	if aerr == nil {
		t.Fatalf("an unstated wait of 30 is over the 92 GiB line at 70 committed: want refused, got admitted")
	}
	if short := asHostRAM(t, aerr); !short.CommitShort || short.TouchShort {
		t.Fatalf("an unstated wait of 30 is over the 92 GiB line at 70 committed: %+v", short.HostRAMCheck)
	}
	f.setCommit(50)
	if err := l.AwaitHostRAM(AwaitHostRAMOptions{}); err != nil {
		t.Fatalf("50 + 30 fits the 92 GiB line: %v", err)
	}
	if raw := recordText(t, m, l.Epoch()); !strings.Contains(raw, `"host_ram_gib":30`) || strings.Contains(raw, "host_ram_commit_gib") {
		t.Fatalf("an unstated commit stays unstated on the record: %s", raw)
	}

	m2, _ := commitScoped(t, commitHostMem(80, 70))
	l2, err := m2.TryAcquire(ClassMedia, Options{Reason: "stated", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: 30, HostRAMWaitCommitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l2.Release() }()
	if err := l2.AwaitHostRAM(AwaitHostRAMOptions{}); err != nil {
		t.Fatalf("a stated wait of 30 fits the commit limit (70 + 30 <= 152): %v", err)
	}
	if raw := recordText(t, m2, l2.Epoch()); !strings.Contains(raw, `"host_ram_commit_gib":30`) {
		t.Fatalf("a stated commit stays stated through the await: %s", raw)
	}
}
