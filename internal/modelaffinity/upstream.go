// upstream.go — the fence in front of llama-swap's per-model passthrough.
//
// THE DEFECT (2026-09-22, the 3-card agent seat on a video render). llama-swap
// answers ANY request under /upstream/<model>/… by STARTING that model when it
// is not loaded (llama-swap v251 internal/server/api.go handleUpstream: the
// request is pinned to the model and handed to the local router, which swaps it
// in; only a path matching the operator's upstream.ignorePaths is refused with
// 409). The harness's generation requests were fenced — Admit waits for the
// card under a media or exclusive lease (gpuwait.go) — but four families of
// harness requests reached /upstream without passing any gate:
//
//   - the served-window probe (internal/agent window.go: /props, /v1/models),
//   - the seat-pin probe (internal/agent props.go: /props, /version, /v1/models),
//   - the real tokenizer (internal/tokclient: /tokenize),
//   - the warm-up, the whisper transcription and the KV-slot lane.
//
// An agent run admitted a moment before a render kept issuing them. A client
// with a short timeout (the seat pin gives up after 3 s) hangs up mid-load and
// llama-swap logs the start as aborted; the window probe waits out a cold start
// and completes one. The render's card took 14.3 GB of seat weights
// mid-video and the render ran 895 s against its usual 228-324 s.
//
// THE INVARIANT. While blocksLoad holds — a media lease, or a text lease stamped
// exclusive, that this process does not inherit — no harness request may make
// llama-swap load a model. AwaitUpstream is the one builder of an /upstream URL
// in the harness (TestUpstreamURLsAreBuiltOnlyBehindTheFence pins that no other
// non-test file spells the route), so a new probe cannot reach the route
// without passing the fence.
//
// WHAT PASSES A FENCE. A request for a model llama-swap already lists as READY
// cannot start anything: it is served by the resident process. So under a fence
// the gate reads /running (never /upstream, which is the thing it guards) and
// lets a ready model through. A model that is absent, starting or stopping is
// not resident; its request waits for the card, bounded by the caller's own
// deadline, and on exhaustion returns the same *LeaseError a generation
// admission returns — typed, naming the holder, carrying "timeout" so the
// ledger files it as congestion. A caller that must not wait (the post-run pin,
// the per-step tokenizer) passes a deadline of now and gets one inspection.
//
// The residency read is taken ONLY when a fence is up, so an unfenced box pays
// what it paid before: one ReadFile of a lease record that usually does not
// exist. The same check-then-act window ADR 0026 names for Admit remains (a
// lease taken microseconds after the read), and a ready model evicted by its
// own ttl between the read and the request is the same window from the other
// side; both are one read wide.
package modelaffinity

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/seatload"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// upstreamResidencyTimeout bounds the /running + roster read the fence makes
// before refusing a request. The read never loads anything, and a llama-swap
// that cannot answer it in this time cannot prove residency either, which the
// fence then treats as "not resident".
var upstreamResidencyTimeout = 3 * time.Second

// ErrNoUpstreamRoot is returned when the endpoint names no llama-swap root.
var ErrNoUpstreamRoot = errors.New("modelaffinity: endpoint has no llama-swap root for an /upstream request")

// AwaitUpstream returns the URL of llama-swap's per-model passthrough for model
// (endpoint's root + "/upstream/" + model + path) once sending a request there
// cannot load a model onto a fenced card.
//
// Unfenced (no lease, a plain text reservation, a draining hold, the caller's
// own inherited lease, or a gate that config.Load never armed) it returns at
// once. Fenced, it lets the request through when llama-swap lists the model as
// ready, and otherwise waits for the fence to lift until deadline or ctx ends,
// returning a *LeaseError when it does not.
//
// path is appended verbatim (it may carry a query); a missing leading slash is
// added. The model name is path-escaped.
func AwaitUpstream(ctx context.Context, endpoint, model, path string, deadline time.Time) (string, error) {
	u, err := upstreamURL(endpoint, model, path)
	if err != nil {
		return "", err
	}
	if err := awaitUpstream(ctx, endpoint, model, deadline); err != nil {
		return "", err
	}
	return u, nil
}

// HolderUpstreamURL builds the same URL with NO fence. It exists for exactly
// one caller: the lease HOLDER warming its own seat back at the end of
// `gpu reserve --drain --unload-seat -- <cmd>`. That process holds the lease it
// would otherwise wait on (it does not carry GPU_LEASE_EPOCH itself; its child
// did), and the warm-back is the load the lease was taken to schedule.
// TestUpstreamURLsAreBuiltOnlyBehindTheFence restricts its callers.
func HolderUpstreamURL(endpoint, model, path string) (string, error) {
	return upstreamURL(endpoint, model, path)
}

func upstreamURL(endpoint, model, path string) (string, error) {
	b := swapclient.BaseURL(endpoint)
	if b == "" {
		return "", ErrNoUpstreamRoot
	}
	if strings.TrimSpace(model) == "" {
		return "", errors.New("modelaffinity: an /upstream request needs a model name")
	}
	if path != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return b + "/upstream/" + url.PathEscape(model) + path, nil
}

// awaitUpstream is the fence itself; see the file header.
func awaitUpstream(ctx context.Context, endpoint, model string, deadline time.Time) error {
	dir := gpuLeaseDir()
	if dir == "" {
		return nil // not armed: inert by construction, exactly like awaitCard
	}
	if !blocksLoad(gpulease.InspectDir(dir)) {
		return nil
	}
	if upstreamResident(ctx, endpoint, model) {
		return nil
	}
	return awaitLease(ctx, swapclient.BaseURL(endpoint), model, deadline, blocksLoad)
}

// upstreamResident reports whether llama-swap lists model (id or alias) as
// loaded and settled — not starting, not stopping. Any failure to read is
// "not resident": the fence never assumes residency it could not see.
func upstreamResident(ctx context.Context, endpoint, model string) bool {
	rctx, cancel := context.WithTimeout(ctx, upstreamResidencyTimeout)
	defer cancel()
	client := &http.Client{Timeout: upstreamResidencyTimeout}
	rd, err := seatload.Running(rctx, client, swapclient.BaseURL(endpoint), model)
	return err == nil && rd.Loaded && !rd.Starting
}

// IsLeaseRefusal reports whether err is (or wraps) a refusal by the GPU-lease
// gate — a *LeaseError from Admit, AwaitRunSlot or AwaitUpstream. Callers that
// fail open on a probe use it to tell "the card is held" from "the endpoint is
// broken", which must never be counted against the endpoint.
func IsLeaseRefusal(err error) bool {
	var le *LeaseError
	return errors.As(err, &le)
}
