package fleetnode

// KV-slot save/restore lane (ADR 0056, Layer 2). llama-server can write one
// slot's KV cache to a file under --slot-save-path and read it back
// (POST /slots/{id}?action=save|restore). This lane wraps the two calls over
// HTTP; the node's own agent loop drives the same core in-process
// (internal/kvslot, ADR 0100), because only the loop knows the byte-exact
// prefix it sends. Both skip the prefill of context already paid for on this
// node across the 5-minute idle unload, which the in-process --cache-ram
// cannot survive.
//
// Contract: the KEY names the seat shape + the byte-exact prefix; the node
// checks the caller's expected seat pin against the live seat before a
// restore so a file is only ever restored into the identical seat shape.
// Both calls are best-effort on the delegator side: any non-2xx here is a
// "skip", never a failed contract.

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/dmmdea/offload-harness/internal/kvslot"
)

const (
	KVSlotSavePath    = "/fleet/kvslot/save"
	KVSlotRestorePath = "/fleet/kvslot/restore"
	kvSlotBodyCap     = 64 << 10
)

// kvSlotKeyRe is the only key shape accepted: "k1-" + a sha256 hex. The hash
// is over model_file, ctx, kv_k, kv_v, build_family and the byte-exact prompt
// prefix (system prompt + context docs in contract order), computed by the
// delegator (delegate.KVSlotKey). A file name derived from it can never
// escape the slot directory.
var kvSlotKeyRe = kvslot.KeyRe

// KVSlotRequest is the body of both endpoints.
type KVSlotRequest struct {
	Seat string `json:"seat"`
	Key  string `json:"key"`
	// SeatPinSHA256 is the caller's expectation of the seat's config pin
	// (agent.ProbeSeatPin over /props). When set, a live pin that differs is a
	// 409: the file would be restored into a different seat shape.
	SeatPinSHA256 string `json:"seat_pin_sha256,omitempty"`
}

// KVSlotResponse is the JSON answer of both endpoints (the shared core's Response).
type KVSlotResponse = kvslot.Response

// kvSlotEnabled is THE predicate behind this lane on BOTH sides of the wire —
// health's `kvslot` and this handler's refusals — exactly as ChatLaneAdmissible
// is for the chat lane. Health must never advertise a lane the handler would
// refuse. The per-seat roster and residency checks stay per call, the same way
// the chat lane has no model condition. The os.Stat is not cached in New: the
// installer can create the directory after the node starts.
func (s *Server) kvSlotEnabled() bool {
	if strings.TrimSpace(s.opts.KVSlotDir) == "" || strings.TrimSpace(s.opts.Cfg.Endpoint) == "" {
		return false
	}
	if !AgentLaneSafelyReachable(s.opts.Cfg, s.opts.LoopbackListener) {
		return false
	}
	st, err := os.Stat(s.opts.KVSlotDir)
	return err == nil && st.IsDir()
}

func (s *Server) handleKVSlotSave(w http.ResponseWriter, r *http.Request) {
	s.handleKVSlot(w, r, "save")
}

func (s *Server) handleKVSlotRestore(w http.ResponseWriter, r *http.Request) {
	s.handleKVSlot(w, r, "restore")
}

func (s *Server) handleKVSlot(w http.ResponseWriter, r *http.Request, action string) {
	// Same bearer rule as the chat lane: the endpoint reaches into llama-swap.
	if !s.authorizeLane(w, r, kvslotRefusalLabel, "kvslot lane requires fleet_auth_token on a non-loopback listener") {
		return
	}
	if !s.kvSlotEnabled() {
		writeError(w, http.StatusNotImplemented, "kvslot lane is not enabled on this node (no slot directory, or no llama-swap endpoint, or a listener this lane will not serve unauthenticated — the caller falls through)")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, kvSlotBodyCap)
	// The siblings' Content-Type gate, verbatim (chat_lane.go, handleVision).
	// It is load-bearing, not hygiene: a cross-origin `fetch` with a body sends
	// text/plain by default, which is a CORS *simple* request — no preflight, the
	// request lands, and only the RESPONSE is hidden from the script. The side
	// effect still happens. On the one unauthenticated configuration this lane
	// blesses (loopback listener, no token) that would let any page the operator
	// has open drive the lane.
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("content-type must be application/json (got %q)", ct))
			return
		}
	}
	var req KVSlotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed kvslot body: "+err.Error())
		return
	}
	req.Seat = strings.TrimSpace(req.Seat)
	req.Key = strings.TrimSpace(req.Key)
	if req.Seat == "" || strings.ContainsAny(req.Seat, "/\\ ") {
		writeError(w, http.StatusBadRequest, "seat required (a llama-swap model id)")
		return
	}
	if !kvSlotKeyRe.MatchString(req.Key) {
		writeError(w, http.StatusBadRequest, "key must be k1-<sha256 hex>")
		return
	}
	file := filepath.Join(s.opts.KVSlotDir, req.Key+kvslot.FileSuffix)
	if action == "restore" {
		if _, err := os.Stat(file); err != nil {
			writeJSON(w, http.StatusNotFound, KVSlotResponse{Status: "miss", Seat: req.Seat, Key: req.Key, Action: action, Note: "no saved slot for this key on this node"})
			return
		}
	}
	rctx, rcancel := context.WithTimeout(r.Context(), chatRosterTimeout)
	serves, rerr := s.rosterServes(rctx, s.opts.Cfg.Endpoint, req.Seat)
	rcancel()
	if rerr != nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("roster of %s unreadable: %v", s.opts.Cfg.Endpoint, rerr))
		return
	}
	if !serves {
		writeError(w, http.StatusNotFound, fmt.Sprintf("this node does not serve seat %q", req.Seat))
		return
	}
	// Residency (never start a model), the config-pin check, the GPU-lease fence, the upstream call and the mapping of
	// its answer are internal/kvslot's: the agent loop's own retention runs the same code, and the two must not
	// disagree about what a save or a restore means.
	out := kvslot.Do(r.Context(), kvslot.Call{
		Endpoint: s.opts.Cfg.Endpoint, Seat: req.Seat, Key: req.Key, Dir: s.opts.KVSlotDir, Action: action,
		PinSHA256: req.SeatPinSHA256,
		BeforeUpstream: func() {
			s.extendWrite(w, kvslot.CallTimeout+chatWriteSlack, "the kvslot lane")
		},
	})
	if out.ErrMsg != "" {
		writeError(w, out.Code, out.ErrMsg)
		return
	}
	if action == "save" && out.Resp.Status == "ok" {
		// Never the file this request just wrote: it is the newest by mtime, so the LRU pass reaches it only when it
		// ALONE exceeds the cap — and then the response would still say ok with a byte count for a file that no
		// longer exists. A full 32k slot is ~1.1 GB, so that is reachable, not theoretical.
		s.sweepKVSlots(file)
	}
	writeJSON(w, out.Code, out.Resp)
}

// sweepKVSlots keeps the slot directory under the node's cap by deleting the
// least recently used files first. Cap 0 = the default 8 GiB; the plan's
// per-tier caps land in profiles.json once measured (Task 3).
func (s *Server) sweepKVSlots(keep string) {
	capGiB := s.opts.KVSlotCapGiB
	if capGiB <= 0 {
		capGiB = 8
	}
	_ = SweepKVSlotDir(s.opts.KVSlotDir, int64(capGiB)<<30, keep)
}

// SweepKVSlotDir is kvslot.SweepDir, kept under its old name for this package's callers.
func SweepKVSlotDir(dir string, capBytes int64, keep string) error {
	return kvslot.SweepDir(dir, capBytes, keep)
}
