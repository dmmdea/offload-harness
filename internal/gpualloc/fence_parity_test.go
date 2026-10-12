package gpualloc

import (
	"context"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// The load fence and the unload list are two readings of ONE placement (register C-86a): the unload
// list is LeaseScope (this package), the fence is modelaffinity armed from Config.ModelPins. For every
// model and every one-card lease, the model is on the leased cards for the one exactly when it is for
// the other, so a drain that leaves a seat alone never lets the fence hold it, and a fence that lets a
// load through never finds the unload list has already taken that seat. A model both layers and the
// serving config declare is read from layers by both; one only the serving config declares is read from
// it by both; one neither places is on every card for both (the direction of every doubt).
func TestTheLoadFenceAndTheUnloadListPlaceEveryModelAlike(t *testing.T) {
	cards := threeCards()
	cfg := servingCfg(t)
	t.Cleanup(modelaffinity.SetCardTableReader(func(context.Context, string) ([]gpuprobe.Card, string, error) {
		return cards, "", nil
	}))
	modelaffinity.SetSeatPins(cfg.ModelPins)
	t.Cleanup(func() { modelaffinity.SetSeatPins(nil) })

	models := []string{
		// declared by layers (scopedCfg)
		"on-card-0", "on-card-2", "pair",
		// declared by the serving config only (servingYAML), with and without a usable pin
		"yaml-card0", "yaml-card0-alias", "yaml-card2", "yaml-idx0", "yaml-idx-noorder", "yaml-pair",
		"yaml-unpinned", "yaml-cmd-only", "yaml-proxy", "yaml-macro",
		// declared by neither
		"not-in-yaml",
	}
	for _, leased := range cards {
		id := leased.LeaseID()
		scope := NewLeaseScope(context.Background(), cfg, []string{id}, hostDeps(cards, false), nil)
		lease := gpulease.Info{Held: true, Class: gpulease.ClassMedia, Epoch: 1, Devices: []string{id}}
		for _, model := range models {
			fenced := modelaffinity.ScopeToModel(lease, model).Held
			unloaded := scope.Touches(model)
			if fenced != unloaded {
				t.Errorf("a lease on card %d: %s is %s by the fence and %s by the unload list (%+v)",
					leased.NvidiaIndex, model, verdictWord(fenced, "fenced", "free"), verdictWord(unloaded, "on the leased card", "left resident"), scope.Locate(model))
			}
		}
	}
}

func verdictWord(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}
