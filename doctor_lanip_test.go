package main

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// The LAN-literal row warns when an endpoint the harness dials names its host by an IP literal in a
// private range instead of a name. Cross-machine reach uses tailnet names: a literal breaks when a
// box moves, or when its firewall is scoped to the tailnet. It is informational (never red, never an
// exit code) and it never prints the address, only the field that carries it.
//
// Every address below is built at runtime: this repository is public, and no private or tailnet
// literal may sit in a tracked file, not even a synthetic one.

func v4(a, b, c, d byte) string { return netip.AddrFrom4([4]byte{a, b, c, d}).String() }

func v6(prefix ...byte) string {
	var raw [16]byte
	copy(raw[:], prefix)
	raw[15] = 1
	return netip.AddrFrom16(raw).String()
}

func lanIPRow(cfg config.Config) string {
	var b strings.Builder
	writeLANIPEndpointsSection(&b, cfg)
	return b.String()
}

// Each field the harness dials is read, and the row names the field (with its index or map key) and
// never the address.
func TestDoctorLANIPWarnsOnEveryDialedFieldWithoutPrintingTheAddress(t *testing.T) {
	lan := v4(192, 168, 7, 9)
	base := "http://" + lan + ":18811"
	cases := []struct {
		name  string
		set   func(*config.Config)
		label string
	}{
		{"endpoint", func(c *config.Config) { c.Endpoint = "http://" + lan + ":11436" }, "endpoint"},
		{"seat_endpoints", func(c *config.Config) { c.SeatEndpoints = map[string]string{"seat-a": base} }, `seat_endpoints["seat-a"]`},
		{"cascade_remote_lanes", func(c *config.Config) { c.CascadeRemoteLanes = map[string]string{"seat-b": base} }, `cascade_remote_lanes["seat-b"]`},
		{"delegate_remotes", func(c *config.Config) { c.DelegateRemotes = []string{"http://node-a:18811", base} }, "delegate_remotes[1]"},
		{"fleet_queue_holder", func(c *config.Config) { c.FleetQueueHolder = base }, "fleet_queue_holder"},
		{"pair_workloads_relay", func(c *config.Config) { c.PairWorkloadsRelay = []string{"off", base} }, "pair_workloads_relay[1]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			tc.set(&cfg)
			got := lanIPRow(cfg)
			if !strings.Contains(got, "lan ip:") || !strings.Contains(got, "WARN") || !strings.Contains(got, tc.label) {
				t.Fatalf("no WARN row naming %s:\n%q", tc.label, got)
			}
			if strings.Contains(got, lan) {
				t.Fatalf("the row prints the address it warns about:\n%q", got)
			}
			if strings.Count(got, "\n") != 1 {
				t.Fatalf("want exactly one row for one offending field, got:\n%q", got)
			}
		})
	}
}

// The ranges: RFC 1918, the link-local block and the IPv6 unique-local block are private-range; the
// tailnet's own addresses get the MagicDNS advice; loopback, names and public literals say nothing.
func TestDoctorLANIPClassifiesTheRanges(t *testing.T) {
	cases := []struct {
		name string
		host string
		want string // a phrase the row must carry; "" means no row at all
	}{
		{"RFC 1918 class A", v4(10, 1, 2, 3), "private-range"},
		{"RFC 1918 class B low edge", v4(172, 16, 0, 1), "private-range"},
		{"RFC 1918 class B high edge", v4(172, 31, 255, 254), "private-range"},
		{"just below class B", v4(172, 15, 0, 1), ""},
		{"just above class B", v4(172, 32, 0, 1), ""},
		{"RFC 1918 class C", v4(192, 168, 0, 5), "private-range"},
		{"link-local v4", v4(169, 254, 3, 4), "link-local"},
		{"unique-local v6", "[" + v6(0xfd, 0x00) + "]", "private-range"},
		{"link-local v6", "[" + v6(0xfe, 0x80) + "]", "link-local"},
		{"v4-mapped private", "[::ffff:" + v4(10, 9, 8, 7) + "]", "private-range"},
		{"v4-mapped tailnet", "[::ffff:" + v4(100, 100, 1, 2) + "]", "MagicDNS name"},
		{"tailnet CGNAT low edge", v4(100, 64, 0, 1), "MagicDNS name"},
		{"tailnet CGNAT high edge", v4(100, 127, 255, 254), "MagicDNS name"},
		{"just below CGNAT", v4(100, 63, 255, 255), ""},
		{"just above CGNAT", v4(100, 128, 0, 1), ""},
		{"tailnet v6", "[" + v6(0xfd, 0x7a, 0x11, 0x5c, 0xa1, 0xe0) + "]", "MagicDNS name"},
		{"loopback v4", v4(127, 0, 0, 1), ""},
		{"loopback v6", "[::1]", ""},
		{"public literal", v4(8, 8, 8, 8), ""},
		{"dotless name", "node-a", ""},
		{"dotted name", "node-a.tailnnnnnn.ts.net", ""},
		{"localhost", "localhost", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.DelegateRemotes = []string{"http://" + tc.host + ":18811/"}
			got := lanIPRow(cfg)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("%s must say nothing, got:\n%q", tc.host, got)
				}
				return
			}
			if !strings.Contains(got, tc.want) || !strings.Contains(got, "delegate_remotes[0]") {
				t.Fatalf("%s: row lacks %q:\n%q", tc.host, tc.want, got)
			}
			host := strings.Trim(tc.host, "[]")
			if strings.Contains(got, host) {
				t.Fatalf("the row prints the address it warns about:\n%q", got)
			}
		})
	}
}

// The rationale is in the row: a tailnet name is the rule for cross-machine reach, and the tailnet's
// own literal is told to use the MagicDNS name.
func TestDoctorLANIPRowStatesWhyAndWhatToUse(t *testing.T) {
	cfg := config.Default()
	cfg.SeatEndpoints = map[string]string{"seat-a": "http://" + v4(10, 0, 0, 7) + ":11436"}
	got := lanIPRow(cfg)
	for _, want := range []string{"tailnet name", "firewall", "moves"} {
		if !strings.Contains(got, want) {
			t.Errorf("a private-range row lacks %q:\n%q", want, got)
		}
	}
	cfg.SeatEndpoints = map[string]string{"seat-a": "http://" + v4(100, 100, 1, 2) + ":11436"}
	got = lanIPRow(cfg)
	if !strings.Contains(got, "use the MagicDNS name") {
		t.Errorf("a tailnet-literal row lacks the MagicDNS advice:\n%q", got)
	}
}

// Rows come out in a fixed order, so doctor output can be diffed: field order of the config, then
// sorted map keys, then list order.
func TestDoctorLANIPRowsAreOrderedAndOnePerField(t *testing.T) {
	lan := "http://" + v4(10, 4, 4, 4) + ":18811"
	cfg := config.Default()
	cfg.Endpoint = lan
	cfg.SeatEndpoints = map[string]string{"zeta": lan, "alpha": lan}
	cfg.DelegateRemotes = []string{lan, "http://node-a:18811", lan}
	got := lanIPRow(cfg)
	if got == "" {
		t.Fatal("five offending fields printed no row")
	}
	var labels []string
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		fields := strings.Fields(strings.TrimPrefix(line, "lan ip:"))
		if len(fields) < 2 {
			t.Fatalf("malformed row %q", line)
		}
		labels = append(labels, fields[1])
	}
	want := []string{"endpoint", `seat_endpoints["alpha"]`, `seat_endpoints["zeta"]`, "delegate_remotes[0]", "delegate_remotes[2]"}
	if strings.Join(labels, "|") != strings.Join(want, "|") {
		t.Fatalf("rows named %v, want %v\n%s", labels, want, got)
	}
}

// A config with nothing to warn about prints nothing, so a green doctor stays as short as it was.
// Values that are not dialed (a disabled or storeless cache binding, a filesystem store, the relay's
// two words) and values another row already calls unusable stay silent too.
func TestDoctorLANIPRowIsAbsentWhereNothingIsDialed(t *testing.T) {
	if got := lanIPRow(config.Default()); got != "" {
		t.Fatalf("the default config must print nothing, got:\n%q", got)
	}
	lan := v4(192, 168, 1, 20)
	cfg := config.Default()
	cfg.PairWorkloadsRelay = []string{"auto", "OFF"}
	cfg.DelegateRemotes = []string{"", lan + ":18811", "not a url"} // unset slot, scheme-less, garbage
	cfg.KVCacheServers = config.KVCacheServers{
		{Enabled: false, Seat: "off", Address: lan + ":6379"},
		{Storeless: true, Seat: "storeless", Reason: "scratch seat"},
		{Enabled: true, Seat: "files", Store: "fs_native", Address: "/mnt/kv"},
		nil,
	}
	if got := lanIPRow(cfg); got != "" {
		t.Fatalf("nothing here is dialed at a private literal, got:\n%q", got)
	}
}

// A cache-server store is not this row's business. The config documents a literal on the direct LAN
// (or the tailnet) as a valid store address, and prefers it to a name: bulk KV over MagicDNS measured
// several times slower, and a name turns off the status readback, which dials literals only. A row
// that told the operator to rename it would push the binding onto the slower path with no way to
// silence it, so a store address says nothing, whatever range it is in.
func TestDoctorLANIPRowLeavesCacheStoreAddressesAlone(t *testing.T) {
	for name, addr := range map[string]string{
		"private v4":            v4(192, 168, 7, 9) + ":6379",
		"private v6, bracketed": "[" + v6(0xfd, 0x00) + "]:6379",
		"link-local v4":         v4(169, 254, 3, 4) + ":6379",
		"tailnet v4":            v4(100, 100, 1, 2) + ":6379",
		"tailnet v6, bracketed": "[" + v6(0xfd, 0x7a, 0x11, 0x5c, 0xa1, 0xe0) + "]:6379",
		"name":                  "store-a:6379",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config.Default()
			cfg.KVCacheServers = config.KVCacheServers{
				{Enabled: true, Seat: "seat-c", Address: addr},
				{Enabled: true, Address: addr},
			}
			if got := lanIPRow(cfg); got != "" {
				t.Fatalf("a cache-server store address must say nothing, got: %q", got)
			}
		})
	}
}

// The row is in doctor's pure-config band, reaches the real output, and never changes the exit code.
func TestDoctorRunPrintsTheLANIPRowAndStaysGreen(t *testing.T) {
	srv := fakeSwap(t, defaultAliasIDs())
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	lan := v4(172, 20, 1, 1)
	cfg.PairWorkloadsRelay = []string{"http://" + lan + ":18811"}
	var out strings.Builder
	if err := doctorRun(cfg, nil, &out); err != nil {
		t.Fatalf("a LAN literal must warn, never fail doctor: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "lan ip:     WARN  pair_workloads_relay[0]") {
		t.Fatalf("doctor output lacks the LAN-literal row:\n%s", out.String())
	}
	if strings.Contains(out.String(), lan) {
		t.Fatalf("doctor prints the address the LAN-literal row warns about:\n%s", out.String())
	}
}
