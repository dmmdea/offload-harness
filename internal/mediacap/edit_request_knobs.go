package mediacap

// Per-request canvas and placement for a generative edit.
//
// The edit lane's canvas (gen_edit_megapixels) and card placement (gen_edit_placement) are binding keys of the machine that renders.
// An edit sent to another node through the fleet cannot rely on that node's binding being its asker's, so a request may carry both
// itself: params.megapixels (0.01 to 16, the range of gen_edit_megapixels) and params.placement ("single" or "multicard"). When
// present they replace the binding's value for that one call, on the config copy the call renders under. Absent, the binding's own
// value stands, which is what every local call has always done.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
)

// The range of an edit canvas in megapixels, as gen_edit_megapixels documents it.
const (
	EditMegapixelsMin = 0.01
	EditMegapixelsMax = 16.0
)

// ApplyEditRequestKnobs returns cfg (an EFFECTIVE edit binding) with the request's own megapixels and placement applied, or the
// reason the request cannot have them. Nothing is leased or started here: the fleet node calls it at admission so a request that
// can never render is refused up front, and the pipeline calls it before it takes a lease, so the two read one verdict. A canvas
// over the largest one the card-only recipe is scaled to is refused here by name; a binding's own multicard verdict stays the
// lease's.
func ApplyEditRequestKnobs(cfg config.Config, params map[string]any) (config.Config, error) {
	overridden := false
	if raw, present := params["megapixels"]; present && raw != nil {
		mp := editParamFloat(raw)
		if mp < EditMegapixelsMin || mp > EditMegapixelsMax {
			return cfg, fmt.Errorf("megapixels must be between %g and %g (got %v)", EditMegapixelsMin, EditMegapixelsMax, raw)
		}
		if cfg.GenEditFamily == config.FamilyQwenImage21 {
			return cfg, fmt.Errorf("megapixels is a %s canvas size; the %s edit graph sizes by its reference resolution and refuses the flag",
				config.EditFamily2511, config.FamilyQwenImage21)
		}
		cfg.GenEditMegapixels, overridden = mp, true
	}
	if raw, present := params["placement"]; present && raw != nil {
		p, _ := raw.(string)
		switch strings.TrimSpace(p) {
		case config.PlacementSingle, config.PlacementMulticard:
			cfg.GenEditPlacement, overridden = strings.TrimSpace(p), true
		default:
			return cfg, fmt.Errorf("placement must be %q or %q (got %v)", config.PlacementSingle, config.PlacementMulticard, raw)
		}
	}
	if !overridden || !cfg.EditMulticard() {
		return cfg, nil
	}
	family := strings.TrimSpace(cfg.GenEditFamily)
	if family == "" {
		family = config.EditFamily2511
	}
	if row := FindMulticardRow("edit", family, EditModelFiles(cfg)); row != nil && row.MaxMegapixels > 0 && cfg.GenEditMegapixels > row.MaxMegapixels {
		max := strconv.FormatFloat(row.MaxMegapixels, 'g', -1, 64)
		return cfg, fmt.Errorf("a multicard edit is held to a canvas of at most %s megapixels (the card-only recipe %s is scaled to it) and this call asks for %g; ask for %s or less, or place it single",
			max, row.ID, cfg.GenEditMegapixels, max)
	}
	return cfg, nil
}

func editParamFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	}
	return 0
}
