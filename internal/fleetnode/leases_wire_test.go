package fleetnode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// Fleet per-card lease truth (plan P7). A node with card-scoped leases has several live
// leases at once; health used to describe only the lowest epoch, in one singular block.
// These tests pin the node's half of the wire: one entry per live lease with the cards it
// sits on, the singular block folded to the worst reading so a delegator one release behind
// is never told less than is true, and a node-level "refusing" that does not close the whole
// box over a lease on one card.

// Synthetic cards: the lease id of a card is its UUID lower-cased.
const (
	testUUIDa = "GPU-AAAA0000-0000-4000-8000-000000000001"
	testUUIDb = "GPU-BBBB0000-0000-4000-8000-000000000002"
	testUUIDc = "GPU-CCCC0000-0000-4000-8000-000000000003"
)

func leaseIDOf(uuid string) string {
	out := []byte(uuid)
	for i, c := range out {
		if c >= 'A' && c <= 'Z' {
			out[i] = c + 32
		}
	}
	return string(out)
}

// threeCardSnapshot is the sampler's reading of a three-card box.
func threeCardSnapshot() (Snapshot, bool) {
	return Snapshot{TotalGiB: 16, FreeGiB: 12.5, At: time.Now(), Devices: []gpuprobe.Device{
		{Index: 0, UUID: testUUIDa, Name: "Card A", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
		{Index: 1, UUID: testUUIDb, Name: "Card B", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
		{Index: 2, UUID: testUUIDc, Name: "Card C", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
	}}, true
}

// liveLease builds one live lease as the inspector returns it.
func liveLease(epoch uint64, class gpulease.Class, devices []string, remaining time.Duration) gpulease.Info {
	return gpulease.Info{
		Held: true, Class: class, Epoch: epoch, Epochs: []uint64{epoch}, PID: 1000 + int(epoch),
		Reason: "lease " + string(class), ExpiresAt: time.Now().Add(remaining), Devices: devices,
	}
}

// severalLeases is what the inspector returns when more than one lease is live: the lowest
// epoch's record with every lease under Leases.
func severalLeases(ls ...gpulease.Info) gpulease.Info {
	out := ls[0]
	out.Leases = ls
	out.Epochs = nil
	for _, l := range ls {
		out.Epochs = append(out.Epochs, l.Epoch)
	}
	return out
}

func leasedNode(t *testing.T, info gpulease.Info, standing func(gpulease.Info) gpulease.Standing, snap func() (Snapshot, bool)) *Server {
	t.Helper()
	if snap == nil {
		snap = threeCardSnapshot
	}
	opts := &Options{
		NodeID: "leased-node", Snapshot: snap,
		Footprints: func() []FootprintEntry { return nil },
		GpuVendor:  "nvidia", GpuArch: "ampere",
		Lease:         func() gpulease.Info { return info },
		LeaseStanding: standing,
	}
	s, _ := newTestServer(t, config.Config{}, &fakeRunner{}, opts)
	return s
}

func rawHealth(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health %d: %s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func leaseEntries(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, _ := m["leases"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		e, _ := r.(map[string]any)
		out = append(out, e)
	}
	return out
}

func strList(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		s, _ := r.(string)
		out = append(out, s)
	}
	return out
}

// Every live lease is published with the cards it sits on. Before this a node with a render
// on one card and a reservation on another published one block for the lowest epoch, and a
// delegator could not tell which cards were spoken for.
func TestHealthPublishesEveryLiveLeaseWithItsCards(t *testing.T) {
	media := liveLease(7, gpulease.ClassMedia, []string{leaseIDOf(testUUIDc)}, 6*time.Hour)
	text := liveLease(9, gpulease.ClassText, []string{leaseIDOf(testUUIDa)}, time.Hour)
	text.Exclusive = true
	s := leasedNode(t, severalLeases(media, text), nil, nil)

	entries := leaseEntries(t, rawHealth(t, s))
	if len(entries) != 2 {
		t.Fatalf("leases = %v, want one entry per live lease", entries)
	}
	if entries[0]["epoch"] != float64(7) || entries[0]["class"] != "media" {
		t.Fatalf("entry 0 = %v, want the media lease, lowest epoch first", entries[0])
	}
	if got := strList(entries[0]["devices"]); len(got) != 1 || got[0] != leaseIDOf(testUUIDc) {
		t.Fatalf("media lease devices = %v, want its one card as a lower-cased uuid", got)
	}
	if entries[0]["busy"] != true {
		t.Fatalf("a six-hour lease must read busy on its own entry: %v", entries[0])
	}
	if entries[1]["epoch"] != float64(9) || entries[1]["exclusive"] != true || entries[1]["class"] != "text" {
		t.Fatalf("entry 1 = %v, want the exclusive text lease", entries[1])
	}
	if got := strList(entries[1]["devices"]); len(got) != 1 || got[0] != leaseIDOf(testUUIDa) {
		t.Fatalf("text lease devices = %v", got)
	}
	if _, ok := entries[1]["until"].(string); !ok {
		t.Fatalf("each entry carries its own term end: %v", entries[1])
	}
}

// A delegator one release behind reads only the singular lease block and the two top-level
// fence flags. With a short media lease on one card and an exclusive text lease on another,
// the lowest epoch is the media lease: a block built from it says "media, not exclusive, not
// busy", which is less than is true. The singular block is the worst across the live leases.
func TestHealthSingularLeaseBlockIsTheWorstAcrossLeases(t *testing.T) {
	short := liveLease(7, gpulease.ClassMedia, []string{leaseIDOf(testUUIDc)}, 30*time.Second)
	text := liveLease(9, gpulease.ClassText, []string{leaseIDOf(testUUIDa)}, time.Hour)
	text.Exclusive = true
	text.Draining = true
	m := rawHealth(t, leasedNode(t, severalLeases(short, text), nil, nil))

	block, _ := m["lease"].(map[string]any)
	if block == nil {
		t.Fatalf("no singular lease block: %v", m)
	}
	if block["class"] != "text" {
		t.Fatalf("lease.class = %v, want text: a text lease anywhere is the one an older delegator refuses on", block["class"])
	}
	if block["busy"] != true {
		t.Fatalf("lease.busy = %v, want true: the hour-long lease is busy even though the lowest epoch is not", block["busy"])
	}
	if m["lease_exclusive"] != true || m["lease_draining"] != true {
		t.Fatalf("lease_exclusive/lease_draining = %v/%v, want both true (any live lease)", m["lease_exclusive"], m["lease_draining"])
	}
	if sec, _ := block["remaining_sec"].(float64); sec < 3000 {
		t.Fatalf("lease.remaining_sec = %v, want the longest remaining across the leases (about 3600)", block["remaining_sec"])
	}
	if block["pid"] != float64(1007) {
		t.Fatalf("lease.pid = %v, want the lowest epoch's holder (the record Info describes)", block["pid"])
	}
}

// The fence flags are the OR across the leases whichever epoch carries them: an exclusive
// reservation on the lowest epoch is not hidden by a plain sibling above it.
func TestHealthFenceFlagsAreAnyLiveLeaseNotTheLastOne(t *testing.T) {
	excl := liveLease(3, gpulease.ClassText, []string{leaseIDOf(testUUIDa)}, time.Hour)
	excl.Exclusive, excl.Draining = true, true
	plain := liveLease(8, gpulease.ClassMedia, []string{leaseIDOf(testUUIDc)}, time.Hour)
	m := rawHealth(t, leasedNode(t, severalLeases(excl, plain), nil, nil))
	if m["lease_exclusive"] != true || m["lease_draining"] != true {
		t.Fatalf("lease_exclusive/lease_draining = %v/%v, want both true: the lowest epoch is exclusive and draining", m["lease_exclusive"], m["lease_draining"])
	}
}

// A stalled lease past its window reads stalled, and an orphaned one past its window reads
// orphaned: the verdict word is the most escalated, not the last one tested.
func TestHealthLeaseVerdictFollowsThePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   gpulease.Standing
		want string
	}{
		{"stalled and overdue", gpulease.Standing{Stalled: true, Overdue: true}, "held-stalled"},
		{"orphaned and overdue", gpulease.Standing{Orphaned: true, Overdue: true}, "held-orphaned"},
		{"stalled and orphaned", gpulease.Standing{Stalled: true, Orphaned: true}, "held-stalled"},
		{"overdue only", gpulease.Standing{Overdue: true}, "held-overdue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := liveLease(1, gpulease.ClassMedia, []string{leaseIDOf(testUUIDa)}, 3*time.Hour)
			entries := leaseEntries(t, rawHealth(t, leasedNode(t, info, func(gpulease.Info) gpulease.Standing { return tc.st }, nil)))
			if len(entries) != 1 || entries[0]["verdict"] != tc.want {
				t.Fatalf("verdict = %v, want %s", entries, tc.want)
			}
		})
	}
}

// A lease that declares no end publishes none: an empty or 1970 date would read as overdue
// to a reader that trusts it.
func TestHealthLeaseWithNoDeclaredEndPublishesNoUntil(t *testing.T) {
	open := liveLease(1, gpulease.ClassMedia, []string{leaseIDOf(testUUIDa)}, 0)
	open.ExpiresAt = time.Time{}
	entries := leaseEntries(t, rawHealth(t, leasedNode(t, open, nil, nil)))
	if len(entries) != 1 {
		t.Fatalf("leases = %v", entries)
	}
	if _, present := entries[0]["until"]; present {
		t.Fatalf("a lease with no declared end must publish no until: %v", entries[0])
	}
	if entries[0]["overdue"] == true || entries[0]["busy"] == true {
		t.Fatalf("nothing to be overdue against: %v", entries[0])
	}
}

// With one lease the singular block is exactly what it was, and the one entry says what the
// block cannot: which cards.
func TestHealthSingleLeaseKeepsTheSingularBlockAndAddsOneEntry(t *testing.T) {
	info := liveLease(4, gpulease.ClassMedia, []string{leaseIDOf(testUUIDb)}, 3*time.Hour)
	m := rawHealth(t, leasedNode(t, info, nil, nil))

	want, _ := json.Marshal(leaseHealthOf(info, time.Now(), 0))
	got, _ := json.Marshal(m["lease"])
	var w, g map[string]any
	_ = json.Unmarshal(want, &w)
	_ = json.Unmarshal(got, &g)
	for k, wv := range w {
		if k == "remaining_sec" {
			continue // moves with the clock
		}
		if g[k] != wv {
			t.Fatalf("lease.%s = %v, want %v (the singular block of one lease must not change)", k, g[k], wv)
		}
	}
	if len(g) != len(w) {
		t.Fatalf("lease block keys = %v, want exactly %v", g, w)
	}
	entries := leaseEntries(t, m)
	if len(entries) != 1 || len(strList(entries[0]["devices"])) != 1 {
		t.Fatalf("leases = %v, want one entry naming its card", entries)
	}
}

// An unreserved node publishes neither key, so a payload from before is byte for byte what it
// was.
func TestHealthNoLeaseNoLeasesKey(t *testing.T) {
	m := rawHealth(t, leasedNode(t, gpulease.Info{}, nil, nil))
	if _, ok := m["leases"]; ok {
		t.Fatalf("an unreserved node must publish no leases key: %v", m["leases"])
	}
	if _, ok := m["lease"]; ok {
		t.Fatalf("an unreserved node must publish no lease key: %v", m["lease"])
	}
}

// A lease says where its cards came from. A whole-node lease names no cards at all (the
// reader must take that as every card), a declared one lists them, and a legacy lease the
// harness scoped by evidence lists the inferred set and says so.
func TestHealthLeaseEntriesSayWhereTheirCardsCameFrom(t *testing.T) {
	declared := liveLease(1, gpulease.ClassMedia, []string{leaseIDOf(testUUIDa)}, 3*time.Hour)
	inferred := liveLease(2, gpulease.ClassMedia, nil, 3*time.Hour)
	inferred.Inferred = []string{leaseIDOf(testUUIDb)}
	inferred.Scope = gpulease.ScopeInferred
	whole := liveLease(3, gpulease.ClassMedia, nil, 3*time.Hour)

	entries := leaseEntries(t, rawHealth(t, leasedNode(t, severalLeases(declared, inferred, whole), nil, nil)))
	if len(entries) != 3 {
		t.Fatalf("leases = %v", entries)
	}
	if entries[0]["scope"] != "declared" || len(strList(entries[0]["devices"])) != 1 {
		t.Fatalf("declared lease = %v", entries[0])
	}
	if entries[1]["scope"] != "inferred" || len(strList(entries[1]["devices"])) != 1 || strList(entries[1]["devices"])[0] != leaseIDOf(testUUIDb) {
		t.Fatalf("inferred lease = %v, want the inferred card as its devices, labelled", entries[1])
	}
	if entries[2]["scope"] != "whole-node" {
		t.Fatalf("whole-node lease = %v", entries[2])
	}
	if _, present := entries[2]["devices"]; present {
		t.Fatalf("a whole-node lease names no cards, and an empty list must not be published as if it did: %v", entries[2])
	}
}

// What each lease is DOING rides on its own entry, and the singular block keeps the OR of
// them, so a stalled lease never hides behind a healthy sibling in either reading.
func TestHealthLeaseEntriesCarryTheirOwnStanding(t *testing.T) {
	a := liveLease(1, gpulease.ClassMedia, []string{leaseIDOf(testUUIDa)}, 3*time.Hour)
	b := liveLease(2, gpulease.ClassMedia, []string{leaseIDOf(testUUIDc)}, 3*time.Hour)
	var asked []uint64
	standing := func(i gpulease.Info) gpulease.Standing {
		asked = append(asked, i.Epoch)
		if i.Epoch == 2 {
			return gpulease.Standing{Stalled: true}
		}
		return gpulease.Standing{}
	}
	m := rawHealth(t, leasedNode(t, severalLeases(a, b), standing, nil))

	entries := leaseEntries(t, m)
	if len(entries) != 2 {
		t.Fatalf("leases = %v", entries)
	}
	if entries[0]["stalled"] == true || entries[0]["verdict"] != "held" {
		t.Fatalf("the healthy lease must read held and not stalled: %v", entries[0])
	}
	if entries[1]["stalled"] != true || entries[1]["verdict"] != "held-stalled" {
		t.Fatalf("the stalled lease must say so on its own entry: %v", entries[1])
	}
	block, _ := m["lease"].(map[string]any)
	if block["stalled"] != true {
		t.Fatalf("the singular block keeps the OR of the standings: %v", block)
	}
	// One standing read per lease: the reader stamps the orphan marker, and health is polled
	// every few seconds.
	if len(asked) != 2 {
		t.Fatalf("standing read for epochs %v, want exactly one read per live lease", asked)
	}
}

// An overdue lease is overdue on its own entry only: its sibling's window has not ended.
func TestHealthLeaseEntryOverdueIsPerLease(t *testing.T) {
	fresh := liveLease(1, gpulease.ClassMedia, []string{leaseIDOf(testUUIDc)}, 3*time.Hour)
	over := liveLease(2, gpulease.ClassMedia, []string{leaseIDOf(testUUIDa)}, -2*time.Hour)
	m := rawHealth(t, leasedNode(t, severalLeases(fresh, over), nil, nil))
	entries := leaseEntries(t, m)
	if len(entries) != 2 {
		t.Fatalf("leases = %v, want both live leases", entries)
	}
	if entries[1]["overdue"] != true || entries[1]["verdict"] != "held-overdue" {
		t.Fatalf("the lease past its window = %v", entries[1])
	}
	if entries[0]["overdue"] == true {
		t.Fatalf("the sibling's window has not ended: %v", entries[0])
	}
	block, _ := m["lease"].(map[string]any)
	if block["overdue"] != true {
		t.Fatalf("the singular block reads overdue if any lease is: %v", block)
	}
}

func satOf(t *testing.T, m map[string]any) (high, idle bool) {
	t.Helper()
	sat, _ := m["saturation"].(map[string]any)
	if sat == nil {
		t.Fatalf("no saturation block: %v", m)
	}
	high, _ = sat["high"].(bool)
	idle, _ = sat["idle_slot"].(bool)
	return high, idle
}

// A long render on one card of three must not close the node: it says "a new dispatch is
// refused right now" only when nothing it could run on is free.
func TestSaturationStaysOpenWhileALeaseLeavesACardFree(t *testing.T) {
	long := liveLease(1, gpulease.ClassMedia, []string{leaseIDOf(testUUIDc)}, 6*time.Hour)
	high, idle := satOf(t, rawHealth(t, leasedNode(t, long, nil, nil)))
	if high || !idle {
		t.Fatalf("saturation high=%v idle_slot=%v with a lease on one of three cards, want an open node", high, idle)
	}
}

// Leases that together cover every card close the node, as one whole-node lease always did.
func TestSaturationClosesWhenLeasesCoverEveryCard(t *testing.T) {
	var ls []gpulease.Info
	for n, u := range []string{testUUIDa, testUUIDb, testUUIDc} {
		ls = append(ls, liveLease(uint64(n+1), gpulease.ClassMedia, []string{leaseIDOf(u)}, 6*time.Hour))
	}
	high, idle := satOf(t, rawHealth(t, leasedNode(t, severalLeases(ls...), nil, nil)))
	if !high || idle {
		t.Fatalf("saturation high=%v idle_slot=%v with every card leased, want a closed node", high, idle)
	}
}

// A whole-node lease, and a card-scoped lease on a node that cannot enumerate its cards, are
// both read as every card: the doubt is "closed".
func TestSaturationClosesOnAWholeNodeLeaseAndOnUnknownCards(t *testing.T) {
	whole := liveLease(1, gpulease.ClassMedia, nil, 6*time.Hour)
	if high, _ := satOf(t, rawHealth(t, leasedNode(t, whole, nil, nil))); !high {
		t.Fatal("a whole-node long lease must keep closing the node")
	}
	scoped := liveLease(2, gpulease.ClassMedia, []string{leaseIDOf(testUUIDc)}, 6*time.Hour)
	noCards := func() (Snapshot, bool) { return Snapshot{TotalGiB: 16, FreeGiB: 12.5, At: time.Now()}, true }
	if high, _ := satOf(t, rawHealth(t, leasedNode(t, scoped, nil, noCards))); !high {
		t.Fatal("a card-scoped lease on a node that publishes no card table cannot be shown to leave a card free: it must close the node")
	}
}

// flagshipNodeCfg is the flagship box as a fleet node: the triple seat (cards 0,1,2) is the
// home agent seat, and the single layer's agent seat sits on card 0.
func flagshipNodeCfg(endpoint string) config.Config {
	cfg := agentHealthCfg(endpoint)
	fx := config.FlagshipFixture()
	cfg.TierProfile, cfg.Tiers, cfg.Layers = fx.TierProfile, fx.Tiers, fx.Layers
	cfg.AgentModel = "agent-pool"
	return cfg
}

func armThreeCards(t *testing.T) {
	t.Helper()
	snap, _ := threeCardSnapshot()
	t.Cleanup(modelaffinity.SetCardTableReader(func(context.Context, string) ([]gpuprobe.Card, string, error) {
		cards, warn := gpuprobe.BuildCards(snap.Devices, "")
		return cards, warn, nil
	}))
}

// A node publishes each seat's pin as the lease ids of its cards, resolved against its own
// card table, so a delegator comparing a lease with a seat never guesses an index space.
func TestHealthSeatRowsCarryTheirCardsAsLeaseIDs(t *testing.T) {
	var probes atomic.Int64
	roster := multiRoster(t, &probes, map[string][]string{"agent-pool": {"agent-pool"}, "gemma-4-26b-agent": nil})
	opts := authOpts(true)
	opts.Snapshot = threeCardSnapshot
	s, _ := newTestServer(t, flagshipNodeCfg(roster.URL), &fakeRunner{}, opts)
	_ = do(t, s, http.MethodGet, "/fleet/health", "", nil)
	waitForResidencyProbe(t, s)
	m := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
	rows, _ := m["layers"].([]any)
	got := map[string][]string{}
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		for _, rs := range row["seats"].([]any) {
			seat, _ := rs.(map[string]any)
			got[row["name"].(string)+"/"+seat["role"].(string)] = strList(seat["device_ids"])
		}
	}
	if want := []string{leaseIDOf(testUUIDa), leaseIDOf(testUUIDb), leaseIDOf(testUUIDc)}; !equalStrings(got["triple/agent"], want) {
		t.Fatalf("triple agent seat device_ids = %v, want %v", got["triple/agent"], want)
	}
	if want := []string{leaseIDOf(testUUIDa)}; !equalStrings(got["single/agent"], want) {
		t.Fatalf("single agent seat device_ids = %v, want %v", got["single/agent"], want)
	}
	if want := []string{leaseIDOf(testUUIDc)}; !equalStrings(got["single/ocr"], want) {
		t.Fatalf("single ocr seat (pin 2) device_ids = %v, want %v", got["single/ocr"], want)
	}
}

// dispatchContract posts one agent dispatch (optionally naming a layer) to a node that
// carries leases.
func dispatchContract(t *testing.T, s *Server, id, layer string) *httptest.ResponseRecorder {
	t.Helper()
	extra := ""
	if layer != "" {
		extra = `,"layer":"` + layer + `"`
	}
	body := `{"job_id":"` + id + `","task_type":"agent","payload":` +
		`{"schema_version":1,"goal":"summarize","output_schema":{"properties":{"answer":{"type":"string"}}}` + extra + `}}`
	return do(t, s, http.MethodPost, "/fleet/dispatch", body, nil)
}

func leasedFlagshipNode(t *testing.T, info gpulease.Info) *Server {
	t.Helper()
	armThreeCards(t)
	roster := multiRoster(t, nil, map[string][]string{"agent-pool": {"agent-pool"}, "gemma-4-26b-agent": nil})
	opts := authOpts(true)
	opts.Snapshot = threeCardSnapshot
	opts.Lease = func() gpulease.Info { return info }
	s, _ := newTestServer(t, flagshipNodeCfg(roster.URL), &fakeRunner{}, opts)
	return s
}

// A text reservation on card 2 does not turn away a contract whose seat is on card 0: the
// placement table falls back to the single layer's seat, which the reservation does not
// hold. One on card 0 holds the triple seat AND the single seat, so it does.
func TestDispatchRefusesATextLeaseOnlyWhereItHoldsEverySeatOfTheContract(t *testing.T) {
	onCard2 := liveLease(3, gpulease.ClassText, []string{leaseIDOf(testUUIDc)}, time.Hour)
	if rec := dispatchContract(t, leasedFlagshipNode(t, onCard2), "j-free-seat", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("a text reservation on card 2 left the single layer's card-0 seat free: dispatch = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	if rec := dispatchContract(t, leasedFlagshipNode(t, onCard2), "j-named", "single"); rec.Code != http.StatusAccepted {
		t.Fatalf("a contract naming the card-0 layer is not held by a card-2 reservation: dispatch = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	if rec := dispatchContract(t, leasedFlagshipNode(t, onCard2), "j-held-layer", "triple"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a contract naming the triple layer sits on card 2 and must be refused: dispatch = %d (%s)", rec.Code, rec.Body.String())
	}
	onCard0 := liveLease(4, gpulease.ClassText, []string{leaseIDOf(testUUIDa)}, time.Hour)
	rec := dispatchContract(t, leasedFlagshipNode(t, onCard0), "j-held", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a text reservation on card 0 holds every seat of the chain: dispatch = %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "node leased") || !strings.Contains(rec.Body.String(), "class=text") {
		t.Fatalf("the refusal still names the holder: %s", rec.Body.String())
	}
}

// Every live text lease counts, not only the lowest epoch, and a media lease below it never
// refuses a dispatch.
func TestDispatchSeesATextLeaseThatIsNotTheLowestEpoch(t *testing.T) {
	media := liveLease(2, gpulease.ClassMedia, []string{leaseIDOf(testUUIDc)}, 6*time.Hour)
	text := liveLease(5, gpulease.ClassText, []string{leaseIDOf(testUUIDa)}, time.Hour)
	if rec := dispatchContract(t, leasedFlagshipNode(t, severalLeases(media, text)), "j-behind-media", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("the text lease behind a media lease must still refuse: dispatch = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := dispatchContract(t, leasedFlagshipNode(t, media), "j-media-only", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("a media lease on card 2 never refuses a dispatch the single seat can take: %d (%s)", rec.Code, rec.Body.String())
	}
	// A media lease on card 0 holds every seat of the chain, and still only a text reservation
	// refuses at dispatch: the pipeline's own fence pre-check defers what a render holds.
	holdsEverything := liveLease(6, gpulease.ClassMedia, []string{leaseIDOf(testUUIDa)}, 6*time.Hour)
	if rec := dispatchContract(t, leasedFlagshipNode(t, holdsEverything), "j-media-holds-chain", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("a media lease is not a text reservation and never refuses a dispatch here: %d (%s)", rec.Code, rec.Body.String())
	}
}

// A task whose cards are chosen later (an image render) keeps the whole-node reading: a text
// reservation on any card refuses it, as it always did.
func TestDispatchOfANonAgentTaskKeepsTheWholeNodeTextRefusal(t *testing.T) {
	text := liveLease(5, gpulease.ClassText, []string{leaseIDOf(testUUIDa)}, time.Hour)
	opts := leasedOpts(text)
	opts.Snapshot = threeCardSnapshot
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, opts)
	if rec := dispatchImage(t, s, "j-image"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("an image job under a card-scoped text reservation = %d, want 503 (its card is chosen later: unknown is every card)", rec.Code)
	}
}
