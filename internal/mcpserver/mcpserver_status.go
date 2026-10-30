// offload_status: the status tool and its local, media, remote, KV-cache, fleet and seat views.
package mcpserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"llamaswap-pp-cli/pkg/llamaswap"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
	"github.com/dmmdea/offload-harness/internal/cache"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/embedmemo"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/mediacap"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/nimclient"
	"github.com/dmmdea/offload-harness/internal/seatload"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// handleStatus (LO-18): the capability-discovery tool. Reports the configured
// LOCAL model roster, live-probes the local endpoint's /v1/models, lists this
// machine's media engines, and names the single remote surface (NIM). A failed
// live probe is reported alongside the roster — never a defer: the configured
// roster is the answer even when the stack is cold.
//
// One optional argument, section (status_section.go): the default is the whole
// payload, byte-identical to the answer before the argument existed; a block
// name returns that block alone and computes nothing else; brief is the sizing
// answer (the fleet block plus one-line lease and local verdicts).
func (s *Server) handleStatus(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	section, bad := parseStatusSection(req.Params.Arguments)
	if bad != nil {
		return bad, nil
	}
	payload := s.statusPayload(ctx, s.p.Cfg(), section)
	if note := s.staleBinaryNote(buildinfo.Version); note != "" {
		payload["stale_binary"] = note
	}
	// The guard that reports a swapped binary says when it cannot: absent while it is watching.
	if state := s.exe.guardState(); state != "" {
		payload["stale_binary_guard"] = state
	}
	if s.configErr != nil {
		// FIRST key, not just present: this is the one tool still answering while
		// every other one defers, so the reason has to be the first thing read —
		// and a Go map marshals in sorted key order, which would bury it.
		return jsonResultFirst("config_error", s.configErr.Error(), payload)
	}
	return jsonResult(payload)
}

// statusLocal is the local block: the configured roster, the live served ids
// and, on a composite box, the layer rows.
func (s *Server) statusLocal(ctx context.Context, cfg config.Config) map[string]any {
	// One table, shared with doctor/acceptance/report (config.ModelRoutes). The
	// two lists used to be hardcoded separately and had drifted — this surface
	// published 10 keys while the doctor gate checked 8. Effective, not
	// Configured: a planner needs the seat that will ACTUALLY run once the
	// agent→workhorse, ocr→vision and embed fallbacks are applied.
	roster := map[string]any{}
	for _, r := range cfg.ModelRoutes() {
		roster[r.StatusKey] = r.Effective
	}
	local := map[string]any{
		"endpoint": cfg.Endpoint,
		"roster":   roster,
		"note":     "every offload_* tool except offload_nim and offload_browse runs on LOCAL models — the GPU roster or a listed accelerator — free, on-box, no cloud; an empty entry means that capability defers on this machine",
	}
	if ids, err := probeServedModels(ctx, cfg.Endpoint); err != nil {
		local["served_probe_error"] = err.Error()
	} else {
		local["served_now"] = ids
	}
	// Composite identity and layer rows (ADR 0052), emitted ONLY when the box
	// seeds layers: on every other box this surface must stay byte-identical,
	// so there is no empty tiers list and no layers: [] to read as "none".
	if cfg.Composite() {
		local["tier_profile"] = cfg.TierProfile
		local["tiers"] = cfg.Tiers
		local["layers"] = layersView(ctx, cfg)
	}
	return local
}

// statusMedia is the media block: this machine's media routes, derived.
func statusMedia(cfg config.Config) map[string]any {
	// Media capability is DERIVED from this machine's bindings and the files they
	// name (internal/mediacap) — never declared. This block used to state
	// "image_engine": "ComfyUI (local)" as a constant, and shipped that to an
	// autonomous planner on a node whose imagegen_engine is stable-diffusion.cpp
	// and which has no ComfyUI installed at all. A capability map the planner acts
	// on is worse wrong than absent.
	mediaRoutes := mediacap.Routes(cfg)
	media := map[string]any{
		"routes": mediacap.Map(mediaRoutes),
		// Every binding a request's `family` param can select (ADR 0058), the
		// default first, each with its license and its route verdict — a planner
		// picks a family from here, and a null license means UNKNOWN, not safe.
		"image_families": mediacap.ImageFamilyRows(cfg, mediaRoutes),
		"edit_families":  mediacap.EditFamilyRows(cfg, mediaRoutes),
		// video_family_bindings: what EACH video family resolves to on this box —
		// an explicit videogen_families[name] entry when one is bound, else the
		// box's flat videogen_* keys (which may intentionally belong to a
		// DIFFERENT family, the back-compat fallback). See mediacap.
		// VideoFamilyBindingRows / config.Config.ResolveVideoFamilyBinding.
		"video_family_bindings": mediacap.VideoFamilyBindingRows(cfg),
		"image_ckpt":            cfg.ImageGenCkpt, // "" = the render script's default checkpoint
		"video_upscale_model":   cfg.VideoGenUpscaleModel,
		"svg_engine":            "deterministic component kit (in-process Go, no model, no engine)",
		"note": "routes are derived from THIS machine's config: CONFIGURED = bound and the file exists; " +
			"BOUND-BUT-MISSING = the configured path is absent, so that task defers when called; " +
			"NOT CONFIGURED = no route on this box. vqa/ocr/transcribe ride the vision/stt models in local.roster.",
	}
	return media
}

// statusRemote is the remote block: offload_nim (the one remote MODEL surface) and the
// opt-in browse lane's loopback decision endpoint (ADR 0060), which the harness calls
// with no key of its own — whatever that local service fronts is the operator's choice.
func statusRemote(cfg config.Config) map[string]any {
	remote := map[string]any{
		"nim_endpoint":        cfg.NIMEndpoint,
		"nim_default_model":   cfg.NIMModel,
		"nim_key_present":     nimclient.KeyForBase(cfg.NIMEndpoint) != "",
		"browse_configured":   cfg.BrowseConfigured(),
		"browse_decision_url": cfg.BrowseDecisionURL,
		"browse_activate_tab": cfg.EffectiveBrowseActivateTab(),
		"note":                "offload_nim is the only remote MODEL tool on this server (opt-in escalation); offload_browse (opt-in, ADR 0060) sends a browse run's typed choices to the loopback decision endpoint above and holds no key",
	}
	// browse_activate_tab is the EFFECTIVE value (the sidecar is told to activate its tab only
	// with a dedicated browse_cdp_url). A key that is set but has nothing to honour it against
	// says so here instead of reading as a silent false.
	if cfg.BrowseActivateTabIgnored() {
		remote["browse_activate_tab_note"] = cfg.BrowseActivateTabIgnoredNote()
	}
	return remote
}

// statusReuse is the reuse block: the embed memo's counters and the result
// cache's state, both read without taking a writer's lock.
func (s *Server) statusReuse(cfg config.Config) map[string]any {
	// T2-C: the embed memo's live counters. This server owns the bbolt handle, so
	// it is the ONLY process that can read them while it runs — `loupe` reports
	// "held by a running local-offload process" and points here. Publishing them
	// from both places is what keeps the measurement answerable in either state
	// rather than only when the server happens to be down.
	reuse := map[string]any{}
	st, reason := s.p.EmbedMemoStats()
	// THIS PROCESS may hold no memo (the kNN pre-filter is off by default), while
	// the STORE on disk is being written by `shadow-label` against the same file.
	// Reporting only "enabled: false" then told the operator the feature was off
	// while `loupe` on the same box showed vectors and a hit rate — two surfaces,
	// opposite answers. Worse, the fault counters this block exists to publish
	// would be unreachable on the one configuration everyone actually runs.
	// So: fall back to a read-only look at the store, exactly as loupe does.
	storeOnly := false
	if reason != "" {
		if ro, err := embedmemo.OpenReadOnly(cfg.EmbedMemoPath, cfg.EmbedModel(), cfg.EmbedMemoEpoch); err == nil {
			if rst, rerr := ro.Stats(); rerr == nil {
				st, storeOnly = rst, true
			}
			ro.Close()
		}
	}
	if reason == "" || storeOnly {
		reuse["embed_memo"] = map[string]any{
			"enabled":            reason == "",
			"scope":              map[bool]string{true: "read from the store on disk; this process is not memoizing", false: "live in this process"}[storeOnly],
			"process_reason":     reason,
			"count_underflows":   st.CountUnderflows,
			"last_fault":         st.LastFault,
			"foreign_namespaces": st.ForeignNamespaces,
			"foreign_vectors":    st.ForeignVectors,
			"file_bytes":         st.FileBytes,
			"embedder":           st.EmbedderID,
			"epoch":              st.Epoch,
			"dim":                st.Dim,
			"distinct":           st.Distinct,
			"session_hits":       st.SessionHits,
			"session_misses":     st.SessionMisses,
			"session_stores":     st.SessionStores,
			"lifetime_hits":      st.LifetimeHits,
			"lifetime_misses":    st.LifetimeMisses,
			"lifetime_stores":    st.LifetimeStores,
			// Fault counters are published, not merely incremented. An unpublished
			// fault counter is not a fault signal: without these there is no
			// observable difference between a memo that is working, one whose store
			// fails every write, and one full of corrupt records.
			"errors_decode":  st.ErrorsDecode,
			"errors_read":    st.ErrorsRead,
			"errors_write":   st.ErrorsWrite,
			"dim_mismatches": st.DimMismatches,
			// nil, not 0, when nothing has been looked up — a zero here would
			// report a measured failure where there is no measurement.
			"hit_rate": st.HitRate,
			"note":     "each hit skips an embedding call, and therefore also skips the ~1-2s cold load the ttl=300 embedder pays after an idle gap",
		}
	} else {
		// One specific reason, never a menu of possibilities the caller has to
		// guess between — and it names WHERE the answer is, since a store may
		// exist and be in use by another process.
		reuse["embed_memo"] = map[string]any{
			"enabled": false,
			"reason":  reason,
			"store":   "not readable from this process either; run `local-offload loupe` with the server stopped to inspect the store on disk",
		}
	}
	// The result cache is the other half of T2-D and was previously reported
	// nowhere. If it failed to open at startup, every agent_run silently loses
	// the in-loop cache and the only signal was one stderr line an MCP stdio
	// client never sees.
	// 0.121.1 (register D-05): the handle is lazy and this block must NOT resolve
	// it — a status call that took the bbolt lock would be the very thing that
	// creates the sibling it is reporting on. Everything here reads state the
	// handle already holds, plus a stat of the cache directory.
	if c := s.p.Cache(); c != nil {
		mode := c.Mode()
		sibCount, sibBytes := cache.SiblingStats(cfg.CachePath)
		rc := map[string]any{
			"mode":          string(mode),
			"available":     mode == cache.ModePrimary || mode == cache.ModeSibling || mode == cache.ModeReadOnly,
			"path":          c.Path(),
			"configured":    cfg.CachePath,
			"fallback":      mode == cache.ModeSibling,
			"siblings":      sibCount,
			"sibling_bytes": sibBytes,
		}
		// How many results the SHARED cache actually holds is the operator's real
		// D-05 question ("is everyone running cache-less?"), and answering it is a
		// pure read — so it goes through the read-only handle, which never creates
		// a file and never takes the writer's exclusive lock. When this server is
		// itself the primary holder, its own handle already has the answer and no
		// second open is needed (nor possible: our own exclusive lock excludes it).
		if mode == cache.ModePrimary {
			if n, err := c.Count(); err == nil {
				rc["entries"], rc["entries_from"] = n, c.Path()
			}
		} else if n, from := readOnlyCacheEntries(cfg.CachePath); n >= 0 {
			rc["entries"], rc["entries_from"] = n, from
		}
		switch mode {
		case cache.ModeUnopened:
			// The steady state after D-05, and deliberately not called a failure:
			// nothing on this server has needed the cache yet, so no lock was
			// taken and no file was created. The first cacheable offload resolves it.
			rc["available"] = false
			rc["note"] = "lazy: no cacheable task has run on this server yet, so the cache is not open and no bbolt lock is held — the first agent_run offload opens it"
		case cache.ModePrimary:
			rc["note"] = "agent_run's in-loop offloads share this cache (nil ledger, shared cache)"
		case cache.ModeSibling:
			// 0.113.21: the configured file is held by another harness process;
			// this server's hits live in its own per-process sibling.
			rc["note"] = "PER-PROCESS fallback: the configured cache is held by another local-offload process, so this server's agent_run hits live in the sibling file named here (not shared with other sessions); on shutdown it is deleted if empty, or promoted into the configured cache if the lock is free"
		case cache.ModeReadOnly:
			rc["note"] = "read-through reader: this handle can serve hits but never writes and never creates a file"
		case cache.ModeUnavailable:
			rc["available"] = false
			rc["reason"] = unavailableCacheReason(cfg.CachePath, c.OpenErr())
		}
		reuse["result_cache"] = rc
	} else {
		sibCount, sibBytes := cache.SiblingStats(cfg.CachePath)
		reuse["result_cache"] = map[string]any{
			"mode":          string(cache.ModeUnavailable),
			"available":     false,
			"path":          cfg.CachePath,
			"configured":    cfg.CachePath,
			"siblings":      sibCount,
			"sibling_bytes": sibBytes,
			"reason":        unavailableCacheReason(cfg.CachePath, nil),
		}
	}
	return reuse
}

// localLeaseView publishes THIS box's machine-wide GPU lease (0.115.2). The fleet
// block already carried every remote node's lease verdict, while the local card —
// the one a session at this desk is about to measure on — was reported nowhere in
// this tool, so sessions read `nvidia-smi` instead and concluded "busy, refuse".
// Held or free, the block ends with the queue command: a held card is a place in
// line, and the line is one flag. Read-only (delegate.LocalLease never acquires).
func localLeaseView(ctx context.Context, cfg config.Config) map[string]any {
	view, _ := localLeaseViewWithActivity(ctx, cfg, true)
	return view
}

// localLeaseViewWithActivity is localLeaseView plus the activity reading behind it, so a
// caller that also wants the per-card table (the brief) reuses the one nvidia-smi sample
// instead of taking a second. driverEvents says whether the caller RETURNS the driver's fault events
// (gpu_lease.activity.driver_events): the read starts a PowerShell child, so the brief, which returns no event, passes false
// and a section computes only what it returns.
func localLeaseViewWithActivity(ctx context.Context, cfg config.Config, driverEvents bool) (map[string]any, gpuactivity.View) {
	info := delegate.LocalLease(cfg.GPULockPath, cfg.StateDir)
	// What the cards are DOING, not only whether they are held (0.117.0,
	// register D-93): the seat's in-flight count and load state, the runs the
	// harness itself has registered, a utilization sample, and one verdict a
	// session can branch on — working / held-idle / held-working / loaded-idle /
	// busy-outside / stale-holder / free, and for a held lease that is not healthy
	// held-stalled / held-orphaned / held-overdue / tree-orphan. A session used to read "held" as
	// "refuse"; now it can read whether the holder is actually using the cards.
	var events func(context.Context) ([]gpuactivity.DriverEvent, error)
	if driverEvents {
		events = statusDriverEventsReader()
	}
	act := gpuactivity.Snapshot(ctx, gpuactivity.Options{LockOverride: cfg.GPULockPath, StateDir: cfg.StateDir, Endpoint: cfg.Endpoint, Seat: cfg.AgentPlannerModel(""), SampleGPU: statusSamplesGPU, Sampler: statusGPUSampler, ProcSampler: statusGPUProcSampler, Scope: modelaffinity.ScopeFunc(cfg.GPULockPath, cfg.StateDir), OrphanGrace: cfg.GPUOrphanGrace(), ComfyDir: cfg.ComfyDir, DriverEvents: events})
	view := map[string]any{
		"held":       info.Held,
		"queue_with": gpulease.QueueHint,
		"verdict":    act.Verdict,
		"activity":   act.Map(),
	}
	// The line behind the holder and the warm the last of them owes the seat
	// (0.129.2, register D-124) — read-only, nothing is acquired.
	var leaseMgr *gpulease.Manager
	if m, err := gpulease.OpenAt(cfg.GPULockPath, cfg.StateDir); err == nil {
		leaseMgr = m
		if ws := m.Waiters(); len(ws) > 0 {
			view["queued"] = len(ws)
		}
		if seat := m.SeatWarmOwed(); seat != "" {
			view["seat_warm_owed"] = seat
		}
	}
	// The host's memory and what the live leases declared of it (2026-10-09, the paging incident):
	// physical, available, commit used and limit, the declared sum, and one verdict, OK / NEAR / OVER.
	// OVER leads the brief line in capitals. Present held or free: a box can be over with no lease at all.
	view["host_memory"] = hostMemoryView(cfg, leaseMgr)
	if !info.Held {
		view["note"] = "free (unreserved): a bench or training run on this box is exposed until it takes the lease — wrap it in the queue_with command"
		return view, act
	}
	// With several live leases (card-scoped), `info` is the LOWEST epoch while the verdict and
	// `activity.holder` are about the MOST ESCALATED one. Everything below describes that
	// lease, from its own record, so the epoch, the owner and the progress contract on this
	// view and the takeover command built from it always belong to the lease the verdict names.
	info = gpuactivity.HeadlineOf(info, act.Holder)
	view["class"] = string(info.Class)
	view["epoch"] = info.Epoch
	view["pid"] = info.PID
	view["age_s"] = int(info.Age.Seconds())
	view["reason"] = info.Reason
	view["origin"] = info.Origin
	view["exclusive"] = info.Exclusive
	view["draining"] = info.Draining
	if info.Command != "" {
		view["command"] = info.Command
	}
	// Who asked for the lease and the contract it is judged by (plan P8); the derived
	// standing (owner state, orphaned-since, progress age) is under activity.holder.
	if info.Owner != nil {
		view["owner"] = info.Owner
	}
	if info.Unattended {
		view["unattended"] = true
	}
	if info.Progress != nil {
		view["progress"] = info.Progress
	}
	if !info.ExpiresAt.IsZero() {
		view["expires_at"] = info.ExpiresAt.Format(time.RFC3339)
	}
	view["note"] = "held: delegations already route to other nodes; a bench or render here must QUEUE behind the holder with queue_with, never be refused or deferred; `verdict`/`activity` say whether the holder is actually using the cards (held-working) or sitting on them idle (held-idle)"
	return view, act
}

// kvCacheServerView reports the OPTIONAL cache-server tier (config.KVCacheServers):
// a second machine's RAM holding the KV pages that left a vLLM seat's VRAM.
//
// It lists EVERY binding, one per vLLM seat, because the tier is not a property of
// one favoured seat: the operator directive of 2026-09-10 is that the store backs
// every vLLM seat while the second device is online, and the shape this replaces
// reported a single `seat` — so a session reading status could not tell "the other
// seats have no tier" from "the other seats were never considered" (B-01).
//
// Absent or disabled is a normal, fully-described state — the tier never gates a
// tool, so tools/list is byte-identical either way. Each enabled binding is
// re-validated first: a server running on a config whose load was refused
// (LoadWithSource hands the decoded config back with the error) must say so and must
// NOT act on the refused address. `unbound_seats` is the same list `doctor` fails on,
// computed by the same code, so the gate and this report cannot disagree.
func kvCacheServerView(ctx context.Context, cfg config.Config) map[string]any {
	list := cfg.KVCacheServers
	unbound := list.UnboundSeats(cfg.VLLMSeats)
	out := map[string]any{
		"enabled":       list.AnyEnabled(),
		"declared":      len(list),
		"vllm_seats":    cfg.VLLMSeats,
		"unbound_seats": unbound,
	}
	bindings := make([]map[string]any, 0, len(list))
	for _, k := range list {
		bindings = append(bindings, kvCacheBindingView(ctx, k))
	}
	out["bindings"] = bindings
	switch {
	case len(list) == 0:
		out["note"] = "no cache server: every vLLM seat's KV lives in VRAM only; declare kv_cache_server in config.json as a LIST of per-seat bindings when a second device can hold evicted KV (optional tier, off by default)"
	case len(unbound) > 0:
		out["note"] = "as declared in config.json (each seat wrapper's seat.env is what that engine actually runs — keep them in agreement); " +
			"unbound_seats are vLLM seats of this box with NO binding: give each one a store or an explicit {\"storeless\":true,\"reason\":\"…\"} opt-out — `local-offload doctor` fails on them"
	default:
		out["note"] = "as declared in config.json (each seat wrapper's seat.env is what that engine actually runs — keep them in agreement); contexts that leave a seat's VRAM come back from its store at parity cost instead of being recomputed and survive a seat swap; ~255 KB/token as stored"
	}
	if extra := list.BoundSeatsNotDeclared(cfg.VLLMSeats); len(extra) > 0 {
		out["bound_seats_not_in_vllm_seats"] = extra
	}
	return out
}

// kvCacheBindingView is ONE binding's row: its wiring, and for a Valkey store named
// by an IP LITERAL a 1 s TCP reachability fact — a hostname is vetted by shape, not
// by what DNS answers, so it is reported as unprobed rather than dialed (the
// two-layer reasoning behind netguard's tailnet dial gate); fs_native has no port to
// dial and says so explicitly, never by omission.
func kvCacheBindingView(ctx context.Context, k *config.KVCacheServer) map[string]any {
	if k == nil {
		return map[string]any{"invalid": "null binding"}
	}
	seat := k.Seat
	if seat == "" {
		seat = "(box default: every vLLM seat with no binding of its own)"
	}
	if k.Storeless {
		return map[string]any{
			"seat":      seat,
			"enabled":   false,
			"storeless": true,
			"reason":    k.Reason,
			"note":      "explicit opt-out: this seat runs on VRAM plus L1 staging only, on purpose",
		}
	}
	if !k.Enabled {
		return map[string]any{
			"seat":     seat,
			"enabled":  false,
			"declared": true,
			"note":     "declared but disabled, with no storeless reason — `doctor` treats this seat as unbound, because \"the tier is off here\" and \"nobody considered this seat\" must not look alike",
		}
	}
	if err := config.ValidateKVCacheServer(k); err != nil {
		return map[string]any{
			"seat":     seat,
			"enabled":  true,
			"declared": true,
			"invalid":  err.Error(),
			"note":     "this binding was refused at config load; the server is running on a config that failed validation — fix config.json and restart. The store was not dialed.",
		}
	}
	view := map[string]any{
		"seat":                 seat,
		"enabled":              true,
		"declared":             true,
		"store":                k.StoreName(),
		"address":              k.Address,
		"l1_staging_gb":        k.EffectiveL1StagingGB(),
		"chunk_size":           k.EffectiveChunkSize(),
		"chunk_size_defaulted": k.ChunkSizeDefaulted(),
		"key_prefix":           k.EffectiveKeyPrefix(),
	}
	if k.KVDtype != "" {
		view["kv_dtype"] = k.KVDtype
	}
	if k.TensorParallel > 0 {
		view["tensor_parallel"] = k.TensorParallel
	}
	switch {
	case k.StoreName() != "valkey":
		fsNativeReachability(k, view)
	case !k.AddressIsIPLiteral():
		view["reachable"] = nil
		view["reachable_note"] = "hostname not probed: only an IP-literal store address is dialed (a name is vetted by shape, not by what DNS answers)"
	default:
		dctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		var d net.Dialer
		conn, err := d.DialContext(dctx, "tcp", k.Address)
		if err != nil {
			view["reachable"] = false
			view["reachable_error"] = err.Error()
		} else {
			_ = conn.Close()
			view["reachable"] = true
		}
	}
	return view
}

// fsNativeReachability fills the reachable fields of an fs_native binding from the
// seat wrapper's own verdict file (B-29). There is no port to dial: the store is a
// mounted path, and whether it is usable is decided at seat start by seat_fg.sh
// (mount + 64 MiB write probe), which writes the file SEAT_L2_STATUS_FILE names
// (`seat-l2-<seat id>.status` in a rendered seat env) either way. Reading
// that file is the end-to-end readback; guessing from the path would be the same
// silence the status field is against.
func fsNativeReachability(k *config.KVCacheServer, view map[string]any) {
	view["reachable"] = nil
	if strings.TrimSpace(k.StatusFile) == "" {
		view["reachable_note"] = "fs_native: a mounted path, no port to probe; declare status_file (this seat's wrapper verdict file, SEAT_L2_STATUS_FILE in its seat env: seat-l2-<seat id>.status when rendered) to publish the wrapper's mount + write-probe verdict here"
		return
	}
	view["status_file"] = k.StatusFile
	raw, err := os.ReadFile(k.StatusFile)
	if err != nil {
		view["reachable_note"] = "status_file unreadable: the seat has not started since it was declared, or the path is wrong (" + err.Error() + ")"
		return
	}
	line := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
	view["status_line"] = line
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		if ts, perr := time.Parse(time.RFC3339, fields[1]); perr == nil {
			view["status_age_s"] = int(time.Since(ts).Seconds())
		}
	}
	switch {
	case strings.HasPrefix(line, "ok "):
		view["reachable"] = true
		view["reachable_note"] = "fs_native: the seat wrapper mounted the share and its write probe passed at the last seat start (status_line)"
	case strings.HasPrefix(line, "degraded "):
		view["reachable"] = false
		view["reachable_error"] = strings.TrimSpace(strings.TrimPrefix(line[strings.Index(line, "reason=")+len("reason="):], ""))
		if !strings.Contains(line, "reason=") {
			view["reachable_error"] = line
		}
		view["reachable_note"] = "fs_native: the seat wrapper DEGRADED to L1-only at the last seat start — the store served nothing since; fix the path and restart the seat"
	default:
		view["reachable_note"] = "status_file present but unparsed (expected `ok <stamp> …` or `degraded <stamp> reason=…`)"
	}
}

// fleetProbeTimeout bounds the whole fleet section. Node health is a CACHED
// read on the node side (it never blocks on llama-swap), so a node that cannot
// answer inside this is down, not busy — and status must stay snappy. Probes
// run concurrently, so this is the wall for the section, not per node.
const fleetProbeTimeout = 8 * time.Second

// fleetView is the LIVE delegation roster: who the delegation seats are and
// what they can take RIGHT NOW.
//
// It exists because capability was being ASSERTED in documents instead of
// reported by the system, and the documents went stale in a way that silently
// suppressed delegation for weeks. Three independent sources (a rules file, a
// tier matrix, and the config this server loads) each described a live, resident
// agent seat as absent or half-capable; there was no runtime surface that could
// contradict them. A caller could not learn the truth at any price.
//
// So the numbers a caller needs to size and place work — the agent seat, its
// context ceiling, whether it is enabled and resident, and how deep its queue is
// — are published here, probed, every call. A written figure is now always the
// weaker source.
//
// A probe failure is REPORTED, never swallowed and never a defer: "the node is
// unreachable" and "the node is busy" route completely differently for the
// caller, and the local seat's answer is still the answer.
func (s *Server) fleetView(ctx context.Context, cfg config.Config) map[string]any {
	out := map[string]any{
		"delegation_enabled": cfg.AgentDelegationEnabled,
		"note": "LIVE capability, probed at call time — trust this over any rules file, matrix or doc. " +
			"Size contracts from ctx_tokens here, never from a written figure.",
	}
	out["local_agent_seat"] = localSeatView(ctx, cfg)
	// How callers narrowed placement (ADR 0078). This server's own counters, so the window is stated in the block; the
	// ledger rows carry pin_reason and route_asked for any longer one. Absent when delegation is off: there is
	// no door to pin through.
	if cfg.AgentDelegationEnabled {
		out["pins"] = s.pinsView()
	}

	// One specific reason, never a menu the caller has to guess between. These
	// two states look identical from the outside (no nodes) and have completely
	// different fixes.
	switch {
	case !cfg.AgentDelegationEnabled:
		out["nodes"] = []any{}
		out["reason"] = "agent_delegation_enabled is false in the config THIS server loaded — agent_delegate is absent from tools/list and no node can be placed on"
		return out
	case len(cfg.DelegateRemotes) == 0:
		out["nodes"] = []any{}
		out["reason"] = "no delegate_remotes in the config THIS server loaded — delegation can only run on the local seat. If a node is live but missing here, the server is reading a different config file than the one being edited"
		return out
	}

	// The node probes get their OWN budget, deliberately not shared with the
	// local-seat probe above. Sharing one deadline across both made a slow local
	// probe eat the entire allowance and report every node as "context deadline
	// exceeded" — nodes that answer a curl in ~130 ms were published as
	// unreachable. A local stall must never be reported as a fleet outage.
	nodeCtx, cancel := context.WithTimeout(ctx, fleetProbeTimeout)
	defer cancel()

	type probe struct {
		base string
		view delegate.NodeView
		err  error
	}
	results := make([]probe, len(cfg.DelegateRemotes))
	var wg sync.WaitGroup
	for i, base := range cfg.DelegateRemotes {
		wg.Add(1)
		go func(i int, base string) {
			defer wg.Done()
			v, err := delegate.FetchNodeView(nodeCtx, base, cfg.FleetAuthToken)
			results[i] = probe{base: base, view: v, err: err}
		}(i, base)
	}
	wg.Wait()

	nodes := make([]any, 0, len(results))
	var capable, idle, fleetSlots int
	for _, r := range results {
		n := map[string]any{"base": r.base}
		if r.err != nil {
			n["reachable"] = false
			n["probe_error"] = r.err.Error()
			nodes = append(nodes, n)
			continue
		}
		n["reachable"] = true
		n["node_id"] = r.view.NodeID
		n["agent_enabled"] = r.view.AgentEnabled
		n["agent_seat"] = r.view.AgentSeat
		n["agent_ctx_tokens"] = r.view.AgentCtxTokens
		n["agent_seat_resident"] = r.view.AgentResident
		n["queue_depth"] = r.view.QueueDepth
		// The saturation triad. queue_depth alone cannot distinguish "this node
		// is chewing through work" from "this node is full and everything is
		// waiting", and since 0.100.0 those are different states with different
		// fixes. A `queue deadline` failure sends an operator here first.
		n["jobs_running"] = r.view.JobsRunning
		n["jobs_queued"] = r.view.JobsQueued
		n["max_concurrent_jobs"] = r.view.MaxConcurrentJobs
		n["served_models"] = r.view.ServedModels
		if r.view.GpuUtilKnown {
			n["gpu_util_pct"] = r.view.GpuUtilPct
		}
		// work_util_pct is the figure PLACEMENT compares (ADR 0057): the busiest
		// card the harness can run a seat on, with a display-active card skipped.
		// gpu_util_pct beside it is the busiest card on the whole box, so on a node
		// whose operator is using it the two differ — and a caller reading only the
		// first would mis-read a free node as busy, the same misread this ends.
		// Absent on a node that predates the field.
		if r.view.WorkUtilKnown {
			n["work_util_pct"] = r.view.WorkUtilPct
		}
		// Saturation + lease (0.113.18): the node's own one-word verdicts. A
		// node that publishes neither is older; the keys are simply absent.
		if r.view.SaturationKnown {
			n["saturation"] = map[string]any{"score": r.view.SaturationScore, "high": r.view.SaturationHigh, "idle_slot": r.view.IdleSlot}
		}
		if r.view.LeasedText {
			n["text_lease_held"] = true
		}
		if len(r.view.Layers) > 0 {
			// A composite node's layers, exactly as it published them (ADR
			// 0052): the delegator places over these rows, so a caller reading
			// status sees the same table the placement decision saw.
			n["layers"] = r.view.Layers
		}
		if r.view.LeaseBusy {
			// 0.113.27: the node says its card is spoken for long enough to
			// place elsewhere, whatever the lease class. Reported beside the
			// text flag so "idle_agent_nodes 0" is never unexplained.
			n["gpu_lease_busy"] = true
		}
		if r.view.LeaseOverdue {
			// GPU routing P1: the lease is held past its declared window. The node
			// is ranked last for placement, not excluded; the flag says why a
			// placement preferred another node.
			n["gpu_lease_overdue"] = true
		}
		// GPU routing P1: per-node free cards, from the node's own gpu_devices[].
		// A free card is idle (utilisation under the working line), not the display
		// and has room for a seat; placement prefers a node with one over a node
		// whose busiest card reads lower. Absent when the node publishes no per-card
		// utilisation, which is UNKNOWN, never zero.
		if free, total, known := r.view.FreeCards(); known {
			n["free_cards"] = free
			n["cards_total"] = total
		}
		// W-31 (item 9): the PAIR-adopted in-flight signal — a job registry
		// count, never utilization or a lease alone — plus the one-word
		// verdict every surface (gpu status, fleet-ui/top, here) now shares.
		inFlight, verdict := nodeVerdict(r.view)
		n["in_flight"] = inFlight
		n["verdict"] = verdict
		if r.view.VisionCallsRunning > 0 {
			// The part of in_flight that is no job of the node: say it, so a busy node with
			// jobs_running 0 is not unexplained.
			n["vision_calls_running"] = r.view.VisionCallsRunning
		}
		// Register A-103: did the node's boot-time GPU power profile take. One short token per node ("ok", "skipped"
		// or "unknown"), so the brief form carries it too; the reason follows only when it is news (a node that
		// applies no profile reads a bare "unknown"). Absent on a node too old to publish it, which is unknown.
		if state, reason, ok := powerProfileTokens(r.view.PowerProfile); ok {
			n["power_profile"] = state
			if reason != "" {
				n["power_profile_reason"] = reason
			}
		}
		if r.view.AgentEnabled {
			capable++
			if r.view.QueueDepth == 0 {
				idle++
			}
			fleetSlots += max(r.view.MaxConcurrentJobs, 1)
		}
		nodes = append(nodes, n)
	}
	out["nodes"] = nodes
	// The cluster's whole agent concurrency, from the live roster: the size a fan-out is cut to. On 2026-10-19 a
	// workflow sized its fan-out to the task (30 agents, 618 research rows) instead of to the ~8 slots that exist.
	localSlots := 0
	if strings.TrimSpace(cfg.AgentPlannerModel("")) != "" {
		localSlots = cfg.FleetConcurrencyLimit()
	}
	out["agent_slots"] = map[string]any{
		"total": localSlots + fleetSlots, "local": localSlots, "fleet": fleetSlots,
		"note": "the most agent jobs the cluster runs at once (local run cap + every reachable agent node's max_concurrent_jobs); keep a fan-out's concurrent harness calls x subtasks per call at or under total, and do the overflow or a defer yourself instead of queueing past it",
	}
	// Published, not merely computed: an idle seat is capacity already paid for
	// in electricity, and the whole point of this surface is that a caller can
	// see it without reading anything.
	out["agent_capable_nodes"] = capable
	out["idle_agent_nodes"] = idle
	return out
}

// powerProfileTokens is a node's published power_profile as the fleet block shows it: the state ("ok", "skipped" or
// "unknown"; anything else a node sends reads "unknown") and, when it is news, the reason token. The reason is left out
// for a node that is ok and for "no_state" (a node that applies no profile), and is dropped unless it is a lower-case
// token of letters, digits and underscores, so a node cannot put free text into the answer. ok is false for a node
// that publishes no power_profile (one release behind), which the block leaves absent.
func powerProfileTokens(pp *delegate.PowerProfileView) (state, reason string, ok bool) {
	if pp == nil || pp.State == "" {
		return "", "", false
	}
	switch pp.State {
	case "ok", "skipped", "unknown":
		state = pp.State
	default:
		state = "unknown"
	}
	if state == "ok" || pp.Reason == "" || pp.Reason == "no_state" || len(pp.Reason) > 32 {
		return state, "", true
	}
	for _, c := range pp.Reason {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return state, "", true
		}
	}
	return state, pp.Reason, true
}

// pinsView is the fleet block's pin accounting (ADR 0078): this server's counts of the subtasks callers pinned to
// route local or remote, by pin_reason, of the ones whose route gave no reason and was placed as a hint, and of the
// hints placement overrode. Nothing here scans the ledger.
func (s *Server) pinsView() map[string]any {
	st := s.pins.Snapshot()
	view := map[string]any{
		"window":           "since this server started",
		"unit":             "subtasks",
		"reasoned":         st.Reasoned,
		"hints":            st.Hints,
		"hints_overridden": st.HintsOverridden,
		"unreasoned":       st.Unreasoned,
		"note": "reasoned: subtasks of calls pinned to route local or remote under each pin_reason, which placement obeyed; " +
			"hints: subtasks whose local or remote route gave no pin_reason and was placed as a hint; hints_overridden: how many of those " +
			"placement did not honour (a local hint placed on a fleet node, a remote hint placed on the local seat); " +
			"unreasoned: subtasks pinned through a caller with no pin_reason channel that reach the engine (the review lane's remote fallthrough " +
			"and the remote route of offload_ask and agent_run; their explicit local route runs on this box's seat without reaching the engine and is not counted), " +
			"which stay authoritative. These counts are this server's and restart at zero with it; " +
			"each ledger row carries pin_reason and route_asked for any longer window",
	}
	if !st.Since.IsZero() {
		view["started_at"] = st.Since.UTC().Format(time.RFC3339)
	}
	return view
}

// nodeVerdict is W-31's in-flight signal + one-word verdict for a REMOTE
// node (item 9, offload_status only — placement's own busy/eligibility
// reading lives in internal/delegate and never calls this).
//
// in_flight adopts the PAIR semantics the operator asked for (14:0x, "nvidia
// pair has a feature that lets us know if a gpu has a job in flight or not
// regardless of GPU usage"): a JOB REGISTRY count, never GPU utilization and
// never a lease alone. jobs_running counts a job the moment a worker claims
// it, including the whole admission phase (cordon, swap pre-flight, cold
// load, coherence probe) where the card may still be idle; jobs_admitting is
// exactly that subset, so subtracting it is what turns "a worker is nominally
// busy" into "a card is actually generating". vision_calls_running is added: a
// vision call another process on the node has on a seat (a CLI loop, an MCP
// server) is in that registry too, and is no job of the node's store.
//
// verdict vocabulary: busy (in_flight > 0) | held-idle (a lease is held —
// exclusive, draining, or a plain reservation the node itself calls busy —
// and nothing is in flight) | loaded-idle (SeatLoaded known true, idle) |
// cold (SeatLoaded known false, idle) | unknown (SeatLoaded never
// published, no lease, idle).
func nodeVerdict(v delegate.NodeView) (inFlight int, verdict string) {
	inFlight = v.JobsRunning - v.JobsAdmitting
	if inFlight < 0 {
		inFlight = 0
	}
	inFlight += v.VisionCallsRunning
	switch {
	case inFlight > 0:
		return inFlight, "busy"
	case v.LeaseExclusive || v.LeaseDraining || v.LeaseBusy || v.LeaseOverdue:
		// An overdue lease (GPU routing P1) is a held lease: the holder is alive
		// and the cards are spoken for, whatever its declared end says.
		return inFlight, "held-idle"
	case v.SeatLoaded != nil && *v.SeatLoaded:
		return inFlight, "loaded-idle"
	case v.SeatLoaded != nil && !*v.SeatLoaded:
		return inFlight, "cold"
	default:
		return inFlight, "unknown"
	}
}

// localSeatVerdict is nodeVerdict's local-seat twin, built from
// probeLocalBusy's own primitive (seatload.Reading) instead of a NodeView —
// the local box has no /fleet/health to decode, but seatload.Inflight reads
// the identical facts (llama-swap's /running state, then the seat's own
// gauge) that the remote path's SeatLoaded/SeatStarting/JobsRunning already
// describe. probeErr != nil (the read itself failed) is "unknown", never
// "cold" — a failed probe must not assert idleness.
func localSeatVerdict(rd seatload.Reading, probeErr error) (inFlight int, verdict string) {
	switch {
	case probeErr != nil, rd.Ambiguous:
		return 0, "unknown"
	case rd.Starting:
		// A load in progress: work is in flight for exactly the reason
		// probeLocalBusy(W-01) reads it as busy — the engine holds the
		// triggering request until the load completes.
		return 0, "busy"
	case !rd.Loaded:
		return 0, "cold"
	case rd.Inflight > 0:
		return rd.Inflight, "busy"
	default:
		return 0, "loaded-idle"
	}
}

// localSeatProbeTimeout bounds the local seat probe. It is a NON-loading read
// (/running, then /props only when the seat already holds VRAM), so it is fast
// or it is broken — there is no legitimate slow case to wait for.
const localSeatProbeTimeout = 4 * time.Second

// localSeatView reports the local agent seat WITHOUT loading it.
//
// The local seat runs agent_run and is ALWAYS subtask 0 of an agent_delegate
// spread, so its window is the ceiling for the contract a caller is about to
// write — and it is the one number no config file holds (it lives in the serving
// stack's launch flags, which is exactly why the written guidance about it
// drifted to a quarter of reality).
//
// It deliberately does NOT use agent.ProbeServedWindow: that helper documents
// itself as "may cold-start the model, which is acceptable: the caller is about
// to use exactly that model", and names the non-loading path "the right default
// for an operator probe, and exactly wrong here". offload_status IS an operator
// probe. Using it here made a capability *question* cost a multi-GB load that
// evicts whatever is resident — including a media render mid-flight.
//
// So a cold seat reports cold. That is the honest answer, and the caller loses
// nothing: agent_run and agent_delegate probe the real ceiling when they warm
// the seat, which is the moment the number is actually needed.
func localSeatView(ctx context.Context, cfg config.Config) map[string]any {
	seat := cfg.AgentPlannerModel("")
	v := map[string]any{"model": seat}

	// W-31 (item 9): in_flight + verdict, from the SAME primitive route=auto's
	// probeLocalBusy reads (seatload.Inflight) — a job registry count, never a
	// load average — attached regardless of which branch below the
	// ContextWindow probe takes, so a cold seat still reports "cold" rather
	// than silently omitting the two keys.
	if seat != "" && cfg.Endpoint != "" {
		lctx, lcancel := context.WithTimeout(ctx, localSeatProbeTimeout)
		rd, rerr := seatload.Inflight(lctx, &http.Client{Timeout: localSeatProbeTimeout}, cfg.Endpoint, seat)
		lcancel()
		inFlight, verdict := localSeatVerdict(rd, rerr)
		v["in_flight"] = inFlight
		v["verdict"] = verdict
		// Review round 1, LOW item 5: mirroring ctx_probe_error below — a
		// verdict of "unknown" says the read failed, but not WHY, and an
		// operator reading offload_status has no other way to see the
		// seatload.Inflight error (it never reaches a log at this call site).
		if rerr != nil {
			v["inflight_probe_error"] = rerr.Error()
		}
	}

	c, err := swapclient.New(cfg.Endpoint, localSeatProbeTimeout)
	if err != nil {
		v["ctx_probe_error"] = err.Error()
		return v
	}
	pctx, cancel := context.WithTimeout(ctx, localSeatProbeTimeout)
	defer cancel()

	// ContextWindow keeps Props' loaded-only contract and adds the one fallback
	// a vLLM seat needs: no /props (404) → its /v1/models max_model_len. Before
	// 0.113.14 the agent-pool seat reported ctx_probe_error "HTTP 404" here
	// every time it was warm.
	//
	// Idle-timer audit (2026-09-22): on a LOADED seat this is one read through
	// llama-swap's /upstream/<seat>/props (then /v1/models), and llama-swap
	// counts an /upstream request as activity, so it restarts the seat's idle
	// countdown once. That is acceptable here and only here because the call
	// is operator-initiated and one-shot (an offload_status request), never a
	// poll: nothing in the harness calls it on a timer. The in-flight read
	// above (seatload.Inflight) reads the seat's own address and touches no
	// /upstream path. A caller that wants to poll this view must not.
	n, err := c.ContextWindow(pctx, seat)
	switch {
	case errors.Is(err, llamaswap.ErrNotLoaded):
		v["loaded"] = false
		v["note"] = "seat is cold; its window is not readable without loading it, and a status call must never trigger a multi-GB load. agent_run/agent_delegate probe the real ceiling when they warm it."
		return v
	case errors.Is(err, llamaswap.ErrWindowUnknown):
		// Residency WAS established (that is what the sentinel means); only
		// the window is unreadable — say exactly that much.
		v["loaded"] = true
		v["ctx_probe_error"] = err.Error()
		return v
	case err != nil:
		// Resolve/running failed: residency is unknown, so `loaded` is left
		// absent rather than asserted either way.
		v["ctx_probe_error"] = err.Error()
		return v
	}
	v["loaded"] = true
	v["ctx_tokens"] = n
	return v
}

// nCtxFromProps pulls the live window out of a llama.cpp /props payload. n_ctx
// lives under default_generation_settings on current builds and at the root on
// older ones; both are accepted so a server upgrade cannot silently turn the
// ceiling into "unknown".
func nCtxFromProps(props map[string]any) (int, bool) {
	if dgs, ok := props["default_generation_settings"].(map[string]any); ok {
		if f, ok := dgs["n_ctx"].(float64); ok && f > 0 {
			return int(f), true
		}
	}
	if f, ok := props["n_ctx"].(float64); ok && f > 0 {
		return int(f), true
	}
	return 0, false
}

// probeServedModels reads the live roster and returns the CANONICAL model ids.
// Short timeout: status must stay snappy even when the endpoint is a black hole.
//
// Ids only, deliberately — `served_now` has always been the canonical list and
// callers read it as such. The alias-aware view is what the roster gate above
// uses; publishing aliases here would change this tool's payload shape.
func probeServedModels(ctx context.Context, endpoint string) ([]string, error) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	roster, err := swapclient.FetchRoster(cctx, endpoint, 3*time.Second)
	if err != nil {
		return nil, err
	}
	return roster.IDs(), nil
}

// statusSamplesGPU lets a test skip the nvidia-smi sample in offload_status.
var statusSamplesGPU = true

// statusDriverEvents is the reader of the driver's recent fault events behind gpu_lease.activity.driver_events
// (C-83-B): production is gpuactivity.ReadDriverEvents (cached 60 s). It is wired only while the GPU sample is on, and
// statusFixture sets it nil, so a test that turns the sample on with a fake sampler never starts PowerShell. A test
// that wants events sets its own reader.
var statusDriverEvents = gpuactivity.ReadDriverEvents

// statusDriverEventsReader is the reader handed to the activity snapshot: off (nil) while the GPU sample is off, so a
// test that skips nvidia-smi never reads a log either.
func statusDriverEventsReader() func(context.Context) ([]gpuactivity.DriverEvent, error) {
	if !statusSamplesGPU {
		return nil
	}
	return statusDriverEvents
}

// statusGPUSampler lets a test FAKE the per-card numbers offload_status samples,
// instead of only turning the sample on/off. The held vs held-idle vs
// held-working verdict (internal/gpuactivity.Assess) branches on the actual
// utilization percentage, so a test asserting held-idle needs idle cards, not
// merely "no sample" (H-49: on a box where another job holds the GPUs at 100%,
// the real nvidia-smi answer is legitimately busy, and the held-idle assertion
// failed on live hardware, not on a bug). Nil (production default) means the
// real nvidia-smi via gpuactivity.SampleGPUs.
var statusGPUSampler func(ctx context.Context) ([]gpuactivity.GPU, error)

// statusGPUProcSampler is statusGPUSampler's twin for the processes on the cards: a test hands
// offload_status process rows of its own (a display card's desktop beside a work card's holder)
// instead of whatever nvidia-smi lists on the machine running it. Nil (production) means
// gpuactivity.SampleProcesses.
var statusGPUProcSampler func(ctx context.Context) ([]gpuactivity.GPUProcess, error)

// unavailableCacheReason names ONE specific reason a status reader can act on,
// never a menu of possibilities. Lock contention alone never lands here — the
// per-process fallback absorbs that, and since 0.121.1 a handle nobody has used
// reports mode "unopened" rather than any kind of failure.
func unavailableCacheReason(configured string, openErr error) string {
	if configured == "" {
		return "cache_path is empty: caching is opted out on this box; agent_run re-runs the model on repeated identical input"
	}
	reason := "neither the configured cache nor its per-process fallback could be opened (not lock contention: the fallback absorbs that) — disk, permissions or a bad cache_path; agent_run re-runs the model on repeated identical input"
	if openErr != nil {
		reason += ": " + openErr.Error()
	}
	return reason
}

// readOnlyCacheEntries counts the configured result cache through a READ-ONLY,
// read-through handle: it creates no file, writes nothing, and gives up in
// milliseconds when a writer holds the exclusive lock. Returns -1 and "" when
// the store is not readable from this process right now, which is a normal
// state, not a failure — some other harness process holds it.
func readOnlyCacheEntries(configured string) (int, string) {
	if configured == "" {
		return -1, ""
	}
	r := cache.NewReader(configured)
	defer r.Close()
	n, err := r.Count()
	if err != nil {
		return -1, ""
	}
	return n, r.Path()
}
