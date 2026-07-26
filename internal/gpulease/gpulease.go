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
	// epochLockName serializes the read-modify-write of the fencing counter. See
	// withEpochLock for why tmp+rename alone is not enough.
	epochLockName = "epoch.lock"
)

const (
	// epochLockStale bounds how long a lock file may sit before it is treated as debris
	// from a process that died mid-bump rather than as live contention. The window it
	// guards is two file operations wide, so this is orders of magnitude generous.
	epochLockStale = 30 * time.Second
	// epochLockAttempts x epochLockPause bound the spin. A bump is two file operations;
	// anything that cannot complete inside this is a wedged lock, not a queue.
	epochLockAttempts = 400
	epochLockPause    = 5 * time.Millisecond

	// acquirePollInterval is how often Acquire re-probes a held card. Frequent enough
	// that a waiter starts within a second of a release, cheap enough to be free.
	acquirePollInterval = time.Second
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
	// Wait is how long Acquire keeps retrying while the card is legitimately held by
	// someone else. Zero means try exactly once. Only contention is waited out; a
	// configuration fault is returned immediately, because waiting cannot fix it.
	Wait time.Duration
}

// Manager binds a resolved state root. Construct with Open, which performs the
// refuse-to-start validation exactly once.
type Manager struct {
	root string
	// leaseOverride, when set, is the fully resolved lease directory (see LeaseDir).
	// It exists so every consumer can be handed the SAME directory rather than each
	// re-deriving one from a different input.
	leaseOverride string
	heartbeatTTL  time.Duration
	now           func() time.Time
	// sleep pauses between Acquire probes, and pollEvery is how long each pause is.
	// Both are injectable so a waiting test can advance its own clock instead of
	// spending real seconds.
	sleep     func(time.Duration)
	pollEvery time.Duration
	procStart func(pid int) (int64, bool)
	pid       int
}

// pause waits between Acquire probes, tolerating a Manager built without a sleep seam.
func (m *Manager) pause(d time.Duration) {
	if m.sleep == nil {
		time.Sleep(d)
		return
	}
	m.sleep(d)
}

// pollInterval is how long Acquire waits between probes.
func (m *Manager) pollInterval() time.Duration {
	if m.pollEvery > 0 {
		return m.pollEvery
	}
	return acquirePollInterval
}

// LeaseDir is THE resolver every consumer must use — the CLI, the pipeline, the
// read-only vision gate, and the render runners via GPU_LEASE_DIR.
//
// It exists because a second resolution order is how the lease silently splits in two.
// `gpu_lock_path`/GPU_LOCK were honoured by some consumers and ignored by others, so
// setting that supported field put the reservation verb and the render path on
// DIFFERENT directories, where they never contend — the original incident, restored
// with no warning. One function, one answer.
func LeaseDir(lockOverride, stateDir string) (string, error) {
	if o := strings.TrimSpace(lockOverride); o != "" {
		abs, err := filepath.Abs(o)
		if err != nil {
			return "", fmt.Errorf("gpulease: gpu_lock_path %q is not a valid path: %w", o, err)
		}
		if why := syncRootReason(abs); why != "" {
			return "", fmt.Errorf("gpulease: refusing gpu_lock_path %q: %s", abs, why)
		}
		return abs, nil
	}
	if v := strings.TrimSpace(os.Getenv("GPU_LOCK")); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			return "", fmt.Errorf("gpulease: GPU_LOCK %q is not a valid path: %w", v, err)
		}
		if why := syncRootReason(abs); why != "" {
			return "", fmt.Errorf("gpulease: refusing GPU_LOCK %q: %s", abs, why)
		}
		return abs, nil
	}
	root, err := ResolveStateRoot(stateDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "gpu", leaseDirName), nil
}

// Open resolves and VALIDATES the lease location, then returns a Manager. It returns
// an error rather than degrading: an unwritable or cloud-synced location is a
// refuse-to-start condition, never a silent fallback.
func Open(override string) (*Manager, error) { return OpenAt("", override) }

// OpenAt is Open with an explicit gpu_lock_path override, so the CLI and the pipeline
// resolve exactly the directory the render runners will be told to use.
func OpenAt(lockOverride, stateDir string) (*Manager, error) {
	lease, err := LeaseDir(lockOverride, stateDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(lease, 0o777); err != nil {
		return nil, fmt.Errorf("gpulease: lease dir %q is not usable: %w", lease, err)
	}
	if err := probeWritable(lease); err != nil {
		return nil, err
	}
	return &Manager{
		leaseOverride: lease,
		root:          filepath.Dir(filepath.Dir(lease)),
		heartbeatTTL:  DefaultHeartbeatTTL,
		now:           time.Now,
		sleep:         time.Sleep,
		procStart:     processStart,
		pid:           os.Getpid(),
	}, nil
}

// Root is the resolved state root (for diagnostics and for threading to the Node
// side as GPU_LEASE_DIR).
func (m *Manager) Root() string { return m.root }
func (m *Manager) gpuDir() string {
	if m.leaseOverride != "" {
		return filepath.Dir(m.leaseOverride)
	}
	return filepath.Join(m.root, "gpu")
}
func (m *Manager) leaseDir() string {
	if m.leaseOverride != "" {
		return m.leaseOverride
	}
	return filepath.Join(m.gpuDir(), leaseDirName)
}
func (m *Manager) epochPath() string     { return filepath.Join(m.gpuDir(), epochFileName) }
func (m *Manager) epochLockPath() string { return filepath.Join(m.gpuDir(), epochLockName) }
func (m *Manager) metaPath() string      { return filepath.Join(m.leaseDir(), metaFileName) }

// clearUnloadMarkers removes the per-epoch "somebody already unloaded for this lease"
// markers the render path elects with. They MUST be cleared when the lease ends: a
// marker outliving its lease, combined with any epoch reuse, silently suppresses a
// needed unload and the next render runs against a full card.
func (m *Manager) clearUnloadMarkers() {
	entries, err := os.ReadDir(m.leaseDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "unloaded.") || strings.HasPrefix(e.Name(), "hb.") {
			_ = os.Remove(filepath.Join(m.leaseDir(), e.Name()))
		}
	}
}

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

// syncRootExact are path segments that are cloud-sync roots only when they match a
// whole segment. Kept separate from the prefix set so short, ambiguous words cannot
// swallow legitimate directory names.
var syncRootExact = []string{
	"my drive", "google drive", "googledrive", "shared drives",
	"box sync", "nextcloud", "owncloud", "syncthing", "pcloud", "mega",
}

// syncRootPrefixes are the vendors whose real-world directory names carry suffixes:
// "OneDrive - Contoso", "Dropbox (Personal)", "iCloudDrive". Exact matching alone
// accepted every one of those.
var syncRootPrefixes = []string{"onedrive", "dropbox", "icloud"}

// syncRootReason returns a non-empty explanation when p sits under a known cloud-sync
// root. Empty means the path is acceptable.
//
// Matching is case-insensitive and SEGMENT-AWARE, never a bare substring: a legitimate
// "/srv/dropbox-exporter" must not be refused. Segment-aware prefixing keeps
// "Dropbox (Personal)" refused while leaving "dropbox-exporter" alone, because the
// latter's segment continues with '-' rather than ending or opening a qualifier.
func syncRootReason(p string) string {
	norm := strings.ToLower(filepath.ToSlash(p))
	for _, seg := range strings.Split(norm, "/") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		for _, marker := range syncRootExact {
			if seg == marker {
				return "path segment " + strconv.Quote(seg) + " is a cloud-sync root"
			}
		}
		for _, marker := range syncRootPrefixes {
			if seg == marker {
				return "path segment " + strconv.Quote(seg) + " is a cloud-sync root"
			}
			// "onedrive - contoso", "dropbox (personal)", "iclouddrive"
			if strings.HasPrefix(seg, marker) {
				rest := seg[len(marker):]
				if rest == "" || rest[0] == ' ' || rest[0] == '(' || rest == "drive" {
					return "path segment " + strconv.Quote(seg) + " is a cloud-sync root"
				}
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

// reclaimable applies the shared rule using the PER-EPOCH heartbeat file rather than
// the value frozen into the record at acquisition. Without this, a holder that renews
// correctly still looks stale to every reader, because the record's RenewedAtMs is
// never updated any more.
func (m *Manager) reclaimable(meta *Meta, now time.Time) bool {
	if meta == nil {
		return true
	}
	effective := *meta
	effective.RenewedAtMs = m.lastHeartbeat(meta)
	return Reclaimable(&effective, now, m.heartbeatTTL, m.procStart)
}

// InspectDir reports the lease state at an explicit lease directory, for READ-ONLY
// consumers that do not own a Manager (the vision gate in internal/gpulock).
//
// It exists so there is ONE inspection path. A second reader that reconstructs the
// judgement from the record alone gets it wrong the moment any detail moves — when the
// heartbeat became a per-epoch file, a record-only reader saw a frozen RenewedAtMs and
// would call a live, renewing holder stale as soon as its declared window lapsed. Same
// divergence as the path split and the schema split, one more layer down.
func InspectDir(leaseDir string) Info { return inspectDirAt(leaseDir, time.Now(), DefaultHeartbeatTTL) }

func inspectDirAt(leaseDir string, now time.Time, hbTTL time.Duration) Info {
	b, err := os.ReadFile(filepath.Join(leaseDir, metaFileName))
	if err != nil {
		return Info{}
	}
	var meta Meta
	if json.Unmarshal(b, &meta) != nil {
		return Info{}
	}
	effective := meta
	effective.RenewedAtMs = heartbeatAt(leaseDir, &meta)
	if Reclaimable(&effective, now, hbTTL, processStart) {
		return Info{}
	}
	return infoFrom(&meta, now)
}

// heartbeatAt reads the per-epoch heartbeat beside a lease record, falling back to the
// value stamped at acquisition when nothing has renewed yet.
func heartbeatAt(leaseDir string, meta *Meta) int64 {
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

// infoFrom builds the public view. Age is time HELD, from the acquirer's own stamp.
func infoFrom(meta *Meta, now time.Time) Info {
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

// Inspect reports the current lease state. A reclaimable lease reports NOT held.
func (m *Manager) Inspect() Info {
	meta, err := m.readMeta()
	if err != nil || meta == nil {
		return Info{}
	}
	now := m.now()
	if m.reclaimable(meta, now) {
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
		// THE DIRECTORY IS A CONTAINER, NEVER A CLAIM. Creating it must be
		// non-exclusive: when Go claimed by creating the directory while Node claimed
		// by exclusively creating meta.json, the two sides held DIFFERENT atomic
		// tokens, and a Node claim landing between Go's mkdir and Go's record was
		// silently overwritten — one GPU, two holders. Regression test:
		// TestGoDoesNotClobberANodeClaimMadeInsideItsAcquireWindow.
		if err := os.MkdirAll(m.leaseDir(), 0o777); err != nil {
			return nil, fmt.Errorf("gpulease: could not create lease dir: %w", err)
		}

		// The epoch is taken BEFORE the claim so the record can be written in the SAME
		// call that creates it. Creating the token empty and filling it in afterwards
		// left a window where the claim was present but unparseable — a reader with no
		// grace period reads that as debris and deletes a live claim. A failed claim
		// therefore burns an epoch number, which is harmless: the counter only has to
		// be monotonic, never gapless.
		epoch, eerr := m.bumpEpoch()
		if eerr != nil {
			return nil, eerr
		}
		rec, rerr := m.record(epoch, class, opts)
		if rerr != nil {
			return nil, rerr
		}

		// THE CLAIM: exclusive creation of meta.json, written complete in one call.
		// This single token is what makes cross-language mutual exclusion real rather
		// than merely documented.
		f, err := os.OpenFile(m.metaPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
		if err == nil {
			_, werr := f.Write(rec)
			cerr := f.Close()
			if werr != nil || cerr != nil {
				_ = removeClaim(m.metaPath()) // drop a claim we could not complete
				return nil, fmt.Errorf("gpulease: writing lease claim: %w", errors.Join(werr, cerr))
			}
			return &Lease{mgr: m, epoch: epoch, class: class}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("gpulease: could not claim the lease: %w", err)
		}

		// Someone else holds the claim.
		meta, rerr := m.readMeta()
		if rerr != nil && m.claimIsFresh() {
			// Present but not yet parseable: a claim caught mid-write. Treat it as
			// held rather than stealing it — the alternative is a race where two
			// acquirers each decide the other's half-written claim is garbage.
			return nil, &ErrHeld{Info: Info{Held: true, Reason: "claim in progress"}}
		}
		if m.reclaimable(meta, m.now()) {
			// Remove only the CLAIM, never the container: another acquirer may be
			// working inside this directory right now.
			if err := removeClaim(m.metaPath()); err != nil {
				// A reclaim we cannot perform is not a free lease. Report it as held
				// rather than looping — spinning here once pegged a core forever.
				return nil, fmt.Errorf("gpulease: cannot reclaim the stale lease at %s: %w", m.metaPath(), err)
			}
			continue
		}
		return nil, &ErrHeld{Info: m.Inspect()}
	}
	return nil, &ErrHeld{Info: m.Inspect()}
}

// Acquire takes the card, retrying for up to opts.Wait while someone else legitimately
// holds it. Zero Wait is exactly TryAcquire.
//
// WHY A BOUNDED WAIT AND NOT A SINGLE TRY: the lock this package replaced existed to
// ORGANIZE concurrent GPU jobs into a serial queue, not to cancel them — a render that
// arrives thirty seconds into someone else's render should run thirty seconds later,
// not fail. Trying once turns ordinary contention into a dropped job.
//
// WHY BOUNDED AND NOT THE OLD 30 MINUTES: the caller is usually a single tool call, and
// a tool call that blocks for half an hour is indistinguishable from a hang. Past the
// window the honest answer — "held by <class>, <n>s in" — is more useful to an agent
// that can retry than another twenty minutes of silence.
//
// Only *ErrHeld is retried. An unwritable or cloud-synced lease location is returned
// immediately: it is a configuration fault, and waiting cannot fix it.
func (m *Manager) Acquire(class Class, opts Options) (*Lease, error) {
	lease, err := m.TryAcquire(class, opts)
	if err == nil || opts.Wait <= 0 {
		return lease, err
	}
	var held *ErrHeld
	if !errors.As(err, &held) {
		return nil, err
	}
	deadline := m.now().Add(opts.Wait)
	for {
		remaining := deadline.Sub(m.now())
		if remaining <= 0 {
			return nil, err // the most recent holder, not the first one we saw
		}
		pause := m.pollInterval()
		if remaining < pause {
			pause = remaining
		}
		m.pause(pause)

		lease, aerr := m.TryAcquire(class, opts)
		if aerr == nil {
			return lease, nil
		}
		if !errors.As(aerr, &held) {
			return nil, aerr
		}
		err = aerr
	}
}

// claimGrace is how long a present-but-unparseable meta.json is treated as a claim in
// progress rather than debris. The write follows its exclusive create immediately, so
// this only has to cover a scheduling hiccup.
const claimGrace = 10 * time.Second

// claimIsFresh reports whether the claim file was created recently enough to be a
// write in progress.
func (m *Manager) claimIsFresh() bool {
	fi, err := os.Stat(m.metaPath())
	if err != nil {
		return false
	}
	return m.now().Sub(fi.ModTime()) < claimGrace
}

// record builds the complete lease record, so the claim can be created and filled in
// by a single write. Nothing observes a partially written token.
func (m *Manager) record(epoch uint64, class Class, opts Options) ([]byte, error) {
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
	b, err := json.Marshal(&meta)
	if err != nil {
		return nil, fmt.Errorf("gpulease: encoding lease record: %w", err)
	}
	return b, nil
}

// bumpEpoch issues the next fencing token. The counter lives OUTSIDE the lease dir so
// it survives reclaim and can never go backwards when a lease is removed.
//
// THE INCREMENT IS SERIALIZED, not merely written atomically. tmp+rename makes each
// WRITE indivisible and does nothing about two acquirers that both READ n and both
// write n+1 — measured: two concurrent acquirers were issued the SAME token. A
// duplicate token is not a cosmetic defect, because the token is threaded to child
// processes as GPU_LEASE_EPOCH: a straggling child of lease A passes Check() against a
// LATER lease B that reused the number, may unload models on top of B, and its release
// deletes B's live claim. Issuance must therefore be a critical section.
func (m *Manager) bumpEpoch() (uint64, error) {
	var next uint64
	err := m.withEpochLock(func() error {
		cur, rerr := m.readEpoch()
		if rerr != nil {
			return rerr
		}
		next = cur + 1
		return m.writeEpoch(next)
	})
	if err != nil {
		return 0, err
	}
	return next, nil
}

// withEpochLock runs fn while holding an exclusive-create lock beside the counter.
// Exclusive creation is the same primitive the claim itself uses, so it works across
// processes and languages rather than only across goroutines.
func (m *Manager) withEpochLock(fn func() error) error {
	lock := m.epochLockPath()
	for attempt := 0; ; attempt++ {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
		if err == nil {
			_ = f.Close()
			defer os.Remove(lock)
			return fn()
		}
		// CONTENTION ON WINDOWS DOES NOT ALWAYS LOOK LIKE EEXIST. Deleting a file whose
		// handle is still open marks it delete-PENDING, and a create issued in that
		// window fails with ACCESS_DENIED rather than "already exists". Treating that as
		// fatal turned an ordinary release-and-reacquire race into a hard acquisition
		// failure — measured, with 8 concurrent acquirers. Both spellings mean "someone
		// else has it", so both retry; a genuinely unwritable root is already refused up
		// front by probeWritable, and anything still failing here falls out of the spin
		// below with a message naming both causes.
		if !os.IsExist(err) && !errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("gpulease: could not take the epoch lock at %s: %w", lock, err)
		}
		// A process that died mid-bump must not wedge every future acquisition on this
		// machine. The critical section is two file operations wide, so a lock older
		// than epochLockStale is debris, not contention.
		if fi, serr := os.Stat(lock); serr == nil && time.Since(fi.ModTime()) > epochLockStale {
			_ = os.Remove(lock)
			continue
		}
		if attempt >= epochLockAttempts {
			return fmt.Errorf("gpulease: the epoch lock at %s did not clear in %s; "+
				"remove it if no acquisition is in flight, and check the directory is writable",
				lock, epochLockAttempts*epochLockPause)
		}
		time.Sleep(epochLockPause)
	}
}

const (
	// removeAttempts x removePause bound the retry on removing the claim. A reader holds
	// its handle for microseconds, so this is generous by orders of magnitude.
	removeAttempts = 20
	removePause    = 5 * time.Millisecond
)

// removeClaim deletes a file, retrying a sharing violation.
//
// WHY THIS IS NOT PARANOIA: on Windows a plain read (os.ReadFile) opens WITHOUT
// FILE_SHARE_DELETE, so any concurrent reader makes os.Remove fail outright. Every
// waiter polls the claim while it waits, so a release landing during someone else's
// poll would fail — and a release that fails LEAKS the lease until both halves of the
// reclaim rule fire, blocking the card behind nobody. Measured under six concurrent
// acquire/release cycles. The overlap is microseconds wide, so retrying clears it.
func removeClaim(path string) error {
	for attempt := 0; ; attempt++ {
		err := os.Remove(path)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		if attempt >= removeAttempts {
			return err
		}
		time.Sleep(removePause)
	}
}

// readEpoch returns the current fencing counter. Callers must hold the epoch lock.
func (m *Manager) readEpoch() (uint64, error) {
	b, err := os.ReadFile(m.epochPath())
	switch {
	case err == nil:
		v, perr := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		if perr != nil {
			// FAIL CLOSED. Silently treating an unreadable counter as 0 restarts the
			// fence at 1, and a straggler still holding epoch 1 then matches the new
			// lease, passes Check(), and its release deletes the live holder's claim.
			return 0, fmt.Errorf("gpulease: fencing counter at %s is unreadable (%q); "+
				"refusing to restart the fence from zero", m.epochPath(), strings.TrimSpace(string(b)))
		}
		return v, nil
	case os.IsNotExist(err):
		return 0, nil // first ever acquisition on this box
	default:
		return 0, fmt.Errorf("gpulease: reading fencing counter: %w", err)
	}
}

// writeEpoch publishes a counter value atomically. Callers must hold the epoch lock.
func (m *Manager) writeEpoch(v uint64) error {
	tmp := m.epochPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatUint(v, 10)), 0o666); err != nil {
		return fmt.Errorf("gpulease: writing epoch: %w", err)
	}
	if err := os.Rename(tmp, m.epochPath()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("gpulease: publishing epoch: %w", err)
	}
	return nil
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
// reclaim rule requires BOTH a stale heartbeat and an expired window, so missing a few
// renewals inside the declared window is harmless by design.
//
// THE HEARTBEAT LIVES IN ITS OWN PER-EPOCH FILE, not in the claim record. Renewing by
// read-modify-writing meta.json is a read-then-write race: if a reclaim and a fresh
// acquisition land between the read and the write, the OLD holder stamps its stale
// record over the LIVE one, and the new holder's next Check() then fails while the
// stale holder carries on — the same shape as the token race that let two parties hold
// the card. Keying the heartbeat by epoch makes a stale writer harmless: it updates
// hb.<its own epoch>, which nothing reads any more.
//
// This is only affordable because the heartbeat is now Go-only. Node no longer
// implements staleness, so the mechanism can change without a cross-language contract.
func (l *Lease) Renew() error {
	if err := l.Check(); err != nil {
		return err
	}
	return os.WriteFile(l.mgr.heartbeatPath(l.epoch),
		[]byte(strconv.FormatInt(l.mgr.now().UnixMilli(), 10)), 0o666)
}

// heartbeatPath is the per-epoch heartbeat file inside the lease directory.
func (m *Manager) heartbeatPath(epoch uint64) string {
	return filepath.Join(m.leaseDir(), "hb."+strconv.FormatUint(epoch, 10))
}

// lastHeartbeat returns the most recent heartbeat for this epoch, falling back to the
// value stamped into the record at acquisition when nothing has renewed yet.
func (m *Manager) lastHeartbeat(meta *Meta) int64 {
	b, err := os.ReadFile(m.heartbeatPath(meta.Epoch))
	if err != nil {
		return meta.RenewedAtMs
	}
	v, perr := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if perr != nil || v < meta.RenewedAtMs {
		return meta.RenewedAtMs
	}
	return v
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
	// Drop the CLAIM only. Removing the container would delete a directory another
	// acquirer may be working inside.
	m.clearUnloadMarkers()
	if err := removeClaim(m.metaPath()); err != nil {
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
	l.mgr.clearUnloadMarkers()
	if err := removeClaim(l.mgr.metaPath()); err != nil {
		return fmt.Errorf("gpulease: releasing lease: %w", err)
	}
	return nil
}
