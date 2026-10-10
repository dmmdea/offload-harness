package gpulease

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// holdHelper is the test binary acting as a process that holds memory: it touches the number of MiB
// GPULEASE_HELPER_HOLD_MIB names, says so on stdout, and waits for its stdin to close. It stands in
// for a ComfyUI that has loaded its weights, so the process-tree reader is tested against a real
// child and not against a fake table.
func holdHelper() int {
	mib, err := strconv.Atoi(os.Getenv("GPULEASE_HELPER_HOLD_MIB"))
	if err != nil || mib <= 0 {
		return 2
	}
	buf := make([]byte, mib<<20)
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 1 // touch every page: allocated but untouched memory is not private memory yet
	}
	os.Stdout.WriteString("ready\n")
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
	if buf[0] == 0 {
		return 3 // keeps buf live to here
	}
	return 0
}

func TestParseStatusPrivate(t *testing.T) {
	const status = "Name:\tpython\nVmRSS:\t  900000 kB\nRssAnon:\t  800000 kB\nRssFile:\t  100000 kB\nVmSwap:\t   50000 kB\n"
	got, ok := parseStatusPrivate(status)
	if !ok || got != (800000+50000)*1024 {
		t.Fatalf("anonymous resident + swapped = %d (ok=%v), want %d", got, ok, (800000+50000)*1024)
	}
	if _, ok := parseStatusPrivate("Name:\told\nVmRSS:\t900000 kB\n"); ok {
		t.Fatal("a kernel without RssAnon must read as unreadable, not as VmRSS (which counts shared file pages)")
	}
	if _, ok := parseStatusPrivate(""); ok {
		t.Fatal("an empty status is unreadable")
	}
}

// The reader sees this process's own private memory grow when it touches pages. (On a platform with
// no reader the bool says so and the test has nothing to measure.)
func TestPrivateBytesSeesMemoryThisProcessTouches(t *testing.T) {
	before, ok := privateBytes(os.Getpid())
	if !ok {
		t.Skip("no private-memory reader on this platform (a lease's whole declared need then counts as still to load)")
	}
	if before == 0 {
		t.Fatal("a running process cannot hold zero private bytes")
	}
	buf := make([]byte, 128<<20)
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 1
	}
	after, _ := privateBytes(os.Getpid())
	if buf[len(buf)-4096] == 2 { // keeps buf live past the second reading
		t.Fatal("unreachable")
	}
	if after < before+100<<20 {
		t.Fatalf("touching 128 MiB moved private bytes from %d to %d", before, after)
	}
	if _, ok := privateBytes(0); ok {
		t.Fatal("pid 0 is not a process")
	}
}

// descendantsPrivateGiB walks the tree below a holder: a child that holds 96 MiB is counted, and the
// holder's own memory (the long-lived server of a pipeline lease) is not.
func TestDescendantsPrivateGiBCountsTheChildrenAndNotTheHolder(t *testing.T) {
	if _, ok := privateBytes(os.Getpid()); !ok {
		t.Skip("no private-memory reader on this platform")
	}
	const mib = 96
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "GPULEASE_HELPER_HOLD_MIB="+strconv.Itoa(mib))
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
		t.Fatal("the memory-holding child never became ready")
	}
	got, ok := descendantsPrivateGiB(os.Getpid())
	if !ok {
		t.Fatal("the process table could not be read")
	}
	if got < float64(mib)*0.9/1024 {
		t.Fatalf("a child holding %d MiB: descendants hold %.3f GiB", mib, got)
	}
	// Nothing is below the child itself.
	if leaf, ok := descendantsPrivateGiB(cmd.Process.Pid); !ok || leaf > 0.05 {
		t.Fatalf("the holder's own memory must not count: a leaf holder reads %.3f GiB (ok=%v)", leaf, ok)
	}
}
