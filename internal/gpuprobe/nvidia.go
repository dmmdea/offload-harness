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
	"log"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
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
	// DisplayAttached is nvidia-smi's display_attached: a physical monitor is
	// connected to one of this card's connectors. It is the signal that holds at the
	// desk, because display_active reads Disabled on every card of the 3-card box
	// unless a display is initialised (a game, a lit screen) while display_attached
	// stays Yes on the card that drives the monitor (measured 2026-10-03). Absent is
	// never "yes": a driver without the field, and a line from the older query,
	// leave it false. Read it through DrivesDisplay, never on its own.
	DisplayAttached bool `json:"display_attached,omitempty"`
	// AttachedUnknown is set on every device of a reading that carries no display_attached
	// because the full query FAILED for a reason that does not name the field (a transient):
	// the reading cannot say which card the monitor is on. A driver that refuses the field has
	// no such mark (the rule rests on display_active alone, as it always did).
	AttachedUnknown bool `json:"attached_unknown,omitempty"`
}

// DrivesDisplay is the ONE per-card answer to "is this the operator's screen":
// display_active Enabled OR display_attached Yes. Every reader of a Device asks it
// here (DisplayCardUUIDs, the card table, the delegator's free-card reading), so the
// rule cannot drift between surfaces.
func (d Device) DrivesDisplay() bool { return d.DisplayActive || d.DisplayAttached }

// smiQueryArgs is the ONE per-device query every reader in the harness runs
// (fleet-serve's 2 s health sampler and the placement guards alike): the uuid
// is there so a card can be pinned across reboots/reseats, utilization so a
// busy card can be told from an idle one, and the two display columns so the
// operator's screen can be told from a card the harness may place work on.
var smiQueryArgs = []string{"--query-gpu=index,uuid,name,memory.total,memory.used,utilization.gpu,display_active,display_attached", "--format=csv,noheader,nounits"}

// smiQueryArgsNoAttached is the query before display_attached was added. A driver
// that does not know the field (an older one, a headless Linux build) refuses the
// whole query with a non-zero exit, so RunDisplayAware falls back to this one: the
// reader still answers, with display_active alone.
var smiQueryArgsNoAttached = []string{"--query-gpu=index,uuid,name,memory.total,memory.used,utilization.gpu,display_active", "--format=csv,noheader,nounits"}

// attachedRetryEvery is how long the full query is skipped after the driver refused it
// and the fallback worked. The 2 s health sampler would otherwise pay a doomed
// nvidia-smi call every tick on an old driver; after the window the full query is tried
// again, so a driver upgrade is picked up and a wrong guess is never a permanent downgrade.
const attachedRetryEvery = 10 * time.Minute

// attachedRefusalLimit is how many failures of the full query IN A ROW, each followed by a
// fallback that worked, are taken as a refusal even though no failure named the field (a
// driver whose refusal reads differently). Fewer than that are transients, remembered by
// nobody.
const attachedRefusalLimit = 3

// smiClock is the clock the fallback window reads; a test injects its own.
var smiClock = time.Now

// smiLogf is where the one line about an armed downgrade goes; a test captures it.
var smiLogf = log.Printf

// attachedGate remembers that the driver does not give display_attached: the window in which
// the full query is skipped, and the unexplained failures counted toward arming it.
var attachedGate struct {
	mu        sync.Mutex
	skipUntil time.Time
	fails     int
}

func resetAttachedGate() {
	attachedGate.mu.Lock()
	attachedGate.skipUntil = time.Time{}
	attachedGate.fails = 0
	attachedGate.mu.Unlock()
}

// ResetDisplayAwareState forgets that a driver refused display_attached, so the next
// query tries the full one again. A test seam for packages whose tests stub nvidia-smi;
// production never calls it.
func ResetDisplayAwareState() { resetAttachedGate() }

// AttachedState says what one reading knows about display_attached.
type AttachedState int

const (
	// AttachedRead: the full query ran, and the output carries display_attached.
	AttachedRead AttachedState = iota
	// AttachedUnsupported: the driver does not give the field (it refused it, or has failed
	// the full query so often that it is treated as refusing). The display rule rests on
	// display_active alone, as it did before the field was read.
	AttachedUnsupported
	// AttachedUnknown: the full query failed once for a reason that does not name the field.
	// This reading carries no display_attached, and it is NOT because the driver lacks it, so
	// the card the monitor is plugged into cannot be told from the others by it.
	AttachedUnknown
)

// refusesAttached reports whether a failed full query is the driver saying it does not know
// display_attached, as opposed to nvidia-smi failing for some other reason. nvidia-smi names the
// field (`Field "display_attached" is not a valid field to query.`, on STDOUT, status 2), so the
// output, the error text and an ExitError's stderr are all read.
func refusesAttached(out string, err error) bool {
	text := strings.ToLower(out)
	if err != nil {
		text += " " + strings.ToLower(err.Error())
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			text += " " + strings.ToLower(string(ee.Stderr))
		}
	}
	if !strings.Contains(text, "display_attached") {
		return false
	}
	for _, w := range []string{"not a valid", "invalid", "unknown field", "unrecognized", "unrecognised", "unsupported"} {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}

// RunDisplayAware runs the per-device query with display_attached and, when the driver
// refuses it, the same query without. See RunDisplayAwareReport; this is the form for a
// caller that does not need to know which of the two it got.
func RunDisplayAware(run func(withAttached bool) (string, error)) (string, error) {
	out, _, err := RunDisplayAwareReport(run)
	return out, err
}

// RunDisplayAwareReport is RunDisplayAware that also says what the reading knows about
// display_attached. run(true) must run the full query and run(false) the fallback; both readers
// of the card table (this package's Read and the lease verdict's sampler in gpuactivity) go
// through here so they degrade the same way.
//
// WHAT ARMS THE DEGRADE. Only a failure of the full query that names the field (the driver
// refusing it), or attachedRefusalLimit unexplained failures in a row, makes the full query be
// skipped for attachedRetryEvery, and the downgrade is logged once when it is armed. Any other
// failure answers THIS call from the fallback and is remembered by nobody: display_attached is
// the only signal that marks the monitor's card while the screen sleeps, so one transient
// nvidia-smi error must not cost ten minutes of it. That one reading says so
// (AttachedUnknown). When both queries fail nvidia-smi itself is the problem: the full
// query's error is returned and nothing is remembered.
func RunDisplayAwareReport(run func(withAttached bool) (string, error)) (string, AttachedState, error) {
	attachedGate.mu.Lock()
	skip := smiClock().Before(attachedGate.skipUntil)
	attachedGate.mu.Unlock()
	if skip {
		out, err := run(false)
		return out, AttachedUnsupported, err
	}
	out, err := run(true)
	if err == nil {
		attachedGate.mu.Lock()
		attachedGate.fails = 0
		attachedGate.mu.Unlock()
		return out, AttachedRead, nil
	}
	legacy, lerr := run(false)
	if lerr != nil {
		return "", AttachedRead, err
	}
	var why string
	attachedGate.mu.Lock()
	switch {
	case refusesAttached(out, err):
		why = "the driver refused the field"
	default:
		attachedGate.fails++
		if attachedGate.fails >= attachedRefusalLimit {
			why = fmt.Sprintf("the full query failed %d times in a row (%v)", attachedGate.fails, err)
		}
	}
	armed := why != ""
	if armed {
		attachedGate.skipUntil = smiClock().Add(attachedRetryEvery)
		attachedGate.fails = 0
	}
	attachedGate.mu.Unlock()
	if armed {
		smiLogf("gpuprobe: nvidia-smi display_attached is not being read: %s. The display rule rests on display_active alone for %s, then the full query is tried again", why, attachedRetryEvery)
		return legacy, AttachedUnsupported, nil
	}
	return legacy, AttachedUnknown, nil
}

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
		// display_active as well, 8 = with display_attached. A line outside that
		// range is not this query's.
		if len(fields) < 5 || len(fields) > 8 {
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
		if len(fields) >= 8 {
			// Same discipline for display_attached: only an exact "Yes" is a yes.
			d.DisplayAttached = strings.EqualFold(strings.TrimSpace(fields[7]), "Yes")
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
// ("GPU-8888bbbb" or the full UUID), compared case-insensitively — <node-b>'s
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
		return RunDisplayAware(func(withAttached bool) (string, error) {
			out, err := exec.Command(bin, smiArgs(withAttached)...).Output()
			return string(out), err
		})
	}
}

// smiArgs picks the per-device query: the full one, or the one without display_attached.
func smiArgs(withAttached bool) []string {
	if withAttached {
		return smiQueryArgs
	}
	return smiQueryArgsNoAttached
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
	bin, err := nvidiaSmiPath()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	return readDisplayAware(func(withAttached bool) (string, error) {
		out, err := exec.CommandContext(ctx, bin, smiArgs(withAttached)...).Output()
		if err != nil && ctx.Err() != nil {
			return "", fmt.Errorf("nvidia-smi: %w", ctx.Err())
		}
		return string(out), err
	})
}

// readDisplayAware is Read over an injected per-device runner (run(true) the full query,
// run(false) the one without display_attached). A reading taken from the fallback after the full
// query failed for a reason that does not name the field cannot say which card the monitor is
// on, so every device of it is marked AttachedUnknown (see RunDisplayAwareReport).
func readDisplayAware(run func(withAttached bool) (string, error)) ([]Device, error) {
	var state AttachedState
	devs, err := ReadWith(func() (string, error) {
		out, st, err := RunDisplayAwareReport(run)
		state = st
		return out, err
	})
	if err == nil && state == AttachedUnknown {
		for i := range devs {
			devs[i].AttachedUnknown = true
		}
	}
	return devs, err
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
