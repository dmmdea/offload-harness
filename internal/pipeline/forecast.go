package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/forecastclient"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// runForecast sends req.Input (the forecast server's JSON payload: series, prediction_length,
// quantiles, covariates, switches) to the Timestral seat and returns the server's JSON object as
// the result's data. Its own branch of Run: numbers in, numbers out, never the text cascade.
//
// No result cache. A forecast is 0.2-0.7 s warm, the payload is the identity and can be megabytes
// of numbers, and a hash of it would cost about what the call does; runTranscribe caches because a
// transcription costs minutes. Zero-always-warm is the seat's ttl, so there is no unload after a
// burst either.
func (p *Pipeline) runForecast(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	if p.cfg.ForecastModel == "" {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), "no forecast model configured")
		res := core.Deferf("no forecast model configured", "", meta)
		res.DeferClass = core.DeferClassConfig
		return res
	}
	model := p.cfg.ForecastModel
	meta.Model = model

	// A request that is not a JSON object cannot be a forecast payload; say so here instead of
	// waking the seat to be told. The server's own checks (shapes, values, horizon) come back as
	// bad input below.
	if !isJSONObject(req.Input) {
		return p.forecastBadInput(req, meta, start, "the forecast input is not a JSON object")
	}

	data, err := p.forecast.Forecast(ctx, model, []byte(req.Input))
	if err != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		// A held card is congestion, not a failure (register C-89): filed as capacity with the holder
		// named, the class a delegator re-places and a caller can retry.
		if modelaffinity.IsLeaseRefusal(err) {
			meta.ErrClass = "gpu_busy"
			reason := "gpu busy: " + err.Error()
			p.recordDefer(req.Task, meta, len(req.Input), reason)
			res := core.Deferf(reason, "", meta)
			res.DeferClass = core.DeferClassCapacity
			return res
		}
		if errors.Is(err, forecastclient.ErrBadInput) {
			return p.forecastBadInput(req, meta, start, err.Error())
		}
		meta.ErrClass = classifyErr(err)
		reason := "forecast call failed: " + err.Error()
		p.recordDefer(req.Task, meta, len(req.Input), reason)
		res := core.Deferf(reason, "", meta)
		res.DeferClass = core.DeferClassInfrastructure
		return res
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	p.record(req.Task, meta, len(req.Input))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// forecastBadInput files a request the forecast cannot run as given: the contract class, the
// bad_input error class, the server's detail in the reason. No node and no retry changes it.
func (p *Pipeline) forecastBadInput(req core.Request, meta core.Meta, start time.Time, reason string) core.Result {
	meta.LatencyMs = time.Since(start).Milliseconds()
	meta.ErrClass = core.ErrClassBadInput
	p.recordDefer(req.Task, meta, len(req.Input), reason)
	res := core.Deferf(reason, "", meta)
	res.DeferClass = core.DeferClassContract
	return res
}

// isJSONObject reports whether s is one well-formed JSON object.
func isJSONObject(s string) bool {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "{") {
		return false
	}
	var m map[string]json.RawMessage
	return json.Unmarshal([]byte(t), &m) == nil
}
