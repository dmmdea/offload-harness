package gpulease

// GPU-F9: a command that pins itself to a card its lease does not hold is MOVED onto the leased card, never refused.
//
// Two variables are the PROGRAM's own pin (the render layer reads them, and ComfyUI resets CUDA_VISIBLE_DEVICES from the
// card they name), so the wrapper cannot confine a command that carries one by setting CUDA_VISIBLE_DEVICES:
//
//   - COMFY_CUDA_DEVICE, a device index (or a comma list of them), counted in ComfyUI's FASTEST_FIRST order unless the
//     command's environment says CUDA_DEVICE_ORDER=PCI_BUS_ID, which makes it nvidia-smi's;
//   - COMFY_CARD_UUID, a driver GPU uuid, which names a keyed per-card ComfyUI instance and is never an index.
//
// The two are not interchangeable: the render layer refuses an index beside a card uuid (COMFY-INSTANCE-CONFLICT), and a
// uuid turns an unkeyed launch into a keyed one with its own port and directories. So a pin is rewritten in the KIND the
// program used: an index becomes the index of the held card, a uuid becomes the held card's uuid. When the held card's
// index is not known (the ComfyUI order is not declared) the index pin is NOT rewritten: a guess would land on the wrong
// card, and switching an index pin to a uuid would change the launch into another instance. The caller keeps its report.
//
// A pin of k cards under a lease of n >= k cards maps to the first k held cards in the order the index is counted in;
// k > n has no card to put the extra ones on and is left alone. The program's own argv (`--cuda-device N`) is never rewritten.
// A `--cuda-device` in COMFY_EXTRA_ARGS is not read here at all: the render layer drops it when COMFY_CUDA_DEVICE or
// COMFY_CARD_UUID is set (render/comfy-lifecycle.mjs launchFlags), so the environment pin this file rewrites is the one that wins.

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// Repin is one environment pin of a command that moves onto the cards its lease holds.
type Repin struct {
	// Var is the variable that is rewritten: "COMFY_CUDA_DEVICE" or "COMFY_CARD_UUID".
	Var string
	// Old is the value the command carried (whitespace trimmed); New is the value it starts with.
	Old, New string
	// From are the lease ids the old pin named, empty when it could not be resolved (see Unresolved).
	From []string
	// To are the held cards the new pin names.
	To []string
	// Unresolved is the reason the old pin named no card, when it named none.
	Unresolved error
}

// Env is the child environment entry for this re-pin.
func (r Repin) Env() string { return r.Var + "=" + r.New }

// RepinPlan is what PlanRepin decided about a command's own environment pins.
type RepinPlan struct {
	// Repins are the pins to rewrite, in the order the variables are listed above.
	Repins []Repin
	// NotMoved says, for each pin that is outside the lease but cannot be moved, why not.
	NotMoved []string
	// Inside lists the variables whose pin was read in the child's order and found inside the lease.
	Inside []string
}

// deviceListRE is the grammar the render layer accepts for COMFY_CUDA_DEVICE once whitespace is removed.
var deviceListRE = regexp.MustCompile(`^\d+(,\d+)*$`)

// ReadsPCIOrder reports whether a bare device index in this environment is nvidia-smi's (PCI bus) index: CUDA counts
// indices in FASTEST_FIRST order, which nvidia-smi cannot report, unless the environment says CUDA_DEVICE_ORDER=PCI_BUS_ID.
func ReadsPCIOrder(env func(string) string) bool {
	return strings.ToUpper(strings.TrimSpace(env("CUDA_DEVICE_ORDER"))) == "PCI_BUS_ID"
}

// HasCudaDeviceFlag reports whether the command line carries ComfyUI's `--cuda-device`. The program obeys its own argv,
// which the wrapper does not rewrite, so an environment pin beside one is not moved.
func HasCudaDeviceFlag(args []string) bool {
	_, ok := cudaDeviceFlag(args)
	return ok
}

// DriverUUID is a lease id as the driver prints the card's uuid. With no card table it is rebuilt: the driver's form is
// "GPU-" and the lower-case hex the lease id carries.
func DriverUUID(id string, cards []gpuprobe.Card) string {
	for _, c := range cards {
		if c.LeaseID() == id {
			return strings.TrimSpace(c.UUID)
		}
	}
	if rest, ok := strings.CutPrefix(id, "gpu-"); ok {
		return "GPU-" + rest
	}
	return id
}

// IsInside reports whether the plan found this variable's pin inside the lease.
func (p RepinPlan) IsInside(v string) bool {
	for _, x := range p.Inside {
		if x == v {
			return true
		}
	}
	return false
}

// PlanRepin decides which of the command's own environment pins sit outside held (or cannot be told to sit inside) and
// what each becomes. pci is the order a device index is read in by the child (ReadsPCIOrder, or true when the wrapper
// itself sets CUDA_DEVICE_ORDER=PCI_BUS_ID for the child). A blank value is no pin: a uuid-bound launch blanks
// COMFY_CUDA_DEVICE on purpose.
func PlanRepin(held []string, env func(string) string, cards []gpuprobe.Card, pci bool) RepinPlan {
	var plan RepinPlan
	if len(held) == 0 {
		return plan
	}
	if raw := strings.TrimSpace(env("COMFY_CARD_UUID")); raw != "" {
		r, ok, why := planUUIDRepin(raw, held, cards)
		switch {
		case ok:
			plan.Repins = append(plan.Repins, r)
		case why != "":
			plan.NotMoved = append(plan.NotMoved, why)
		default:
			plan.Inside = append(plan.Inside, "COMFY_CARD_UUID")
		}
	}
	if raw := strings.Join(strings.Fields(env("COMFY_CUDA_DEVICE")), ""); raw != "" {
		r, ok, why := planIndexRepin(raw, held, cards, pci)
		switch {
		case ok:
			plan.Repins = append(plan.Repins, r)
		case why != "":
			plan.NotMoved = append(plan.NotMoved, why)
		case deviceListRE.MatchString(raw):
			plan.Inside = append(plan.Inside, "COMFY_CUDA_DEVICE")
		}
	}
	return plan
}

func planUUIDRepin(raw string, held []string, cards []gpuprobe.Card) (Repin, bool, string) {
	r := Repin{Var: "COMFY_CARD_UUID", Old: raw}
	if containsLeaseID(held, strings.ToLower(raw)) {
		return Repin{}, false, "" // a lease id is the card's uuid lower-cased: inside, with or without a card table
	}
	if _, err := strconv.Atoi(raw); err == nil {
		r.Unresolved = fmt.Errorf("%w: COMFY_CARD_UUID=%q is an index, and an index counts cards in a different order than a lease does", ErrDeviceUnresolved, raw)
	} else if got, err := gpuprobe.ResolveCards(cards, []string{raw}); err != nil {
		r.Unresolved = fmt.Errorf("%w: COMFY_CARD_UUID=%q: %w", ErrDeviceUnresolved, raw, err)
	} else {
		r.From = []string{got[0].LeaseID()}
		if containsLeaseID(held, r.From[0]) {
			return Repin{}, false, "" // inside the lease: the command's own business
		}
	}
	target := orderHeld(held, cards, false)[0]
	r.To = []string{target}
	r.New = DriverUUID(target, cards)
	return r, true, ""
}

func planIndexRepin(raw string, held []string, cards []gpuprobe.Card, pci bool) (Repin, bool, string) {
	if !deviceListRE.MatchString(raw) {
		return Repin{}, false, "" // not a device index list: the program reports that itself
	}
	r := Repin{Var: "COMFY_CUDA_DEVICE", Old: raw}
	seen := map[int]bool{}
	k := 0
	inside := true
	for _, tok := range strings.Split(raw, ",") {
		n, _ := strconv.Atoi(tok)
		if seen[n] {
			continue
		}
		seen[n] = true
		k++
		c, err := cardAtIndex(n, pci, "COMFY_CUDA_DEVICE", cards)
		if err != nil {
			r.Unresolved = err
			inside = false
			continue
		}
		r.From = append(r.From, c.LeaseID())
		if !containsLeaseID(held, c.LeaseID()) {
			inside = false
		}
	}
	if inside {
		return Repin{}, false, ""
	}
	if r.Unresolved != nil {
		r.From = nil
	}
	if k > len(held) {
		return Repin{}, false, fmt.Sprintf("the pin names %d cards and the lease holds %d", k, len(held))
	}
	target := orderHeld(held, cards, pci)
	idx := make([]int, 0, k)
	for _, id := range target[:k] {
		n, ok := indexOfHeld(id, cards, pci)
		if !ok {
			return Repin{}, false, fmt.Sprintf("the held card %s has no known ComfyUI device index (set gpu_comfy_order), so the pin cannot be moved without a guess", id)
		}
		idx = append(idx, n)
		r.To = append(r.To, id)
	}
	sort.Ints(idx)
	parts := make([]string, len(idx))
	for i, n := range idx {
		parts[i] = strconv.Itoa(n)
	}
	r.New = strings.Join(parts, ",")
	return r, true, ""
}

// orderHeld lists the held lease ids in the order a device index is counted in (nvidia-smi's when pci, else ComfyUI's), so
// "the first k held cards" is the same cards the program would number first. When any held card's position is not
// known the lease's own order is kept.
func orderHeld(held []string, cards []gpuprobe.Card, pci bool) []string {
	out := append([]string(nil), held...)
	for _, id := range out {
		if _, ok := indexOfHeld(id, cards, pci); !ok {
			return out
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := indexOfHeld(out[i], cards, pci)
		b, _ := indexOfHeld(out[j], cards, pci)
		return a < b
	})
	return out
}

// indexOfHeld is the index a held card has in the order a device index is counted in.
func indexOfHeld(id string, cards []gpuprobe.Card, pci bool) (int, bool) {
	for _, c := range cards {
		if c.LeaseID() != id {
			continue
		}
		if pci {
			return c.NvidiaIndex, true
		}
		if c.ComfyOrder >= 0 {
			return c.ComfyOrder, true
		}
		return 0, false
	}
	return 0, false
}

func containsLeaseID(list []string, id string) bool {
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}
