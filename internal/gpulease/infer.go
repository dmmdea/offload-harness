package gpulease

// Legacy-lease inference (plan P4, register C-86).
//
// A lease written by a binary that predates card-scoped leases is a WHOLE-NODE claim: its
// record names no cards, so every consumer fences every seat for as long as it lives, and a
// hours-long render on one card halts text on the other two. The operator cannot be asked to
// end it, and the old wrapper cannot be taught anything. What can be done is to read what the
// job actually runs and scope the lease to that, on strong evidence only.
//
// WHICH RECORDS. Only a record that an older binary wrote (Info.Legacy: no Format stamp, no
// wrapper version, no devices). A whole-node record from a binary that could have named cards
// (an explicit --whole-node, a reserve with the writer flag off, the pipeline's own media
// lease) is the whole node by its writer's word, and no command line narrows it. The rule is
// also off unless the host turns it on (config gpu_legacy_scope_inference, armed in
// modelaffinity): the live capture of what a real legacy tree shows is still owed (plan P4
// step 1, milestone P6), so nothing narrows a lease on a guess before that.
//
// THE EVIDENCE RULE (in order; the first that names a card wins this evaluation):
//
//	(a) a command line: the lease's recorded wrapped command and the command line of every
//	    process in the wrapper's tree, read for ComfyUI's `--cuda-device N`. The environment
//	    of a foreign process is not readable, so COMFY_CUDA_DEVICE and CUDA_VISIBLE_DEVICES
//	    are never evidence here. N is in ComfyUI's FASTEST_FIRST order, which nvidia-smi does
//	    not report: it resolves only on a box that declared gpu_comfy_order (P3), and an
//	    index that cannot be placed is named, never guessed onto a card. ONE process naming a
//	    card that cannot be placed spoils the whole reading: the lease stays whole-node, because
//	    the card that process uses is unknown and every doubt fences.
//	(b) the ComfyUI launch marker, but only when it is TIED to this lease: its pid or its
//	    owner process is in the wrapper's tree, or it started at or after the lease did and no
//	    other live lease claims the card it names. A leftover marker proves nothing.
//	(c) the cards the tree's processes hold memory on, sampled at least five minutes apart,
//	    on a lease at least ten minutes old (its first minutes are model loads on cards the
//	    job will not keep). Under WDDM nvidia-smi often names no compute process at all, and
//	    an empty answer is no evidence, never "the tree uses no card".
//
// If none holds the lease stays whole-node and the reason says exactly what was missing.
//
// THREE RULES AROUND IT.
//
//   - Sticky, never shrinking. Once a card has been inferred it stays in the set (the sidecar
//     seen.<epoch> carries it across processes); a later reading that names fewer cards does
//     not free one. The sidecar is shared by every process that reads the lease, so it is
//     MERGED under the epoch lock (read, union, write), never overwritten from a snapshot
//     taken before the evidence was gathered. The set may WIDEN, and each establishment and
//     widening writes one line to the scope ledger (once, by the process that changed the set
//     on disk), so a seat that was admitted on a card the job later spread to can be explained
//     (and yields on its own release: modelaffinity's seat race rule).
//   - Never frees the display card on an inference while the operator may be at the desk (the
//     presence reading, unknown = present): WDDM shows the desktop's own use as noise, so the
//     card's silence is not evidence, and invariant I6 says it is never auto-assigned.
//   - Inference never rewrites the record. Info.Devices stays what the record declares, so the
//     card table and the allocator keep reading the lease as the whole node it is.
//   - A wrapper that cannot be trusted is not walked. A record that names no holder pid has no
//     tree (pid 0 is the system process: walking it would read every command line on the box),
//     and a root that started after the lease was taken is a recycled pid, not the wrapper.
//
// COST. Every consumer that gates a seat reads this, so the evidence is gathered at most once
// per scopeRecheck per lease per process (concurrent first readers share one gathering), the
// sidecar is written only when something changed (a scope established or widened, a sample
// taken), and a lease with declared devices, no lease at all, or a record that is not legacy
// costs nothing here.
//
// WHO WRITES. A Scoper with ReadOnly set (the status surfaces, the delegator's reads) gathers
// and reports and writes nothing. Only the load gate's own Scoper writes the sidecar and the
// ledger, and neither it nor anything here unloads a model.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

const (
	// scopeRecheck is how long one process trusts its last reading of a lease's evidence.
	scopeRecheck = time.Minute
	// sampleMinLease is the youngest lease the sampled-cards evidence will judge.
	sampleMinLease = 10 * time.Minute
	// sampleMinGap is how far apart two samples must be to count as evidence.
	sampleMinGap = 5 * time.Minute
	// sampleEvery is how often a new sample is recorded while the evidence is still wanted.
	sampleEvery = 2 * time.Minute
	// recycledSlack is how much later than the lease a wrapper's start time may read before
	// the pid is judged recycled (clock and unit rounding; the wrapper starts BEFORE it takes
	// the lease).
	recycledSlack = 5 * time.Second
	// scopeWarnEvery bounds how often one failure is repeated to the log.
	scopeWarnEvery = 10 * time.Minute
	// maxSamples bounds the sidecar: the first sample and the latest ones are kept.
	maxSamples = 12

	seenPrefix = "seen."
)

// TreeProc is one process of the wrapper's tree. Cmdline is "" when it cannot be read.
type TreeProc struct {
	PID     int
	PPID    int
	StartMs int64
	Cmdline string
}

// Marker is the ComfyUI launch marker (render/comfy-ownership.mjs, .offload-launch.json): the
// python pid, the node process that spawned it, when, and the exact argv it was given.
type Marker struct {
	PID         int
	OwnerPID    int
	StartedAtMs int64
	Args        []string
}

// ScopeEvent is one line of the scope ledger: a legacy lease's inferred scope established
// ("scoped") or grown ("widened").
type ScopeEvent struct {
	AtMs   int64    `json:"at_ms"`
	Epoch  uint64   `json:"epoch"`
	Kind   string   `json:"kind"`
	From   []string `json:"from,omitempty"`
	To     []string `json:"to"`
	Source string   `json:"source,omitempty"`
	Why    string   `json:"why,omitempty"`
}

// Scoper infers the effective card set of legacy whole-node leases. Every member but Dir is a
// seam; nil is the conservative default (no card table, no marker, no sample, present).
type Scoper struct {
	// Dir is the lease directory; seen.<epoch> lives in it and the scope ledger beside it.
	Dir string
	// Now is the clock (time.Now when nil).
	Now func() time.Time
	// Cards reads the card table, ComfyOrder included.
	Cards func() ([]gpuprobe.Card, error)
	// Tree returns the wrapper's process and its descendants (ProcessTree when nil).
	Tree func(root int) ([]TreeProc, error)
	// Marker reads the ComfyUI launch marker, if one is on disk.
	Marker func() (Marker, bool)
	// TreeCards returns the lease ids of the cards the processes hold memory on; ok is false
	// when the driver names no process (WDDM) or cannot be read.
	TreeCards func(pids []int) ([]string, bool)
	// Present reports whether the operator may be at the desk (nil = yes).
	Present func() bool
	// Audit receives each ledger line (a line in <gpu dir>/scope-ledger.jsonl when nil).
	Audit func(ScopeEvent)
	// ReadOnly makes the scoper an INSPECTOR: it gathers the evidence and reports the scope
	// it implies (the sidecar's sticky set included), and writes nothing: no sidecar, no
	// sample, no ledger line. The status surfaces read this way.
	ReadOnly bool
	// Live reports whether the lease with this epoch is still held; a sidecar is written only
	// for a live lease (InspectDir when nil).
	Live func(epoch uint64) bool
	// Warn receives one line for each failure the scope would otherwise hide (log.Printf
	// when nil), each at most once per scopeWarnEvery.
	Warn func(string)
	// StartUnixMs converts a TreeProc.StartMs, an opaque platform-specific process-start
	// identity, to Unix milliseconds; false when the platform cannot (the recycled-pid check
	// is then skipped). nil is the platform's own (startUnixMs).
	StartUnixMs func(startMs int64) (int64, bool)

	mu       sync.Mutex
	last     map[uint64]scopeMemo
	inflight map[uint64]*scopeFlight
	warned   map[string]time.Time
}

type scopeMemo struct {
	at  time.Time
	res scopeResult
}

// scopeFlight is one gathering in progress: readers that arrive meanwhile wait for its result.
type scopeFlight struct {
	done chan struct{}
	res  scopeResult
}

type scopeResult struct {
	devices []string // the sticky set, before the display card is added
	source  string
	why     string
	widened bool
}

func (r scopeResult) wholeNode(why string) scopeResult { return scopeResult{why: why} }

// seenRecord is the sidecar: the sticky inferred set, the samples behind evidence (c) and
// the bookkeeping the ledger needs.
type seenRecord struct {
	Epoch   uint64       `json:"epoch"`
	Devices []string     `json:"devices,omitempty"`
	Source  string       `json:"source,omitempty"`
	Widened bool         `json:"widened,omitempty"`
	FirstMs int64        `json:"first_at_ms,omitempty"`
	Samples []seenSample `json:"samples,omitempty"`
}

type seenSample struct {
	AtMs  int64    `json:"at_ms"`
	Cards []string `json:"cards"`
}

// InspectScoped is InspectDir with the evidence rule applied to every legacy whole-node
// lease in it: the one inspection a consumer that gates a SEAT should make. With a nil
// scoper it is InspectDir.
func InspectScoped(dir string, s *Scoper) Info {
	info := InspectDir(dir)
	if s == nil {
		return info
	}
	return s.Scope(info)
}

// Scope returns info with the scope of each legacy lease in it filled in. A lease that
// declares devices, and an Info that holds nothing, are returned as they are.
func (s *Scoper) Scope(info Info) Info {
	if !info.Held {
		return info
	}
	if len(info.Leases) == 0 {
		return s.scopeOne(info, nil)
	}
	scoped := make([]Info, len(info.Leases))
	for i, l := range info.Leases {
		var others []Info
		for j, o := range info.Leases {
			if j != i {
				others = append(others, o)
			}
		}
		scoped[i] = s.scopeOne(l, others)
	}
	out := scoped[0]
	out.Epochs = info.Epochs
	out.Leases = scoped
	return out
}

func (s *Scoper) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scoper) present() bool { return s.Present == nil || s.Present() }

func (s *Scoper) scopeOne(l Info, others []Info) Info {
	if len(l.Devices) > 0 {
		l.Scope = ScopeDeclared
		return l
	}
	l.Scope = ScopeWholeNode
	if !l.Legacy {
		// Written by a binary that could have named cards: the whole node is its writer's word.
		l.ScopeWhy = "the record was written by a binary that can name cards and says the whole node (its writer's word); only a record an older binary wrote is scoped by evidence"
		return l
	}
	res := s.legacyScope(l, others)
	l.ScopeWhy = res.why
	if len(res.devices) == 0 {
		return l
	}
	devs := res.devices
	if s.present() {
		// The display card is never reported free by an inference while the operator may be
		// at the desk (plan I6): its silence is the desktop's noise, not the job's absence.
		padded := unionIDs(devs, s.displayIDs())
		if len(padded) > len(devs) {
			l.ScopeWhy += "; the display card is included while the operator may be at the desk"
		}
		devs = padded
	}
	l.Inferred, l.Scope, l.ScopeWidened = devs, ScopeInferred, res.widened
	return l
}

func (s *Scoper) displayIDs() []string {
	if s.Cards == nil {
		return nil
	}
	cards, err := s.Cards()
	if err != nil {
		return nil
	}
	var out []string
	for _, c := range cards {
		if c.Display {
			out = append(out, c.LeaseID())
		}
	}
	return out
}

// legacyScope reads (at most once per scopeRecheck) the evidence for one legacy lease and
// folds it into the sticky set. Readers that arrive while a gathering is under way share its
// result instead of each walking the process table.
func (s *Scoper) legacyScope(l Info, others []Info) scopeResult {
	now := s.now()
	s.mu.Lock()
	if m, ok := s.last[l.Epoch]; ok && now.Sub(m.at) < scopeRecheck {
		s.mu.Unlock()
		return m.res
	}
	if f, ok := s.inflight[l.Epoch]; ok {
		s.mu.Unlock()
		<-f.done
		return f.res
	}
	if s.inflight == nil {
		s.inflight = map[uint64]*scopeFlight{}
	}
	f := &scopeFlight{done: make(chan struct{})}
	s.inflight[l.Epoch] = f
	prior := s.last[l.Epoch].res
	s.mu.Unlock()

	var res scopeResult
	defer func() {
		s.mu.Lock()
		f.res = res
		delete(s.inflight, l.Epoch)
		close(f.done)
		s.mu.Unlock()
	}()
	res = s.gather(l, others, now, prior)

	s.mu.Lock()
	if s.last == nil {
		s.last = map[uint64]scopeMemo{}
	}
	s.last[l.Epoch] = scopeMemo{at: now, res: res}
	s.mu.Unlock()
	return res
}

func (s *Scoper) seenPath(epoch uint64) string {
	return filepath.Join(s.Dir, fmt.Sprintf("%s%d", seenPrefix, epoch))
}

func (s *Scoper) readSeen(epoch uint64) seenRecord {
	var rec seenRecord
	b, err := os.ReadFile(s.seenPath(epoch))
	if err != nil || json.Unmarshal(b, &rec) != nil || rec.Epoch != epoch {
		return seenRecord{Epoch: epoch}
	}
	return rec
}

// persistent reports whether this scoper may write the sidecar and the ledger.
func (s *Scoper) persistent() bool { return !s.ReadOnly && s.Dir != "" }

// live reports whether the lease with this epoch is still held.
func (s *Scoper) live(epoch uint64) bool {
	if s.Live != nil {
		return s.Live(epoch)
	}
	return InspectDir(s.Dir).HoldsEpoch(epoch)
}

func (s *Scoper) warnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	now := s.now()
	s.mu.Lock()
	if s.warned == nil {
		s.warned = map[string]time.Time{}
	}
	if at, ok := s.warned[msg]; ok && now.Sub(at) < scopeWarnEvery {
		s.mu.Unlock()
		return
	}
	s.warned[msg] = now
	s.mu.Unlock()
	if s.Warn != nil {
		s.Warn(msg)
		return
	}
	log.Printf("gpulease: %s", msg)
}

// gather runs the evidence rule and folds the result into the sidecar. prior is the set this
// process reported last time, so a reading that names fewer cards never shrinks it even when
// the sidecar cannot be written.
func (s *Scoper) gather(l Info, others []Info, now time.Time, prior scopeResult) scopeResult {
	if l.PID <= 0 {
		// No holder pid, no wrapper, no tree: pid 0 is the system process, and walking it
		// would make every command line on the box evidence.
		return scopeResult{why: "stays whole-node: the record names no holder pid, so there is no wrapper process tree to read"}
	}
	rec := s.readSeen(l.Epoch)
	leaseStart := now.Add(-l.Age)

	// The card table is an nvidia-smi exec: read it only when evidence needs a card placed.
	var cards []gpuprobe.Card
	var cardsErr error
	cardsRead := false
	cardTable := func() ([]gpuprobe.Card, error) {
		if !cardsRead {
			cardsRead = true
			if s.Cards == nil {
				cardsErr = errors.New("no card table is available")
			} else {
				cards, cardsErr = s.Cards()
			}
		}
		return cards, cardsErr
	}

	var reasons []string
	tree, treeWhy, recycled := s.processTree(l.PID, leaseStart)
	if treeWhy != "" {
		reasons = append(reasons, treeWhy)
	}
	pids := map[int]bool{}
	var pidList []int
	if !recycled {
		// The recorded wrapper pid is the wrapper unless it was recycled.
		pids[l.PID] = true
		pidList = append(pidList, l.PID)
	}
	for _, p := range tree {
		if !pids[p.PID] {
			pids[p.PID] = true
			pidList = append(pidList, p.PID)
		}
	}

	var cur []string
	source := ""

	// (a) a command line.
	lines := []string{l.Command}
	for _, p := range tree {
		lines = append(lines, p.Cmdline)
	}
	aIDs, aWhy, spoiled := idsFromCommandLines(lines, cardTable, treeWhy == "")
	if len(aIDs) > 0 {
		cur, source = aIDs, "command line"
	} else {
		reasons = append(reasons, aWhy)
	}

	// (b) the launch marker, when tied to this lease. A command line that named a card it
	// could not place stops the rule here: the doubt fences, whatever else could be read.
	if len(cur) == 0 && !spoiled {
		ids, why := s.markerEvidence(others, pids, leaseStart, cardTable)
		if len(ids) > 0 {
			cur, source = ids, "launch marker ("+why+")"
		} else {
			reasons = append(reasons, why)
		}
	}

	// (c) the cards the tree holds memory on, sampled apart.
	if len(cur) == 0 && !spoiled {
		var ids []string
		var why string
		ids, why, _ = s.sampleEvidence(&rec, l, pidList, now)
		if len(ids) > 0 {
			cur, source = ids, "sampled tree cards"
		} else {
			reasons = append(reasons, why)
		}
	}

	merged := mergeSeen(rec, rec, cur, source, now)
	caveat := ""
	if s.persistent() {
		out, perr := s.commit(l.Epoch, rec, cur, source, now)
		switch {
		case perr != nil:
			caveat = fmt.Sprintf(" (note: %s could not be updated: %v; this scope is not sticky across processes)", seenPrefix+fmt.Sprint(l.Epoch), perr)
			s.warnf("%s%d could not be updated, so the inferred scope of lease %d is not sticky across processes: %v", seenPrefix, l.Epoch, l.Epoch, perr)
		default:
			merged = out
		}
	}
	devices := unionIDs(prior.devices, merged.Devices)
	res := scopeResult{devices: devices, source: merged.Source, widened: merged.Widened || (len(merged.Devices) > 0 && len(devices) > len(merged.Devices))}
	if res.source == "" {
		res.source = prior.source
	}
	if len(prior.devices) > 0 && len(merged.Devices) == 0 {
		res.widened = prior.widened
	}
	if len(devices) > 0 {
		res.why = fmt.Sprintf("inferred from %s; the record itself is a whole-node claim", res.source)
		if res.widened {
			res.why += " (scope-widened since it was first established)"
		}
		res.why += caveat
	} else {
		res.why = "stays whole-node: " + strings.Join(reasons, "; ")
	}
	return res
}

// processTree reads the wrapper's process tree. why is empty when it was read and believed;
// otherwise it says what stopped the tree from being evidence (the process table cannot be
// read; the root started after the lease, so its pid was recycled).
func (s *Scoper) processTree(root int, leaseStart time.Time) (procs []TreeProc, why string, recycled bool) {
	fn := s.Tree
	if fn == nil {
		fn = ProcessTree
	}
	procs, err := fn(root)
	if err != nil {
		return nil, fmt.Sprintf("the process table cannot be read (%v)", err), false
	}
	for _, p := range procs {
		if p.PID != root || p.StartMs == 0 {
			continue
		}
		conv := s.StartUnixMs
		if conv == nil {
			conv = startUnixMs
		}
		if abs, ok := conv(p.StartMs); ok && abs > leaseStart.Add(recycledSlack).UnixMilli() {
			return nil, fmt.Sprintf("pid %d started after the lease was taken, so it is not the wrapper (a recycled pid) and its tree is not read", root), true
		}
		break
	}
	return procs, "", false
}

// idsFromCommandLines reads ComfyUI's `--cuda-device` out of every line and resolves it
// against the card table. why says what was missing when no card came out. A line that names a
// card that cannot be placed spoils the reading even when another line resolves: the card that
// process uses is unknown, and every doubt fences (spoiled: no other evidence may narrow the
// lease either). treeRead says whether the wrapper's tree contributed lines (for the reason's
// wording only).
func idsFromCommandLines(lines []string, cardTable func() ([]gpuprobe.Card, error), treeRead bool) (ids []string, why string, spoiled bool) {
	noEnv := func(string) string { return "" }
	named := false
	var problems []string
	for _, line := range lines {
		args := splitCommandLine(line)
		if len(args) == 0 || !WouldDerive(args, noEnv) {
			continue
		}
		named = true
		cards, cardsErr := cardTable()
		if cardsErr != nil || len(cards) == 0 {
			problems = append(problems, "the card table cannot be read, so --cuda-device cannot be placed on a card")
			break
		}
		d, err := DevicesFromCommand(args, noEnv, cards)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		ids = unionIDs(ids, d.IDs)
	}
	if named && len(problems) > 0 {
		return nil, "a command line names a card it cannot place (" + strings.Join(dedupeIDs(problems), "; ") + ")", true
	}
	if len(ids) > 0 {
		return ids, "", false
	}
	if !treeRead {
		return nil, "the recorded command names no card (--cuda-device), and the wrapper's tree was not read", false
	}
	return nil, "no command line in the wrapper's tree names a card (--cuda-device)", false
}

// markerEvidence applies the tie rule to the launch marker.
func (s *Scoper) markerEvidence(others []Info, pids map[int]bool, leaseStart time.Time, cardTable func() ([]gpuprobe.Card, error)) (ids []string, why string) {
	if s.Marker == nil {
		return nil, "no ComfyUI launch marker is tied to this lease"
	}
	mk, ok := s.Marker()
	if !ok {
		return nil, "no ComfyUI launch marker is tied to this lease"
	}
	var d Derived
	var derr error
	cards, cardsErr := cardTable()
	if cardsErr != nil || len(cards) == 0 {
		derr = fmt.Errorf("the card table cannot be read")
	} else {
		d, derr = DevicesFromCommand(mk.Args, func(string) string { return "" }, cards)
	}
	tie := ""
	switch {
	case mk.PID > 0 && pids[mk.PID]:
		tie = "its pid is in the wrapper's tree"
	case mk.OwnerPID > 0 && pids[mk.OwnerPID]:
		tie = "the process that launched it is in the wrapper's tree"
	case mk.StartedAtMs > 0 && !time.UnixMilli(mk.StartedAtMs).Before(leaseStart):
		if derr == nil && len(d.IDs) > 0 && claimedByOther(others, d.IDs) {
			return nil, "the ComfyUI launch marker is not tied to this lease (another live lease claims its card)"
		}
		tie = "it started after the lease began and no other lease claims its card"
	default:
		return nil, "the ComfyUI launch marker is not tied to this lease (a leftover from before it, owned by no process in its tree)"
	}
	if derr != nil {
		return nil, "the ComfyUI launch marker is tied to this lease (" + tie + ") but its card cannot be placed: " + derr.Error()
	}
	if len(d.IDs) == 0 {
		return nil, "the ComfyUI launch marker is tied to this lease but names no card"
	}
	return d.IDs, tie
}

func claimedByOther(others []Info, ids []string) bool {
	for _, o := range others {
		if eff := o.EffectiveDevices(); len(eff) > 0 && devicesIntersect(eff, ids) {
			return true
		}
	}
	return false
}

// sampleEvidence records a sample of the tree's cards when one is due and judges the stored
// samples. took reports whether the record changed.
func (s *Scoper) sampleEvidence(rec *seenRecord, l Info, pids []int, now time.Time) (ids []string, why string, took bool) {
	const noSampler = "sampled tree cards: the driver names no process on any card (typical of WDDM)"
	if l.Age < sampleMinLease {
		return nil, fmt.Sprintf("sampled tree cards: not judged until the lease is %s old", sampleMinLease), false
	}
	if s.TreeCards != nil {
		var latest int64
		if n := len(rec.Samples); n > 0 {
			latest = rec.Samples[n-1].AtMs
		}
		if latest == 0 || now.Sub(time.UnixMilli(latest)) >= sampleEvery {
			if cards, ok := s.TreeCards(pids); ok && len(cards) > 0 {
				rec.Samples = append(rec.Samples, seenSample{AtMs: now.UnixMilli(), Cards: lowerIDs(cards)})
				if len(rec.Samples) > maxSamples {
					rec.Samples = append(rec.Samples[:1:1], rec.Samples[len(rec.Samples)-(maxSamples-1):]...)
				}
				took = true
			}
		}
	}
	if len(rec.Samples) == 0 {
		return nil, noSampler, took
	}
	first, last := rec.Samples[0].AtMs, rec.Samples[len(rec.Samples)-1].AtMs
	if time.Duration(last-first)*time.Millisecond < sampleMinGap {
		return nil, fmt.Sprintf("sampled tree cards: %d sample(s) so far, need two at least %s apart", len(rec.Samples), sampleMinGap), took
	}
	for _, sm := range rec.Samples {
		ids = unionIDs(ids, sm.Cards)
	}
	return ids, "", took
}

// mergeSeen folds one reading into the sidecar as it is on disk: the union of the sticky sets,
// of the samples, and of what the two knew of how the set was established. It never returns a
// narrower set than disk had.
func mergeSeen(disk, mine seenRecord, cur []string, source string, now time.Time) seenRecord {
	out := seenRecord{Epoch: disk.Epoch}
	out.Devices = unionIDs(disk.Devices, mine.Devices, cur)
	switch {
	case len(disk.Devices) > 0:
		out.Source = disk.Source
	default:
		out.Source = source
	}
	out.Widened = disk.Widened || (len(disk.Devices) > 0 && len(out.Devices) > len(disk.Devices))
	out.FirstMs = disk.FirstMs
	if out.FirstMs == 0 && len(out.Devices) > 0 && len(disk.Devices) == 0 {
		out.FirstMs = now.UnixMilli()
	}
	out.Samples = mergeSamples(disk.Samples, mine.Samples)
	return out
}

// mergeSamples is the union of two sample lists by time, oldest first, bounded: the first
// sample and the latest ones are kept.
func mergeSamples(a, b []seenSample) []seenSample {
	byAt := map[int64]seenSample{}
	for _, list := range [][]seenSample{a, b} {
		for _, sm := range list {
			if prev, ok := byAt[sm.AtMs]; ok {
				sm.Cards = unionIDs(prev.Cards, sm.Cards)
			}
			byAt[sm.AtMs] = sm
		}
	}
	if len(byAt) == 0 {
		return nil
	}
	out := make([]seenSample, 0, len(byAt))
	for _, sm := range byAt {
		out = append(out, sm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AtMs < out[j].AtMs })
	if len(out) > maxSamples {
		out = append(out[:1:1], out[len(out)-(maxSamples-1):]...)
	}
	return out
}

// sameSeen reports whether two sidecars hold the same thing.
func sameSeen(a, b seenRecord) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// commit merges this reading into the sidecar under the epoch lock and returns what is on disk
// afterwards. The lock is what makes "sticky, never shrinking" true across processes: a reader
// that gathered its evidence slowly, from a snapshot that is stale by now, adds to the set that
// is there and never replaces it. The ledger line is written by the process that changed the
// set on disk, once, and BEFORE the sidecar is renamed into place, so an interrupted write
// repeats a line on the next reading rather than losing it. A lease that has ended is not
// written for: its sidecar would outlive the release that removes it.
func (s *Scoper) commit(epoch uint64, mine seenRecord, cur []string, source string, now time.Time) (seenRecord, error) {
	mgr := &Manager{leaseOverride: s.Dir}
	var out seenRecord
	err := mgr.withEpochLock(func() error {
		disk := s.readSeen(epoch)
		out = mergeSeen(disk, mine, cur, source, now)
		if !s.live(epoch) {
			return nil // the lease ended while the evidence was being read: nothing to persist
		}
		if sameSeen(disk, out) {
			return nil
		}
		b, err := json.Marshal(out)
		if err != nil {
			return err
		}
		path := s.seenPath(epoch)
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, b, 0o666); err != nil {
			return err
		}
		if ev := scopeEventFor(disk, out, epoch, now); ev != nil {
			ev.Source = out.Source
			if source != "" {
				ev.Source = source
			}
			ev.Why = fmt.Sprintf("inferred from %s; the record itself is a whole-node claim", ev.Source)
			s.audit(*ev)
		}
		if err := renameReplacing(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		return nil
	})
	if err != nil {
		return seenRecord{}, err
	}
	return out, nil
}

// scopeEventFor is the ledger line a change of the sticky set earns: "scoped" when a set is
// established, "widened" when it grows, nothing when only samples changed.
func scopeEventFor(disk, now seenRecord, epoch uint64, at time.Time) *ScopeEvent {
	switch {
	case len(disk.Devices) == 0 && len(now.Devices) > 0:
		return &ScopeEvent{AtMs: at.UnixMilli(), Epoch: epoch, Kind: "scoped", To: now.Devices}
	case len(now.Devices) > len(disk.Devices):
		return &ScopeEvent{AtMs: at.UnixMilli(), Epoch: epoch, Kind: "widened", From: disk.Devices, To: now.Devices}
	}
	return nil
}

func (s *Scoper) audit(e ScopeEvent) {
	if s.Audit != nil {
		s.Audit(e)
		return
	}
	if err := AppendScopeLedger(s.Dir, e); err != nil {
		s.warnf("the scope ledger line for lease %d could not be written: %v", e.Epoch, err)
	}
}

// AppendScopeLedger appends one line to the scope ledger, <gpu dir>/scope-ledger.jsonl (the
// directory above the lease directory, so no reader of the lease directory ever meets it). A
// ledger that cannot be written never fails a read, but the caller is told.
func AppendScopeLedger(leaseDir string, e ScopeEvent) error {
	if leaseDir == "" {
		return errors.New("no lease directory")
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(leaseDir), "scope-ledger.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// LaunchMarkerReader returns a Marker reader over a ComfyUI install directory's
// .offload-launch.json (the file render/comfy-ownership.mjs writes). A missing or unparseable
// file is no marker.
func LaunchMarkerReader(comfyDir string) func() (Marker, bool) {
	return func() (Marker, bool) {
		if strings.TrimSpace(comfyDir) == "" {
			return Marker{}, false
		}
		b, err := os.ReadFile(filepath.Join(comfyDir, ".offload-launch.json"))
		if err != nil {
			return Marker{}, false
		}
		var raw struct {
			StartedAt int64    `json:"startedAt"`
			PID       int      `json:"pid"`
			OwnerPID  int      `json:"ownerPid"`
			Args      []string `json:"args"`
		}
		if json.Unmarshal(b, &raw) != nil || raw.PID == 0 {
			return Marker{}, false
		}
		return Marker{PID: raw.PID, OwnerPID: raw.OwnerPID, StartedAtMs: raw.StartedAt, Args: raw.Args}, true
	}
}

// splitCommandLine splits a command line into arguments, honouring double and single quotes.
// It is a reader of a recorded line, not a shell: backslashes are literal (Windows paths).
func splitCommandLine(s string) []string {
	var out []string
	var cur strings.Builder
	in := rune(0)
	started := false
	flush := func() {
		if started {
			out = append(out, cur.String())
			cur.Reset()
			started = false
		}
	}
	for _, r := range s {
		switch {
		case in != 0:
			if r == in {
				in = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			in, started = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	flush()
	return out
}

func lowerIDs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.ToLower(strings.TrimSpace(s)))
	}
	return out
}

// unionIDs is the sorted, de-duplicated union of id lists.
func unionIDs(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, id := range l {
			if id = strings.ToLower(strings.TrimSpace(id)); id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sort.Strings(out)
	return out
}
