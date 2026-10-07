package main

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

// healthyReading is a roster reading whose health was read; unreachableReading a node that did not
// answer; refusedReading an entry the tailnet guard refuses (never dialled).
func healthyReading(i int, base string) rosterprobe.Reading {
	return rosterprobe.Reading{Member: rosterprobe.Member{Index: i, Base: base}}
}

func unreachableReading(i int, base string) rosterprobe.Reading {
	return rosterprobe.Reading{Member: rosterprobe.Member{Index: i, Base: base}, Err: errors.New("dial tcp: connection refused")}
}

func refusedReading(i int, base string) rosterprobe.Reading {
	m := rosterprobe.Members([]string{base})[0]
	m.Index = i
	return rosterprobe.Reading{Member: m, Err: m.Refused}
}

// Health ignores the bearer, so a wrong, missing or absent fleet token reads healthy on every other
// doctor row. This section is where it shows: one row per roster entry, by its delegate_remotes slot,
// naming what the node answered, and never the token or the node's reply text.
func TestDoctorFleetTokenRowsNameEachNodeByRosterIndex(t *testing.T) {
	rostertest.Zones(t)
	const secret = "s3cret-fleet-token"
	cfg := config.Default()
	cfg.FleetAuthToken = secret
	roster := []rosterprobe.Reading{
		healthyReading(0, "http://192.0.2.1:18811"),
		healthyReading(1, "http://192.0.2.2:18811"),
		healthyReading(2, "http://192.0.2.3:18811"),
		healthyReading(4, "http://192.0.2.5:18811"), // slot 3 is blank: the index is the CONFIG slot
		healthyReading(5, "http://192.0.2.6:18811"),
		unreachableReading(6, "http://192.0.2.7:18811"),
		refusedReading(7, "http://203.0.113.9:18811"),
	}
	answers := map[string]struct {
		status int
		body   string
		err    error
	}{
		"http://192.0.2.1:18811": {400, `{"error":"job_id required"}`, nil},
		"http://192.0.2.2:18811": {401, `{"error":"unauthorized, echo ` + secret + `"}`, nil},
		"http://192.0.2.3:18811": {403, `{"error":"agent lane requires fleet_auth_token"}`, nil},
		"http://192.0.2.5:18811": {0, "", errors.New("context deadline exceeded")},
		"http://192.0.2.6:18811": {404, `not found`, nil},
	}
	var asked atomic.Int32
	var b strings.Builder
	writeFleetTokenSection(&b, cfg, roster, func(base string) (int, string, error) {
		asked.Add(1)
		a, ok := answers[base]
		if !ok {
			t.Errorf("the token probe was sent to %s: a refused or unreachable entry is never asked", base)
		}
		return a.status, a.body, a.err
	})
	out := b.String()
	for _, want := range []string{
		"fleet token (fleet_auth_token is set here; its value is never printed):",
		"OK           delegate_remotes[0] http://192.0.2.1:18811 ",
		"MISMATCH     delegate_remotes[1] http://192.0.2.2:18811 ",
		"NO-TOKEN     delegate_remotes[2] http://192.0.2.3:18811 ",
		"UNKNOWN      delegate_remotes[4] http://192.0.2.5:18811 — the token probe failed: context deadline exceeded",
		"UNKNOWN      delegate_remotes[5] http://192.0.2.6:18811 — the node answered 404 ",
		"UNCHECKED    delegate_remotes[6] http://192.0.2.7:18811 — not asked: the node's health could not be read",
		"UNCHECKED    delegate_remotes[7] http://203.0.113.9:18811 — not asked: the entry is refused by the tailnet guard",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor token rows lack %q:\n%s", want, out)
		}
	}
	if got := asked.Load(); got != 5 {
		t.Errorf("the probe was sent %d times, want 5 (only nodes whose health was read)", got)
	}
	if strings.Contains(out, secret) || strings.Contains(out, "unauthorized, echo") {
		t.Errorf("doctor printed the token or a node's reply text:\n%s", out)
	}
}

// A box with no token says so once, without asking any node: the rows below would all be 401s the
// operator can already predict, and the useful line is the missing key.
func TestDoctorFleetTokenSaysWhenThisBoxHasNone(t *testing.T) {
	cfg := config.Default()
	cfg.FleetAuthToken = ""
	var b strings.Builder
	writeFleetTokenSection(&b, cfg, []rosterprobe.Reading{healthyReading(0, "http://192.0.2.1:18811")}, func(string) (int, string, error) {
		t.Fatal("no token here: no node is asked")
		return 0, "", nil
	})
	if out := b.String(); !strings.Contains(out, "fleet token: NOT SET here (fleet_auth_token)") {
		t.Errorf("a missing token must be named:\n%s", out)
	}
	b.Reset()
	cfg.FleetAuthToken = "x"
	writeFleetTokenSection(&b, cfg, nil, func(string) (int, string, error) { t.Fatal("no roster: nothing to ask"); return 0, "", nil })
	if b.Len() != 0 {
		t.Errorf("no roster must print nothing, got %q", b.String())
	}
}

// A refused roster entry is named by its slot in the version section too, with the guard's reason, and
// is not called unreachable. Nothing a URL can carry that is a secret is printed.
func TestDoctorFleetVersionsNameARefusedEntryByIndex(t *testing.T) {
	rostertest.Zones(t)
	rosterprobe.Default.Reset()
	t.Cleanup(rosterprobe.Default.Reset)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{"", "http://203.0.113.9:18811?token=tok3n"}
	roster := rosterprobe.Probe(context.Background(), cfg.DelegateRemotes, "", time.Second)
	var b strings.Builder
	writeFleetSkewSection(&b, cfg, "1.2.3", rosterVersionReader(roster))
	out := b.String()
	if !strings.Contains(out, "REFUSED      delegate_remotes[1] http://203.0.113.9:18811 — not dialled, refused by the tailnet guard") {
		t.Errorf("a refused entry must be named by slot, as refused, not dialled:\n%s", out)
	}
	if strings.Contains(out, "tok3n") || strings.Contains(out, "UNREACHABLE") {
		t.Errorf("a query-string secret leaked, or a refused entry was called unreachable:\n%s", out)
	}
}

type tokenTestRunner struct{}

func (tokenTestRunner) Run(context.Context, core.Request) core.Result { return core.Result{} }

// The whole doctor path against the REAL node handler: health reads fine whatever the bearer is, and
// only the token rows tell the nodes apart.
func TestDoctorFleetTokenAgainstRealNodes(t *testing.T) {
	rostertest.Zones(t)
	rosterprobe.Default.Reset()
	t.Cleanup(rosterprobe.Default.Reset)
	node := func(token string) string {
		cfg := config.Config{FleetAuthToken: token, MediaDir: t.TempDir()}
		jobs := fleetnode.NewJobs(time.Hour, cfg.FleetConcurrencyLimit())
		t.Cleanup(func() { jobs.DrainAndStop(2 * time.Second) })
		s := fleetnode.New(tokenTestRunner{}, jobs, fleetnode.Options{
			NodeID: "node-b", Cfg: cfg, GpuVendor: "nvidia", GpuArch: "ampere",
			Snapshot: func() (fleetnode.Snapshot, bool) {
				return fleetnode.Snapshot{TotalGiB: 16, FreeGiB: 12, At: time.Now()}, true
			},
			Footprints: func() []fleetnode.FootprintEntry { return nil },
		})
		srv := httptest.NewServer(s.Handler())
		t.Cleanup(srv.Close)
		return srv.URL
	}
	same, other := node("the-shared-token"), node("a-different-token")
	cfg := config.Default()
	cfg.FleetAuthToken = "the-shared-token"
	cfg.DelegateRemotes = []string{same, other}
	roster := rosterprobe.Probe(context.Background(), cfg.DelegateRemotes, cfg.FleetAuthToken, 2*time.Second)
	var b strings.Builder
	writeFleetTokenSection(&b, cfg, roster, func(base string) (int, string, error) {
		return rosterprobe.CheckToken(context.Background(), base, cfg.FleetAuthToken)
	})
	out := b.String()
	if !strings.Contains(out, "OK           delegate_remotes[0] "+same) {
		t.Errorf("the node holding this box's token must read OK:\n%s", out)
	}
	if !strings.Contains(out, "MISMATCH     delegate_remotes[1] "+other) {
		t.Errorf("the node holding another token must read MISMATCH:\n%s", out)
	}
	if strings.Contains(out, "the-shared-token") || strings.Contains(out, "a-different-token") {
		t.Errorf("a token was printed:\n%s", out)
	}
}

// A roster entry is a base URL, but nothing stops one being pasted with a user:password or a token in it,
// and doctor output gets shared. Whatever the row is (refused and never dialled, health unreadable, the
// token probe failing at the transport), no credential an entry carries appears in it, in any spelling:
// userinfo, a query value, a %-escaped query value. The host and the cause stay, so the row is still
// useful, and the lanes' "probed ..." line for the same entries reads from the same redaction.
func TestDoctorFleetRowsNeverPrintACredentialFromARosterEntry(t *testing.T) {
	rostertest.Zones(t)
	rosterprobe.Default.Reset()
	t.Cleanup(rosterprobe.Default.Reset)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedHost := l.Addr().String()
	_ = l.Close() // nothing listens here any more: the dial is refused
	const (
		userinfo = "user:pw-s3cret@"
		query    = "?token=tok%33-s3cret&k=plain-s3cret#frag-s3cret"
	)
	cfg := config.Default()
	cfg.FleetAuthToken = "the-fleet-bearer"
	cfg.DelegateRemotes = []string{
		"http://" + userinfo + "203.0.113.9:18811/" + query, // refused by the tailnet guard, never dialled
		"http://" + userinfo + closedHost + query,           // admitted, refused at the transport
		"http://" + closedHost + query,                      // query only
	}
	roster := rosterprobe.Probe(context.Background(), cfg.DelegateRemotes, cfg.FleetAuthToken, time.Second)

	var b strings.Builder
	writeFleetSkewSection(&b, cfg, "1.2.3", rosterVersionReader(roster))
	// Force the token probe to dial too: it is a separate request with its own error.
	dialled := []rosterprobe.Reading{healthyReading(0, cfg.DelegateRemotes[1]), healthyReading(1, cfg.DelegateRemotes[2])}
	writeFleetTokenSection(&b, cfg, append(roster[:1:1], dialled...), func(base string) (int, string, error) {
		return rosterprobe.CheckToken(context.Background(), base, cfg.FleetAuthToken)
	})
	// Readers and probe functions that hand back RAW errors (ones that quote the entry as configured)
	// are scrubbed by the row itself, not only by the producers.
	writeFleetSkewSection(&b, cfg, "1.2.3", func(base string) (string, error) {
		if base == strings.TrimRight(cfg.DelegateRemotes[0], "/") {
			return "", &rosterprobe.Refusal{Reason: errors.New(`hostname not allowed in "` + base + `"`)}
		}
		return "", errors.New(`Get "` + base + `/fleet/health": dial tcp: refused`)
	})
	writeFleetTokenSection(&b, cfg, dialled, func(base string) (int, string, error) {
		return 0, "", errors.New(`Post "` + base + `/fleet/dispatch": dial tcp: refused`)
	})
	out := b.String()
	for _, secret := range []string{"pw-s3cret", "s3cret", "tok%33", "tok3", "frag", "user:"} {
		if strings.Contains(out, secret) {
			t.Errorf("doctor printed %q from a roster entry:\n%s", secret, out)
		}
	}
	for _, want := range []string{"REFUSED      delegate_remotes[0] http://203.0.113.9:18811/ — not dialled", "UNREACHABLE  delegate_remotes[1] http://" + closedHost, "the token probe failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("the redacted row must still name the entry and the cause, missing %q:\n%s", want, out)
		}
	}
	// The lanes word the same entries through Reading.Miss; it carries no credential either.
	for _, r := range roster {
		if m := r.Miss(); strings.Contains(m, "s3cret") || strings.Contains(m, "tok%33") || strings.Contains(m, "user:") {
			t.Errorf("Miss printed a credential: %s", m)
		}
	}
}
