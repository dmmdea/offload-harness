package gpulease

// THE PRE-CHANGE READER, KEPT AS A FIXTURE (plan invariant I8).
//
// Everything below is the lease reader and writer exactly as it stood at 0.160.0, before
// the v2 record (per-card claims under e/ and cards/) existed: same bodies, only the
// names carry a `legacy` prefix so they can sit beside the new code. It is the stand-in
// for every binary on a host that predates card-scoped leases — a rolled-back image, a
// separate copy of the binary a media repo carries, a retained backup — and for the
// Node reader's record parse. Tests run it against directories the NEW code wrote, so
// the cross-version behaviour is pinned by execution, not by argument.
//
// DO NOT "fix" or modernise this file when the real code changes: its whole value is
// that it does not move. A change to the lease format that this fixture handles
// differently from the shipped code is a cross-version hazard and must be answered in
// the test that exposes it, not by editing the fixture.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func legacyMetaPath(dir string) string { return filepath.Join(dir, metaFileName) }

// legacyReclaimable is Reclaimable at 0.160.0.
func legacyReclaimable(m *Meta, now time.Time, heartbeatTTL time.Duration, procStart func(int) (int64, bool)) bool {
	if m == nil {
		return true
	}
	if m.Holder.PID <= 0 {
		return now.UnixMilli() > m.ExpiresAtMs
	}
	if !pidAlive(m.Holder.PID) {
		return true
	}
	if m.Holder.StartTimeMs != 0 {
		if st, ok := procStart(m.Holder.PID); ok && st != m.Holder.StartTimeMs {
			return true
		}
	}
	nowMs := now.UnixMilli()
	heartbeatStale := m.RenewedAtMs > 0 && nowMs-m.RenewedAtMs > heartbeatTTL.Milliseconds()
	windowExpired := nowMs > m.ExpiresAtMs
	return heartbeatStale && windowExpired
}

func legacyHeartbeatAt(leaseDir string, meta *Meta) int64 {
	b, err := os.ReadFile(filepath.Join(leaseDir, "hb."+strconv.FormatUint(meta.Epoch, 10)))
	if err != nil {
		return meta.RenewedAtMs
	}
	v, perr := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if perr != nil || v < meta.RenewedAtMs {
		return meta.RenewedAtMs
	}
	return v
}

// legacyInspect is inspectDirDetailAt at 0.160.0: what an old `gpu status`, an old
// fleet daemon, an old vision gate and an old MCP server make of a lease directory.
func legacyInspect(leaseDir string, now time.Time, hbTTL time.Duration, procStart func(int) (int64, bool)) (Info, *Meta, bool) {
	b, err := os.ReadFile(legacyMetaPath(leaseDir))
	if err != nil {
		return Info{}, nil, false
	}
	var meta Meta
	if json.Unmarshal(b, &meta) != nil {
		return Info{}, nil, false
	}
	effective := meta
	effective.RenewedAtMs = legacyHeartbeatAt(leaseDir, &meta)
	if legacyReclaimable(&effective, now, hbTTL, procStart) {
		return Info{}, &meta, true
	}
	age := now.Sub(time.UnixMilli(meta.AcquiredAtMs))
	if age < 0 {
		age = 0
	}
	return Info{Held: true, Class: meta.Class, Epoch: meta.Epoch, PID: meta.Holder.PID, Age: age,
		Reason: meta.Reason, ExpiresAt: time.UnixMilli(meta.ExpiresAtMs)}, &meta, false
}

// legacyCheck is Lease.Check at 0.160.0: the epoch compare against meta.json.
func legacyCheck(leaseDir string, epoch uint64) error {
	b, err := os.ReadFile(legacyMetaPath(leaseDir))
	if err != nil {
		return fmt.Errorf("lease is gone (epoch %d)", epoch)
	}
	var meta Meta
	if err := json.Unmarshal(b, &meta); err != nil {
		return err
	}
	if meta.Epoch != epoch {
		return fmt.Errorf("fenced out — our epoch %d, current epoch %d", epoch, meta.Epoch)
	}
	return nil
}

// legacyTryAcquire is TryAcquire at 0.160.0 reduced to its arbitration: probe, bump the
// epoch, exclusive-create meta.json, and reclaim a stale record. The record it writes
// is the 0.160.0 shape. m supplies only the epoch counter, the clock and the pid.
func legacyTryAcquire(t *testing.T, m *Manager, class Class) (uint64, error) {
	t.Helper()
	dir := m.leaseDir()
	for attempt := 0; attempt < 3; attempt++ {
		if info, _, reclaimable := legacyInspect(dir, m.now(), m.heartbeatTTL, m.procStart); info.Held && !reclaimable {
			return 0, errors.New("held")
		}
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return 0, err
		}
		epoch, err := m.bumpEpoch()
		if err != nil {
			return 0, err
		}
		start, _ := m.procStart(m.pid)
		now := m.now()
		rec, _ := json.Marshal(&Meta{Epoch: epoch, Class: class, Holder: Holder{PID: m.pid, StartTimeMs: start},
			Reason: "legacy writer", AcquiredAtMs: now.UnixMilli(), ExpiresAtMs: now.Add(time.Hour).UnixMilli(), RenewedAtMs: now.UnixMilli()})
		f, cerr := createClaim(legacyMetaPath(dir))
		if cerr == nil {
			_, werr := f.Write(rec)
			_ = f.Close()
			if werr != nil {
				_ = removeClaim(legacyMetaPath(dir))
				return 0, werr
			}
			return epoch, nil
		}
		if !os.IsExist(cerr) {
			return 0, cerr
		}
		meta, _ := os.ReadFile(legacyMetaPath(dir))
		var cur Meta
		if json.Unmarshal(meta, &cur) == nil && !legacyReclaimable(&cur, m.now(), m.heartbeatTTL, m.procStart) {
			return 0, errors.New("held")
		}
		_ = removeClaim(legacyMetaPath(dir))
	}
	return 0, errors.New("held")
}
