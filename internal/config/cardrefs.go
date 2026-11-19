package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/servingcfg"
)

// The settings that can hold a card's UUID, listed for gpuprobe.CheckRefs. When a card is replaced the UUID stays
// in the file and names nothing; each reader below then degrades its own way, and the Effect says how.
//
//   - gpu_comfy_order: comma list of nvidia-smi indices or UUID prefixes.
//   - primary_gpu_uuid: one whole UUID.
//   - layers[].display_device: an index, a UUID prefix, or "auto"; layers[].devices[] and layers[].seats[].device:
//     index lists that a box which re-enumerates its cards writes as UUIDs.
//   - the serving config's CUDA_VISIBLE_DEVICES (serving_config_path): the llama-swap seats' own pins.
//
// Plain indices are not UUIDs and are not judged here (comfy_cuda_device, the *_pool_* "cuda:N" keys and
// audiocpp_device hold only indices). A pin a macro writes is not a literal list to the serving config reader
// (internal/servingcfg) and is not read; `audit-yaml` expands macros and judges it.
const (
	effectComfyOrder = "ComfyUI's device order is not declared, so a command naming --cuda-device N (comfy_cuda_device) is reserved as the whole node and card-scoped media leases fall back to whole-node holds"
	effectPrimary    = "/fleet/health's headline VRAM falls back to the largest-total-VRAM card, so the node advertises another card's capacity"
	effectDisplay    = "the layer's display-card guards cannot resolve the card, so they refuse every placement on that layer (they fail closed)"
	effectLayerSeat  = "the seat's placed devices name no card, and the display-floor guard cannot show the seat to be off the display card, so it counts the seat as on it"
	effectLayerSet   = "documentary only (the seats carry the pins), but it names a card that is not there"
	effectServing    = "CUDA hides every device from the first pin it cannot resolve, so a seat pinned only to it sees no GPU and llama.cpp loads it on the CPU (host RAM); a seat that also pins a live card runs on fewer cards than it was sized for"
)

// uuidKeys picks the UUID-valued elements out of a comma list. loose accepts any element a card key can be (a UUID
// prefix with or without "GPU-", as the card table reads gpu_comfy_order and a layer pin); otherwise only an element
// that starts "GPU-" counts, which is how CUDA_VISIBLE_DEVICES spells one. Indices, "auto", -1 and MIG ids never do.
func uuidKeys(list string, loose bool) []string {
	var out []string
	for _, k := range strings.Split(list, ",") {
		k = strings.Trim(strings.TrimSpace(k), `"'`)
		if k == "" || k == "-1" || isCUDAIndex(k) || IsAutoDisplayDevice(k) {
			continue
		}
		if len(k) >= 4 && strings.EqualFold(k[:4], "MIG-") {
			continue
		}
		if !loose && !(len(k) >= 4 && strings.EqualFold(k[:4], "GPU-")) {
			continue
		}
		out = append(out, k)
	}
	return out
}

// CardRefs lists every place this config names a card by UUID. unchecked names what could have held one and
// could not be read (a serving_config_path that is set and does not read): those are unknown, never clean.
func (c Config) CardRefs() (refs []gpuprobe.Ref, unchecked []string) {
	for _, k := range uuidKeys(c.GPUComfyOrder, true) {
		refs = append(refs, gpuprobe.Ref{Setting: "gpu_comfy_order", Key: k, Effect: effectComfyOrder})
	}
	if k := strings.TrimSpace(c.PrimaryGPUUUID); k != "" {
		refs = append(refs, gpuprobe.Ref{Setting: "primary_gpu_uuid", Key: k, Effect: effectPrimary, Exact: true})
	}
	for _, l := range c.Layers {
		at := fmt.Sprintf("layers[%s]", l.Name)
		for _, k := range uuidKeys(l.DisplayDevice, true) {
			refs = append(refs, gpuprobe.Ref{Setting: at + ".display_device", Key: k, Effect: effectDisplay})
		}
		for _, d := range l.Devices {
			for _, k := range uuidKeys(d, true) {
				refs = append(refs, gpuprobe.Ref{Setting: at + ".devices", Key: k, Effect: effectLayerSet})
			}
		}
		for _, s := range l.Seats {
			for _, k := range uuidKeys(s.Device, true) {
				refs = append(refs, gpuprobe.Ref{Setting: fmt.Sprintf("%s.seats[%s].device", at, s.Role), Key: k, Effect: effectLayerSeat})
			}
		}
	}
	serving, bad := c.servingCardRefs()
	if bad != "" {
		unchecked = append(unchecked, bad)
	}
	return append(refs, serving...), unchecked
}

// servingCardRefs is the UUIDs the serving config's seats pin, one reference per distinct UUID with the seats that
// hold it (a node pins sixteen seats to one card; that is one defect, not sixteen). Unset: no reference and no
// complaint, the default. Set and unreadable: the reason, so the caller can say the seats were not checked.
func (c Config) servingCardRefs() ([]gpuprobe.Ref, string) {
	path := strings.TrimSpace(c.ServingConfigPath)
	if path == "" {
		return nil, ""
	}
	snap, err := servingcfg.Load(path)
	if err != nil {
		return nil, fmt.Sprintf("serving_config_path: seat pins not read (%v)", err)
	}
	var order []string
	seatsOf := map[string][]string{}
	for _, id := range snap.IDs() {
		p, ok := snap.Pin(id)
		if !ok {
			continue
		}
		for _, el := range p.Pins {
			for _, k := range uuidKeys(el, false) {
				lk := strings.ToLower(k)
				if _, seen := seatsOf[lk]; !seen {
					order = append(order, k)
				}
				seatsOf[lk] = append(seatsOf[lk], id)
			}
		}
	}
	refs := make([]gpuprobe.Ref, 0, len(order))
	for _, k := range order {
		seats := seatsOf[strings.ToLower(k)]
		sort.Strings(seats)
		refs = append(refs, gpuprobe.Ref{Setting: "serving_config_path seats' CUDA_VISIBLE_DEVICES", Key: k, Seats: seats, Effect: effectServing})
	}
	return refs, ""
}
