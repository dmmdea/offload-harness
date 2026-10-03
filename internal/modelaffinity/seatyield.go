// seatyield.go — the seat race rule (plan P4, register C-86).
//
// THE RACE. A lease claims its cards FIRST and then unloads the seats resident on them
// (gpu reserve --unload-seat, the render lane's freeLlamaSwap). A text load that passed the
// gate a moment BEFORE the claim finishes loading AFTER that unload, so its model lands on
// cards the lease now holds, beside a render, and stays until its own idle ttl: the very
// residency switch the gate exists to prevent. The check-then-act window cannot be closed at
// the gate (the load is llama-swap's, one request later), so it is closed from the other side:
// once a model is resident, the seat re-reads the lease, and if one that this process does not
// hold now sits on the seat's cards, THE SEAT UNLOADS ITSELF. The seat always yields; the long
// job never does.
//
// THE INFERRED SCOPE OF A LEGACY LEASE reaches a seat the same way and no other. The scope is
// learned, not declared, so it can grow after a seat was admitted onto a card that looked free:
// a ComfyUI that started on card 2 and later spread to card 0. The next reading of the lease
// carries the wider scope, and the seat re-reads it when its own request completes (Release,
// or the delegation door's warm-up), finds the lease on its card, and unloads itself. NOTHING
// ELSE UNLOADS A SEAT FOR AN INFERENCE: a read of the lease (a status, a poll, a gate check)
// never touches llama-swap, because it is not the action of a process that wants the card
// (plan I5), and an inference is a guess about a foreign job. A seat that is resident and idle
// on a card the scope has just spread onto keeps it until its own idle ttl (five minutes), or
// its next request.
//
// WHAT NEVER YIELDS. The memory stack (register C-87: mem0 never yields to a lease); a model
// with requests in flight (the engine's own gauge: it is pulled only once idle, and the next
// release picks it up); a process running UNDER the lease (insideLease); a model on a card the
// lease does not touch. Unloading goes through the vendored llama-swap client, whose keep-set
// refuses a protected member by id and by alias.
package modelaffinity

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"llamaswap-pp-cli/pkg/llamaswap"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/seatload"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// yieldTimeout bounds each llama-swap call a yield makes. The yield runs after the caller's
// own request has completed, so it is kept short: a seat that cannot be unloaded in this time
// is reported and left for the next release.
const yieldTimeout = 8 * time.Second

var (
	yieldMu      sync.RWMutex
	yieldProtect []string

	yieldClient = &http.Client{Timeout: yieldTimeout}

	// The read and the write a yield makes against llama-swap; a test swaps them.
	yieldRead = func(ctx context.Context, endpoint, model string) (seatload.Reading, error) {
		return seatload.Inflight(ctx, yieldClient, endpoint, model)
	}
	yieldUnload = func(ctx context.Context, endpoint, model string) error {
		ls, err := swapclient.New(endpoint, yieldTimeout)
		if err != nil {
			return err
		}
		_, err = ls.Unload(ctx, model, &llamaswap.UnloadOpts{})
		return err
	}
)

// YieldSeams are the llama-swap read and the unload the seat-yield path makes. A nil member
// keeps the production one. A caller outside this package that needs to prove its wiring (a
// test of the cold-load warm-up) installs fakes; nothing in production does.
type YieldSeams struct {
	Read   func(ctx context.Context, endpoint, model string) (seatload.Reading, error)
	Unload func(ctx context.Context, endpoint, model string) error
}

// SetYieldSeams installs fakes for the yield path and returns the function that puts the
// previous ones back.
func SetYieldSeams(s YieldSeams) (restore func()) {
	yieldMu.Lock()
	pr, pu := yieldRead, yieldUnload
	if s.Read != nil {
		yieldRead = s.Read
	}
	if s.Unload != nil {
		yieldUnload = s.Unload
	}
	yieldMu.Unlock()
	return func() {
		yieldMu.Lock()
		yieldRead, yieldUnload = pr, pu
		yieldMu.Unlock()
	}
}

// SetYieldProtect arms the models that never yield (config memory_stack). config.Load arms it
// beside the lease directory.
func SetYieldProtect(protect []string) {
	yieldMu.Lock()
	yieldProtect = append([]string(nil), protect...)
	yieldMu.Unlock()
}

func yieldProtected() []string {
	yieldMu.RLock()
	defer yieldMu.RUnlock()
	return yieldProtect
}

func isProtected(model string, protect []string) bool {
	for _, p := range protect {
		if strings.EqualFold(strings.TrimSpace(p), strings.TrimSpace(model)) {
			return true
		}
	}
	return false
}

// YieldIfFenced is the seat race rule for one seat: when a lease this process does not hold
// sits on named cards (declared or inferred, never a whole-node lease) that model is pinned
// to, and fences loads (a media lease, or an exclusive text
// one), and the model is resident with nothing in flight, it unloads the model. yielded is
// true only when the unload succeeded; why says what happened either way when it matters (a
// yield, a yield that failed, a seat whose state could not be read and so stayed where it
// was). It never waits and never touches a lease.
func YieldIfFenced(ctx context.Context, endpoint, model string) (yielded bool, why string) {
	dir := gpuLeaseDir()
	if dir == "" || strings.TrimSpace(endpoint) == "" {
		return false, ""
	}
	if isProtected(model, yieldProtected()) {
		return false, ""
	}
	info := ScopeToModel(InspectLease(dir), model)
	var fence gpulease.Info
	for _, l := range info.Each() {
		if blocksLoadOne(l) {
			fence = l
			break
		}
	}
	// Only a lease that sits on CARDS moves a seat off them. A whole-node lease fences
	// what it always fenced and unloads nothing here: before card-scoped leases it never
	// did, and a host with them off must behave as before (0.161.0 release recheck).
	if !fence.Held || len(fence.EffectiveDevices()) == 0 {
		return false, ""
	}
	ctx, cancel := context.WithTimeout(ctx, yieldTimeout)
	defer cancel()
	rd, err := yieldRead(ctx, endpoint, model)
	if err != nil {
		// Folding an unreadable gauge into "not loaded" would hide a seat that stayed on a held
		// card. Said once per model per yieldWarnEvery, so a down engine is not a line per release.
		if yieldWarnDue(model) {
			return false, fmt.Sprintf("seat %s may be resident on cards held by %s: its state could not be read (%v); left resident", model, describeLease(fence), err)
		}
		return false, ""
	}
	if !rd.Loaded || rd.Starting || rd.Inflight > 0 {
		return false, ""
	}
	if err := yieldUnload(ctx, endpoint, model); err != nil {
		return false, fmt.Sprintf("seat %s is resident on cards held by %s but could not be unloaded: %v", model, describeLease(fence), err)
	}
	return true, fmt.Sprintf("seat %s unloaded itself: %s now holds its cards", model, describeLease(fence))
}

// yieldWarnEvery bounds how often one seat's unreadable gauge is reported.
const yieldWarnEvery = 5 * time.Minute

var (
	yieldWarnMu sync.Mutex
	yieldWarned = map[string]time.Time{}
)

// yieldWarnDue reports whether a warning about model is due now, and starts its quiet period.
func yieldWarnDue(model string) bool {
	yieldWarnMu.Lock()
	defer yieldWarnMu.Unlock()
	now := time.Now()
	if at, ok := yieldWarned[model]; ok && now.Sub(at) < yieldWarnEvery {
		return false
	}
	yieldWarned[model] = now
	return true
}
