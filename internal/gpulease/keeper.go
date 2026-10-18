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
	"errors"
	"io/fs"
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
	sign, _ := m.keeperEvidence(epoch, record)
	return sign
}

// keeperEvidence is keeperSignOfLife and whether a heartbeat could not be told. A heartbeat is replaced by a rename over the live name
// WITHOUT the epoch lock, so it is the one lease file that can be in flight under the lock the sweep and the grant hold, and it is read
// the way the reclaim rule reads it (heartbeatAt): its content when that is a time of renewal (a number not smaller than minHeartbeatMs: a
// prefix of one, torn, is not); else the file's modification time, which ages like any heartbeat (a torn or unreadable one that nobody
// renews goes stale); a file SETTLED absent is no sign (a holder that never renewed);
// and a heartbeat that can be neither read nor examined, or whose rename is installing it, is UNKNOWN. Unknown is no evidence that the
// holder is gone, so keeperAlive reads it as alive. Reading it as "no sign of life" had the sweep remove the record and the claims of
// a live holder whose record could not be read, and the next grant take its cards. That holds for the scan of every heartbeat (epoch 0)
// as well: it works from a listing, a listing does not show a name that a rename is installing, so a staged heartbeat whose live name is
// not listed counts as unknown too.
func (m *Manager) keeperEvidence(epoch uint64, record string) (newest time.Time, unknown bool) {
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
		if err == nil {
			if v, perr := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); perr == nil && v >= minHeartbeatMs {
				note(time.UnixMilli(v))
				return
			}
			// read, but it says nothing (empty, not a number, or too small to be a time of renewal): age the file itself
		}
		fi, serr := statSettled(path)
		switch {
		case serr == nil:
			note(fi.ModTime())
		case err != nil && errors.Is(err, fs.ErrNotExist) && errors.Is(serr, fs.ErrNotExist):
			// settled absent: the holder never renewed, no sign
		default:
			unknown = true
		}
	}
	if epoch != 0 {
		noteHeartbeat(m.heartbeatPath(epoch))
		return newest, unknown
	}
	// The epoch is not known, so the heartbeats are found by LISTING the directory, and a listing is blind to a rename in flight: the
	// live name of a heartbeat that is being installed is not in it, and only its staged file is. A staged heartbeat whose live name is
	// not listed and which is fresh (stagedInFlight) is a heartbeat that cannot be told, as a launch marker's is (tree.go). A listing
	// that fails says nothing about any heartbeat: unknown as well.
	entries, err := os.ReadDir(m.leaseDir())
	if err != nil {
		return newest, true
	}
	listed := make(map[string]bool, len(entries))
	for _, e := range entries {
		listed[e.Name()] = true
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "hb.") {
			continue
		}
		if live := withoutStagedSuffix(name); live != name {
			if isHeartbeatName(live) && !listed[live] && stagedInFlight(filepath.Join(m.leaseDir(), live)) {
				unknown = true
			}
			continue
		}
		if !isHeartbeatName(name) {
			continue
		}
		noteHeartbeat(filepath.Join(m.leaseDir(), name))
	}
	return newest, unknown
}

// isHeartbeatName says whether a directory entry is the live name of some lease's heartbeat: hb.<epoch>.
func isHeartbeatName(name string) bool {
	rest, ok := strings.CutPrefix(name, "hb.")
	if !ok {
		return false
	}
	_, err := strconv.ParseUint(rest, 10, 64)
	return err == nil
}

// keeperAlive reports whether the holder of the lease with this record gave a sign of life within the heartbeat TTL (m.heartbeatTTL,
// the threshold the reader judges every record's heartbeat by), or whether its heartbeat could not be told (keeperEvidence): a holder
// that cannot be shown to be gone is alive. A sign dated in the future (a clock that ran backwards) is alive: the safe side.
func (m *Manager) keeperAlive(epoch uint64, record string) bool {
	sign, unknown := m.keeperEvidence(epoch, record)
	return unknown || (!sign.IsZero() && m.now().Sub(sign) < m.heartbeatTTL)
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
