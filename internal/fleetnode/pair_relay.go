package fleetnode

// The PAIR card relay's member side (D26): POST /fleet/pair-relay takes ONE workload lifecycle frame
// from a box that is not a PAIR cluster member (a view-only box, a thin client where PAIR is not
// installed) and posts it as a card from THIS node's own emitter, so that box's work reaches the
// operator's Jobs list. The frame decode, the id namespace, the requester rewrite and the node
// resolution live in internal/pairworkloads (ParseRelay); this file is the door.
//
// The door is token-gated like every other gated lane (tokenGated: a fleet_auth_token for anything
// beyond loopback, loopback with no token stays open), checked before a byte of the body is read; the
// relaying box names itself in X-Offload-Asker (required); a per-asker token bucket and a global one
// answer 429 before the body is read, so a token holder cannot flood PAIR, and a cap on the cards left
// open at once (per asker and overall, 429 past it; a terminal frame is always admitted) stops a slow
// flood of cards that never close. It is advertised in health
// (pair_relay) exactly when it would admit: this node has a PAIR identity of its own (an emitter that
// itself relays never relays for another) and the reachability rule holds.
//
// The node's own emitter posts the card, so the member's orphan register covers the in-flight card:
// a relayed marker has no local pid (its producer is on another box) and closes by its terminal
// relayed frame or the age cap (pairworkloads.RelayOpenMaxAge).

import (
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

// PairRelayTask is the relay door's name in tokenGated; it is not a job type and never reaches the
// job store.
const PairRelayTask = "pair-relay"

// PairRelayAdmissible is THE predicate behind the relay door, on both sides of the wire (the health
// advertisement and the handler): this node has a PAIR identity of its own, and the lane's
// reachability rule holds (a loopback listener, or a fleet_auth_token for anything beyond it).
func PairRelayAdmissible(cfg config.Config, loopbackListener, localIdentity bool) bool {
	return localIdentity && AgentLaneSafelyReachable(cfg, loopbackListener)
}

// pairRelayOpen is PairRelayAdmissible over this server's resolved listener and emitter. The identity
// read is the emitter's cached one (re-read at most once a minute; its documented cost is the
// node-info probes when node-id.json is unreadable).
func (s *Server) pairRelayOpen() bool {
	return PairRelayAdmissible(s.opts.Cfg, s.opts.LoopbackListener, s.opts.Pair.LocalIdentity())
}

const pairRelayClosed = "pair-relay is not open on this node (it needs a PAIR identity of its own: pair_workloads_enabled with PAIR installed here)"

func (s *Server) handlePairRelay(w http.ResponseWriter, r *http.Request) {
	// The bearer first, before anything else is read or decided about the request.
	if !s.authorizeGated(w, r, PairRelayTask) {
		return
	}
	if !s.pairRelayOpen() {
		writeError(w, http.StatusServiceUnavailable, pairRelayClosed)
		return
	}
	asker := core.SanitizeAsker(r.Header.Get(core.AskerHeader))
	if asker == "" {
		writeError(w, http.StatusBadRequest, core.AskerHeader+" is required: the relaying box's name")
		return
	}
	if ok, wait := s.relayLimiter.Allow(asker); !ok {
		secs := int(math.Ceil(wait.Seconds()))
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("pair-relay: %q is sending frames faster than this node relays them; retry in %d s", asker, secs))
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("content-type must be application/json (got %q)", ct))
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, pairworkloads.RelayBodyMax)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("relay body over %d bytes", pairworkloads.RelayBodyMax))
			return
		}
		writeError(w, http.StatusBadRequest, "reading relay body: "+err.Error())
		return
	}
	ev, err := pairworkloads.ParseRelay(body, asker)
	if err != nil {
		var re *pairworkloads.RelayError
		if errors.As(err, &re) {
			writeError(w, http.StatusBadRequest, re.Msg)
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The call rate is bounded above; this bounds the cards a token holder can leave open (each is a
	// PAIR card and a register marker for up to the age cap). A terminal frame is always admitted.
	if !s.relayLimiter.AdmitCard(asker, ev.JobID, ev.State) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("pair-relay: %q (or this node) already holds the most relayed cards it keeps open; finish or fail one first", asker))
		return
	}
	// The member's own emitter posts it: the card is the member's, the orphan register covers it.
	s.opts.Pair.Emit(ev)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
