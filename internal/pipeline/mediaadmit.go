package pipeline

// Per-card media admission (plan P13).
//
// A generation call used to take the machine-wide media lease: one card or all of them, the
// same thing, because a lease named no cards. On a host that has turned card-scoped leases on
// (config gpu_card_scoped_leases, a green reader audit) a call now asks for what it needs:
//
//   - a single-card route (image, edit, inpaint, upscale, animate, music, un-pooled video) asks
//     the allocator for ONE card, holds a lease on it, and runs in the ComfyUI instance bound to
//     that card (its own port, directories and launch marker, pinned by GPU uuid). Two such calls
//     run on two cards at once; the display card is never auto-assigned while the operator is at
//     the desk (the shared rule in gpuprobe, which reads display_attached as well as
//     display_active);
//   - an EXPLICIT pin (comfy_cuda_device) is a hard constraint, never re-picked: the call queues
//     for that card, FIFO, however many others are free. The pin is in ComfyUI's order, so it is
//     turned into a card through the order the box declares (gpu_comfy_order); an order that is
//     not declared, or a pin that names several cards, falls back to the whole-node lease and
//     today's `--cuda-device`, with one logged reason, never a guess;
//   - a POOLED route computes on cards its pool keys name (also ComfyUI order), through the
//     default instance with every card visible, so its launch is unchanged. It leases the
//     pool's cards when that is provably safe (see poolMayBeScoped) and otherwise the whole node;
//   - run-graph with ONE declared device runs in that card's own instance; with several, or none,
//     it holds the whole node (a graph in the default instance sees every card, so a lease on
//     some of them would not confine it);
//   - everything else (sdcpp, voice, a host with card-scoped leases off or no card table) takes
//     the whole-node lease exactly as before, byte for byte.
//
// A call that cannot get a card inside its window does not fail with "gpu busy". It leaves a
// place-keeping token (gpulease/tokens.go) and answers with it, its position and an ETA; the
// caller re-calls with waiter_token and resumes the place it left. A busy card is a place in
// line.
//
// WHY A KEYED INSTANCE FOR EVERY SINGLE-CARD JOB. ComfyUI's default instance (port 8188) is one
// process for the whole box: two jobs that both use it contend on its port, its output
// directory and its launch marker, and a second job whose launch profile differs is refused
// (COMFY-PROFILE-MISMATCH) while one that asks for none silently reuses the first one's
// instance, on the first one's cards. A job that holds only SOME of the box's cards therefore
// must not run in it. Single-card jobs get an instance per card; the jobs that DO use the
// default instance hold the whole node, or (pooled) cards that every other pooled job on a box
// of at most three cards also needs, so they cannot run beside each other either.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/comfyinst"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpualloc"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/imagegen"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// mediaKind says how a call chooses its cards.
type mediaKind int

const (
	needWhole    mediaKind = iota // the whole node (and the default instance): the legacy lease
	needSingle                    // one card: the allocator's, or the explicit pin's
	needPooled                    // the cards a pool's keys name
	needDeclared                  // cards the operator declared (run-graph `devices`)
)

// mediaNeed is what one media call asks of the GPU.
type mediaNeed struct {
	Kind mediaKind
	// Pin is comfy_cuda_device as configured (needSingle): "" = let the allocator choose.
	Pin string
	// Pool is the pool's ComfyUI device keys, "cuda:1" or "1" (needPooled).
	Pool []string
	// Devices are the cards the operator declared (needDeclared): nvidia-smi indices or UUID prefixes.
	Devices []string
	// Token is the waiter_token the caller got from an earlier queued answer ("" = a new arrival).
	Token string
	// Resumable says the call came through a door that can hand a place in line back and take it
	// again (core.Request.Resumable). Only then is a place kept for a call that waited its window
	// with no card; any other gets the plain "gpu busy" and leaves nothing behind, because a
	// token it can never claim would hold a card back from the next caller for the grace.
	Resumable bool
}

// resumableBy marks the need with whether the request's door can resume a place in line.
func (n mediaNeed) resumableBy(req core.Request) mediaNeed {
	n.Resumable = req.Resumable
	return n
}

// wholeNeed is a call that holds the whole node. token is the waiter_token it resumes, if any.
func wholeNeed(token string) mediaNeed { return mediaNeed{Kind: needWhole, Token: token} }

// singleCardNeed is a route that renders on one card. The binding's explicit pin, if any, is a
// hard constraint on which.
func singleCardNeed(cfg config.Config, token string) mediaNeed {
	return mediaNeed{Kind: needSingle, Pin: strings.ReplaceAll(cfg.ComfyCudaDevice, " ", ""), Token: token}
}

// pooledNeed is a pooled route: it computes on the cards its keys name.
func pooledNeed(token string, keys ...string) mediaNeed {
	return mediaNeed{Kind: needPooled, Pool: keys, Token: token}
}

// declaredNeed is a call whose operator named the cards (run-graph). No devices = the whole node.
func declaredNeed(devices []string, token string) mediaNeed {
	if len(devices) == 0 {
		return wholeNeed(token)
	}
	return mediaNeed{Kind: needDeclared, Devices: devices, Token: token}
}

// imageNeed is the need of the image-generation route (and its batch) for the resolved binding.
func imageNeed(cfg config.Config, token string) mediaNeed {
	if cfg.ImagePooled() {
		return pooledNeed(token, cfg.ImageGenPoolCompute, cfg.ImageGenPoolDonor)
	}
	return singleCardNeed(cfg, token)
}

// videoNeed is the need of the video route: pooled, or a single card like image generation.
func videoNeed(cfg config.Config, token string) mediaNeed {
	if cfg.VideoPooled() {
		return pooledNeed(token, cfg.VideoGenPoolCompute, cfg.VideoGenPoolDonor)
	}
	return singleCardNeed(cfg, token)
}

// mediaGrant is what a call holds while it renders.
type mediaGrant struct {
	// Env hands the lease down to the runner (GPU_LEASE_*).
	Env []string
	// Card and API are set when the call runs in the ComfyUI instance bound to one card.
	Card *gpuprobe.Card
	API  string
	// Release gives everything back; call it exactly when the render is done (it is idempotent).
	Release func()
}

// launch applies the grant to a binding's launch profile: a call on a per-card instance is
// pinned by the card's uuid at the instance's endpoint, and the legacy device index goes blank
// (the runner refuses both).
func (g mediaGrant) launch(l imagegen.ComfyLaunch) imagegen.ComfyLaunch {
	if g.Card == nil {
		return l
	}
	l.CardUUID, l.API, l.CudaDevice = g.Card.UUID, g.API, ""
	return l
}

// errGPUQueued is a media call's answer when it waited its window and has no card: the place it
// holds in line. The caller resumes it with the token.
type errGPUQueued struct {
	Token    string
	Position int
	ETASec   int
	Devices  []string
	HeldBy   []uint64
	Why      string
}

func (e *errGPUQueued) Error() string {
	// The estimate is the declared end of the LONGEST lease in the way: a request is served when
	// every lease it conflicts with has ended. 0 means nothing in the way declares an end (the
	// places ahead are callers, not leases), which is not "no wait".
	wait := "no estimate (nothing ahead of you declares an end)"
	if e.ETASec > 0 {
		wait = fmt.Sprintf("at most %ds until the lease(s) in the way end", e.ETASec)
	}
	return fmt.Sprintf("gpu queued: %s; your place in line is #%d (token %s), %s; call again with waiter_token=%s to keep it",
		e.Why, e.Position, e.Token, wait, e.Token)
}

// queuedPayload is the machine-readable half of the answer.
func (e *errGPUQueued) payload() json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"queued":         true,
		"waiter_token":   e.Token,
		"queue_position": e.Position,
		// An upper bound: the declared window of the lease in the way, which for media is a ceiling
		// the holder usually finishes well inside, not a promise.
		"eta_s":   e.ETASec,
		"devices": e.Devices,
		"held_by": e.HeldBy,
		"resume":  "call again with the same request and waiter_token=" + e.Token + " within 10 minutes to resume this place; a place not re-polled for 30 seconds stops holding back later callers",
	})
	return b
}

// mediaClaimRetries bounds how many times a call re-allocates after losing a claim race before
// it queues on the card it last picked.
const mediaClaimRetries = 16

var scopedWarnOnce sync.Once

// scopedManager opens the lease Manager with the per-host card-scoped switch applied. A host that
// asked for card-scoped leases and has no green reader audit keeps the writer off; that is said
// once per process, not per call.
func (p *Pipeline) scopedManager() (*gpulease.Manager, error) {
	m, err := gpulease.OpenAt(p.cfg.GPULockPath, p.cfg.StateDir)
	if err != nil {
		return nil, err
	}
	if err := m.ApplyCardScopedConfig(p.cfg.GPUCardScopedLeases); err != nil {
		scopedWarnOnce.Do(func() { log.Printf("media admission: %v", err) })
	}
	return m, nil
}

var planNoteOnce sync.Map // note -> struct{}

// planNote logs why a call fell back to the whole-node lease, once per distinct reason.
func planNote(why string) {
	if _, dup := planNoteOnce.LoadOrStore(why, struct{}{}); !dup {
		log.Printf("media admission: %s", why)
	}
}

// mediaPlan is the decision for one call.
type mediaPlan struct {
	whole    bool     // the legacy whole-node lease
	auto     bool     // the allocator picks the card
	ids      []string // the cards, when fixed (lease ids)
	instance bool     // run in the per-card instance of ids[0]
}

// planMedia turns a need into a plan. cards is the card table (nil when it could not be read).
func planMedia(need mediaNeed, cards []gpuprobe.Card, tableErr error) (mediaPlan, error) {
	whole := func(why string) (mediaPlan, error) {
		if why != "" {
			planNote(why)
		}
		return mediaPlan{whole: true}, nil
	}
	if need.Kind == needWhole {
		return whole("")
	}
	if tableErr != nil || len(cards) == 0 {
		return whole(fmt.Sprintf("the card table is unreadable (%v): this call holds the whole node, as it did before cards were leased", tableErr))
	}
	switch need.Kind {
	case needSingle:
		if need.Pin == "" {
			return mediaPlan{auto: true, instance: true}, nil
		}
		if strings.Contains(need.Pin, ",") {
			return whole(fmt.Sprintf("comfy_cuda_device %q names several cards: this call holds the whole node and keeps today's --cuda-device", need.Pin))
		}
		n, err := strconv.Atoi(need.Pin)
		if err != nil {
			return whole(fmt.Sprintf("comfy_cuda_device %q is not a ComfyUI device index: this call holds the whole node", need.Pin))
		}
		c, ok := gpuprobe.CardByComfyOrder(cards, n)
		if !ok {
			return whole(fmt.Sprintf("comfy_cuda_device %q is in ComfyUI's device order, which this box has not declared (config gpu_comfy_order), so it cannot be turned into a card: this call holds the whole node and keeps today's --cuda-device", need.Pin))
		}
		// The explicit pin is a hard constraint: this card and no other.
		return mediaPlan{ids: []string{c.LeaseID()}, instance: true}, nil
	case needPooled:
		ids, why := poolCards(cards, need.Pool)
		if why != "" {
			return whole(why)
		}
		return mediaPlan{ids: ids}, nil
	case needDeclared:
		got, err := gpuprobe.ResolveCards(cards, need.Devices)
		if err != nil {
			return mediaPlan{}, fmt.Errorf("devices %q: %w", strings.Join(need.Devices, ","), err)
		}
		ids := make([]string, 0, len(got))
		for _, c := range got {
			ids = append(ids, c.LeaseID())
		}
		sort.Strings(ids)
		// One device runs in that card's own instance, pinned by uuid: the graph sees one card and
		// can touch no other. Several would run in the DEFAULT instance, which sees every card, and
		// an arbitrary graph may place work on any of them, including a card another call's own
		// instance holds and the display card; a lease on only some of the cards would not stop
		// that, so several devices hold the whole node (unlike a pooled route, whose graph the
		// harness itself builds for its pool's cards).
		if len(ids) == 1 {
			return mediaPlan{ids: ids, instance: true}, nil
		}
		return whole(fmt.Sprintf("devices %q name several cards of a box with %d: the graph runs in the default instance, which sees every card, so a lease on some of them would not keep it off the others: this call holds the whole node", strings.Join(need.Devices, ","), len(cards)))
	}
	return whole("")
}

// poolCards resolves a pool's ComfyUI device keys to cards, through the order the box declares.
func poolCards(cards []gpuprobe.Card, keys []string) (ids []string, why string) {
	seen := map[string]bool{}
	for _, k := range keys {
		k = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(k)), "cuda:"))
		if k == "" {
			continue
		}
		n, err := strconv.Atoi(k)
		if err != nil {
			return nil, fmt.Sprintf("pool key %q is not a ComfyUI device: this call holds the whole node", k)
		}
		c, ok := gpuprobe.CardByComfyOrder(cards, n)
		if !ok {
			return nil, fmt.Sprintf("pool key cuda:%d is in ComfyUI's device order, which this box has not declared (config gpu_comfy_order), so it cannot be turned into a card: this call holds the whole node", n)
		}
		if !seen[c.LeaseID()] {
			seen[c.LeaseID()] = true
			ids = append(ids, c.LeaseID())
		}
	}
	sort.Strings(ids)
	if !poolMayBeScoped(len(ids), len(cards)) {
		return nil, fmt.Sprintf("a pool of %d card(s) on a box with %d may not be leased as a set: this call holds the whole node", len(ids), len(cards))
	}
	return ids, ""
}

// poolMayBeScoped says whether a job that runs in the DEFAULT instance may lease only some
// cards. Two default-instance jobs must never run at once (one process, one port, one marker; a
// second would reuse the first's instance on the first's cards), and the lease is what keeps
// them apart, so the sets have to overlap whatever they are. Two sets of at least two cards on a
// box of at most three always overlap; on a larger box they need not, so the whole node is held.
func poolMayBeScoped(set, total int) bool { return set >= 2 && total <= 3 }

// mediaGrantEnv is the lease env a runner is handed. Devices (lease ids) are exported only for a
// card-scoped lease, so a whole-node call's env is what it always was.
func (p *Pipeline) mediaGrantEnv(lease *gpulease.Lease, ids []string, unload string) []string {
	env := []string{
		"GPU_LEASE_DIR=" + lease.Dir(),
		"GPU_LEASE_EPOCH=" + strconv.FormatUint(lease.Epoch(), 10),
		"GPU_LEASE_CLASS=" + string(lease.Class()),
	}
	if len(ids) > 0 {
		env = append(env, "GPU_LEASE_DEVICES="+strings.Join(ids, ","))
	}
	if unload != "" {
		env = append(env, unload)
	}
	return append(env, p.lockEnv()...)
}

// unloadEnv is the GPU_LEASE_UNLOAD_MODELS entry for a lease on ids: the models the runner's
// unload may take, which excludes the seats pinned to cards the lease does not hold. Without it
// the render lane unloads every model off the memory stack, and a render on one card empties the
// seats on the others (register C-86). "" (no roster, unreadable) leaves the lane on its own rule.
func (p *Pipeline) unloadEnv(ctx context.Context, ids []string) string {
	if len(ids) == 0 || strings.TrimSpace(p.cfg.Endpoint) == "" {
		return ""
	}
	roster, err := swapclient.FetchRoster(ctx, p.cfg.Endpoint, 5*time.Second)
	if err != nil {
		log.Printf("media admission: the llama-swap roster could not be read (%v): the render lane keeps its own rule and unloads every model off the memory stack", err)
		return ""
	}
	models, known := gpualloc.UnloadModels(ctx, p.cfg, roster.IDs(), ids, p.alloc, io.Discard)
	return gpualloc.UnloadEnv(models, known)
}

// instanceEndpoint is the endpoint of the ComfyUI instance bound to a card: the base port plus
// the card's nvidia-smi index (8189, 8190, 8191 on the 3-card box). Stable per card, because the
// render layer refuses to reuse an instance whose marker records another port.
func (p *Pipeline) instanceEndpoint(c gpuprobe.Card) string {
	if p.instanceAPI != nil {
		return p.instanceAPI(c)
	}
	return defaultInstanceAPI(c)
}

// instancePortBase is the first port of the per-card range (COMFY_PORT_BASE moves the range, as
// it does in the render layer).
const instancePortBase = 8189

func defaultInstanceAPI(c gpuprobe.Card) string {
	base := instancePortBase
	if v := strings.TrimSpace(os.Getenv("COMFY_PORT_BASE")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1024 && n <= 65535 {
			base = n
		}
	}
	return "http://127.0.0.1:" + strconv.Itoa(base+c.NvidiaIndex)
}

// stopKeptInstances stops the ComfyUI instances kept under a lease epoch (internal/comfyinst),
// before the lease is released: they live no longer than it does. A failure is logged and never
// blocks the release.
func (p *Pipeline) stopKeptInstances(epoch uint64) {
	stop := p.stopKept
	if stop == nil {
		stop = func(ctx context.Context, dir string, epoch uint64) []comfyinst.Outcome {
			return comfyinst.StopForLease(ctx, dir, epoch, comfyinst.RealDeps())
		}
	}
	if strings.TrimSpace(p.cfg.ComfyDir) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, o := range stop(ctx, p.cfg.ComfyDir, epoch) {
		if o.Stopped {
			log.Printf("media admission: stopped the ComfyUI instance %s (pid %d) kept under lease epoch %d", o.Key, o.PID, epoch)
		} else if o.Why != "" {
			log.Printf("media admission: the ComfyUI instance %s (pid %d) kept under lease epoch %d was not stopped: %s", o.Key, o.PID, epoch, o.Why)
		}
	}
}

// acquireMediaLease takes what one generation job needs of the GPU and returns the grant. See the
// file comment for what a call asks for; a call that needs the whole node, or runs on a host that
// cannot lease cards, takes the whole-node lease exactly as it always did (acquireWholeNode).
func (p *Pipeline) acquireMediaLease(ctx context.Context, reason string, ttl, wait time.Duration, need mediaNeed) (mediaGrant, error) {
	start := time.Now()
	if need.Kind != needWhole {
		if g, handled, err := p.acquireCardScoped(ctx, reason, ttl, wait, need); handled {
			return g, err
		}
	}
	// The whole-node path. A host that leases cards also keeps places in line for it: the place a
	// caller resumes is looked up BEFORE the wait, because the wait's own waiter consumes the token
	// and the answer below must still carry the arrival time it left with.
	var place *wholePlace
	if need.Token != "" {
		if m, err := p.scopedManager(); err == nil && m.CardScoped() {
			if tok, ok := m.ResumeToken(need.Token); ok {
				place = &wholePlace{m: m, tok: tok}
			}
		}
	}
	// The place a whole-node call resumed is kept held while it waits, as a single-card call's is
	// (keepPlace); the keeper is stopped before the place is spent or left again.
	stopKeep := func() {}
	if place != nil {
		stopKeep = keepPlace(place.m, place.tok.ID)
	}
	env, release, err := p.acquireWholeNode(ctx, reason, ttl, wait, need.Token)
	stopKeep()
	if err != nil {
		return mediaGrant{}, p.wholeNodeBusy(err, reason, ttl, start, place, need.Resumable)
	}
	if place != nil {
		place.m.DropToken(place.tok.ID) // served: the place is spent (a grant without a wait never consumed it)
	}
	return mediaGrant{Env: env, Release: release}, nil
}

// wholePlace is a place in line a whole-node call resumes.
type wholePlace struct {
	m   *gpulease.Manager
	tok gpulease.Token
}

// wholeNodeBusy turns the whole-node path's busy refusal into a place in line on a host that
// leases cards: the token replaces the refusal there, for a call that holds the whole node as for
// one that holds a card. A host that cannot lease cards (no flag, or no green reader audit) keeps
// the refusal byte for byte, and any other error is returned as it is.
func (p *Pipeline) wholeNodeBusy(err error, reason string, ttl time.Duration, start time.Time, place *wholePlace, resumable bool) error {
	var busy *errGPUBusy
	if !errors.As(err, &busy) && !errors.Is(err, gpulease.ErrStillQueued) {
		return err
	}
	if !resumable {
		// No door to hand a token back through: the refusal it always got.
		if busy != nil {
			return err
		}
		return &errGPUBusy{detail: err.Error()}
	}
	m, merr := p.scopedManager()
	if merr != nil || !m.CardScoped() {
		return err
	}
	since, tokenID := start, ""
	if place != nil {
		since, tokenID = place.tok.Since(), place.tok.ID
	}
	opts := gpulease.Options{Reason: reason, Origin: "pipeline", TTL: ttl, ResumeToken: tokenID}
	if qerr := p.queuedAnswer(m, nil, since, tokenID, opts, reason, nil, true); qerr != nil {
		return qerr
	}
	return err
}

// acquireCardScoped is the per-card path. handled is false when the call is not eligible for it
// (an ambient whole-node lease, a host that cannot write card-scoped leases, a plan that falls
// back to the whole node): the caller then takes the legacy path.
func (p *Pipeline) acquireCardScoped(ctx context.Context, reason string, ttl, wait time.Duration, need mediaNeed) (mediaGrant, bool, error) {
	inherited, ierr := ambientLeaseEnv()
	if ierr != nil {
		return mediaGrant{}, true, ierr
	}
	if inherited != nil {
		return p.acquireInherited(ctx, inherited, wait, need)
	}
	m, err := p.scopedManager()
	if err != nil || !m.CardScoped() {
		return mediaGrant{}, false, nil // the legacy path reports the open error, or runs as before
	}
	cards, _, terr := p.alloc.CardTable(ctx, p.cfg)
	plan, perr := planMedia(need, cards, terr)
	if perr != nil {
		return mediaGrant{}, true, perr
	}
	if plan.whole {
		return mediaGrant{}, false, nil
	}
	g, err := p.acquireCards(ctx, m, reason, ttl, wait, need, plan, cards)
	return g, true, err
}

// lookupCard finds the card with a lease id.
func lookupCard(cards []gpuprobe.Card, id string) (gpuprobe.Card, bool) {
	for _, c := range cards {
		if c.LeaseID() == id {
			return c, true
		}
	}
	return gpuprobe.Card{}, false
}

// queuedClaims lists the cards callers are queued for ahead of a new arrival: live waiters and
// the live tokens (a place held for a caller who may come back). They are not free for a newcomer,
// who queues behind them (FIFO). A queued whole-node request is a barrier for every card. ownToken
// is the caller's own place, never counted against it.
func queuedClaims(m *gpulease.Manager, cards []gpuprobe.Card, ownToken string) map[string]bool {
	out := map[string]bool{}
	add := func(devs []string) {
		if len(devs) == 0 {
			for _, c := range cards {
				out[c.LeaseID()] = true
			}
			return
		}
		for _, d := range devs {
			out[d] = true
		}
	}
	for _, w := range m.Waiters() {
		if ownToken != "" && w.Token == ownToken {
			continue
		}
		add(w.Devices)
	}
	for _, t := range m.Tokens() {
		if t.ID == ownToken || !m.TokenLive(t) {
			continue
		}
		add(t.Devices)
	}
	return out
}

// placeKeepEvery is how often a call that resumed a place in line re-asserts it while it waits in
// this process: a third of the grace, so one missed tick still leaves the place held. A var so a
// test can shorten it.
var placeKeepEvery = gpulease.TokenGrace / 3

// keepPlace re-asserts the place in line token id holds, every placeKeepEvery, until the returned
// func is called (which also JOINS the goroutine, so nothing touches the token after the caller
// moves on to spend, consume or re-leave it). A token's life is its last poll; a resumed call that
// then waits for the card (in this process, for its slot) polls nothing, so without this its place
// read as absent after the grace and later callers and other processes' waiters skipped it. It is
// one timer for the length of a wait that is already bounded by gpu_wait_ms, and it only ever
// refreshes a token that still exists (gpulease.TouchToken). An empty id keeps nothing.
func keepPlace(m *gpulease.Manager, id string) (stop func()) {
	if id == "" {
		return func() {}
	}
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(placeKeepEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				m.TouchToken(id)
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-stopped
		})
	}
}

// acquireCards holds a lease on the planned cards, allocating the card when the plan says so.
func (p *Pipeline) acquireCards(ctx context.Context, m *gpulease.Manager, reason string, ttl, wait time.Duration,
	need mediaNeed, plan mediaPlan, cards []gpuprobe.Card) (mediaGrant, error) {
	start := time.Now()
	deadline := start.Add(wait)
	remaining := func() time.Duration {
		if r := time.Until(deadline); r > 0 {
			return r
		}
		return 0
	}
	// A call that resumes a place keeps the arrival time it left with, and keeps the place itself
	// held while it waits (keepPlace). The keeper is stopped before the call spends the place
	// (grantCards), consumes it (the lease wait registers a waiter) or leaves it again.
	since, tokenID := start, ""
	stopKeep := func() {}
	if tok, ok := m.ResumeToken(need.Token); ok {
		since, tokenID = tok.Since(), tok.ID
		stopKeep = keepPlace(m, tok.ID)
	}
	defer stopKeep()
	optsFor := func(ids []string) gpulease.Options {
		return gpulease.Options{Reason: reason, Origin: "pipeline", TTL: ttl, Devices: ids, ResumeToken: tokenID}
	}
	build := func() (gpulease.AllocInput, error) {
		claimed := mediaSlots.held()
		for id := range queuedClaims(m, cards, tokenID) {
			claimed[id] = true
		}
		return gpualloc.BuildInput(ctx, m, p.cfg, gpualloc.Need{Claimed: claimed}, p.alloc)
	}

	var ids []string
	for lost := 0; ; lost++ {
		free := true
		if plan.auto {
			picked, isFree, err := gpualloc.PickAuto(gpualloc.Plan{Min: 1, Max: 1}, remaining(), build, io.Discard, time.Sleep, time.Now)
			if err != nil {
				var none *gpulease.NoCardsError
				if errors.As(err, &none) {
					return mediaGrant{}, p.noCardsAnswer(m, none, since, tokenID, optsFor, reason, need.Resumable)
				}
				return mediaGrant{}, err
			}
			ids, free = picked, isFree
		} else {
			ids = plan.ids
			in, err := build()
			if err != nil {
				return mediaGrant{}, err
			}
			free = !in.WholeNodeHeld
			for _, id := range ids {
				if in.Claimed[id] {
					free = false
				}
			}
		}
		if free {
			if !mediaSlots.tryTake(ids) {
				// A job in this process holds it and has no lease record to show: it is claimed.
				if plan.auto && lost < mediaClaimRetries {
					continue
				}
				free = false
			} else {
				lease, err := m.TryAcquire(gpulease.ClassMedia, optsFor(ids))
				if err == nil {
					stopKeep()
					return p.grantCards(ctx, m, lease, ids, plan, cards, tokenID), nil
				}
				mediaSlots.release(ids)
				var held *gpulease.ErrHeld
				if !errors.As(err, &held) {
					return mediaGrant{}, err
				}
				if plan.auto && lost < mediaClaimRetries {
					continue // another claimant took it first: allocate again with its claim visible
				}
				free = false
			}
		}
		break
	}

	// Nothing is free: take a place in line. The in-process slots first (bounded, FIFO among this
	// process's callers), then the lease queue, FIFO across processes, keeping the arrival time.
	if !mediaSlots.take(ids, remaining()) {
		return mediaGrant{}, p.queuedAnswer(m, ids, since, tokenID, optsFor(ids), reason, nil, need.Resumable)
	}
	if remaining() <= 0 {
		mediaSlots.release(ids)
		return mediaGrant{}, p.queuedAnswer(m, ids, since, tokenID, optsFor(ids), reason, nil, need.Resumable)
	}
	qo := optsFor(ids)
	qo.Wait, qo.WaitOut = remaining(), true
	stopKeep() // the waiter the lease wait registers stands for the place from here
	lease, err := m.Acquire(gpulease.ClassMedia, qo)
	if err != nil {
		mediaSlots.release(ids)
		var held *gpulease.ErrHeld
		if errors.As(err, &held) || errors.Is(err, gpulease.ErrStillQueued) {
			return mediaGrant{}, p.queuedAnswer(m, ids, since, tokenID, optsFor(ids), reason, held, need.Resumable)
		}
		return mediaGrant{}, err
	}
	return p.grantCards(ctx, m, lease, ids, plan, cards, tokenID), nil
}

// noCardsAnswer is the answer for a call that waited its window and the allocator still found no
// card for it although the cards may be idle: the host is short of RAM, a transient nvidia-smi
// failure left the monitor's card unknown, every card is the operator's screen or quarantined. It
// used to leave through the raw allocator error, classed as a lease fault
// (gpu_lease_unavailable). A call that can resume keeps a place on the cards that would qualify
// but for what is short (the allocator lists them as Waitable), with the reason; when no card
// could qualify however long it waited there is nothing to hold a place on, and every call gets
// the plain busy defer, with the reason.
func (p *Pipeline) noCardsAnswer(m *gpulease.Manager, none *gpulease.NoCardsError, since time.Time, tokenID string,
	optsFor func([]string) gpulease.Options, reason string, resumable bool) error {
	why := "no card can take this call right now: " + gpualloc.SkipSummary(none)
	ids := none.Waitable
	if !resumable || len(ids) == 0 {
		return &errGPUBusy{detail: why}
	}
	return p.queuedAnswerWhy(m, ids, since, tokenID, optsFor(ids), reason, nil, true, why)
}

// queuedAnswer leaves the place-keeping token for a call that waited its window and still has no
// card, and builds the answer that carries it. A call whose door cannot resume a place (resumable
// false) leaves nothing and gets the plain busy answer.
func (p *Pipeline) queuedAnswer(m *gpulease.Manager, ids []string, since time.Time, tokenID string, opts gpulease.Options, reason string, held *gpulease.ErrHeld, resumable bool) error {
	return p.queuedAnswerWhy(m, ids, since, tokenID, opts, reason, held, resumable, "")
}

// queuedAnswerWhy is queuedAnswer with the reason the call is queued stated by the caller, for a
// wait that no lease explains (why != "").
func (p *Pipeline) queuedAnswerWhy(m *gpulease.Manager, ids []string, since time.Time, tokenID string, opts gpulease.Options, reason string, held *gpulease.ErrHeld, resumable bool, why string) error {
	if !resumable {
		return busyAnswer(ids, held)
	}
	tok, err := m.LeaveToken(gpulease.ClassMedia, opts, since)
	if err != nil {
		// Without a place to keep, the honest answer is the old one: the card is busy.
		return &errGPUBusy{detail: fmt.Sprintf("the card(s) %s are held and a place in line could not be kept: %v", strings.Join(ids, ", "), err)}
	}
	e := &errGPUQueued{Token: tok.ID, Position: m.QueuePosition(ids, since, tok.ID), Devices: ids}
	now := time.Now()
	for _, l := range m.Leases() {
		// A request that names cards is in the way of, and blocked by, the leases on those cards;
		// a request for the whole node (no ids) is blocked by every lease, as conflict() in
		// mediaslots.go has it. A whole-node lease (no devices) blocks everyone.
		if len(ids) != 0 && len(l.Devices) != 0 && !intersects(l.Devices, ids) {
			continue
		}
		e.HeldBy = append(e.HeldBy, l.Epoch)
		if !l.ExpiresAt.IsZero() {
			if s := int(l.ExpiresAt.Sub(now).Round(time.Second) / time.Second); s > e.ETASec {
				e.ETASec = s
			}
		}
	}
	subject := "the whole node"
	if len(ids) != 0 {
		subject = "card(s) " + strings.Join(ids, ", ")
	}
	switch {
	case why != "":
		e.Why = why
	case held != nil:
		e.Why = fmt.Sprintf("%s held by %s (%q)", subject, held.Info.Class, held.Info.Reason)
	case len(e.HeldBy) > 0:
		e.Why = fmt.Sprintf("%s in use", subject)
	default:
		e.Why = fmt.Sprintf("%s promised to callers ahead of this one", subject)
	}
	return e
}

// busyAnswer is the refusal a call gets when it waited its window with no card and keeps no place:
// the holder when there is one, else the cards (or the node) it was waiting for.
func busyAnswer(ids []string, held *gpulease.ErrHeld) error {
	if held != nil {
		return &errGPUBusy{info: held.Info}
	}
	subject := "the whole node"
	if len(ids) != 0 {
		subject = "card(s) " + strings.Join(ids, ", ")
	}
	return &errGPUBusy{detail: subject + " are held, or promised to callers ahead of this one, and the wait ended before they freed"}
}

func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// grantCards builds the grant for a lease just taken (the slots for ids are already held).
func (p *Pipeline) grantCards(ctx context.Context, m *gpulease.Manager, lease *gpulease.Lease, ids []string, plan mediaPlan, cards []gpuprobe.Card, tokenID string) mediaGrant {
	if tokenID != "" {
		m.DropToken(tokenID) // the call is served: its place is spent
	}
	stopHB := startHeartbeat(lease)
	var once sync.Once
	release := func() {
		once.Do(func() {
			stopHB()
			// The instances kept under this lease go BEFORE it is released, so the next holder never
			// finds one on its card, and only while the lease is still this call's: a call whose lease
			// was taken away (released from outside, reclaimed after a suspend) is a straggler, and
			// the instance may already be the next lease's job. It leaves them, as Release leaves
			// the claim.
			if err := lease.Check(); err != nil {
				log.Printf("media admission: leaving the ComfyUI instances kept under lease epoch %d running: the lease is no longer this call's (%v)", lease.Epoch(), err)
			} else {
				p.stopKeptInstances(lease.Epoch())
			}
			_ = lease.Release()
			mediaSlots.release(ids)
		})
	}
	// The card is ours from here: the call's PAIR card turns running.
	core.MarkWorking(ctx)
	g := mediaGrant{Env: p.mediaGrantEnv(lease, ids, p.unloadEnv(ctx, ids)), Release: release}
	if plan.instance {
		if c, ok := lookupCard(cards, ids[0]); ok {
			g.Card, g.API = &c, p.instanceEndpoint(c)
		}
	}
	return g
}

// startHeartbeat renews a lease every 15 s while a render runs: at most ONE timer for the
// duration of GPU work that lasts minutes, nothing ticking while the harness is idle. The returned
// func stops it and JOINS: Release mutates the Lease a Renew already in flight is reading, and
// would let that Renew re-create the heartbeat file after the release swept it.
func startHeartbeat(lease *gpulease.Lease) func() {
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				_ = lease.Renew()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-stopped
		})
	}
}

// acquireInherited serves a call running under a lease its parent holds on some cards
// (`gpu reserve --devices ... -- <cmd>`): GPU_LEASE_DEVICES names them. The lease is not ours to
// renew or release; the in-process slots arbitrate which of its cards this call runs on, so two
// calls under a two-card lease run on both cards. handled is false when the lease holds the whole
// node, or the call cannot be placed on its cards (the legacy path then serves it).
func (p *Pipeline) acquireInherited(ctx context.Context, inherited []string, wait time.Duration, need mediaNeed) (mediaGrant, bool, error) {
	var held []string
	for _, id := range strings.Split(os.Getenv("GPU_LEASE_DEVICES"), ",") {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			held = append(held, id)
		}
	}
	if len(held) == 0 || need.Kind != needSingle {
		return mediaGrant{}, false, nil
	}
	cards, _, err := p.alloc.CardTable(ctx, p.cfg)
	if err != nil {
		return mediaGrant{}, false, nil
	}
	var cands []string
	for _, id := range held {
		if _, ok := lookupCard(cards, id); ok {
			cands = append(cands, id)
		}
	}
	if len(cands) == 0 {
		return mediaGrant{}, false, nil
	}
	if need.Pin != "" {
		// An explicit pin is the one card; it must be among the parent's, or the legacy path (and its
		// own --cuda-device) serves the call exactly as it did.
		plan, perr := planMedia(need, cards, nil)
		if perr != nil || plan.whole || len(plan.ids) != 1 || !containsString(cands, plan.ids[0]) {
			return mediaGrant{}, false, nil
		}
		cands = plan.ids
	}
	picked, ok := mediaSlots.takeAny(cands, wait)
	if !ok {
		return mediaGrant{}, true, &errGPUBusy{detail: fmt.Sprintf("every card of the lease this process runs under (%s) is in use by another job here after %s", strings.Join(cands, ", "), wait)}
	}
	core.MarkWorking(ctx)
	c, _ := lookupCard(cards, picked)
	var once sync.Once
	return mediaGrant{
		Env:     append(append([]string(nil), inherited...), append([]string{"GPU_LEASE_DEVICES=" + strings.Join(held, ",")}, p.lockEnv()...)...),
		Card:    &c,
		API:     p.instanceEndpoint(c),
		Release: func() { once.Do(func() { mediaSlots.release([]string{picked}) }) },
	}, true, nil
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
