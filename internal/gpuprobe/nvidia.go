// Package gpuprobe is the LEAF that owns the machine's live GPU and host-RAM
// readers: the nvidia-smi per-device query (its command line, its PATH
// fallback, its CSV parser) and the free-host-RAM reader. It imports nothing
// from the rest of the harness so that BOTH the fleet node's health sampler
// (internal/fleetnode) and the composite tier's placement guards
// (internal/placement) read the same numbers through the same parser — one
// parser, one set of pins, no second "nvidia-smi reader" that could disagree
// with the first about what a card's free memory is. ADR 0039 / plan Task 4.
package gpuprobe

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Device is one parsed nvidia-smi device line: the index/uuid/name nvidia-smi
// itself reports plus its GiB memory figures. JSON tags match the
// gpu_devices[] shape documented in docs/FLEET-NODE.md (fleetnode.GPUDevice is
// an alias of this type, so the health payload's wire shape is unchanged by
// the move). UUID is what primary_gpu_uuid — and the composite tier's
// display-card pin — match against: an index is only stable until the board
// reorders on a power loss, the UUID is burned into the card.
type Device struct {
	Index    int     `json:"index"`
	UUID     string  `json:"uuid"`
	Name     string  `json:"name"`
	TotalGiB float64 `json:"vram_total_gb"`
	FreeGiB  float64 `json:"vram_free_gb"`
	// UtilPct is nvidia-smi utilization.gpu (0-100) when the query carried it.
	// UtilKnown distinguishes "0 %" from "not queried" (a 5-field line from an
	// older launcher); consumers must never treat an unknown as idle.
	UtilPct   int  `json:"util_pct"`
	UtilKnown bool `json:"util_known"`
	// DisplayActive is nvidia-smi's display_active for this card: whether a
	// display is attached and being driven by it. It is the DIRECT answer to
	// "is this the operator's screen" — the property itself, not an inference
	// from which processes happen to be visible — and it costs no extra
	// nvidia-smi call because it rides the one per-device query every reader
	// already runs. False for a driver that does not report it and for a line
	// from an older launcher that lacks the column: absent is never "yes".
	DisplayActive bool `json:"display_active,omitempty"`
}

// smiQueryArgs is the ONE per-device query every reader in the harness runs
// (fleet-serve's 2 s health sampler and the placement guards alike): the uuid
// is there so a card can be pinned across reboots/reseats, utilization so a
// busy card can be told from an idle one.
var smiQueryArgs = []string{"--query-gpu=index,uuid,name,memory.total,memory.used,utilization.gpu,display_active", "--format=csv,noheader,nounits"}

// ParseSmiMemoryDevices parses `nvidia-smi --query-gpu=index,uuid,name,
// memory.total,memory.used[,utilization.gpu] --format=csv,noheader,nounits`
// output — one line per GPU, e.g. "0, GPU-1111aaaa-2222-3333-4444-555566667777,
// NVIDIA GeForce RTX 5060 Ti, 16311, 867" (5 fields) or with utilization:
// "0, GPU-1111aaaa-2222-3333-4444-555566667777, NVIDIA GeForce RTX 5060 Ti,
// 16311, 867, 37" (6 fields) — into an ordered []Device (nvidia-smi's own
// enumeration order, i.e. PCI bus order; NOT necessarily CUDA device order —
// see HeadlineDevice's doc comment for why that distinction is the whole bug
// this parser exists to fix, and fleetnode.SelectHeadlineDevice /
// primary_gpu_uuid for the deterministic override).
//
// CRLF and surrounding whitespace are tolerated (nvidia-smi emits \r\n on
// Windows). Blank lines and lines that don't parse as "index, uuid, name,
// total, used[, utilization]" are SKIPPED rather than failing the whole probe
// — a single garbled/partial row (a transient nvidia-smi hiccup) must not take
// down every other card's reading. A line whose total is <= 0, whose used is
// negative, or whose used EXCEEDS its total is likewise skipped (not a
// working GPU line — see the used>total check below for why this is a skip,
// not a clamp).
//
// The optional 6th field (utilization.gpu) is parsed when present. If the 6th
// field is malformed, out of range (not 0-100), or missing (5-field line), the
// device is STILL returned with UtilKnown=false and UtilPct=0 — the memory
// data is too valuable to lose over a transient utilization query failure or an
// older query format. Only the first five fields are mandatory for a valid
// device; the 6th is never required.
//
// If NO line parses into a valid device (memory fields), that IS an error: the
// fleet contract treats vram_total_gb <= 0 as a failed probe, so a would-be
// zero-device snapshot must never reach the caller as success — and a
// placement guard that received an empty list as success could admit onto a
// card it cannot see.
func ParseSmiMemoryDevices(out string) ([]Device, error) {
	var devices []Device
	for _, rawLine := range strings.Split(out, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		// 5 = an older launcher (no utilization), 6 = with utilization, 7 = with
		// display_active as well. A line outside that range is not this query's.
		if len(fields) < 5 || len(fields) > 7 {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			continue
		}
		uuid := strings.TrimSpace(fields[1])
		name := strings.TrimSpace(fields[2])
		totalMiB, err := strconv.ParseFloat(strings.TrimSpace(fields[3]), 64)
		if err != nil {
			continue
		}
		usedMiB, err := strconv.ParseFloat(strings.TrimSpace(fields[4]), 64)
		if err != nil {
			continue
		}
		if totalMiB <= 0 || usedMiB < 0 {
			continue
		}
		// used > total is a corrupt reading (a broken driver, a query that raced
		// a hot-unplug, garbled counter values), not a legitimate "over budget"
		// state nvidia-smi can actually report. The OLD behavior clamped this to
		// 0 free and published the device anyway — that hides a broken
		// driver/query behind a plausible-looking number (0 free reads as "this
		// card is full", not "this reading is garbage"), which is worse than
		// skipping it: skipping is honest about "this device's line was bad this
		// tick," and (per the file-level contract) the OTHER devices on the same
		// box still publish normally, while a fully bad probe still fails via the
		// "zero valid devices" check below.
		if usedMiB > totalMiB {
			continue
		}
		d := Device{
			Index:    idx,
			UUID:     uuid,
			Name:     name,
			TotalGiB: totalMiB / 1024,
			FreeGiB:  (totalMiB - usedMiB) / 1024,
		}
		if len(fields) >= 6 {
			u, err := strconv.Atoi(strings.TrimSpace(fields[5]))
			if err == nil && u >= 0 && u <= 100 {
				d.UtilPct, d.UtilKnown = u, true
			}
			// If the 6th field is malformed or out-of-range, keep the device
			// with UtilKnown=false; don't skip the whole device just for bad
			// utilization data.
		}
		if len(fields) >= 7 {
			// Only an exact "Enabled" is a yes. A driver that does not support the
			// field answers "[Not Supported]" and older ones "[N/A]"; both mean "we
			// do not know", which must never read as "this is the operator's screen"
			// — the whole point of excluding a display card is that we are SURE.
			d.DisplayActive = strings.EqualFold(strings.TrimSpace(fields[6]), "Enabled")
		}
		devices = append(devices, d)
	}
	if len(devices) == 0 {
		return nil, fmt.Errorf("nvidia-smi multi-device memory query: no valid GPU lines parsed (contract: vram_total_gb <= 0 = failed probe)")
	}
	return devices, nil
}

// HeadlineDevice picks which parsed device a single vram_total_gb/vram_free_gb
// pair describes. A single render job binds to exactly ONE CUDA device — so
// the admission-relevant number is the biggest device a job could actually
// land on, never an arbitrary enumeration index. That distinction is the
// whole bug this fixes: nvidia-smi enumerates in PCI bus order, which has no
// relationship to which device a CUDA app actually computes on
// (CUDA_DEVICE_ORDER=FASTEST_FIRST can bind cuda:0 to nvidia-smi index 1) —
// "index 0 wins" was an artifact of enumeration order dressed up as a
// measurement, and it silently mis-sizes admission whenever the compute card
// isn't index 0.
//
// Rule: the device with the LARGEST TotalGiB wins. An exact tie in total is
// broken by whichever has more FreeGiB right now (more headroom to admit a
// job against). This is deliberately NOT a sum across devices — summing free
// VRAM would let the dispatcher admit a job that no single card can actually
// hold, since a render binds to one device, not the fleet of them combined.
//
// Documented limitation: on a box with two near-identical-capacity cards
// (16311 MiB vs 16303 MiB total, an 8 MiB gap from per-SKU driver/firmware
// reserve, not a real capacity difference), this rule has no way to know
// which index a CUDA app's device-order policy will actually pick —
// nvidia-smi carries no such signal. It picks the device with more raw
// capacity, which is a defensible, deterministic, non-arbitrary choice, but
// is NOT guaranteed to be "the compute card" on near-twin hardware. The full
// per-device list exists precisely so a caller that needs to reason about
// this edge case (the placement guards, a smarter dispatcher) has the real
// numbers for every card, not just the headline guess. This is the FALLBACK
// rule — fleetnode.SelectHeadlineDevice applies it only when no
// primary_gpu_uuid is pinned (or the pinned UUID isn't present).
//
// devices must be non-empty — callers only reach this after a successful
// parse, which never returns an empty slice.
func HeadlineDevice(devices []Device) Device {
	head := devices[0]
	for _, d := range devices[1:] {
		if d.TotalGiB > head.TotalGiB || (d.TotalGiB == head.TotalGiB && d.FreeGiB > head.FreeGiB) {
			head = d
		}
	}
	return head
}

// FreeGiB finds one device by the key a config pins it with and returns its
// free memory. key is either a bare nvidia-smi index ("1") or a UUID prefix
// ("GPU-2a44210f" or the full UUID), compared case-insensitively — the Qube's
// live config pins the display card by UUID because the board reorders
// indices on power loss, while a plain box pins by index, and one lookup must
// serve both. The bool is the ONLY honest answer for "not found": a guard
// that read an absent card as 0 GiB would refuse every load, one that read it
// as unbounded would admit onto a card it cannot see, so the caller must
// treat !ok as "refuse" (the guards fail closed). An empty key, an index no
// device carries, or a UUID prefix that matches MORE than one card (ambiguous
// — "GPU-" matches every card) is !ok.
// IndexOf resolves a device key — a CUDA index or a GPU-UUID prefix — to the
// CUDA index nvidia-smi reports for it. Ambiguous (a prefix matching two
// cards) or absent = !ok, so a caller refuses rather than guessing which card
// an operator's UUID meant. It exists because a display card is pinned by
// UUID on a board that reorders indices on power loss, while a seat is pinned
// by index: the two must be compared in one space.
func IndexOf(devs []Device, key string) (string, bool) {
	key = strings.TrimSpace(key)
	if key == "" || len(devs) == 0 {
		return "", false
	}
	if idx, err := strconv.Atoi(key); err == nil {
		for _, d := range devs {
			if d.Index == idx {
				return key, true
			}
		}
		return "", false
	}
	lk := strings.ToLower(key)
	found, n := Device{}, 0
	for _, d := range devs {
		if strings.HasPrefix(strings.ToLower(d.UUID), lk) {
			found, n = d, n+1
		}
	}
	if n != 1 {
		return "", false
	}
	return strconv.Itoa(found.Index), true
}

func FreeGiB(devs []Device, key string) (float64, bool) {
	key = strings.TrimSpace(key)
	if key == "" || len(devs) == 0 {
		return 0, false
	}
	if idx, err := strconv.Atoi(key); err == nil {
		for _, d := range devs {
			if d.Index == idx {
				return d.FreeGiB, true
			}
		}
		return 0, false
	}
	lk := strings.ToLower(key)
	found, n := Device{}, 0
	for _, d := range devs {
		if strings.HasPrefix(strings.ToLower(d.UUID), lk) {
			found, n = d, n+1
		}
	}
	if n != 1 {
		return 0, false
	}
	return found.FreeGiB, true
}

// NvidiaSmiRunner returns the function that shells the per-device query —
// the exact command fleet-serve's health sampler has always run — resolving
// nvidia-smi from PATH first and, on Windows, falling back to
// C:\Windows\System32\nvidia-smi.exe: a service/task principal (the fleet
// node runs as a scheduled task) can carry a PATH without the driver's
// directory even though the binary is installed, and "nvidia-smi not found"
// on a box with three NVIDIA cards would fail every display-card guard
// closed forever. The lookup happens per call, not at construction, so a
// driver install after the harness started is picked up.
func NvidiaSmiRunner() func() (string, error) {
	return func() (string, error) {
		bin, err := nvidiaSmiPath()
		if err != nil {
			return "", err
		}
		out, err := exec.Command(bin, smiQueryArgs...).Output()
		return string(out), err
	}
}

// nvidiaSmiPath resolves the binary: PATH, then the Windows driver drop.
func nvidiaSmiPath() (string, error) {
	if p, err := exec.LookPath("nvidia-smi"); err == nil {
		return p, nil
	}
	if runtime.GOOS == "windows" {
		fallback := filepath.Join(`C:\Windows\System32`, "nvidia-smi.exe")
		if p, err := exec.LookPath(fallback); err == nil {
			return p, nil
		}
	}
	return "", errors.New("nvidia-smi: not on PATH" + windowsFallbackHint())
}

func windowsFallbackHint() string {
	if runtime.GOOS == "windows" {
		return ` and not at C:\Windows\System32\nvidia-smi.exe`
	}
	return ""
}

// errExecFailed is the sentinel a test injects for "the runner itself failed".
var errExecFailed = errors.New("gpuprobe: nvidia-smi exec failed")

// Read runs the live nvidia-smi query and parses it — the one call a
// placement Snapshot or a health tick makes to learn every card's free
// memory. ctx bounds the exec so a wedged driver (nvidia-smi can hang for
// tens of seconds during a device reset) cannot stall an admission decision
// past the caller's deadline; a hang, a missing binary, or a parse of zero
// valid devices all return an error, never an empty success.
func Read(ctx context.Context) ([]Device, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return ReadWith(func() (string, error) {
		bin, err := nvidiaSmiPath()
		if err != nil {
			return "", err
		}
		out, err := exec.CommandContext(ctx, bin, smiQueryArgs...).Output()
		if err != nil && ctx.Err() != nil {
			return "", fmt.Errorf("nvidia-smi: %w", ctx.Err())
		}
		return string(out), err
	})
}

// ReadWith is Read over an injected runner (a recorded nvidia-smi output in
// tests, a remote/cached reader in a sampler) so every consumer of the parse
// exercises the SAME error path: a failed runner is an error, never an empty
// device list that a guard could mistake for "no card is busy".
func ReadWith(run func() (string, error)) ([]Device, error) {
	if run == nil {
		return nil, errors.New("gpuprobe: nil nvidia-smi runner")
	}
	out, err := run()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	return ParseSmiMemoryDevices(out)
}
