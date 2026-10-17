package gpulease

// Process-tree containment: a dead wrapper with a live tree keeps its claim (plan P10).
//
// WHY. The reclaim rule used to read "the wrapper's pid is gone" as "the job is gone". It is
// not: a wrapper that is killed, crashes or loses its machine's session leaves the command it
// started running, on a card that is truly in use, and the next acquirer was handed that card
// at once. Two jobs on one card is the defect this closes.
//
// THE RULE. A wrapper records the processes of the tree it started (pid and start identity) in
// the lease record, when the set changes. A lease whose wrapper is gone (its pid dead, or recycled
// for another process) is NOT reclaimable while any recorded member is alive by pid AND start
// identity. The lease is then tree-orphan: the claim is kept, the card reads in use, and the
// waiter is told how to take it over (a later change). Nothing here kills a process.
//
// BOUNDED BY OBSERVATION. There is no timer. Every read of the lease checks the members again,
// so a pid recycled for an unrelated process cannot hold a card (its start identity differs)
// and a card frees itself at the first read after its members are gone. The declared window and
// the heartbeat play no part: a lease is never freed by the clock (invariant I3).
//
// WHERE THE MEMBERS COME FROM, in order, only while the wrapper is gone and no recorded member
// is alive:
//
//  1. the record itself (above);
//  2. a ComfyUI launch marker, which names the lease epoch an instance was launched under: an
//     instance can sit outside the wrapper's job (a launcher that broke away), and the marker
//     is the only thing that ties it to the lease. A marker with no lease epoch (the default
//     instance, or one kept outside any lease) ties only to a lease that recorded no tree, when
//     it began at or after that lease and no other lease record exists in the directory;
//  3. for a lease that recorded no tree (a wrapper that predates this change), the children of
//     the dead wrapper's pid in the process table. Windows keeps a child's parent link when the
//     parent dies, so the children are found; Linux reparents them and the walk finds none. A
//     wrapper pid that is alive but recycled is never walked: its children are another
//     process's.
//
// WHAT IT DOES NOT USE. The driver's per-process compute listing: under WDDM it names no
// process for most of them, and a process on a card is not attributable to a lease by itself.
// Such a process is REPORTED by the callers that stop things (proctree.PlanStop), never counted
// as a member here.
//
// CROSS-VERSION (I7). A reader that predates this change ignores the recorded tree and reclaims
// a dead wrapper's lease at once, as it always did (pinned by TestOldReaderStillReclaimsA-
// TreeOrphanRecord). The rule holds on a host where every reader is this build.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TreeMember is one process of the job a wrapper started: its pid and its start identity (the
// platform's own, opaque, compared only for equality on the same box: ProcessStart). A member
// recorded without a start identity is held by its pid alone.
type TreeMember struct {
	PID     int   `json:"pid"`
	StartMs int64 `json:"start_ms,omitempty"`
}

var (
	// startUnixMsFn converts a process start identity to Unix milliseconds (a seam: the platform's
	// own conversion in production).
	startUnixMsFn = startUnixMs
	// processTreeFn walks a pid's descendants in the process table (a seam).
	processTreeFn = ProcessTree

	comfyDirMu sync.RWMutex
	comfyDir   string

	// treeMarkerSlack is how much later than a launch marker its process may have started
	// (the launcher writes the marker right after spawning it); a process that began after the
	// marker plus this cannot be the instance the marker describes (a recycled pid).
	treeMarkerSlack = 10 * time.Second
)

// SetComfyDir arms the ComfyUI install directory the launch markers are read from (config
// comfy_dir), once per process like the lease directory. Empty disables the marker evidence.
func SetComfyDir(dir string) {
	comfyDirMu.Lock()
	comfyDir = strings.TrimSpace(dir)
	comfyDirMu.Unlock()
}

// ComfyDir is the directory SetComfyDir armed ("" = none).
func ComfyDir() string { return armedComfyDir() }

func armedComfyDir() string {
	comfyDirMu.RLock()
	defer comfyDirMu.RUnlock()
	return comfyDir
}

// memberAlive reports whether a recorded member is still the process that was recorded: its pid
// is alive and, when both identities are known, its start is the recorded one. A start that
// cannot be read says nothing about a recycled pid, so the member stays held (a safety check
// does not turn itself off), the same stance Reclaimable takes for the wrapper's own start.
func memberAlive(mem TreeMember, procStart func(int) (int64, bool)) bool {
	if mem.PID <= 0 || !pidAlive(mem.PID) {
		return false
	}
	if mem.StartMs != 0 {
		if st, ok := procStart(mem.PID); ok && st != mem.StartMs {
			return false // the pid was recycled
		}
	}
	return true
}

// liveMembers is the subset of members that are still alive.
func liveMembers(members []TreeMember, procStart func(int) (int64, bool)) []TreeMember {
	var out []TreeMember
	for _, mem := range members {
		if memberAlive(mem, procStart) {
			out = append(out, mem)
		}
	}
	return out
}

// wrapperGone reports whether the process that holds the lease is provably gone: its pid is
// dead, or alive but a different process (recycled). A record that names no holder pid (a
// synthetic record) has no wrapper to be gone.
func wrapperGone(m *Meta, procStart func(int) (int64, bool)) bool {
	if m.Holder.PID <= 0 {
		return false
	}
	if !pidAlive(m.Holder.PID) {
		return true
	}
	if m.Holder.StartTimeMs != 0 {
		if st, ok := procStart(m.Holder.PID); ok && st != m.Holder.StartTimeMs {
			return true
		}
	}
	return false
}

// normalizeTree makes a member list canonical: one entry per pid (the last one wins), sorted.
func normalizeTree(in []TreeMember) []TreeMember {
	byPID := map[int]TreeMember{}
	for _, m := range in {
		if m.PID > 0 {
			byPID[m.PID] = m
		}
	}
	out := make([]TreeMember, 0, len(byPID))
	for _, m := range byPID {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

func sameTree(a, b []TreeMember) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// treeState is what a Lease remembers of the tree it last recorded, so the wrapper's tick takes
// the epoch lock only when the set changed.
type treeState struct {
	mu       sync.Mutex
	recorded bool
	last     []TreeMember
}

// RecordTree stores the members of the tree the holder started in the lease record, so a reader
// that finds the holder gone can tell whether the job it started is still alive. It writes only
// when the set differs from the one it last recorded (the record rewrite takes the epoch lock),
// and refuses when the lease is no longer this holder's. The holder's own pid is not a member:
// the record already carries it.
func (l *Lease) RecordTree(members []TreeMember) error {
	norm := normalizeTree(members)
	l.tree.mu.Lock()
	defer l.tree.mu.Unlock()
	if l.tree.recorded && sameTree(l.tree.last, norm) {
		return nil
	}
	if err := l.Restamp(func(m *Meta) { m.Tree = norm }); err != nil {
		return err
	}
	l.tree.recorded, l.tree.last = true, norm
	return nil
}

// treeReading is what a reader found out about a lease whose wrapper is gone.
type treeReading struct {
	// Orphan: the wrapper is gone and at least one member is alive.
	Orphan bool
	Live   []TreeMember
	Why    string
}

// judge is the one place a reader decides what a record's tree is. It returns the record as the
// reclaim rule must see it: the heartbeat read from its per-epoch file and, when the wrapper is
// gone and no RECORDED member is alive, the members the other evidence found appended to Tree.
// A record whose wrapper is alive is returned with the heartbeat only: nothing is read from the
// process table for a healthy lease.
func (r reader) judge(meta *Meta) (Meta, treeReading) {
	eff := *meta
	// An UNKNOWN heartbeat (it could not be read or examined, or a rename is installing it) is no stamp at all: RenewedAtMs 0 is
	// what Reclaimable reads as "nothing to call stale". The record on disk is not touched, this is the reader's copy.
	if at, known := heartbeatAt(r.dir, meta); known {
		eff.RenewedAtMs = at
	} else {
		eff.RenewedAtMs = 0
	}
	if !wrapperGone(&eff, r.procStart) {
		return eff, treeReading{}
	}
	if live := liveMembers(eff.Tree, r.procStart); len(live) > 0 {
		return eff, treeReading{Orphan: true, Live: live, Why: "a process the wrapper recorded is still running (" + pidList(live) + ")"}
	}
	live, why := r.extraEvidence(&eff)
	if len(live) == 0 {
		return eff, treeReading{}
	}
	eff.Tree = append(append([]TreeMember(nil), eff.Tree...), live...)
	return eff, treeReading{Orphan: true, Live: live, Why: why}
}

func pidList(ms []TreeMember) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = "pid " + strconv.Itoa(m.PID)
	}
	return strings.Join(parts, ", ")
}

// extraEvidence looks beyond the record for members of a dead wrapper's tree: the launch markers
// tied to the lease, then (for a lease that recorded no tree) the dead wrapper's children in the
// process table. Both are read only on this path.
func (r reader) extraEvidence(eff *Meta) ([]TreeMember, string) {
	var live []TreeMember
	var why []string
	seen := map[int]bool{}
	add := func(mem TreeMember, reason string) {
		if seen[mem.PID] {
			return
		}
		seen[mem.PID] = true
		live = append(live, mem)
		why = append(why, reason)
	}
	legacyShape := len(eff.Tree) == 0
	var walked []TreeMember
	if legacyShape && eff.Holder.PID > 0 && !pidAlive(eff.Holder.PID) {
		walked = r.walkDeadWrapper(eff)
		for _, mem := range walked {
			add(mem, "a child of the dead wrapper found in the process table is still running ("+pidList([]TreeMember{mem})+")")
		}
	}
	for _, mk := range readLaunchMarkers(armedComfyDir()) {
		mem, ok := r.markerMember(eff, mk, legacyShape, walked)
		if !ok {
			continue
		}
		reason := "its ComfyUI launch marker (" + pidList([]TreeMember{mem}) + ") is tied to this lease and the process is alive"
		add(mem, reason)
	}
	return live, strings.Join(why, "; ")
}

// walkDeadWrapper lists the children of the dead wrapper's pid that began at or after the lease.
// The wrapper started them after it took the lease, so an older process naming that pid as its
// parent is another process's.
func (r reader) walkDeadWrapper(eff *Meta) []TreeMember {
	procs, err := processTreeFn(eff.Holder.PID)
	if err != nil {
		return nil
	}
	var out []TreeMember
	for _, p := range procs {
		if p.PID == eff.Holder.PID || p.PID <= 0 {
			continue
		}
		if abs, ok := startUnixMsFn(p.StartMs); ok && p.StartMs != 0 && abs < eff.AcquiredAtMs-recycledSlack.Milliseconds() {
			continue
		}
		mem := TreeMember{PID: p.PID, StartMs: p.StartMs}
		if memberAlive(mem, r.procStart) {
			out = append(out, mem)
		}
	}
	return out
}

// launchMarker is one ComfyUI launch marker (render/comfy-ownership.mjs): the instance's python
// pid, the process that launched it, when, and, for a keyed instance launched under a lease, the
// lease epoch.
type launchMarker struct {
	PID        int     `json:"pid"`
	OwnerPID   int     `json:"ownerPid"`
	StartedAt  int64   `json:"startedAt"`
	Key        string  `json:"key"`
	LeaseEpoch *uint64 `json:"leaseEpoch"`
}

// readLaunchMarkers reads every launch marker in the ComfyUI directory: the default instance's
// and each keyed instance's. An unreadable one is skipped.
func readLaunchMarkers(dir string) []launchMarker {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []launchMarker
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, ".offload-launch") || !strings.HasSuffix(name, ".json") {
			continue
		}
		// Node restamps a marker through a staged .tmp file and a rename over the live name; the shared open does not hold that rename
		// up, and an absence is only an answer once it is settled (gate.go).
		b, rerr := readSharedFile(filepath.Join(dir, name))
		if rerr != nil {
			continue
		}
		var mk launchMarker
		if json.Unmarshal(b, &mk) != nil || mk.PID <= 0 {
			continue
		}
		out = append(out, mk)
	}
	return out
}

// markerMember decides whether a launch marker ties a live process to the lease. The marker's
// process must be alive and must not have begun after the marker was written (a recycled pid).
// A marker that names a lease epoch is this lease's only if it is this epoch. A marker that names
// none ties by ancestry (its pid or the process that launched it is in the walked tree), or, for
// a lease that recorded no tree, by having begun at or after the lease while no other lease
// record exists to claim it.
func (r reader) markerMember(eff *Meta, mk launchMarker, legacyShape bool, walked []TreeMember) (TreeMember, bool) {
	if !pidAlive(mk.PID) {
		return TreeMember{}, false
	}
	st, known := r.procStart(mk.PID)
	if known {
		if abs, ok := startUnixMsFn(st); ok && mk.StartedAt > 0 && abs > mk.StartedAt+treeMarkerSlack.Milliseconds() {
			return TreeMember{}, false
		}
	}
	mem := TreeMember{PID: mk.PID}
	if known {
		mem.StartMs = st
	}
	if mk.LeaseEpoch != nil {
		return mem, *mk.LeaseEpoch == eff.Epoch
	}
	for _, w := range walked {
		if w.PID == mk.PID || (mk.OwnerPID > 0 && w.PID == mk.OwnerPID) {
			return mem, true
		}
	}
	if !legacyShape || mk.StartedAt <= 0 || mk.StartedAt < eff.AcquiredAtMs {
		return TreeMember{}, false
	}
	if r.otherLeaseRecords(eff.Epoch) {
		return TreeMember{}, false
	}
	return mem, true
}

// otherLeaseRecords reports whether the lease directory holds a lease record besides epoch's: a
// whole-node record or a device lease. A marker with no lease epoch cannot be told apart from
// another lease's when one exists, so it ties to nothing then.
func (r reader) otherLeaseRecords(epoch uint64) bool {
	if b, err := os.ReadFile(filepath.Join(r.dir, metaFileName)); err == nil {
		var m Meta
		if json.Unmarshal(b, &m) != nil || m.Epoch != epoch {
			return true
		}
	}
	entries, err := os.ReadDir(filepath.Join(r.dir, v2DirName))
	if err != nil {
		return false
	}
	for _, e := range entries {
		n := strings.TrimSuffix(e.Name(), ".json")
		if e.IsDir() || n == e.Name() {
			continue
		}
		if v, perr := strconv.ParseUint(n, 10, 64); perr == nil && v != epoch {
			return true
		}
	}
	return false
}

// applyTree stamps what the reader found out about the lease's tree onto its Info.
func (i *Info) applyTree(t treeReading) {
	if !t.Orphan {
		return
	}
	i.TreeOrphan = true
	i.TreeLive = append([]TreeMember(nil), t.Live...)
	i.TreeWhy = t.Why
}
