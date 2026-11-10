package mcpserver

// stale_config.go: the stale-config notice, the sibling of the stale-binary notice (stalebinary.go).
//
// An MCP server loads its config once, at start, and keeps those values for as long as its client keeps it (weeks): an operator
// who edits the config file (the ComfyUI instance, extra launch arguments, an upscale model, a lane binding) while sessions are
// open changed nothing those sessions use, and nothing told them. The one value the server does re-read is the delegate_remotes
// roster (internal/mediaremote reads it again when the file's mtime moves). cfgWatch remembers the stamp and the sha256 of the file
// the server started from, and says so when the file on disk is not that file any more; stale_notice.go carries the sentence as
// a text block on every tool result, and offload_status carries it under stale_config.

import (
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"sync"
)

// cfgWatch is the config file this server started from: its path, its stamp and the hash of its bytes at that moment. A nil
// *cfgWatch watches nothing (a server built without a config source: built-in defaults, tests), and every method is safe on it.
type cfgWatch struct {
	path    string
	started exeStamp
	// sum is the sha256 of the file at start; hashed says it was read. A start whose bytes could not be read, or that moved
	// while they were read, leaves the moved stamp to decide alone.
	sum    [sha256.Size]byte
	hashed bool
	logged sync.Once

	// mu guards the one-entry cache of the last moved stamp that was hashed, so a changed file is read once, not per call.
	mu        sync.Mutex
	checked   exeStamp
	checkedOK bool
	changed   bool
}

// newCfgWatch reads the stamp and the hash of the file at path. An empty path, a file that is not there and a stamp that cannot
// be read give nil: there is nothing to compare with, so the watch says nothing rather than accuse a file it never saw.
func newCfgWatch(path string) *cfgWatch {
	if path == "" {
		return nil
	}
	before, err := stampOf(path)
	if err != nil || before.Missing {
		return nil
	}
	w := &cfgWatch{path: path, started: before}
	b, err := os.ReadFile(path)
	if err != nil {
		return w
	}
	// A file rewritten while it was read has a hash that may belong to either version: keep the stamp from before the read
	// and no hash, so a later look finds the stamp moved and decides on that alone.
	if after, err := stampOf(path); err == nil && sameStamp(before, after) {
		w.sum, w.hashed = sha256.Sum256(b), true
	}
	return w
}

func sameStamp(a, b exeStamp) bool {
	return a.Missing == b.Missing && a.Size == b.Size && a.ModTime.Equal(b.ModTime)
}

// staleConfigNote is "" while the file on disk is the one this server started from, and otherwise the sentence the status
// door and the second text block carry. A stamp that has not moved is the cheap answer (one stat). A moved stamp is not yet a
// different file (a touch, or a save that wrote the same bytes), so the bytes are compared when the start hash is known; a file
// that cannot be read right now proves nothing and accuses nothing.
func (w *cfgWatch) staleConfigNote() string {
	if w == nil {
		return ""
	}
	now, err := stampOf(w.path)
	if err != nil {
		return ""
	}
	if sameStamp(now, w.started) {
		return ""
	}
	if !now.Missing && w.hashed && !w.bytesDiffer(now) {
		return ""
	}
	var what string
	if now.Missing {
		what = "the file is no longer at its path"
	} else {
		what = "now " + now.String()
	}
	note := fmt.Sprintf("this MCP server read its config from %s when it started (%s) and the file has changed since (%s). "+
		"A running server never reloads its config, except the delegate_remotes roster, so the new values are not in this process: "+
		"reconnect this server with /mcp (select local-offload, Reconnect) or restart the MCP client to apply them. "+
		"Nothing has been interrupted and this call ran as it always does.", w.path, w.started, what)
	w.logged.Do(func() { log.Printf("mcpserver: %s", note) })
	return note
}

// bytesDiffer reports whether the file whose stamp is now holds other bytes than the file at start. It reads the file once per
// stamp (the result is cached by stamp); a read that fails answers true, because a moved stamp with bytes that cannot be compared
// stands, as the stale-binary note does.
func (w *cfgWatch) bytesDiffer(now exeStamp) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.checkedOK && sameStamp(w.checked, now) {
		return w.changed
	}
	b, err := os.ReadFile(w.path)
	if err != nil {
		return true
	}
	w.checked, w.checkedOK, w.changed = now, true, sha256.Sum256(b) != w.sum
	return w.changed
}
