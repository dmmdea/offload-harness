package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/delegate"
)

// The delegator's rescue of a finished answer used to try ONE place: this box's own agent seat. The
// recorded rescue found that seat cold and its cards held by another session's lease, warmed it,
// and was cut at its 120 s allowance: a finished, correct answer lost for a seat that was never the
// natural place to re-pack it. The node that wrote the answer has just decoded it and holds its seat
// hot. The rescue is handed that node and seat (delegate.WithRescueOrigin) and, when its own seat
// is not resident, re-packs there through the node's chat door, which waits for no load here and
// never queues behind this box's lease (register C-41c).

// originNode fakes the node that wrote the answer: GET /fleet/health as a delegator reads it, and
// POST /fleet/chat, recording the credential and the model it was asked for.
type originNode struct {
	srv   *httptest.Server
	hits  atomic.Int64
	auth  atomic.Value // string
	model atomic.Value // string
	user  atomic.Value // string
}

func newOriginNode(t *testing.T, seat string, chatLane bool, reply func(n int64) string) *originNode {
	t.Helper()
	n := &originNode{}
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/fleet/health":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"node_id": "node-o", "chat_lane": chatLane, "served_models": []string{seat}})
		case r.Method == http.MethodPost && r.URL.Path == "/fleet/chat":
			count := n.hits.Add(1)
			n.auth.Store(r.Header.Get("Authorization"))
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m, _ := body["model"].(string)
			n.model.Store(m)
			_, u := promptOf(t, body)
			n.user.Store(u)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(doneChat(reply(count))))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(n.srv.Close)
	return n
}

func (n *originNode) ctx(seat string) context.Context {
	return delegate.WithRescueOrigin(context.Background(), delegate.RescueOrigin{Base: n.srv.URL, Seat: seat, Node: "node-o"})
}

func str(v *atomic.Value) string { s, _ := v.Load().(string); return s }

// coldBox is a delegator box whose agent seat is not resident, and counts every request that
// reaches it. It serves no completion at all, so a request that arrives is a request that shows
// the rescue did not leave.
func coldBox(t *testing.T) (*agentFake, *httptest.Server) {
	t.Helper()
	fake := &agentFake{rosterIDs: []string{agentTestSeat}, running: func(int64) string { return `{"running":[]}` }}
	return fake, fake.server(t)
}

const rescueAnswer = "The answer is 42, from the notes."

func TestRescueRepackRePacksOnTheNodeThatWroteTheAnswerWhenItsOwnSeatIsCold(t *testing.T) {
	node := newOriginNode(t, "origin-seat", true, func(int64) string { return `{"answer":"42"}` })
	fake, srv := coldBox(t)
	p := admissionTestPipeline(t, srv.URL, 5)
	p.cfg.FleetAuthToken = "tok"

	start := time.Now()
	got, err := p.RescueRepack(node.ctx("origin-seat"), testContract(), rescueAnswer, 0)
	if err != nil || string(got.Structured) != `{"answer":"42"}` {
		t.Fatalf("rescued = %+v err = %v, want the object the node's seat wrote", got, err)
	}
	if got.Seat != "origin-seat" || !strings.Contains(got.How, "node-o") {
		t.Fatalf("seat = %q how = %q, want the origin seat and the node named in the note", got.Seat, got.How)
	}
	if node.hits.Load() != 1 || str(&node.auth) != "Bearer tok" || str(&node.model) != "origin-seat" {
		t.Fatalf("node saw hits=%d auth=%q model=%q, want one call with the fleet bearer for its own seat", node.hits.Load(), str(&node.auth), str(&node.model))
	}
	if n := fake.grammarCNT.Load() + fake.chatFallbackCNT.Load() + fake.upstreamCNT.Load(); n != 0 {
		t.Fatalf("this box's seat saw %d request(s) (completion or warm-up), want none: it is cold and the node holds the answer's seat hot", n)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("the rescue took %v", time.Since(start))
	}
}

// The recorded shape: this box's cards are held by another session's lease. The node's door never
// queues behind it, so the rescue finishes at once instead of waiting out the lease and its allowance.
func TestRescueRepackDoesNotQueueBehindThisBoxsLeaseWhenTheNodeCanRePack(t *testing.T) {
	node := newOriginNode(t, "origin-seat", true, func(int64) string { return `{"answer":"42"}` })
	fake := &agentFake{rosterIDs: []string{agentTestSeat}, running: func(int64) string { return `{"running":[]}` }}
	srv := fake.server(t)
	p, _ := fencedPipeline(t, srv.URL, 5)
	p.cfg.FleetAuthToken = "tok"

	start := time.Now()
	got, err := p.RescueRepack(node.ctx("origin-seat"), testContract(), rescueAnswer, 0)
	if err != nil || string(got.Structured) != `{"answer":"42"}` {
		t.Fatalf("rescued = %+v err = %v", got, err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the rescue took %v with the lease held: it queued behind it", took)
	}
	if node.hits.Load() != 1 {
		t.Fatalf("node calls = %d, want 1", node.hits.Load())
	}
}

// A resident seat is the fastest place there is: the node is not asked.
func TestRescueRepackKeepsItsOwnResidentSeat(t *testing.T) {
	node := newOriginNode(t, "origin-seat", true, func(int64) string { return `{"answer":"from the node"}` })
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		repack:    func(int64) string { return `{"answer":"42"}` },
		running:   func(int64) string { return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"y"}]}` },
	}
	srv := fake.server(t)
	p := admissionTestPipeline(t, srv.URL, 5)
	p.cfg.FleetAuthToken = "tok"

	got, err := p.RescueRepack(node.ctx("origin-seat"), testContract(), rescueAnswer, 0)
	if err != nil || string(got.Structured) != `{"answer":"42"}` || got.Seat != agentTestSeat {
		t.Fatalf("rescued = %+v err = %v, want the object this box's resident seat wrote", got, err)
	}
	if node.hits.Load() != 0 {
		t.Fatalf("the node was asked %d time(s) though this box's seat was resident", node.hits.Load())
	}
}

// "Could not tell" never moves work off the box: an unreadable /running leaves the rescue where it was.
func TestRescueRepackStaysHomeWhenItCannotTellWhetherItsSeatIsResident(t *testing.T) {
	node := newOriginNode(t, "origin-seat", true, func(int64) string { return `{"answer":"from the node"}` })
	fake := &agentFake{rosterIDs: []string{agentTestSeat}, repack: func(int64) string { return `{"answer":"42"}` }} // no /running route: 404
	srv := fake.server(t)
	p := admissionTestPipeline(t, srv.URL, 5)
	p.cfg.FleetAuthToken = "tok"

	got, err := p.RescueRepack(node.ctx("origin-seat"), testContract(), rescueAnswer, 0)
	if err != nil || string(got.Structured) != `{"answer":"42"}` || node.hits.Load() != 0 {
		t.Fatalf("rescued = %+v err = %v node calls = %d, want this box's own seat and no call to the node", got, err, node.hits.Load())
	}
}

// A node that cannot take the call (an older build with no chat door) leaves the rescue where it
// was: the cold seat is warmed and the re-pack runs there, and the note says what the node did.
func TestRescueRepackFallsBackToItsOwnSeatWhenTheNodeHasNoChatDoor(t *testing.T) {
	defer compressLiveness(t, 300*time.Millisecond, 100*time.Millisecond, 1800)()
	defer compressRepackBound(t, 300*time.Millisecond)()
	node := newOriginNode(t, "origin-seat", false, func(int64) string { return `{"answer":"from the node"}` })
	fake := coldRescueFake(50 * time.Millisecond)
	srv := fake.server(t)
	p := admissionTestPipeline(t, srv.URL, 30)
	p.cfg.FleetAuthToken = "tok"

	got, err := p.RescueRepack(node.ctx("origin-seat"), testContract(), rescueAnswer, 0)
	if err != nil || string(got.Structured) != `{"answer":"42"}` || got.Seat != agentTestSeat {
		t.Fatalf("rescued = %+v err = %v, want the object this box's seat wrote after the warm-up", got, err)
	}
	if node.hits.Load() != 0 {
		t.Fatalf("the node was called %d time(s) though it advertises no chat door", node.hits.Load())
	}
	if !strings.Contains(got.How, "node-o") || !strings.Contains(got.How, "cold load") {
		t.Fatalf("how = %q, want the node's refusal and the warm-up both named", got.How)
	}
}

// A rescue that fails says everything it tried: the node, and the warm-up that burned this box's
// window (the error used to carry neither, and the recorded note could not say whether warming the
// fenced seat had spent the time).
func TestRescueRepackFailureNamesEveryRungItTried(t *testing.T) {
	defer compressLiveness(t, 300*time.Millisecond, 100*time.Millisecond, 1800)()
	defer compressRepackBound(t, 300*time.Millisecond)()
	node := newOriginNode(t, "origin-seat", false, func(int64) string { return `{"answer":"from the node"}` })
	fake := coldRescueFake(50 * time.Millisecond)
	fake.repackStream = func(_ int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}
	srv := fake.server(t)
	p := admissionTestPipeline(t, srv.URL, 30)
	p.cfg.FleetAuthToken = "tok"

	_, err := p.RescueRepack(node.ctx("origin-seat"), testContract(), rescueAnswer, 0)
	if err == nil {
		t.Fatal("a rescue that could not produce an object returned no error")
	}
	for _, want := range []string{"node-o", "cold load", agentTestSeat} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to name %q", err, want)
		}
	}
}

// The node's object is held to the same reading as any other: an all-empty list over an answer that
// holds list items under another key is no rescue.
func TestRescueRepackRefusesAHollowObjectFromTheNode(t *testing.T) {
	answer := invertedKeysAnswer(t, 5411)
	node := newOriginNode(t, "origin-seat", true, func(int64) string { return hollowObject })
	fake, srv := coldBox(t)
	_ = fake
	p := admissionTestPipeline(t, srv.URL, -1) // no warm-up: this box's seat is not an option
	p.cfg.FleetAuthToken = "tok"

	got, err := p.RescueRepack(node.ctx("origin-seat"), fidelityContract(), answer, 0)
	if err == nil || !strings.Contains(err.Error(), "emptied") {
		t.Fatalf("rescued = %+v err = %v, want the hollow object refused for its emptied list", got, err)
	}
	if len(got.Structured) != 0 {
		t.Fatalf("a hollow object was returned as the rescue: %s", got.Structured)
	}
}

// B3: the first attempt's failure on the box's own seat names the warm-up too.
func TestRescueRepackOwnSeatFailureCarriesTheWarmUpNote(t *testing.T) {
	defer compressLiveness(t, 300*time.Millisecond, 100*time.Millisecond, 1800)()
	defer compressRepackBound(t, 300*time.Millisecond)()
	fake := coldRescueFake(50 * time.Millisecond)
	fake.repackStream = func(_ int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}
	srv := fake.server(t)
	p := admissionTestPipeline(t, srv.URL, 30)

	_, err := p.RescueRepack(context.Background(), testContract(), rescueAnswer, 0)
	if err == nil || !strings.Contains(err.Error(), "cold load") {
		t.Fatalf("error = %v, want the warm-up named: a rescue whose own seat was cold must say whether the load spent its window", err)
	}
}

// The rescue's budget is one budget: a node that spent it leaves nothing to warm this box's seat with,
// and the caller's re-placement is credited the time the rescue did not waste.
func TestRescueRepackDoesNotWarmItsOwnSeatWhenTheNodeSpentTheBudget(t *testing.T) {
	node := newOriginNode(t, "origin-seat", true, func(int64) string {
		time.Sleep(300 * time.Millisecond)
		return `{"wrong":"shape"}`
	})
	fake, srv := coldBox(t)
	p := admissionTestPipeline(t, srv.URL, 30)
	p.cfg.FleetAuthToken = "tok"

	_, err := p.RescueRepack(node.ctx("origin-seat"), testContract(), rescueAnswer, 700*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "too little") || !strings.Contains(err.Error(), "node-o") {
		t.Fatalf("error = %v, want the node's failure and the budget it left named", err)
	}
	if fake.upstreamCNT.Load() != 0 || node.hits.Load() != 1 {
		t.Fatalf("warm requests = %d node calls = %d, want no warm-up and one call to the node", fake.upstreamCNT.Load(), node.hits.Load())
	}
}

// With budget to spare, a node that cannot take the call leaves this box's seat the whole of what is left.
func TestRescueRepackUsesItsOwnSeatWithTheBudgetTheNodeLeft(t *testing.T) {
	defer compressLiveness(t, 300*time.Millisecond, 100*time.Millisecond, 1800)()
	defer compressRepackBound(t, 300*time.Millisecond)()
	node := newOriginNode(t, "origin-seat", false, func(int64) string { return `{"answer":"from the node"}` })
	fake := coldRescueFake(50 * time.Millisecond)
	srv := fake.server(t)
	p := admissionTestPipeline(t, srv.URL, 30)
	p.cfg.FleetAuthToken = "tok"

	got, err := p.RescueRepack(node.ctx("origin-seat"), testContract(), rescueAnswer, 60*time.Second)
	if err != nil || string(got.Structured) != `{"answer":"42"}` || got.Seat != agentTestSeat {
		t.Fatalf("rescued = %+v err = %v, want the object this box's seat wrote", got, err)
	}
}

// The origin's object that kept fewer list items than its answer held says so in the note.
func TestRescueRepackNotesAnObjectThatKeptFewerItemsThanItsAnswerHeld(t *testing.T) {
	answer := invertedKeysAnswer(t, 5411)
	node := newOriginNode(t, "origin-seat", true, func(int64) string { return faithfulObject() })
	_, srv := coldBox(t)
	p := admissionTestPipeline(t, srv.URL, -1)
	p.cfg.FleetAuthToken = "tok"

	got, err := p.RescueRepack(node.ctx("origin-seat"), fidelityContract(), answer, 0)
	if err != nil || !strings.Contains(got.How, "kept 10 of 11") {
		t.Fatalf("rescued = %+v err = %v, want the note that ten of eleven items were kept", got, err)
	}
}
