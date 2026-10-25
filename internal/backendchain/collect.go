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
	// maxServingConfigBytes bounds the read of the serving config.
	maxServingConfigBytes = 4 << 20
)

// Options tune one collection.
type Options struct {
	// HTTP is the client every read goes over; the caller supplies the one its package already builds for
	// the configured llama-swap endpoint (a bare client here would be a new site for the ADR 0042 lint).
	// Required: without it nothing can be read and every entry is "none".
	HTTP *http.Client
	// ProbeCompletion adds a 1-token completion probe. It LOADS an idle seat, so it is off unless a
	// person asked for it (doctor --probe-completion); /fleet/health never sets it. A completion goes only
	// to a seat that is already running (it loads nothing), or, on a box where nothing is running, to the
	// one seat the run is allowed to load; a seat with no chat route is never sent one.
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
//
// Every row's starting state (loaded, not loaded, loading) comes from that one opening reading of the
// running list, the snapshot. Only the explicit completion probe reads the list again, immediately before
// each completion (see completionVerdict), and that later reading decides whether a completion may be sent;
// it never rewrites what a row says about the state the report began with.
//
// A seat has one engine kind wherever it is read. The kinds a lane's plan declares (the tier table's
// preferred and fallback seats) are registered first, and every candidate of every lane and every seat row
// is then classified by the same rule, engineOf.
func Collect(ctx context.Context, cfg config.Config, opt Options) Report {
	s := newState(ctx, cfg, opt)
	var rep Report
	lanes := PlanLanes(cfg)
	for _, lane := range lanes {
		for _, c := range lane.Chain {
			s.declare(c.Seat, c.Engine)
		}
	}
	for _, lane := range lanes {
		for i := range lane.Chain {
			lane.Chain[i].Engine = s.engineOf(lane.Chain[i].Seat)
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
	// start / startErr are the snapshot: llama-swap's running list as the report found it, read once and
	// never replaced. Every row's starting state comes from it.
	start    []seatload.Occupant
	startErr error
	// running / runErr are the live reading: the snapshot at first, read again immediately before each
	// completion is decided. They gate completions and feed nothing else.
	running []seatload.Occupant
	runErr  error
	// loadSent is the seat of the one completion this run sent to a seat that was not running ("" until
	// then): at most one such completion is sent, however it ends.
	loadSent string
	// declared maps a seat name (lower case) to the kind a lane's plan declares for it (the tier table).
	declared map[string]string

	entries    []servingtmpl.SeatEntry
	entriesWhy string // why entries is empty, "" when the serving config was read

	memo map[string]Verdict
}

func newState(ctx context.Context, cfg config.Config, opt Options) *state {
	s := &state{cfg: cfg, opt: opt, memo: map[string]Verdict{}, declared: map[string]string{}}
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
			s.start, s.startErr = s.running, s.runErr
		}
	}
	switch path := strings.TrimSpace(cfg.ServingConfigPath); {
	case path == "":
		s.entriesWhy = "serving_config_path is unset"
	default:
		text, tooBig, err := readBounded(path, maxServingConfigBytes)
		switch {
		case err != nil:
			s.entriesWhy = "the serving config cannot be read"
		case tooBig:
			s.entriesWhy = "the serving config is too large to read"
		default:
			if s.entries, err = servingtmpl.SeatEntries(string(text)); err != nil {
				s.entriesWhy = "the serving config is not parseable"
			}
		}
	}
	return s
}

var (
	errNoEndpoint = errors.New("no llama-swap endpoint is configured")
	errNoClient   = errors.New("no HTTP client was supplied")
)

// readBounded reads a file of at most max bytes. A longer file is not read past max+1 and is reported as
// too big, so a file of any size at the configured path cannot be pulled whole into memory.
func readBounded(path string, max int64) (text []byte, tooBig bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	text, err = io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(text)) > max {
		return nil, true, nil
	}
	return text, false, nil
}

// statusOf finds the HTTP status in a failed read of llama-swap or of a seat, whichever client made it.
func statusOf(err error) (int, bool) {
	var se *llamaclient.StatusError
	var re *seatload.StatusError
	switch {
	case errors.As(err, &se):
		return se.StatusCode, true
	case errors.As(err, &re):
		return re.Status, true
	}
	return swapclient.HTTPStatus(err)
}

// isTimeout reports whether a read failed because it ran out of time. The vendored llama-swap client
// flattens a transport failure into text, so the typed checks are backed by the words Go uses for one.
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "deadline exceeded") || strings.Contains(msg, "client.timeout") || strings.Contains(msg, "i/o timeout")
}

// short turns a failed read into one clause with no address, path or command in it: the status that
// answered, "timed out", "connection refused", or "no answer" when nothing answered and nothing more is known.
func short(err error) string {
	if status, ok := statusOf(err); ok {
		return fmt.Sprintf("HTTP %d", status)
	}
	switch {
	case isTimeout(err):
		return "timed out"
	case strings.Contains(strings.ToLower(err.Error()), "refused"):
		return "connection refused"
	}
	return "no answer"
}

// downDetail says why the roster is unavailable, in words that carry no address and say each thing once.
func (s *state) downDetail() string {
	switch {
	case errors.Is(s.rosterErr, errNoEndpoint), errors.Is(s.rosterErr, errNoClient):
		return s.rosterErr.Error()
	}
	switch why := short(s.rosterErr); {
	case why == "no answer":
		return "llama-swap is unreachable"
	case why == "timed out":
		return "llama-swap's roster read timed out"
	case strings.HasPrefix(why, "HTTP "):
		return "llama-swap's roster could not be read (" + why + ")"
	default:
		return "llama-swap is unreachable (" + why + ")"
	}
}

// pinned reports whether seat_endpoints sends every call that names the seat to a remote llama-swap. The
// key is the exact name a call uses, as the client resolves it.
func (s *state) pinned(seat string) bool {
	_, ok := s.cfg.SeatEndpoints[strings.TrimSpace(seat)]
	return ok
}

// declare records the kind a lane's plan declares for a seat.
func (s *state) declare(seat, kind string) {
	if kind = strings.TrimSpace(kind); kind == "" {
		return
	}
	for _, n := range s.names(seat) {
		s.declared[strings.ToLower(strings.TrimSpace(n))] = kind
	}
}

// declaredKind is the kind a lane's plan declares for the seat, under any name it answers to.
func (s *state) declaredKind(seat string) string {
	for _, n := range s.names(seat) {
		if k, ok := s.declared[strings.ToLower(strings.TrimSpace(n))]; ok {
			return k
		}
	}
	return ""
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

// engineOf classifies a seat, the one rule for a lane's candidate and for a seat's own row: a seat served
// by another node (seat_endpoints) is read there and is unknown here; else declared vLLM (vllm_seats,
// matched under every name the seat answers to); else the command llama-swap reports it spawned, when the
// seat is loaded and that command names an engine (it is what is true, where the file on disk may have
// changed since); else the command its serving-config entry runs; else the kind a lane's plan declares
// (the tier table); else unknown.
func (s *state) engineOf(seat string) string {
	if s.pinned(seat) {
		return EngineUnknown
	}
	f := SeatFacts{DeclaredVLLM: s.cfg.DeclaresVLLMSeat(seat), Declared: s.declaredKind(seat)}
	canon, _ := s.canonical(seat)
	if canon != "" && s.cfg.DeclaresVLLMSeat(canon) {
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
	if occ, ok := s.occupant(canon); ok && strings.TrimSpace(occ.Cmd) != "" {
		if k := ClassifyEngine(SeatFacts{DeclaredVLLM: f.DeclaredVLLM, Cmd: occ.Cmd}); k != EngineUnknown {
			return k
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

// occupantIn finds a row of a running list by canonical id.
func occupantIn(list []seatload.Occupant, canon string) (seatload.Occupant, bool) {
	for _, o := range list {
		if strings.EqualFold(strings.TrimSpace(o.Model), canon) {
			return o, true
		}
	}
	return seatload.Occupant{}, false
}

// occupant finds a seat's row in the opening snapshot: the state the report began with.
func (s *state) occupant(canon string) (seatload.Occupant, bool) { return occupantIn(s.start, canon) }

// Probe is the default cheap prober; it implements Prober. Results are kept per canonical seat, so a
// seat several lanes point at is asked once.
func (s *state) Probe(ctx context.Context, c Candidate) Verdict {
	canon, inRoster := s.canonical(c.Seat)
	key := strings.ToLower(canon) + "\x00" + kindOf(c)
	if v, ok := s.memo[key]; ok {
		return v
	}
	var v Verdict
	if s.pinned(c.Seat) {
		// The call goes to another node's llama-swap, which this node's roster never lists: not a seat
		// that is missing here, and not one this node can read.
		v = Verdict{OK: true, Probe: ProbeProcess, Detail: "served by another node (seat_endpoints pins it there); its engine is read on that node, not here"}
	} else {
		v = s.probe(ctx, c, canon, inRoster)
		if v.OK && kindOf(c) == EngineUnknown {
			v.Detail += " (engine kind unknown: " + s.unknownWhy(c.Seat) + ")"
		}
	}
	s.memo[key] = v
	return v
}

func (s *state) probe(ctx context.Context, c Candidate, canon string, inRoster bool) Verdict {
	if s.rosterErr != nil {
		return Verdict{Probe: ProbeProcess, Unknown: isTimeout(s.rosterErr), Detail: s.downDetail()}
	}
	if !inRoster {
		return Verdict{Probe: ProbeProcess, Detail: "not in the llama-swap roster"}
	}
	var v Verdict
	occ, found := s.occupant(canon)
	state := strings.ToLower(strings.TrimSpace(occ.State))
	switch {
	case s.startErr != nil:
		v = Verdict{OK: true, Probe: ProbeProcess, Detail: "in the roster; llama-swap's running list could not be read (" + short(s.startErr) + "), so its load state is unknown"}
	case !found || (state != "ready" && state != "starting" && state != "stopping"):
		v = Verdict{OK: true, Probe: ProbeProcess, Detail: "in the roster, not loaded; loads on first use, not asked (asking would load it)"}
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
		return Verdict{Probe: ProbeEndpoint, Unknown: isTimeout(err), Detail: "loaded, but its health endpoint did not answer (" + short(err) + ")"}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Verdict{Probe: ProbeEndpoint, Detail: fmt.Sprintf("loaded, but its health endpoint answered HTTP %d", resp.StatusCode)}
	}
	return Verdict{OK: true, Probe: ProbeEndpoint, Detail: fmt.Sprintf("loaded and ready; its health endpoint answered %d", resp.StatusCode)}
}

// isRunning reports whether a row of a running list is loaded or loading: a seat that is stopping is
// going away and the next request reloads it, so it is not running.
func isRunning(o seatload.Occupant) bool {
	st := strings.ToLower(strings.TrimSpace(o.State))
	return st == "ready" || st == "starting"
}

// resident reports whether llama-swap, on the live reading, has the seat loaded or loading, so that a
// completion to it loads nothing new.
func (s *state) resident(canon string) bool {
	o, ok := occupantIn(s.running, canon)
	return ok && isRunning(o)
}

// otherResident finds a seat other than canon that the live reading has loaded or loading, whatever its
// engine: the seat a load of canon can evict.
func (s *state) otherResident(canon string) (seatload.Occupant, bool) {
	for _, o := range s.running {
		if isRunning(o) && !strings.EqualFold(strings.TrimSpace(o.Model), canon) {
			return o, true
		}
	}
	return seatload.Occupant{}, false
}

// refreshRunning re-reads llama-swap's running list into the live reading (never the snapshot). A failed
// read leaves no list and the error, so every decision made on it afterwards fails closed (a seat holding
// the cards cannot be ruled out).
func (s *state) refreshRunning(ctx context.Context) {
	timeout := s.opt.ProbeTimeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	qctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	s.running, s.runErr = seatload.Occupants(qctx, s.opt.HTTP, swapclient.BaseURL(s.cfg.Endpoint))
}

// names lists every name a seat answers to: the name asked, the id llama-swap lists it under, and the
// serving-config entry's id and aliases.
func (s *state) names(seat string) []string {
	out := []string{strings.TrimSpace(seat)}
	if id, _ := s.canonical(seat); id != "" {
		out = append(out, id)
	}
	if e, ok := s.entryFor(seat); ok {
		out = append(out, e.Names()...)
	}
	return out
}

// answersTo reports whether any name the seat answers to is want.
func (s *state) answersTo(seat, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return false
	}
	for _, n := range s.names(seat) {
		if strings.EqualFold(strings.TrimSpace(n), want) {
			return true
		}
	}
	return false
}

// hasChatRoute reports whether a chat completion can be asked of the seat. A seat that serves speech to
// text, embeddings or reranking has no chat route: llama-swap answers a chat request for it with a 404,
// which says nothing about whether the seat is up. The roles come from what the box states, never from a
// guessed name: the seat is the configured embedder, speech-to-text or forecast seat, or its own command
// carries the flag that makes it one (the way the serving config's author wrote it), or it is a whisper
// or forecast server.
func (s *state) hasChatRoute(seat string) bool {
	for _, role := range []string{s.cfg.EmbedModel(), s.cfg.STTModel, s.cfg.STTModelHQ, s.cfg.ForecastModel} {
		if s.answersTo(seat, role) {
			return false
		}
	}
	if e, ok := s.entryFor(seat); ok && cmdHasNonChatFlag(e.Cmd) {
		return false
	}
	engine := s.engineOf(seat)
	return engine != EngineWhisperCpp && engine != EngineForecast
}

// completionVerdict runs the explicit 1-token completion probe on a seat the cheap probe passed.
//
// A completion to a seat that is not running LOADS it, and llama-swap unloads whatever else holds the
// cards to make room (a vLLM seat, or a llama.cpp seat the lane is bound to: internal/seatguard exists for
// the same reason). So a completion goes only
//
//   - to a seat that is already running: it loads nothing, so it can evict nothing; or
//   - to a seat that is not, when nothing else is running, and then to at most ONE such seat in the run,
//     however that completion ends.
//
// Any other seat is not probed, and its row says over which running seat the load would have been. A
// seat with no chat route (speech to text, an embedder) is never sent a chat completion: its row keeps
// the reading default mode gives it. Every seat is decided on a fresh reading of /running taken immediately
// before its completion, because the list stored from an earlier reading can be behind: a seat it still
// calls running may have stopped (another client unloaded it, or its idle ttl ran out), and a completion to
// it would then load it over whatever runs now. A seat the run itself loaded (the completion may load it
// and then fail) or one another client loaded meanwhile is likewise seen only on a fresh reading. A seat is
// completed as a running seat only if it is running on that reading. When the list cannot be read, whether
// a seat is running, or what holds the cards, cannot be ruled out, and no seat is sent a completion.
//
// A row says what the report found when it began (the snapshot); the readings taken here decide whether to
// send, and the one clause they may add is that a seat was loaded since the report began.
func (s *state) completionVerdict(ctx context.Context, c Candidate, canon string, prior Verdict) Verdict {
	if !s.hasChatRoute(c.Seat) {
		prior.Detail += "; not probed: not a chat seat"
		return prior
	}
	// The list stored from an earlier reading is not what is running now: read it again immediately before
	// this completion, whether or not the stored list calls the seat running.
	hadRunErr := s.runErr != nil
	s.refreshRunning(ctx)
	resident := s.resident(canon)
	began, inStart := s.occupant(canon)
	beganRunning := s.startErr == nil && inStart && isRunning(began)
	switch {
	case resident && !beganRunning:
		// Another client loaded it after the opening read: the cheap probe's "not loaded ... not asked" is stale.
		prior.Detail = "in the roster, loaded since the report began"
	case !resident && hadRunErr && s.runErr == nil:
		prior.Detail = "in the roster" // the earlier "load state is unknown" no longer holds
	}
	if !resident {
		if s.runErr != nil {
			prior.Detail += "; completion probe skipped: llama-swap's running list could not be read, so a seat holding the cards cannot be ruled out"
			return prior
		}
		if occ, ok := s.otherResident(canon); ok {
			prior.Detail += "; not probed: would load over " + seatLabel(s.engineOf(occ.Model), occ.Model)
			return prior
		}
		if s.loadSent != "" {
			prior.Detail += "; not probed: this run already sent one completion that could load a seat"
			return prior
		}
		s.loadSent = c.Seat
	}
	timeout := s.opt.CompletionTimeout
	if timeout <= 0 {
		timeout = defaultCompletionTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := llamaclient.New(s.cfg.Endpoint, s.cfg.CompletionPath, c.Seat, timeout)
	_, err := client.Generate(cctx, c.Seat, "", "ping", "", 1, 0, 0)
	if err != nil {
		return Verdict{Probe: ProbeCompletion, Unknown: isTimeout(err), Detail: "the 1-token completion failed (" + short(err) + ")"}
	}
	if !resident {
		// The cheap probe said "not asked"; now it was, so that clause goes.
		return Verdict{OK: true, Probe: ProbeCompletion, Detail: "in the roster, was not loaded; a 1-token completion loaded it and answered"}
	}
	return Verdict{OK: true, Probe: ProbeCompletion, Detail: prior.Detail + "; a 1-token completion answered"}
}
