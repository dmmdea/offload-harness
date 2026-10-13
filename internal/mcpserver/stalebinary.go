package mcpserver

// The stale-binary guard. An MCP server is spawned once per client and lives as long as the client does, and a
// process keeps the executable image it started from: a release swapped in on disk afterwards is not in it. In one
// day 24 subtasks were lost to a capacity wait the next release had already fixed, because the delegators that
// served them predated it, and nothing in the server said so. The server cannot replace itself, and must not exit
// or re-exec while a call is running (that would cut the jobs it holds), so it does the one thing it can: it
// compares the stamp of the file it started from with the file on disk and, when they differ, says so on the doors
// whose behaviour a release changes (agent_delegate, offload_research, offload_status), once in the log and on
// every answer. Nothing is interrupted, and the work still runs.

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
)

// startupIdentity is the seam the identity this process captured at start is read through (buildinfo.StartupIdentity):
// a test names the image a process "started from" without capturing the test binary's own.
var startupIdentity = buildinfo.StartupIdentity

// exeStamp identifies the executable image a server runs from, cheap enough to read on every call.
type exeStamp struct {
	Size    int64
	ModTime time.Time
	// Missing: the path the process started from no longer exists (the file was replaced by unlinking it).
	Missing bool
}

func (s exeStamp) String() string {
	if s.Missing {
		return "gone"
	}
	return fmt.Sprintf("%d bytes, modified %s", s.Size, s.ModTime.UTC().Format(time.RFC3339))
}

// readOwnExecutable is the real stamp reader: the file at the path the running process was started from.
func readOwnExecutable() (exeStamp, error) {
	p, err := os.Executable()
	if err != nil {
		return exeStamp{}, err
	}
	return stampOf(p)
}

// stampOf is the stamp of the file at path p: Missing when it is gone, an error for anything else that stops the read.
func stampOf(p string) (exeStamp, error) {
	fi, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return exeStamp{Missing: true}, nil
		}
		return exeStamp{}, err
	}
	return exeStamp{Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// exeWatch remembers the stamp of the executable at start and compares it with the file on disk now.
type exeWatch struct {
	read func() (exeStamp, error)
	// started is the stamp read when the server was built; known says it was readable. A server that could not
	// read its own file at start has nothing to compare with and never accuses, and says that it does not watch
	// (inactive, in the log once and on the status door): a guard whose whole purpose is to stop a silent failure
	// must not be one.
	started  exeStamp
	known    bool
	inactive string
	logged   sync.Once
	// blind is the last stat error that was not "the path is gone" (nil while the stamp reads). The first failure
	// is logged once (blindLogged); the status door shows it for as long as it lasts.
	mu          sync.Mutex
	blind       error
	blindLogged sync.Once
}

func newExeWatch(read func() (exeStamp, error)) *exeWatch {
	st, err := read()
	return newExeWatchFrom(st, err, read)
}

// newServerExeWatch is the guard New builds. When the process captured its startup identity (runMCP does, as the first
// thing it does: buildinfo.CaptureStartup), the guard starts from THAT image: the stamp the capture read through the
// handle it hashed, compared with the file at the path the capture recorded. New runs after the config is loaded and the
// pipeline opened, and a deploy that lands in between must not be taken for the image the process runs: reading the
// stamp here, as the guard used to, re-opened the window the capture closed (D-36). A process that captured nothing (a
// test, a short CLI call) starts from its own file as of now.
func newServerExeWatch() *exeWatch {
	if st, ok := startupIdentity(); ok && st.Path != "" && st.Size > 0 {
		path := st.Path
		return newExeWatchFrom(exeStamp{Size: st.Size, ModTime: st.ModTime}, nil, func() (exeStamp, error) { return stampOf(path) })
	}
	return newExeWatch(readOwnExecutable)
}

// newExeWatchFrom builds the guard on a start stamp already read (st, err as a read returned them).
func newExeWatchFrom(st exeStamp, err error, read func() (exeStamp, error)) *exeWatch {
	w := &exeWatch{read: read}
	switch {
	case err != nil:
		w.inactive = "the stamp of its own executable could not be read at start: " + err.Error()
	case st.Missing:
		w.inactive = "the executable it started from is already gone from its path"
	default:
		w.started, w.known = st, true
	}
	if w.inactive != "" {
		log.Printf("mcpserver: the stale-binary guard is inactive: %s. A binary swapped in on disk after this point will not be reported by this server.", w.inactive)
	}
	return w
}

// guardState is "" while the guard is watching, and otherwise the sentence the status door carries: why it is not
// (inactive since start) or that its stamp cannot be read right now (the stat error).
func (w *exeWatch) guardState() string {
	if w == nil {
		return ""
	}
	if w.inactive != "" {
		return "inactive: " + w.inactive
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.blind != nil {
		return "unreadable: the stamp of the executable could not be read on the last check (" + w.blind.Error() + "), so no image change is reported until it can"
	}
	return ""
}

// staleNote is "" while the file on disk is the one this process started from, and otherwise the sentence the
// doors carry. A stat that fails for any reason but "the path is gone" proves nothing about the image, so it accuses
// nothing; it is logged once and shown on the status door while it lasts (guardState).
//
// This is the stamp alone: staleNoteWith without a comparison of bytes, which is what a watch with nothing to compare
// against decides on.
func (w *exeWatch) staleNote(version string) string { return w.staleNoteWith(version, nil) }

// staleNoteWith is staleNote with the comparison of bytes. A stamp that has moved is not yet a different image: a
// redeploy of the same build rewrites the file and leaves its bytes alone. compare (nil when the process holds no
// startup identity) is the installed check offload_status's process block makes, and it settles the question the way
// that block does: equal bytes are not stale, different bytes are (and the note names the installed build when its
// marker reads), and bytes that cannot be compared leave the moved stamp standing, said so, because a guard against a
// silent failure must not go quiet when it cannot look. A path that is gone is stale without a comparison: there is
// nothing to read.
func (w *exeWatch) staleNoteWith(version string, compare func() imageCheck) string {
	if w == nil || !w.known || w.read == nil {
		return ""
	}
	now, err := w.read()
	w.mu.Lock()
	w.blind = err
	w.mu.Unlock()
	if err != nil {
		w.blindLogged.Do(func() {
			log.Printf("mcpserver: the stale-binary guard could not read the stamp of the executable (%v); it reports nothing while it cannot", err)
		})
		return ""
	}
	if !now.Missing && now.Size == w.started.Size && now.ModTime.Equal(w.started.ModTime) {
		return ""
	}
	var chk imageCheck
	if !now.Missing && compare != nil {
		if chk = compare(); chk.stale != nil && !*chk.stale {
			return ""
		}
	}
	var evidence string
	switch {
	case chk.stale != nil && chk.installedVersion != "":
		evidence = "; the installed build is " + chk.installedVersion
	case chk.stale == nil && compare != nil && !now.Missing:
		evidence = "; its bytes could not be compared with the image it started from, so this rests on the changed size or time alone"
		if chk.note != "" {
			// Why, as offload_status's process block says it: a lock on the freshly renamed file reads differently from a
			// start-up hash that failed, and the log line below is the only other place this is written down.
			evidence += "; the comparison said: " + chk.note
		}
	}
	note := fmt.Sprintf("this MCP server is running %s from an executable image that has changed on disk since it started "+
		"(it started from %s; now %s%s). A long-lived server keeps the image it started from, so a fix shipped since is not in "+
		"this process: restart the MCP client to load it. Nothing has been interrupted and this call ran as it always does.",
		version, w.started, nowWords(now), evidence)
	w.logged.Do(func() { log.Printf("mcpserver: %s", note) })
	return note
}

func nowWords(now exeStamp) string {
	if now.Missing {
		return "the file is no longer at its path"
	}
	return now.String()
}

// staleBinaryFields is the pair a delegating door's answer carries: the stale_binary note ("" = none) and the guard's own
// state ("" while it is watching: stale_binary_guard). The state is read after the note, because the note's read of the stamp
// is what finds a stamp that cannot be read right now. A guard that is not watching says so on every door whose answer it
// would have annotated, not only on offload_status: a blind guard is otherwise a server that reads as fresh.
func (s *Server) staleBinaryFields(version string) (note, guard string) {
	note = s.staleBinaryNote(version)
	return note, s.exe.guardState()
}

// staleBinaryNote is the server's note for a door's answer ("" = none). The comparison of bytes is the process block's
// own (s.installed.check against the identity the server holds), so the two signals share one capture, one cache and one
// definition; a server that holds no identity (the test seam) has nothing to compare and the stamp decides.
func (s *Server) staleBinaryNote(version string) string {
	var compare func() imageCheck
	if st := s.startup; st != nil {
		compare = func() imageCheck { return s.installed.check(*st) }
	}
	return s.exe.staleNoteWith(version, compare)
}
