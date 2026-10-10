package gpulease

import (
	"bufio"
	"os"
	"os/exec"
	"runtime"
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
	if os.Getenv("GPULEASE_HELPER_TOUCH") == "0" {
		// COMMIT the memory and leave it untouched: the process is promised mib MiB it never occupies.
		// This is the shape of a GPU allocation charged to a process's commit without ever being system
		// RAM, reproduced with plain memory so the difference between private and resident is measured on
		// a real process instead of assumed.
		os.Stdout.WriteString("ready\n")
		_, _ = bufio.NewReader(os.Stdin).ReadByte()
		_ = buf[len(buf)-1]
		return 0
	}
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

// descendantsResidentGiB walks the tree below a holder: a child that holds 96 MiB is counted, and the
// holder's own memory (the long-lived server of a pipeline lease) is not.
func TestDescendantsResidentGiBCountsTheChildrenAndNotTheHolder(t *testing.T) {
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
	got, ok := descendantsResidentGiB(os.Getpid())
	if !ok {
		t.Fatal("the process table could not be read")
	}
	if got < float64(mib)*0.9/1024 {
		t.Fatalf("a child holding %d MiB: descendants hold %.3f GiB", mib, got)
	}
	// Nothing is below the child itself.
	if leaf, ok := descendantsResidentGiB(cmd.Process.Pid); !ok || leaf > 0.05 {
		t.Fatalf("the holder's own memory must not count: a leaf holder reads %.3f GiB (ok=%v)", leaf, ok)
	}
}

func TestParseStatusResident(t *testing.T) {
	const status = "Name:\tpython\nVmRSS:\t  900000 kB\nRssAnon:\t  800000 kB\nRssFile:\t  100000 kB\nVmSwap:\t   50000 kB\n"
	got, ok := parseStatusResident(status)
	if !ok || got != 900000*1024 {
		t.Fatalf("the resident set is VmRSS: got %d (ok=%v), want %d", got, ok, 900000*1024)
	}
	for name, text := range map[string]string{"empty": "", "no VmRSS line": "Name:\tx\nRssAnon:\t1 kB\n", "not a number": "VmRSS:\tmany kB\n"} {
		if _, ok := parseStatusResident(text); ok {
			t.Errorf("%s must read as unreadable", name)
		}
	}
}

// TreeMemory is what the measurement path samples while a render runs (G3 of the P0 plan): the root's own
// memory AND everything below it, private and resident. A child that holds 96 MiB is in the sum, and the root
// (unlike descendantsResidentGiB, which leaves a long-lived holder out) is too.
func TestTreeMemoryCountsTheRootAndTheChildren(t *testing.T) {
	if _, ok := privateBytes(os.Getpid()); !ok {
		t.Skip("no process-memory reader on this platform")
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

	rootPriv, _ := privateBytes(os.Getpid())
	priv, res, err := TreeMemory(os.Getpid())
	if err != nil {
		t.Fatalf("the process table could not be read: %v", err)
	}
	if want := (float64(rootPriv) + float64(mib)*0.9*(1<<20)) / (1 << 30); priv < want {
		t.Fatalf("root + a child holding %d MiB: tree private %.3f GiB, want at least %.3f", mib, priv, want)
	}
	if res < float64(mib)*0.9/1024 {
		t.Fatalf("the child touched %d MiB, so the tree's resident set holds at least that: %.3f GiB", mib, res)
	}
	// A leaf is its own whole tree: the child's memory and nothing of the root's.
	leafPriv, _, err := TreeMemory(cmd.Process.Pid)
	if err != nil || leafPriv < float64(mib)*0.9/1024 || leafPriv > priv {
		t.Fatalf("a leaf reads its own memory only: %.3f GiB (err %v), root tree %.3f GiB", leafPriv, err, priv)
	}
}

// G3: private and resident are different quantities, and the not-yet-loaded sum is in RAM units. A child that is
// COMMITTED 256 MiB it never touches (the shape of a GPU allocation charged to a process's commit) reads as
// 256 MiB private and next to nothing resident, and the held figure the grant subtracts a lease's declared need
// by must not count it: the RAM it was declared to load is still to come.
func TestHeldCountsWhatIsResidentNotWhatIsMerelyCommitted(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("untouched anonymous memory is not private on Linux (RssAnon), so there is no difference to show here")
	}
	const mib = 256
	cmd := exec.Command(os.Args[0])
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
	priv, res, err := TreeMemory(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("measured on this process model: committed %d MiB untouched -> private %.0f MiB, resident %.0f MiB", mib, priv*1024, res*1024)
	if priv*1024 < mib*0.8 {
		t.Skipf("this runtime did not commit the allocation (private %.0f MiB): the distinction cannot be shown here", priv*1024)
	}
	if res*1024 > mib*0.5 {
		t.Fatalf("untouched committed memory must not read as resident: private %.0f MiB, resident %.0f MiB", priv*1024, res*1024)
	}
	// The grant's held figure is the resident one: the child holds next to nothing of the RAM a lease declared.
	if held, ok := descendantsResidentGiB(os.Getpid()); !ok || held*1024 > mib*0.5 {
		t.Fatalf("held (resident) = %.0f MiB (ok=%v): committed-but-untouched memory was counted as loaded", held*1024, ok)
	}
}
