package pairworkloads

// The PAIR card relay (PAIR routing fixes, D26): cards for a box that is not a PAIR cluster member.
//
// A box with no PAIR identity (no node-id.json it can read, no node-info and ingress on loopback: a
// view-only box, a thin client built by `install client` where PAIR is not installed) could never
// card its own work, and the operator's desktop showed that box's view-only card idle while its
// leases, CLI calls and served jobs stayed invisible. The relay closes that gap with the same frames:
//
//   - MEMBER side. A fleet-serve whose own emitter has a local identity advertises `pair_relay` in
//     /fleet/health and serves the token-gated route POST /fleet/pair-relay (internal/fleetnode).
//     ParseRelay turns one relayed frame into an Event for the member's OWN emitter: the id is
//     namespaced per asker, originatedFrom is never taken from the body (the member's emitter stamps
//     itself, as on every frame it posts), the requester names the asker, and scheduledOn is
//     resolved here, by the member's resolver (members.json, then view-only-nodes.json), from a node
//     name hint the relay supplies beside the frame. The member's emitter posts it, so the member's
//     orphan register covers the in-flight card: a relayed marker carries no local pid (its producer
//     is on another box) and closes only by its terminal relayed frame or the age cap.
//   - RELAYING side. An emitter with no local identity (Config.Relay; mode "relay" below) posts its
//     frames to the first healthy member instead of PAIR's loopback ingress, with the fleet bearer
//     and X-Offload-Asker, and records the relay's route URL as each marker's endpoint, so the
//     relaying box's own sweeper closes only the cards it opened through a relay.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/netguard"
)

const (
	// RelayPath is the member's route.
	RelayPath = "/fleet/pair-relay"
	// RelayBodyMax caps one relayed frame. A frame is a few hundred bytes; the cap only stops a
	// token holder from making the member read megabytes per call.
	RelayBodyMax = 64 << 10
	// RelayOpenMaxAge is the age past which a RELAYED in-flight marker is closed by the member's
	// sweep. The member cannot see whether the producer's process is alive (it is on another
	// box), so a relayed card closes by its terminal frame or by this cap, never by a pid probe.
	// It is the register's own leak cap: a lease card legitimately runs for hours.
	RelayOpenMaxAge = OpenMaxAge

	relayIDMax         = 160 // bound on the namespaced id a member posts
	relayFieldMax      = 128 // bound on model / requester text from a relay
	relayAliasesMax    = 8
	relayProbeTTL      = time.Minute      // a healthy verdict is trusted this long
	relayProbeFailTTL  = 30 * time.Second // a failed probe is retried after this
	relayProbeTimeout  = time.Second
	relayDemoteFor     = 30 * time.Second // a relay that failed a post waits this long behind the others
	relayPinsMax       = 4096
	relayHealthMax     = 1 << 20
	relayRequester     = "offload-harness/fleet:"
	relayRequesterRoot = "offload-harness"
)

// Modes an emitter reports (Emitter.Mode).
const (
	ModeLocal    = "local ingress"
	ModeNodeInfo = "node-info fallback"
	ModeRelay    = "relay"
	ModeOff      = "off"
)

// RelayConfig is the relaying side's configuration (FromConfig builds it from `pair_workloads_relay`
// and `delegate_remotes`). The zero value is no relay.
type RelayConfig struct {
	// Bases are explicit member fleet-serve base URLs (pair_workloads_relay entries).
	Bases []string
	// Auto adds every delegate_remotes base whose /fleet/health advertises `pair_relay`.
	Auto bool
	// Remotes are the delegate_remotes bases Auto considers.
	Remotes []string
	// Token is the fleet bearer the member's route requires.
	Token string
}

// RelayFromConfig reads `pair_workloads_relay`: absent or empty is auto, "auto" is auto, "off"
// turns the relay off, and any other entry is an explicit member base URL (a box with no
// delegate_remotes sets this by hand). "off" wins over every other entry.
func RelayFromConfig(cfg config.Config) RelayConfig {
	rc := RelayConfig{Token: cfg.FleetAuthToken}
	entries := cfg.PairWorkloadsRelay
	if len(entries) == 0 {
		rc.Auto = true
	}
	for _, raw := range entries {
		switch v := strings.TrimSpace(raw); strings.ToLower(v) {
		case "":
		case "off":
			return RelayConfig{}
		case "auto":
			rc.Auto = true
		default:
			rc.Bases = append(rc.Bases, v)
		}
	}
	if rc.Auto {
		for _, r := range cfg.DelegateRemotes {
			if r = strings.TrimSpace(r); r != "" {
				rc.Remotes = append(rc.Remotes, r)
			}
		}
	}
	return rc
}

func (r RelayConfig) configured() bool {
	return len(r.Bases) > 0 || (r.Auto && len(r.Remotes) > 0)
}

// signature identifies a relay configuration in a cache key.
func (r RelayConfig) signature() string {
	return strings.Join([]string{boolKey(r.Auto), strings.Join(r.Bases, ","), strings.Join(r.Remotes, ","), boolKey(r.Token != "")}, "|")
}

// relayURL is a base's route URL.
func relayURL(base string) string {
	return strings.TrimRight(strings.TrimSpace(base), "/") + RelayPath
}

// relayMeta is what a relay marker keeps beside the workloadInfo so a sweep can rebuild the relay
// body the member needs to place the card.
type relayMeta struct {
	Node    string   `json:"node"`
	Aliases []string `json:"node_aliases,omitempty"`
}

type relayVerdict struct {
	ok bool
	at time.Time
}

// relayState is the relaying side's mutable state, guarded by its own lock: probing a remote member
// must never run under the identity lock the node-info probes already hold.
type relayState struct {
	mu       sync.Mutex
	verdicts map[string]relayVerdict // base -> last health verdict
	demoted  map[string]time.Time    // route URL -> when a post to it last failed
	pinned   map[string]string       // job id -> the route URL its in-flight frames went to
	// probe reads a member's health: does it advertise pair_relay. probeRelayHealth unless a test
	// swaps it.
	probe  func(ctx context.Context, base, token string) (bool, error)
	client *http.Client
}

// newRelayClient is the client every relay request rides: netguard.SafeTransport, because a relay
// is a tailnet host (never-cloud, ADR 0001), enforced at dial time.
func newRelayClient() *http.Client {
	return &http.Client{Transport: netguard.SafeTransport(nil), Timeout: sendTimeout}
}

// probeRelayHealth is the default probe: GET <base>/fleet/health, decode the pair_relay flag.
func (e *Emitter) probeRelayHealth(ctx context.Context, base, token string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, relayProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(strings.TrimSpace(base), "/")+"/fleet/health", nil)
	if err != nil {
		return false, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.relay.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("health answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, relayHealthMax))
	if err != nil {
		return false, err
	}
	var h struct {
		PairRelay bool `json:"pair_relay"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		return false, err
	}
	return h.PairRelay, nil
}

// autoAdvertising returns the Remotes whose health advertises pair_relay, probing the ones whose
// verdict is stale, in parallel, outside every lock a caller of Enabled may hold.
func (e *Emitter) autoAdvertising() []string {
	rc := e.cfg.Relay
	if !rc.Auto || len(rc.Remotes) == 0 {
		return nil
	}
	now := time.Now()
	var stale []string
	e.relay.mu.Lock()
	for _, b := range rc.Remotes {
		v, ok := e.relay.verdicts[b]
		ttl := relayProbeFailTTL
		if ok && v.ok {
			ttl = relayProbeTTL
		}
		if !ok || now.Sub(v.at) >= ttl {
			stale = append(stale, b)
		}
	}
	e.relay.mu.Unlock()
	if len(stale) > 0 {
		answers := make([]bool, len(stale))
		var wg sync.WaitGroup
		for i, b := range stale {
			wg.Add(1)
			go func(i int, b string) {
				defer wg.Done()
				ok, err := e.relay.probe(context.Background(), b, rc.Token)
				answers[i] = ok && err == nil
			}(i, b)
		}
		wg.Wait()
		at := time.Now()
		e.relay.mu.Lock()
		if e.relay.verdicts == nil {
			e.relay.verdicts = map[string]relayVerdict{}
		}
		for i, b := range stale {
			e.relay.verdicts[b] = relayVerdict{ok: answers[i], at: at}
		}
		e.relay.mu.Unlock()
	}
	var out []string
	e.relay.mu.Lock()
	for _, b := range rc.Remotes {
		if v := e.relay.verdicts[b]; v.ok {
			out = append(out, b)
		}
	}
	e.relay.mu.Unlock()
	return out
}

// relayCandidates is the route URLs frames may go to, in order: the explicit bases, then the auto
// remotes whose health advertises pair_relay. Empty = no relay.
func (e *Emitter) relayCandidates() []string {
	rc := e.cfg.Relay
	if !rc.configured() {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(base string) {
		u := relayURL(base)
		if k := endpointKey(u); !seen[k] {
			seen[k] = true
			out = append(out, u)
		}
	}
	for _, b := range rc.Bases {
		add(b)
	}
	for _, b := range e.autoAdvertising() {
		add(b)
	}
	return out
}

// relayUsable reports whether a relay can take a frame right now.
func (e *Emitter) relayUsable() bool { return len(e.relayCandidates()) > 0 }

// relayOwns reports whether ep is the route URL of a relay this emitter is configured for (healthy
// or not: a marker's relay may be down while its card is still open).
func (e *Emitter) relayOwns(ep string) bool {
	rc := e.cfg.Relay
	for _, b := range rc.Bases {
		if sameEndpoint(ep, relayURL(b)) {
			return true
		}
	}
	if rc.Auto {
		for _, b := range rc.Remotes {
			if sameEndpoint(ep, relayURL(b)) {
				return true
			}
		}
	}
	return false
}

// pickRelay is the first candidate that has not just failed a post; when every one has, the first.
func (e *Emitter) pickRelay() string {
	cands := e.relayCandidates()
	if len(cands) == 0 {
		return ""
	}
	now := time.Now()
	e.relay.mu.Lock()
	defer e.relay.mu.Unlock()
	for _, u := range cands {
		if at, bad := e.relay.demoted[u]; !bad || now.Sub(at) >= relayDemoteFor {
			return u
		}
	}
	return cands[0]
}

func (e *Emitter) demoteRelay(u string) {
	e.relay.mu.Lock()
	if e.relay.demoted == nil {
		e.relay.demoted = map[string]time.Time{}
	}
	e.relay.demoted[u] = time.Now()
	e.relay.mu.Unlock()
}

// relayRoute decides where an event's frame goes when this emitter has no local identity: the relay
// its job's in-flight frames went to (a terminal frame must land on the card those opened, even if
// the first healthy member changed since), else the first healthy one. A terminal frame forgets the
// pin. "" = no relay.
func (e *Emitter) relayRoute(ev Event) string {
	e.relay.mu.Lock()
	pinned, ok := e.relay.pinned[ev.JobID]
	if ok && isTerminal(ev.State) {
		delete(e.relay.pinned, ev.JobID)
	}
	e.relay.mu.Unlock()
	if ok {
		return pinned
	}
	u := e.pickRelay()
	if u != "" && !isTerminal(ev.State) && ev.JobID != "" {
		e.relay.mu.Lock()
		if e.relay.pinned == nil {
			e.relay.pinned = map[string]string{}
		}
		if len(e.relay.pinned) >= relayPinsMax {
			for k := range e.relay.pinned { // a job that never closed: drop an arbitrary old pin
				delete(e.relay.pinned, k)
				break
			}
		}
		e.relay.pinned[ev.JobID] = u
		e.relay.mu.Unlock()
	}
	return u
}

// pinnedRelay is the relay a job's in-flight frames went to, if any (without consuming the pin).
func (e *Emitter) pinnedRelay(jobID string) (string, bool) {
	e.relay.mu.Lock()
	defer e.relay.mu.Unlock()
	u, ok := e.relay.pinned[jobID]
	return u, ok
}

// relaySelfName is the node name a relayed event with no node carries: this box's own short
// hostname, which the member resolves against its members and view-only nodes.
func relaySelfName() string { return core.SanitizeAsker(shortHostname()) }

// buildRelay is build for a box with no PAIR identity: the same workloadInfo keys, with
// originatedFrom and scheduledOn null (the member stamps itself and resolves the node), and the
// node hint and its aliases beside the frame.
func (e *Emitter) buildRelay(ev Event) ([]byte, map[string]json.RawMessage, *relayMeta) {
	null := json.RawMessage("null")
	ms := func(v int64) json.RawMessage {
		if v == 0 {
			return null
		}
		return json.RawMessage(fmt.Sprint(v))
	}
	errRaw, reqRaw := null, null
	if ev.Error != "" {
		errRaw = mustJSON(CardError(ev.Error))
	}
	if ev.Requester != "" {
		reqRaw = mustJSON(ev.Requester)
	}
	created := ev.CreatedAt
	if created == 0 {
		created = time.Now().UnixMilli()
	}
	info := map[string]json.RawMessage{
		"id":             mustJSON(ev.JobID),
		"model":          mustJSON(ev.Model),
		"engine":         mustJSON(ev.Engine),
		"runId":          mustJSON(ev.JobID),
		"state":          mustJSON(ev.State),
		"originatedFrom": null,
		"scheduledOn":    null,
		"createdAt":      json.RawMessage(fmt.Sprint(created)),
		"startedAt":      ms(ev.StartedAt),
		"completedAt":    ms(ev.CompletedAt),
		"error":          errRaw,
		"requesterId":    reqRaw,
	}
	meta := &relayMeta{Node: strings.TrimSpace(ev.Node)}
	if meta.Node == "" {
		meta.Node = relaySelfName()
	}
	for _, a := range ev.NodeAliases {
		if a = strings.TrimSpace(a); a != "" {
			meta.Aliases = append(meta.Aliases, a)
		}
	}
	body, _ := relayFrameBody(MethodFor(ev.State), info, *meta)
	return body, info, meta
}

// relayFrameBody wraps a workloadInfo and its node hint in the relay body.
func relayFrameBody(method string, info map[string]json.RawMessage, m relayMeta) ([]byte, error) {
	out := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  map[string]any{"workloadInfo": info},
		"node":    m.Node,
	}
	if len(m.Aliases) > 0 {
		out["node_aliases"] = m.Aliases
	}
	return json.Marshal(out)
}

// postRelay delivers one relay body to the route URL u with the fleet bearer and this box's name.
// A relay that fails the post is put behind the others for a while.
func (e *Emitter) postRelay(ctx context.Context, u string, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := e.cfg.Relay.Token; tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if name := relaySelfName(); name != "" {
		req.Header.Set(core.AskerHeader, name)
	}
	resp, err := e.relay.client.Do(req)
	if err != nil {
		e.demoteRelay(u)
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if rejection(resp.StatusCode) {
			return &rejectedError{endpoint: u, status: resp.StatusCode}
		}
		e.demoteRelay(u)
		return fmt.Errorf("pairworkloads: relay %s answered %d", u, resp.StatusCode)
	}
	return nil
}

// Mode is what an emitter's frames currently do.
type ModeInfo struct {
	// Mode is one of ModeLocal, ModeNodeInfo, ModeRelay, ModeOff.
	Mode string `json:"mode"`
	// Relay is the route URL frames go to, in relay mode.
	Relay string `json:"relay,omitempty"`
	// Reason says why the emitter is off.
	Reason string `json:"reason,omitempty"`
}

// Mode reports which way this emitter reports: PAIR's loopback ingress with the identity from
// node-id.json, the same ingress with the identity from PAIR's node-info, a card relay on a member,
// or nothing.
func (e *Emitter) Mode() ModeInfo {
	if e == nil || !e.cfg.Enabled {
		return ModeInfo{Mode: ModeOff, Reason: "pair_workloads_enabled is off"}
	}
	e.idMu.Lock()
	e.loadIdentityLocked()
	self, src := e.selfUUID, e.idSource
	e.idMu.Unlock()
	if self != "" {
		if src == idSourceNodeInfo {
			return ModeInfo{Mode: ModeNodeInfo}
		}
		return ModeInfo{Mode: ModeLocal}
	}
	if !e.cfg.Relay.configured() {
		return ModeInfo{Mode: ModeOff, Reason: "no PAIR identity here and no relay configured (pair_workloads_relay, delegate_remotes)"}
	}
	if u := e.pickRelay(); u != "" {
		return ModeInfo{Mode: ModeRelay, Relay: u}
	}
	return ModeInfo{Mode: ModeOff, Reason: "no PAIR identity here and no relay member advertises pair_relay"}
}

// LocalIdentity reports whether this emitter has a PAIR identity of its own (a readable node-id.json,
// or PAIR's node-info with the ingress answering) and is enabled. A fleet-serve serves the relay only
// then: a node that itself relays never relays for another.
func (e *Emitter) LocalIdentity() bool {
	if e == nil || !e.cfg.Enabled {
		return false
	}
	self, _ := e.identity()
	return self != ""
}

// --- the member's side ---------------------------------------------------------------------------

// RelayError is a relayed frame the member refuses: a 400 with Msg.
type RelayError struct{ Msg string }

func (e *RelayError) Error() string { return e.Msg }

func relayErr(format string, a ...any) error { return &RelayError{Msg: fmt.Sprintf(format, a...)} }

var (
	relayTopKeys  = map[string]bool{"jsonrpc": true, "method": true, "params": true, "node": true, "node_aliases": true}
	relayInfoKeys = map[string]bool{"id": true, "model": true, "engine": true, "runId": true, "state": true,
		"originatedFrom": true, "scheduledOn": true, "createdAt": true, "startedAt": true, "completedAt": true,
		"error": true, "requesterId": true}
	relayEngineRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// relayStates maps a state to the lifecycle method a frame of that state must carry.
var relayStates = map[string]bool{"queued": true, "running": true, "completed": true, "failed": true}

// ParseRelay is the member's strict decode of one relayed frame sent by asker (already sanitized,
// non-empty). It accepts exactly the frame the relaying box builds (a JSON-RPC notification whose
// workloadInfo carries the documented keys) plus the top-level node and node_aliases, refuses
// anything else, and returns the Event the member's own emitter posts: id namespaced per asker,
// requester rewritten, node hint carried for the member's resolver. originatedFrom and scheduledOn
// in the body are checked for shape and then ignored: the member's emitter stamps its own identity
// and resolves the node itself.
func ParseRelay(body []byte, asker string) (Event, error) {
	asker = core.SanitizeAsker(asker)
	if asker == "" {
		return Event{}, relayErr("%s is required", core.AskerHeader)
	}
	if len(body) > RelayBodyMax {
		return Event{}, relayErr("relay body over %d bytes", RelayBodyMax)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return Event{}, relayErr("malformed relay body: %v", err)
	}
	for k := range top {
		if !relayTopKeys[k] {
			return Event{}, relayErr("unknown field %q", k)
		}
	}
	var jsonrpc, method string
	if json.Unmarshal(top["jsonrpc"], &jsonrpc) != nil || jsonrpc != "2.0" {
		return Event{}, relayErr(`jsonrpc must be "2.0"`)
	}
	if json.Unmarshal(top["method"], &method) != nil {
		return Event{}, relayErr("method must be a workload lifecycle method")
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(top["params"], &params) != nil || len(params) != 1 || params["workloadInfo"] == nil {
		return Event{}, relayErr("params must be exactly {workloadInfo}")
	}
	var info map[string]json.RawMessage
	if json.Unmarshal(params["workloadInfo"], &info) != nil || info == nil {
		return Event{}, relayErr("workloadInfo must be an object")
	}
	for k := range info {
		if !relayInfoKeys[k] {
			return Event{}, relayErr("unknown workloadInfo field %q", k)
		}
	}
	str := func(key string, max int, required bool) (string, error) {
		raw, present := info[key]
		if !present || string(raw) == "null" {
			if required {
				return "", relayErr("workloadInfo.%s is required", key)
			}
			return "", nil
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", relayErr("workloadInfo.%s must be a string or null", key)
		}
		if len(s) > max {
			return "", relayErr("workloadInfo.%s is over %d bytes", key, max)
		}
		for _, r := range s {
			if !unicode.IsPrint(r) {
				return "", relayErr("workloadInfo.%s holds a control or non-printing character", key)
			}
		}
		if required && strings.TrimSpace(s) == "" {
			return "", relayErr("workloadInfo.%s is required", key)
		}
		return s, nil
	}
	id, err := str("id", relayFieldMax, true)
	if err != nil {
		return Event{}, err
	}
	if runID, err := str("runId", relayFieldMax, false); err != nil {
		return Event{}, err
	} else if runID != "" && runID != id {
		return Event{}, relayErr("workloadInfo.runId must equal id")
	}
	model, err := str("model", relayFieldMax, true)
	if err != nil {
		return Event{}, err
	}
	engine, err := str("engine", 64, true)
	if err != nil {
		return Event{}, err
	}
	if !relayEngineRe.MatchString(engine) {
		return Event{}, relayErr("workloadInfo.engine %q is not an engine name", engine)
	}
	state, err := str("state", 16, true)
	if err != nil {
		return Event{}, err
	}
	if !relayStates[state] {
		return Event{}, relayErr("workloadInfo.state %q is not queued, running, completed or failed", state)
	}
	if MethodFor(state) != method {
		return Event{}, relayErr("method %q does not match state %q (want %q)", method, state, MethodFor(state))
	}
	for _, k := range []string{"originatedFrom", "scheduledOn"} {
		if _, err := str(k, relayFieldMax, false); err != nil {
			return Event{}, err
		}
	}
	errText, err := str("error", 4096, false)
	if err != nil {
		return Event{}, err
	}
	reqIn, err := str("requesterId", relayFieldMax, false)
	if err != nil {
		return Event{}, err
	}
	epoch := func(key string) (int64, error) {
		raw, present := info[key]
		if !present || string(raw) == "null" {
			return 0, nil
		}
		var n int64
		if json.Unmarshal(raw, &n) != nil || n < 0 || n > 1e15 {
			return 0, relayErr("workloadInfo.%s must be epoch milliseconds or null", key)
		}
		return n, nil
	}
	created, err := epoch("createdAt")
	if err != nil {
		return Event{}, err
	}
	started, err := epoch("startedAt")
	if err != nil {
		return Event{}, err
	}
	completed, err := epoch("completedAt")
	if err != nil {
		return Event{}, err
	}

	ev := Event{
		JobID:       RelayJobID(asker, id),
		Model:       model,
		Engine:      engine,
		State:       state,
		Error:       errText,
		Requester:   RelayRequester(asker, reqIn),
		CreatedAt:   created,
		StartedAt:   started,
		CompletedAt: completed,
		Remote:      true,
	}
	// The node hint: the relay's, else the asker's own name. Never empty, because an empty node
	// means "this box" to the resolver, and a relayed job did not run here.
	hint := ""
	if raw, ok := top["node"]; ok && string(raw) != "null" {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return Event{}, relayErr("node must be a string")
		}
		hint = core.SanitizeAsker(s)
	}
	if raw, ok := top["node_aliases"]; ok && string(raw) != "null" {
		var list []string
		if json.Unmarshal(raw, &list) != nil {
			return Event{}, relayErr("node_aliases must be a list of strings")
		}
		if len(list) > relayAliasesMax {
			return Event{}, relayErr("node_aliases is over %d names", relayAliasesMax)
		}
		for _, a := range list {
			if a = core.SanitizeAsker(a); a != "" {
				ev.NodeAliases = append(ev.NodeAliases, a)
			}
		}
	}
	if hint == "" {
		hint = asker
	}
	ev.Node = hint
	return ev, nil
}

// RelayJobID is the id a member posts for a relayed job: "relay-<asker>-<id>", bounded, with a short
// digest of the exact (asker, id) pair appended so two different pairs can never map to one card
// ("n" + "led-1" and "n-led" + "1" would otherwise both read relay-n-led-1), whatever the bound cut.
func RelayJobID(asker, id string) string {
	sum := sha256.Sum256([]byte(asker + "\x00" + id))
	h := hex.EncodeToString(sum[:])[:8]
	head := "relay-" + asker + "-" + id
	if max := relayIDMax - 1 - len(h); len(head) > max {
		head = head[:max]
	}
	return head + "-" + h
}

// RelayRequester is the requesterId a member posts for a relayed job: offload-harness/fleet:<asker>,
// plus "/" and the relayed requester's own suffix (its session) when it has one, bounded.
func RelayRequester(asker, original string) string {
	out := relayRequester + asker
	suffix := strings.TrimSpace(original)
	suffix = strings.TrimPrefix(suffix, relayRequesterRoot)
	suffix = strings.TrimPrefix(suffix, "/")
	if suffix = core.SanitizeAsker(suffix); suffix != "" {
		out += "/" + suffix
	}
	return out
}

// --- the member's per-asker limit ----------------------------------------------------------------

// RelayLimiter is the member's token bucket: one per asker name, and one over every asker, because
// the asker name is a header a token holder chooses (a fresh name per call would otherwise be a
// fresh bucket per call).
type RelayLimiter struct {
	mu     sync.Mutex
	rate   float64 // tokens per second, per asker
	burst  float64
	global bucket
	gRate  float64
	gBurst float64
	b      map[string]*bucket
	now    func() time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

// RelayBuckets bounds the askers a limiter tracks; the least recently seen is dropped past it.
const RelayBuckets = 1024

// NewRelayLimiter builds a limiter: rate frames per second per asker with a burst, and a global
// rate and burst over all askers together.
func NewRelayLimiter(rate float64, burst int, gRate float64, gBurst int) *RelayLimiter {
	return &RelayLimiter{rate: rate, burst: float64(burst), gRate: gRate, gBurst: float64(gBurst),
		b: map[string]*bucket{}, now: time.Now}
}

// DefaultRelayLimiter is the member's production limit: 5 frames/s per asker (burst 60), 50/s
// overall (burst 200). A job is two or three frames, so a busy box stays well inside it.
func DefaultRelayLimiter() *RelayLimiter { return NewRelayLimiter(5, 60, 50, 200) }

func (b *bucket) take(now time.Time, rate, burst float64) (ok bool, wait time.Duration) {
	if b.at.IsZero() {
		b.tokens, b.at = burst, now
	}
	b.tokens += now.Sub(b.at).Seconds() * rate
	if b.tokens > burst {
		b.tokens = burst
	}
	b.at = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / rate * float64(time.Second))
}

// Allow spends one token of asker's bucket and of the global one. A refusal says how long to wait.
func (l *RelayLimiter) Allow(asker string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	bk := l.b[asker]
	if bk == nil {
		if len(l.b) >= RelayBuckets {
			l.evictLocked()
		}
		bk = &bucket{}
		l.b[asker] = bk
	}
	if ok, wait := bk.take(now, l.rate, l.burst); !ok {
		return false, wait
	}
	if ok, wait := l.global.take(now, l.gRate, l.gBurst); !ok {
		bk.tokens++ // the asker did not get through: give its token back
		return false, wait
	}
	return true, 0
}

// evictLocked drops the buckets idle the longest (a quarter of them).
func (l *RelayLimiter) evictLocked() {
	type kv struct {
		k string
		t time.Time
	}
	all := make([]kv, 0, len(l.b))
	for k, v := range l.b {
		all = append(all, kv{k, v.at})
	}
	for n := len(all) / 4; n > 0; n-- {
		oldest := 0
		for i := range all {
			if all[i].t.Before(all[oldest].t) {
				oldest = i
			}
		}
		delete(l.b, all[oldest].k)
		all = append(all[:oldest], all[oldest+1:]...)
	}
}

var errNoRelay = errors.New("pairworkloads: PAIR identity unavailable and no relay member is reachable")
