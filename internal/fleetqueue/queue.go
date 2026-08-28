// Package fleetqueue is the consolidated PULL queue — Option B of the
// 2026-08-26 decision, built DARK per the operator's 2026-08-28 direction:
// the code ships complete and tested, and stays inert until the operator
// binds the three config keys (ADR 0030). Option A's push path remains the
// default; nothing here runs on an unconfigured box.
//
// Shape (from the decision doc): ONE node — config-elected, the always-on
// box — HOSTS the queue inside its existing fleet-node process (the
// established-exception daemon; no new process). Delegators SUBMIT; every
// claiming node — the holder included — pulls work it is eligible for. The
// store is durable (bbolt, already in-tree), so holder restarts lose nothing.
//
// Semantics are AT-LEAST-ONCE by design: a claimed job whose lease expires
// returns to the queue, so a claimant that died mid-run is retried elsewhere —
// and one that merely stalled may complete a job that re-ran. Double
// EXECUTION is therefore possible at lease boundaries; double RESULT is not
// (the first ack wins, later acks are ignored). The lease is sized from the
// contract's own timeout plus slack to make the double-execution window a
// crashed-node event, not a slow-node event.
package fleetqueue

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	// StateQueued .. StateFailed are the job lifecycle. Queued jobs are
	// claimable; claimed jobs run under a lease; done/failed are terminal.
	StateQueued  = "queued"
	StateClaimed = "claimed"
	StateDone    = "done"
	StateFailed  = "failed"

	// maxNacks bounds requeues: a job two nodes could not run is not going
	// to succeed on a third identical seat; it fails loudly instead.
	maxNacks = 2
	// leaseSlack pads the claimant's declared budget: the lease must outlive
	// an honest slow run (queue wait is not charged to contracts on nodes),
	// so expiry means a dead claimant, not a busy one.
	leaseSlack = 5 * time.Minute
	// bucket is the single bbolt bucket; keys are job ids.
	bucket = "jobs"
)

// Job is the stored envelope + lifecycle. Payload/TaskType mirror the push
// path's dispatch envelope exactly so a claiming node runs it through the
// SAME BuildRequest surface a pushed job uses.
type Job struct {
	ID         string          `json:"id"`
	TaskType   string          `json:"task_type"`
	Payload    json.RawMessage `json:"payload"`
	TimeoutSec int             `json:"timeout_sec"`
	State      string          `json:"state"`
	Claimant   string          `json:"claimant,omitempty"`
	LeaseUntil int64           `json:"lease_until,omitempty"`
	Nacks      int             `json:"nacks"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	Submitted  int64           `json:"submitted"`
}

// Queue is the durable store. All methods are safe for concurrent use; bbolt
// serializes writes and the mutex keeps read-modify-write transitions atomic
// at this layer's granularity.
type Queue struct {
	mu sync.Mutex
	db *bolt.DB
	// now is the clock seam (tests move it); nil = time.Now.
	now func() time.Time
}

func (q *Queue) clock() time.Time {
	if q.now != nil {
		return q.now()
	}
	return time.Now()
}

// Open opens (creating if absent) the queue store at path.
func Open(path string) (*Queue, error) {
	db, err := bolt.Open(path, 0o644, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("fleetqueue: open %s: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, berr := tx.CreateBucketIfNotExists([]byte(bucket))
		return berr
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("fleetqueue: bucket: %w", err)
	}
	return &Queue{db: db}, nil
}

// Close closes the store.
func (q *Queue) Close() error { return q.db.Close() }

func (q *Queue) get(tx *bolt.Tx, id string) (*Job, error) {
	raw := tx.Bucket([]byte(bucket)).Get([]byte(id))
	if raw == nil {
		return nil, nil
	}
	var j Job
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, fmt.Errorf("fleetqueue: corrupt job %s: %w", id, err)
	}
	return &j, nil
}

func (q *Queue) put(tx *bolt.Tx, j *Job) error {
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return tx.Bucket([]byte(bucket)).Put([]byte(j.ID), raw)
}

// Submit stores a new queued job. Idempotent on id: re-submitting an existing
// job (lost-ack retry) returns its current state without touching it.
func (q *Queue) Submit(id, taskType string, payload json.RawMessage, timeoutSec int) (state string, err error) {
	if id == "" || taskType == "" {
		return "", errors.New("fleetqueue: id and task_type required")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	err = q.db.Update(func(tx *bolt.Tx) error {
		if existing, gerr := q.get(tx, id); gerr != nil {
			return gerr
		} else if existing != nil {
			state = existing.State
			return nil
		}
		state = StateQueued
		return q.put(tx, &Job{
			ID: id, TaskType: taskType, Payload: payload, TimeoutSec: timeoutSec,
			State: StateQueued, Submitted: q.clock().Unix(),
		})
	})
	return state, err
}

// Claim hands the oldest queued job whose task type the claimant declared to
// nodeID under a lease, or ok=false when nothing is claimable. Expired leases
// are reclaimed inline (no background reaper needed for correctness; ReapLeases
// exists to keep latency down between claims).
func (q *Queue) Claim(nodeID string, taskTypes []string) (job *Job, ok bool, err error) {
	if nodeID == "" {
		return nil, false, errors.New("fleetqueue: node_id required")
	}
	accepts := map[string]bool{}
	for _, t := range taskTypes {
		accepts[t] = true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.clock()
	err = q.db.Update(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(bucket)).Cursor()
		var oldest *Job
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var j Job
			if json.Unmarshal(v, &j) != nil {
				continue
			}
			claimable := j.State == StateQueued ||
				(j.State == StateClaimed && now.Unix() > j.LeaseUntil)
			if !claimable || !accepts[j.TaskType] {
				continue
			}
			// An expired lease is a requeue-with-history, counted like a nack:
			// a job that keeps killing (or outliving) its claimants must
			// terminate, not orbit.
			if j.State == StateClaimed {
				j.Nacks++
				if j.Nacks > maxNacks {
					j.State = StateFailed
					j.Error = fmt.Sprintf("lease expired %d times (last claimant %s) — abandoned", j.Nacks, j.Claimant)
					if perr := q.put(tx, &j); perr != nil {
						return perr
					}
					continue
				}
			}
			if oldest == nil || j.Submitted < oldest.Submitted {
				jj := j
				oldest = &jj
			}
		}
		if oldest == nil {
			return nil
		}
		oldest.State = StateClaimed
		oldest.Claimant = nodeID
		lease := time.Duration(oldest.TimeoutSec)*time.Second + leaseSlack
		oldest.LeaseUntil = now.Add(lease).Unix()
		if perr := q.put(tx, oldest); perr != nil {
			return perr
		}
		job, ok = oldest, true
		return nil
	})
	return job, ok, err
}

// Ack finalizes a job with its result. First ack wins; a later ack (an
// expired-lease claimant finishing after the re-run) is ignored, never an
// error — at-least-once means late duplicates are expected, not exceptional.
func (q *Queue) Ack(id, nodeID string, result json.RawMessage, jobErr string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.db.Update(func(tx *bolt.Tx) error {
		j, err := q.get(tx, id)
		if err != nil {
			return err
		}
		if j == nil {
			return fmt.Errorf("fleetqueue: ack of unknown job %s", id)
		}
		if j.State == StateDone || j.State == StateFailed {
			return nil // first ack won; this one is the expected late duplicate
		}
		if jobErr != "" {
			j.State, j.Error = StateFailed, jobErr
		} else {
			j.State, j.Result = StateDone, result
		}
		j.Claimant = nodeID
		return q.put(tx, j)
	})
}

// Nack returns a claimed job to the queue (the claimant could not run it —
// build failure, refused admission). Bounded by maxNacks, then failed.
func (q *Queue) Nack(id, nodeID, reason string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.db.Update(func(tx *bolt.Tx) error {
		j, err := q.get(tx, id)
		if err != nil {
			return err
		}
		if j == nil {
			return fmt.Errorf("fleetqueue: nack of unknown job %s", id)
		}
		if j.State != StateClaimed {
			return nil // already terminal or requeued; nothing to undo
		}
		j.Nacks++
		if j.Nacks > maxNacks {
			j.State = StateFailed
			j.Error = fmt.Sprintf("nacked %d times (last: %s by %s) — abandoned", j.Nacks, reason, nodeID)
		} else {
			j.State, j.Claimant, j.LeaseUntil = StateQueued, "", 0
		}
		return q.put(tx, j)
	})
}

// Get returns the job view for result polling.
func (q *Queue) Get(id string) (*Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out *Job
	err := q.db.View(func(tx *bolt.Tx) error {
		j, gerr := q.get(tx, id)
		out = j
		return gerr
	})
	return out, err
}

// Depth counts non-terminal jobs (queued + claimed) — the holder's health datum.
func (q *Queue) Depth() (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	err := q.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucket)).ForEach(func(_, v []byte) error {
			var j Job
			if json.Unmarshal(v, &j) == nil && (j.State == StateQueued || j.State == StateClaimed) {
				n++
			}
			return nil
		})
	})
	return n, err
}
