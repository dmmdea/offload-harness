// The fleet "forecast" lane: one time-series forecast runs on THIS node's own forecast seat
// (the llama-swap media seat bound by config forecast_model) for a caller whose box binds no
// forecast model, or whose own card is held. The node runs the call through its OWN pipeline
// (runForecast), so the seat, the GPU-lease fence and the request timeout all live here, where
// they are true, and the caller reads back the node's full core.Result, defers included.
//
// It is the text lane's shape (text_task.go) with a body of its own: a long multi-series request
// is megabytes of numbers, past dispatch's 1 MiB, so the door takes ForecastBodyCap and the job id
// rides inside the payload. The same token rule (tokenGated, the bearer BEFORE the body), the same
// admission path (Server.admit), results through the job store. The lane has one task, so there
// is no per-task list to publish: health lists "forecast" and names the bound alias.

package fleetnode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// ForecastTask is the lane's fleet task_type: what health lists in supported_task_types and
// what the job feed shows as `task`.
const ForecastTask = "forecast"

// ForecastBodyCap is the request-body ceiling of POST /fleet/forecast: 32 MiB, the size of
// a long multi-series request with covariates (the forecast server's own limit is the
// binding one; this only bounds what the door will buffer).
const ForecastBodyCap = 32 << 20

// ForecastPayload is the wire body of POST /fleet/forecast. job_id is the caller-minted id
// exactly as on /fleet/dispatch; request is the forecast server's payload object (series,
// prediction_length, quantiles, covariates, switches), passed through to the pipeline as-is.
type ForecastPayload struct {
	JobID   string          `json:"job_id"`
	Request json.RawMessage `json:"request"`
}

// ForecastLaneAdmissible is THE predicate behind the forecast lane, on both sides of the wire
// (health advertisement AND ack-time admission, the vision lane's discipline): the node binds a
// forecast model, and the listener is safely reachable (loopback, or a fleet_auth_token for
// anything beyond it).
func ForecastLaneAdmissible(cfg config.Config, loopbackListener bool) bool {
	return cfg.ForecastModel != "" && AgentLaneSafelyReachable(cfg, loopbackListener)
}

// handleForecast is the forecast lane's ack path: handleText with ForecastBodyCap and a
// ForecastPayload whose job_id rides inside it. The bearer is checked before the body is read.
// The decoded body becomes a "forecast" envelope and joins admit, so the known-id re-ack, drain,
// lease, band and queue gates and the job store apply exactly as for every job.
func (s *Server) handleForecast(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeGated(w, r, ForecastTask) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, ForecastBodyCap)
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("content-type must be application/json (got %q)", ct))
			return
		}
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusBadRequest, "request body too large (limit 32 MiB)")
			return
		}
		writeError(w, http.StatusBadRequest, "reading forecast body: "+err.Error())
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var p ForecastPayload
	if err := dec.Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "malformed forecast body: "+err.Error())
		return
	}
	s.admit(w, r, dispatchEnvelope{JobID: p.JobID, TaskType: ForecastTask, Payload: body})
}

// buildForecast translates a forecast payload into the core.Request the node's pipeline runs:
// the request object, compacted to canonical JSON, as the input. The pipeline and the forecast
// server own every deeper check (shapes, values, horizon), and their refusals come back inside
// the job result as the full core.Result; only a body that cannot be a forecast payload at all is
// a 400 at ack time.
func buildForecast(cfg config.Config, payload json.RawMessage) (core.Request, func(), error) {
	noop := func() {}
	var p ForecastPayload
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return core.Request{}, noop, fmt.Errorf("forecast: payload: %w", err)
	}
	if cfg.ForecastModel == "" {
		return core.Request{}, noop, fmt.Errorf("forecast: this node binds no forecast model")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(p.Request, &obj); err != nil || obj == nil {
		return core.Request{}, noop, fmt.Errorf("forecast: request must be a JSON object")
	}
	var canon bytes.Buffer
	if err := json.Compact(&canon, p.Request); err != nil {
		return core.Request{}, noop, fmt.Errorf("forecast: request: %w", err)
	}
	return core.Request{Task: core.TaskForecast, Input: canon.String(), Door: "fleet"}, noop, nil
}

// forecastJobData is what a forecast job stores as its result: the FULL core.Result, defers
// included, exactly as the text lane does (textJobData), so a caller's local and remote results
// stay byte-comparable and a defer is a "done" job whose data says so.
func forecastJobData(res core.Result) (json.RawMessage, error) {
	b, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("forecast: encoding result: %w", err)
	}
	return b, nil
}
