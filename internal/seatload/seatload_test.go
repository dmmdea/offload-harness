package seatload

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSwap is a llama-swap stand-in: /v1/models serves a roster (an id with
// aliases), /running lists the seat by CANONICAL id when loaded, and
// each /running entry carries the seat's own address (`proxy`, as llama-swap
// reports it), where /metrics serves a vLLM-shaped exposition with the
// in-flight count the test controls. It records whether the seat was ever
// asked while NOT loaded, and every /upstream/… request at all: a gauge read
// through /upstream resets llama-swap's idle timer, so a reader must never
// issue one.
type fakeSwap struct {
	id, alias                 string
	roster                    bool // serve /v1/models at all (false = roster unreadable)
	loaded                    atomic.Bool
	starting                  atomic.Bool // loaded AND listed as `starting` (a load in progress)
	stopping                  atomic.Bool // loaded AND listed as `stopping` (an unload in progress)
	inflight                  atomic.Int64
	upstreamHitsWhileUnloaded atomic.Int64
	metricsStatus             atomic.Int64
	metricsHits               atomic.Int64 // every seat /metrics request, loaded or not
	upstreamHits              atomic.Int64 // every /upstream/… request (must stay 0)
	noProxy                   bool         // /running omits the seat's proxy (an old llama-swap)
	proxyHost                 string       // /running reports the seat at this host:port instead of the request's Host
}

func (f *fakeSwap) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !f.roster {
			http.Error(w, "no roster", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"` + f.id + `","meta":{"llamaswap":{"aliases":["` + f.alias + `"]}}},{"id":"other-seat"}]}`))
	})
	mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
		var running []map[string]string
		if f.loaded.Load() {
			state := "ready"
			if f.starting.Load() {
				state = "starting"
			}
			if f.stopping.Load() {
				state = "stopping"
			}
			entry := map[string]string{"model": f.id, "state": state}
			if !f.noProxy {
				host := r.Host
				if f.proxyHost != "" {
					host = f.proxyHost
				}
				entry["proxy"] = "http://" + host + "/direct/" + f.id
			}
			running = append(running, entry)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"running": running})
	})
	metrics := func(w http.ResponseWriter, r *http.Request) {
		f.metricsHits.Add(1)
		if !f.loaded.Load() {
			f.upstreamHitsWhileUnloaded.Add(1)
		}
		if st := f.metricsStatus.Load(); st != 0 {
			w.WriteHeader(int(st))
			return
		}
		n := f.inflight.Load()
		_, _ = w.Write([]byte("# HELP vllm:num_requests_running x\nvllm:num_requests_running{engine=\"0\"} " +
			strconv.FormatInt(n, 10) + "\nvllm:num_requests_waiting{engine=\"0\"} 0.0\nvllm:num_requests_running_total 99\n"))
	}
	slots := func(w http.ResponseWriter, r *http.Request) {
		if !f.loaded.Load() {
			f.upstreamHitsWhileUnloaded.Add(1)
		}
		n := f.inflight.Load()
		out := []map[string]any{}
		for i := 0; i < 2; i++ {
			out = append(out, map[string]any{"id": i, "is_processing": int64(i) < n})
		}
		_ = json.NewEncoder(w).Encode(out)
	}
	mux.HandleFunc("/direct/"+f.id+"/metrics", metrics)
	mux.HandleFunc("/direct/"+f.id+"/slots", slots)
	mux.HandleFunc("/upstream/", func(w http.ResponseWriter, r *http.Request) {
		f.upstreamHits.Add(1)
		http.Error(w, "the reader must not use /upstream", http.StatusTeapot)
	})
	return mux
}

// TestInflightResolvesAnAliasBoundSeat is the regression for the 0.113.16–19
// drain defect: the seat is bound as an ALIAS, /running lists the canonical
// id, and the reader must still see it as loaded and count its requests.
func TestInflightResolvesAnAliasBoundSeat(t *testing.T) {
	f := &fakeSwap{id: "qwen3.8-27b-vllm", alias: "agent-pool", roster: true}
	f.loaded.Store(true)
	f.inflight.Store(3)
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	rd, err := Inflight(context.Background(), srv.Client(), srv.URL, "agent-pool")
	if err != nil {
		t.Fatalf("Inflight: %v", err)
	}
	if !rd.Loaded || rd.Inflight != 3 || rd.Canonical != "qwen3.8-27b-vllm" || rd.Source != "metrics" {
		t.Fatalf("reading = %+v; want loaded, 3 in flight, canonical qwen3.8-27b-vllm via metrics", rd)
	}
}

// TestInflightMatchesTheBareNameWhenTheRosterIsUnreadable: an unreadable
// roster must not turn into a refusal — the reader falls back to the name the
// caller bound (the pre-0.113.20 behaviour), which still works for an id-bound
// seat.
func TestInflightMatchesTheBareNameWhenTheRosterIsUnreadable(t *testing.T) {
	f := &fakeSwap{id: "qwen3.5-4b-vllm", alias: "a2-pool", roster: false}
	f.loaded.Store(true)
	f.inflight.Store(1)
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	rd, err := Inflight(context.Background(), srv.Client(), srv.URL, "qwen3.5-4b-vllm")
	if err != nil {
		t.Fatalf("Inflight: %v", err)
	}
	if !rd.Loaded || rd.Inflight != 1 || rd.Canonical != "" {
		t.Fatalf("reading = %+v; want loaded, 1 in flight, no canonical (roster unreadable)", rd)
	}
	// And the alias-bound case WITHOUT a roster reads as not loaded but
	// AMBIGUOUS (the roster failed and /running holds an entry the reading
	// could not resolve) — the honest limit of the fallback, flagged, not an
	// error: the drain refuses to call it drained, the deal treats it as idle.
	rd, err = Inflight(context.Background(), srv.Client(), srv.URL, "a2-pool")
	if err != nil || rd.Loaded || !rd.Ambiguous || rd.RosterErr == nil || rd.RunningOthers != 1 {
		t.Fatalf("alias without a roster: reading = %+v, err %v; want not loaded, ambiguous, roster error, 1 other running", rd, err)
	}
	// With NOTHING running the same unreadable roster is not ambiguous: an
	// empty /running is idle whatever the names are.
	f.loaded.Store(false)
	rd, err = Inflight(context.Background(), srv.Client(), srv.URL, "a2-pool")
	if err != nil || rd.Loaded || rd.Ambiguous || rd.RunningOthers != 0 {
		t.Fatalf("alias, no roster, nothing running: reading = %+v, err %v; want plainly not loaded", rd, err)
	}
}

func TestInflightNeverTouchesTheUpstreamOfAnUnloadedSeat(t *testing.T) {
	f := &fakeSwap{id: "seat", alias: "seat-alias", roster: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	for _, name := range []string{"seat", "seat-alias"} {
		rd, err := Inflight(context.Background(), srv.Client(), srv.URL, name)
		if err != nil || rd.Loaded || rd.Inflight != 0 {
			t.Fatalf("%s: reading = %+v, err %v; want not loaded", name, rd, err)
		}
	}
	if f.upstreamHitsWhileUnloaded.Load() != 0 {
		t.Fatal("the reader probed /upstream on an unloaded seat — that path LOADS the model")
	}
}

func TestInflightFallsBackToSlotsOnlyOn501Or404(t *testing.T) {
	f := &fakeSwap{id: "seat", alias: "seat-alias", roster: true}
	f.loaded.Store(true)
	f.inflight.Store(1)
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	f.metricsStatus.Store(http.StatusNotImplemented)
	rd, err := Inflight(context.Background(), srv.Client(), srv.URL, "seat-alias")
	if err != nil || rd.Inflight != 1 || rd.Source != "slots" {
		t.Fatalf("501 → /slots: reading = %+v, err %v; want 1 processing via slots", rd, err)
	}
	f.metricsStatus.Store(http.StatusInternalServerError)
	if _, err := Inflight(context.Background(), srv.Client(), srv.URL, "seat-alias"); err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("a 500 must be an error naming the status, never idle; got %v", err)
	}
}

func TestParseInflightSumsOnlyTheInflightGauges(t *testing.T) {
	body := `# HELP
vllm:num_requests_running{engine="0",model_name="m"} 2.0
vllm:num_requests_waiting{engine="0",model_name="m"} 1.0
vllm:num_requests_running_total 500
llamacpp:requests_processing 1
vllm:prompt_tokens_total 12345
`
	if got := ParseInflight(strings.NewReader(body)); got != 4 {
		t.Fatalf("inflight = %d, want 4 (2 running + 1 waiting + 1 processing; the _total counter must not count)", got)
	}
}

func TestParseSlotsInflightCountsProcessingSlots(t *testing.T) {
	n, err := ParseSlotsInflight(strings.NewReader(`[{"id":0,"is_processing":true,"n_ctx":65536},{"id":1,"is_processing":false},{"id":2,"is_processing":true}]`))
	if err != nil || n != 2 {
		t.Fatalf("parse = %d, %v; want 2 processing", n, err)
	}
	if _, err := ParseSlotsInflight(strings.NewReader(`{"error":"x"}`)); err == nil {
		t.Fatal("a non-array body must be an error, never zero in flight")
	}
}

// TestInflightReportsAStartingSeatWithoutTouchingTheUpstream (register D-92):
// llama-swap lists a loading seat as `starting` and holds /upstream/<seat>/…
// until the load completes (4m08s on the 27B, 2026-09-11). The reading must say
// "starting" from /running alone — one blocked upstream read is what timed
// out the H-24 gate's drain and stranded its lease.
func TestInflightReportsAStartingSeatWithoutTouchingTheUpstream(t *testing.T) {
	f := &fakeSwap{id: "qwen3.8-27b-vllm", alias: "agent-pool", roster: true}
	f.loaded.Store(true)
	f.starting.Store(true)
	f.inflight.Store(7) // whatever the upstream would say, it must not be asked
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	hitsBefore := f.metricsHits.Load()
	rd, err := Inflight(context.Background(), srv.Client(), srv.URL, "agent-pool")
	if err != nil {
		t.Fatalf("Inflight: %v", err)
	}
	if !rd.Loaded || !rd.Starting || rd.Inflight != 0 || rd.Source != "running-state:starting" {
		t.Fatalf("reading = %+v; want loaded + starting, no count, source running-state:starting", rd)
	}
	if f.metricsHits.Load() != hitsBefore {
		t.Fatal("the reader asked the upstream of a STARTING seat — llama-swap holds that request for the whole load")
	}
	// Once the seat is ready the same reader asks the upstream as before.
	f.starting.Store(false)
	rd, err = Inflight(context.Background(), srv.Client(), srv.URL, "agent-pool")
	if err != nil || rd.Starting || rd.Inflight != 7 || rd.Source != "metrics" {
		t.Fatalf("after ready: reading = %+v err=%v; want 7 in flight via metrics", rd, err)
	}
}

// A seat llama-swap lists as `stopping` (its ttl ran out) is in transition like
// a starting one, but it is leaving: the reading says so, so nothing reports a
// ttl unload as "loading" (2026-09-23) or owes it a warm-back.
func TestRunningTellsAStoppingSeatFromALoadingOne(t *testing.T) {
	f := &fakeSwap{id: "qwen3.8-27b-vllm", alias: "agent-pool", roster: true}
	f.loaded.Store(true)
	f.stopping.Store(true)
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	rd, err := Running(context.Background(), srv.Client(), srv.URL, "agent-pool")
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	if !rd.Loaded || !rd.Starting || !rd.Stopping || rd.Source != "running-state:stopping" {
		t.Fatalf("reading = %+v; want loaded + in transition + stopping", rd)
	}
	f.stopping.Store(false)
	f.starting.Store(true)
	if rd, err = Running(context.Background(), srv.Client(), srv.URL, "agent-pool"); err != nil || rd.Stopping || !rd.Starting {
		t.Fatalf("a loading seat is not stopping: %+v err=%v", rd, err)
	}
}

// TestRunningReadsTheSeatStateWithoutEverAskingTheUpstream is register C-05 in
// one test: a health path may learn whether the agent seat is LOADED, and may
// not do anything that could load it. Running answers from /running alone —
// alias-resolved, so a seat bound by alias and listed by canonical id is still
// seen — and never issues an /upstream request, not even for a seat that is
// loaded and idle and would answer instantly.
func TestRunningReadsTheSeatStateWithoutEverAskingTheUpstream(t *testing.T) {
	f := &fakeSwap{id: "qwen3.8-27b-vllm", alias: "agent-pool", roster: true}
	f.loaded.Store(true)
	f.inflight.Store(2) // the upstream has an answer; Running must not want it
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	rd, err := Running(context.Background(), srv.Client(), srv.URL, "agent-pool")
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	if !rd.Loaded || rd.Starting || rd.Canonical != "qwen3.8-27b-vllm" {
		t.Fatalf("reading = %+v; want loaded, not starting, canonical resolved from the alias", rd)
	}
	if rd.Inflight != 0 {
		t.Fatalf("Inflight = %d: Running does not count requests, and a caller must not read one out of it", rd.Inflight)
	}

	// A starting seat, and an unloaded one, are both answered the same way.
	f.starting.Store(true)
	if rd, err = Running(context.Background(), srv.Client(), srv.URL, "agent-pool"); err != nil || !rd.Loaded || !rd.Starting {
		t.Fatalf("starting seat: reading = %+v err = %v; want loaded + starting", rd, err)
	}
	f.loaded.Store(false)
	if rd, err = Running(context.Background(), srv.Client(), srv.URL, "agent-pool"); err != nil || rd.Loaded {
		t.Fatalf("unloaded seat: reading = %+v err = %v; want not loaded", rd, err)
	}
	if n := f.metricsHits.Load(); n != 0 {
		t.Fatalf("Running issued %d upstream metrics request(s): probing a seat through /upstream is what LOADS it (C-05)", n)
	}
}

// TestInflightNeverReadsThroughUpstream: the gauge of a LOADED seat is read at
// the seat's own address (/running's `proxy`), never via /upstream/<seat>/…,
// which llama-swap counts as activity — a polled read there keeps an idle seat
// resident past its ttl. Both the metrics path and the /slots fallback.
func TestInflightNeverReadsThroughUpstream(t *testing.T) {
	f := &fakeSwap{id: "qwen3.8-27b-vllm-3card", alias: "agent-pool", roster: true}
	f.loaded.Store(true)
	f.inflight.Store(1)
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	for i := 0; i < 3; i++ {
		rd, err := Inflight(context.Background(), srv.Client(), srv.URL, "agent-pool")
		if err != nil || rd.Inflight != 1 || rd.Source != "metrics" {
			t.Fatalf("read %d: reading = %+v, err %v; want 1 in flight via metrics", i, rd, err)
		}
		if !strings.HasSuffix(rd.Proxy, "/direct/qwen3.8-27b-vllm-3card") {
			t.Fatalf("reading.Proxy = %q, want the /running proxy of the seat", rd.Proxy)
		}
	}
	f.metricsStatus.Store(http.StatusNotImplemented)
	if rd, err := Inflight(context.Background(), srv.Client(), srv.URL, "agent-pool"); err != nil || rd.Source != "slots" {
		t.Fatalf("slots fallback: reading = %+v, err %v", rd, err)
	}
	if f.metricsHits.Load() == 0 {
		t.Fatal("the seat's own /metrics was never read")
	}
	if n := f.upstreamHits.Load(); n != 0 {
		t.Fatalf("Inflight issued %d /upstream request(s): that path resets the seat's idle unload timer", n)
	}
}

// TestInflightWithoutAProxyIsAnErrorNotAnUpstreamRead: a llama-swap that does
// not report the seat's proxy leaves no address to read — "could not read",
// never a fall-back through /upstream and never "idle".
func TestInflightWithoutAProxyIsAnErrorNotAnUpstreamRead(t *testing.T) {
	f := &fakeSwap{id: "seat", alias: "seat-alias", roster: true, noProxy: true}
	f.loaded.Store(true)
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	rd, err := Inflight(context.Background(), srv.Client(), srv.URL, "seat-alias")
	if !errors.Is(err, ErrNoSeatAddress) || !rd.Loaded {
		t.Fatalf("reading = %+v, err %v; want loaded + ErrNoSeatAddress", rd, err)
	}
	if f.upstreamHits.Load() != 0 {
		t.Fatal("a missing proxy fell back to /upstream")
	}
}

// stubThisHost makes endpointIsThisHost answer from a fixed set for one test.
func stubThisHost(t *testing.T, self ...string) {
	t.Helper()
	prev := endpointIsThisHost
	endpointIsThisHost = func(h string) bool {
		for _, s := range self {
			if strings.EqualFold(h, s) {
				return true
			}
		}
		return false
	}
	t.Cleanup(func() { endpointIsThisHost = prev })
}

func TestSeatURLResolvesTheProxyFromLlamaSwapsBox(t *testing.T) {
	stubThisHost(t, "this-box", "this-box.tailnet.example")
	cases := []struct{ endpoint, proxy, want string }{
		// The endpoint names THIS box by hostname / MagicDNS name: the seat binds
		// loopback only, so the loopback proxy is kept (review F1).
		{"http://this-box:11436/v1", "http://127.0.0.1:18797", "http://127.0.0.1:18797"},
		{"http://this-box.tailnet.example:11436", "http://localhost:18797", "http://localhost:18797"},
		{"http://this-box:11436", "http://0.0.0.0:18797", "http://127.0.0.1:18797"},
		{"http://127.0.0.1:11436", "http://127.0.0.1:18797", "http://127.0.0.1:18797"},
		{"http://127.0.0.1:11436/v1", "http://localhost:18797/", "http://localhost:18797"},
		{"http://127.0.0.1:11436", "http://0.0.0.0:18797", "http://127.0.0.1:18797"},
		// A remote llama-swap: its loopback proxy means ITS box.
		{"http://node-b:11436/v1", "http://127.0.0.1:18797", "http://node-b:18797"},
		{"http://node-b:11436", "http://seat-host:18797", "http://seat-host:18797"},
	}
	for _, c := range cases {
		got, err := SeatURL(c.endpoint, c.proxy)
		if err != nil || got != c.want {
			t.Errorf("SeatURL(%q, %q) = %q, %v; want %q", c.endpoint, c.proxy, got, err, c.want)
		}
	}
	if _, err := SeatURL("http://127.0.0.1:11436", ""); !errors.Is(err, ErrNoSeatAddress) {
		t.Errorf("empty proxy: err = %v, want ErrNoSeatAddress", err)
	}
	if _, err := SeatURL("http://127.0.0.1:11436", "not a url"); err == nil {
		t.Error("a proxy that is not an http(s) URL must be an error")
	}
}

// TestInflightOnAnEndpointThatNamesThisBoxReadsTheLoopbackSeat (review F1): the
// harness config names its own llama-swap by hostname, /running reports the
// seat's proxy as 127.0.0.1:<port>, and the seat binds loopback only. Re-pointing
// the read at <hostname>:<port> is refused on every read (the drain would run to
// its deadline, the spread deal would call the seat idle); the reader must keep
// the loopback address.
func TestInflightOnAnEndpointThatNamesThisBoxReadsTheLoopbackSeat(t *testing.T) {
	stubThisHost(t, "this-box.invalid")
	f := &fakeSwap{id: "qwen3.8-27b-vllm-3card", alias: "agent-pool", roster: true}
	f.loaded.Store(true)
	f.inflight.Store(2)
	seat := httptest.NewServer(f.handler()) // bound to 127.0.0.1 only
	defer seat.Close()
	swap := httptest.NewServer(f.handler())
	defer swap.Close()
	f.proxyHost = strings.TrimPrefix(seat.URL, "http://")
	_, swapPort, _ := net.SplitHostPort(strings.TrimPrefix(swap.URL, "http://"))
	swapAddr := strings.TrimPrefix(swap.URL, "http://")

	// "this-box" reaches llama-swap on its port; any other port on "this-box"
	// is refused, the way a 127.0.0.1-bound seat refuses the box's own hostname.
	dialer := &net.Dialer{}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, _ := net.SplitHostPort(addr)
			if host == "this-box.invalid" {
				if port != swapPort {
					return nil, errors.New("connection refused: the seat binds 127.0.0.1 only")
				}
				addr = swapAddr
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}}
	rd, err := Inflight(context.Background(), client, "http://this-box.invalid:"+swapPort, "agent-pool")
	// Canonical proves the roster was read over the SAME client (its dialer is
	// the only thing that can reach this-box.invalid): the alias resolved.
	if err != nil || !rd.Loaded || rd.Inflight != 2 || rd.Source != "metrics" || rd.Canonical != "qwen3.8-27b-vllm-3card" {
		t.Fatalf("reading = %+v, err %v; want loaded, canonical resolved, 2 in flight from the seat's loopback /metrics", rd, err)
	}
	if f.upstreamHits.Load() != 0 {
		t.Fatal("the reader used /upstream")
	}
}

// TestHostIsThisMachineKnowsItsOwnNameAndAddresses: the real check behind
// endpointIsThisHost answers true for this machine's hostname and for an
// address on one of its interfaces, and false for an address that is not.
func TestHostIsThisMachineKnowsItsOwnNameAndAddresses(t *testing.T) {
	if name, err := os.Hostname(); err == nil && name != "" {
		if !hostIsThisMachine(name) {
			t.Errorf("hostIsThisMachine(%q) = false for os.Hostname()", name)
		}
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
				if !hostIsThisMachine(ipn.IP.String()) {
					t.Errorf("hostIsThisMachine(%s) = false for an interface address", ipn.IP)
				}
				break
			}
		}
	}
	// 192.0.2.0/24 is TEST-NET-1: never assigned to a real interface.
	if hostIsThisMachine("192.0.2.77") {
		t.Error("hostIsThisMachine(192.0.2.77) = true for a documentation address")
	}
}

// remoteSwap wires a fake llama-swap reached as node-b:<its port> and a seat
// server whose /running proxy is 127.0.0.1:<seat port>. seatRoutable decides
// whether node-b:<seat port> reaches the seat (a seat that binds a routable
// address there) or is refused (the reference seat binds 127.0.0.1 only).
func remoteSwap(t *testing.T, seatRoutable bool) (*fakeSwap, *http.Client, string) {
	t.Helper()
	stubThisHost(t) // node-b is never this machine
	f := &fakeSwap{id: "qwen3.8-27b-vllm", alias: "agent-pool", roster: true}
	f.loaded.Store(true)
	f.inflight.Store(1)
	seat := httptest.NewServer(f.handler())
	t.Cleanup(seat.Close)
	swap := httptest.NewServer(f.handler())
	t.Cleanup(swap.Close)
	seatAddr := strings.TrimPrefix(seat.URL, "http://")
	swapAddr := strings.TrimPrefix(swap.URL, "http://")
	f.proxyHost = seatAddr
	_, swapPort, _ := net.SplitHostPort(swapAddr)
	_, seatPort, _ := net.SplitHostPort(seatAddr)
	dialer := &net.Dialer{}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, _ := net.SplitHostPort(addr)
			if host == "node-b.invalid" {
				switch {
				case port == swapPort:
					addr = swapAddr
				case port == seatPort && seatRoutable:
					addr = seatAddr
				default:
					return nil, errors.New("connection refused")
				}
			} else if host == "127.0.0.1" && port == seatPort {
				// The harness is NOT on node-b: this machine's loopback holds no
				// such seat, so a reader that kept the loopback proxy would be
				// reading some other process here.
				return nil, errors.New("connection refused: no seat on this machine's loopback")
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}}
	return f, client, "http://node-b.invalid:" + swapPort
}

// TestInflightOnARemoteEndpointWithALoopbackSeatSaysWhy (review R3): a llama-swap
// on another machine reports its seat at 127.0.0.1; the seat binds loopback
// only, so the re-pointed read is refused. The reader reports
// ErrRemoteSeatUnreachable — never idle, never an /upstream read.
func TestInflightOnARemoteEndpointWithALoopbackSeatSaysWhy(t *testing.T) {
	f, client, endpoint := remoteSwap(t, false)
	rd, err := Inflight(context.Background(), client, endpoint, "agent-pool")
	if !errors.Is(err, ErrRemoteSeatUnreachable) || !rd.Loaded {
		t.Fatalf("reading = %+v, err %v; want loaded + ErrRemoteSeatUnreachable", rd, err)
	}
	if f.upstreamHits.Load() != 0 {
		t.Fatal("the reader fell back to /upstream")
	}
}

// TestInflightOnARemoteEndpointReadsARoutableSeat: the same remote llama-swap,
// but the seat listens on a routable address there — the re-pointed read works.
func TestInflightOnARemoteEndpointReadsARoutableSeat(t *testing.T) {
	f, client, endpoint := remoteSwap(t, true)
	rd, err := Inflight(context.Background(), client, endpoint, "agent-pool")
	if err != nil || !rd.Loaded || rd.Inflight != 1 || rd.Source != "metrics" {
		t.Fatalf("reading = %+v, err %v; want loaded, 1 in flight from node-b's seat", rd, err)
	}
	if f.upstreamHits.Load() != 0 {
		t.Fatal("the reader used /upstream")
	}
}

// TestHostIsThisMachineCachesAFailedLookup (review R2): a name that does not
// resolve is remembered as "not this machine" for the short failure TTL, so a
// 2 s poll does not pay the lookup timeout on every read.
func TestHostIsThisMachineCachesAFailedLookup(t *testing.T) {
	const h = "seatload-selftest.invalid" // RFC 2606: never resolves
	thisHostCache.Delete(h)
	t.Cleanup(func() { thisHostCache.Delete(h) })
	before := time.Now()
	if hostIsThisMachine(h) {
		t.Fatalf("hostIsThisMachine(%q) = true for a name that cannot resolve", h)
	}
	v, ok := thisHostCache.Load(h)
	if !ok {
		t.Fatal("a failed lookup was not cached: every poll would pay the lookup timeout again")
	}
	a := v.(thisHostAnswer)
	if a.mine || a.expires.After(before.Add(thisHostFailTTL+thisHostLookupTimeout+time.Second)) || !a.expires.After(before) {
		t.Fatalf("cached answer %+v; want not-mine, expiring within the failure TTL", a)
	}
	// A cached answer is served without a new lookup.
	thisHostCache.Store(h, thisHostAnswer{mine: true, expires: time.Now().Add(time.Minute)})
	if !hostIsThisMachine(h) {
		t.Fatal("an unexpired cached answer was not used")
	}
	// An expired one is not.
	thisHostCache.Store(h, thisHostAnswer{mine: true, expires: time.Now().Add(-time.Second)})
	if hostIsThisMachine(h) {
		t.Fatal("an expired cached answer was served instead of a new lookup")
	}
}
