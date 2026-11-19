package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpucards"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// writeCardRefsSection prints the settings that name a card by UUID which no live card has, and returns how many
// there are: the number that makes doctor exit non-zero, like every other value that loads and then cannot do what
// it says (the config findings).
//
// A replaced card leaves its UUID behind in gpu_comfy_order, primary_gpu_uuid, a layer's pins and the llama-swap
// seats' CUDA_VISIBLE_DEVICES (serving_config_path). Each reader degrades by itself and quietly: the ComfyUI order
// is rejected with one note line and card-scoped media leases become whole-node holds, and a seat whose only pin is
// the missing UUID sees no CUDA device and loads on the CPU. Doctor is where an operator goes to find out what is
// wrong, so it names each stale UUID with the setting that holds it, what that setting does meanwhile, the live
// cards the stale settings do not name (the replacement candidates) and the fix.
//
//	card refs:  FAIL  gpu_comfy_order names GPU-..., which no live card has: ComfyUI's device order is not declared, ...
//	card refs:        live cards the stale settings do not name (replacement candidates): idx 1 <name> GPU-...
//	card refs:        fix: replace each stale UUID ...
//	card refs:  UNKNOWN  ... could not be checked against the live cards (<why>); none is called stale
//
// It is silent on a config that names no card by UUID (and then reads no GPU), and silent when every name resolves,
// so a green doctor stays as short as it is today. A card table that cannot be read is UNKNOWN and never FAIL:
// unknown is not absent. UNKNOWN does not change doctor's exit code. The one GPU read goes through doctorGPURead.
func writeCardRefsSection(w io.Writer, cfg config.Config) int {
	refs, unchecked := cfg.CardRefs()
	var cards []gpuprobe.Card
	var cerr error
	if len(refs) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var devs []gpuprobe.Device
		if devs, cerr = doctorGPURead(ctx); cerr == nil {
			cards, _ = gpuprobe.BuildCards(devs, "")
		}
	}
	rep := gpuprobe.CheckRefs(cards, cerr, refs, unchecked)
	const pad = "card refs:        "
	switch rep.State {
	case gpuprobe.RefsStale:
		for _, s := range rep.Stale {
			fmt.Fprintf(w, "card refs:  FAIL  %s\n", gpucards.StaleText(s))
		}
		if c := gpucards.CandidatesText(rep); c != "" {
			fmt.Fprintf(w, "%s%s\n", pad, c)
		}
		fmt.Fprintf(w, "%sfix: %s\n", pad, rep.Fix)
	case gpuprobe.RefsUnknown:
		fmt.Fprintf(w, "card refs:  UNKNOWN  %s\n", gpucards.UnknownText(rep))
	}
	for _, u := range rep.Unchecked {
		fmt.Fprintf(w, "card refs:  UNKNOWN  not checked: %s\n", u)
	}
	return len(rep.Stale)
}
