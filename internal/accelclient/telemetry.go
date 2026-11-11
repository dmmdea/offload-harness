package accelclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Device telemetry: a launcher that answers `<cmd> --telemetry` prints ONE JSON document about its
// card (temperature, loads, clocks, link, card memory, processes) and exits, without starting the
// sidecar; a running sidecar embeds the same document in GET /health under "telemetry". Today only
// the Axera card has one. Two readers use it: offload_status (the one-shot probe, while the sidecar
// is down) and /fleet/health (the cached, off-request-path collector below).

const (
	// TelemetryArg is the launcher argument that prints the telemetry document.
	TelemetryArg = "--telemetry"
	// ProbeTimeout bounds one `<cmd> --telemetry` run.
	ProbeTimeout = 6 * time.Second
	// OneShotTTL is how long a one-shot probe result (success or failure) is reused per device.
	OneShotTTL = 10 * time.Second
	// CollectorTTL is how long the fleet collector's value is fresh.
	CollectorTTL = 15 * time.Second
	// healthTimeout bounds the sidecar /health read the collector tries first.
	healthTimeout = 2 * time.Second
)

type oneShotEntry struct {
	mu  sync.Mutex
	at  time.Time
	doc map[string]any
	err error
}

var (
	oneShotMu sync.Mutex
	oneShots  = map[string]*oneShotEntry{}
)

// OneShotTelemetry runs `<cmd> --telemetry` once (ProbeTimeout) and decodes its stdout as one JSON
// object. The result, error included, is cached per device for OneShotTTL, so a status poll loop
// cannot hammer the card's driver or wait out the timeout on every call; concurrent callers share
// one run. It never starts the sidecar. An empty cmd is an error.
func OneShotTelemetry(ctx context.Context, device, cmd string) (map[string]any, error) {
	if cmd == "" {
		return nil, errors.New("no launcher configured")
	}
	oneShotMu.Lock()
	e := oneShots[device]
	if e == nil {
		e = &oneShotEntry{}
		oneShots[device] = e
	}
	oneShotMu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.at.IsZero() && time.Since(e.at) < OneShotTTL {
		return e.doc, e.err
	}
	pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	out, err := ProbeCmd(pctx, cmd, TelemetryArg)
	var doc map[string]any
	if err == nil {
		if jerr := json.Unmarshal(out, &doc); jerr != nil {
			err = fmt.Errorf("telemetry output is not one JSON object: %w", jerr)
			doc = nil
		}
	}
	if err != nil && ctx.Err() != nil {
		// The caller gave up; that says nothing about the card, so do not cache it.
		return nil, err
	}
	e.at, e.doc, e.err = time.Now(), doc, err
	return doc, err
}

// CompactTelemetry reduces a telemetry document to the flat object /fleet/health publishes: the
// first card's headline numbers, absent keys left out (a reader takes absent as unknown, never as
// zero). ok is false when the document carries no card.
func CompactTelemetry(doc map[string]any) (map[string]any, bool) {
	cards, _ := doc["cards"].([]any)
	if len(cards) == 0 {
		return nil, false
	}
	card, _ := cards[0].(map[string]any)
	if card == nil {
		return nil, false
	}
	out := map[string]any{}
	copyKeys := func(dst string, src map[string]any, key string) {
		if v, ok := src[key]; ok && v != nil {
			out[dst] = v
		}
	}
	for _, k := range []string{"temp_c", "npu_load_pct", "cpu_load_pct", "cmm_used_mib", "cmm_total_mib", "mem_used_mib", "mem_total_mib"} {
		copyKeys(k, card, k)
	}
	if clocks, _ := card["clocks"].(map[string]any); clocks != nil {
		for _, k := range []string{"cpu_mhz", "cpu_max_mhz", "npu_mhz", "npu_max_mhz"} {
			copyKeys(k, clocks, k)
		}
		copyKeys("clocks_at_max", clocks, "at_max")
	}
	if pcie, _ := card["pcie"].(map[string]any); pcie != nil {
		for _, k := range []string{"link_speed", "link_width", "link_degraded"} {
			copyKeys(k, pcie, k)
		}
	}
	if procs, ok := card["processes"].([]any); ok {
		out["processes"] = len(procs)
	}
	copyKeys("sampled_at", doc, "sampled_at")
	return out, true
}

// TelemetrySource names one device's telemetry inputs: its sidecar base (read first) and its
// launcher (the one-shot fallback).
type TelemetrySource struct {
	Device   string
	Endpoint string
	Cmd      string
}

type collectorEntry struct {
	val      map[string]any
	at       time.Time
	inflight bool
}

// TelemetryCollector serves /fleet/health's per-device telemetry WITHOUT ever blocking a request:
// Get returns the cached compact object (nil before the first refresh lands) and, when the cache
// is older than CollectorTTL, starts one background refresh per device (never more than one in
// flight). A refresh reads the running sidecar's /health "telemetry" (2 s) and only when the
// sidecar does not answer falls back to the one-shot launcher probe, so a node whose sidecar is
// idle still reports its card.
type TelemetryCollector struct {
	mu  sync.Mutex
	ent map[string]*collectorEntry
	now func() time.Time
}

// NewTelemetryCollector returns an empty collector.
func NewTelemetryCollector() *TelemetryCollector {
	return &TelemetryCollector{ent: map[string]*collectorEntry{}, now: time.Now}
}

// Get returns a copy of the device's cached compact telemetry, or nil when none was read yet.
func (c *TelemetryCollector) Get(src TelemetrySource) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.ent[src.Device]
	if e == nil {
		e = &collectorEntry{}
		c.ent[src.Device] = e
	}
	if !e.inflight && (e.at.IsZero() || c.now().Sub(e.at) >= CollectorTTL) {
		e.inflight = true
		go c.refresh(src)
	}
	if e.val == nil {
		return nil
	}
	cp := make(map[string]any, len(e.val))
	for k, v := range e.val {
		cp[k] = v
	}
	return cp
}

func (c *TelemetryCollector) refresh(src TelemetrySource) {
	val := readCompactTelemetry(src)
	c.mu.Lock()
	e := c.ent[src.Device]
	e.val, e.at, e.inflight = val, c.now(), false
	c.mu.Unlock()
}

// readCompactTelemetry reads one device's telemetry now: the sidecar's /health first, the one-shot
// probe second. A device that yields nothing is reported as {error, sidecar_up, sampled_at}.
func readCompactTelemetry(src TelemetrySource) map[string]any {
	stamp := func(m map[string]any, up bool) map[string]any {
		m["sidecar_up"] = up
		if _, ok := m["sampled_at"]; !ok {
			m["sampled_at"] = time.Now().UTC().Format(time.RFC3339)
		}
		return m
	}
	fail := func(up bool, msg string) map[string]any {
		return stamp(map[string]any{"error": msg}, up)
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthTimeout+ProbeTimeout+time.Second)
	defer cancel()

	hctx, hcancel := context.WithTimeout(ctx, healthTimeout)
	h, herr := NewDevice(src.Device, src.Endpoint, healthTimeout).Health(hctx)
	hcancel()
	if herr == nil {
		doc, _ := h["telemetry"].(map[string]any)
		if doc == nil {
			return fail(true, "sidecar health carries no telemetry")
		}
		if compact, ok := CompactTelemetry(doc); ok {
			return stamp(compact, true)
		}
		return fail(true, "sidecar telemetry carries no card")
	}
	if src.Cmd == "" {
		return fail(false, "sidecar not running and no launcher configured")
	}
	doc, err := OneShotTelemetry(ctx, src.Device, src.Cmd)
	if err != nil {
		return fail(false, err.Error())
	}
	if compact, ok := CompactTelemetry(doc); ok {
		return stamp(compact, false)
	}
	return fail(false, "telemetry carries no card")
}
