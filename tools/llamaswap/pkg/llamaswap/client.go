// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package llamaswap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"llamaswap-pp-cli/internal/mirror"
)

// DefaultBaseURL is the address llama-swap listens on for a standard local
// install. Always the IP literal — see the loopback discipline note in the
// package doc.
const DefaultBaseURL = mirror.DefaultBaseURL

// DefaultTimeout is the per-request deadline applied when [Options.Timeout] is
// zero. Model loads legitimately exceed it; pass a longer timeout (or a context
// deadline) for calls that may trigger one.
const DefaultTimeout = 30 * time.Second

// Options configures a [Client]. The zero value is the supported default for
// every field.
type Options struct {
	// Timeout is the per-request deadline. Zero means [DefaultTimeout].
	// A context deadline still applies on top of it.
	Timeout time.Duration

	// HTTPClient overrides the transport. Its Timeout field is honored as-is
	// when set, so a caller who supplies a client owns the deadline policy.
	// Nil means an internally-built client using Timeout.
	HTTPClient *http.Client

	// YAMLPath points at the llama-swap config the keep-set is read from.
	// Empty falls back to the LLAMASWAP_YAML environment variable and then to
	// the documented install path. The file is parsed READ-ONLY; nothing in
	// this package ever writes it.
	YAMLPath string

	// ConfigPath points at the CLI's own JSON config, whose optional
	// "keep_set" array unions with the YAML-derived set. Empty falls back to
	// the LLAMASWAP_CONFIG environment variable.
	ConfigPath string

	// ExtraKeepSet adds names to the keep-set for this client's lifetime.
	// Useful for a caller that protects something the config does not.
	ExtraKeepSet []string
}

// Client talks to one llama-swap proxy. It is safe for concurrent use by
// multiple goroutines; it holds no per-call state.
type Client struct {
	mc      *mirror.Client
	keepSet *mirror.KeepSet
	base    string
}

// New returns a client bound to baseURL. An empty baseURL means
// [DefaultBaseURL].
//
// A base URL whose host resolves entirely to loopback addresses is rewritten to
// the 127.0.0.1 literal (the ::1 stall described in the package doc). A host
// that resolves anywhere else — a Tailscale MagicDNS name for a remote rig, a
// LAN address — is left exactly as given.
//
// The keep-set is loaded once, here, from configuration. It is never re-read
// from the server, and never derived from the server's ttl field.
func New(baseURL string, opts *Options) (*Client, error) {
	if opts == nil {
		opts = &Options{}
	}
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = DefaultBaseURL
	}
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("llamaswap: invalid base URL %q: %w", baseURL, err)
	}
	base = normalizeLoopback(base)

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	mc := mirror.NewClient(base, timeout)
	if opts.HTTPClient != nil {
		mc.HTTP = opts.HTTPClient
	}
	return &Client{
		mc:   mc,
		base: mc.BaseURL,
		keepSet: mirror.LoadKeepSet(mirror.KeepSetOptions{
			YAMLPath:   opts.YAMLPath,
			ConfigPath: opts.ConfigPath,
			Extra:      opts.ExtraKeepSet,
		}),
	}, nil
}

// BaseURL returns the normalized address this client dials.
func (c *Client) BaseURL() string { return c.base }

// normalizeLoopback rewrites a host that resolves ONLY to loopback addresses
// into the 127.0.0.1 literal. Resolution-based rather than name-based, so an
// alias mapped to loopback in the hosts file is normalized too, and a remote
// hostname is never touched.
func normalizeLoopback(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return strings.TrimRight(raw, "/")
	}
	host := u.Hostname()
	if host == "" || net.ParseIP(host) != nil {
		return strings.TrimRight(raw, "/")
	}
	ips, lookupErr := net.LookupIP(host)
	if lookupErr != nil || len(ips) == 0 {
		return strings.TrimRight(raw, "/")
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return strings.TrimRight(raw, "/")
		}
	}
	if p := u.Port(); p != "" {
		u.Host = net.JoinHostPort("127.0.0.1", p)
	} else {
		u.Host = "127.0.0.1"
	}
	return strings.TrimRight(u.String(), "/")
}

// ---------------------------------------------------------------- roster

// Model is one roster entry from GET /v1/models.
type Model struct {
	// ID is the canonical model id llama-swap keys the seat by.
	ID string `json:"id"`
	// Name is the human label from the seat's config, when set.
	Name string `json:"name,omitempty"`
	// Aliases are the additional names the model answers to, from
	// meta.llamaswap.aliases.
	Aliases []string `json:"aliases,omitempty"`
	// Status is the server's own word for the seat: "loaded", "unloaded",
	// "starting", and so on. Empty when the build does not report it.
	Status string `json:"status,omitempty"`
}

// Models returns the full roster, aliases included.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	entries, err := c.mc.Models(ctx)
	if err != nil {
		return nil, wrapTransport(err, "GET /v1/models")
	}
	out := make([]Model, 0, len(entries))
	for _, e := range entries {
		out = append(out, Model{ID: e.ID, Name: e.Name, Aliases: e.Aliases, Status: e.State})
	}
	return out, nil
}

// RunningModel is one entry of GET /running: a model holding VRAM right now.
type RunningModel struct {
	// ID is the canonical model id.
	ID string `json:"id"`
	// State is the lifecycle state llama-swap reports ("ready", "starting").
	State string `json:"state"`
	// Cmd is the process argv as llama-swap actually spawned it, after macro
	// expansion and port assignment. This is ground truth for what flags the
	// seat is really running, which the config file alone cannot tell you.
	Cmd string `json:"cmd"`
	// Proxy is the upstream address llama-swap forwards to.
	Proxy string `json:"proxy"`
	// TTL is the server's reported ttl. Treat it as UNRELIABLE for keep-set
	// decisions: a seat configured ttl:-1 is reported here as 0. Use
	// [Client.KeepSet] instead.
	TTL int `json:"ttl"`
}

// Running lists the models currently holding VRAM.
func (c *Client) Running(ctx context.Context) ([]RunningModel, error) {
	entries, err := c.mc.Running(ctx)
	if err != nil {
		return nil, wrapTransport(err, "GET /running")
	}
	out := make([]RunningModel, 0, len(entries))
	for _, e := range entries {
		out = append(out, RunningModel{ID: e.Model, State: e.State, Cmd: e.Cmd, Proxy: e.Proxy, TTL: e.TTL})
	}
	return out, nil
}

// Resolve maps a canonical id or any alias to the canonical id, returning the
// full alias list alongside it. This is the whole point of the package: every
// consumer that skipped it eventually addressed a model by an alias and got a
// 404 it blamed on the server.
//
// Matching is case-insensitive. An unknown name returns [ErrModelNotFound] with
// the roster listed in the message.
func (c *Client) Resolve(ctx context.Context, nameOrAlias string) (id string, aliases []string, err error) {
	models, err := c.Models(ctx)
	if err != nil {
		return "", nil, err
	}
	m, ok := resolveIn(models, nameOrAlias)
	if !ok {
		ids := make([]string, 0, len(models))
		for _, e := range models {
			ids = append(ids, e.ID)
		}
		return "", nil, fmt.Errorf("%w: %q (roster: %s)", ErrModelNotFound, nameOrAlias, strings.Join(ids, ", "))
	}
	return m.ID, m.Aliases, nil
}

// resolveIn matches a name against ids first, then aliases. Ids win so a model
// whose id is another model's alias cannot be shadowed.
func resolveIn(models []Model, name string) (Model, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Model{}, false
	}
	for _, m := range models {
		if strings.EqualFold(m.ID, name) {
			return m, true
		}
	}
	for _, m := range models {
		for _, a := range m.Aliases {
			if strings.EqualFold(a, name) {
				return m, true
			}
		}
	}
	return Model{}, false
}

// ---------------------------------------------------------------- keep-set

// KeepSetMember is one protected model with every name it answers to.
type KeepSetMember struct {
	// ID is the name the keep-set entry was recorded under.
	ID string `json:"id"`
	// Aliases are the other names that resolve to the same protection.
	Aliases []string `json:"aliases,omitempty"`
	// Origin says where the entry came from: a YAML ttl value, the CLI
	// config, an environment variable, or a caller-supplied extra.
	Origin string `json:"origin"`
}

// KeepSetInfo is the resolved protected set plus the provenance needed to
// answer "why is this protected?" and "why is this NOT protected?".
type KeepSetInfo struct {
	// Members are the protected models.
	Members []KeepSetMember `json:"members"`
	// Sources lists the files and environment variables that contributed.
	Sources []string `json:"sources,omitempty"`
	// Warnings records sources that could not be read. A keep-set built from
	// an unreadable config is reported as such rather than silently empty:
	// "unknown, and here is why" is the only honest answer.
	Warnings []string `json:"warnings,omitempty"`
}

// KeepSet returns the protected set, resolved once at [New] from configuration.
//
// It is deliberately NOT read from the server. GET /running reports ttl:0 for a
// seat configured ttl:-1 on current builds, so a server-derived keep-set would
// be built on a value the server misreports — and the failure mode is a
// memory-stack outage.
func (c *Client) KeepSet() KeepSetInfo {
	info := KeepSetInfo{Sources: c.keepSet.Sources, Warnings: c.keepSet.Warnings}
	for _, m := range c.keepSet.Members {
		info.Members = append(info.Members, KeepSetMember{ID: m.ID, Aliases: m.Aliases, Origin: m.Origin})
	}
	return info
}

// IsProtected reports whether a name — canonical id or alias — is in the
// keep-set, and returns the matching member.
func (c *Client) IsProtected(name string) (KeepSetMember, bool) {
	m, ok := c.keepSet.Match(name)
	if !ok {
		return KeepSetMember{}, false
	}
	return KeepSetMember{ID: m.ID, Aliases: m.Aliases, Origin: m.Origin}, true
}

// ---------------------------------------------------------------- unload

// UnloadOpts controls one unload.
type UnloadOpts struct {
	// Drain waits for the model to go idle before unloading. The check fails
	// CLOSED: unreadable slot state unloads nothing and returns
	// [ErrDrainUnobservable]; a still-busy model at the deadline returns
	// [ErrDrainTimeout].
	Drain bool

	// DrainTimeout bounds the drain wait. Zero means 30 seconds.
	DrainTimeout time.Duration

	// KeepsetOverride unloads a protected member anyway. Loud by design: the
	// result records that the override was used. Reach for it only when
	// taking the seat down is the actual intent.
	KeepsetOverride bool
}

// UnloadResult describes what happened, including what could not be observed.
type UnloadResult struct {
	// Model is the canonical id that was targeted.
	Model string `json:"model"`
	// Requested is the name the caller passed, before alias resolution.
	Requested string `json:"requested"`
	// Status is the HTTP status of the unload call, when one was made.
	Status int `json:"status,omitempty"`
	// WasRunning records whether the model held VRAM before the call. False
	// means the unload was a no-op.
	WasRunning bool `json:"was_running"`
	// Drained is nil when no drain check ran, true when idleness was
	// confirmed. It is never true on a guess.
	Drained *bool `json:"drained"`
	// DrainMethod names how idleness was established: "slots" (authoritative)
	// or "activity-fallback" (weaker evidence, used when /slots is absent).
	DrainMethod string `json:"drain_method,omitempty"`
	// KeepsetOverridden records that a protected member was unloaded anyway.
	KeepsetOverridden bool `json:"keepset_overridden"`
	// Notes carry anything the caller should know that the fields cannot say.
	Notes []string `json:"notes,omitempty"`
}

// Unload unloads one model by id or alias.
//
// Two guards stand in front of the call, in this order:
//
//  1. Keep-set refusal (default on). A protected member — matched by alias as
//     well as by id — returns [ErrKeepsetRefusal] and nothing is sent to the
//     server. Set [UnloadOpts.KeepsetOverride] to proceed anyway.
//
//  2. Drain (opt-in). With [UnloadOpts.Drain], slot state is polled until the
//     model is idle. The check fails closed; see [UnloadOpts.Drain].
//
// A model absent from /running is never probed for drain state: any /upstream
// request auto-starts a stopped model, so probing to decide whether to unload
// would load it first.
func (c *Client) Unload(ctx context.Context, nameOrAlias string, opts *UnloadOpts) (*UnloadResult, error) {
	if opts == nil {
		opts = &UnloadOpts{}
	}
	res := &UnloadResult{Requested: nameOrAlias, KeepsetOverridden: opts.KeepsetOverride}

	// Protection first, and it does not depend on the roster being readable:
	// a keep-set name that resolves nowhere still refuses.
	if m, protected := c.keepSet.Match(nameOrAlias); protected && !opts.KeepsetOverride {
		res.Model = m.ID
		return res, fmt.Errorf("%w: %q (%s)", ErrKeepsetRefusal, m.ID, m.Origin)
	}

	models, err := c.Models(ctx)
	if err != nil {
		return res, err
	}
	entry, ok := resolveIn(models, nameOrAlias)
	if !ok {
		ids := make([]string, 0, len(models))
		for _, e := range models {
			ids = append(ids, e.ID)
		}
		return res, fmt.Errorf("%w: %q (roster: %s)", ErrModelNotFound, nameOrAlias, strings.Join(ids, ", "))
	}
	res.Model = entry.ID

	if !opts.KeepsetOverride {
		names := append([]string{entry.ID}, entry.Aliases...)
		if m, protected := c.keepSet.MatchAny(names...); protected {
			return res, fmt.Errorf("%w: %q (%s), matched via %q", ErrKeepsetRefusal, m.ID, m.Origin, nameOrAlias)
		}
	}

	running, err := c.Running(ctx)
	if err != nil {
		return res, err
	}
	for _, r := range running {
		if r.ID == entry.ID {
			res.WasRunning = true
			break
		}
	}

	if opts.Drain {
		method, notes, derr := c.drain(ctx, entry.ID, res.WasRunning, opts.DrainTimeout)
		res.DrainMethod = method
		res.Notes = append(res.Notes, notes...)
		if derr != nil {
			no := false
			res.Drained = &no
			return res, derr
		}
		yes := true
		res.Drained = &yes
	}

	status, err := c.mc.UnloadModel(ctx, entry.ID)
	res.Status = status
	switch {
	case err != nil:
		return res, wrapTransport(err, "POST /api/models/unload/"+entry.ID)
	case status == http.StatusNotFound:
		// Version drift: this build has no per-model route. The bulk routes are
		// not selective, so this is reported rather than escalated into an
		// unload-everything.
		return res, fmt.Errorf("%w: per-model unload route absent on this llama-swap build", ErrModelNotFound)
	case status >= 500:
		return res, &HTTPError{Status: status, Method: http.MethodPost, Path: "/api/models/unload/" + entry.ID}
	case status < 200 || status >= 300:
		return res, fmt.Errorf("unload %s: HTTP %d", entry.ID, status)
	}
	if !res.WasRunning {
		res.Notes = append(res.Notes, "model was not in /running before the call; the unload is a no-op")
	}
	return res, nil
}

// drain polls until the model is idle. Returns the method used and any notes;
// a non-nil error means the caller must NOT unload.
func (c *Client) drain(ctx context.Context, model string, isRunning bool, timeout time.Duration) (string, []string, error) {
	if !isRunning {
		return "none", []string{"not in /running: no drain probe sent (an /upstream probe would auto-start the model)"}, nil
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	var notes []string
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return "slots", notes, fmt.Errorf("%w: %s still processing after %s", ErrDrainTimeout, model, timeout)
		}
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		slots, status, err := c.mc.Slots(callCtx, model)
		cancel()
		if ctx.Err() != nil {
			return "slots", notes, ctx.Err()
		}
		switch {
		case err == nil:
			busy := false
			for _, s := range slots {
				if s.IsProcessing {
					busy = true
					break
				}
			}
			if !busy {
				return "slots", notes, nil
			}
		case status == http.StatusNotFound:
			// Endpoint absent (llama-server started without --slots), which is a
			// different fact from unobservable. Documented fallback: an in-flight
			// request appears in the activity ring without a terminal status.
			notes = appendOnce(notes, "/upstream/"+model+"/slots returned 404 (started without --slots); using the activity-ring fallback")
			page, ferr := c.mc.Activity(ctx, mirror.ActivityOpts{Model: model, Limit: 25, Sort: "id", Order: "desc"})
			if ferr != nil {
				return "activity-fallback", notes, fmt.Errorf("%w: %s (/slots absent, activity fallback failed: %v)", ErrDrainUnobservable, model, ferr)
			}
			busy := false
			for _, row := range page.Data {
				if !row.Terminal() {
					busy = true
					break
				}
			}
			if !busy {
				return "activity-fallback", notes, nil
			}
		case status >= 500:
			return "slots", notes, fmt.Errorf("%w: %s (/slots answered HTTP %d)", ErrDrainUnobservable, model, status)
		default:
			return "slots", notes, fmt.Errorf("%w: %s (/slots did not answer: %v)", ErrDrainUnobservable, model, err)
		}
		select {
		case <-ctx.Done():
			return "slots", notes, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func appendOnce(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

// ---------------------------------------------------------------- passthrough

// Slot is one llama-server slot. IsProcessing is the drain signal.
type Slot struct {
	// ID is the slot index.
	ID int `json:"id"`
	// NCtx is the slot's context window in tokens.
	NCtx int `json:"n_ctx"`
	// IsProcessing reports whether the slot is mid-request.
	IsProcessing bool `json:"is_processing"`
}

// Slots reads a loaded model's slot state through the passthrough.
//
// The model must already be in /running: this method checks first and returns
// [ErrNotLoaded] rather than triggering a multi-GB auto-start. A 404 surfaces
// as an [HTTPError] with Status 404, meaning llama-server was started without
// --slots.
func (c *Client) Slots(ctx context.Context, nameOrAlias string) ([]Slot, error) {
	id, err := c.requireLoaded(ctx, nameOrAlias)
	if err != nil {
		return nil, err
	}
	slots, status, err := c.mc.Slots(ctx, id)
	if err != nil {
		return nil, c.upstreamErr(err, status, http.MethodGet, "/upstream/"+id+"/slots")
	}
	out := make([]Slot, 0, len(slots))
	for _, s := range slots {
		out = append(out, Slot{ID: s.ID, NCtx: s.NCtx, IsProcessing: s.IsProcessing})
	}
	return out, nil
}

// Props reads a loaded model's llama-server /props: the effective runtime
// configuration, including n_ctx and build_info, which the config file cannot
// tell you.
//
// Same loaded-only contract as [Client.Slots]: an unloaded model returns
// [ErrNotLoaded] instead of being started.
func (c *Client) Props(ctx context.Context, nameOrAlias string) (map[string]any, error) {
	id, err := c.requireLoaded(ctx, nameOrAlias)
	if err != nil {
		return nil, err
	}
	props, status, err := c.mc.Props(ctx, id)
	if err != nil {
		return nil, c.upstreamErr(err, status, http.MethodGet, "/upstream/"+id+"/props")
	}
	return props, nil
}

// requireLoaded resolves a name and asserts the model holds VRAM.
func (c *Client) requireLoaded(ctx context.Context, nameOrAlias string) (string, error) {
	id, _, err := c.Resolve(ctx, nameOrAlias)
	if err != nil {
		return "", err
	}
	running, err := c.Running(ctx)
	if err != nil {
		return "", err
	}
	for _, r := range running {
		if r.ID == id {
			return id, nil
		}
	}
	return "", fmt.Errorf("%w: %s (an /upstream probe would auto-start it; load it deliberately first)", ErrNotLoaded, id)
}

// TokenizeResult is the outcome of a tokenize call.
type TokenizeResult struct {
	// Tokens are the token ids.
	Tokens []int `json:"tokens"`
	// Count is len(Tokens), the number this call exists to establish. Never
	// estimate it from character count: chars/4 is off by ~2x on Gemma
	// tokenizers, which has produced retracted context-budget verdicts.
	Count int `json:"count"`
}

// Tokenize counts a string's real tokens against a LOADED model's own
// tokenizer. Same loaded-only contract as [Client.Props].
func (c *Client) Tokenize(ctx context.Context, nameOrAlias, text string) (*TokenizeResult, error) {
	id, err := c.requireLoaded(ctx, nameOrAlias)
	if err != nil {
		return nil, err
	}
	var out struct {
		Tokens []int `json:"tokens"`
	}
	if err := c.postJSON(ctx, "/upstream/"+url.PathEscape(id)+"/tokenize",
		map[string]any{"content": text}, &out); err != nil {
		return nil, err
	}
	return &TokenizeResult{Tokens: out.Tokens, Count: len(out.Tokens)}, nil
}

// EmbeddingsResult is the outcome of an embeddings call.
type EmbeddingsResult struct {
	// Model is the canonical id that served the request.
	Model string `json:"model"`
	// Vectors holds one embedding per input, in input order.
	Vectors [][]float64 `json:"vectors"`
	// Dims is the dimensionality of the first vector, or 0 when none.
	Dims int `json:"dims"`
}

// Embeddings embeds one or more inputs through the PRODUCTION route
// (POST /v1/embeddings), which is what real callers use — measuring a
// different route measures a different thing.
//
// The model is loaded on demand by llama-swap, so this call can legitimately
// take as long as a model load; give it a context deadline that allows for one.
func (c *Client) Embeddings(ctx context.Context, nameOrAlias string, inputs []string) (*EmbeddingsResult, error) {
	id, _, err := c.Resolve(ctx, nameOrAlias)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := c.postJSON(ctx, "/v1/embeddings", map[string]any{"model": id, "input": inputs}, &out); err != nil {
		return nil, err
	}
	res := &EmbeddingsResult{Model: id}
	for _, d := range out.Data {
		res.Vectors = append(res.Vectors, d.Embedding)
	}
	if len(res.Vectors) > 0 {
		res.Dims = len(res.Vectors[0])
	}
	return res, nil
}

// RerankHit is one scored document.
type RerankHit struct {
	// Index is the position of the document in the input slice.
	Index int `json:"index"`
	// Score is the reranker's relevance score. Absolute values are only
	// comparable within one model at one configuration.
	Score float64 `json:"score"`
}

// Rerank scores documents against a query through POST /v1/rerank.
//
// Older llama-swap builds expose the bare /rerank path instead; this method
// falls back to it on a 404 so a version difference is not a failure.
func (c *Client) Rerank(ctx context.Context, nameOrAlias, query string, documents []string) ([]RerankHit, error) {
	id, _, err := c.Resolve(ctx, nameOrAlias)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"model": id, "query": query, "documents": documents}
	var out struct {
		Results []struct {
			Index int     `json:"index"`
			Score float64 `json:"relevance_score"`
		} `json:"results"`
	}
	err = c.postJSON(ctx, "/v1/rerank", body, &out)
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusNotFound {
		err = c.postJSON(ctx, "/rerank", body, &out)
	}
	if err != nil {
		return nil, err
	}
	hits := make([]RerankHit, 0, len(out.Results))
	for _, r := range out.Results {
		hits = append(hits, RerankHit{Index: r.Index, Score: r.Score})
	}
	return hits, nil
}

// ---------------------------------------------------------------- transport

// postJSON issues a POST and decodes the response, mapping failures onto this
// package's typed errors.
func (c *Client) postJSON(ctx context.Context, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.mc.HTTP.Do(req)
	if err != nil {
		return wrapTransport(err, "POST "+path)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{Status: resp.StatusCode, Method: http.MethodPost, Path: path, Body: truncate(string(data), 400)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// upstreamErr classifies a passthrough failure. A 404 keeps its status so the
// caller can read it as "endpoint absent"; a 5xx maps onto [ErrUpstream5xx].
func (c *Client) upstreamErr(err error, status int, method, path string) error {
	if status > 0 {
		return &HTTPError{Status: status, Method: method, Path: path, Body: err.Error()}
	}
	return wrapTransport(err, method+" "+path)
}

// wrapTransport turns a connection-level failure into [ErrUnreachable] and
// leaves an HTTP-level failure classified by status.
func wrapTransport(err error, what string) error {
	if err == nil {
		return nil
	}
	var he *mirror.HTTPError
	if errors.As(err, &he) {
		return &HTTPError{Status: he.Status, Method: http.MethodGet, Path: he.Path, Body: he.Body}
	}
	return fmt.Errorf("%w: %s: %v", ErrUnreachable, what, err)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
