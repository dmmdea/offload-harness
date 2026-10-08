package mcpserver

// pin_reason on agent_delegate and offload_research (ADR 0078).
//
// route local and route remote are a PIN only with a pin_reason from the closed set (privacy, locality, measurement,
// operator); without one they are hints that placement may override. These tests pin the doors: the schema offers the
// closed set, a bad value or a reason on a route that is not a pin is refused before anything is read or fetched, a
// reason reaches the engine and comes back on the result, and offload_status publishes the server's accounting.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/research"
)

// pinServer is delegateTestServer with no local endpoint: a hinted route is placed as auto, which reads the local
// seat's load through llama-swap, and a test must not depend on whatever listens on this machine's default port.
func pinServer(t *testing.T, local func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error), roster ...string) *Server {
	t.Helper()
	home := t.TempDir()
	cfg := config.Default()
	cfg.Home = home
	cfg.Endpoint = ""
	cfg.LedgerPath = filepath.Join(home, "ledger.jsonl")
	cfg.AgentDelegationEnabled = true
	cfg.DelegateRemotes = roster
	s := New(pipeline.New(cfg, nil, nil, nil))
	s.localAgent = local
	return s
}

func passingSeat(calls *int) func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
	return func(_ context.Context, _ core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		*calls++
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "fake-seat",
			Output: "done", Structured: json.RawMessage(`{"answer":"42"}`), StopReason: "done"}, nil
	}
}

// schemaProp is one property of a published input schema: its enum (strings or numbers, as the schema has them) and
// its description.
type schemaProp struct {
	Enum        []any  `json:"enum"`
	Description string `json:"description"`
}

// pinSchemas returns, per tool, the input schema's properties as published.
func pinSchemas(t *testing.T) map[string]map[string]schemaProp {
	t.Helper()
	cfg := config.Default()
	cfg.AgentDelegationEnabled = true
	out := map[string]map[string]schemaProp{}
	for _, tool := range listTools(t, cfg) {
		if tool.Name != "agent_delegate" && tool.Name != "offload_research" {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]schemaProp `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s schema not JSON: %v", tool.Name, err)
		}
		out[tool.Name] = schema.Properties
	}
	return out
}

// TestBothDelegationToolsOfferPinReasonAsTheClosedSet: the enum in the schema is the engine's closed set, in its
// order, and the route text no longer says local is a force: it says a pin needs a reason and what a hint is.
func TestBothDelegationToolsOfferPinReasonAsTheClosedSet(t *testing.T) {
	schemas := pinSchemas(t)
	for _, tool := range []string{"agent_delegate", "offload_research"} {
		props := schemas[tool]
		if props == nil {
			t.Fatalf("%s is not advertised", tool)
		}
		pin, ok := props["pin_reason"]
		if !ok {
			t.Fatalf("%s has no pin_reason property", tool)
		}
		var enum []string
		for _, v := range pin.Enum {
			enum = append(enum, fmt.Sprint(v))
		}
		if got, want := strings.Join(enum, ","), strings.Join(delegate.PinReasons(), ","); got != want {
			t.Errorf("%s pin_reason enum = %s, want the engine's closed set %s", tool, got, want)
		}
		for _, want := range []string{"privacy", "locality", "measurement", "operator", "route local only", "route local or remote", "Only a pin can have a reason"} {
			if !strings.Contains(pin.Description, want) {
				t.Errorf("%s pin_reason description does not say %q", tool, want)
			}
		}
		route := props["route"].Description
		for _, want := range []string{"pin_reason", "hint", "honoured or overridden"} {
			if !strings.Contains(route, want) {
				t.Errorf("%s route description does not say %q", tool, want)
			}
		}
		if strings.Contains(route, "force in-process") || strings.Contains(route, "force a fleet node") {
			t.Errorf("%s route description still calls local and remote a force: %q", tool, route)
		}
	}
}

// TestTheDelegationToolsSayWhereAPinRunsRetryIncluded: a pin is authoritative to the end of the subtask (R4e review F1), and
// the text a calling model reads says so for the second chance too. A local pin is never retried on another node, and a remote
// pin's failed-verification retry goes to another fleet node or is skipped and never lands on the local seat.
func TestTheDelegationToolsSayWhereAPinRunsRetryIncluded(t *testing.T) {
	schemas := pinSchemas(t)
	route := schemas["agent_delegate"]["route"].Description
	for _, want := range []string{"local runs on this box's seat and nowhere else, never retried on another node", "remote runs on a fleet node and nowhere else", "never lands on the local seat"} {
		if !strings.Contains(route, want) {
			t.Errorf("agent_delegate route description does not say %q", want)
		}
	}
	for _, tool := range []string{"agent_delegate", "offload_research"} {
		if pin := schemas[tool]["pin_reason"].Description; !strings.Contains(pin, "authoritative to the end of the subtask") || !strings.Contains(pin, "a remote pin's retry stays on a fleet node") {
			t.Errorf("%s pin_reason description does not say a pin is authoritative to the end of the subtask: %q", tool, pin)
		}
	}
}

// TestTheVisionToolsSayTheirAutoRouteAlsoReadsTheVisionSeat: the auto route left the box only while the machine-wide GPU lease
// was held until ADR 0078 decision 9; it also goes to a node when the vision seat the call would run on is busy. The route
// text a model reads has to name that trigger, or a caller who trusts "only while the lease is held" is surprised by an image
// that left the box with no lease anywhere.
func TestTheVisionToolsSayTheirAutoRouteAlsoReadsTheVisionSeat(t *testing.T) {
	seen := 0
	for _, tool := range listTools(t, config.Default()) {
		if tool.Name != "offload_vqa" && tool.Name != "offload_assess_image" && tool.Name != "offload_ocr" {
			continue
		}
		seen++
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]schemaProp `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s schema not JSON: %v", tool.Name, err)
		}
		route := schema.Properties["route"].Description
		for _, want := range []string{"machine-wide GPU lease is held", "the local vision seat is busy", "a request in flight", "a load that would unload a loaded vLLM seat"} {
			if !strings.Contains(route, want) {
				t.Errorf("%s route description does not say %q: %q", tool.Name, want, route)
			}
		}
	}
	if seen != 3 {
		t.Fatalf("saw %d of the 3 single-image vision tools", seen)
	}
}

func TestBothDelegationToolDescriptionsSayAPinNeedsAReason(t *testing.T) {
	cfg := config.Default()
	cfg.AgentDelegationEnabled = true
	seen := 0
	for _, tool := range listTools(t, cfg) {
		if tool.Name != "agent_delegate" && tool.Name != "offload_research" {
			continue
		}
		seen++
		if !strings.Contains(tool.Description, "pin_reason") || !strings.Contains(tool.Description, "hint placement may override") {
			t.Errorf("%s description does not say a local or remote route pins only with a pin_reason", tool.Name)
		}
		if strings.Contains(tool.Description, "force with local|remote") {
			t.Errorf("%s description still says to force with local|remote", tool.Name)
		}
	}
	if seen != 2 {
		t.Fatalf("saw %d of the 2 delegation tools", seen)
	}
}

// TestAgentDelegateRefusesABadPinReasonBeforeReadingAnyContext: the refusal comes first. The subtask names a
// context file that does not exist; had the door prepared contracts before checking the reason, the answer would be
// about the file. Nothing runs, and the reason lists the valid set.
func TestAgentDelegateRefusesABadPinReasonBeforeReadingAnyContext(t *testing.T) {
	var calls int
	s := pinServer(t, passingSeat(&calls))
	sub := `[{"goal":"g","context_paths":["no-such-file.txt"]}]`
	for _, tc := range []struct{ name, args string }{
		{"a value outside the set", `{"subtasks":` + sub + `,"route":"local","pin_reason":"because"}`},
		{"privacy with remote", `{"subtasks":` + sub + `,"route":"remote","pin_reason":"privacy"}`},
		{"locality with remote", `{"subtasks":` + sub + `,"route":"remote","pin_reason":"locality"}`},
		{"a reason with auto", `{"subtasks":` + sub + `,"route":"auto","pin_reason":"operator"}`},
		{"a reason with no route", `{"subtasks":` + sub + `,"pin_reason":"operator"}`},
		{"a reason with spread", `{"subtasks":` + sub + `,"route":"spread","pin_reason":"measurement"}`},
		{"a reason with queue", `{"subtasks":` + sub + `,"route":"queue","pin_reason":"measurement"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := s.handleAgentDelegate(context.Background(), callReq(tc.args))
			if err != nil {
				t.Fatalf("MCP error (want a deferred-shape result): %v", err)
			}
			m := decodeResult(t, res)
			reason, _ := m["reason"].(string)
			if m["deferred"] != true || !strings.Contains(reason, "pin_reason") || !strings.Contains(reason, "operator (route local or remote)") {
				t.Fatalf("result = %v, want a deferred refusal that names pin_reason and lists the valid set", m)
			}
			if strings.Contains(reason, "no-such-file") {
				t.Errorf("reason = %q is about the context file: the pin_reason must be refused before any context is read", reason)
			}
		})
	}
	if calls != 0 {
		t.Errorf("the local seat ran %d time(s) for refused calls", calls)
	}
	if st := s.pins.Snapshot(); st.Hints != 0 || st.Unreasoned != 0 {
		t.Errorf("tally %+v: a refused call counted", st)
	}
}

// TestOffloadResearchRefusesABadPinReasonBeforeFetchingAnyPage: twelve fetches are the expensive part of a research
// call, and a refusal that came after them would have spent them. A reason with no route is a reason on spread,
// research's default, and is refused the same way.
func TestOffloadResearchRefusesABadPinReasonBeforeFetchingAnyPage(t *testing.T) {
	s := researchServer(t, nil)
	fetches := 0
	s.researchFetch = func(_ context.Context, urls []string, _ research.Options) []research.Fetched {
		fetches++
		return nil
	}
	base := `"goal":"digest the page","urls":["https://docs.example/a"]`
	for _, tc := range []struct{ name, extra string }{
		{"a value outside the set", `,"route":"local","pin_reason":"because"`},
		{"privacy with remote", `,"route":"remote","pin_reason":"privacy"`},
		{"a reason with auto", `,"route":"auto","pin_reason":"operator"`},
		{"a reason with spread", `,"route":"spread","pin_reason":"operator"`},
		{"a reason with no route (research's default is spread)", `,"pin_reason":"operator"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := s.handleResearch(context.Background(), callReq(`{`+base+tc.extra+`}`))
			if err != nil {
				t.Fatalf("MCP error (want a deferred-shape result): %v", err)
			}
			m := decodeResult(t, res)
			reason, _ := m["reason"].(string)
			if m["deferred"] != true || !strings.Contains(reason, "pin_reason") || !strings.Contains(reason, "measurement (route local or remote)") {
				t.Fatalf("result = %v, want a deferred refusal that names pin_reason and lists the valid set", m)
			}
		})
	}
	if fetches != 0 {
		t.Fatalf("the door fetched pages %d time(s) for calls refused at intake", fetches)
	}
}

// TestAgentDelegateRecordsTheReasonOfAPinAndSaysWhatBecameOfAHint: through the real handler, a reasoned local pin is
// authoritative and its result carries the reason; the same route without one is a hint, placed as auto, and its
// result says so. The server's tally counts both.
func TestAgentDelegateRecordsTheReasonOfAPinAndSaysWhatBecameOfAHint(t *testing.T) {
	var calls int
	s := pinServer(t, passingSeat(&calls))
	call := func(extra string) map[string]any {
		t.Helper()
		res, err := s.handleAgentDelegate(context.Background(), callReq(`{"subtasks":[{"goal":"answer it"}],"route":"local"`+extra+`}`))
		if err != nil {
			t.Fatal(err)
		}
		m := decodeResult(t, res)
		results, _ := m["results"].([]any)
		if len(results) != 1 {
			t.Fatalf("results = %v, want one", m)
		}
		return results[0].(map[string]any)
	}

	pinned := call(`,"pin_reason":"privacy"`)
	if pinned["pin_reason"] != "privacy" || pinned["placement"] != "route=local forced" {
		t.Errorf("pinned result = %v, want pin_reason privacy and the unchanged forced placement", pinned)
	}
	hinted := call(``)
	if _, has := hinted["pin_reason"]; has {
		t.Errorf("hinted result = %v, want no pin_reason on a hint", hinted)
	}
	if placement, _ := hinted["placement"].(string); !strings.HasPrefix(placement, "route=local was a hint (no pin_reason), honoured: placed on the local seat") {
		t.Errorf("hinted placement = %q, want it to say the hint was honoured", placement)
	}
	if calls != 2 {
		t.Errorf("the local seat ran %d times, want 2", calls)
	}
	if st := s.pins.Snapshot(); st.Reasoned["privacy"] != 1 || st.Hints != 1 || st.HintsOverridden != 0 || st.Unreasoned != 0 {
		t.Errorf("tally %+v, want 1 subtask pinned under privacy and 1 hint, none overridden", st)
	}
}

// TestAgentDelegateOptionsOfferPinReasonAndCarryTheServersTally: the door's half of the contract, pinned in one place
// as the roster guard is (roster_remotes_test.go): without PinNeedsReason a bare route would stay a hard pin.
func TestAgentDelegateOptionsOfferPinReasonAndCarryTheServersTally(t *testing.T) {
	s := pinServer(t, nil)
	opts := s.agentDelegateOptions(1, "measurement", time.Time{}, nil)
	if !opts.PinNeedsReason || opts.PinTally == nil || opts.PinTally != s.pins || !opts.RosterOnly || opts.PinReason != "measurement" {
		t.Fatalf("options = %+v, want PinNeedsReason, this server's tally, RosterOnly and the caller's reason", opts)
	}
	if bare := s.agentDelegateOptions(1, "", time.Time{}, nil); bare.PinReason != "" || !bare.PinNeedsReason {
		t.Fatalf("options = %+v, want no reason but still the door's opt-in: a bare route is a hint here", bare)
	}
}

// TestOffloadStatusPublishesThePinAccountingOfThisServer: the fleet block carries a small pins object whose window
// is labelled "since this server started". It counts the server's own calls and nothing is scanned from the ledger.
func TestOffloadStatusPublishesThePinAccountingOfThisServer(t *testing.T) {
	s, _, _ := statusFixture(t)
	pins := func() map[string]any {
		t.Helper()
		fleet, _ := decodeObject(t, statusCall(t, s, json.RawMessage(`{"section":"fleet"}`)))["fleet"].(map[string]any)
		p, _ := fleet["pins"].(map[string]any)
		if p == nil {
			t.Fatalf("fleet block = %v, want a pins object", fleet)
		}
		return p
	}
	p := pins()
	if p["window"] != "since this server started" || p["unit"] != "subtasks" {
		t.Errorf("pins window/unit = %v / %v, want the honest label and the unit", p["window"], p["unit"])
	}
	if _, err := time.Parse(time.RFC3339, p["started_at"].(string)); err != nil {
		t.Errorf("started_at = %v, want RFC 3339: %v", p["started_at"], err)
	}
	reasoned, _ := p["reasoned"].(map[string]any)
	for _, reason := range delegate.PinReasons() {
		if reasoned[reason] != float64(0) {
			t.Errorf("reasoned[%s] = %v, want 0 on a fresh server", reason, reasoned[reason])
		}
	}
	if p["hints"] != float64(0) || p["hints_overridden"] != float64(0) || p["unreasoned"] != float64(0) {
		t.Errorf("pins = %v, want zero counts on a fresh server", p)
	}

	// a pinned call and a hinted call through the real handler change the published counts, and only by what ran
	calls := 0
	s.localAgent = passingSeat(&calls)
	for _, args := range []string{
		`{"subtasks":[{"goal":"a"},{"goal":"b"}],"route":"local","pin_reason":"measurement"}`,
		`{"subtasks":[{"goal":"c"}],"route":"local"}`,
	} {
		if _, err := s.handleAgentDelegate(context.Background(), callReq(args)); err != nil {
			t.Fatal(err)
		}
	}
	p = pins()
	reasoned, _ = p["reasoned"].(map[string]any)
	if reasoned["measurement"] != float64(2) || p["hints"] != float64(1) || p["hints_overridden"] != float64(0) {
		t.Errorf("pins = %v, want 2 subtasks pinned under measurement and 1 hint kept", p)
	}
}

// TestOffloadStatusHasNoPinsBlockWhenDelegationIsOff: there is no door to pin through, so nothing is published and
// the fleet block of a box that does not delegate is as it was.
func TestOffloadStatusHasNoPinsBlockWhenDelegationIsOff(t *testing.T) {
	cfg := config.Default() // agent_delegation_enabled defaults false
	s := New(pipeline.New(cfg, nil, nil, nil))
	fleet, _ := decodeObject(t, statusCall(t, s, json.RawMessage(`{"section":"fleet"}`)))["fleet"].(map[string]any)
	if fleet == nil {
		t.Fatal("no fleet block")
	}
	if _, has := fleet["pins"]; has {
		t.Errorf("fleet block = %v, want no pins when delegation is off", fleet)
	}
}

// TestOffloadResearchCarriesThePinReasonToEveryPageAndHintsTheRest: the research door hands the engine the same pair
// as agent_delegate. A reasoned local pin stays authoritative and every page's result carries the reason; the same
// route without one is a hint placed as auto, and each page says it was honoured.
func TestOffloadResearchCarriesThePinReasonToEveryPageAndHintsTheRest(t *testing.T) {
	seat := func(_ context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		name := ""
		if len(c.Context) > 0 {
			name = c.Context[0].Name
		}
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "fake-seat",
			Output: "digest of " + name, Structured: json.RawMessage(`{"summary":"digest of ` + name + `"}`), StopReason: "done"}, nil
	}
	s := pinServer(t, seat)
	s.researchFetch = func(_ context.Context, urls []string, _ research.Options) []research.Fetched {
		out := make([]research.Fetched, len(urls))
		for i, u := range urls {
			out[i] = research.Fetched{URL: u, Status: 200, Title: "Page", Text: "A short page about widgets.", Bytes: 28, TextBytes: 28}
		}
		return out
	}
	pages := func(extra string) []any {
		t.Helper()
		args := strings.Replace(researchArgs, `"route":"local"`, `"route":"local"`+extra, 1)
		res, err := s.handleResearch(context.Background(), callReq(args))
		if err != nil {
			t.Fatal(err)
		}
		results, _ := decodeResult(t, res)["results"].([]any)
		if len(results) != 3 {
			t.Fatalf("results = %v, want one per page", results)
		}
		return results
	}
	for i, r := range pages(`,"pin_reason":"locality"`) {
		row := r.(map[string]any)
		if row["pin_reason"] != "locality" || row["placement"] != "route=local forced" {
			t.Errorf("pinned page %d = pin_reason %v, placement %v, want locality and the unchanged forced placement", i, row["pin_reason"], row["placement"])
		}
	}
	for i, r := range pages(``) {
		row := r.(map[string]any)
		if _, has := row["pin_reason"]; has {
			t.Errorf("hinted page %d carries pin_reason %v", i, row["pin_reason"])
		}
		if placement, _ := row["placement"].(string); !strings.HasPrefix(placement, "route=local was a hint (no pin_reason), honoured: placed on the local seat") {
			t.Errorf("hinted page %d placement = %q, want it to say the hint was honoured", i, placement)
		}
	}
	if st := s.pins.Snapshot(); st.Reasoned["locality"] != 3 || st.Hints != 3 || st.HintsOverridden != 0 {
		t.Errorf("tally %+v, want 3 pages pinned under locality and 3 hinted pages, none overridden", st)
	}
}

// TestTheFleetDispatchOfAskAndReviewStaysAPinAndIsCounted: offload_ask, agent_run and the review lane's remote fallthrough
// hand the engine a bare route and have no way to give a reason. The review lane in particular uses route=remote so that
// nothing runs on the fenced local seat, which a hint would not promise. They keep the authoritative pin (no opt-in, no
// reason) and carry this server's tally, so offload_status counts them as unreasoned.
func TestTheFleetDispatchOfAskAndReviewStaysAPinAndIsCounted(t *testing.T) {
	s := pinServer(t, nil)
	var got *delegate.RunOptions
	var gotRoute string
	s.reviewFleet = func(_ context.Context, _ config.Config, _ delegate.LocalRunner, _ []core.AgentContract, route string, _ []string, opts *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error) {
		got, gotRoute = opts, route
		return nil, delegate.Summary{}, fmt.Errorf("the seam dispatches nothing")
	}
	if _, _, note, ok := s.reviewOnFleet(context.Background(), core.AgentContract{Goal: "g"}, "a lease fences the seat"); ok || !strings.Contains(note, "the seam dispatches nothing") {
		t.Fatalf("reviewOnFleet = ok %v, note %q, want the seam's refusal", ok, note)
	}
	if got == nil || gotRoute != "remote" || got.PinNeedsReason || got.PinReason != "" || got.PinTally != s.pins {
		t.Fatalf("route %q, options %+v, want route remote, no opt-in, no reason and this server's tally", gotRoute, got)
	}
}
