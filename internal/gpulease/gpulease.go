// Package gpulease owns the machine-wide, crash-safe, FENCED GPU lease that every
// GPU consumer takes regardless of how its work arrived.
//
// WHY THIS SUPERSEDES internal/gpulock's READ-ONLY INVARIANT (ADR: see
// docs/architecture/decisions): gpulock deliberately never acquires — acquisition
// lived only in render/gpu-lock.mjs, so the ONLY parties serialized were the Node
// generation runners. Measured consequence: a media job dispatched to
// POST /fleet/dispatch called freeLlamaSwap() and unloaded every GPU-resident model
// out from under an in-flight text benchmark. The llama-swap server log holds 3,356
// such unloads, 330 of them for the text workhorse. Text work never took the lock, so
// nothing could have prevented it. A lock only one side takes is not mutual exclusion.
//
// FOUR PROPERTIES THE OLD LOCK LACKED, each fixing an observed defect:
//
//  1. MACHINE-WIDE. gpu-lock.mjs defaulted to join(tmpdir(), ...), which on Windows is
//     PER-USER (C:\Users\<u>\AppData\Local\Temp). A process in another security context
//     — e.g. a SYSTEM-registered service — silently took a DIFFERENT lock and mutual
//     exclusion evaporated with no error anywhere. The state root is now machine-wide,
//     and if it is not writable we REFUSE TO START rather than fall back per-user:
//     a silent fallback is precisely the bug.
//
//  2. FENCED. The lid of a laptop closing is not a crash — the process survives and
//     resumes later. Without a fence it would resume and call freeLlamaSwap() on top of
//     whoever holds the card now, which is the original incident replayed by a lid.
//     Every acquisition bumps a monotonic epoch; holders re-validate with Check() before
//     each irreversible action (unloading models, submitting a graph, promoting output).
//
//  3. PID-RECYCLE SAFE. The old rule trusted pid liveness alone. A recycled pid reads as
//     "holder alive" forever. We record the holder's process START TIME alongside its pid
//     and treat a mismatch as a dead holder.
//
//  4. TWO CLASSES. `media` (image/video/audio/run-graph) and `text` (a reservation taken
//     by a benchmark/eval). Both are EXCLUSIVE — one card, one holder. The distinction is
//     not access control, it is intent: only a `media` holder may unload models, and a
//     `text` reservation makes a dispatched render WAIT instead of destroying the run.
//     Ordinary interactive text calls are deliberately NOT lease participants: thousands
//     per day at ~46ms, and leasing them is untenable. That asymmetry is a known limit,
//     not an oversight — see docs.
package gpulease

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Class labels what the holder intends to do with the card. Both classes are
// exclusive; the label drives POLICY (who may unload, what a waiter is told).
type Class string

const (
	// ClassMedia is a generation job. Its holder is the ONLY party permitted to
	// unload llama-swap models (freeLlamaSwap), and it must do so once per LEASE,
	// not once per job — the per-job call is what produced 330 teardowns.
	ClassMedia Class = "media"
	// ClassText is a reservation held by a benchmark, eval, or measured run. Its
	// holder unloads nothing; it exists so media work WAITS rather than tearing the
	// text tier down mid-measurement.
	ClassText Class = "text"
)

// Valid reports whether c is a known class. Unknown classes are refused at the
// boundary so a typo can never create a third, unarbitrated category.
func (c Class) Valid() bool { return c == ClassMedia || c == ClassText }

const (
	// DefaultHeartbeatTTL is how long a holder may go without renewing before the
	// heartbeat is considered stale. Deliberately generous: under a saturating
	// benchmark the holder process can be descheduled for a long time, and expiring
	// its lease during exactly the load it was taken to protect would reproduce the
	// incident. Staleness alone never reclaims — see Reclaimable.
	DefaultHeartbeatTTL = 120 * time.Second

	// DefaultTTL is the fallback lease duration when a caller does not stamp one.
	// A real video generation runs many minutes.
	DefaultTTL = time.Hour

	// leaseDirName / epochFileName / waitersDirName live under <root>/gpu/.
	leaseDirName   = "lease"
	epochFileName  = "epoch"
	waitersDirName = "waiters"
	metaFileName   = "meta.json"
)

// ErrHeld is returned by TryAcquire when the card is legitimately held by someone
// else. It carries the current holder so a caller can report an honest ETA rather
// than a bare failure.
type ErrHeld struct{ Info Info }

func (e *ErrHeld) Error() string {
	return fmt.Sprintf("GPU held by %s (pid %d, held %s, reason %q)",
		e.Info.Class, e.Info.PID, e.Info.Age.Round(time.Second), e.Info.Reason)
}

// Holder identifies the process holding the lease. StartTimeMs is an opaque,
// platform-specific process-start identity used ONLY for equality comparison; it
// closes the pid-recycling hole that pid-liveness alone cannot see.
type Holder struct {
	PID         int   `json:"pid"`
	StartTimeMs int64 `json:"start_time_ms"`
}

// Meta is the on-disk lease record (<root>/gpu/lease/meta.json).
type Meta struct {
	Epoch        uint64 `json:"epoch"`
	Class        Class  `json:"class"`
	Holder       Holder `json:"holder"`
	JobID        string `json:"job_id,omitempty"`
	Origin       string `json:"origin,omitempty"`
	Reason       string `json:"reason,omitempty"`
	AcquiredAtMs int64  `json:"acquired_at_ms"`
	// ExpiresAtMs is stamped by the ACQUIRER from its own declared duration. It is
	// half of the reclaim conjunction: a heartbeat gap alone must never reclaim a
	// lease whose owner declared a long, still-unexpired window.
	ExpiresAtMs int64 `json:"expires_at_ms"`
	// RenewedAtMs is the heartbeat. A detached holder is a PROXY process, so pid
	// liveness cannot detect a benchmark that died behind it — hence the heartbeat.
	RenewedAtMs int64 `json:"renewed_at_ms"`
}

// Info is one point-in-time inspection.
type Info struct {
	Held      bool
	Class     Class
	Epoch     uint64
	PID       int
	Age       time.Duration
	Reason    string
	Origin    string
	JobID     string
	ExpiresAt time.Time
}

// Options configure an acquisition.
type Options struct {
	Reason string
	Origin string
	JobID  string
	// TTL is how long the holder declares it will need the card. Stamped into
	// ExpiresAtMs. Zero uses DefaultTTL.
	TTL time.Duration
}

// Manager binds a resolved state root. Construct with Open, which performs the
// refuse-to-start validation exactly once.
type Manager struct {
	root         string
	heartbeatTTL time.Duration
	now          func() time.Time
	procStart    func(pid int) (int64, bool)
	pid          int
}

// Open resolves and VALIDATES the state root, then returns a Manager. It returns an
// error rather than degrading: an unwritable or cloud-synced root is a refuse-to-start
// condition, never a silent fallback.
func Open(override string) (*Manager, error) {
	root, err := ResolveStateRoot(override)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "gpu"), 0o777); err != nil {
		return nil, fmt.Errorf("gpulease: state root %q is not usable: %w", root, err)
	}
	if err := probeWritable(filepath.Join(root, "gpu")); err != nil {
		return nil, err
	}
	return &Manager{
		root:         root,
		heartbeatTTL: DefaultHeartbeatTTL,
		now:          time.Now,
		procStart:    processStart,
		pid:          os.Getpid(),
	}, nil
}

// Root is the resolved state root (for diagnostics and for threading to the Node
// side as GPU_LEASE_DIR).
func (m *Manager) Root() string      { return m.root }
func (m *Manager) gpuDir() string    { return filepath.Join(m.root, "gpu") }
func (m *Manager) leaseDir() string  { return filepath.Join(m.gpuDir(), leaseDirName) }
func (m *Manager) epochPath() string { return filepath.Join(m.gpuDir(), epochFileName) }
func (m *Manager) metaPath() string  { return filepath.Join(m.leaseDir(), metaFileName) }

// ResolveStateRoot picks the machine-wide state root and REFUSES bad ones.
//
// Order: explicit override (config state_dir) > LOCAL_OFFLOAD_STATE_DIR env > platform
// default. The platform default is machine-wide on purpose (%ProgramData% on Windows,
// /var/lib on POSIX) — a per-user default is the defect this package exists to fix.
func ResolveStateRoot(override string) (string, error) {
	root := strings.TrimSpace(override)
	if root == "" {
		root = strings.TrimSpace(os.Getenv("LOCAL_OFFLOAD_STATE_DIR"))
	}
	if root == "" {
		root = defaultStateRoot()
	}
	if root == "" {
		return "", errors.New("gpulease: could not determine a machine-wide state root; set state_dir in config")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("gpulease: state root %q is not a valid path: %w", root, err)
	}
	if why := syncRootReason(abs); why != "" {
		return "", fmt.Errorf("gpulease: refusing state root %q: %s. A cloud-sync client "+
			"replicating a LOCK FILE between machines would hand the same GPU to two hosts. "+
			"Set state_dir to a local, unsynced path", abs, why)
	}
	return abs, nil
}

// defaultStateRoot is the machine-wide default per platform.
func defaultStateRoot() string {
	if runtime.GOOS == "windows" {
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, "local-offload")
		}
		return `C:\ProgramData\local-offload`
	}
	return "/var/lib/local-offload"
}

// syncRootMarkers are path segments that indicate a cloud-sync-managed directory.
// Matching is case-insensitive and segment-aware so a legitimate path such as
// "/srv/dropbox-exporter" is not refused for containing the substring.
var syncRootMarkers = []string{
	"my drive", "google drive", "googledrive",
	"onedrive", "dropbox", "icloumdrive", "icloud drive", "icloud",
	"box sync", "nextcloud", "owncloud", "syncthing", "pcloud", "mega",
}

// syncRootReason returns a non-empty explanation when p sits under a known
// cloud-sync root. Empty means the path is acceptable.
func syncRootReason(p string) string {
	norm := strings.ToLower(filepath.ToSlash(p))
	for _, seg := range strings.Split(norm, "/") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		for _, marker := range syncRootMarkers {
			if seg == marker {
				return "path segment " + strconv.Quote(seg) + " is a cloud-sync root"
			}
		}
	}
	return ""
}

// probeWritable proves the directory is actually writable NOW, rather than assuming
// it from a successful MkdirAll (which succeeds on an existing read-only dir).
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".writeprobe-*")
	if err != nil {
		return fmt.Errorf("gpulease: state dir %q is not writable: %w. Refusing to fall back "+
			"to a per-user path — that silently breaks mutual exclusion across security contexts", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

// ---------------------------------------------------------------------------
// Reclaim rule
// ---------------------------------------------------------------------------

// Reclaimable decides whether an existing lease may be taken over. The rule is a
// deliberate CONJUNCTION, and both halves are load-bearing:
//
//	(holder is provably gone)  OR  (heartbeat is stale AND the declared window has expired)
//
// A bare heartbeat timeout is wrong: under the saturating benchmark a text
// reservation exists to protect, the holder can stall past any short TTL, and
// expiring it hands the card to a render mid-measurement — the exact incident.
// Pid-liveness alone is also wrong: `reserve --detach` spawns a PROXY holder that
// stays alive after the benchmark behind it dies, so a dead run could hold the card
// for its full declared window. Requiring BOTH staleness and expiry for the
// liveness-independent path, while letting provable death reclaim immediately,
// is the only combination that survives both.
func Reclaimable(m *Meta, now time.Time, heartbeatTTL time.Duration, procStart func(int) (int64, bool)) bool {
	if m == nil {
		return true // no/unreadable meta => stale, same rule the .mjs used
	}
	if m.Holder.PID <= 0 {
		// No identifiable holder: fall back to the declared window alone.
		return now.UnixMilli() > m.ExpiresAtMs
	}
	if !pidAlive(m.Holder.PID) {
		return true // provably gone
	}
	// Alive pid, but is it the SAME process? A recycled pid otherwise reads as a
	// live holder forever.
	if m.Holder.StartTimeMs != 0 {
		if st, ok := procStart(m.Holder.PID); ok && st != m.Holder.StartTimeMs {
			return true // pid was recycled; the original holder is gone
		}
	}
	nowMs := now.UnixMilli()
	heartbeatStale := m.RenewedAtMs > 0 && nowMs-m.RenewedAtMs > heartbeatTTL.Milliseconds()
	windowExpired := nowMs > m.ExpiresAtMs
	return heartbeatStale && windowExpired
}

// ---------------------------------------------------------------------------
// Inspection
// ---------------------------------------------------------------------------

// Inspect reports the current lease state. A reclaimable lease reports NOT held.
func (m *Manager) Inspect() Info {
	meta, err := m.readMeta()
	if err != nil || meta == nil {
		return Info{}
	}
	now := m.now()
	if Reclaimable(meta, now, m.heartbeatTTL, m.procStart) {
		return Info{}
	}
	age := now.Sub(time.UnixMilli(meta.AcquiredAtMs))
	if age < 0 {
		age = 0
	}
	return Info{
		Held:      true,
		Class:     meta.Class,
		Epoch:     meta.Epoch,
		PID:       meta.Holder.PID,
		Age:       age,
		Reason:    meta.Reason,
		Origin:    meta.Origin,
		JobID:     meta.JobID,
		ExpiresAt: time.UnixMilli(meta.ExpiresAtMs),
	}
}

func (m *Manager) readMeta() (*Meta, error) {
	b, err := os.ReadFile(m.metaPath())
	if err != nil {
		return nil, err
	}
	var meta Meta
	if err := json.Unmarshal(b, &meta); err != nil {
		return nil, err // corrupt => treated as stale by callers
	}
	return &meta, nil
}

// ---------------------------------------------------------------------------
// Acquisition
// ---------------------------------------------------------------------------

// Lease is a held lease. Every irreversible action must be preceded by Check().
type Lease struct {
	mgr   *Manager
	epoch uint64
	class Class
	done  bool
}

// Epoch is the fencing token. Threaded to child processes as GPU_LEASE_EPOCH so an
// inherited-lease child can prove it still holds what its parent acquired.
func (l *Lease) Epoch() uint64 { return l.epoch }
func (l *Lease) Class() Class  { return l.class }
func (l *Lease) Dir() string   { return l.mgr.leaseDir() }

// TryAcquire attempts to take the card once. It returns *ErrHeld when someone else
// legitimately holds it, so the caller can report a real ETA instead of a bare error.
func (m *Manager) TryAcquire(class Class, opts Options) (*Lease, error) {
	if !class.Valid() {
		return nil, fmt.Errorf("gpulease: unknown class %q (want %q or %q)", class, ClassMedia, ClassText)
	}
	for attempt := 0; attempt < 3; attempt++ {
		err := os.Mkdir(m.leaseDir(), 0o777) // atomic on every OS
		if err == nil {
			lease, werr := m.stamp(class, opts)
			if werr != nil {
				_ = os.RemoveAll(m.leaseDir()) // never leave a lease we failed to stamp
				return nil, werr
			}
			return lease, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("gpulease: could not create lease dir: %w", err)
		}
		meta, _ := m.readMeta()
		if Reclaimable(meta, m.now(), m.heartbeatTTL, m.procStart) {
			// Reclaim and retry. Bounded retries stop two racing reclaimers from
			// spinning against each other forever.
			_ = os.RemoveAll(m.leaseDir())
			continue
		}
		return nil, &ErrHeld{Info: m.Inspect()}
	}
	return nil, &ErrHeld{Info: m.Inspect()}
}

// stamp bumps the epoch and writes meta.json for a freshly created lease dir.
func (m *Manager) stamp(class Class, opts Options) (*Lease, error) {
	epoch, err := m.bumpEpoch()
	if err != nil {
		return nil, err
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	now := m.now()
	start, _ := m.procStart(m.pid)
	meta := Meta{
		Epoch:        epoch,
		Class:        class,
		Holder:       Holder{PID: m.pid, StartTimeMs: start},
		JobID:        opts.JobID,
		Origin:       opts.Origin,
		Reason:       opts.Reason,
		AcquiredAtMs: now.UnixMilli(),
		ExpiresAtMs:  now.Add(ttl).UnixMilli(),
		RenewedAtMs:  now.UnixMilli(),
	}
	if err := m.writeMeta(&meta); err != nil {
		return nil, err
	}
	return &Lease{mgr: m, epoch: epoch, class: class}, nil
}

// writeMeta writes meta.json atomically (temp + rename) so a reader never observes
// a half-written record.
func (m *Manager) writeMeta(meta *Meta) error {
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp := m.metaPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o666); err != nil {
		return fmt.Errorf("gpulease: writing lease meta: %w", err)
	}
	if err := os.Rename(tmp, m.metaPath()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("gpulease: publishing lease meta: %w", err)
	}
	return nil
}

// bumpEpoch increments the monotonic fencing counter. The counter lives OUTSIDE the
// lease dir so it survives reclaim and can never go backwards when a lease is removed.
func (m *Manager) bumpEpoch() (uint64, error) {
	cur := uint64(0)
	if b, err := os.ReadFile(m.epochPath()); err == nil {
		if v, perr := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); perr == nil {
			cur = v
		}
	}
	next := cur + 1
	tmp := m.epochPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatUint(next, 10)), 0o666); err != nil {
		return 0, fmt.Errorf("gpulease: writing epoch: %w", err)
	}
	if err := os.Rename(tmp, m.epochPath()); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("gpulease: publishing epoch: %w", err)
	}
	return next, nil
}

// Check is the FENCE. Call it immediately before every irreversible action:
// unloading models, submitting a graph to ComfyUI, promoting an output file.
// A non-nil error means we no longer hold the card and must abort — not retry.
func (l *Lease) Check() error {
	if l.done {
		return errors.New("gpulease: lease already released")
	}
	meta, err := l.mgr.readMeta()
	if err != nil || meta == nil {
		return fmt.Errorf("gpulease: lease is gone (epoch %d); another holder has the GPU", l.epoch)
	}
	if meta.Epoch != l.epoch {
		return fmt.Errorf("gpulease: fenced out — our epoch %d, current epoch %d; another holder has the GPU",
			l.epoch, meta.Epoch)
	}
	return nil
}

// Renew refreshes the heartbeat. A long holder should call this periodically; the
// reclaim rule requires BOTH a stale heartbeat and an expired window, so missing a
// few renewals inside the declared window is harmless by design.
func (l *Lease) Renew() error {
	if err := l.Check(); err != nil {
		return err
	}
	meta, err := l.mgr.readMeta()
	if err != nil || meta == nil {
		return errors.New("gpulease: lease vanished during renew")
	}
	meta.RenewedAtMs = l.mgr.now().UnixMilli()
	return l.mgr.writeMeta(meta)
}

// ReleaseByEpoch drops the CURRENT lease from a process that does not hold it (the
// `gpu release` verb). Passing a non-zero epoch makes it a compare-and-delete: the
// lease is removed only if it is still the one the caller saw. Zero means "release
// whatever is there", which is the operator's explicit override.
//
// A detached holder notices its lease vanish on its next Check() and exits on its
// own, so releasing never has to kill a process by pid — which would be unsafe under
// pid recycling.
func (m *Manager) ReleaseByEpoch(epoch uint64) (bool, error) {
	meta, err := m.readMeta()
	if err != nil || meta == nil {
		return false, nil // nothing held
	}
	if epoch != 0 && meta.Epoch != epoch {
		return false, fmt.Errorf("gpulease: lease has moved on (asked for epoch %d, current is %d)", epoch, meta.Epoch)
	}
	if err := os.RemoveAll(m.leaseDir()); err != nil {
		return false, fmt.Errorf("gpulease: releasing lease: %w", err)
	}
	return true, nil
}

// Release drops the lease — but ONLY if we still hold it. Releasing unconditionally
// would let a fenced-out straggler delete the CURRENT holder's lease, which is a
// worse failure than leaking one: it silently hands the card to a third party.
func (l *Lease) Release() error {
	if l.done {
		return nil
	}
	l.done = true
	meta, err := l.mgr.readMeta()
	if err != nil || meta == nil {
		return nil // already gone
	}
	if meta.Epoch != l.epoch {
		return nil // fenced out; the lease is someone else's now — leave it alone
	}
	if err := os.RemoveAll(l.mgr.leaseDir()); err != nil {
		return fmt.Errorf("gpulease: releasing lease: %w", err)
	}
	return nil
}
