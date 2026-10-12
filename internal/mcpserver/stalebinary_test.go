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
