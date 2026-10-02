package mediacap

// CT-47: the per-family video binding rows (offload_status media.video_family_bindings,
// doctor) publish each family's license and commercial_use the way FamilyRow does for
// the image families: null when the family declares none (UNKNOWN, never
// commercial-safe), and no warning text anywhere (operator order 2026-10-01).

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

func TestVideoFamilyBindingRowsCarryLicenseAndCommercialUse(t *testing.T) {
	no := false
	cfg := bare()
	cfg.VideoGenFamily = "ltx25"
	cfg.VideoGenFamilies = map[string]config.VideoFamilyBinding{
		"hunyuan": {License: "Example Community License", CommercialUse: &no},
	}
	rows := VideoFamilyBindingRows(cfg)
	byName := map[string]VideoFamilyBindingRow{}
	for _, r := range rows {
		byName[r.Family] = r
	}
	hy := byName["hunyuan"]
	if hy.License == nil || *hy.License != "Example Community License" || hy.CommercialUse == nil || *hy.CommercialUse {
		t.Errorf("hunyuan row = %+v, want its license and commercial_use false", hy)
	}
	for _, name := range []string{"wan22", "ltx25", "h3"} {
		if r := byName[name]; r.License != nil || r.CommercialUse != nil {
			t.Errorf("%s declares no license; row = %+v must read null, never invented", name, r)
		}
	}
	// The wire shape: both keys are always present, null when undeclared.
	raw, err := json.Marshal(byName["wan22"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"license":null`) || !strings.Contains(string(raw), `"commercial_use":null`) {
		t.Errorf("undeclared row JSON = %s, want license and commercial_use as null", raw)
	}
	if strings.Contains(strings.ToLower(string(raw)), "note") {
		t.Errorf("row JSON carries warning text: %s", raw)
	}
}

// The box's own default family is tagged too (its weights come from the flat keys, its
// license from its videogen_families entry).
func TestVideoFamilyBindingRowTagsTheBoxsDefaultFamily(t *testing.T) {
	yes := true
	cfg := bare()
	cfg.VideoGenFamily = "wan22"
	cfg.VideoGenFamilies = map[string]config.VideoFamilyBinding{
		"wan22": {License: "Example Open License", CommercialUse: &yes},
	}
	for _, r := range VideoFamilyBindingRows(cfg) {
		if r.Family != "wan22" {
			continue
		}
		if !r.Default || r.License == nil || *r.License != "Example Open License" || r.CommercialUse == nil || !*r.CommercialUse {
			t.Errorf("default wan22 row = %+v", r)
		}
		return
	}
	t.Fatal("no wan22 row")
}
