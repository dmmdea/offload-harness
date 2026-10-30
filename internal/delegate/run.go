// run.go is the shared delegation execution engine (Task 6, §S4): both
// delegator surfaces — the MCP agent_delegate tool and the `delegate` CLI
// verb — hand their prepared contracts to Run, which places each subtask
// (gate.go's quality-first Place), executes it locally in-process or remotely
// over the fleet wire, then applies DELEGATOR-SIDE acceptance before anything
// counts as a success. A schema-valid result that fails an acceptance check is
// flipped to failed-verification — the wrong-valid-schema hole this engine
// exists to close (roast delta 3): the remote node can prove shape, only the
// delegator can prove the content is the content it asked for.
//
// Job protocol (roast delta 14): the DELEGATOR mints every job id
// ("agd-" + crypto/rand hex) so a re-dispatch on transport doubt carries the
// SAME id and the node's store re-acks 202 idempotently — a lost ack can never
// buy a duplicate run. Polling stops hard at TimeoutSec + a grace window,
// because a delegator that polls forever pins a goroutine on a dead node.
//
// What the deadline PRODUCES depends on whether the node ever reported OWNING
// the job — a 200 whose state is accepted/running/done/error. One that did gets
// an honest "poll deadline …" defer; anything else gets a FAILURE. Reachability
// is NOT ownership: a 404 is a positive denial that the job was ever there, and
// a 5xx says nothing about what the node holds. The delegator may report what a
// node said about its own work; it may never author a report on the behalf of a
// node that never claimed the work.

package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	// mathrand is the JITTER source only. crypto/rand above owns anything an
	// operator or another process could observe (job ids); a sleep length is
	// neither, and a lock-free generator is what a fan-out of dispatch
	// goroutines should be calling.
	//
	// semgrep's p/golang flags every math/rand import as a crypto finding. It is
	// refuted here rather than suppressed globally: the only consumer is
	// jittered(), whose output is a sleep duration nobody can observe and which
	// guards nothing. Seeding this from crypto/rand would buy no property and
	// would put a syscall on the dispatch path. If a SECOND consumer is ever
	// added, delete this line and make it argue its own case.
	mathrand "math/rand/v2" // nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used — timer jitter only, not a security use
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/netguard"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
	// Aliased: this file's `placement` struct (a resolved "run it HERE") predates
	// the package and is used at every placement site; the package is the
	// composite tier's decision TABLE (ADR 0052), so the alias names what it is.
	placetable "github.com/dmmdea/offload-harness/internal/placement"
)

const (
	// maxSubtasks bounds one Run (the MCP arg schema mirrors it).
	maxSubtasks = 8
	// MaxSubtasks is the same bound, exported for callers that size a call against RunWith's.
	MaxSubtasks = maxSubtasks
	// maxBatchSubtasks is how many subtasks RunBatched deals in ONE joint deal (ADR 0076): twice RunWith's
	// bound, because the one caller that batches (offload_research, at most 12 pages) used to be cut into
	// sequential chunks of eight, and a chunk waited on the slowest page of the one before it. A longer list
	// is still cut, into consecutive deals of this size.
	maxBatchSubtasks = 16
	// MaxBatchSubtasks is the same bound, exported for callers that batch (RunBatched).
	MaxBatchSubtasks = maxBatchSubtasks
	// runConcurrency is the FLOOR of a call's fan-out and the most one node that publishes no
	// max_concurrent_jobs is given of it. 4 overlaps remote polls, and it is all a call has when nothing sizes
	// it: a call that has a deal (route=auto, remote or spread) is as wide as the deal (dealParallelism, ADR
	// 0076). It once also stood for "small enough that a local fallback burst cannot stampede the one GPU";
	// that is the local run-cap line's job now (pipeline/agenttask.go, register C-42/C-60: runs past
	// fleet_max_concurrent_jobs wait their turn in the seat's own FIFO), and the deal counts that line.
	runConcurrency = 4
	// dispatchAttempts: the initial POST + one retry on transport doubt
	// (roast delta 14's 202-reack — same job id, so the store dedupes).
	dispatchAttempts = 2
	// maxRedispatches bounds 404-triggered re-dispatches during polling: a
	// node that keeps forgetting the job after two re-acks is broken, and
	// re-POSTing forever would re-run the contract on every node restart.
	maxRedispatches = 2
	// maxRemoteReplacements bounds how many ADDITIONAL REMOTE nodes one subtask
	// may be offered to after a node refused it at dispatch. It is spent from
	// ONE per-subtask ledger (see the placements struct), so the verification
	// retry draws from what the first attempt left rather than starting a fresh
	// copy of the bound.
	//
	// Local is NOT counted against it — the fallback to the one seat that
	// always exists is reserved, so a wide roster can never spend the bound
	// before reaching it.
	//
	// The resulting ceiling per subtask is FIVE placements, and the arithmetic
	// is worth spelling out because a shorter number was wrong once already:
	//
	//	1  the first placement
	//	2  at most maxRemoteReplacements re-placements onto further remotes
	//	1  at most one fall-back to the local seat (bounded by the ledger's
	//	   `tried` set, not by this constant)
	//	1  at most one verification-retry placement — a SEPARATE mechanism with
	//	   its own bound of exactly one, which is why it is not folded in here
	//
	// and every one of those five targets a dial base the subtask has not used
	// before, because they all consult the same `tried` set.
	//
	// Why 2, and why a bound at all:
	//
	//   - Walking the whole roster is its own failure mode. Each refused
	//     placement costs up to dispatchAttempts × dispatchRequestTimeout = 60s
	//     of dial time before a transport verdict, so an unbounded walk turns
	//     one saturated fleet into minutes of wall clock spent collecting
	//     refusals — and the contract's budget is being spent the whole time.
	//   - The first choice plus two alternates covers a three-node remote fleet
	//     completely. A refusal that survives three DISTINCT nodes is a
	//     fleet-wide condition (everything saturated, everything draining), not
	//     a node condition, and a fourth dial does not fix a fleet-wide one.
	//   - Local is the one seat that is always able to take the contract, so
	//     the bound is a bound on HUNTING, not on getting the work done.
	//
	// Two independent things also bound the loop tighter in practice: each node
	// is tried at most once (the base-URL exclusion set), and every placement
	// must fit in what is LEFT of the contract's timeout_sec.
	maxRemoteReplacements = 2
	// dispatchRequestTimeout / pollRequestTimeout bound ONE HTTP exchange;
	// the overall poll deadline is the contract's business, not the client's.
	dispatchRequestTimeout = 30 * time.Second
	pollRequestTimeout     = 15 * time.Second
	// maxFleetBody bounds a decoded dispatch/poll response (a wire result is
	// small; the cap only guards a misconfigured base).
	maxFleetBody = 4 << 20
)

// jitterFrac is how far a fixed sleep is randomised either way (+/-20 %).
//
// Every dispatcher in this fleet sleeps on the SAME constants — pollEvery,
// placementPollInterval, refusalCooldown — so K sessions that start within a
// second of each other stay in lockstep for the whole run: they re-read health
// together, re-dispatch together and re-ask a refusing node together, which is
// exactly the convoy that makes a busy node look busier than it is. Randomising
// each sleep by a fifth spreads them apart within a few ticks and costs nothing
// else: the cadence's MEAN is unchanged, and no caller can observe an individual
// sleep's length.
//
// 20 % is the usual choice for this (it is what exponential-backoff jitter
// recipes use) and is deliberately small: a bigger spread would start to change
// how quickly the capacity wait notices a node that freed.
const jitterFrac = 0.2

// jittered scales d by a uniform factor in [0.8, 1.2]. A non-positive d is
// returned untouched — a test that compressed a clock to zero means zero.
func jittered(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	j := time.Duration(float64(d) * (1 - jitterFrac + 2*jitterFrac*mathrand.Float64()))
	if j <= 0 {
		j = 1
	}
	return j
}

// jitteredWithin is jittered, clamped so the sleep can never run PAST the
// deadline it lives under. Jitter is a de-synchroniser, never a licence to
// overshoot a budget: `left` is what remains of that deadline, and a
// non-positive `left` means there is no deadline left to protect (the caller's
// own loop condition decides what happens next).
func jitteredWithin(d, left time.Duration) time.Duration {
	j := jittered(d)
	if left > 0 && j > left {
		return left
	}
	return j
}

// pollEvery/pollGrace are vars (not consts) so tests compress the cadence;
// production never mutates them. Grace rides ON TOP of the contract's
// TimeoutSec: the node reports TimeoutSec as its wall (the run's expectation,
// ADR 0055: what ends a run is a stall or the node's ceiling), so the delegator
// allows that plus transport slack before declaring the poll dead, and holds the
// poll open past it while the node reports progress.
var (
	pollEvery = 3 * time.Second
	pollGrace = 60 * time.Second
	// pollSecond is the unit a contract's wall (an integer number of SECONDS)
	// is converted to wall clock with. One real second in production; a test
	// compresses it because an auto contract's bound never falls below
	// AgentTimeoutSecDefault (300 s) and a deadline test cannot wait that out.
	// A var for that reason only — production never mutates it.
	pollSecond = time.Second
)

// maxQueuedWait is the ceiling on how long a subtask waits for a node that
// publishes no ETA to START it, and the delegator's own yardstick for a node's
// Retry-After (the node clamps its hint at 300 s and says ">=300 s").
//
// Since ADR 0063 it is no longer THE queue deadline. A node that publishes an ETA
// (queue_wait_estimate_sec, or a recent wall to derive one from) is given
// clamp(1.5 x that wait + 30 s, 60 s, the caller's patience) by queueBudgetFor,
// which is longer than five minutes for a node that honestly says it needs it and
// shorter for one that says a job starts now. A node that publishes nothing is no
// opinion and keeps the wait that always applied: min(patience, maxQueuedWait), so
// a short contract never waits longer for a slot than it was willing to spend on
// the work. The pull-queue lane (queue.go) has no health view to derive from and
// still uses it as its ceiling.
const maxQueuedWait = 5 * time.Minute

// fleetClient rides netguard.SafeTransport like the health client: the
// delegation lane may only ever reach loopback or the operator's tailnet
// (never-cloud, ADR 0001), enforced at every dial. No client-level Timeout —
// per-request ctx deadlines own the budget. Like the health client it never
// follows a redirect: a dispatch, a poll and a withdraw carry the fleet bearer
// (refuseRedirect).
var fleetClient = &http.Client{Transport: netguard.SafeTransport(nil), CheckRedirect: refuseRedirect}

// Run is RunWith without options — the signature every existing caller uses.
func Run(ctx context.Context, cfg config.Config, local LocalRunner, subtasks []core.AgentContract, route string, remotes []string) ([]PlacedResult, Summary, error) {
	return RunWith(ctx, cfg, local, subtasks, route, remotes, nil)
}

// RunBatched runs contracts in order as ONE joint deal of up to MaxBatchSubtasks
// (16), and a longer list as consecutive deals of that size (ADR 0076).
// offload_research builds ONE contract per usable page and allows 12 URLs per
// call, so a 9–12-page call hit Run's 8-subtask refusal and every page was
// lost (2026-09-01, three sessions); the fix cut it into chunks of eight and
// ran them SEQUENTIALLY, which left chunk two waiting on the slowest page of
// chunk one and a 12-page call using at most four of the fleet's slots. The
// whole list is now dealt at once, across every node, and sized by that deal
// (dealParallelism): the bound the sequential chunks kept, that a burst cannot
// stampede a GPU, is the node's published ceiling and the local seat's run-cap
// line, both of which the deal counts. Results come back in input order;
// Summary counters are summed; the first error is returned WITH the results
// collected before it, so a caller can render the partial work.
func RunBatched(ctx context.Context, cfg config.Config, local LocalRunner, subtasks []core.AgentContract, route string, remotes []string, opts *RunOptions) ([]PlacedResult, Summary, error) {
	var all []PlacedResult
	var total Summary
	// The call deadline (ADR 0065) covers the WHOLE batched call, not each chunk:
	// one shared state, so the unfinished count in every published reason spans
	// every chunk, and a chunk that would begin after the deadline never does.
	dl := newCallDeadline(ctx, opts, len(subtasks))
	if dl != nil {
		o := *opts
		o.call = dl
		opts = &o
	}
	for start := 0; start < len(subtasks); start += MaxBatchSubtasks {
		end := min(start+MaxBatchSubtasks, len(subtasks))
		chunkOpts := opts
		if opts != nil && opts.OnProgress != nil {
			// Progress counts against the whole call, not against each chunk.
			o := *opts
			o.OnProgress = shiftedProgress(opts.OnProgress, len(all), len(subtasks))
			chunkOpts = &o
		}
		res, sum, err := runWith(ctx, runParams{cfg: cfg, local: local, subtasks: subtasks[start:end], route: route, remotes: remotes, opts: chunkOpts, limit: MaxBatchSubtasks})
		all = append(all, res...)
		total = addSummary(total, sum)
		total.Batches++
		if err != nil {
			// The erroring chunk's own subtasks (no results came back) plus every
			// chunk after it were never attempted: count them, or the summary
			// reads as if the batch simply ended (silent-failure review, 2026-09-02).
			total.Skipped += (end - start - len(res)) + (len(subtasks) - end)
			return all, total, err
		}
	}
	if len(subtasks) == 0 {
		return runWith(ctx, runParams{cfg: cfg, local: local, subtasks: subtasks, route: route, remotes: remotes, opts: opts, limit: MaxBatchSubtasks}) // the "at least one subtask" error, unchanged
	}
	return all, total, nil
}

// addSummary sums every int counter of Summary by reflection, so a counter
// added later cannot be silently dropped from a batched total (the reflection
// test pins it). Non-int fields (none today) would need explicit handling.
func addSummary(a, b Summary) Summary {
	av := reflect.ValueOf(&a).Elem()
	bv := reflect.ValueOf(b)
	for i := 0; i < av.NumField(); i++ {
		if av.Field(i).Kind() == reflect.Int && av.Field(i).CanSet() {
			av.Field(i).SetInt(av.Field(i).Int() + bv.Field(i).Int())
		}
	}
	return a
}

// RunWith is Run with caller-owned options (see RunOptions). It refuses more than MaxSubtasks (8) subtasks, which
// is the bound of the MCP door (agent_delegate) and of the CLI verb; RunBatched is the entry that lifts it.
func RunWith(ctx context.Context, cfg config.Config, local LocalRunner, subtasks []core.AgentContract, route string, remotes []string, opts *RunOptions) ([]PlacedResult, Summary, error) {
	return runWith(ctx, runParams{cfg: cfg, local: local, subtasks: subtasks, route: route, remotes: remotes, opts: opts, limit: maxSubtasks})
}

// runParams is one call's arguments, threaded through the Run / RunWith / RunBatched ladder as a unit instead of
// being forwarded one by one. limit is the subtask bound: RunWith passes maxSubtasks and RunBatched maxBatchSubtasks.
type runParams struct {
	cfg      config.Config
	local    LocalRunner
	subtasks []core.AgentContract
	route    string
	remotes  []string
	opts     *RunOptions
	limit    int
}

// runWith is RunWith with the subtask bound in its parameters: RunWith passes maxSubtasks and RunBatched
// maxBatchSubtasks, so a research call's pages are one deal and the doors that cap a call at eight keep doing so
// (ADR 0076). Nothing else about a call depends on the bound.
func runWith(ctx context.Context, rp runParams) ([]PlacedResult, Summary, error) {
	cfg, local, subtasks, route, remotes, opts, limit := rp.cfg, rp.local, rp.subtasks, rp.route, rp.remotes, rp.opts, rp.limit
	switch route {
	case "":
		route = "auto"
	case "auto", "local", "remote", "spread", "queue":
	default:
		return nil, Summary{}, fmt.Errorf("delegate: route %q not recognized (want auto, spread, local, remote, or queue)", route)
	}
	// A pin_reason is checked against the route before anything is read or spent (ADR 0078), and a local or remote
	// route that has none, through a door that offers one, becomes a hint placed as auto (pin.go). From here `route`
	// is the route the run APPLIES; pin keeps what the caller asked.
	pin, route, perr := resolvePin(route, opts)
	if perr != nil {
		return nil, Summary{}, perr
	}
	// A browse grant is local work by construction (ADR 0060): the browse tool drives THIS machine's own browser, so a
	// call that carries one cannot have its local route overridden onto another node's. The grant is the reason: a bare
	// local that would have been a hint is pinned under the implied reason locality, and says so on its rows and results.
	if pin.hint && pin.asked == "local" && anyBrowse(subtasks) {
		pin.hint, pin.reason, route = false, PinLocality, "local"
	}
	if len(subtasks) == 0 {
		return nil, Summary{}, fmt.Errorf("delegate: at least one subtask required")
	}
	if err := checkSubtaskLimit(len(subtasks), limit); err != nil {
		return nil, Summary{}, err
	}
	// The whole-call deadline (ADR 0065): from here every placement, probe, poll
	// and local run answers to a context that ends at RunOptions.Deadline. nil =
	// no deadline, and everything below behaves exactly as it did before.
	dl := newCallDeadline(ctx, opts, len(subtasks))
	if dl != nil {
		var cancelCall context.CancelFunc
		ctx, cancelCall = context.WithDeadlineCause(ctx, dl.at, ErrCallDeadline)
		defer cancelCall()
	}
	// route "queue" bypasses the whole push machinery (ADR 0030): the holder
	// owns durability and the claim loops own placement.
	if route == "queue" {
		var rescue RescueFunc
		if opts != nil {
			rescue = opts.Rescue
		}
		results, sum, err := runQueued(ctx, cfg, subtasks, rescue)
		return dl.cutQueued(cfg, subtasks, results, sum, err)
	}
	// Fleet membership is configuration: a call that names no remotes uses the
	// config's delegate_remotes. A call's own list REPLACES it (never merges) so
	// one node can still be targeted deliberately.
	named := len(remotes) > 0
	if !named {
		remotes = cfg.DelegateRemotes
	}
	for _, base := range remotes {
		if err := netguard.TailnetURL(base); err != nil {
			return nil, Summary{}, fmt.Errorf("delegate: remote %q: %w", base, err)
		}
	}
	// A list a model named may only narrow the configured fleet (ADR 0038, amendment 2026-10-07): the
	// roster is what the operator vetted, and a caller-named URL outside it is never dialled.
	if named && opts != nil && opts.RosterOnly {
		// The list that proceeds is the roster's own spelling of it, so the process in-flight gate and every
		// per-base map after this keep one node under one key (CanonicalRosterRemotes).
		canonical, err := CanonicalRosterRemotes(cfg, remotes)
		if err != nil {
			return nil, Summary{}, fmt.Errorf("delegate: %w", err)
		}
		remotes = canonical
	}

	// Ledger row per delegation (roast delta 9). ledger.Open is the existing
	// RECORDLESS-COMPATIBLE append path: a standalone O_APPEND JSONL writer,
	// safe beside a live MCP server's handle — nothing here touches the
	// pipeline's cache/shadow/exemplar stores. Telemetry failure never blocks
	// delivery (same posture as pipeline.record's ignored error).
	var led *ledger.Ledger
	ledgerUnopened := false
	// ledgerDrained is set only when the call deadline passed with subtask
	// goroutines still running: they write their rows when they end, so the ledger
	// closes after the last of them instead of under them.
	var ledgerDrained <-chan struct{}
	if cfg.LedgerPath != "" {
		if l, err := ledger.Open(cfg.LedgerPath); err == nil {
			led = l
			defer func() {
				if ledgerDrained != nil {
					go func() { <-ledgerDrained; l.Close() }()
					return
				}
				l.Close()
			}()
		} else {
			// Every row this run would have written is LOST, and record() must
			// count them as such — see runner.ledgerUnopened.
			ledgerUnopened = true
			// Not fatal — but not silent either. Without this line a run that
			// records nothing looks exactly like a run that records everything,
			// and `local-offload stats` quietly under-reports forever.
			log.Printf("delegate: ledger %s could not be opened; this run records no ledger rows: %v", cfg.LedgerPath, err)
		}
	}

	// Option A durability (intent.go): open the intent ledger for this run and
	// fire the once-per-process orphan recovery in the background. Both are
	// additions — a nil ledger and a failed recovery change nothing about how
	// this run places or reports.
	maybeRecoverOrphans(cfg)
	r := &runner{cfg: cfg, local: local, route: route, pin: pin, remotes: remotes, intent: openIntentLedger(cfg), led: led, ledgerUnopened: ledgerUnopened, pair: pairworkloads.New(pairworkloads.FromConfig(cfg))}
	// Deliver every PAIR frame before returning: the delegate CLI exits right
	// after this, and a frame still in flight dies with the process — PAIR then
	// shows the card "running" until its staleness sweep fails it. Each send is
	// bounded (2 s), so this cannot hold a run hostage to a down PAIR.
	defer r.pair.Wait()
	// Registered after the Wait above, so it runs BEFORE it: from here on a late
	// frame from an abandoned goroutine is dropped, never added to a WaitGroup that
	// is being waited on.
	defer r.shutPair()
	r.call = dl
	if opts != nil {
		r.quarantine = opts.Quarantine
		r.tally = opts.PinTally
		r.priority = core.ClampBand(opts.Priority)
		r.tenant = opts.Tenant
		r.decider = opts.LocalDecider
		r.rescue = opts.Rescue
		r.remoteRetryOnly = opts.RemoteRetryOnly
	}
	// route=spread probes the fleet ONCE per run: every subtask deals itself
	// across the same roster, so per-subtask probing would be N identical GETs
	// and could even deal two subtasks against different snapshots.
	if route == "spread" {
		r.spreadViews, r.spreadBases, r.spreadProbeErrs = r.fetchViews(ctx)
		r.spreadLease = LocalLease(cfg.GPULockPath, cfg.StateDir) // read whole, once; spreadLeaseFor narrows it per contract
		// The local seat's load is read here, once, for the same reason the
		// fleet is probed once: every subtask must deal against ONE snapshot.
		if cfg.SpreadLocalSlot() == config.SpreadLocalAlways {
			r.spreadLocalBusy = busyReading{note: "agent_spread_local_slot=always"}
		} else {
			r.spreadLocalBusy = r.probeLocalBusy(ctx)
			// A live driver-fault repeat on the seat's cards (ADR 0096) deals the local slots away like a busy seat;
			// the capacity wait reads the seat afresh, so the seat is demoted, not excluded.
			if faulted, why := r.localFaulted(subtasks); faulted {
				r.spreadLocalBusy.busy = true
				r.spreadLocalBusy.note += "; " + why
			}
		}
		// One line per run says which rule dealt the local slot and from what
		// reading — the placement reasons only mention it when it fired.
		log.Printf("delegate: spread local slot: mode=%s busy=%v inflight=%d (%s)", cfg.SpreadLocalSlot(), r.spreadLocalBusy.busy, r.spreadLocalBusy.inflight, r.spreadLocalBusy.note)
		// The deal is computed HERE, once, over every subtask at once —
		// dealSpread's comment carries the proof that a per-subtask pick cannot
		// hold the one-per-seat-per-cycle invariant. It must run before the
		// goroutines below: they read it, they never build it.
		r.spreadDeal = r.dealSpread(subtasks, r.localView())
	}
	// route=auto/remote: ONE joint deal over ONE fleet snapshot (W-06,
	// register S-11/S-13) — the same reason route=spread already probes and
	// deals once. Before this, every subtask's attempt() probed the fleet and
	// called Place independently, so runConcurrency siblings could read the
	// same free slot within milliseconds of each other and pile onto it.
	if route == "auto" || route == "remote" {
		leaseInfo := LocalLease(cfg.GPULockPath, cfg.StateDir)
		// W-01 (register S-01): busy = the lease, OR the local seat's own
		// in-flight count at or past the fleet's own concurrency cap, OR a
		// load in progress — the SAME formula attempt() used per-subtask,
		// now read once so the whole batch agrees. route=remote forces busy
		// unconditionally: local is never a placement for an explicit remote.
		busy := route == "remote"
		// The local seat's run-cap room is read ONCE here, for the whole run (ADR 0076): the gate below needs it
		// before the deal, and the deal counts what it commits to the seat against it. route=remote never deals
		// the seat, so it reads none.
		room, roomNote := 0, ""
		if route == "auto" {
			busy = r.readAutoLocalSlot(ctx, leaseInfo, subtasks)
			room, roomNote = r.localRunCapRoom()
			// A remote hint (ADR 0078) is placed remotes-first, as route=remote is: the deal treats the seat as
			// unavailable whatever it read. The reading is still taken and kept, unlike route=remote's, because the
			// hint may fall back to the seat where remote would defer, and that fallback must still wait for a lease
			// or for the vLLM seat that holds the cards (autoDealBusy), never load over them. hintSeatFree is what
			// the seat read BEFORE the forcing: the deal may give the seat what no remote has room for only if it
			// is free, and a lease, a full line, a load in progress or an occupant each make it not free.
			if r.remoteHint() {
				r.hintSeatFree = !busy
				busy = true
			}
		}
		// The fleet is read when the local seat is busy; for an idle one, when some contract names a layer
		// this box does not declare (register A-108), because that contract is dealt to the node that declares
		// it; and when the call has more subtasks than the seat's run-cap line takes (ADR 0076), because the
		// overflow is dealt to the remotes with room instead of into a line it could not leave. Nothing else
		// about an idle box needs the roster. A box with no agent seat at all (a delegation client) reads it
		// for every contract, the same way.
		localView := r.localView()
		readFleet := busy || len(subtasks) > room
		for _, c := range subtasks {
			if !r.localServesLayer(localView, c) {
				readFleet = true
				break
			}
		}
		var failed map[string]string
		if readFleet {
			r.autoViews, r.autoBases, r.autoProbeErrs, failed = r.fetchViewsDetailed(ctx)
		}
		r.autoDeal = r.dealAutoRemoteRoom(subtasks, localView, r.autoViews, r.autoBases, busy, failed, room, roomNote)
	}
	board := newResultBoard(len(subtasks))
	progress := newProgressor(opts, len(subtasks))
	// The call is as wide as its deal (ADR 0076), not a constant: dealParallelism reads what the deal committed to
	// each node. A call with no deal to read (route=local) keeps runConcurrency.
	width := r.dealParallelism()
	if width != runConcurrency {
		log.Printf("delegate: fan-out width %d for %d subtask(s), sized from the %s deal (floor %d)", width, len(subtasks), route, runConcurrency)
	}
	sem := make(chan struct{}, width)
	var wg sync.WaitGroup
	launched := 0
	var stop launchStop
launch:
	for i, c := range subtasks {
		if dl == nil {
			sem <- struct{}{} // no deadline: block for a slot exactly as always
		} else {
			// A subtask still waiting for a run slot when the call ends never begins.
			if dl.reached() {
				break launch
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				break launch
			}
			// Nor does one that would begin inside the reserve (ADR 0073). The slot may have taken
			// minutes to come, and a job started with no more than a placed job needs left could only
			// be cut by the deadline and then run on in its node with nobody waiting for it.
			if left, noRoom := dl.noRoom(); noRoom {
				<-sem
				stop = launchStop{noRoom: true, left: left}
				break launch
			}
		}
		dl.begin()
		wg.Add(1)
		launched++
		// The subtask's place in the call's width (runSlot): the capacity wait may give it up while the
		// subtask waits for a node and takes one again to dispatch; whatever it holds is given back here.
		slot := newRunSlot(sem)
		go func(i int, contract core.AgentContract) {
			defer wg.Done()
			defer slot.finish()
			progress.started(i)
			// runOne's outcome was already cut where it was PRODUCED (finish, settle, exhaustedSettled): the
			// only moment "after the deadline?" can be answered. Asking again here, with no
			// timestamp, would rewrite an answer that finished before the deadline — an
			// abstention whose retry was cut, for one — into a call-deadline defer.
			pr := r.runCapped(withRunSlot(ctx, slot), i, contract)
			if board.put(i, pr) {
				if !pr.deadlineCut {
					dl.answer()
				}
				progress.finished(i, pr)
			}
		}(i, c)
	}
	drained, abandoned := dl.await(ctx, &wg)
	if abandoned && led != nil {
		// Goroutines are still running and will record their rows when they end:
		// the ledger stays open for them and closes once the last has returned.
		ledgerDrained = drained
	}
	results, done := board.close()
	for i := range results {
		switch {
		case done[i]:
		case i >= launched:
			results[i] = r.unlaunched(subtasks[i], stop)
			progress.finished(i, results[i])
		default:
			results[i] = r.abandoned(i, subtasks[i])
			progress.finished(i, results[i])
		}
	}
	// The call's pin (ADR 0078) goes on every result the caller receives, once, here: the reason a pinned call carries
	// and the plain-words clause a hinted call's placement reason opens with. record() puts the same on the ledger
	// row of each attempt, from the same function. The pin accounting offload_status publishes counts these
	// published results, so a retried or re-placed subtask is one and a hint is judged by where it was placed.
	for i := range results {
		results[i] = r.stampPin(results[i])
	}
	r.tallyPins(results)

	var sum Summary
	for _, pr := range results {
		if pr.retried {
			sum.Retried++
		}
		if pr.retryRecovered {
			sum.RetryRecovered++
		}
		if pr.waited {
			sum.Waited++
		}
		if pr.shed {
			sum.Shed++
		}
		if pr.Replacements > 0 {
			sum.Replaced++
			// "Recovered" is stated on the REFUSAL, not on the answer: Err == ""
			// means some node took the work and reported on it. Whether that
			// report was good is what the four buckets below are for. A capacity
			// defer or a shed (Unplaced) is the one Err == "" result no node
			// ever ran, and it is not a recovery.
			if pr.Err == "" && !pr.Unplaced {
				sum.ReplacementRecovered++
			}
		}
		// The bucket and the broken-stack ANNOTATION are decided separately: a
		// local placement can succeed while the fleet it declined to use is
		// down, and that must still count as infrastructure.
		//
		// lost is the STRICTER half — this subtask delivered no usable result,
		// the contracted output never arrived, and the stack is why — kept apart
		// from remotesUnreachable, which annotates a result that succeeded. Note
		// what "no usable result" does NOT mean: `output` may be populated here
		// (a finished loop whose re-pack seat was unreachable), and the count is
		// on the CONTRACTED deliverable — see Summary.LostToStack above. Both are
		// infrastructure; only one is lost work, and a consumer that must decide
		// "did anything get eaten?" (the loud signal of a partial result, and the
		// MCP error flag when nothing succeeded) cannot answer it from the merged
		// count.
		lost := pr.Result.Deferred && BrokenStackDefer(pr.Result.DeferClass)
		infra := pr.remotesUnreachable || lost
		// DO NOT add "every remote refused" to this. It is tempting after
		// 0.101.0 — a fleet that 503s every dispatch and leaves the local seat
		// carrying the run feels like it should be loud — and it is wrong twice.
		// Infrastructure means THE FLEET IS BROKEN: a node that answered its
		// health probe and then declined a job is working exactly as designed
		// (back-pressure), and a local placement that SUCCEEDED is not a broken
		// stack by any reading. remotesUnreachable is deliberately narrower: it
		// is set only when the remotes failed their HEALTH PROBE, which is a
		// different fact from refusing work. Widening this field would flag
		// finished, verified work as infrastructure failure on both surfaces
		// (non-zero CLI exit, IsError to the MCP caller) — the exact
		// flag-on-successful-work defect delegateIsError was rewritten to
		// remove. The audibility for a shedding fleet is Summary.Replaced, which
		// exists for it and says the true thing without redefining this one.
		switch {
		case pr.Err != "":
			sum.Failed++
		case pr.Result.Deferred:
			sum.Deferred++
		case len(pr.AcceptanceFailures) > 0:
			sum.FailedVerification++
		default:
			sum.Succeeded++
		}
		if infra {
			sum.Infrastructure++
		}
		if lost {
			sum.LostToStack++
		}
	}
	// The denominators are carried only WITH a loss — one decision point, here,
	// so the wire renderer stays a straight copy and a healthy run's Summary
	// keeps the zero-valued telemetry block every caller already compares against.
	sum.Quarantined = int(r.quarantined.Load())
	if sum.CorpusRowsLost = int(r.corpusLost.Load()); sum.CorpusRowsLost > 0 {
		sum.CorpusRowsAttempted = int(r.corpusTried.Load())
	}
	if sum.LedgerRowsLost = int(r.ledgerLost.Load()); sum.LedgerRowsLost > 0 {
		sum.LedgerRowsAttempted = int(r.ledgerTried.Load())
	}
	r.reportTelemetryLoss()
	return results, sum, nil
}

// BrokenStackDefer reports whether a defer_class means an OPERATOR, not a
// bigger model, has to act: the stack failed (infrastructure) or the
// node/config combination can never work as configured (config). An empty
// class — a pre-0.65 node, which cannot classify — is deliberately NOT broken:
// assuming the worst about an older peer would fail every mixed-version fleet.
//
// core.DeferClassContract is explicitly NOT a broken stack. It was split out of
// `config` for exactly this line: three of the placement gate's five conditions
// are properties of the CALLER'S CONTRACT (no output_schema — legal per
// Validate; a non-origin depth; a token estimate no advertised ceiling can
// hold), and lumping them in here made `--route remote` exit non-zero, on a run
// where every node was healthy, with a message telling the delegating model a
// node was broken or misconfigured. Nobody has to touch a box to fix those.
func BrokenStackDefer(class string) bool {
	return class == core.DeferClassInfrastructure || class == core.DeferClassConfig
}

// localBusyProbeTimeout bounds the one-shot read of the local seat's load: two
// loopback GETs (three with the roster). A slow or dead llama-swap deals as
// idle — the pre-0.113.20 deal — and logs why.
const localBusyProbeTimeout = 4 * time.Second

var localBusyClient = &http.Client{Timeout: localBusyProbeTimeout}

// attempt places (per route, or as forced by a retry) and executes one
// subtask, then verifies and records it. Every return path passes through
// finish() so no outcome can skip telemetry.
func (r *runner) attempt(ctx context.Context, i int, contract core.AgentContract, forced *placement) PlacedResult {
	start := time.Now()
	// Delegator-mints-the-id (roast delta 14): minted per attempt, BEFORE
	// placement, so even a local run correlates its telemetry line — and a
	// retry never reuses the id a node may still hold.
	jobID := mintJobID()
	r.lastJob.Store(i, jobID)

	finish := func(pr PlacedResult) PlacedResult {
		// A finished answer whose structured re-pack failed is re-packed here,
		// before the row is recorded, so the ledger and the corpus say what the
		// caller receives (rescue.go). The job is named first: the rescue's log line
		// has to say WHICH job's answer it tried to save.
		pr.JobID = jobID
		pr = r.rescueSchemaMiss(ctx, contract, pr, start)
		pr.wallMs = time.Since(start).Milliseconds()
		// An outcome that was not an answer and arrived once the call deadline had
		// passed is published — and recorded — as the call deadline (ADR 0065).
		pr = r.cutByDeadline(pr)
		// Intent ledger close-out (Option A, intent.go): an acked job whose
		// terminal answer THIS process observed is closed; the orphanable
		// exits (cancel / owned-deadline / queued give-up) stay open for the
		// recovery pass — that gap IS the durability feature.
		if pr.intentRecorded && !pr.orphanable {
			switch {
			case pr.withdrawn:
				r.intent.withdrawn(jobID) // the node confirmed it took the job back
			case pr.nodeNeverRan != "":
				r.intent.neverStarted(jobID, pr.nodeNeverRan) // the node's own record says it never ran
			default:
				r.intent.done(jobID, intentNoteTerminal)
			}
		}
		// A dispatch a node refused for capacity is re-placed or waited out by the engine, so its row is a refusal,
		// not an outcome. Whatever ends the subtask without a node taking it writes a terminal row of its own.
		pr.refusalRow = isRefusalRow(pr)
		r.record(contract, pr)
		r.pairTerminal(jobID, &pr)
		return pr
	}

	// A call whose deadline has passed starts nothing new (ADR 0065): no probe, no
	// dispatch, no local run. cutByDeadline (in finish) publishes it as the call
	// deadline.
	if r.call.reached() {
		return finish(PlacedResult{Err: "canceled: the call's deadline passed before this placement began"})
	}

	// Defensive re-validate: the surfaces run PrepareContract, but Run is an
	// exported engine — an invalid contract must die here, before any network.
	// At the BOX's cap (config.AgentContextCapBytes): a composite box admits a
	// contract sized for its long seats, a plain box keeps the 256 KiB cap.
	if err := contract.ValidateWithCap(r.cfg.AgentContextCapBytes()); err != nil {
		return finish(PlacedResult{Err: err.Error(), PlacementReason: "refused before placement"})
	}

	st := Subtask{Contract: contract, EstTokens: EstimateTokens(contract)}
	localView := r.localView()

	var chosen NodeView
	var base, reason string
	// deadFleet marks a LOCAL placement taken while the configured fleet was
	// failing its health probe — see PlacedResult.remotesUnreachable.
	deadFleet := false
	switch {
	case forced != nil:
		chosen, base, reason = forced.view, forced.base, forced.reason
	case r.route == "spread":
		// Read, never re-derive: the deal is a joint assignment across all the
		// subtasks, so recomputing one subtask's slot in isolation here would
		// throw away the very constraint that makes it correct.
		d := r.spreadDeal[i]
		deadFleet = d.deadFleet
		chosen, base, reason = d.view, d.base, d.reason
		if d.capacityWait {
			// Every remote with room was already dealt to its headroom and the
			// local seat is not in this subtask's rotation: hand it to the
			// capacity wait, which places it on the first node that frees.
			return PlacedResult{waitCapacity: true, overflow: true, pendingReason: reason, PlacementReason: reason}
		}
		if d.reserved {
			// Reserved at deal time; the lease may have cleared since (a run
			// can wait minutes in the semaphore), so re-read before deciding.
			// Still reserved: hand the subtask to the capacity wait (0.113.18),
			// which watches the lease AND every remote's room — not through
			// finish(): nothing ran, so there is nothing to record.
			if Reserved(r.localLease(contract)) {
				// reason already names the holder and why no remote qualified
				// (placeSpread built it); it is the deferral's text when nothing frees.
				return PlacedResult{waitCapacity: true, pendingReason: reason, PlacementReason: reason, remotesUnreachable: deadFleet}
			}
			if _, _, fenced := r.fencedLocal(contract); fenced {
				// Dealt local under a fence (fencedLocalSpread) and still fenced.
				return PlacedResult{waitCapacity: true, fenced: true, pendingReason: reason, PlacementReason: reason, remotesUnreachable: deadFleet}
			}
			reason += " — lease cleared, running local"
		}
	case (r.route == "auto" || r.route == "remote") && r.autoDeal != nil:
		// Read, never re-derive: W-06's joint deal (dealAutoRemote), computed
		// once in RunWith over one fleet snapshot — see its own doc for why a
		// per-subtask re-probe/re-place here would throw away the very
		// invariant that makes headroom accounting correct. r.autoDeal is nil
		// only for a caller that reached attempt() without going through
		// RunWith (a white-box test driving runOne/attempt directly); that
		// case falls to default below, unchanged from before W-06.
		d := r.autoDeal[i]
		deadFleet = d.deadFleet
		if d.jobID != "" {
			// Review round 1, MEDIUM item 3: reuse the SAME id W-11's P2C
			// draw was seeded with at deal time, rather than minting an
			// unrelated one here — the seed the ranking recorded and the id
			// the published result/ledger/corpus row carries must agree.
			jobID = d.jobID
			// The id the runner remembers for this subtask (lastJob) is the one the
			// abandon publishes when this goroutine never returns. It was stored above
			// with the id minted for this attempt, which the deal's has just replaced:
			// the attempt's seat, node and rows all carry the deal's, so that is the id
			// the caller must be given (ADR 0065, decision 3).
			r.lastJob.Store(i, jobID)
		}
		switch {
		case d.capacityWait:
			// At least one remote was otherwise eligible; every one of them
			// was already dealt to its own headroom by this subtask's turn.
			// The existing capacity wait watches for room to free — the same
			// mechanism a 503 refusal or a held lease already sends work to.
			return PlacedResult{waitCapacity: true, overflow: true, pendingReason: d.reason, PlacementReason: d.reason}
		case d.noRemote && (r.route == "remote" || (r.remoteHint() && seatless(localView))):
			// Nothing in the fleet could ever take this contract (capability,
			// not capacity): the established "route=remote: no eligible
			// remote" Unplaced defer, unchanged. A remote HINT gets it too on a
			// box with no agent seat (a delegation client): the seat is the
			// hint's fallback only where there is one, and sending the contract
			// to a seat that does not exist would answer "configure an agent
			// seat" to a client built to have none, count a placement on the
			// local seat that never happened, and leave the fleet's verdict in
			// the placement text alone.
			why, class := r.noEligibleRemote(st, r.autoViews, r.autoProbeErrs)
			// The deal narrated every node it saw (D-105); keep that beside the
			// aggregate verdict so the operator reads WHICH gate refused WHOM.
			placementReason := "route=remote: no eligible remote"
			if strings.TrimSpace(d.reason) != "" {
				placementReason += "; " + d.reason
				why += " — " + d.reason
			}
			return finish(PlacedResult{
				Node: localView.NodeID, Seat: localView.AgentSeat,
				Unplaced:        true,
				PlacementReason: placementReason,
				Result: core.AgentWireResult{
					SchemaVersion: core.AgentWireSchemaVersion,
					Deferred:      true,
					DeferClass:    class,
					Reason:        "route=remote: " + why,
				},
			})
		case d.noRemote:
			// route=auto, nothing eligible. A TEXT lease held NOW (re-read at
			// dispatch time — the deal's own snapshot can be minutes stale by
			// the time this subtask's turn comes, same as the spread case
			// above) still reserves the seat; otherwise queued-local beats
			// ineligible-remote, exactly as before W-06.
			leaseInfo := r.localLease(contract)
			why, class := r.noEligibleRemote(st, r.autoViews, r.autoProbeErrs)
			// A contract that names a layer this box does not declare (register A-108) waits on no lease
			// and is not "queued-local": the seat cannot take it. It lands here only so the seat's own
			// decision defers it naming the layer, and the fleet's verdict rides beside that.
			localServes := r.localServesLayer(localView, contract)
			// The class of "no eligible remote" rides every branch below, the waits included: a fleet
			// that fails every probe is reported whether the seat runs the work now or the subtask
			// waits in line for a lease to end (a fallback must not hide a failure).
			deadFleet = class == core.DeferClassInfrastructure
			if localServes && Reserved(leaseInfo) {
				return PlacedResult{waitCapacity: true, pendingReason: "local seat reserved (" + HolderLine(leaseInfo) + "); no eligible remote — " + why, remotesUnreachable: deadFleet}
			}
			if fence, fenceWhy, fenced := r.fencedLocal(contract); localServes && fenced {
				return PlacedResult{waitCapacity: true, fenced: true, pendingReason: "local seat fenced (" + fenceWhy + " — " + HolderLine(fence) + "); no eligible remote — " + why, remotesUnreachable: deadFleet}
			}
			// An occupied seat (operator 2026-10-06: "wait in line") is a place in line too, never
			// "queued-local": loading it would unload the vLLM seat holding the cards. The wait takes
			// it once the occupant leaves (localStillBusy reads occupiedBy) or a remote frees.
			if occ := r.autoOccupant(); localServes && occ.occupiedBy != "" {
				return PlacedResult{waitCapacity: true, overflow: true, pendingReason: occ.why() + "; no eligible remote — " + why, remotesUnreachable: deadFleet}
			}
			chosen = localView
			reason = r.queuedLocalReason(why)
			if !localServes {
				reason = fmt.Sprintf("no eligible remote, and %s so the seat cannot take it (%s)", localRefusal(localView, contract), why)
			}
		default:
			chosen, base, reason = d.view, d.base, d.reason
		}
	default:
		// Placement. Health is fetched ONLY when a remote could actually be
		// chosen (route=remote, or route=auto with the local GPU spoken for):
		// Place ignores remotes entirely when the local node is idle, so probing
		// them would be pure chatter.
		busy := false
		var leaseInfo gpulease.Info
		switch r.route {
		case "remote":
			busy = true // forced remote behaves as "local unavailable" for Place
		case "auto":
			leaseInfo = r.localLease(contract)
			// W-01 (register S-01): a lease is one way the local seat is
			// spoken for, but the overwhelming majority of runs hold no lease
			// at all — and a local seat already carrying more in-flight
			// requests than the fleet's own concurrency cap is exactly as
			// unavailable as one under a lease, yet Place never looked at the
			// fleet for it. probeLocalBusy is read ONCE per Run (cached on
			// the runner, sync.Once) so runConcurrency sibling subtasks agree
			// on one reading, the same invariant spreadLocalBusy holds for
			// route=spread. A probe failure fails OPEN to idle, exactly as
			// probeLocalBusy always has.
			r.autoLocalBusyOnce.Do(func() { r.autoLocalBusy = r.probeLocalBusy(ctx) })
			local := r.autoLocalBusy
			busy = leaseInfo.Held || r.atRunCap(local.inflight) || local.loading || local.occupiedBy != ""
			if r.atRunCap(local.inflight) || local.loading || local.occupiedBy != "" {
				r.autoDealReadBusy.Store(true) // the seat's load, never the lease (see the joint deal)
			}
			// A remote hint (ADR 0078) is placed remotes-first: the seat is unavailable whatever it read, and the
			// reading above is kept for the fallback to wait on a lease or an occupant.
			if r.remoteHint() {
				busy = true
			}
		}
		var views []NodeView
		var bases []string
		var probeErrs []string
		if busy && r.route != "local" {
			views, bases, probeErrs = r.fetchViews(ctx)
		}
		chosen = Place(jobID, st, localView, views, busy)
		// The wasted local leg (GPU routing P7): nothing else could take it, so the local seat
		// would - but a lease this process does not hold fences every seat of the contract's
		// chain, and the seat would turn the run away at its own pre-check.
		var fence gpulease.Info
		var fenceWhy string
		var fenced bool
		if chosen.Local && busy && r.route == "auto" && r.localServesLayer(localView, contract) {
			fence, fenceWhy, fenced = r.fencedLocal(contract)
		}

		switch {
		case r.route == "local":
			reason = "route=local forced"
			// A reasoned pin joins the local seat's line only while the line has room (pinline.go).
			if r.pin.reason != "" && r.pin.asked == "local" {
				release, refused := r.admitPinnedLocal(ctx, localView)
				if refused != nil {
					return finish(*refused)
				}
				defer release()
			}
		case r.route == "remote" && chosen.Local:
			// An explicit remote route with nothing eligible must NOT silently
			// fall local — defer loudly and let the caller decide. The diagnosis
			// distinguishes the causes that used to share one sentence, and the
			// class separates "a box is broken" from "this contract cannot be
			// placed anywhere, however healthy the fleet".
			why, class := r.noEligibleRemote(st, views, probeErrs)
			return finish(PlacedResult{
				Node: localView.NodeID, Seat: localView.AgentSeat,
				// NO node ran this: the route was forced remote and nothing was
				// eligible, so this is the same "nobody took the work" state a
				// capacity defer or a shed reports. Node/Seat above are the
				// DECIDING box, not the box that ran it — a surface that renders
				// them as the node under test names the operator's own machine
				// and hides which base failed (issue #250), so it needs this flag
				// to tell the two apart. Inert for the tallies: the one branch
				// that reads Unplaced also requires Replacements > 0.
				Unplaced:        true,
				PlacementReason: "route=remote: no eligible remote",
				Result: core.AgentWireResult{
					SchemaVersion: core.AgentWireSchemaVersion,
					Deferred:      true,
					DeferClass:    class,
					Reason:        "route=remote: " + why,
				},
			})
		case r.route == "remote":
			reason = "route=remote forced → " + chosen.NodeID
		case !busy:
			reason = "local idle"
		case chosen.Local && Reserved(leaseInfo):
			// A TEXT lease reserves the seat: "queued-local beats
			// ineligible-remote" is exactly the placement the reservation
			// exists to forbid. Wait for the holder (agent_lease_wait_sec),
			// then defer naming it — never run on the reserved cards.
			// Since 0.113.18 the wait is the capacity wait (awaitCapacity):
			// it watches the lease AND every remote's room, so a remote that
			// frees while the local card is reserved takes the work.
			why, class := r.noEligibleRemote(st, views, probeErrs)
			return PlacedResult{waitCapacity: true, pendingReason: "local seat reserved (" + HolderLine(leaseInfo) + "); no eligible remote — " + why, remotesUnreachable: class == core.DeferClassInfrastructure}
		case chosen.Local && fenced:
			// A lease fences every local seat this contract could run on (fencedLocal): the same
			// place in line as a reservation, without the dial that would be turned away.
			why, class := r.noEligibleRemote(st, views, probeErrs)
			return PlacedResult{waitCapacity: true, fenced: true, pendingReason: "local seat fenced (" + fenceWhy + " — " + HolderLine(fence) + "); no eligible remote — " + why, remotesUnreachable: class == core.DeferClassInfrastructure}
		case chosen.Local && r.route == "auto" && r.autoOccupant().occupiedBy != "" && r.localServesLayer(localView, contract):
			// The occupied seat waits in line like a reservation (operator 2026-10-06: "wait in
			// line"): never loaded over the vLLM seat holding the cards while that seat stays.
			why, class := r.noEligibleRemote(st, views, probeErrs)
			return PlacedResult{waitCapacity: true, overflow: true, pendingReason: r.autoOccupant().why() + "; no eligible remote — " + why, remotesUnreachable: class == core.DeferClassInfrastructure}
		case chosen.Local:
			why, class := r.noEligibleRemote(st, views, probeErrs)
			reason = r.queuedLocalReason(why)
			// USE the class here too. route=remote already exits non-zero on a
			// fleet that failed every probe; route=auto discarded the identical
			// verdict (`why, _ :=`), so a fleet that had been down for a week read
			// green forever behind a series of correct local placements. The
			// placement stays right — the work runs locally — but the broken fleet
			// gets reported. Only the INFRASTRUCTURE class is loud: "no remotes
			// configured" and "they answered and did not qualify" are ordinary
			// idle-local life and must never make a normal run exit non-zero.
			deadFleet = class == core.DeferClassInfrastructure
		default:
			reason = "local busy; placed on " + chosen.NodeID
			if r.remoteHint() {
				reason = "remotes first; placed on " + chosen.NodeID
			}
			if r.route == "auto" && r.autoLocalBusy.occupiedBy != "" {
				reason = r.autoLocalBusy.why() + "; placed on " + chosen.NodeID
			}
		}
		if !chosen.Local {
			base = baseFor(chosen, views, bases)
		}
	}

	if chosen.Local {
		// The composite decision (ADR 0052): which layer and seat serve this
		// contract on THIS box. A forced placement from the capacity wait
		// carries the decision it found; otherwise decide now. The zero
		// Decision (a plain box) runs the planner seat and publishes nothing.
		var dec placetable.Decision
		if forced != nil && forced.decided != nil {
			dec = *forced.decided
		} else {
			dec = r.decide(ctx, contract, st)
		}
		switch {
		case dec.Defer:
			pr := r.decidedDefer(localView, dec, reason)
			pr.remotesUnreachable = deadFleet
			return finish(pr)
		case dec.Wait:
			// The decided seat would evict a busy seat (the pair's long seat
			// over a loaded agent-pool): hold in the capacity wait, which
			// re-runs the decider per tick and runs here once it drains —
			// not through finish(): nothing ran, nothing to record yet.
			return PlacedResult{waitCapacity: true, pendingReason: dec.Reason, PlacementReason: reason, decided: &dec}
		}
		pr := r.runLocal(ctx, jobID, contract, localView, reason, dec)
		pr.remotesUnreachable = deadFleet
		pr.ranLocal = true
		return finish(pr)
	}
	if base == "" {
		// Unreachable (chosen came from views); kept for defense — a placement
		// with no dial target is a bug, not a defer.
		return finish(PlacedResult{Node: chosen.NodeID, PlacementReason: reason, Err: "internal: placed node has no base URL"})
	}
	// A composite remote (advertised rows): the dispatched copy names the layer
	// the table chose so the node runs the same decision for that layer with
	// its own live guards (council R5); the delegator's block is published
	// until the node's own arrives on the wire.
	dispatched := contract
	var remotePlaced *core.Placed
	// runSeat is the seat that decision puts the contract on — the layer's
	// seat, not the advertised agent seat, whenever the table chose a layer.
	// It is carried into the poll bound (register D-116, review finding 2):
	// the node sizes its wall from the seat it actually runs on, and health
	// advertises a rate for the agent seat alone.
	runSeat := ""
	if dec, ok := remoteDecision(st, chosen); ok && !dec.Defer {
		dispatched.Layer = dec.Layer
		remotePlaced = placedOrNil(dec)
		runSeat = dec.Seat
	}
	// The process gate (processgate.go): this process never holds more dispatches
	// open on a node than the node's published admission ceiling, across EVERY
	// concurrent Run. A dispatch that would is not sent - nothing reached the node,
	// so this is no refusal and writes no row - and the subtask waits in line for the
	// first node that frees.
	release, admitted := processGate.tryAcquire(base, admissionCeiling(chosen))
	if !admitted {
		why := gateFullReason(base, chosen)
		return PlacedResult{waitCapacity: true, gated: true, pendingReason: why, PlacementReason: why}
	}
	defer release()
	pr := r.runRemote(ctx, base, jobID, dispatched, chosen, runSeat)
	// The node's part of the job is over: runRemote returns on its terminal answer, on
	// the delegator giving up on the job, or on a refusal. What finish() does next can be
	// the delegator's own rescue of the answer (rescue.go), on this box and allowed
	// minutes; a slot still counted through it would keep an idle node out of every
	// sibling Run's reach (ADR 0063 decision 7: the count ends with a terminal answer
	// or with the delegator giving up). The deferred release above stays as the net for
	// a panic; release is safe to run twice.
	release()
	pr.ranBase = base
	pr.PlacementReason = reason
	// results[].node is the id the placement used: the node_id the node publishes in /fleet/health, the name the placement
	// text, the progress lines and the dispatch marker carry. runRemote copied the id the node stamped on its own result into
	// pr.Node; that one stays in pr.Result.NodeID and is published as node_reported when it differs. The result's own id
	// names the node only when health published none (laneID).
	if chosen.NodeID != "" || pr.Node == "" {
		pr.Node = chosen.NodeID
	}
	pr.Placed = remotePlaced
	if pr.Result.Placed != nil {
		pr.Placed = pr.Result.Placed
	}
	if pr.Seat == "" {
		pr.Seat = chosen.AgentSeat
		if remotePlaced != nil && remotePlaced.Seat != "" {
			pr.Seat = remotePlaced.Seat
		}
	}
	return finish(pr)
}

// runLocal executes in-process via the LocalRunner seam and shapes the result.
// dec is the composite decision the run is made under: its seat and placed
// block are handed to the runner (LocalOptions) and published on the result;
// the zero Decision (a plain box) hands zero options, exactly as before.
func (r *runner) runLocal(ctx context.Context, jobID string, contract core.AgentContract, view NodeView, reason string, dec placetable.Decision) PlacedResult {
	pr := PlacedResult{Node: view.NodeID, Seat: view.AgentSeat, PlacementReason: reason, Placed: placedOrNil(dec)}
	if r.local == nil {
		pr.Err = "no local runner wired (delegator surfaces must supply one)"
		return pr
	}
	opts := LocalOptions{Seat: dec.Seat, Placed: pr.Placed, ParentJobID: jobID}
	if dec.Seat != "" {
		pr.Seat = dec.Seat
	}
	// A local placement starts the moment it is handed to the runner: no
	// queue, no ack. "" as the node = this box's PAIR identity — unless the
	// endpoint is another box's engine, whose name the card then carries
	// (register C-58: PAIR showed <node-b> doing <node-c>'s work).
	pairNode := ""
	if host := modelaffinity.EndpointHost(r.cfg.Endpoint, r.cfg.FleetNodeID); host != "" {
		pairNode = host
		pr.PlacementReason += "; engine " + r.cfg.Endpoint + " is " + host + "'s (attributed there)"
	}
	// The run is handed to the seat now: mark it in the ledger (PR-14, ADR 0064),
	// so a runner that wedges is visible while it does.
	r.recordStarted(contract, jobID, "", pr.Node, pr.Seat, pr.PlacementReason)
	// Queued until the seat is working on it (seatWorking): the run's own
	// progress reports flip the card, so a cold load does not read "running".
	r.pairInflight(&pr, jobID, pairNode, nil, pr.Seat, "queued", true)
	gate := newPairStartGate(func() { r.pairInflight(&pr, jobID, pairNode, nil, pr.Seat, "running", true) })
	wire, err := r.local(core.WithProgressReport(ctx, gate.observe), contract, opts)
	gate.stop()
	if err != nil {
		pr.Err = "local run: " + err.Error()
		return pr
	}
	pr.Result = wire
	// The runner's own id names the node, except where the placement attributed the run to another box (pairNode, set above
	// from the endpoint's host): the runner stamps its own box, and that would undo the attribution.
	if wire.NodeID != "" && pairNode == "" {
		pr.Node = wire.NodeID
	}
	if wire.Seat != "" {
		pr.Seat = wire.Seat
	}
	if wire.Placed != nil {
		// The pipeline's own stamp wins: identical by construction (it was
		// handed this block), and the one source of truth for the wire.
		pr.Placed = wire.Placed
	}
	if !wire.Deferred {
		// Same guard runRemote applies: a defer produced no answer, so running
		// acceptance over it manufactures "failures" about content that was
		// never claimed — turning an honest defer into a verification failure
		// in the ledger and the corpus.
		pr.gradeAcceptance(contract, wire)
		// No strikeOnFingerprint here ON PURPOSE: the local seat is the one
		// placement always able to take a contract (replacementNode reserves it
		// as the last resort), so quarantining it would strand every subtask.
		// A local off-document answer still fails verification and is counted.
	}
	return pr
}

// localNodeID mirrors runAgentTask's rule: configured fleet_node_id, else the
// OS hostname, so a shared config never bakes one box's name into another's
// results.
func (r *runner) localNodeID() string {
	// A "local" run against another box's engine (a bench config whose
	// endpoint is <node-c>'s arm) is that box's work: name the node after
	// the endpoint host, as a remote placement is named after its base
	// (register C-58: PAIR showed <node-b> doing <node-c>'s work).
	if host := modelaffinity.EndpointHost(r.cfg.Endpoint, r.cfg.FleetNodeID); host != "" {
		return host
	}
	if r.cfg.FleetNodeID != "" {
		return r.cfg.FleetNodeID
	}
	if hn, err := os.Hostname(); err == nil {
		return hn
	}
	return "local"
}

// ---- telemetry (roast delta 9) ----

// delegationLogMu serializes in-process corpus appends. Unlike the ledger's
// small rows, a corpus line can run to hundreds of KiB (it carries the whole
// contract), so O_APPEND atomicity alone cannot be trusted to keep concurrent
// goroutines' lines from interleaving.
var delegationLogMu sync.Mutex

// delegationLogLine is one delegation-log corpus record: the spec'd telemetry
// keys plus the full (contract, result, acceptance) tuple — the standing
// small-model agent-task corpus (no sub-27B agent data exists anywhere; this
// accumulates it from real work).
type delegationLogLine struct {
	TS                 int64                 `json:"ts"`
	JobID              string                `json:"job_id"`
	Node               string                `json:"node"`
	Seat               string                `json:"seat"`
	PlacementReason    string                `json:"placement_reason"`
	Deferred           bool                  `json:"deferred"`
	DeferClass         string                `json:"defer_class,omitempty"`
	AcceptancePass     bool                  `json:"acceptance_pass"`
	WallMs             int64                 `json:"wall_ms"`
	EstTokens          int                   `json:"est_tokens"`
	Error              string                `json:"error,omitempty"`
	Contract           core.AgentContract    `json:"contract"`
	Result             *core.AgentWireResult `json:"result,omitempty"`
	AcceptanceFailures []string              `json:"acceptance_failures,omitempty"`
	// AcceptanceAuthority is the authority the contract closed at (PlacedResult.AcceptanceAuthority), by name. Omitempty:
	// absent on a row that claims none, and on every row written before the field.
	AcceptanceAuthority string `json:"acceptance_authority,omitempty"`
	// Arm labels which experimental arm produced this row.
	//
	// This is an ENABLER, not a convenience. The delegation log is append-only and is
	// written CONCURRENTLY by whatever sessions are running -- during one review pass it
	// grew 20 -> 24 rows, and some of those new rows were themselves a cross-seat replay.
	// Once arms are interleaved in one file with no label, they cannot be separated after
	// the fact: timestamps do not distinguish an experiment from ordinary traffic.
	//
	// Set from OFFLOAD_DELEGATE_ARM. Empty (omitted) for ordinary traffic, which is what
	// makes the field safe to add: existing rows and unlabelled runs read as "not part of
	// any arm" rather than as a missing value.
	//
	// NOTE the rejected alternative: pointing the delegation log at a scratch directory to
	// isolate a run. BaseDir() is the single state root for cache.db, ledger.jsonl, media,
	// exemplars, router weights and confhead labels -- relocating it would run the
	// experiment against an empty cache, which changes the thing being measured.
	Arm string `json:"arm,omitempty"`
	// DelegatorVersion / DelegatorBuildSHA256 pin the DELEGATOR-side code that
	// produced this row (A1, 0.81.0): acceptance evaluation, retry policy and
	// placement all run here, so a corpus row is only pairable with another
	// when BOTH ends are pinned — the node's end travels inside Result
	// (harness_version / harness_build_sha256 / seat_config_*), this is the
	// other end. Omitempty: pre-0.81 rows read as unknown, never as a value.
	DelegatorVersion     string `json:"delegator_version,omitempty"`
	DelegatorBuildSHA256 string `json:"delegator_build_sha256,omitempty"`
	// Placed (ADR 0052) is the placement decision the row was produced under
	// (PlacedResult.Placed). Omitempty: a plain box's corpus row is unchanged,
	// and a pre-0.116 row reads as "one implicit layer", never as unplaced.
	Placed *core.Placed `json:"placed,omitempty"`
	// Refusal marks a row that is a node's capacity refusal at dispatch (503 or 429), not an outcome: the engine
	// outwaits it and re-places the subtask, whose outcome is another row (the one that landed, or the terminal
	// row a lost subtask gets under a job id of its own). Absent on every other row, and on rows written before
	// the marker, so a reader that sees none reads them as it always did. The rigger does not count a marked row
	// as a failure.
	Refusal *refusalMark `json:"refusal,omitempty"`
}

// refusalMark is the delegation log's record of one capacity refusal: the status the node sent and its own
// Retry-After (0 = none).
type refusalMark struct {
	Status        int `json:"status"`
	RetryAfterSec int `json:"retry_after_sec,omitempty"`
}

// record writes one subtask's telemetry: the delegation-log corpus line and a
// ledger row (task=agent_delegate). Best-effort by design — telemetry must
// never fail the work it describes (pipeline.record's exact posture).
func (r *runner) record(contract core.AgentContract, pr PlacedResult) {
	// The row says what the caller pinned (ADR 0078): the hint clause opens the placement note, so the ledger's
	// short form of it (120 bytes) keeps it, and the closed reason and the asked route are columns of their own.
	// Only the note is read from the stamped copy: the reason code is decided from the result as the engine made it
	// (reasonCodeFor matches the placement text "refused before placement" exactly).
	placement := r.stampPin(pr).PlacementReason
	pinReason, routeAsked := r.pinLedgerFields()
	line := delegationLogLine{
		TS:                  time.Now().Unix(),
		JobID:               pr.JobID,
		Node:                pr.Node,
		Seat:                pr.Seat,
		PlacementReason:     placement,
		Deferred:            pr.Result.Deferred,
		DeferClass:          pr.Result.DeferClass,
		AcceptancePass:      pr.Err == "" && !pr.Result.Deferred && len(pr.AcceptanceFailures) == 0,
		WallMs:              pr.wallMs,
		EstTokens:           EstimateTokens(contract),
		Error:               pr.Err,
		Contract:            contract,
		AcceptanceFailures:  pr.AcceptanceFailures,
		AcceptanceAuthority: publishedAuthority(pr.AcceptanceAuthority),
		Arm:                 strings.TrimSpace(os.Getenv("OFFLOAD_DELEGATE_ARM")),
		DelegatorVersion:    buildinfo.Version,
		// Computed once per process (sync.Once) — the per-row cost is a read.
		DelegatorBuildSHA256: buildinfo.BuildSHA256(),
		Placed:               pr.Placed,
	}
	if pr.Err == "" {
		res := pr.Result
		line.Result = &res
	}
	if pr.refusalRow {
		line.Refusal = &refusalMark{Status: pr.refusalStatus, RetryAfterSec: pr.retryAfterSec}
	}
	r.corpusTried.Add(1)
	if err := appendDelegationLog(r.cfg.BaseDir(), line); err != nil {
		r.corpusLost.Add(1)
		// The corpus is a DELIVERABLE of this lane (the standing small-model
		// agent-task dataset), not incidental logging: an unwritable corpus is
		// worth an operator's attention even though it can never fail the work.
		r.warnCorpus.Do(func() {
			log.Printf("delegate: delegation-log write failed under %s; this run's corpus rows are LOST (results unaffected): %v", r.cfg.BaseDir(), err)
		})
	}

	if r.led == nil {
		if r.ledgerUnopened {
			// Attempted-and-lost: the row was owed and no handle exists to write
			// it. (No configured LedgerPath at all is not a loss — nothing was
			// ever owed — so it stays uncounted.)
			r.ledgerTried.Add(1)
			r.ledgerLost.Add(1)
		}
	} else {
		reason := pr.Err
		if reason == "" && pr.Result.Deferred {
			reason = pr.Result.Reason
		}
		if reason == "" && len(pr.AcceptanceFailures) > 0 {
			reason = "failed verification: " + pr.AcceptanceFailures[0]
		}
		// Layer (council R8): the composite layer this row was placed on, so
		// the scoreboard can sum work per layer later; "" (omitted) on a
		// plain box.
		layer := ""
		if pr.Placed != nil {
			layer = pr.Placed.Layer
		}
		r.ledgerTried.Add(1)
		if err := r.led.Record(ledger.Entry{
			Task:      "agent_delegate",
			Layer:     layer,
			LatencyMs: pr.wallMs,
			// TokensIn stays 0 ON PURPOSE: the summary counts a completed
			// row's TokensIn as tokens-saved, and a delegation row claiming
			// savings would double-count the node-side agent row.
			TokensOut:    pr.Result.TokensOut,
			SeatTokensIn: pr.Result.SeatTokensIn,
			// tok_per_s on the DELEGATE row (0.131.0): until now only the
			// node's own `agent` twin row carried it, so the row the operator
			// reads showed 0 on a job that produced 6,236 tokens.
			TokPerSec: firstNonZero(pr.Result.SeatTokS, pr.Result.ObservedTokS),
			Deferred:  pr.Result.Deferred || pr.Err != "" || len(pr.AcceptanceFailures) > 0,
			Reason:    reason,
			// ModelTier carries placement:seat — the ledger has no placement
			// column, and "which node/seat ran it" is the row's whole story.
			ModelTier: pr.Node + ":" + pr.Seat,
			// The job behind the row (D-101 / F15): what this result already
			// knew, so a reader never has to open the corpus for it. The
			// session that asked is stamped by ledger.Record itself.
			JobID: pr.JobID,
			// PR-14 (ADR 0064): the surface that admitted the contract, the id the
			// fleet node knows the job by (a remote attempt only: a job that stayed
			// on this box has none) and the closed-set reason code — so a reader
			// never joins on latency or greps prose. The reason above is stored
			// whole: the ledger no longer cuts it.
			Door:             doorOf(contract),
			FleetJobID:       fleetJobIDOf(pr),
			ReasonCode:       reasonCodeFor(pr),
			Route:            r.route,
			RouteAsked:       routeAsked,
			PinReason:        pinReason,
			Placement:        placement,
			Steps:            pr.Result.Steps,
			StopReason:       pr.Result.StopReason,
			RepackMs:         pr.Result.RepackMs,
			RepackAttempts:   pr.Result.RepackAttempts,
			QueuedMs:         pr.Result.QueuedMs,
			AcceptanceResult: acceptanceResult(pr),
			// The same mark the corpus line carries (delegationLogLine.Refusal): a place in line, not the subtask's outcome.
			Refusal: pr.refusalRow,
		}); err != nil {
			r.ledgerLost.Add(1)
			r.warnLedger.Do(func() {
				log.Printf("delegate: ledger row write failed for %s; this run's savings accounting is incomplete (results unaffected): %v", r.cfg.LedgerPath, err)
			})
		}
	}
}

// acceptanceResult is the ledger's one-word verdict for a placed result:
// "pass" (the run completed and every acceptance check held — including a
// contract that declared none), "fail" (a check failed), "" (nothing was
// evaluated: the run deferred or the wire failed). It mirrors the corpus
// row's acceptance_pass, spelled so a deferred row cannot read as a failed
// check.
func acceptanceResult(pr PlacedResult) string {
	switch {
	case len(pr.AcceptanceFailures) > 0:
		return "fail"
	case pr.Err == "" && !pr.Result.Deferred:
		return "pass"
	}
	return ""
}

// appendDelegationLog appends one JSONL line to
// BaseDir()/delegation-log/YYYY-MM-DD.jsonl (day-sharded so the corpus stays
// tail-able and old days archive by file).
func appendDelegationLog(baseDir string, line delegationLogLine) error {
	dir := filepath.Join(baseDir, "delegation-log")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	val, err := json.Marshal(line)
	if err != nil {
		return err
	}
	val = append(val, '\n')
	path := filepath.Join(dir, time.Now().Format("2006-01-02")+".jsonl")
	delegationLogMu.Lock()
	defer delegationLogMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(val)
	return err
}

// firstNonZero is the ledger's tok_per_s preference: the calibrated seat rate,
// else the run's observed decode rate (0.131.1).
func firstNonZero(a, b float64) float64 {
	if a > 0 {
		return a
	}
	return b
}
