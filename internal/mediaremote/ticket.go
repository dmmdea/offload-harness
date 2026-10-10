package mediaremote

// The ticket queue (P0 plan S2; ADR 0083): a busy image call is a TICKET, and the harness holds it until the first
// matching lane that frees ANYWHERE in the cluster runs it, with no caller loop.
//
// S1 made a busy local lane overflow to an IDLE matching node. A ticket is the other half of the operator's P0 ("route
// work to any PC in the cluster, even if all PCs are full"): when every lane is full the call waits for the first one
// to free instead of being refused or looping. One file per ticket under <state_dir>/media-queue records what every
// process of the machine needs to know about it: who owns it, where it stands, which node holds its job. The request
// BODY is held by the owning process only (it carries paths and a prompt the file has no business keeping); the file is
// enough to answer for a ticket, to collect its result, to see which nodes the machine is using, and to adopt a job
// whose owner died.
//
// What is persisted, and why each:
//
//	req_key       sha256 of (task, prompt, params minus waiter_token): any identical re-send attaches to the live ticket
//	              instead of making a second job (what ends the 85-answers loop, even for a caller that ignores the text)
//	seed          fixed once at creation when the caller gave none, so a bounce, a re-placement and an adoption all render
//	              the same composition
//	owner, beat   the process that runs it and the last time it said so; an owner that is gone (or silent past
//	              ownerStale) is dead, and what it left is swept (waiting, local) or adopted (placed, fetching)
//	place         the token that keeps the ticket's arrival time in the machine's own lease line (core.PlaceKeeper)
//	state         waiting, local, placing, placed, fetching, done or failed
//	node, job     where the job runs and under which id, for adoption and for the in-use rule
//	matches       the nodes whose recipe matches, as of the owner's last scan: how an older ticket of ANOTHER process holds
//	              a node against a younger one
//	cluster       the last answer to "where does it stand and why", per node, so any process can give it
//
// A scan writes none of: a PAIR card, a ledger row, a lease waiter. The one ledger row and the one PAIR card of a call
// are opened when it is dispatched (the remote attribution handle at the node's acceptance, or the local run's own).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// Timings of the queue. Transport and housekeeping constants, CHOSEN (not measured) and named here so a reader can see
// what is not a resource number: the scan rhythm bounds how long a lane that freed waits to be noticed, and the rest
// only bound how long a dead process's leftovers stay.
var (
	// scanEvery and scanSpread: one scanner per process looks at its tickets about every scanEvery, +/- scanSpread of
	// it so several processes of one machine do not look at the same instant.
	scanEvery  = 10 * time.Second
	scanSpread = 0.2
	// attachPoll is how often a call attached to a ticket another process owns reads its file.
	attachPoll = 250 * time.Millisecond
	// firstScanBound is how long a call waits for its ticket's first scan before it answers "queued" without one: the
	// answer is "where it stands and why", which a ticket nobody has looked at yet cannot give.
	firstScanBound = 15 * time.Second
	// ownerStale: an owner whose pid is alive but has not written its beat for this long is hung, and is treated as dead.
	ownerStale = 10 * time.Minute
	// doneKeep: a finished ticket nobody collected stays this long (the lease line's own place TTL).
	doneKeep = 10 * time.Minute
	// deadKeep: a placed ticket whose owner died stays adoptable this long (a job renders for minutes to hours).
	deadKeep = 6 * time.Hour
	// ticketMaxWait ends a ticket that has waited this long without a lane (the default wait of `gpu reserve --wait`, ADR 0039).
	ticketMaxWait = 8 * time.Hour
	// tmpStale: a temp file of a write that never finished.
	tmpStale = time.Minute
	// deadClaimGrace: a claim whose owner died stays in force this long after the owner's last beat. The job it sent keeps
	// running on the node, and the node's health shows its lease only once the job has been admitted; for those seconds the node
	// reads idle, and another process would send it a second job. After the grace the node's own health speaks for it.
	deadClaimGrace = 2 * time.Minute
)

const (
	queueDirName = "media-queue"
	lockName     = ".lock"
)

// ticket states.
const (
	stWaiting  = "waiting"  // no lane yet: it is looked at every scan
	stLocal    = "local"    // the local run is in flight (its wait for the lane is the pipeline's)
	stPlacing  = "placing"  // a node is claimed and the job is being sent
	stPlaced   = "placed"   // a node accepted the job
	stFetching = "fetching" // the node finished and its outputs are coming back
	stDone     = "done"     // the result is in the file, ok
	stFailed   = "failed"   // the result is in the file, a deferral or an error
)

func terminalState(s string) bool { return s == stDone || s == stFailed }
func inFlightState(s string) bool { return s == stPlacing || s == stPlaced || s == stFetching }

// ownerID names a process: its pid and the time it started (a recycled pid is a different process).
type ownerID struct {
	PID     int   `json:"pid"`
	StartMs int64 `json:"start_ms,omitempty"`
}

func selfOwner() ownerID {
	pid := os.Getpid()
	st, _ := gpulease.ProcessStart(pid)
	return ownerID{PID: pid, StartMs: st}
}

// ownerAliveFn is the liveness rule (a var so a test can kill an owner without a process to kill).
var ownerAliveFn = func(o ownerID) bool {
	if o.PID <= 0 || !gpulease.PIDAlive(o.PID) {
		return false
	}
	if o.StartMs != 0 {
		if st, ok := gpulease.ProcessStart(o.PID); ok && st != o.StartMs {
			return false
		}
	}
	return true
}

func (o ownerID) alive() bool { return ownerAliveFn(o) }

// ticketFile is what is persisted for one ticket.
type ticketFile struct {
	ID      string  `json:"id"`
	ReqKey  string  `json:"req_key"`
	Created int64   `json:"created_ms"`
	Owner   ownerID `json:"owner"`
	Beat    int64   `json:"heartbeat_ms"`
	Updated int64   `json:"updated_ms"`

	Task   string `json:"task"`
	Family string `json:"family,omitempty"`
	Digest string `json:"recipe_digest,omitempty"`
	Out    string `json:"out,omitempty"`
	Seed   int    `json:"seed"`

	// Place is the lease-line token that keeps the ticket's arrival time on this machine; PlaceNote says why there is none.
	Place     string `json:"local_token,omitempty"`
	PlaceNote string `json:"local_place_note,omitempty"`

	State string `json:"state"`
	// Node is the node id, Base its URL and Job the id the node knows the job by, once a node is claimed (Job: accepted).
	Node string `json:"node,omitempty"`
	Base string `json:"base,omitempty"`
	Job  string `json:"remote_job_id,omitempty"`

	// LaneWhy is the sentence of the last lane reading that found this machine's lane busy: the placement text of a result
	// that ran on a node says what the call left behind.
	LaneWhy string `json:"lane_why,omitempty"`

	// The last scan: whether one happened, which nodes' recipes matched, what each node said, where the ticket stands.
	Scanned  bool         `json:"scanned,omitempty"`
	Matches  []string     `json:"matches,omitempty"`
	Cluster  []ClusterRow `json:"cluster,omitempty"`
	Local    *LocalLane   `json:"local,omitempty"`
	Position int          `json:"position,omitempty"`

	// Result is the finished call's core.Result, once it is terminal.
	Result    json.RawMessage `json:"result,omitempty"`
	Collected bool            `json:"collected,omitempty"`
}

// LocalLane is the answer's word about this machine's own lane.
type LocalLane struct {
	// State is free, busy or ram (the host's memory, not the card, is what is short).
	State  string     `json:"state"`
	Why    string     `json:"why,omitempty"`
	Holder *RowHolder `json:"holder,omitempty"`
	// Ahead is how many callers hold a place in this machine's line before the ticket.
	Ahead int    `json:"ahead,omitempty"`
	Since string `json:"since,omitempty"`
}

var ticketIDPattern = regexp.MustCompile(`^mq-[a-z0-9]{8,32}$`)

// validTicketID: a ticket id arrives from a tool caller and becomes a path, so it is checked before anything else.
func validTicketID(s string) bool { return ticketIDPattern.MatchString(s) }

func newTicketID() string { return "mq-" + randHex(6) }

// reqKey identifies a request by its content: sha256 of the task, the prompt and the parameters AS SENT without the
// waiter_token. It is taken before the ticket fixes its seed, so a re-send that names none attaches, and a re-send that
// names another seed is another job. Numbers are canonicalised through JSON, so 2048, 2048.0 and a json.Number agree.
func reqKey(req core.Request) string {
	params := map[string]any{}
	for k, v := range req.Params {
		if k != "waiter_token" {
			params[k] = v
		}
	}
	b, err := json.Marshal(struct {
		Task   core.TaskType  `json:"task"`
		Prompt string         `json:"prompt"`
		Params map[string]any `json:"params"`
	}{req.Task, req.Input, params})
	if err == nil {
		var round any
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.UseNumber()
		if dec.Decode(&round) == nil {
			if b2, err2 := json.Marshal(round); err2 == nil {
				b = b2
			}
		}
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// store is the directory of ticket files of one machine.
type store struct{ dir string }

// storeFor opens the queue directory of cfg's state dir; ok is false when the machine has none (no state_dir), and then
// there is no queue.
func storeFor(cfg config.Config) (*store, bool) {
	if strings.TrimSpace(cfg.StateDir) == "" {
		return nil, false
	}
	return &store{dir: filepath.Join(cfg.StateDir, queueDirName)}, true
}

func (s *store) path(id string) string { return filepath.Join(s.dir, id+".json") }

func nowMs() int64 { return time.Now().UnixMilli() }

// read returns one ticket; ok is false for a ticket that is not there or does not parse.
func (s *store) read(id string) (ticketFile, bool) {
	if !validTicketID(id) {
		return ticketFile{}, false
	}
	var b []byte
	var err error
	for i := 0; i < fsTries; i++ {
		if b, err = os.ReadFile(s.path(id)); err == nil || os.IsNotExist(err) {
			break
		}
		time.Sleep(fsPause)
	}
	if err != nil {
		return ticketFile{}, false
	}
	var t ticketFile
	if json.Unmarshal(b, &t) != nil || t.ID != id {
		return ticketFile{}, false
	}
	return t, true
}

// write persists t.
func (s *store) write(t ticketFile) error {
	t.Updated = nowMs()
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path(t.ID), b)
}

func (s *store) remove(id string) {
	if validTicketID(id) {
		removeFile(s.path(id))
	}
}

// list returns every ticket on file, oldest first. A file that does not parse is skipped (sweep removes it).
func (s *store) list() []ticketFile {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []ticketFile
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "mq-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		if t, ok := s.read(strings.TrimSuffix(name, ".json")); ok {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created < out[j].Created
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// dead reports whether the owner of t is gone: its process is, or it has not written its beat for ownerStale.
func (t ticketFile) dead() bool {
	if !t.Owner.alive() {
		return true
	}
	return t.Beat > 0 && time.Since(time.UnixMilli(t.Beat)) > ownerStale
}

// ---- node claims ---------------------------------------------------------------------------------------------------

// claim says a ticket of this machine has a job on a node. It is the machine-wide "in use": every scanner on the
// machine reads the claims before it sends a node a job, and takes one (under the directory lock, which makes the
// read and the write one step) before it does. Reading the tickets' states and then sending would leave exactly the
// window in which two processes both read a node idle and both send it a job.
type claim struct {
	Base   string  `json:"base"`
	Ticket string  `json:"ticket"`
	Owner  ownerID `json:"owner"`
	At     int64   `json:"at_ms"`
}

func (s *store) claimPath(base string) string {
	sum := sha256.Sum256([]byte(normBase(base)))
	return filepath.Join(s.dir, "use-"+hex.EncodeToString(sum[:6])+".json")
}

func (s *store) readClaim(path string) (claim, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return claim{}, false
	}
	var c claim
	if json.Unmarshal(b, &c) != nil || c.Base == "" || c.Ticket == "" {
		return claim{}, false
	}
	return c, true
}

// claimLive: the claim's ticket is on file with a job in flight on that node, and its owner is alive (and has written its beat
// within ownerStale) or died less than deadClaimGrace ago. Anything else is a leak, or a dead process's long after, and does
// not hold the node.
func (s *store) claimLive(c claim) bool {
	t, ok := s.read(c.Ticket)
	if !ok || !inFlightState(t.State) || normBase(t.Base) != normBase(c.Base) {
		return false
	}
	age := time.Since(time.UnixMilli(t.Beat))
	if c.Owner.alive() {
		return t.Beat == 0 || age < ownerStale
	}
	return age < deadClaimGrace
}

// claimNode takes base for the ticket under the lock: ok is false, with the holder, when another live ticket has it.
// On success t is saved in `state` (placing for a job about to be sent; placed or fetching for a job taken over) on base,
// in the same step, so the claim is live from its first instant. A ticket that already holds the claim keeps it.
func (s *store) claimNode(t *ticketFile, base, nodeID, state string) (ok bool, holder claim, err error) {
	err = s.withLock(func() error {
		path := s.claimPath(base)
		if c, have := s.readClaim(path); have && c.Ticket != t.ID && s.claimLive(c) {
			holder = c
			return nil
		}
		t.State, t.Base, t.Node = state, normBase(base), nodeID
		if err := s.write(*t); err != nil {
			return err
		}
		b, _ := json.Marshal(claim{Base: normBase(base), Ticket: t.ID, Owner: t.Owner, At: nowMs()})
		if err := writeFileAtomic(path, b); err != nil {
			return err
		}
		ok = true
		return nil
	})
	return ok, holder, err
}

// releaseNode gives base back if the ticket holds it.
func (s *store) releaseNode(base, ticket string) {
	if base == "" {
		return
	}
	_ = s.withLock(func() error {
		path := s.claimPath(base)
		if c, have := s.readClaim(path); have && c.Ticket == ticket {
			removeFile(path)
		}
		return nil
	})
}

// claims returns the live claims, by node base.
func (s *store) claims() map[string]claim {
	out := map[string]claim{}
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return out
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "use-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if c, ok := s.readClaim(filepath.Join(s.dir, e.Name())); ok && s.claimLive(c) {
			out[normBase(c.Base)] = c
		}
	}
	return out
}

// sweep removes what a dead process left that nothing will ever collect, and returns the places of the tickets it
// removed so the caller can drop them from the lease line. It keeps a placed ticket of a dead owner (the next identical
// call adopts it) for deadKeep, and a finished one nobody collected for doneKeep.
func (s *store) sweep(all []ticketFile) (dropped []string) {
	now := time.Now()
	for _, t := range all {
		age := now.Sub(time.UnixMilli(maxInt64(t.Updated, t.Beat)))
		switch {
		case terminalState(t.State):
			if age > doneKeep {
				s.remove(t.ID)
			}
		case t.dead() && (t.State == stWaiting || t.State == stLocal):
			// Nothing was sent anywhere, or a render died with its process: there is nothing to adopt.
			s.remove(t.ID)
			if t.Place != "" {
				dropped = append(dropped, t.Place)
			}
		case t.dead() && inFlightState(t.State) && age > deadKeep:
			s.remove(t.ID)
		}
	}
	// A claim whose owner or ticket is gone holds nothing; a temp file of a write that never finished is debris.
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return dropped
	}
	for _, e := range ents {
		name := e.Name()
		path := filepath.Join(s.dir, name)
		switch {
		case strings.HasPrefix(name, "use-") && strings.HasSuffix(name, ".json"):
			if c, ok := s.readClaim(path); !ok || !s.claimLive(c) {
				_ = s.withLock(func() error {
					if c2, ok2 := s.readClaim(path); !ok2 || !s.claimLive(c2) {
						removeFile(path)
					}
					return nil
				})
			}
		case strings.HasSuffix(name, ".tmp"):
			if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > tmpStale {
				removeFile(path)
			}
		case strings.HasPrefix(name, "mq-") && strings.HasSuffix(name, ".json"):
			if _, ok := s.read(strings.TrimSuffix(name, ".json")); !ok {
				if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > tmpStale {
					removeFile(path) // does not parse and is not being written
				}
			}
		}
	}
	return dropped
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// QueueRow is one ticket as offload_status lists it (media_queue[]).
type QueueRow struct {
	Ticket   string `json:"ticket"`
	State    string `json:"state"`
	Task     string `json:"task"`
	Family   string `json:"family,omitempty"`
	Position int    `json:"position,omitempty"`
	// Node is where the job runs once a node has it; Job is the id the node knows it by.
	Node string `json:"node,omitempty"`
	Job  string `json:"remote_job_id,omitempty"`
	// Created is when the call arrived (RFC 3339, UTC) and AgeSec how long ago.
	Created string `json:"created"`
	AgeSec  int    `json:"age_s"`
	// Owner says whether the process that runs the ticket is alive; a dead owner's waiting ticket is swept by the next scan
	// and its placed one is adopted by the next identical call.
	OwnerAlive bool `json:"owner_alive"`
	// Place says whether the ticket holds a place in this machine's own line.
	Place bool `json:"holds_place"`
	// Why is the one sentence of where the ticket stands, for a ticket still waiting.
	Why string `json:"why,omitempty"`
}

// Queue lists the tickets of the machine, oldest first. nil when there are none (and when the machine keeps no queue), so a
// box that never queued answers offload_status byte-identically to before.
func Queue(cfg config.Config) []QueueRow {
	st, ok := storeFor(cfg)
	if !ok {
		return nil
	}
	var out []QueueRow
	for _, t := range st.list() {
		row := QueueRow{Ticket: t.ID, State: t.State, Task: t.Task, Family: t.Family, Position: t.Position, Node: t.Node, Job: t.Job,
			Created: time.UnixMilli(t.Created).UTC().Format(time.RFC3339), AgeSec: int(time.Since(time.UnixMilli(t.Created)).Seconds()),
			OwnerAlive: !t.dead(), Place: t.Place != ""}
		if t.State == stWaiting && t.Scanned {
			row.Why = clusterSummary(t)
		}
		out = append(out, row)
	}
	return out
}
