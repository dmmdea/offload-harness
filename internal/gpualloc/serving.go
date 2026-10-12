package gpualloc

import (
	"fmt"
	"strings"
	"sync"

	"github.com/dmmdea/offload-harness/internal/servingcfg"
)

// The serving config (config key serving_config_path) is the llama-swap yaml the box actually runs. Its
// entries pin their cards with an env CUDA_VISIBLE_DEVICES, and that pin places a model when `layers`
// does not declare it (register F15). The reader, and the rule for what counts as a pin, live in
// internal/servingcfg: the load fence reads the same file through the same parser (register C-86a,
// Config.ModelPins), which is why it is not here, where config cannot import it.

// servingState is the serving config as one scope reads it: lazily, once, and only when a model
// is not declared by layers.
type servingState struct {
	once    sync.Once
	path    string
	snap    servingcfg.Snapshot
	problem string
}

func (st *servingState) load() {
	st.once.Do(func() {
		snap, err := servingcfg.Load(st.path)
		if err != nil {
			if strings.TrimSpace(st.path) == "" {
				st.problem = "serving_config_path is unset, so the serving config places no model"
			} else {
				st.problem = fmt.Sprintf("the serving config could not be read (%v), so it places no model", err)
			}
			return
		}
		st.snap = snap
	})
}

// servingModelIDs lists the model ids of the serving config at path (aliases are not ids), for the
// candidate list of a lease whose llama-swap roster could not be read. Empty when it cannot be read.
func servingModelIDs(path string) []string { return servingcfg.IDs(path) }
