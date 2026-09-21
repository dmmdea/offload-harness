package fleetnode

// KV-slot save/restore lane (ADR 0055, Layer 2). llama-server can write one
// slot's KV cache to a file under --slot-save-path and read it back
// (POST /slots/{id}?action=save|restore). The harness never drove it; this
// lane wraps the two calls so the delegator can skip the prefill of context
// it has already paid for on this node — across the 5-minute idle unload,
// which is exactly what the in-process --cache-ram cannot survive.
//
// Contract: the KEY names the seat shape + the byte-exact prefix; the node
// checks the caller's expected seat pin against the live seat before a
// restore so a file is only ever restored into the identical seat shape.
// Both calls are best-effort on the delegator side: any non-2xx here is a
// "skip", never a failed contract.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/netguard"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

const (
	KVSlotSavePath    = "/fleet/kvslot/save"
	KVSlotRestorePath = "/fleet/kvslot/restore"
	kvSlotBodyCap     = 64 << 10
	kvSlotCallTimeout = 5 * time.Minute
	kvSlotPinTimeout  = 10 * time.Second
	kvSlotFileSuffix  = ".bin"
)

// kvSlotKeyRe is the only key shape accepted: "k1-" + a sha256 hex. The hash
// is over model_file, ctx, kv_k, kv_v, build_family and the byte-exact prompt
// prefix (system prompt + context docs in contract order), computed by the
// delegator (delegate.KVSlotKey). A file name derived from it can never
// escape the slot directory.
var kvSlotKeyRe = regexp.MustCompile(`^k1-[0-9a-f]{64}$`)

var kvSlotUpstream = &http.Client{Transport: netguard.SafeTransport(nil)}

// KVSlotRequest is the body of both endpoints.
type KVSlotRequest struct {
	Seat string `json:"seat"`
	Key  string `json:"key"`
	// SeatPinSHA256 is the caller's expectation of the seat's config pin
	// (agent.ProbeSeatPin over /props). When set, a live pin that differs is a
	// 409: the file would be restored into a different seat shape.
	SeatPinSHA256 string `json:"seat_pin_sha256,omitempty"`
}

// KVSlotResponse is the JSON answer of both endpoints.
type KVSlotResponse struct {
	Status   string `json:"status"`
	Seat     string `json:"seat"`
	Key      string `json:"key"`
	Action   string `json:"action"`
	Tokens   int    `json:"n_tokens,omitempty"`
	Bytes    int64  `json:"n_bytes,omitempty"`
	Ms       int64  `json:"ms"`
	Note     string `json:"note,omitempty"`
	Upstream string `json:"upstream_status,omitempty"`
}

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
	if s.opts.Cfg.FleetAuthToken == "" {
		if !AgentLaneSafelyReachable(s.opts.Cfg, s.opts.LoopbackListener) {
			writeError(w, http.StatusForbidden, "kvslot lane requires fleet_auth_token on a non-loopback listener")
			return
		}
	} else if !bearerOK(r, s.opts.Cfg.FleetAuthToken) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
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
	file := filepath.Join(s.opts.KVSlotDir, req.Key+kvSlotFileSuffix)
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
	// NEVER load a model. `/upstream/<seat>/…` STARTS an unloaded seat (the same
	// trap server.go documents for the residency read), so without this an
	// unauthenticated loopback POST is a model-load primitive against the cards:
	// it defeats the 5-minute idle unload, ignores a drain or a GPU lease, and on
	// a save it would then store an EMPTY slot over a good file and call it ok.
	// A cold seat also has nothing worth saving and nowhere useful to restore to.
	lctx, lcancel := context.WithTimeout(r.Context(), agentResidencyProbeTimeout)
	reading, lerr := swapSeatRunning(lctx, s.opts.Cfg.Endpoint, req.Seat)
	lcancel()
	if lerr != nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("seat state unreadable: %v", lerr))
		return
	}
	if !reading.Loaded {
		writeJSON(w, http.StatusConflict, KVSlotResponse{Status: "seat-cold", Seat: req.Seat, Key: req.Key, Action: action,
			Note: "the seat is not loaded and this lane never starts one; warm it through the normal path first"})
		return
	}
	if req.SeatPinSHA256 != "" {
		pctx, pcancel := context.WithTimeout(r.Context(), kvSlotPinTimeout)
		pin, ok := agent.ProbeSeatPin(pctx, s.opts.Cfg.Endpoint, req.Seat)
		pcancel()
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "seat pin unreadable (the seat may be cold); retry after a warm-up")
			return
		}
		if !strings.EqualFold(pin.SHA256, req.SeatPinSHA256) {
			writeJSON(w, http.StatusConflict, KVSlotResponse{Status: "shape-mismatch", Seat: req.Seat, Key: req.Key, Action: action,
				Note: "the live seat's config pin differs from the caller's; a slot file is only restored into the identical seat shape"})
			return
		}
	}
	s.extendWrite(w, kvSlotCallTimeout+chatWriteSlack, "the kvslot lane")
	uctx, ucancel := context.WithTimeout(r.Context(), kvSlotCallTimeout)
	defer ucancel()
	upstream := swapclient.BaseURL(s.opts.Cfg.Endpoint) + "/upstream/" + req.Seat + "/slots/0?action=" + action
	body, _ := json.Marshal(map[string]string{"filename": req.Key + kvSlotFileSuffix})
	ureq, err := http.NewRequestWithContext(uctx, http.MethodPost, upstream, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "building upstream request: "+err.Error())
		return
	}
	ureq.Header.Set("Content-Type", "application/json")
	t0 := time.Now()
	resp, err := kvSlotUpstream.Do(ureq)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("forwarding to %s: %v", upstream, err))
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, kvSlotBodyCap))
	ms := time.Since(t0).Milliseconds()
	out := KVSlotResponse{Seat: req.Seat, Key: req.Key, Action: action, Ms: ms, Upstream: resp.Status}
	switch {
	case resp.StatusCode == http.StatusNotImplemented:
		out.Status = "unsupported"
		out.Note = "the seat runs without --slot-save-path (re-render the node)"
		writeJSON(w, http.StatusNotImplemented, out)
		return
	case resp.StatusCode/100 != 2:
		out.Status = "error"
		out.Note = strings.TrimSpace(string(raw))
		code := http.StatusBadGateway
		if action == "restore" && resp.StatusCode == http.StatusBadRequest {
			code = http.StatusNotFound // llama-server answers 400 for a missing/incompatible file
			out.Status = "miss"
		}
		writeJSON(w, code, out)
		return
	}
	// The upstream body is the ONLY evidence that anything happened. Parsing it
	// with a discarded error would turn a changed field name — or a truncated
	// read — into status:"ok" with zero tokens, and a delegator that trusted
	// that would skip a prefill it never actually restored. Measured shape
	// (llama.cpp b9934, binxarn 2026-09-21):
	//   save    {"id_slot":0,"filename":"…","n_saved":3240,"n_written":158937840,…}
	//   restore {"id_slot":0,"filename":"…","n_restored":3240,"n_read":158937840,…}
	var up struct {
		NSaved    int   `json:"n_saved"`
		NRestored int   `json:"n_restored"`
		NWritten  int64 `json:"n_written"`
		NRead     int64 `json:"n_read"`
	}
	if err := json.Unmarshal(raw, &up); err != nil {
		out.Status = "unparsed"
		out.Note = fmt.Sprintf("upstream answered 2xx but its body did not parse (%v): %.300s", err, string(raw))
		writeJSON(w, http.StatusBadGateway, out)
		return
	}
	if action == "save" {
		out.Tokens, out.Bytes = up.NSaved, up.NWritten
	} else {
		out.Tokens, out.Bytes = up.NRestored, up.NRead
	}
	if out.Tokens <= 0 {
		// Zero tokens is not a transport failure, so it is not an error — but it
		// is not a hit either. A restore that moved nothing MUST read as a miss,
		// or the caller skips a prefill that never happened.
		if action == "restore" {
			out.Status = "miss"
			out.Note = "upstream restored 0 tokens — the caller must prefill normally"
			writeJSON(w, http.StatusNotFound, out)
			return
		}
		out.Status = "empty"
		out.Note = "the seat's slot 0 held nothing to save"
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.Status = "ok"
	if action == "save" {
		// Never the file this request just wrote: it is the newest by mtime, so
		// the LRU pass reaches it only when it ALONE exceeds the cap — and then
		// the response would still say ok with a byte count for a file that no
		// longer exists. A full 32k slot is ~1.1 GB, so that is reachable, not
		// theoretical.
		s.sweepKVSlots(file)
	}
	writeJSON(w, http.StatusOK, out)
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

// SweepKVSlotDir deletes the oldest-modified *.bin files under dir until the
// total is at or under capBytes. Only slot files are touched, and never `keep`
// (the file the caller just wrote). Eviction is by MODIFICATION time, which is
// least-recently-WRITTEN, not least-recently-used: llama.cpp does not touch a
// file on restore, so a hot key that is read often and never rewritten ages out
// like a cold one. That is a known limitation of this pass, not an oversight —
// fixing it needs a read-side touch, which belongs with the delegator work that
// is blocked (ADR 0055).
func SweepKVSlotDir(dir string, capBytes int64, keep string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type f struct {
		path string
		size int64
		mod  time.Time
	}
	var files []f
	var total int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), kvSlotFileSuffix) {
			continue
		}
		if keep != "" && filepath.Join(dir, e.Name()) == keep {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, f{filepath.Join(dir, e.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, x := range files {
		if total <= capBytes {
			break
		}
		if err := os.Remove(x.path); err == nil {
			total -= x.size
		}
	}
	return nil
}
