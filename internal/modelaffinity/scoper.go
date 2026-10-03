// scoper.go — the production wiring of legacy-lease inference (gpulease/infer.go).
//
// A lease that an OLDER binary wrote is a whole-node claim whose record names no cards. When
// the host has turned the inference on (config gpu_legacy_scope_inference, SetLegacyInference),
// a read of the lease carries the cards the evidence rule scoped it to, or the reason it
// stayed whole-node. Off, every legacy lease fences every seat, exactly as it always did.
//
// TWO KINDS OF READER, and only one of them writes:
//
//   - The OBSERVER (InspectLease): the load gate, the delegation door and the agent_run door
//     read the lease because a process wants a card. They remember what they see: the sticky
//     set (seen.<epoch>) and one ledger line when a scope is established or grows. They
//     unload nothing; a seat whose card the scope has spread onto yields on its own release
//     (seatyield.go).
//   - The INSPECTOR (PeekLease, ScopeInfo, ScopeFunc, ScopeLeases, CardsHeld, and the
//     delegator's own reads): gpu status, offload_status, the placement table and the
//     delegator's routing read the same evidence and report the same scope, and write
//     nothing at all: no sidecar, no ledger line, no call to llama-swap.
//
// Each Scoper is built once per lease directory and kind (its throttle memo has to outlive one
// read) over this package's armed state: the card table (memoised, ComfyUI order from config
// gpu_comfy_order), the ComfyUI install directory (config comfy_dir) for the launch marker,
// and the driver's per-process card listing for the sampled-cards evidence. Presence is not
// armed here: an unreadable presence is "present", so an inferred scope always keeps the
// display card (the conservative reading; a reader that can tell the operator is away
// installs gpulease.Scoper.Present).
package modelaffinity

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

type scoperKey struct {
	dir      string
	readOnly bool
}

var (
	scoperMu sync.Mutex
	scopers  = map[scoperKey]*gpulease.Scoper{}

	comfyDirMu sync.RWMutex
	comfyDir   string

	legacyInferenceMu sync.RWMutex
	legacyInference   bool

	// Seams a test swaps; nil is the production reader.
	scopeTree func(root int) ([]gpulease.TreeProc, error)
	scopeNow  func() time.Time
)

// legacyOffWhy is what a legacy lease reports while the inference is off.
const legacyOffWhy = "the record is a legacy whole-node claim (written by an older binary) and its cards are not inferred on this host: set gpu_legacy_scope_inference to scope it by what its process tree runs"

// SetLegacyInference arms config gpu_legacy_scope_inference: whether a legacy whole-node lease
// (one an older binary wrote) is scoped by the evidence rule at all. Off (the default), every
// legacy lease fences every seat, exactly as it always did.
func SetLegacyInference(on bool) {
	legacyInferenceMu.Lock()
	legacyInference = on
	legacyInferenceMu.Unlock()
}

func legacyInferenceOn() bool {
	legacyInferenceMu.RLock()
	defer legacyInferenceMu.RUnlock()
	return legacyInference
}

// LegacyInference reports whether the inference of legacy leases is on for this process.
func LegacyInference() bool { return legacyInferenceOn() }

// SetComfyDir arms config comfy_dir: where the ComfyUI launch marker is read from.
func SetComfyDir(dir string) {
	comfyDirMu.Lock()
	comfyDir = strings.TrimSpace(dir)
	comfyDirMu.Unlock()
}

func resetScopers() {
	scoperMu.Lock()
	scopers = map[scoperKey]*gpulease.Scoper{}
	scoperMu.Unlock()
}

// InspectLease is gpulease.InspectDir with the evidence rule applied to every legacy
// whole-node lease in it, as the OBSERVER: it remembers what it sees (the sticky set and the
// ledger line). Use it where a process reads the lease because it wants a card. A directory that
// holds nothing, or only leases that are not legacy, costs exactly what InspectDir costs.
func InspectLease(dir string) gpulease.Info {
	return scopeWith(dir, gpulease.InspectDir(dir), false)
}

// PeekLease is InspectLease as an INSPECTOR: the same scope, and nothing written.
func PeekLease(dir string) gpulease.Info {
	return scopeWith(dir, gpulease.InspectDir(dir), true)
}

// ScopeInfo applies the evidence rule to an inspection a caller already holds (the status
// surfaces read the lease through a Manager), as an INSPECTOR: every legacy whole-node lease in
// it gets its inferred scope, and nothing is written.
func ScopeInfo(dir string, info gpulease.Info) gpulease.Info {
	return scopeWith(dir, info, true)
}

// ScopeFunc is ScopeInfo bound to a lease directory, for a reader (the activity snapshot) that
// is handed the evidence rule without importing this package. A directory that cannot be
// resolved gives nil: the reader then sees only declared devices.
func ScopeFunc(gpuLockPath, stateDir string) func(gpulease.Info) gpulease.Info {
	dir, err := gpulease.LeaseDir(gpuLockPath, stateDir)
	if err != nil {
		return nil
	}
	return func(i gpulease.Info) gpulease.Info { return ScopeInfo(dir, i) }
}

// ScopeLeases is ScopeInfo for a list of leases, one Info per lease.
func ScopeLeases(dir string, leases []gpulease.Info) []gpulease.Info {
	out := make([]gpulease.Info, len(leases))
	for i, l := range leases {
		out[i] = ScopeInfo(dir, l)
	}
	return out
}

func scopeWith(dir string, info gpulease.Info, readOnly bool) gpulease.Info {
	if !info.Held || !hasLegacy(info) {
		return info
	}
	if !legacyInferenceOn() {
		return info.WithLegacyNote(legacyOffWhy)
	}
	return scoperFor(dir, readOnly).Scope(info)
}

func hasLegacy(info gpulease.Info) bool {
	for _, l := range info.Each() {
		if l.Held && l.Legacy && len(l.Devices) == 0 {
			return true
		}
	}
	return false
}

func scoperFor(dir string, readOnly bool) *gpulease.Scoper {
	scoperMu.Lock()
	defer scoperMu.Unlock()
	key := scoperKey{dir: dir, readOnly: readOnly}
	if sc, ok := scopers[key]; ok {
		return sc
	}
	sc := &gpulease.Scoper{
		Dir:      dir,
		ReadOnly: readOnly,
		Now: func() time.Time {
			if scopeNow != nil {
				return scopeNow()
			}
			return time.Now()
		},
		Cards: func() ([]gpuprobe.Card, error) {
			cards, ok := cardTable()
			if !ok {
				return nil, errors.New("the card table cannot be read")
			}
			return cards, nil
		},
		Tree: func(root int) ([]gpulease.TreeProc, error) {
			if scopeTree != nil {
				return scopeTree(root)
			}
			return gpulease.ProcessTree(root)
		},
		Marker: func() (gpulease.Marker, bool) {
			comfyDirMu.RLock()
			dir := comfyDir
			comfyDirMu.RUnlock()
			return gpulease.LaunchMarkerReader(dir)()
		},
		TreeCards: treeCards,
	}
	scopers[key] = sc
	return sc
}

// treeCards names the cards the driver says the given processes hold memory on. ok is false
// when it names no process at all (WDDM lists none for most processes) or cannot be read:
// that is no evidence, never "the tree uses no card".
func treeCards(pids []int) ([]string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), cardTableTimeout)
	defer cancel()
	procs, err := gpuactivity.SampleProcesses(ctx)
	if err != nil || len(procs) == 0 {
		return nil, false
	}
	want := map[int]bool{}
	for _, p := range pids {
		want[p] = true
	}
	var ids []string
	seen := map[string]bool{}
	for _, p := range procs {
		if !want[p.PID] {
			continue
		}
		id := strings.ToLower(strings.TrimSpace(p.GPUUUID))
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, len(ids) > 0
}
