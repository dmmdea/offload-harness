package gpualloc

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// The serving config (config key serving_config_path) is the llama-swap yaml the box actually runs.
// Nearly every llama.cpp model in it pins its cards with an env entry,
// `CUDA_VISIBLE_DEVICES=<uuid>[,<uuid>]` (GPU UUIDs, sometimes indices), and that pin is where the
// seat really runs, so it places a model when `layers` does not declare it (register F15).
//
// ONLY THE ENV PIN COUNTS. A model whose entry names no device in env (an unpinned dual-card
// `-sm layer` / `--tensor-split` entry), a `proxy:` seat (a vLLM engine, whose cards are pinned in
// its own unit and not here), a pin that is not a literal device list (a macro), an alias two
// entries claim, and a bare index when the entry does not also set CUDA_DEVICE_ORDER=PCI_BUS_ID
// (without it a CUDA index is not an nvidia-smi index, and reading it as one could place a seat on
// the wrong card) are all UNKNOWN, with the reason. Device flags in cmd are never read.

// ServingPin is one model's placement as the serving config states it: its pin elements, or why it
// has none the harness can use.
type ServingPin struct {
	Pins []string
	Why  string
}

var pciOrderEntry = regexp.MustCompile(`(?i)^\s*CUDA_DEVICE_ORDER\s*=\s*["']?PCI_BUS_ID["']?\s*$`)

// ServingPins reads the llama-swap config at path and returns the pin of every model id and alias
// (lower-cased). An error means no model is placed by it.
func ServingPins(path string) (map[string]ServingPin, error) {
	pins, _, err := parseServing(path)
	return pins, err
}

// parseServing is ServingPins plus the model ids the config declares (sorted, aliases not included).
func parseServing(path string) (map[string]ServingPin, []string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil, fmt.Errorf("serving_config_path is unset")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var doc struct {
		Models map[string]struct {
			Aliases []string `yaml:"aliases"`
			Env     []string `yaml:"env"`
			Proxy   string   `yaml:"proxy"`
		} `yaml:"models"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, nil, fmt.Errorf("not parseable YAML: %w", err)
	}
	out := map[string]ServingPin{}
	ids := make([]string, 0, len(doc.Models))
	for id := range doc.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		m := doc.Models[id]
		pin := servingPinOf(m.Env, m.Proxy)
		for _, name := range append([]string{id}, m.Aliases...) {
			key := strings.ToLower(strings.TrimSpace(name))
			if key == "" {
				continue
			}
			if prev, dup := out[key]; dup && !sameServingPin(prev, pin) {
				out[key] = ServingPin{Why: "two entries of the serving config claim the name with different pins"}
				continue
			}
			out[key] = pin
		}
	}
	return out, ids, nil
}

func sameServingPin(a, b ServingPin) bool {
	return a.Why == b.Why && strings.Join(a.Pins, ",") == strings.Join(b.Pins, ",")
}

func servingPinOf(env []string, proxy string) ServingPin {
	if strings.TrimSpace(proxy) != "" {
		return ServingPin{Why: "it is a proxy: seat, whose cards are pinned in its own engine and not in the serving config"}
	}
	value, found, pci := "", false, false
	for _, e := range env {
		if pciOrderEntry.MatchString(e) {
			pci = true
		}
		if k, v, ok := strings.Cut(e, "="); ok && strings.TrimSpace(k) == "CUDA_VISIBLE_DEVICES" && !found {
			value, found = strings.Trim(strings.TrimSpace(v), `"'`), true
		}
	}
	if !found {
		return ServingPin{Why: "its serving config entry pins no CUDA_VISIBLE_DEVICES in env"}
	}
	if value == "" || strings.ContainsAny(value, "${}") {
		return ServingPin{Why: "its serving config pin is not a literal device list"}
	}
	var pins []string
	for _, el := range strings.Split(value, ",") {
		el = strings.TrimSpace(el)
		if el == "" {
			return ServingPin{Why: "its serving config pin has a blank element"}
		}
		if isIndex(el) && !pci {
			return ServingPin{Why: "its serving config pin is a CUDA index and the entry does not set CUDA_DEVICE_ORDER=PCI_BUS_ID, so it is not an nvidia-smi index"}
		}
		pins = append(pins, el)
	}
	return ServingPin{Pins: pins}
}

func isIndex(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// servingState is the serving config as one scope reads it: lazily, once, and only when a model
// is not declared by layers.
type servingState struct {
	once    sync.Once
	path    string
	pins    map[string]ServingPin
	problem string
}

func (st *servingState) load() {
	st.once.Do(func() {
		pins, err := ServingPins(st.path)
		if err != nil {
			if strings.TrimSpace(st.path) == "" {
				st.problem = "serving_config_path is unset, so the serving config places no model"
			} else {
				st.problem = fmt.Sprintf("the serving config could not be read (%v), so it places no model", err)
			}
			return
		}
		st.pins = pins
	})
}

// servingModelIDs lists the model ids of the serving config at path (aliases are not ids), for the
// candidate list of a lease whose llama-swap roster could not be read. Empty when it cannot be read.
func servingModelIDs(path string) []string {
	_, ids, err := parseServing(path)
	if err != nil {
		return nil
	}
	return ids
}
