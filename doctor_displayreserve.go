package main

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// Seams for the machine facts the display-reserve row reads: the GPU reading and the OS. A test stands in for the box.
var (
	doctorGPURead = gpuprobe.Read
	doctorGOOS    = runtime.GOOS
)

// writeDisplayReserveRow prints one row when a card that drives a display is the card a bound ComfyUI lane runs on and the
// launch carries no --reserve-vram of its own. The runners hold back 1.0 GiB for the desktop, a desktop on a 16 GB card
// takes 2.5 to 5.4 GiB, and a generative edit then placed more than the card had free: the driver spilled into shared
// memory and sampling ran about 60 times slower (195-240 s/it against 3.5 s/it on a 16 GB tier box). The 16 GB tier seeds
// "--reserve-vram 3.0" into a FRESH config only, so an installed box whose card drives a desktop never got it.
//
//	reserve:    WARN  the ComfyUI card (<name>, idx N, X GiB) drives a display and no --reserve-vram is set; ...
//	reserve:    UNKNOWN  the ComfyUI card's display state cannot be read (<why>); ...
//
// It reads, in order and stopping at the first that does not hold, so the GPU probe runs last: a ComfyUI lane is bound
// (a configured non-prerequisite comfyui route); the effective extra args carry no --reserve-vram (imagegen_reserve_vram
// does not count: it reaches the image lane only); the box is Windows (the shared-memory spill is the WDDM behaviour that
// was measured; on another OS the row stays silent); and the card the lane runs on drives a display. The display state is
// read from the RAW nvidia-smi device (display_active or display_attached), not from the card table, which hides a sole
// display card on purpose. The lane's card is the one at comfy_cuda_device in ComfyUI's order (a comma list: any of
// them), else its default device, the fastest card. When that card or its display state cannot be read the row says so
// and does not warn. It is informational: it never changes doctor's exit code, and it needs no endpoint.
func writeDisplayReserveRow(w io.Writer, cfg config.Config, routes []mediacap.Route) {
	if !comfyLaneBound(routes) {
		return
	}
	if _, set := cfg.ComfyReserveVRAM(); set {
		return
	}
	if doctorGOOS != "windows" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	devs, err := doctorGPURead(ctx)
	if err != nil {
		fmt.Fprintf(w, "reserve:    UNKNOWN  the display state of the ComfyUI card cannot be read (%v), so whether it needs a --reserve-vram is not known\n", err)
		return
	}
	dev, why := comfyLaneDevice(cfg, devs)
	if dev == nil {
		fmt.Fprintf(w, "reserve:    UNKNOWN  the display state of the ComfyUI card cannot be read (%s), so whether it needs a --reserve-vram is not known\n", why)
		return
	}
	if !dev.DrivesDisplay() {
		if dev.AttachedUnknown {
			fmt.Fprintf(w, "reserve:    UNKNOWN  the display state of the ComfyUI card (%s, idx %d) cannot be read (nvidia-smi did not report display_attached), so whether it needs a --reserve-vram is not known\n", dev.Name, dev.Index)
		}
		return
	}
	fmt.Fprintf(w, "reserve:    WARN  the ComfyUI card (%s, idx %d, %.0f GiB) drives a display and no --reserve-vram is set; the runners hold back 1.0 GiB and a desktop takes 2.5-5.4 GiB, so the driver may spill into shared memory and sampling can run about 60 times slower. Set \"comfy_extra_args\": \"--reserve-vram 3.0\" (3.0 is what the 16 GB tier seeds on a fresh install; comfy_extra_args replaces the COMFY_EXTRA_ARGS env, so copy any flags that env carries into it first)\n",
		dev.Name, dev.Index, dev.TotalGiB)
}

// comfyLaneBound reports whether any ComfyUI lane of the box is bound: a configured comfyui route that is not the shared
// ComfyUI install prerequisite.
func comfyLaneBound(routes []mediacap.Route) bool {
	for _, r := range routes {
		if r.Engine == "comfyui" && !r.Prereq && r.State == mediacap.Configured {
			return true
		}
	}
	return false
}

// comfyLaneDevice finds the device the ComfyUI lanes run on: the one card of a one-card box, else the card(s) at
// comfy_cuda_device in ComfyUI's order (the fastest, position 0, when none is pinned). With several cards it needs
// gpu_comfy_order to know the positions. A device that drives a display wins over one that does not, so a pin list that
// reaches the display card is told. nil comes with the reason the card cannot be named.
func comfyLaneDevice(cfg config.Config, devs []gpuprobe.Device) (*gpuprobe.Device, string) {
	if len(devs) == 0 {
		return nil, "no card was reported"
	}
	if len(devs) == 1 {
		return &devs[0], ""
	}
	cards, warn := gpuprobe.BuildCards(devs, cfg.GPUComfyOrder)
	orders := []int{0}
	if pin := strings.TrimSpace(cfg.ComfyCudaDevice); pin != "" {
		orders = orders[:0]
		for _, p := range strings.Split(pin, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				return nil, "comfy_cuda_device is not a list of card positions"
			}
			orders = append(orders, n)
		}
	}
	var first *gpuprobe.Device
	for _, n := range orders {
		var found *gpuprobe.Device
		for _, c := range cards {
			if c.ComfyOrder != n {
				continue
			}
			for i := range devs {
				if devs[i].UUID == c.UUID {
					found = &devs[i]
				}
			}
		}
		if found == nil {
			if warn != "" {
				return nil, warn
			}
			return nil, "gpu_comfy_order does not name the card ComfyUI runs on"
		}
		if found.DrivesDisplay() {
			return found, ""
		}
		if first == nil {
			first = found
		}
	}
	return first, ""
}
