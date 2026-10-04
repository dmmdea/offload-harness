package gpulease

// Card-scoped leases: lease record v2 (plan P2, register C-86).
//
// WHY. The lease was whole-node: a card-2 render fenced cards 0 and 1 too, so a box with
// three free cards ran one job at a time and queued everything else behind it. A lease
// now MAY name the cards it holds. Two leases conflict when either is whole-node or
// their card sets intersect; everything else runs side by side.
//
// ON DISK (all under the lease directory, beside the legacy meta.json):
//
//	e/<epoch>.json        the lease record for a device lease (authoritative for v2)
//	cards/<uuid>.claim    one per held card; names the epoch that holds it
//	hb.<epoch>            the per-epoch heartbeat (unchanged from whole-node leases)
//	../FORMAT             "2" once a device lease has ever been written here
//
// A WHOLE-NODE lease is unchanged: meta.json is its real record and nothing under e/ or
// cards/ is touched. A DEVICE lease never writes meta.json. DEVIATION FROM PLAN REV 2,
// PENDING ACCEPTANCE: the plan specified a synthetic pid-0 "umbrella" meta.json over a
// set of device leases so that a pre-v2 writer queues behind it. Its premise was checked
// against the real 0.158.3 and 0.160.0 binaries and holds (a pid-0 record is held for
// its declared window and reclaimed after it); it was dropped as a second source of
// truth to restamp on every grant and release, not because it failed (see the P2 notes
// in docs/systems/gpu-lease.md). The protection is instead the writer gate: a device
// lease can only be written on a host with a green reader audit
// (Manager.ApplyCardScopedConfig). What follows from that, and is pinned by tests:
//
//   - Fence is PER EPOCH. A device lease is current while its e/<epoch>.json exists
//     and every card it names carries a claim naming its epoch. Nothing compares two
//     leases' epochs, so a higher-epoch lease is never fenced out by a lower one.
//   - Arbitration between a whole-node lease and a device lease runs under the epoch
//     lock on BOTH sides: a device grant refuses while a live meta.json exists, and a
//     whole-node grant that has just created meta.json verifies, under the lock, that
//     no device lease is live, and backs off if one is.
//   - Expiry never frees a card (invariant I3): readers judge a lease with the same
//     Reclaimable rule as a whole-node lease, and only an acquirer that wants a card
//     (a whole-node acquirer too: verifyNoDeviceLeases sweeps first) removes a dead or
//     stalled lease's files, one lease at a time, under the epoch lock, which is what
//     fences a holder that resumes.
//   - A crash mid-grant leaves a `granting` record and/or claims. Past claimGrace the
//     next acquirer removes them (the debris rule); before it they count as a grant in
//     flight.
//
// A reader that predates the format sees a directory holding only device leases as a
// FREE card. That is the one cross-version hazard, and the gate for it is the writer
// switch (config key AND the green reader-audit marker, Manager.ApplyCardScopedConfig),
// not a compatibility shim.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	v2DirName      = "e"
	cardsDirName   = "cards"
	formatFileName = "FORMAT"
	formatVersion  = "2"

	stateGranting = "granting"
	stateActive   = "active"
)

// maxDeviceIDLen bounds one device id; ids name a file under cards/.
const maxDeviceIDLen = 64

var deviceIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// NormalizeDevices canonicalises a device list: trimmed, lower-cased (the claim file
// name must be one per card even on a case-insensitive filesystem), de-duplicated and
// sorted. An empty list is the whole node. A blank or non-token entry is an ERROR,
// never a silent widening to the whole node and never a path component.
func NormalizeDevices(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range in {
		s := strings.ToLower(strings.TrimSpace(d))
		if s == "" {
			return nil, errors.New("gpulease: a device id is blank (an empty list means the whole node; a blank entry is a mistake)")
		}
		if len(s) > maxDeviceIDLen || !deviceIDPattern.MatchString(s) {
			return nil, fmt.Errorf("gpulease: device id %q is not a plain GPU id (letters, digits, '.', '_', '-')", d)
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out, nil
}

// devicesConflict is the conflict rule: either side whole-node (empty), or an
// intersection.
func devicesConflict(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Layout helpers
// ---------------------------------------------------------------------------

func epochRecordPath(leaseDir string, epoch uint64) string {
	return filepath.Join(leaseDir, v2DirName, strconv.FormatUint(epoch, 10)+".json")
}

func cardClaimPath(leaseDir, device string) string {
	return filepath.Join(leaseDir, cardsDirName, device+".claim")
}

// cardClaim is the content of cards/<uuid>.claim. AtMs lets the debris rule age a claim
// under any clock, including a test's.
type cardClaim struct {
	Epoch uint64 `json:"epoch"`
	AtMs  int64  `json:"at_ms,omitempty"`
}

// readCardClaim parses a claim. ok is false for a missing or unparseable file. A bare
// decimal is accepted too: the claim only has to NAME an epoch.
func readCardClaim(leaseDir, device string) (cardClaim, bool) {
	b, err := os.ReadFile(cardClaimPath(leaseDir, device))
	if err != nil {
		return cardClaim{}, false
	}
	return parseCardClaim(b)
}

func parseCardClaim(b []byte) (cardClaim, bool) {
	var c cardClaim
	if json.Unmarshal(b, &c) == nil && c.Epoch != 0 {
		return c, true
	}
	if v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil && v != 0 {
		return cardClaim{Epoch: v}, true
	}
	return cardClaim{}, false
}

func readEpochRecord(leaseDir string, epoch uint64) (*Meta, error) {
	b, err := os.ReadFile(epochRecordPath(leaseDir, epoch))
	if err != nil {
		return nil, err
	}
	var rec Meta
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	if rec.Epoch != epoch {
		return nil, fmt.Errorf("gpulease: record e/%d.json names epoch %d", epoch, rec.Epoch)
	}
	return &rec, nil
}

// writeEpochRecord publishes a record atomically (beside, then rename over): a reader
// never meets a torn record. Callers hold the epoch lock.
func writeEpochRecord(leaseDir string, rec *Meta) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("gpulease: encoding lease record: %w", err)
	}
	path := epochRecordPath(leaseDir, rec.Epoch)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o666); err != nil {
		return fmt.Errorf("gpulease: writing lease record: %w", err)
	}
	if err := renameReplacing(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("gpulease: publishing lease record: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The reader: one judgement of a lease directory, shared by every entry point
// ---------------------------------------------------------------------------

// reader bundles what judging a lease directory needs. The Manager builds one from its
// own seams (so a test's clock and process table apply); the package-level inspectors
// build one from the real clock and the real process table.
type reader struct {
	dir       string
	now       time.Time
	hbTTL     time.Duration
	procStart func(int) (int64, bool)
}

func (m *Manager) reader() reader {
	return reader{dir: m.leaseDir(), now: m.now(), hbTTL: m.heartbeatTTL, procStart: m.procStart}
}

func realReader(dir string) reader {
	return reader{dir: dir, now: time.Now(), hbTTL: DefaultHeartbeatTTL, procStart: processStart}
}

// liveLease is one lease a reader judged live, with the record behind it.
type liveLease struct {
	Meta Meta
	Info Info
	V2   bool
}

// legacy reads meta.json: the record (nil when absent or unparseable) and whether it is
// a live lease (not reclaimable by the shared rule).
func (r reader) legacy() (*Meta, bool) {
	b, err := os.ReadFile(filepath.Join(r.dir, metaFileName))
	if err != nil {
		return nil, false
	}
	var meta Meta
	if json.Unmarshal(b, &meta) != nil {
		return nil, false
	}
	effective := meta
	effective.RenewedAtMs = heartbeatAt(r.dir, &meta)
	return &meta, !Reclaimable(&effective, r.now, r.hbTTL, r.procStart)
}

// v2Judged is one e/<epoch>.json with the reader's verdict.
type v2Judged struct {
	Meta Meta
	// Live: an active lease whose holder is not reclaimable, or a grant still inside
	// its grace window (a grant in flight holds its cards).
	Live bool
	// Debris: a grant older than the grace window, or an active lease that is
	// reclaimable. A reader treats it as absent; only an acquirer removes it.
	Debris bool
}

func (r reader) grantInFlight(m *Meta) bool {
	age := r.now.Sub(time.UnixMilli(m.AcquiredAtMs))
	return age < claimGrace // a clock that ran backwards reads as fresh: the safe side
}

func (r reader) v2() []v2Judged {
	entries, err := os.ReadDir(filepath.Join(r.dir, v2DirName))
	if err != nil {
		return nil
	}
	var out []v2Judged
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		epoch, perr := strconv.ParseUint(strings.TrimSuffix(e.Name(), ".json"), 10, 64)
		if perr != nil {
			continue
		}
		rec, rerr := readEpochRecord(r.dir, epoch)
		if rerr != nil {
			continue // unreadable: the sweep deals with it once it is old
		}
		j := v2Judged{Meta: *rec}
		if rec.State == stateGranting {
			j.Live = r.grantInFlight(rec)
			j.Debris = !j.Live
		} else {
			effective := *rec
			effective.RenewedAtMs = heartbeatAt(r.dir, rec)
			j.Debris = Reclaimable(&effective, r.now, r.hbTTL, r.procStart)
			j.Live = !j.Debris
		}
		out = append(out, j)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Epoch < out[j].Meta.Epoch })
	return out
}

// live lists every live lease, lowest epoch first, each carrying the full epoch list so
// HoldsEpoch answers for any of them.
func (r reader) live() []liveLease {
	var out []liveLease
	if meta, ok := r.legacy(); ok {
		info := infoFrom(meta, r.now)
		info.HeartbeatAt = time.UnixMilli(heartbeatAt(r.dir, meta))
		out = append(out, liveLease{Meta: *meta, Info: info})
	}
	for _, j := range r.v2() {
		if !j.Live {
			continue
		}
		rec := j.Meta
		info := infoFrom(&rec, r.now)
		info.HeartbeatAt = time.UnixMilli(heartbeatAt(r.dir, &rec))
		out = append(out, liveLease{Meta: rec, Info: info, V2: true})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Meta.Epoch < out[j].Meta.Epoch })
	epochs := make([]uint64, len(out))
	for i, l := range out {
		epochs[i] = l.Meta.Epoch
	}
	for i := range out {
		out[i].Info.Epochs = append([]uint64(nil), epochs...)
	}
	return out
}

func (r reader) infos() []Info {
	ll := r.live()
	if len(ll) == 0 {
		return nil
	}
	out := make([]Info, len(ll))
	for i, l := range ll {
		out[i] = l.Info
	}
	return out
}

// primary is the single Info the whole-node-era API reports: the lowest live epoch. When
// more than one lease is live it also carries each of them (Info.Leases), so a consumer
// can judge every lease instead of only the lowest.
func (r reader) primary() Info {
	if ll := r.live(); len(ll) > 0 {
		out := ll[0].Info
		out.Leases = perLease(ll)
		return out
	}
	return Info{}
}

// perLease is Info.Leases: one Info per live lease, each naming only its own epoch. Nil
// for a single lease, so the whole-node-era shape of Info is unchanged.
func perLease(ll []liveLease) []Info {
	if len(ll) < 2 {
		return nil
	}
	out := make([]Info, len(ll))
	for i, l := range ll {
		in := l.Info
		in.Epochs = []uint64{l.Meta.Epoch}
		in.Leases = nil
		out[i] = in
	}
	return out
}

// conflicting is the first live lease (lowest epoch) that conflicts with devices.
func (r reader) conflicting(devices []string) (Info, bool) {
	for _, l := range r.live() {
		if devicesConflict(devices, l.Meta.Devices) {
			return l.Info, true
		}
	}
	return Info{}, false
}

// detail is InspectDirDetail: the live primary with its record, or — when nothing is
// live — whatever lease record is left over, so a caller can say "a record is left from
// a holder that is gone" instead of "free".
func (r reader) detail() (Info, *Meta, bool) {
	if ll := r.live(); len(ll) > 0 {
		meta := ll[0].Meta
		info := ll[0].Info
		info.Leases = perLease(ll)
		return info, &meta, false
	}
	if meta, _ := r.legacy(); meta != nil {
		return Info{}, meta, true
	}
	for _, j := range r.v2() {
		if j.Debris && j.Meta.State != stateGranting {
			meta := j.Meta
			return Info{}, &meta, true
		}
	}
	return Info{}, nil, false
}

// epochIsCurrent is the per-epoch fence for a holder that has only an epoch number.
func (r reader) epochIsCurrent(epoch uint64) bool {
	if epoch == 0 {
		return false
	}
	if rec, err := readEpochRecord(r.dir, epoch); err == nil {
		if rec.State == stateGranting {
			return false
		}
		effective := *rec
		effective.RenewedAtMs = heartbeatAt(r.dir, rec)
		return !Reclaimable(&effective, r.now, r.hbTTL, r.procStart) && claimsNameEpoch(r.dir, rec)
	}
	meta, live := r.legacy()
	return live && meta.Epoch == epoch
}

// claimsNameEpoch: every card the record names carries a claim naming the record's epoch.
func claimsNameEpoch(leaseDir string, rec *Meta) bool {
	for _, d := range rec.Devices {
		c, ok := readCardClaim(leaseDir, d)
		if !ok || c.Epoch != rec.Epoch {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Public readers
// ---------------------------------------------------------------------------

// InspectFor is Inspect narrowed to a request: the live lease that conflicts with a
// caller wanting devices (empty = the whole node, which any live lease conflicts with).
func (m *Manager) InspectFor(devices []string) Info {
	devs, err := NormalizeDevices(devices)
	if err != nil {
		devs = devices
	}
	info, _ := m.reader().conflicting(devs)
	return info
}

// Leases lists every live lease under this Manager's clock and process table.
func (m *Manager) Leases() []Info { return m.reader().infos() }

// InspectLeases lists every live lease in a lease directory, lowest epoch first. A
// whole-node lease is one entry with no Devices.
func InspectLeases(leaseDir string) []Info { return realReader(leaseDir).infos() }

// InspectDetail is InspectDirDetail under this Manager's clock and process table.
func (m *Manager) InspectDetail() (Info, *Meta, bool) { return m.reader().detail() }

// EpochIsCurrent is the fence for a process that holds no Lease object (an inherited
// lease, a render child): the lease it was handed is live and still its own. For a
// device lease that is per epoch, never a compare against another lease's epoch.
func EpochIsCurrent(leaseDir string, epoch uint64) bool {
	return realReader(leaseDir).epochIsCurrent(epoch)
}

// EpochIsCurrent is the package-level EpochIsCurrent under this Manager's seams.
func (m *Manager) EpochIsCurrent(epoch uint64) bool { return m.reader().epochIsCurrent(epoch) }

// ---------------------------------------------------------------------------
// Writing: the grant
// ---------------------------------------------------------------------------

func (m *Manager) v2Dir() string    { return filepath.Join(m.leaseDir(), v2DirName) }
func (m *Manager) cardsDir() string { return filepath.Join(m.leaseDir(), cardsDirName) }
func (m *Manager) formatPath() string {
	return filepath.Join(m.gpuDir(), formatFileName)
}

// hasV2Dir is the cheap pre-check a whole-node grant uses to skip the verification: no
// e/ directory has ever been created here. A device grant creates e/ BEFORE it looks at
// meta.json, so a whole-node claim that lands after that look is guaranteed to see the
// directory (the argument is in tryAcquireDevices).
//
// It answers false ONLY when the stat proves e/ is absent. Any other failure (access
// denied, a sharing violation, an AV scan holding the directory) says nothing about
// whether a device lease exists, so it answers true and the caller verifies under the
// lock: a safety check must not turn itself off on an I/O hiccup.
func (m *Manager) hasV2Dir() bool {
	fi, err := m.stat(m.v2Dir())
	if err != nil {
		return !os.IsNotExist(err)
	}
	return fi.IsDir()
}

// stat and remove are os.Stat and removeClaim unless a test installed a seam.
func (m *Manager) stat(path string) (os.FileInfo, error) {
	if m.statHook != nil {
		return m.statHook(path)
	}
	return os.Stat(path)
}

func (m *Manager) remove(path string) error {
	if m.removeHook != nil {
		return m.removeHook(path)
	}
	return removeClaim(path)
}

// bumpEpochLocked issues the next fencing token. The caller holds the epoch lock.
func (m *Manager) bumpEpochLocked() (uint64, error) {
	cur, err := m.readEpoch()
	if err != nil {
		return 0, err
	}
	next := cur + 1
	return next, m.writeEpoch(next)
}

// tryAcquireDevices is the device-scoped grant: ONE critical section under the epoch
// lock, so nothing interleaves with it but a reader.
//
// ORDER, and why. (1) e/ and cards/ are created (non-exclusively) BEFORE anything is
// judged: a whole-node acquirer's cheap "has a device lease ever existed here" check can
// then never miss this grant. (2) Debris is swept. (3) A live meta.json refuses the
// grant. (4) Live device leases that intersect refuse it. (5) The epoch is issued and
// the record written as `granting`. (6) Each card claim is created exclusively; a
// failure rolls back everything made so far. (7) The record flips to `active`.
//
// A crash anywhere between (5) and (7) leaves debris the next acquirer removes after
// claimGrace; nothing here depends on a cleanup that might not run.
func (m *Manager) tryAcquireDevices(class Class, opts Options, devs []string) (*Lease, error) {
	// Cheap read-only probe first, so a one-second poller does not spin the epoch lock.
	if info := m.InspectFor(devs); info.Held {
		return nil, m.heldErr(info)
	}
	for _, d := range []string{m.v2Dir(), m.cardsDir()} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			return nil, fmt.Errorf("gpulease: could not create %s: %w", d, err)
		}
	}
	var lease *Lease
	var held *ErrHeld
	err := m.withEpochLock(func() error {
		var gerr error
		lease, held, gerr = m.grantDevicesLocked(class, opts, devs)
		return gerr
	})
	if err != nil {
		return nil, err
	}
	if held != nil {
		return nil, held
	}
	return lease, nil
}

// grantDevicesLocked is steps (2) to (7) of tryAcquireDevices. The caller holds the
// epoch lock. It returns the lease, or a non-nil *ErrHeld when the cards are not free,
// or an error; a refusal leaves nothing behind.
func (m *Manager) grantDevicesLocked(class Class, opts Options, devs []string) (*Lease, *ErrHeld, error) {
	m.sweepDebrisLocked()
	r := m.reader()

	// (3) a live whole-node record fences every device request.
	meta, live := r.legacy()
	switch {
	case meta != nil && live:
		return nil, m.heldErr(infoFrom(meta, m.now())), nil
	case meta != nil: // a reclaimable whole-node claim: remove only that record
		if err := removeClaim(m.metaPath()); err != nil {
			return nil, nil, fmt.Errorf("gpulease: cannot reclaim the stale lease at %s: %w", m.metaPath(), err)
		}
	default:
		if _, serr := os.Stat(m.metaPath()); serr == nil {
			// Present but unparseable: a claim caught mid-write, or debris.
			if m.claimIsFresh() {
				return nil, &ErrHeld{Info: Info{Held: true, Reason: "claim in progress"}}, nil
			}
			if err := removeClaim(m.metaPath()); err != nil {
				return nil, nil, fmt.Errorf("gpulease: cannot remove the unreadable claim at %s: %w", m.metaPath(), err)
			}
		}
	}

	// (4) live device leases that intersect.
	if info, ok := r.conflicting(devs); ok {
		return nil, m.heldErr(info), nil
	}

	// (5) issue the epoch, write the record as `granting`.
	epoch, err := m.bumpEpochLocked()
	if err != nil {
		return nil, nil, err
	}
	rec, err := m.v2Record(epoch, class, opts, devs)
	if err != nil {
		return nil, nil, err
	}
	rec.State = stateGranting
	if err := writeEpochRecord(m.leaseDir(), rec); err != nil {
		return nil, nil, err
	}

	// (6) the card claims, exclusively; all or nothing.
	var made []string
	rollback := func() {
		for _, d := range made {
			_ = removeClaim(cardClaimPath(m.leaseDir(), d))
		}
		_ = removeClaim(epochRecordPath(m.leaseDir(), epoch))
	}
	for _, d := range devs {
		f, cerr := createClaim(cardClaimPath(m.leaseDir(), d))
		if cerr != nil {
			rollback()
			if os.IsExist(cerr) {
				return nil, &ErrHeld{Info: Info{Held: true, Reason: "card " + d + " is claimed (claim in progress)", Devices: []string{d}}}, nil
			}
			return nil, nil, fmt.Errorf("gpulease: could not claim card %s: %w", d, cerr)
		}
		c, _ := json.Marshal(cardClaim{Epoch: epoch, AtMs: m.now().UnixMilli()})
		_, werr := f.Write(c)
		cerr = f.Close()
		made = append(made, d)
		if werr != nil || cerr != nil {
			rollback()
			return nil, nil, fmt.Errorf("gpulease: writing card claim %s: %w", d, errors.Join(werr, cerr))
		}
	}

	// (7) active.
	rec.State = stateActive
	if err := writeEpochRecord(m.leaseDir(), rec); err != nil {
		rollback()
		return nil, nil, err
	}
	if _, serr := os.Stat(m.formatPath()); serr != nil {
		_ = os.WriteFile(m.formatPath(), []byte(formatVersion+"\n"), 0o666)
	}
	return &Lease{mgr: m, epoch: epoch, class: class, v2: true, devices: devs}, nil, nil
}

// v2Record builds the lease record for a device grant. The holder, window and flags are
// exactly a whole-node record's; Devices and State are what make it a v2 record.
func (m *Manager) v2Record(epoch uint64, class Class, opts Options, devs []string) (*Meta, error) {
	raw, err := m.record(epoch, class, opts)
	if err != nil {
		return nil, err
	}
	var rec Meta
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	rec.Devices = devs
	return &rec, nil
}

// verifyNoDeviceLeases is the whole-node grant's half of the arbitration. The caller has
// just created meta.json. Under the epoch lock, if any device lease is live, the
// whole-node claim is withdrawn and the caller is told the card is held. A device grant
// that began before this claim is serialised by the same lock; one that began after it
// saw meta.json and refused.
//
// A device lease the shared rule judges reclaimable (a holder that is alive but stalled,
// or gone) is not a conflict, and it is not left standing either: it is swept here, under
// the lock and BEFORE anything is judged live, exactly as a device grant sweeps. Leaving
// its record and claims in place would let the stalled holder resume, pass its per-epoch
// fence and renew a heartbeat over cards the whole-node holder now owns. The whole-node
// world fenced that holder by deleting meta.json; this is the same act for v2.
func (m *Manager) verifyNoDeviceLeases(epoch uint64) error {
	var held *ErrHeld
	err := m.withEpochLock(func() error {
		m.sweepDebrisLocked()
		var conflict *Info
		for _, j := range m.reader().v2() {
			if j.Live {
				info := infoFrom(&j.Meta, m.now())
				conflict = &info
				break
			}
		}
		if conflict == nil {
			return nil
		}
		// Withdraw our own claim, and only ours.
		if cur, rerr := m.readMeta(); rerr == nil && cur != nil && cur.Epoch == epoch {
			if err := removeClaim(m.metaPath()); err != nil {
				return fmt.Errorf("gpulease: withdrawing a whole-node claim over a live device lease: %w", err)
			}
		}
		held = m.heldErr(*conflict)
		return nil
	})
	if err != nil {
		// We could not verify. Do not keep a claim we could not prove safe.
		if cur, rerr := m.readMeta(); rerr == nil && cur != nil && cur.Epoch == epoch {
			_ = removeClaim(m.metaPath())
		}
		return err
	}
	if held != nil {
		return held
	}
	return nil
}

// ---------------------------------------------------------------------------
// Debris and per-lease removal
// ---------------------------------------------------------------------------

// sweepDebrisLocked removes what a crashed or finished lease left behind, one lease at
// a time. The caller holds the epoch lock, so no grant, release or restamp is in flight
// on this host. It never removes anything belonging to a live lease.
//
//   - a `granting` record older than claimGrace, with its claims;
//   - an active lease the shared rule judges reclaimable (holder gone), with its claims;
//   - an unreadable record file once it is older than claimGrace;
//   - a card claim whose record is gone, or does not name that card, once its own
//     timestamp is older than claimGrace.
//
// A removal that fails here is left for the next sweep: it is debris, no reader treats
// it as a live lease, and a claim it leaves behind only delays a grant for claimGrace
// ("claim in progress"), never double-books a card. Unlike a release, nobody is waiting
// to be told it worked.
func (m *Manager) sweepDebrisLocked() {
	r := m.reader()
	liveEpoch := map[uint64]*Meta{}
	for _, j := range r.v2() {
		if j.Debris {
			_ = m.removeLeaseFilesLocked(j.Meta.Epoch)
			continue
		}
		meta := j.Meta
		liveEpoch[j.Meta.Epoch] = &meta
	}
	// The inferred-scope sidecars of leases that are gone (an older binary's release does not
	// know them). A whole-node claim on the lease directory is a lease that may own one.
	keep := map[uint64]bool{}
	for e := range liveEpoch {
		keep[e] = true
	}
	if cur, err := m.readMeta(); err == nil && cur != nil {
		keep[cur.Epoch] = true
	}
	m.sweepSeenLocked(keep)
	// Unreadable records.
	if entries, err := os.ReadDir(m.v2Dir()); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			epoch, perr := strconv.ParseUint(strings.TrimSuffix(e.Name(), ".json"), 10, 64)
			if perr != nil {
				continue
			}
			if _, rerr := readEpochRecord(m.leaseDir(), epoch); rerr == nil {
				continue
			}
			if fi, serr := e.Info(); serr == nil && m.now().Sub(fi.ModTime()) > claimGrace {
				_ = m.removeLeaseFilesLocked(epoch)
			}
		}
	}
	// Orphan claims.
	entries, err := os.ReadDir(m.cardsDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".claim") {
			continue
		}
		device := strings.TrimSuffix(e.Name(), ".claim")
		path := cardClaimPath(m.leaseDir(), device)
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		c, ok := parseCardClaim(b)
		age := time.Duration(0)
		if ok && c.AtMs != 0 {
			age = m.now().Sub(time.UnixMilli(c.AtMs))
		} else if fi, serr := e.Info(); serr == nil {
			age = m.now().Sub(fi.ModTime())
		}
		if ok {
			if rec := liveEpoch[c.Epoch]; rec != nil && containsString(rec.Devices, device) {
				continue // a live lease's claim
			}
		}
		if age > claimGrace {
			_ = m.remove(path)
		}
	}
}

// sweepSeen removes the inferred-scope sidecars (seen.<epoch>) of every lease not in keep. The
// listing is cheap and unlocked; the removal runs under the epoch lock, so a reader that is
// persisting a sidecar for a lease it just saw live cannot be raced into recreating one.
func (m *Manager) sweepSeen(keep map[uint64]bool) {
	if len(m.staleSeen(keep)) == 0 {
		return
	}
	_ = m.withEpochLock(func() error {
		m.sweepSeenLocked(keep)
		return nil
	})
}

// sweepSeenLocked is sweepSeen for a caller that holds the epoch lock.
func (m *Manager) sweepSeenLocked(keep map[uint64]bool) {
	for _, path := range m.staleSeen(keep) {
		_ = m.remove(path)
	}
}

func (m *Manager) staleSeen(keep map[uint64]bool) []string {
	entries, err := os.ReadDir(m.leaseDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, seenPrefix) {
			continue
		}
		rest := strings.TrimSuffix(strings.TrimPrefix(name, seenPrefix), ".tmp")
		epoch, perr := strconv.ParseUint(rest, 10, 64)
		if perr != nil || keep[epoch] {
			continue
		}
		out = append(out, filepath.Join(m.leaseDir(), name))
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// removeLeaseFilesLocked removes ONE lease's files: its record, every card claim that
// names its epoch, its heartbeat and its unload marker. Another lease's files are never
// touched. The caller holds the epoch lock.
//
// It returns what it could not remove of the record and the claims (the files that make
// the lease live), joined, after trying every one of them: a release that reports success
// over a record or claim that is still there leaves the card held by a lease whose owner
// believes it let go. The markers (heartbeat, unload) are best-effort: a leftover one
// holds nothing.
func (m *Manager) removeLeaseFilesLocked(epoch uint64) error {
	var errs []error
	if err := m.remove(epochRecordPath(m.leaseDir(), epoch)); err != nil {
		errs = append(errs, err)
	}
	entries, err := os.ReadDir(m.cardsDir())
	switch {
	case err != nil && !os.IsNotExist(err):
		errs = append(errs, err)
	case err == nil:
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".claim") {
				continue
			}
			device := strings.TrimSuffix(e.Name(), ".claim")
			if c, ok := readCardClaim(m.leaseDir(), device); ok && c.Epoch == epoch {
				if rerr := m.remove(cardClaimPath(m.leaseDir(), device)); rerr != nil {
					errs = append(errs, rerr)
				}
			}
		}
	}
	m.clearEpochMarkers(epoch)
	return errors.Join(errs...)
}

// clearEpochMarkers removes the per-epoch sidecars of ONE lease. The whole-node release
// sweeps every hb.* and unloaded.*; with several leases live that would delete a
// sibling's heartbeat and make it reclaimable.
func (m *Manager) clearEpochMarkers(epoch uint64) {
	e := strconv.FormatUint(epoch, 10)
	for _, name := range []string{"unloaded." + e, "hb." + e, "hb." + e + ".tmp", orphanMarkPrefix + e, orphanMarkPrefix + e + ".tmp"} {
		_ = m.remove(filepath.Join(m.leaseDir(), name))
	}
}

// ---------------------------------------------------------------------------
// Holder-side: fence, release, restamp
// ---------------------------------------------------------------------------

// checkV2 is the fence for a device lease: the record is there and active, and every
// card it names carries a claim naming this epoch.
func (l *Lease) checkV2() error {
	rec, err := readEpochRecord(l.mgr.leaseDir(), l.epoch)
	if err != nil {
		return fmt.Errorf("gpulease: lease is gone (epoch %d); another holder may have the GPU", l.epoch)
	}
	if rec.State != stateActive {
		return fmt.Errorf("gpulease: epoch %d is not an active lease (state %q)", l.epoch, rec.State)
	}
	for _, d := range rec.Devices {
		c, ok := readCardClaim(l.mgr.leaseDir(), d)
		if !ok {
			return fmt.Errorf("gpulease: fenced out — card %s has no claim for our epoch %d; another holder has the GPU", d, l.epoch)
		}
		if c.Epoch != l.epoch {
			return fmt.Errorf("gpulease: fenced out — card %s is claimed by epoch %d, our epoch is %d; another holder has the GPU", d, c.Epoch, l.epoch)
		}
	}
	return nil
}

// releaseV2 removes one device lease, under the epoch lock. Idempotent; a lease that
// was already swept leaves nothing to remove, and a claim now naming another epoch is
// left alone. A record or claim that cannot be removed is an error, as it is for a
// whole-node release: the lease is then still live and the caller has to know.
func (m *Manager) releaseV2(epoch uint64) error {
	return m.withEpochLock(func() error {
		return m.removeLeaseFilesLocked(epoch)
	})
}

// restampV2 rewrites one device lease's record in place, under the epoch lock.
func (m *Manager) restampV2(epoch uint64, fn func(*Meta)) error {
	return m.withEpochLock(func() error {
		rec, err := readEpochRecord(m.leaseDir(), epoch)
		if err != nil {
			return fmt.Errorf("gpulease: restamp: the lease is gone (epoch %d)", epoch)
		}
		if !claimsNameEpoch(m.leaseDir(), rec) {
			return fmt.Errorf("gpulease: restamp: fenced out — a card of epoch %d is claimed by another holder", epoch)
		}
		devices := rec.Devices
		fn(rec)
		rec.Epoch = epoch
		rec.Devices = devices
		if rec.State == "" || rec.State == stateGranting {
			rec.State = stateActive
		}
		return writeEpochRecord(m.leaseDir(), rec)
	})
}

// releaseByEpochV2 is ReleaseByEpoch's device-lease half. handled is false when the
// epoch is not a device lease (the caller falls through to the whole-node path).
func (m *Manager) releaseByEpochV2(epoch uint64) (released, handled bool, err error) {
	if epoch != 0 {
		if _, serr := os.Stat(epochRecordPath(m.leaseDir(), epoch)); serr != nil {
			return false, false, nil
		}
		if err := m.releaseV2(epoch); err != nil {
			return false, true, err
		}
		return true, true, nil
	}
	// Epoch 0 is "whatever is held". With a whole-node record that is the old rule; with
	// device leases it is only unambiguous when exactly one is live.
	if meta, _ := m.readMeta(); meta != nil {
		return false, false, nil
	}
	switch live := m.liveCardLeaseEpochs(); len(live) {
	case 0:
		return false, false, nil
	case 1:
		if err := m.releaseV2(live[0]); err != nil {
			return false, true, err
		}
		return true, true, nil
	default:
		return false, true, errSeveralCardLeases(live)
	}
}

// liveCardLeaseEpochs lists the epochs of the live card-scoped leases, lowest first.
func (m *Manager) liveCardLeaseEpochs() []uint64 {
	var live []uint64
	for _, j := range m.reader().v2() {
		if j.Live {
			live = append(live, j.Meta.Epoch)
		}
	}
	return live
}

// errSeveralCardLeases is the refusal for "release whatever is held" when several card leases
// are live: which one is meant is the operator's to say.
func errSeveralCardLeases(live []uint64) error {
	parts := make([]string, len(live))
	for i, e := range live {
		parts[i] = strconv.FormatUint(e, 10)
	}
	return fmt.Errorf("gpulease: %d card-scoped leases are held (epochs %s); pass --epoch N to release one",
		len(live), strings.Join(parts, ", "))
}
