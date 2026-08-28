// HTTP surface of the queue holder. Mounted by the fleet node ONLY when the
// operator sets fleet_queue_host (ADR 0030) — an unconfigured node serves
// none of these routes. Auth mirrors the fleet's bearer rule: when a
// fleet_auth_token is configured every queue route requires it (the queue
// carries agent contracts, the lane the fleet already tokens).
package fleetqueue

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

const maxQueueBody = 1 << 20 // mirror the dispatch cap: 1 MiB

// Mount registers the queue routes on mux. authOK decides bearer acceptance
// (the fleet node passes its existing check so the two surfaces cannot drift).
func Mount(mux *http.ServeMux, q *Queue, authOK func(*http.Request) bool) {
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !authOK(r) {
				jsonError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /fleet/queue/submit", guard(handleSubmit(q)))
	mux.HandleFunc("POST /fleet/queue/claim", guard(handleClaim(q)))
	mux.HandleFunc("POST /fleet/queue/ack", guard(handleAck(q)))
	mux.HandleFunc("POST /fleet/queue/nack", guard(handleNack(q)))
	mux.HandleFunc("GET /fleet/queue/jobs/{id}", guard(handleGet(q)))
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxQueueBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			jsonError(w, http.StatusBadRequest, "request body too large (limit 1 MiB)")
			return false
		}
		jsonError(w, http.StatusBadRequest, "malformed body: "+err.Error())
		return false
	}
	return true
}

func handleSubmit(q *Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			JobID      string          `json:"job_id"`
			TaskType   string          `json:"task_type"`
			Payload    json.RawMessage `json:"payload"`
			TimeoutSec int             `json:"timeout_sec"`
		}
		if !decode(w, r, &in) {
			return
		}
		state, err := q.Submit(in.JobID, in.TaskType, in.Payload, in.TimeoutSec)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": in.JobID, "state": state})
	}
}

func handleClaim(q *Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			NodeID    string   `json:"node_id"`
			TaskTypes []string `json:"task_types"`
		}
		if !decode(w, r, &in) {
			return
		}
		job, ok, err := q.Claim(in.NodeID, in.TaskTypes)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !ok {
			w.WriteHeader(http.StatusNoContent) // nothing claimable — the idle answer
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(job)
	}
}

func handleAck(q *Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			JobID  string          `json:"job_id"`
			NodeID string          `json:"node_id"`
			Result json.RawMessage `json:"result"`
			Error  string          `json:"error"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := q.Ack(in.JobID, in.NodeID, in.Result, in.Error); err != nil {
			jsonError(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleNack(q *Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			JobID  string `json:"job_id"`
			NodeID string `json:"node_id"`
			Reason string `json:"reason"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := q.Nack(in.JobID, in.NodeID, in.Reason); err != nil {
			jsonError(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleGet mirrors the push path's /fleet/jobs/{id} wire shape
// ({state,data,error}) so the delegator's poll implementation reads BOTH
// surfaces with one decoder.
func handleGet(q *Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		job, err := q.Get(id)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if job == nil {
			jsonError(w, http.StatusNotFound, fmt.Sprintf("no such job %s", id))
			return
		}
		state := job.State
		// The push namespace says accepted/running/done/error; map so one
		// poller serves both. claimed reads as running (a node holds it),
		// queued as accepted (admitted, not started) — the same semantics the
		// push node's backlog uses.
		switch job.State {
		case StateQueued:
			state = "accepted"
		case StateClaimed:
			state = "running"
		case StateFailed:
			state = "error"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state": state, "data": job.Result, "error": job.Error,
		})
	}
}
