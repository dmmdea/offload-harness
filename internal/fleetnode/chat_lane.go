// The fleet "chat" lane (C-41b): this node forwards ONE OpenAI chat
// completion, for a model its own llama-swap serves, on behalf of a delegator
// whose local seat would make the call wait.
//
// Why it exists at all. The cascade lane (internal/llamaclient/lanes.go) was
// written to send a cascade tier at a fleet node that serves the identical
// GGUF while the local 27B seat holds this box's cards. It probed the lane
// base's llama-swap directly — and BOTH fleet nodes bind llama-swap to
// 127.0.0.1:11436 and nothing else (the Aorus since the 2026-08-24 native
// cutover, the Lenovo by design), so from another box that base is
// unreachable, every residency probe fails, and the lane silently never fires.
// Binding llama-swap to the tailnet would be a NEW unauthenticated listener,
// which is exactly what the vision lane refused to do in 0.116.0 (ADR 0040):
// it put a bearer-gated route on the node's own :18811 that proxies to the
// node's loopback llama-swap. This is that same door for text.
//
// What it is NOT. It is not a job: no job_id, no queue, no polling, no
// admission band. A cascade tier is a single short synchronous call whose
// caller already holds a deadline, and wrapping it in the job store would add
// a poll loop to a request that finishes in seconds. Queueing still happens —
// on the node's llama-swap, which is where it belongs.
//
// Auth is the agent and vision lanes' rule, verbatim: fleet_auth_token
// required beyond loopback, loopback + no token stays open
// (AgentLaneSafelyReachable). The check runs FIRST, before the body is even
// read, because unlike the vision lane nothing about the decision depends on
// the body — so an unauthorized caller gets the auth verdict and nothing else.
package fleetnode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/netguard"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// ChatLanePath is this lane's route. llamaclient.FleetChatPath is the caller's
// copy of the same string; their equality is pinned by a test in this package
// (fleetnode may import llamaclient — the reverse would be an import cycle).
const ChatLanePath = "/fleet/chat"

// ChatBodyCap bounds the forwarded request. A cascade call carries the
// document it is summarizing or extracting from, so this is sized from the
// largest context the tiers are configured for (a 128k-token prompt is ~0.5 MB
// of UTF-8) with room for a grammar and the JSON framing — not from dispatch's
// 1 MiB envelope cap, which is right for a job descriptor and wrong for a
// prompt.
const ChatBodyCap = 8 << 20

// chatResponseCap bounds what is copied back. A completion is bounded by
// max_tokens long before this, so the cap exists only so a misbehaving
// upstream cannot stream unbounded bytes through the node.
const chatResponseCap = 16 << 20

// chatRosterTimeout bounds the residency check. The same budget the agent
// seat's residency probe uses — one GET /v1/models against loopback.
const chatRosterTimeout = agentResidencyProbeTimeout

// ChatProxyTimeout bounds the forwarded call end to end on the NODE's side: a
// cold swap on this node (a llama.cpp cascade seat loads in 10-30 s, and
// llama-swap will not swap while another model's requests are in flight) plus
// the generation itself. The caller's own deadline is normally shorter and
// cancels first through the request context; this is the backstop that keeps a
// forgotten connection from pinning a proxy goroutine forever.
const ChatProxyTimeout = 10 * time.Minute

// chatWriteSlack is what this handler adds to ChatProxyTimeout when it extends
// its own write deadline: the upstream's whole budget, plus room to copy the
// answer back after the upstream has finished speaking. It is slack on a
// deadline, not a second timeout — the request context is still the bound.
const chatWriteSlack = 30 * time.Second

// chatLaneTokenRequired is the 403 refusal for a chat-lane call on a
// non-loopback listener with no fleet_auth_token configured — the agent lane's
// signal, worded for this route: a loud misconfiguration, never a silent open
// door.
const chatLaneTokenRequired = "chat lane requires fleet_auth_token on a non-loopback listener"

// chatUpstream is the transport the forward rides. netguard.SafeTransport for
// the same reason every other outbound client in the harness uses it: the
// never-cloud boundary is enforced at DIAL time, and this one is pointed at
// whatever `endpoint` the node's config names. Its Timeout is zero on purpose
// — the per-request context (ChatProxyTimeout, or the caller's shorter
// deadline) is the bound, and a client-level timeout would also cut the
// response body copy.
var chatUpstream = &http.Client{Transport: netguard.SafeTransport(nil)}

// ChatLaneAdmissible is THE predicate behind the chat lane, on both sides of
// the wire — the health advertisement (`chat_lane`) and this handler's own
// gate — exactly as AgentLaneAdmissible and VisionLaneAdmissible are for
// theirs. Health must never advertise a lane the handler would refuse: the
// delegator reads `chat_lane` alone to decide a lane base is usable, and a
// lane advertised past a tokenless non-loopback listener would send every
// cascade call into a 403.
//
// Two conditions: a bound llama-swap `endpoint` to forward to, and safe
// reachability (loopback listener, or a fleet_auth_token for anything beyond
// it). There is deliberately no model condition — the lane serves whatever
// this node's roster serves, checked per call.
func ChatLaneAdmissible(cfg config.Config, loopbackListener bool) bool {
	return strings.TrimSpace(cfg.Endpoint) != "" && AgentLaneSafelyReachable(cfg, loopbackListener)
}

// chatRequestHead is the ONE field this node reads out of an otherwise opaque
// OpenAI chat body. The body is forwarded BYTE FOR BYTE — grammar,
// chat_template_kwargs, logprobs, cache_prompt and every other field the
// caller set must arrive unchanged, or the lane would silently answer a
// different question than the local path would. So the decode is lenient
// (unknown fields allowed) and its result is used for the routing decision
// only, never to rebuild the request.
type chatRequestHead struct {
	Model string `json:"model"`
}

// handleChat is the lane: auth, cap, read the model, verify THIS node serves
// it (alias-aware, the same rosterServes seam the agent seat's residency uses),
// forward verbatim to this node's llama-swap, copy the answer back.
//
// Refusals are distinct on purpose, because the caller's fallback differs:
// 401/403 is a credential problem the operator must fix, 404 says this node
// does not serve that model (the caller should stop routing it here), 503 says
// the roster could not be read right now (retryable), 502 says the forward
// itself failed.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	// AUTH FIRST — see the package comment. Nothing about this decision needs
	// the body, so an unauthorized caller never reaches a validation 400 it
	// could probe this node's configuration with.
	if s.opts.Cfg.FleetAuthToken == "" {
		// The reachability condition is CONSULTED, never re-derived: the same
		// expression ChatLaneAdmissible applies to the same resolved listener,
		// so this refusal can never disagree with what health advertised.
		if !AgentLaneSafelyReachable(s.opts.Cfg, s.opts.LoopbackListener) {
			writeError(w, http.StatusForbidden, chatLaneTokenRequired)
			return
		}
	} else if !bearerOK(r, s.opts.Cfg.FleetAuthToken) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !s.chatLane {
		// Authorized, but this node has nothing to forward TO (no endpoint).
		writeError(w, http.StatusNotFound, "chat lane is not enabled on this node (no llama-swap endpoint configured)")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, ChatBodyCap)
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("content-type must be application/json (got %q)", ct))
			return
		}
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("reading chat body (limit %d bytes): %v", ChatBodyCap, err))
		return
	}
	var head chatRequestHead
	if err := json.Unmarshal(body, &head); err != nil {
		writeError(w, http.StatusBadRequest, "malformed chat body: "+err.Error())
		return
	}
	model := strings.TrimSpace(head.Model)
	if model == "" {
		writeError(w, http.StatusBadRequest, "model required: the chat lane routes by model, and this node serves several")
		return
	}

	// Residency, alias-aware (Roster.Serves matches canonical ids AND
	// meta.llamaswap.aliases): the caller names a model by whatever id or alias
	// it knows, and an id-only match would 404 a seat this node serves
	// perfectly well under an alias — the plannerUnserved lesson.
	rctx, rcancel := context.WithTimeout(r.Context(), chatRosterTimeout)
	serves, rerr := s.rosterServes(rctx, s.opts.Cfg.Endpoint, model)
	rcancel()
	if rerr != nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("roster of %s unreadable: %v", s.opts.Cfg.Endpoint, rerr))
		return
	}
	if !serves {
		writeError(w, http.StatusNotFound, fmt.Sprintf("this node does not serve model %q", model))
		return
	}

	// THIS handler's own write deadline, past the server's blanket
	// WriteTimeout (register S-09). net/http arms that blanket at header-read
	// for every handler alike, so a 30-second table silently bounded a lane
	// whose whole budget is ChatProxyTimeout: a cascade call that forwards
	// past it was cut mid-write and read to the caller as a dead node. The
	// blanket stays as the floor for every other handler; this one says what
	// it needs, per request.
	s.extendWrite(w, ChatProxyTimeout+chatWriteSlack, "the chat lane")
	pctx, pcancel := context.WithTimeout(r.Context(), ChatProxyTimeout)
	defer pcancel()
	upstream := swapclient.BaseURL(s.opts.Cfg.Endpoint) + "/v1/chat/completions"
	ureq, err := http.NewRequestWithContext(pctx, http.MethodPost, upstream, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "building upstream request: "+err.Error())
		return
	}
	ureq.Header.Set("Content-Type", "application/json")
	resp, err := chatUpstream.Do(ureq)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("forwarding to %s: %v", upstream, err))
		return
	}
	defer resp.Body.Close()
	// The upstream's STATUS is copied through unchanged, not flattened: the
	// caller's own retry logic keys on llama-swap's 429 / 503-not-ready
	// answers (seatwait.Retryable), and a lane that turned those into a 502
	// would break the seat-wait loop it exists to serve.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		w.Header().Set("Retry-After", ra)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, chatResponseCap))
}
