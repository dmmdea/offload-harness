//go:build linux

package gpulease

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// committedASGiB is Committed_AS (/proc/meminfo): the commit charge the kernel has granted, which a strict-overcommit
// host compares to CommitLimit. It counts a private writable mapping when the mapping is made, touched or not.
func committedASGiB(t *testing.T) (float64, bool) {
	t.Helper()
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "Committed_AS:") {
			f := strings.Fields(line)
			if len(f) < 2 {
				return 0, false
			}
			kb, err := strconv.ParseFloat(f[1], 64)
			return kb / (1 << 20), err == nil
		}
	}
	return 0, false
}

// The held commit the grant subtracts from a lease's declared commit must carry what the kernel has already charged for the
// lease's processes. A child that maps 256 MiB and never touches it moves Committed_AS by about that much; if the held figure
// read only the touched pages (RssAnon + VmSwap), the same memory would count twice on a strict host: once inside Committed_AS
// and once as the part of the declaration still to be charged.
func TestLinuxHeldCommitCarriesWhatTheKernelAlreadyCharges(t *testing.T) {
	if _, ok := privateBytes(os.Getpid()); !ok {
		t.Skip("no process-memory reader on this host")
	}
	const mib = 256
	before, haveAS := committedASGiB(t)
	cmd := winexec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "GPULEASE_HELPER_HOLD_MIB="+strconv.Itoa(mib), "GPULEASE_HELPER_TOUCH=0")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); _ = cmd.Wait() }()
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(out).ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		if line != "ready\n" {
			t.Fatalf("helper said %q", line)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the memory-committing child never became ready")
	}
	after, _ := committedASGiB(t)
	load, ok := descendantsHeld(os.Getpid())
	if !ok {
		t.Fatal("the process table could not be read")
	}
	charged := (after - before) * 1024
	t.Logf("a child committed %d MiB untouched: Committed_AS moved %.0f MiB; held commit %.1f MiB, held touched %.1f MiB", mib, charged, load.CommitGiB*1024, load.TouchGiB*1024)
	if load.CommitGiB*1024 < mib*0.8 {
		t.Fatalf("the child was charged its %d MiB (Committed_AS moved %.0f MiB, readable=%v) but the held commit reads %.1f MiB: a strict host would count the rest twice",
			mib, charged, haveAS, load.CommitGiB*1024)
	}
	if load.TouchGiB*1024 > mib*0.5 {
		t.Fatalf("the child touched next to nothing but the held working set reads %.1f MiB", load.TouchGiB*1024)
	}
}
