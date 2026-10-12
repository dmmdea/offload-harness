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
)

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
	// read its own file at start has nothing to compare with and never accuses.
	started exeStamp
	known   bool
	logged  sync.Once
}

func newExeWatch(read func() (exeStamp, error)) *exeWatch {
	w := &exeWatch{read: read}
	if st, err := read(); err == nil && !st.Missing {
		w.started, w.known = st, true
	}
	return w
}

// staleNote is "" while the file on disk is the one this process started from, and otherwise the sentence the
// doors carry. A stat that fails for any reason but "the path is gone" proves nothing and says nothing.
func (w *exeWatch) staleNote(version string) string {
	if w == nil || !w.known || w.read == nil {
		return ""
	}
	now, err := w.read()
	if err != nil {
		return ""
	}
	if !now.Missing && now.Size == w.started.Size && now.ModTime.Equal(w.started.ModTime) {
		return ""
	}
	note := fmt.Sprintf("this MCP server is running %s from an executable image that has changed on disk since it started "+
		"(it started from %s; now %s). A long-lived server keeps the image it started from, so a fix shipped since is not in "+
		"this process: restart the MCP client to load it. Nothing has been interrupted and this call ran as it always does.",
		version, w.started, nowWords(now))
	w.logged.Do(func() { log.Printf("mcpserver: %s", note) })
	return note
}

func nowWords(now exeStamp) string {
	if now.Missing {
		return "the file is no longer at its path"
	}
	return now.String()
}

// staleBinaryNote is the server's note for a door's answer ("" = none).
func (s *Server) staleBinaryNote(version string) string { return s.exe.staleNote(version) }
