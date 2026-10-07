package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/netguard"
)

// Fixture zones: the operator's own tailnet, the tailnet that shared a node in, and a
// third nobody configured.
const (
	zoneOwn      = "tailnnnnnn.ts.net"
	zoneSharer   = "tailmmmmmm.ts.net"
	zoneStranger = "tailkkkkkk.ts.net"
)

// restoreZones snapshots the process-wide zone list and puts the WHOLE list back after
// the test: Load installs zones as a side effect, and withTailnet (the sibling helper)
// restores only the first one.
func restoreZones(t *testing.T) {
	t.Helper()
	prev := netguard.TailnetSuffixes()
	t.Cleanup(func() { _ = netguard.SetTailnetSuffixes(prev) })
}

func loadJSON(t *testing.T, body string) (Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

// A node shared in from another tailnet is named by its FQDN under the SHARER's zone.
// Every key that vets a base through the tailnet guard must accept it once that zone is
// listed, and still refuse a zone nobody listed.
func TestTailnetSuffixesAdmitTheSharersZoneOnEveryGuardedKey(t *testing.T) {
	restoreZones(t)
	shared := "http://node-b." + zoneSharer + ":11436"
	stranger := "http://node-x." + zoneStranger + ":11436"
	head := `{"tailnet_suffix":"` + zoneOwn + `","tailnet_suffixes":["` + zoneSharer + `"],`

	cases := []struct {
		name, body, wantErr string
	}{
		{"seat_endpoints accepts the sharer's zone", head + `"seat_endpoints":{"s":"` + shared + `"}}`, ""},
		{"cascade_remote_lanes accepts the sharer's zone", head + `"cascade_remote_lanes":{"s":"` + shared + `"}}`, ""},
		{"fleet_queue_holder accepts the sharer's zone", head + `"fleet_queue_holder":"http://node-b.` + zoneSharer + `:18811"}`, ""},
		{"seat_endpoints refuses an unlisted zone", head + `"seat_endpoints":{"s":"` + stranger + `"}}`, `seat_endpoints["s"]`},
		{"cascade_remote_lanes refuses an unlisted zone", head + `"cascade_remote_lanes":{"s":"` + stranger + `"}}`, `cascade_remote_lanes["s"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadJSON(t, tc.body)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("load refused: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("load error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// Load installs the union into netguard, the own zone first, so a process that loaded
// the config has the same answer everywhere the guard is consulted.
func TestLoadInstallsBothKeysAsOneOrderedList(t *testing.T) {
	restoreZones(t)
	if _, err := loadJSON(t, `{"tailnet_suffix":"`+zoneOwn+`","tailnet_suffixes":["`+zoneSharer+`"]}`); err != nil {
		t.Fatal(err)
	}
	if got, want := netguard.TailnetSuffixes(), []string{zoneOwn, zoneSharer}; !reflect.DeepEqual(got, want) {
		t.Errorf("installed zones = %q, want %q", got, want)
	}
	// Reloading a config that names fewer zones REPLACES the list: a zone removed from the
	// file stops being admitted, instead of lingering until the process exits.
	if _, err := loadJSON(t, `{"tailnet_suffix":"`+zoneOwn+`"}`); err != nil {
		t.Fatal(err)
	}
	if got, want := netguard.TailnetSuffixes(), []string{zoneOwn}; !reflect.DeepEqual(got, want) {
		t.Errorf("zones after a reload with one key = %q, want %q", got, want)
	}
}

// The original single key is untouched: a config that only sets tailnet_suffix admits
// that zone and no other, exactly as before the list existed.
func TestTheSingleSuffixKeyStillWorksAlone(t *testing.T) {
	restoreZones(t)
	if _, err := loadJSON(t, `{"tailnet_suffix":"`+zoneOwn+`","seat_endpoints":{"s":"http://node-a.`+zoneOwn+`:11436"}}`); err != nil {
		t.Fatalf("own zone refused: %v", err)
	}
	_, err := loadJSON(t, `{"tailnet_suffix":"`+zoneOwn+`","seat_endpoints":{"s":"http://node-b.`+zoneSharer+`:11436"}}`)
	if err == nil || !strings.Contains(err.Error(), `seat_endpoints["s"]`) {
		t.Fatalf("a zone that was not listed must stay refused, got %v", err)
	}
}

// A malformed zone is a load error that names the key and the index, in both keys, and
// leaves nothing half-installed.
func TestMalformedZonesFailTheLoadNamingTheKey(t *testing.T) {
	restoreZones(t)
	if err := netguard.SetTailnetSuffixes([]string{zoneOwn}); err != nil {
		t.Fatal(err)
	}
	for body, want := range map[string]string{
		`{"tailnet_suffix":"not a zone"}`:                                               `tailnet_suffix`,
		`{"tailnet_suffix":"` + zoneOwn + `","tailnet_suffixes":["x.ts.net","nodots"]}`: `tailnet_suffixes[1]`,
		`{"tailnet_suffixes":["http://` + zoneSharer + `"]}`:                            `tailnet_suffixes[0]`,
	} {
		_, err := loadJSON(t, body)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("config %s: load error = %v, want one naming %q", body, err, want)
			continue
		}
		if got := netguard.TailnetSuffixes(); !reflect.DeepEqual(got, []string{zoneOwn}) {
			t.Errorf("config %s: a refused load changed the installed zones to %q", body, got)
		}
	}
}

// TailnetZones is the union: own zone first, entries in the order written, a zone named
// twice (or by spelling variants) once, unset slots skipped.
func TestTailnetZonesIsTheNormalizedUnion(t *testing.T) {
	c := Config{TailnetSuffix: "  " + strings.ToUpper(zoneOwn) + ".", TailnetSuffixes: []string{"", zoneSharer, "." + zoneOwn, zoneSharer + ".", zoneStranger}}
	got, err := c.TailnetZones()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{zoneOwn, zoneSharer, zoneStranger}; !reflect.DeepEqual(got, want) {
		t.Errorf("TailnetZones() = %q, want %q", got, want)
	}
	if got, err := (Config{}).TailnetZones(); err != nil || len(got) != 0 {
		t.Errorf("a config naming no zone must have none, got %q, %v", got, err)
	}
}

// The KV store host check judges every zone: a store reached by a name under the
// sharer's zone is admitted once that zone is listed, and still refused otherwise.
func TestKVCacheServerAcceptsAHostUnderAnyConfiguredZone(t *testing.T) {
	restoreZones(t)
	if err := netguard.SetTailnetSuffixes([]string{zoneOwn, zoneSharer}); err != nil {
		t.Fatal(err)
	}
	ok := KVCacheServer{Enabled: true, Address: "store." + zoneSharer + ":18799", Seat: "s"}
	if err := ValidateKVCacheServer(&ok); err != nil {
		t.Errorf("a store under the second listed zone was refused: %v", err)
	}
	bad := KVCacheServer{Enabled: true, Address: "store." + zoneStranger + ":18799", Seat: "s"}
	err := ValidateKVCacheServer(&bad)
	if err == nil || !strings.Contains(err.Error(), zoneOwn) || !strings.Contains(err.Error(), zoneSharer) {
		t.Errorf("a store under an unlisted zone must be refused naming the zones, got %v", err)
	}
}
