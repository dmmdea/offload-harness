package gpulease

// The default card set of a wrapped command (plan P3): `gpu reserve -- <cmd>` with no
// --devices reads what the command says about its own cards, so a ComfyUI job that pins
// itself to one card holds one card and the rest of the box keeps working.
//
// Evidence, strongest first:
//
//  1. CUDA_VISIBLE_DEVICES in the environment the command inherits: the process can see
//     nothing else, so it bounds everything below. Entries are GPU UUIDs (direct) or bare
//     indices, which CUDA counts in nvidia-smi's PCI order when CUDA_DEVICE_ORDER is
//     PCI_BUS_ID and in FASTEST_FIRST order otherwise.
//  2. `--cuda-device N` (or `--cuda-device=N`) on the command line: ComfyUI's flag, in
//     ComfyUI's (FASTEST_FIRST) order.
//  3. COMFY_CUDA_DEVICE in the environment: the harness's own render-runner pin, the same
//     order.
//
// No evidence is not an error: the command may use any card, so the lease is whole-node.
// Evidence that cannot be turned into a card (an index whose order the card table does
// not know, a UUID no card carries) IS an error, ErrDeviceUnresolved, and the caller
// decides what to do about it; this function never guesses a card, because a lease on the
// wrong card fences the wrong work and leaves the right card double-booked.

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// ErrDeviceUnresolved wraps every refusal to turn what a command says into a card set.
var ErrDeviceUnresolved = errors.New("gpulease: the command's card selection cannot be resolved to cards")

// Derived is the card set a command's own text names.
type Derived struct {
	// IDs are lease ids (lower-cased UUIDs); empty = the command names no card.
	IDs []string
	// Source says where the evidence came from; empty when IDs is.
	Source string
}

// DevicesFromCommand derives the card set from the command line and its environment
// against the card table. env reads one variable ("" when unset).
func DevicesFromCommand(args []string, env func(string) string, cards []gpuprobe.Card) (Derived, error) {
	if raw := strings.TrimSpace(env("CUDA_VISIBLE_DEVICES")); raw != "" && raw != "-1" {
		ids, err := fromVisibleDevices(raw, strings.ToUpper(strings.TrimSpace(env("CUDA_DEVICE_ORDER"))) == "PCI_BUS_ID", cards)
		if err != nil {
			return Derived{}, err
		}
		return Derived{IDs: ids, Source: "CUDA_VISIBLE_DEVICES"}, nil
	}
	if raw, ok := cudaDeviceFlag(args); ok {
		ids, err := fromComfyOrder(raw, "--cuda-device", cards)
		if err != nil {
			return Derived{}, err
		}
		return Derived{IDs: ids, Source: "--cuda-device"}, nil
	}
	if raw := strings.TrimSpace(env("COMFY_CUDA_DEVICE")); raw != "" {
		ids, err := fromComfyOrder(raw, "COMFY_CUDA_DEVICE", cards)
		if err != nil {
			return Derived{}, err
		}
		return Derived{IDs: ids, Source: "COMFY_CUDA_DEVICE"}, nil
	}
	return Derived{}, nil
}

// WouldDerive reports whether the command says anything about its cards at all, so a
// caller that cannot read the card table can still tell "no evidence" from "evidence I
// cannot resolve".
func WouldDerive(args []string, env func(string) string) bool {
	if raw := strings.TrimSpace(env("CUDA_VISIBLE_DEVICES")); raw != "" && raw != "-1" {
		return true
	}
	if _, ok := cudaDeviceFlag(args); ok {
		return true
	}
	return strings.TrimSpace(env("COMFY_CUDA_DEVICE")) != ""
}

// cudaDeviceFlag finds ComfyUI's `--cuda-device` value among the arguments.
func cudaDeviceFlag(args []string) (string, bool) {
	for i, a := range args {
		if a == "--cuda-device" && i+1 < len(args) {
			return args[i+1], true
		}
		if v, ok := strings.CutPrefix(a, "--cuda-device="); ok {
			return v, true
		}
	}
	return "", false
}

func fromVisibleDevices(raw string, pci bool, cards []gpuprobe.Card) ([]string, error) {
	var ids []string
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			return nil, fmt.Errorf("%w: CUDA_VISIBLE_DEVICES=%q has an empty entry", ErrDeviceUnresolved, raw)
		}
		if n, err := strconv.Atoi(tok); err == nil {
			c, err := cardAtIndex(n, pci, "CUDA_VISIBLE_DEVICES", cards)
			if err != nil {
				return nil, err
			}
			ids = append(ids, c.LeaseID())
			continue
		}
		got, err := gpuprobe.ResolveCards(cards, []string{tok})
		if err != nil {
			return nil, fmt.Errorf("%w: CUDA_VISIBLE_DEVICES entry %q: %v", ErrDeviceUnresolved, tok, err)
		}
		ids = append(ids, got[0].LeaseID())
	}
	return dedupeIDs(ids), nil
}

func fromComfyOrder(raw, source string, cards []gpuprobe.Card) ([]string, error) {
	var ids []string
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		n, err := strconv.Atoi(tok)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%w: %s=%q is not a list of device indices", ErrDeviceUnresolved, source, raw)
		}
		c, err := cardAtIndex(n, false, source, cards)
		if err != nil {
			return nil, err
		}
		ids = append(ids, c.LeaseID())
	}
	return dedupeIDs(ids), nil
}

// cardAtIndex resolves a bare index. pci: nvidia-smi's order; otherwise CUDA's
// FASTEST_FIRST, which nvidia-smi cannot report, so it needs the declared order.
func cardAtIndex(n int, pci bool, source string, cards []gpuprobe.Card) (gpuprobe.Card, error) {
	if pci {
		for _, c := range cards {
			if c.NvidiaIndex == n {
				return c, nil
			}
		}
		return gpuprobe.Card{}, fmt.Errorf("%w: %s index %d (PCI order) matches no card", ErrDeviceUnresolved, source, n)
	}
	if c, ok := gpuprobe.CardByComfyOrder(cards, n); ok {
		return c, nil
	}
	known := false
	for _, c := range cards {
		if c.ComfyOrder >= 0 {
			known = true
		}
	}
	if !known {
		return gpuprobe.Card{}, fmt.Errorf("%w: %s names ComfyUI/CUDA device %d, but nvidia-smi does not report that order and this box has not declared it: "+
			"set gpu_comfy_order (every card, fastest first, as nvidia-smi indices or UUID prefixes) or name the cards with --devices", ErrDeviceUnresolved, source, n)
	}
	return gpuprobe.Card{}, fmt.Errorf("%w: %s names ComfyUI/CUDA device %d, which no card has", ErrDeviceUnresolved, source, n)
}

func dedupeIDs(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
