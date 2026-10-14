package backendchain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/seatload"
	"github.com/dmmdea/offload-harness/internal/servingtmpl"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

const (
	defaultProbeTimeout      = 3 * time.Second
	defaultCompletionTimeout = 6 * time.Minute
)

// Options tune one collection.
type Options struct {
	// HTTP is the client every read goes over; the caller supplies the one its package already builds for
	// the configured llama-swap endpoint (a bare client here would be a new site for the ADR 0042 lint).
	// Required: without it nothing can be read and every entry is "none".
	HTTP *http.Client
	// ProbeCompletion adds a 1-token completion probe. It LOADS an idle seat, so it is off unless a
	// person asked for it (doctor --probe-completion); /fleet/health never sets it.
	ProbeCompletion bool
	// ProbeTimeout bounds each cheap read; zero means 3 s.
	ProbeTimeout time.Duration
	// CompletionTimeout bounds each completion probe; zero means 6 min (a cold vLLM load takes minutes).
	CompletionTimeout time.Duration
}

// Collect reads the node once and reports every lane and every seat.
//
// One observation backs the whole report: llama-swap's roster (GET /v1/models) and running list (GET
// /running) are read once, the serving config is read once, and a loaded seat is asked for its own
// health endpoint at most once however many lanes point at it. None of that loads a model: the seat's
// health endpoint is asked at the seat's own address (seatload.SeatURL), never through /upstream, which
// loads on demand and resets the idle timer. The default is therefore safe to run against a node that is
// serving, and to run on every /fleet/health refresh.
func Collect(ctx context.Context, cfg config.Config, opt Options) Report {
	s := newState(ctx, cfg, opt)
	var rep Report
	for _, lane := range PlanLanes(cfg) {
		for i := range lane.Chain {
			if strings.TrimSpace(lane.Chain[i].Engine) == "" {
				lane.Chain[i].Engine = s.engineOf(lane.Chain[i].Seat)
			}
		}
		rep.Lanes = append(rep.Lanes, Resolve(ctx, lane, s))
	}
	for _, id := range s.seatIDs() {
		rep.Seats = append(rep.Seats, Resolve(ctx, Lane{Chain: []Candidate{{Seat: id, Engine: s.engineOf(id), Bound: true}}}, s))
	}
	return rep
}

// state is one observation of the node.
type state struct {
	cfg config.Config
	opt Options

	roster    swapclient.Roster
	rosterErr error
	running   []seatload.Occupant
	runErr    error

	entries    []servingtmpl.SeatEntry
	entriesWhy string // why entries is empty, "" when the serving config was read

	memo map[string]Verdict
}

func newState(ctx context.Context, cfg config.Config, opt Options) *state {
	s := &state{cfg: cfg, opt: opt, memo: map[string]Verdict{}}
	timeout := opt.ProbeTimeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	switch {
	case strings.TrimSpace(cfg.Endpoint) == "":
		s.rosterErr = errNoEndpoint
	case opt.HTTP == nil:
		s.rosterErr = errNoClient
	default:
		rctx, cancel := context.WithTimeout(ctx, timeout)
		s.roster, s.rosterErr = swapclient.FetchRosterWith(rctx, opt.HTTP, cfg.Endpoint, timeout)
		cancel()
		if s.rosterErr == nil {
			qctx, qcancel := context.WithTimeout(ctx, timeout)
			s.running, s.runErr = seatload.Occupants(qctx, opt.HTTP, swapclient.BaseURL(cfg.Endpoint))
			qcancel()
		}
	}
	switch path := strings.TrimSpace(cfg.ServingConfigPath); {
	case path == "":
		s.entriesWhy = "serving_config_path is unset"
	default:
		text, err := os.ReadFile(path)
		if err != nil {
			s.entriesWhy = "the serving config cannot be read"
			break
		}
		if s.entries, err = servingtmpl.SeatEntries(string(text)); err != nil {
			s.entriesWhy = "the serving config is not parseable"
		}
	}
	return s
}

var (
	errNoEndpoint = errors.New("no llama-swap endpoint is configured")
	errNoClient   = errors.New("no HTTP client was supplied")
)

// short turns a transport error into one clause with no address, path or command in it.
func short(err error) string {
	var se *llamaclient.StatusError
	var ne net.Error
	switch {
	case errors.As(err, &se):
		return fmt.Sprintf("HTTP %d", se.StatusCode)
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timed out"
	case strings.Contains(strings.ToLower(err.Error()), "refused"):
		return "connection refused"
	}
	return "unreachable"
}

// downDetail says why the roster is unavailable, in words that carry no address.
func (s *state) downDetail() string {
	switch {
	case errors.Is(s.rosterErr, errNoEndpoint), errors.Is(s.rosterErr, errNoClient):
		return s.rosterErr.Error()
	}
	return "llama-swap is unreachable (" + short(s.rosterErr) + ")"
}

// entryFor finds the serving-config entry that answers to name (an id or an alias).
func (s *state) entryFor(name string) (servingtmpl.SeatEntry, bool) {
	for _, e := range s.entries {
		if e.Answers(name) {
			return e, true
		}
	}
	return servingtmpl.SeatEntry{}, false
}

// canonical resolves a seat name to the id llama-swap lists it under; the name itself when the roster
// cannot say.
func (s *state) canonical(name string) (string, bool) {
	if s.rosterErr == nil {
		if id, ok := s.roster.Canonical(name); ok {
			return id, true
		}
		return strings.TrimSpace(name), false
	}
	if e, ok := s.entryFor(name); ok {
		return e.ID, false
	}
	return strings.TrimSpace(name), false
}

// engineOf classifies a seat: declared vLLM (vllm_seats, matched under every name the seat answers to),
// else the command its serving-config entry runs.
func (s *state) engineOf(seat string) string {
	f := SeatFacts{DeclaredVLLM: s.cfg.DeclaresVLLMSeat(seat)}
	if id, ok := s.canonical(seat); ok && s.cfg.DeclaresVLLMSeat(id) {
		f.DeclaredVLLM = true
	}
	if e, ok := s.entryFor(seat); ok {
		f.Cmd = e.Cmd
		for _, n := range e.Names() {
			if s.cfg.DeclaresVLLMSeat(n) {
				f.DeclaredVLLM = true
			}
		}
	}
	return ClassifyEngine(f)
}

// unknownWhy says why a seat's engine could not be named.
func (s *state) unknownWhy(seat string) string {
	if s.entriesWhy != "" {
		return s.entriesWhy
	}
	if _, ok := s.entryFor(seat); !ok {
		return "the serving config has no entry for it"
	}
	return "its command names no engine the table knows"
}

// seatIDs lists every seat to report: the serving config's entries, then any roster id it does not
// carry, each once, in a stable order.
func (s *state) seatIDs() []string {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if k := strings.ToLower(strings.TrimSpace(id)); k != "" && !seen[k] {
			seen[k] = true
			ids = append(ids, strings.TrimSpace(id))
		}
	}
	for _, e := range s.entries {
		add(e.ID)
	}
	var extra []string
	if s.rosterErr == nil {
		for _, id := range s.roster.IDs() {
			if !seen[strings.ToLower(strings.TrimSpace(id))] {
				extra = append(extra, id)
			}
		}
	}
	sort.Strings(extra)
	for _, id := range extra {
		add(id)
	}
	return ids
}

// occupant finds a running row by canonical id.
func (s *state) occupant(canon string) (seatload.Occupant, bool) {
	for _, o := range s.running {
		if strings.EqualFold(strings.TrimSpace(o.Model), canon) {
			return o, true
		}
	}
	return seatload.Occupant{}, false
}

// Probe is the default cheap prober; it implements Prober. Results are kept per canonical seat, so a
// seat several lanes point at is asked once.
func (s *state) Probe(ctx context.Context, c Candidate) Verdict {
	canon, inRoster := s.canonical(c.Seat)
	key := strings.ToLower(canon)
	if v, ok := s.memo[key]; ok {
		return v
	}
	v := s.probe(ctx, c, canon, inRoster)
	if v.OK && (strings.TrimSpace(c.Engine) == "" || c.Engine == EngineUnknown) {
		v.Detail += " (engine kind unknown: " + s.unknownWhy(c.Seat) + ")"
	}
	s.memo[key] = v
	return v
}

func (s *state) probe(ctx context.Context, c Candidate, canon string, inRoster bool) Verdict {
	if s.rosterErr != nil {
		return Verdict{Probe: ProbeProcess, Detail: s.downDetail()}
	}
	if !inRoster {
		return Verdict{Probe: ProbeProcess, Detail: "not in the llama-swap roster"}
	}
	var v Verdict
	occ, found := s.occupant(canon)
	state := strings.ToLower(strings.TrimSpace(occ.State))
	switch {
	case s.runErr != nil:
		v = Verdict{OK: true, Probe: ProbeProcess, Detail: "in the roster; llama-swap's running list could not be read (" + short(s.runErr) + "), so its load state is unknown"}
	case !found || (state != "ready" && state != "starting" && state != "stopping"):
		v = Verdict{OK: true, Probe: ProbeProcess, Detail: "in the roster, not loaded; llama-swap loads it on the first request (not asked further: a probe would load it)"}
	case state == "starting":
		v = Verdict{OK: true, Probe: ProbeProcess, Detail: "loading (llama-swap state starting)"}
	case state == "stopping":
		v = Verdict{OK: true, Probe: ProbeProcess, Detail: "unloading after its idle ttl; llama-swap reloads it on the next request"}
	default:
		v = s.endpointVerdict(ctx, canon, occ)
	}
	if v.OK && s.opt.ProbeCompletion {
		v = s.completionVerdict(ctx, c, canon, v)
	}
	return v
}

// endpointVerdict asks a LOADED seat for its own health endpoint, at the seat's own address (never
// through llama-swap's /upstream path, which would reset its idle timer).
func (s *state) endpointVerdict(ctx context.Context, canon string, occ seatload.Occupant) Verdict {
	path := "/health"
	if e, ok := s.entryFor(canon); ok {
		switch c := strings.TrimSpace(e.CheckEndpoint); {
		case strings.EqualFold(c, "none"):
			return Verdict{OK: true, Probe: ProbeProcess, Detail: "loaded and ready (its health endpoint is switched off in the serving config)"}
		case c != "":
			path = "/" + strings.TrimLeft(c, "/")
		}
	}
	base, err := seatload.SeatURL(s.cfg.Endpoint, occ.Proxy)
	if err != nil {
		return Verdict{OK: true, Probe: ProbeProcess, Detail: "loaded and ready; llama-swap does not report its own address, so its health endpoint was not asked"}
	}
	timeout := s.opt.ProbeTimeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, base+path, nil)
	if err != nil {
		return Verdict{OK: true, Probe: ProbeProcess, Detail: "loaded and ready; its health endpoint address could not be built"}
	}
	resp, err := s.opt.HTTP.Do(req)
	if err != nil {
		return Verdict{Probe: ProbeEndpoint, Detail: "loaded, but its health endpoint did not answer (" + short(err) + ")"}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Verdict{Probe: ProbeEndpoint, Detail: fmt.Sprintf("loaded, but its health endpoint answered %d", resp.StatusCode)}
	}
	return Verdict{OK: true, Probe: ProbeEndpoint, Detail: fmt.Sprintf("loaded and ready; its health endpoint answered %d", resp.StatusCode)}
}

// completionVerdict runs the explicit 1-token completion probe on a seat the cheap probe passed. A
// completion on an idle seat loads it, and while a vLLM seat holds the cards that can evict it
// (internal/seatguard exists for the same reason), so the probe is skipped, and says why, whenever it
// would load a seat while a vLLM seat is resident or while that cannot be ruled out.
func (s *state) completionVerdict(ctx context.Context, c Candidate, canon string, prior Verdict) Verdict {
	if occ, loaded := s.occupant(canon); !loaded || strings.EqualFold(occ.State, "stopping") {
		if s.runErr != nil {
			prior.Detail += "; completion probe skipped: llama-swap's running list could not be read, so a vLLM seat holding the cards cannot be ruled out"
			return prior
		}
		if s.engineOf(canon) != EngineVLLM {
			for _, o := range s.running {
				st := strings.ToLower(o.State)
				if (st == "ready" || st == "starting") && !strings.EqualFold(o.Model, canon) && s.engineOf(o.Model) == EngineVLLM {
					prior.Detail += fmt.Sprintf("; completion probe skipped: it would load %s while the vLLM seat %s holds the cards", c.Seat, o.Model)
					return prior
				}
			}
		}
	}
	timeout := s.opt.CompletionTimeout
	if timeout <= 0 {
		timeout = defaultCompletionTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := llamaclient.New(s.cfg.Endpoint, s.cfg.CompletionPath, c.Seat, timeout)
	if _, err := client.Generate(cctx, c.Seat, "", "ping", "", 1, 0, 0); err != nil {
		return Verdict{Probe: ProbeCompletion, Detail: "the 1-token completion failed (" + short(err) + ")"}
	}
	return Verdict{OK: true, Probe: ProbeCompletion, Detail: prior.Detail + "; a 1-token completion answered"}
}
