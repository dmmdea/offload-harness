package mcpserver

// A long-lived MCP server keeps the executable image it started from. A fix that ships afterwards (a new binary
// swapped in on disk) is not in that process, and nothing said so: in one day 24 subtasks were lost to a capacity
// wait that the next release had already fixed, because the delegators that served them had started before it and
// were never restarted. The server cannot (and must not, mid-call) replace itself, but it can notice that the file
// it started from changed and say so on the doors whose behaviour a fix changes.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/research"
)

// staleMovableExe is a fake executable on disk: the stamp the server reads, changeable by the test.
type staleMovableExe struct {
	mu  sync.Mutex
	cur exeStamp
	err error
}

func (m *staleMovableExe) read() (exeStamp, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur, m.err
}

func (m *staleMovableExe) set(st exeStamp, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cur, m.err = st, err
}

var staleExeThen = exeStamp{Size: 24_117_248, ModTime: time.Date(2026, 10, 12, 2, 43, 0, 0, time.UTC)}
var staleExeNow = exeStamp{Size: 24_330_240, ModTime: time.Date(2026, 10, 12, 4, 51, 21, 0, time.UTC)}

func staleLocalRun() func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
	return func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "fake-seat", Output: "done",
			Structured: json.RawMessage(`{"summary":"x"}`), StopReason: "done"}, nil
	}
}

const staleDelegateArgs = `{"subtasks":[{"goal":"say done"}],"route":"local"}`

func staleResearchServer(t *testing.T) *Server {
	t.Helper()
	s := delegateTestServer(t, staleLocalRun())
	s.researchFetch = func(_ context.Context, urls []string, _ research.Options) []research.Fetched {
		out := make([]research.Fetched, len(urls))
		for i, u := range urls {
			out[i] = research.Fetched{URL: u, Status: 200, Title: "Page", Text: "A short page about widgets.", Bytes: 28, TextBytes: 28}
		}
		return out
	}
	return s
}

const staleResearchArgs = `{"goal":"digest the page","urls":["https://docs.example/a"],"route":"local","output_schema":{"properties":{"summary":{"type":"string"}}}}`

// TestAServerWhoseExecutableChangedSaysSoOnTheDelegatingDoors: the file on disk is not the one the server started
// from; agent_delegate, offload_research and offload_status carry a stale_binary note, and the work still runs.
func TestAServerWhoseExecutableChangedSaysSoOnTheDelegatingDoors(t *testing.T) {
	exe := &staleMovableExe{cur: staleExeThen}
	check := func(t *testing.T, door string, m map[string]any) {
		t.Helper()
		note, _ := m["stale_binary"].(string)
		if note == "" {
			t.Fatalf("%s: no stale_binary note in %v", door, staleKeys(m))
		}
		for _, want := range []string{buildinfo.Version, "restart", "changed on disk"} {
			if !strings.Contains(strings.ToLower(note), strings.ToLower(want)) {
				t.Errorf("%s: note %q does not say %q", door, note, want)
			}
		}
	}

	t.Run("agent_delegate", func(t *testing.T) {
		s := delegateTestServer(t, staleLocalRun())
		s.exe = newExeWatch(exe.read)
		exe.set(staleExeThen, nil)
		res, err := s.handleAgentDelegate(context.Background(), callReq(staleDelegateArgs))
		if err != nil {
			t.Fatal(err)
		}
		if m := decodeResult(t, res); m["stale_binary"] != nil {
			t.Fatalf("a server whose executable has not changed carries a note: %v", m["stale_binary"])
		}
		exe.set(staleExeNow, nil)
		res, err = s.handleAgentDelegate(context.Background(), callReq(staleDelegateArgs))
		if err != nil {
			t.Fatal(err)
		}
		m := decodeResult(t, res)
		check(t, "agent_delegate", m)
		if sum, _ := m["summary"].(map[string]any); sum["succeeded"] != float64(1) {
			t.Errorf("the work did not run on a stale server: %v", m["summary"])
		}
	})
	t.Run("offload_research", func(t *testing.T) {
		exe.set(staleExeThen, nil)
		s := staleResearchServer(t)
		s.exe = newExeWatch(exe.read)
		exe.set(staleExeNow, nil)
		res, err := s.handleResearch(context.Background(), callReq(staleResearchArgs))
		if err != nil {
			t.Fatal(err)
		}
		check(t, "offload_research", decodeResult(t, res))
	})
	t.Run("offload_status", func(t *testing.T) {
		exe.set(staleExeThen, nil)
		s := delegateTestServer(t, staleLocalRun())
		s.exe = newExeWatch(exe.read)
		exe.set(staleExeNow, nil)
		res, err := s.handleStatus(context.Background(), callReq(`{"section":"brief"}`))
		if err != nil {
			t.Fatal(err)
		}
		check(t, "offload_status", decodeResult(t, res))
	})
}

// TestAServerWhoseExecutableWasReplacedOrRemovedIsStaleToo: on a platform that unlinks the old file, the path the
// process started from no longer exists.
func TestAServerWhoseExecutableWasReplacedOrRemovedIsStaleToo(t *testing.T) {
	exe := &staleMovableExe{cur: staleExeThen}
	w := newExeWatch(exe.read)
	exe.set(exeStamp{Missing: true}, nil)
	if note := w.staleNote(buildinfo.Version); !strings.Contains(note, "no longer") {
		t.Fatalf("note %q, want it to say the executable is no longer at its path", note)
	}
}

// TestAnUnreadableExecutableIsNeverAccused: a stat that fails for any other reason proves nothing, so no note, and a
// server that could not read its own file at start has nothing to compare with.
func TestAnUnreadableExecutableIsNeverAccused(t *testing.T) {
	exe := &staleMovableExe{cur: staleExeThen}
	w := newExeWatch(exe.read)
	exe.set(exeStamp{}, errors.New("permission denied"))
	if note := w.staleNote(buildinfo.Version); note != "" {
		t.Fatalf("note %q on a failed stat, want none", note)
	}
	blind := newExeWatch((&staleMovableExe{err: errors.New("no executable path")}).read)
	exe2 := &staleMovableExe{cur: staleExeNow}
	blind.read = exe2.read
	if note := blind.staleNote(buildinfo.Version); note != "" {
		t.Fatalf("note %q from a watch that never read its start stamp, want none", note)
	}
}

// TestAFileTouchedBackToTheStartStampIsNotStale: the comparison is the stamp, not an event.
func TestAFileTouchedBackToTheStartStampIsNotStale(t *testing.T) {
	exe := &staleMovableExe{cur: staleExeThen}
	w := newExeWatch(exe.read)
	exe.set(staleExeNow, nil)
	if w.staleNote(buildinfo.Version) == "" {
		t.Fatal("setup: a changed file is not noticed")
	}
	exe.set(staleExeThen, nil)
	if note := w.staleNote(buildinfo.Version); note != "" {
		t.Fatalf("note %q once the file is the one the server started from again", note)
	}
}

func staleKeys(m map[string]any) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// TestEitherThingThatDiffersMakesTheImageStale: a size change alone and a modification time alone each count; a
// release is usually both, but a rebuilt file of the same size is still another image.
func TestEitherThingThatDiffersMakesTheImageStale(t *testing.T) {
	for name, changed := range map[string]exeStamp{
		"size only":  {Size: staleExeNow.Size, ModTime: staleExeThen.ModTime},
		"mtime only": {Size: staleExeThen.Size, ModTime: staleExeNow.ModTime},
	} {
		exe := &staleMovableExe{cur: staleExeThen}
		w := newExeWatch(exe.read)
		exe.set(changed, nil)
		if w.staleNote(buildinfo.Version) == "" {
			t.Errorf("%s: a changed file is not noticed", name)
		}
	}
}

// TestAMissingFileIsAStampOfItsOwn: a path that is gone reads as Missing, an unreadable one as an error, and a file
// as its size and modification time.
func TestAMissingFileIsAStampOfItsOwn(t *testing.T) {
	dir := t.TempDir()
	if st, err := stampOf(filepath.Join(dir, "gone.exe")); err != nil || !st.Missing {
		t.Fatalf("stampOf(missing) = %+v, %v, want Missing", st, err)
	}
	p := filepath.Join(dir, "here.exe")
	if err := os.WriteFile(p, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, err := stampOf(p); err != nil || st.Missing || st.Size != 5 || st.ModTime.IsZero() {
		t.Fatalf("stampOf(file) = %+v, %v, want 5 bytes and a time", st, err)
	}
}

// TestAServerOnItsOwnExecutableIsNotStale: the real reader against the running test binary, which nobody replaced.
func TestAServerOnItsOwnExecutableIsNotStale(t *testing.T) {
	st, err := readOwnExecutable()
	if err != nil || st.Missing || st.Size <= 0 {
		t.Fatalf("readOwnExecutable = %+v, %v, want the stamp of the running executable", st, err)
	}
	s := delegateTestServer(t, staleLocalRun())
	if !s.exe.known {
		t.Fatal("a server built by New could not read its own executable")
	}
	if note := s.staleBinaryNote(buildinfo.Version); note != "" {
		t.Fatalf("a server on the executable it started from carries the note %q", note)
	}
}

// staleLog redirects the standard logger into a buffer for one test (the pattern of rungraph_tempfiles_test.go); the
// buffer takes a lock because the logger's writers and the test's reader may be different goroutines.
type staleLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *staleLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *staleLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func captureStaleLog(t *testing.T) *staleLog {
	t.Helper()
	l := &staleLog{}
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(l)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return l
}

// TestAGuardThatCannotStampItsOwnExecutableSaysSo: the guard exists to stop a silent failure, so it must not become one.
// A server that cannot read the stamp of its own executable at start has nothing to compare with and never accuses, which
// from outside is a server that is fresh; the log and the status door say it is not watching.
func TestAGuardThatCannotStampItsOwnExecutableSaysSo(t *testing.T) {
	logged := captureStaleLog(t)
	exe := &staleMovableExe{err: errors.New("no executable path")}
	s := delegateTestServer(t, staleLocalRun())
	s.exe = newExeWatch(exe.read)

	if got := logged.String(); !strings.Contains(got, "stale-binary guard") || !strings.Contains(got, "inactive") || !strings.Contains(got, "no executable path") {
		t.Fatalf("log %q: want one line saying the guard is inactive and why", got)
	}
	for i := 0; i < 3; i++ {
		if note := s.staleBinaryNote(buildinfo.Version); note != "" {
			t.Fatalf("an inactive guard accused: %q", note)
		}
	}
	if n := strings.Count(logged.String(), "inactive"); n != 1 {
		t.Fatalf("the inactive line was logged %d times, want once", n)
	}

	res, err := s.handleStatus(context.Background(), callReq(`{"section":"brief"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	guard, _ := m["stale_binary_guard"].(string)
	if !strings.Contains(guard, "inactive") || !strings.Contains(guard, "no executable path") {
		t.Fatalf("status carries stale_binary_guard %q (keys %v), want it to say the guard is inactive and why", guard, staleKeys(m))
	}
	if m["stale_binary"] != nil {
		t.Fatalf("an inactive guard put a stale_binary note on the status door: %v", m["stale_binary"])
	}
}

// TestAGuardWhoseExecutableIsGoneAtStartSaysSoToo: a path that is already missing at start is no baseline either.
func TestAGuardWhoseExecutableIsGoneAtStartSaysSoToo(t *testing.T) {
	logged := captureStaleLog(t)
	exe := &staleMovableExe{cur: exeStamp{Missing: true}}
	w := newExeWatch(exe.read)
	if w.known || !strings.Contains(logged.String(), "inactive") || !strings.Contains(logged.String(), "gone") {
		t.Fatalf("known=%v log=%q, want an inactive guard that says the executable was gone at start", w.known, logged.String())
	}
}

// TestAStatFailureLaterIsLoggedOnceAndShownOnTheStatusDoor: a stat that fails after a good start still proves nothing about
// the image (no accusation), but the guard is blind for as long as it fails, and says that once in the log and while it
// lasts on the status door.
func TestAStatFailureLaterIsLoggedOnceAndShownOnTheStatusDoor(t *testing.T) {
	logged := captureStaleLog(t)
	exe := &staleMovableExe{cur: staleExeThen}
	s := delegateTestServer(t, staleLocalRun())
	s.exe = newExeWatch(exe.read)
	if strings.Contains(logged.String(), "inactive") {
		t.Fatalf("a guard that started well logged %q", logged.String())
	}
	exe.set(exeStamp{}, errors.New("permission denied"))
	for i := 0; i < 4; i++ {
		if note := s.staleBinaryNote(buildinfo.Version); note != "" {
			t.Fatalf("a failed stat accused: %q", note)
		}
	}
	if n := strings.Count(logged.String(), "permission denied"); n != 1 {
		t.Fatalf("the failure was logged %d times (log %q), want once", n, logged.String())
	}
	res, err := s.handleStatus(context.Background(), callReq(`{"section":"brief"}`))
	if err != nil {
		t.Fatal(err)
	}
	if guard, _ := decodeResult(t, res)["stale_binary_guard"].(string); !strings.Contains(guard, "permission denied") {
		t.Fatalf("status stale_binary_guard %q, want the failure while it lasts", guard)
	}
	// it reads again: the guard is back, the status door stops saying so, and a NEW failure is a new line
	exe.set(staleExeThen, nil)
	res, err = s.handleStatus(context.Background(), callReq(`{"section":"brief"}`))
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeResult(t, res); m["stale_binary_guard"] != nil {
		t.Fatalf("status still carries stale_binary_guard %v after the stamp could be read again", m["stale_binary_guard"])
	}
}

// TestAWatchingGuardAddsNothingToTheStatusDoor: the key exists only when the guard is not watching.
func TestAWatchingGuardAddsNothingToTheStatusDoor(t *testing.T) {
	captureStaleLog(t)
	s := delegateTestServer(t, staleLocalRun())
	res, err := s.handleStatus(context.Background(), callReq(`{"section":"brief"}`))
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeResult(t, res); m["stale_binary_guard"] != nil {
		t.Fatalf("a watching guard put stale_binary_guard %v on the status door", m["stale_binary_guard"])
	}
}
