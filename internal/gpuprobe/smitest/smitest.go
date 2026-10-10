// Package smitest is test support for code that reads the card table through nvidia-smi: a stand-in
// nvidia-smi that is the test binary itself, put first on PATH, answering as slowly as a loaded box does.
//
// It exists because every other test of the card-table read injects a reader, so the production reader
// (gpuprobe.Read: the PATH lookup, exec.CommandContext, the kill at the deadline, the parse) never ran
// under test, and what the allocation's longer retry relies on, that no reader below it adds a deadline
// of its own, rested on a comment. It is a separate package so the production gpuprobe package never
// imports "testing" (the shape internal/rosterprobe/rostertest has).
//
// How it works. A package that uses it calls MaybeRun first thing in its TestMain. Install copies the
// running test binary to <dir>/nvidia-smi[.exe], puts <dir> first on PATH and names a state directory in
// the environment. When that copy is exec'd it finds itself in MaybeRun (the environment names the state
// directory AND the executable is called nvidia-smi), records its argv, sleeps as the script says and
// prints the per-device table. The same stand-in works on Windows and Linux with no shell, no
// interpreter and no second binary (internal/gpugen's engine stub is the precedent), and a call that its
// caller's deadline kills dies the way nvidia-smi does: it is one process, so its pipe closes with it.
//
// The stand-in is strict on purpose. It answers the per-device query and nothing else: any other
// invocation (the process listing, -L, a compute-apps query) is recorded and refused with exit 99, so a
// test that asserts on Calls sees every invocation a code path made, not only the ones that succeeded.
package smitest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// The synthetic cards the stand-in reports (repeated-nibble heads, the shape the leak gate treats as
// placeholders). Card B is the one a monitor is attached to.
const (
	UUIDA = "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee" // nvidia index 0
	UUIDB = "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff" // nvidia index 1, the monitor is attached
	UUIDC = "GPU-cccc3333-dddd-eeee-ffff-000000000000" // nvidia index 2
)

const (
	stateEnv = "SMITEST_STATE" // names the state directory; set on every process of a test that Installed
	warmArg  = "--smitest-warm"
)

// Step is what one call of the stand-in does. Calls take the steps in order and the last step repeats.
type Step struct {
	// Delay is how long the call takes before it answers. A delay past the caller's deadline is a hang: the
	// caller kills the process, which is the point.
	Delay time.Duration
}

// Stand is an installed stand-in.
type Stand struct{ state string }

type script struct {
	DelaysMs []int64 `json:"delays_ms"`
}

// Install puts the stand-in first on PATH for the rest of the test and returns it. With no steps every call
// answers at once. It also starts the stand-in once, uncounted, so the first call a test makes is not the
// first time the operating system has run the copy (a virus scanner's look at a new executable has taken
// seconds, longer than the deadline a test gives a call to start).
func Install(t testing.TB, steps ...Step) *Stand {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("smitest: cannot find the test binary: %v", err)
	}
	state := t.TempDir()
	bin := filepath.Join(state, "bin")
	for _, d := range []string{bin, filepath.Join(state, "calls")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("smitest: %v", err)
		}
	}
	name := "nvidia-smi"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(bin, name)
	if err := copyExecutable(exe, dst); err != nil {
		t.Fatalf("smitest: copying the test binary to %s: %v", dst, err)
	}
	sc := script{DelaysMs: make([]int64, len(steps))}
	for i, s := range steps {
		sc.DelaysMs[i] = s.Delay.Milliseconds()
	}
	b, _ := json.Marshal(sc)
	if err := os.WriteFile(filepath.Join(state, "script.json"), b, 0o644); err != nil {
		t.Fatalf("smitest: %v", err)
	}
	t.Setenv(stateEnv, state)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// A driver that refused display_attached would be remembered across tests; this one never does, but a
	// test that ran before this one may have armed the memory.
	gpuprobe.ResetDisplayAwareState()
	t.Cleanup(gpuprobe.ResetDisplayAwareState)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, dst, warmArg).CombinedOutput(); err != nil {
		t.Fatalf("smitest: the stand-in does not start (does this package's TestMain call smitest.MaybeRun first?): %v\n%s", err, out)
	}
	return &Stand{state: state}
}

// Calls is the argv (without the program name) of every call the stand-in has taken, in order. A call
// records itself the moment it starts, before it sleeps, so one that its caller killed still counts.
func (s *Stand) Calls() [][]string {
	entries, err := os.ReadDir(filepath.Join(s.state, "calls"))
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var out [][]string
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(s.state, "calls", n))
		if err != nil {
			continue
		}
		var args []string
		if json.Unmarshal(b, &args) == nil {
			out = append(out, args)
		}
	}
	return out
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// MaybeRun is the first statement of a TestMain of a package that Installs the stand-in: when this process
// is the stand-in (the environment names its state directory and the executable is called nvidia-smi) it
// plays nvidia-smi and exits; otherwise it returns at once. Test flags are not parsed before m.Run, so the
// stand-in's own arguments (--query-gpu=...) never reach the testing package.
func MaybeRun() {
	state := os.Getenv(stateEnv)
	if state == "" || len(os.Args) == 0 {
		return
	}
	if strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe") != "nvidia-smi" {
		return
	}
	os.Exit(play(state, os.Args[1:]))
}

// row is one synthetic card.
type row struct {
	index             int
	uuid, name        string
	totalMiB, usedMiB int
	util              int
	active, attached  string
}

var rows = []row{
	{0, UUIDA, "NVIDIA GeForce RTX 5060 Ti", 16311, 867, 3, "Disabled", "No"},
	{1, UUIDB, "NVIDIA GeForce RTX 5070 Ti", 16303, 900, 5, "Disabled", "Yes"},
	{2, UUIDC, "NVIDIA GeForce RTX 5060 Ti", 16311, 1200, 100, "Disabled", "No"},
}

func field(r row, f string) (string, bool) {
	switch f {
	case "index":
		return strconv.Itoa(r.index), true
	case "uuid":
		return r.uuid, true
	case "name":
		return r.name, true
	case "memory.total":
		return strconv.Itoa(r.totalMiB), true
	case "memory.used":
		return strconv.Itoa(r.usedMiB), true
	case "utilization.gpu":
		return strconv.Itoa(r.util), true
	case "display_active":
		return r.active, true
	case "display_attached":
		return r.attached, true
	}
	return "", false
}

func play(state string, args []string) int {
	if len(args) == 1 && args[0] == warmArg {
		return 0
	}
	n, ok := record(state, args)
	if !ok {
		fmt.Fprintln(os.Stderr, "smitest: could not record the call")
		return 98
	}
	var fields []string
	format := false
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--query-gpu="):
			fields = strings.Split(strings.TrimPrefix(a, "--query-gpu="), ",")
		case a == "--format=csv,noheader,nounits":
			format = true
		default:
			fmt.Fprintf(os.Stderr, "smitest: this stand-in answers only the per-device query (--query-gpu=... --format=csv,noheader,nounits), got %q\n", args)
			return 99
		}
	}
	if len(fields) == 0 || !format {
		fmt.Fprintf(os.Stderr, "smitest: this stand-in answers only the per-device query (--query-gpu=... --format=csv,noheader,nounits), got %q\n", args)
		return 99
	}
	for _, f := range fields {
		if _, known := field(rows[0], strings.TrimSpace(f)); !known {
			// What nvidia-smi says of a field its driver does not know (on stdout, status 2).
			fmt.Printf("Field \"%s\" is not a valid field to query.\n", strings.TrimSpace(f))
			return 2
		}
	}
	if d := delayOf(state, n); d > 0 {
		time.Sleep(d)
	}
	eol := "\n"
	if runtime.GOOS == "windows" {
		eol = "\r\n"
	}
	for _, r := range rows {
		vals := make([]string, 0, len(fields))
		for _, f := range fields {
			v, _ := field(r, strings.TrimSpace(f))
			vals = append(vals, v)
		}
		fmt.Print(strings.Join(vals, ", ") + eol)
	}
	return 0
}

// record claims the next call number and writes the argv under it. The file is created exclusively, so
// two calls that start together never share a number.
func record(state string, args []string) (int, bool) {
	b, _ := json.Marshal(args)
	for n := 0; n < 10000; n++ {
		f, err := os.OpenFile(filepath.Join(state, "calls", fmt.Sprintf("%04d.json", n)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return 0, false
		}
		_, werr := f.Write(b)
		cerr := f.Close()
		return n, werr == nil && cerr == nil
	}
	return 0, false
}

func delayOf(state string, n int) time.Duration {
	b, err := os.ReadFile(filepath.Join(state, "script.json"))
	if err != nil {
		return 0
	}
	var sc script
	if json.Unmarshal(b, &sc) != nil || len(sc.DelaysMs) == 0 {
		return 0
	}
	i := n
	if i >= len(sc.DelaysMs) {
		i = len(sc.DelaysMs) - 1
	}
	return time.Duration(sc.DelaysMs[i]) * time.Millisecond
}
