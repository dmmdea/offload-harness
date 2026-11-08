// Package kvslot is the one implementation of llama-server's slot save/restore call (ADR 0056 Layer 2, ADR 0100),
// shared by the fleet node's HTTP lane (internal/fleetnode) and the agent loop's own retention (internal/pipeline).
// It sits below both so neither imports the other, and imports internal/agent only for the seat-pin probe, never the
// other way round.
//
// The call never starts a model: a cold seat has nothing worth saving and nowhere useful to restore to, and
// llama-swap's `/upstream/<seat>/...` would start it. The residency read, the config-pin check, the GPU-lease fence
// and the status mapping are the lane's, moved here whole so the loop and the HTTP lane cannot disagree about what a
// save or a restore means.
package kvslot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/netguard"
	"github.com/dmmdea/offload-harness/internal/seatload"
	"github.com/dmmdea/offload-harness/internal/servingtmpl"
	"github.com/dmmdea/offload-harness/internal/swapclient"
	"github.com/dmmdea/offload-harness/internal/volumes"
)

const (
	// FileSuffix is the extension of every slot file this package writes and the sweeper touches.
	FileSuffix = ".bin"
	// MinFreeBytes is the free space a save leaves alone: below it on the slot directory's volume, a save is skipped.
	MinFreeBytes = 6 << 30
	// BodyCap bounds the upstream's answer.
	BodyCap = 64 << 10
	// CallTimeout bounds one upstream save/restore (a full 32k slot is ~1.1 GB).
	CallTimeout = 5 * time.Minute
	// PinTimeout bounds the seat-pin probe before a call.
	PinTimeout = 10 * time.Second
	// ResidencyTimeout bounds the /running read before a call.
	ResidencyTimeout = 5 * time.Second
	// rosterTimeout bounds the alias-resolving roster read of SlotDir.
	rosterTimeout = 3 * time.Second
)

// KeyRe is the only key shape accepted: "k1-" + a sha256 hex. A file name derived from it can never escape the slot
// directory.
var KeyRe = regexp.MustCompile(`^k1-[0-9a-f]{64}$`)

// kvslotClient dials the node's own llama-swap. netguard's transport for the same reason every outbound client here uses
// it; the per-call timeouts ride the contexts.
var kvslotClient = &http.Client{Transport: netguard.SafeTransport(nil)}

// Response is the JSON answer of the node's two lane endpoints, and the shape of Do's verdict.
type Response struct {
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

// Call is one save or restore of slot 0 on the seat.
type Call struct {
	// Endpoint is the llama-swap root; Seat a model id or alias on it.
	Endpoint, Seat string
	// Key is validated by the caller against KeyRe; Dir is the slot directory the seat was started with.
	Key, Dir string
	// Action is "save" or "restore".
	Action string
	// PinSHA256, when set, is the caller's expectation of the seat's config pin: a live pin that differs is a
	// shape-mismatch, since a file is only restored into the identical seat shape.
	PinSHA256 string
	// BeforeUpstream, when set, runs once after every check passed and before the upstream request: the HTTP lane
	// extends its write deadline there.
	BeforeUpstream func()
}

// File is the slot file this call reads or writes.
func (c Call) File() string { return Path(c.Dir, c.Key) }

// Path is the slot file of a key under dir.
func Path(dir, key string) string { return filepath.Join(dir, key+FileSuffix) }

// Outcome is Do's verdict in the HTTP lane's own terms: Code is the status the lane answers with. A non-empty ErrMsg
// is an error body ({"error": ...}); otherwise Resp is the JSON body. Callers outside the lane read Resp.Status.
type Outcome struct {
	Code   int
	ErrMsg string
	Resp   Response
}

// OK reports a save or restore that moved tokens.
func (o Outcome) OK() bool { return o.ErrMsg == "" && o.Code == http.StatusOK && o.Resp.Status == "ok" }

func failure(code int, format string, a ...any) Outcome {
	return Outcome{Code: code, ErrMsg: fmt.Sprintf(format, a...)}
}

// Do runs the call: residency check (a cold seat is never started), config-pin check, the GPU-lease fence, the
// upstream request and the mapping of its answer. Zero tokens on a restore is a miss, a 501 is "unsupported", a 400
// on a restore is a miss. A restore that moved tokens touches its file, so the LRU sweep (by mtime) keeps hot keys.
func Do(ctx context.Context, c Call) Outcome {
	lctx, lcancel := context.WithTimeout(ctx, ResidencyTimeout)
	reading, lerr := seatload.Running(lctx, kvslotClient, c.Endpoint, c.Seat)
	lcancel()
	if lerr != nil {
		return failure(http.StatusServiceUnavailable, "seat state unreadable: %v", lerr)
	}
	if !reading.Loaded {
		return Outcome{Code: http.StatusConflict, Resp: Response{Status: "seat-cold", Seat: c.Seat, Key: c.Key, Action: c.Action,
			Note: "the seat is not loaded and this lane never starts one; warm it through the normal path first"}}
	}
	// One stated slot only, on both doors. Slot 0 of a seat with several slots belongs to whichever request last held it,
	// so a save there stores another request's context and a restore overwrites a live one. An entry that states no
	// --parallel runs the server's default of several (the e2b entries through the shared macro do).
	if _, ok, why := SlotDir(ctx, c.Endpoint, c.Seat); !ok {
		return Outcome{Code: http.StatusConflict, Resp: Response{Status: "not-eligible", Seat: c.Seat, Key: c.Key, Action: c.Action,
			Note: why}}
	}
	// A save is a whole-context file (about 80 KB per token on a 9B with q8_0 KV, gigabytes for a full window), and the
	// cap sweep runs after it; on a nearly full disk it would take the ledger and the logs down with it. Skip instead.
	if c.Action == "save" && c.Dir != "" {
		if free, err := volumes.FreeBytes(c.Dir); err == nil && free < MinFreeBytes {
			return Outcome{Code: http.StatusInsufficientStorage, Resp: Response{Status: "disk-low", Seat: c.Seat, Key: c.Key, Action: c.Action,
				Note: fmt.Sprintf("%d MiB free on the slot directory's volume; a save is skipped below %d MiB", free>>20, uint64(MinFreeBytes)>>20)}}
		}
	}
	if c.PinSHA256 != "" {
		pctx, pcancel := context.WithTimeout(ctx, PinTimeout)
		pin, ok := agent.ProbeSeatPin(pctx, c.Endpoint, c.Seat)
		pcancel()
		if !ok {
			return failure(http.StatusServiceUnavailable, "seat pin unreadable (the seat may be cold); retry after a warm-up")
		}
		if !strings.EqualFold(pin.SHA256, c.PinSHA256) {
			return Outcome{Code: http.StatusConflict, Resp: Response{Status: "shape-mismatch", Seat: c.Seat, Key: c.Key, Action: c.Action,
				Note: "the live seat's config pin differs from the caller's; a slot file is only restored into the identical seat shape"}}
		}
	}
	if c.BeforeUpstream != nil {
		c.BeforeUpstream()
	}
	uctx, ucancel := context.WithTimeout(ctx, CallTimeout)
	defer ucancel()
	// The residency read above already refuses a cold seat; the GPU-lease fence (no-wait form) closes the window
	// between that read and this request for a fenced card, and is the one builder of an /upstream URL in the harness.
	upstream, ferr := modelaffinity.AwaitUpstream(uctx, c.Endpoint, c.Seat, "/slots/0?action="+c.Action, time.Now())
	if ferr != nil {
		return Outcome{Code: http.StatusConflict, Resp: Response{Status: "seat-cold", Seat: c.Seat, Key: c.Key, Action: c.Action,
			Note: "a GPU lease holds the card and the seat is not resident; this lane never starts one: " + ferr.Error()}}
	}
	body, _ := json.Marshal(map[string]string{"filename": c.Key + FileSuffix})
	ureq, err := http.NewRequestWithContext(uctx, http.MethodPost, upstream, bytes.NewReader(body))
	if err != nil {
		return failure(http.StatusInternalServerError, "building upstream request: %v", err)
	}
	ureq.Header.Set("Content-Type", "application/json")
	t0 := time.Now()
	resp, err := kvslotClient.Do(ureq)
	if err != nil {
		return failure(http.StatusBadGateway, "forwarding to %s: %v", upstream, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, BodyCap))
	ms := time.Since(t0).Milliseconds()
	out := Response{Seat: c.Seat, Key: c.Key, Action: c.Action, Ms: ms, Upstream: resp.Status}
	switch {
	case resp.StatusCode == http.StatusNotImplemented:
		out.Status = "unsupported"
		out.Note = "the seat runs without --slot-save-path (re-render the node)"
		return Outcome{Code: http.StatusNotImplemented, Resp: out}
	case resp.StatusCode/100 != 2:
		out.Status = "error"
		out.Note = strings.TrimSpace(string(raw))
		code := http.StatusBadGateway
		if c.Action == "restore" && resp.StatusCode == http.StatusBadRequest {
			code = http.StatusNotFound // llama-server answers 400 for a missing/incompatible file
			out.Status = "miss"
		}
		return Outcome{Code: code, Resp: out}
	}
	// The upstream body is the ONLY evidence that anything happened. Parsing it with a discarded error would turn a
	// changed field name — or a truncated read — into status:"ok" with zero tokens, and a caller that trusted that
	// would skip a prefill it never actually restored. Measured shape (llama.cpp b11977, 2026-11-08):
	//   save    {"id_slot":0,"filename":"…","n_saved":3279,"n_written":267939204,"timings":{"save_ms":250.9}}
	//   restore {"id_slot":0,"filename":"…","n_restored":3279,"n_read":267939204,"timings":{"restore_ms":299.0}}
	var up struct {
		NSaved    int   `json:"n_saved"`
		NRestored int   `json:"n_restored"`
		NWritten  int64 `json:"n_written"`
		NRead     int64 `json:"n_read"`
	}
	if err := json.Unmarshal(raw, &up); err != nil {
		out.Status = "unparsed"
		out.Note = fmt.Sprintf("upstream answered 2xx but its body did not parse (%v): %.300s", err, string(raw))
		return Outcome{Code: http.StatusBadGateway, Resp: out}
	}
	if c.Action == "save" {
		out.Tokens, out.Bytes = up.NSaved, up.NWritten
	} else {
		out.Tokens, out.Bytes = up.NRestored, up.NRead
	}
	if out.Tokens <= 0 {
		// Zero tokens is not a transport failure, so it is not an error — but it is not a hit either. A restore that
		// moved nothing MUST read as a miss, or the caller skips a prefill that never happened.
		if c.Action == "restore" {
			out.Status = "miss"
			out.Note = "upstream restored 0 tokens — the caller must prefill normally"
			return Outcome{Code: http.StatusNotFound, Resp: out}
		}
		out.Status = "empty"
		out.Note = "the seat's slot 0 held nothing to save"
		return Outcome{Code: http.StatusOK, Resp: out}
	}
	out.Status = "ok"
	if c.Action == "restore" {
		// llama.cpp does not touch a file it reads, and the sweep is by modification time: without this a hot key that
		// is restored often and never rewritten ages out like a cold one.
		now := time.Now()
		_ = os.Chtimes(c.File(), now, now)
	}
	return Outcome{Code: http.StatusOK, Resp: out}
}

// SweepDir deletes the oldest-modified *.bin files under dir until the total is at or under capBytes. Only slot files
// are touched, and never `keep` (the file the caller just wrote). Eviction is by MODIFICATION time; a restore touches
// its file (Do), so it is least-recently-USED for every key the node restores through this package.
func SweepDir(dir string, capBytes int64, keep string) error {
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
		// Only the harness's own slot files: the directory comes from a seat's command line, and a hand-kept yaml that
		// points --slot-save-path at a model folder must never cost a whisper ggml-*.bin or any other .bin there.
		if e.IsDir() || !strings.HasSuffix(e.Name(), FileSuffix) || !KeyRe.MatchString(strings.TrimSuffix(e.Name(), FileSuffix)) {
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

// SlotDir is the --slot-save-path the RUNNING seat was started with, read from the command llama-swap spawned (GET
// /running, "cmd"), so the node never depends on OFFLOAD_HOME agreeing with the render. seat may be an alias: it is
// resolved through the roster the way every other /running reader does (seatload, swapclient.Roster.Canonical).
//
// ok=false, with the reason, when the seat is not running, its command carries no --slot-save-path, or it runs more
// than one slot (or states none, so the build's default of several applies): slot 0 would not be this session's.
func SlotDir(ctx context.Context, endpoint, seat string) (dir string, ok bool, why string) {
	names := []string{seat}
	rctx, cancel := context.WithTimeout(ctx, rosterTimeout)
	roster, rerr := swapclient.FetchRosterWith(rctx, kvslotClient, endpoint, rosterTimeout)
	cancel()
	if rerr == nil {
		if id, found := roster.Canonical(seat); found && !strings.EqualFold(id, seat) {
			names = append(names, id)
		}
	}
	rows, err := seatload.Occupants(ctx, kvslotClient, endpoint)
	if err != nil {
		return "", false, "running list unreadable: " + err.Error()
	}
	cmd, found := "", false
	for _, m := range rows {
		if m.State == "stopped" || m.State == "shutdown" {
			continue
		}
		for _, n := range names {
			if strings.EqualFold(m.Model, n) {
				cmd, found = m.Cmd, true
			}
		}
	}
	if !found {
		return "", false, "seat not running"
	}
	dir, has := servingtmpl.CmdSlotSavePath(cmd)
	if !has {
		return "", false, "seat runs without --slot-save-path"
	}
	n, stated := servingtmpl.CmdParallel(cmd)
	switch {
	case !stated:
		return "", false, "seat states no --parallel (several slots by default)"
	case n > 1:
		return "", false, fmt.Sprintf("seat runs %d slots", n)
	}
	return dir, true, ""
}
