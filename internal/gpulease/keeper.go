package gpulease

// A lease record that EXISTS and cannot be READ (an access error while it is replaced, a sharing violation under a scanner, an I/O
// error) is held for as long as its keeper is alive, and the keeper gives up first.
//
// The acquirers used to sweep such a record, and its claims, once it was older than claimGrace, whatever its holder was doing:
// debris is judged by the record, and the record could not be read. A keeper whose record is unreadable keeps retrying for a while and
// writes no heartbeat, so a long job's record was swept and the cards were handed out with the job still running on them.
//
// So the acquirer side asks the other evidence: is the holder alive? That is the heartbeat file (hb.<epoch>, which the record's
// unreadability does not touch) and the record's own modification time, both within the heartbeat TTL every record is judged by. And the
// keeper side (gpu_cmd.go: the wrapper, its drain heartbeat, its drain loop, the hidden holder) stops, within windows that are each
// shorter than that TTL, once its own read of the record has failed for its window. Corrupt bytes are not an I/O failure: a record that
// parses to nothing is still debris after the grace window, as before.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// keeperSignOfLife is the newest sign that the holder of the lease with this record is alive, taken from what does not need the record
// read: the heartbeat file hb.<epoch> (its content is the moment of the holder's last renewal) and the record file's modification
// time (its last write). epoch 0 means the epoch is not known, as it is not for an unreadable meta.json: every hb.* counts then. The
// zero time is no sign at all.
func (m *Manager) keeperSignOfLife(epoch uint64, record string) time.Time {
	var newest time.Time
	note := func(t time.Time) {
		if t.After(newest) {
			newest = t
		}
	}
	// The record is replaced by a rename over the live name, so a look that met the instant it is absent waits for the rename
	// (statSettled); a look that cannot tell gives no sign, as an absent record does.
	if fi, err := statSettled(record); err == nil {
		note(fi.ModTime())
	}
	noteHeartbeat := func(path string) {
		b, err := readSharedFile(path)
		if err != nil {
			return
		}
		if v, perr := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); perr == nil && v > 0 {
			note(time.UnixMilli(v))
		}
	}
	if epoch != 0 {
		noteHeartbeat(m.heartbeatPath(epoch))
		return newest
	}
	entries, err := os.ReadDir(m.leaseDir())
	if err != nil {
		return newest
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "hb.") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		if _, perr := strconv.ParseUint(strings.TrimPrefix(name, "hb."), 10, 64); perr != nil {
			continue
		}
		noteHeartbeat(filepath.Join(m.leaseDir(), name))
	}
	return newest
}

// keeperAlive reports whether the holder of the lease with this record gave a sign of life within the heartbeat TTL (m.heartbeatTTL,
// the threshold the reader judges every record's heartbeat by). A sign dated in the future (a clock that ran backwards) is alive: the
// safe side.
func (m *Manager) keeperAlive(epoch uint64, record string) bool {
	sign := m.keeperSignOfLife(epoch, record)
	return !sign.IsZero() && m.now().Sub(sign) < m.heartbeatTTL
}

// unreadableHeld is the refusal for a lease whose record cannot be read and whose holder is alive.
func unreadableHeld(epoch uint64) *ErrHeld {
	reason := "the lease record cannot be read, and its holder is alive (a heartbeat or a write within the heartbeat TTL); it is held until the holder lets go or its heartbeat goes stale"
	if epoch != 0 {
		reason = "lease epoch " + strconv.FormatUint(epoch, 10) + ": " + reason
	}
	return &ErrHeld{Info: Info{Held: true, Epoch: epoch, Reason: reason}}
}

// keptUnreadable lists the card leases whose record exists, cannot be read for an I/O reason and whose keeper is alive: held, though
// no reader can see them. The sweep leaves them (and their claims) alone, and a whole-node grant is refused over them.
func (m *Manager) keptUnreadable() []uint64 {
	entries, err := os.ReadDir(m.v2Dir())
	if err != nil {
		return nil
	}
	var kept []uint64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		epoch, perr := strconv.ParseUint(strings.TrimSuffix(e.Name(), ".json"), 10, 64)
		if perr != nil {
			continue
		}
		if _, rerr := readEpochRecord(m.leaseDir(), epoch); rerr != nil && isIOFailure(rerr) && m.keeperAlive(epoch, epochRecordPath(m.leaseDir(), epoch)) {
			kept = append(kept, epoch)
		}
	}
	return kept
}
