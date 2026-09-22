package pairworkloads

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/seatinflight"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// Seat activity (0.133.0): traffic that reaches this box's vLLM seats WITHOUT
// the harness — a curl soak, opencode's own chat model, codex pointed at
// llama-swap — shows in PAIR's Jobs list as one card per busy stretch of a seat.
//
// The harness's own requests already have cards (delegations, tool calls), so
// the watcher counts only what the seat serves BEYOND them: the seat's live
// request count (vLLM's num_requests_running + num_requests_waiting) minus the
// harness requests in flight on it (internal/seatinflight, written by every
// on-box admission and by the fleet chat lane). A harness job is never shown
// twice.
//
// vLLM seats only: their /metrics count is exact, and the llama.cpp seats on
// these boxes are the cascade rungs and the mem0 embedder, whose direct callers
// would turn the Jobs list into noise.

const (
	// seatPollInterval is how often the seats are read. Two polls in a row
	// open or close a card (seatDebounce), so a stretch shows within ~4 s.
	seatPollInterval = 2 * time.Second
	// seatDebounce is the number of consecutive polls that must agree before
	// a card opens or closes. The seat count and the marker register are two
	// reads; a harness request that ends between them reads as direct for one
	// poll, and a one-poll blip must never become a card.
	seatDebounce = 2
	// seatUnreadableLimit closes an open card as failed when the seat's
	// metrics stay unreadable this many polls in a row (~1 min): the seat died
	// under its callers without llama-swap noticing yet.
	seatUnreadableLimit = 30
	seatHTTPTimeout     = 2 * time.Second
	seatRosterTTL       = 10 * time.Minute
	// SeatRequester is the requesterId a seat-activity card carries.
	SeatRequester = "llama-swap/direct"
)

// SeatWatchConfig is the emitter configuration for the seat watcher: the same
// ingress, enabled by its own key (pair_seat_activity_enabled). It is separate
// from pair_workloads_enabled on purpose — that key belongs on delegator boxes,
// while seat activity belongs on every box that SERVES a vLLM seat.
func SeatWatchConfig(cfg config.Config) Config {
	c := FromConfig(cfg)
	c.Enabled = cfg.PairSeatActivityEnabled
	return c
}

// seatEpisode is one open card: a stretch during which the seat served
// direct traffic.
type seatEpisode struct {
	id         string
	startedAt  int64 // ms
	lastBusyAt int64 // ms
	idle       int   // consecutive polls with no direct traffic
	unreadable int   // consecutive polls with the metrics unreadable
}

// seatState is what the watcher remembers per seat between polls.
type seatState struct {
	busyStreak int
	firstBusy  int64 // ms, the first poll of the current busy streak
	open       *seatEpisode
}

// SeatWatcher polls this box's vLLM seats and reports direct traffic to PAIR.
type SeatWatcher struct {
	em        *Emitter
	swapBase  string
	vllmSeats []string
	interval  time.Duration
	client    *http.Client

	// Seams (tests): the harness's own in-flight requests per lower-cased
	// request model name, the llama-swap roster, and the clock.
	harness     func() map[string]int
	fetchRoster func(ctx context.Context, endpoint string, timeout time.Duration) (swapclient.Roster, error)
	now         func() time.Time

	mu       sync.Mutex
	seats    map[string]*seatState // canonical seat id -> state
	roster   swapclient.Roster
	rosterOK bool
	rosterAt time.Time
}

// NewSeatWatcher builds a watcher over the llama-swap cfg.Endpoint names.
func NewSeatWatcher(em *Emitter, cfg config.Config) *SeatWatcher {
	return &SeatWatcher{
		em:          em,
		swapBase:    swapclient.BaseURL(cfg.Endpoint),
		vllmSeats:   append([]string(nil), cfg.VLLMSeats...),
		interval:    seatPollInterval,
		client:      &http.Client{Timeout: seatHTTPTimeout},
		harness:     seatinflight.CountLive,
		fetchRoster: swapclient.FetchRoster,
		now:         time.Now,
		seats:       map[string]*seatState{},
	}
}

// Run polls until ctx ends, then closes every open card and waits for the
// frames to leave. It returns at once when the emitter is disabled.
func (w *SeatWatcher) Run(ctx context.Context) {
	if !w.em.Enabled() || len(w.vllmSeats) == 0 {
		return
	}
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		w.Poll(ctx)
		select {
		case <-ctx.Done():
			w.closeAll()
			w.em.Wait()
			return
		case <-t.C:
		}
	}
}

// Poll reads the seats once and emits whatever changed.
func (w *SeatWatcher) Poll(ctx context.Context) {
	running, err := w.readRunning(ctx)
	if err != nil {
		// llama-swap itself unreadable: nothing is known, nothing changes.
		return
	}
	harness := w.harnessBySeat(ctx)
	now := w.now().UnixMilli()

	w.mu.Lock()
	defer w.mu.Unlock()
	seen := map[string]bool{}
	for _, seat := range running {
		seen[seat] = true
		st := w.seats[seat]
		if st == nil {
			st = &seatState{}
			w.seats[seat] = st
		}
		total, ok := w.readLoad(ctx, seat)
		if !ok {
			st.busyStreak = 0
			if st.open != nil {
				st.open.unreadable++
				if st.open.unreadable >= seatUnreadableLimit {
					w.finish(seat, st, "failed", "seat metrics unreachable for a minute", now)
				}
			}
			continue
		}
		direct := total - harness[strings.ToLower(seat)]
		if direct > 0 {
			if st.busyStreak == 0 {
				st.firstBusy = now
			}
			st.busyStreak++
			if st.open == nil && st.busyStreak >= seatDebounce {
				st.open = &seatEpisode{id: fmt.Sprintf("seat-%s-%d", seat, st.firstBusy), startedAt: st.firstBusy, lastBusyAt: now}
				w.emit(seat, st.open, "running", "", 0)
			} else if st.open != nil {
				st.open.lastBusyAt, st.open.idle, st.open.unreadable = now, 0, 0
			}
			continue
		}
		st.busyStreak = 0
		if st.open != nil {
			st.open.unreadable = 0
			st.open.idle++
			if st.open.idle >= seatDebounce {
				w.finish(seat, st, "completed", "", st.open.lastBusyAt)
			}
		}
	}
	// A seat that left the running list with a card open stopped serving its
	// callers: llama-swap only unloads an idle seat (ttl), so this is an exit.
	for seat, st := range w.seats {
		if seen[seat] {
			continue
		}
		if st.open != nil {
			w.finish(seat, st, "failed", "seat exited while serving direct traffic", now)
		}
		delete(w.seats, seat)
	}
}

// finish emits the terminal frame of the seat's open card and forgets it.
// Caller holds mu.
func (w *SeatWatcher) finish(seat string, st *seatState, state, reason string, at int64) {
	w.emit(seat, st.open, state, reason, at)
	st.open = nil
	st.busyStreak = 0
}

// closeAll completes every open card at its last busy poll (shutdown).
func (w *SeatWatcher) closeAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for seat, st := range w.seats {
		if st.open != nil {
			w.finish(seat, st, "completed", "", st.open.lastBusyAt)
		}
	}
}

func (w *SeatWatcher) emit(seat string, ep *seatEpisode, state, reason string, completedAt int64) {
	w.em.Emit(Event{
		JobID:       ep.id,
		Model:       seat,
		Engine:      "vllm",
		State:       state,
		Error:       reason,
		Requester:   SeatRequester,
		CreatedAt:   ep.startedAt,
		StartedAt:   ep.startedAt,
		CompletedAt: completedAt,
	})
}

// readRunning lists the canonical ids of the declared vLLM seats llama-swap
// reports ready.
func (w *SeatWatcher) readRunning(ctx context.Context) ([]string, error) {
	body, err := w.get(ctx, w.swapBase+"/running")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("pairworkloads: /running: %w", err)
	}
	var out []string
	for _, m := range doc.Running {
		if m.State == "ready" && declaresVLLM(w.vllmSeats, m.Model) {
			out = append(out, m.Model)
		}
	}
	return out, nil
}

// readLoad is the seat's live request count: running plus waiting. ok is
// false when the metrics cannot be read or carry neither gauge.
func (w *SeatWatcher) readLoad(ctx context.Context, seat string) (int, bool) {
	body, err := w.get(ctx, w.swapBase+"/upstream/"+url.PathEscape(seat)+"/metrics")
	if err != nil {
		return 0, false
	}
	return parseVLLMLoad(body)
}

// parseVLLMLoad sums vllm:num_requests_running and vllm:num_requests_waiting
// across every engine label set. The _by_reason breakdown of waiting is a
// different metric and is not added again.
func parseVLLMLoad(body []byte) (int, bool) {
	total, found := 0.0, false
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		name := line
		if i := strings.IndexAny(line, "{ "); i >= 0 {
			name = line[:i]
		}
		if name != "vllm:num_requests_running" && name != "vllm:num_requests_waiting" {
			continue
		}
		fields := strings.Fields(line[strings.LastIndex(line, "}")+1:])
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		total += v
		found = true
	}
	return int(total + 0.5), found
}

// harnessBySeat folds the harness's in-flight markers onto canonical seat ids
// (lower-cased): a marker carries the name the request used, which may be an
// alias (`agent-pool`). An unreadable roster leaves the names as they are.
func (w *SeatWatcher) harnessBySeat(ctx context.Context) map[string]int {
	raw := w.harness()
	if len(raw) == 0 {
		return raw
	}
	roster, ok := w.rosterFor(ctx)
	out := make(map[string]int, len(raw))
	for name, n := range raw {
		key := name
		if ok {
			if c, hit := roster.Canonical(name); hit && c != "" {
				key = strings.ToLower(c)
			}
		}
		out[key] += n
	}
	return out
}

func (w *SeatWatcher) rosterFor(ctx context.Context) (swapclient.Roster, bool) {
	w.mu.Lock()
	if w.rosterOK && w.now().Sub(w.rosterAt) < seatRosterTTL {
		r := w.roster
		w.mu.Unlock()
		return r, true
	}
	w.mu.Unlock()
	if w.fetchRoster == nil {
		return swapclient.Roster{}, false
	}
	r, err := w.fetchRoster(ctx, w.swapBase, seatHTTPTimeout)
	if err != nil {
		return swapclient.Roster{}, false
	}
	w.mu.Lock()
	w.roster, w.rosterOK, w.rosterAt = r, true, w.now()
	w.mu.Unlock()
	return r, true
}

func (w *SeatWatcher) get(ctx context.Context, u string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, seatHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pairworkloads: %s answered %d", u, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// LogSeatWatch prints the one startup line fleet-serve shows when it is on.
func LogSeatWatch(cfg config.Config) {
	log.Printf("fleet-serve: PAIR seat activity on (vLLM seats %v behind %s; direct traffic only — the harness's own requests are subtracted)", cfg.VLLMSeats, swapclient.BaseURL(cfg.Endpoint))
}
