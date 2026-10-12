package gpuprobe

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// The optional thermal query. temperature.gpu and the active clock event reasons are NOT columns of the
// per-device query every reader runs (smiQueryArgs): that query is shared by the 2 s health sampler and the
// placement guards, and a column an old driver refuses fails the whole thing. ReadThermal asks for them in a
// second query of its own, so a refusal costs this feature and nothing else.

// The throttle-reason field is called clocks_throttle_reasons.active on older drivers and clocks_event_reasons.active
// on newer ones (a current driver lists both names for the one field in `nvidia-smi --help-query-gpu`).
const (
	thermalFieldOld  = "clocks_throttle_reasons.active"
	thermalFieldNew  = "clocks_event_reasons.active"
	thermalTempField = "temperature.gpu"
)

// thermalVariants are the queries ReadThermal walks, in order: the older field name, the renamed one, and the
// temperature alone for a driver that has neither (the empty string).
var thermalVariants = []string{thermalFieldOld, thermalFieldNew, ""}

// A GPU core reading outside this range is a garbled counter, not a temperature (the same discipline as
// ParseSmiMemoryDevices skipping used > total): it is left out, not published.
const (
	minPlausibleC = 1
	maxPlausibleC = 150
)

// The clock event reason bits, as NVML defines them (nvml.h: nvmlClocksEventReason* and nvmlClocksThrottleReason*).
// GpuIdle (0x1) says the clocks dropped because nothing is running, which is not a limit on work, so it is never
// listed. Bits above the table are published as unknown:0x<bit> instead of being dropped, because the header has
// grown before (BoardLimit and Reliability are newer than the rest).
const throttleGpuIdle uint64 = 0x1

var throttleNames = map[uint64]string{
	0x2:   "applications_clocks_setting",
	0x4:   "sw_power_cap",
	0x8:   "hw_slowdown",
	0x10:  "sync_boost",
	0x20:  "sw_thermal_slowdown",
	0x40:  "hw_thermal_slowdown",
	0x80:  "hw_power_brake_slowdown",
	0x100: "display_clock_setting",
	0x200: "board_limit",
	0x400: "reliability",
}

// Thermal is one card's row of the thermal query, keyed by the card's UUID. A nil TemperatureC is "not read", never a
// value. ThrottleReasons has three states: nil is "not read" (the driver answered [N/A] or [Not Supported], or the query
// had no throttle column), a non-nil empty slice is "read, and nothing is limiting the clocks" (a mask of 0, or the idle
// bit alone), and a slice with names is the reasons active.
type Thermal struct {
	Index           int
	UUID            string
	TemperatureC    *int
	ThrottleReasons []string
}

// ThrottleReasonNames names the bits set in an NVML clock event reasons mask, lowest bit first. Idle is not
// listed; a bit this table does not know comes out as unknown:0x<bit>. A mask with nothing to say is nil.
func ThrottleReasonNames(mask uint64) []string {
	var out []string
	for b := uint(0); b < 64; b++ {
		bit := uint64(1) << b
		if mask&bit == 0 || bit == throttleGpuIdle {
			continue
		}
		if name, ok := throttleNames[bit]; ok {
			out = append(out, name)
		} else {
			out = append(out, fmt.Sprintf("unknown:%#x", bit))
		}
	}
	return out
}

// parseThrottleMask reads the hex mask nvidia-smi prints ("0x0000000000000060"). "[N/A]" and "[Not Supported]" are
// not a mask.
func parseThrottleMask(s string) (uint64, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 3 || (s[:2] != "0x" && s[:2] != "0X") {
		return 0, false
	}
	m, err := strconv.ParseUint(s[2:], 16, 64)
	if err != nil {
		return 0, false
	}
	return m, true
}

// ParseThermal parses `nvidia-smi --query-gpu=index,uuid,temperature.gpu[,<throttle field>]
// --format=csv,noheader,nounits`: one line per card, e.g. "0, GPU-1111aaaa-..., 83, 0x0000000000000060". A line that is
// not that shape is skipped, like ParseSmiMemoryDevices; a column the driver cannot give ("[N/A]",
// "[Not Supported]") leaves that value out of the row and keeps the other. No line parsing at all is an error: an
// empty success must never read as a cool node.
func ParseThermal(out string) ([]Thermal, error) {
	var rows []Thermal
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 3 || len(f) > 4 {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil {
			continue
		}
		uuid := strings.TrimSpace(f[1])
		if uuid == "" {
			continue
		}
		row := Thermal{Index: idx, UUID: uuid}
		if c, err := strconv.Atoi(strings.TrimSpace(f[2])); err == nil && c >= minPlausibleC && c <= maxPlausibleC {
			row.TemperatureC = &c
		}
		if len(f) == 4 {
			if mask, ok := parseThrottleMask(f[3]); ok {
				row.ThrottleReasons = ThrottleReasonNames(mask)
				if row.ThrottleReasons == nil {
					row.ThrottleReasons = []string{} // read, nothing active: not the same as not read
				}
			}
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, errors.New("nvidia-smi thermal query: no valid GPU lines parsed")
	}
	return rows, nil
}

// MergeThermal copies each row's values onto the device with the same UUID. It matches by UUID, never by index: an
// index is only stable until the board reorders, and a thermal figure on the wrong card is worse than none. A device
// no row names is left as it is, and so is a row no device names. The values are copied, so a device edited later
// cannot reach the rows a caller keeps.
func MergeThermal(devs []Device, rows []Thermal) {
	if len(rows) == 0 {
		return
	}
	byUUID := make(map[string]Thermal, len(rows))
	for _, r := range rows {
		if r.UUID != "" {
			byUUID[strings.ToLower(r.UUID)] = r
		}
	}
	for i := range devs {
		if devs[i].UUID == "" {
			continue
		}
		r, ok := byUUID[strings.ToLower(devs[i].UUID)]
		if !ok {
			continue
		}
		if r.TemperatureC != nil {
			v := *r.TemperatureC
			devs[i].TemperatureC = &v
		}
		if r.ThrottleReasons != nil {
			devs[i].ThrottleReasons = append([]string{}, r.ThrottleReasons...) // non-nil even when empty: read, none active
		}
	}
}

// thermalRun runs nvidia-smi with args and returns what it wrote to stdout. A test injects its own.
type thermalRun func(ctx context.Context, args []string) (string, error)

// thermalGate remembers which variant of the query the driver answered, so a driver that refuses the older field
// name costs one doomed spawn once, not one per read.
var thermalGate struct {
	mu    sync.Mutex
	known int // index into thermalVariants
}

// ResetThermalState forgets which variant of the thermal query worked. A test seam; production never calls it.
func ResetThermalState() {
	thermalGate.mu.Lock()
	thermalGate.known = 0
	thermalGate.mu.Unlock()
}

func thermalArgs(field string) []string {
	cols := "index,uuid," + thermalTempField
	if field != "" {
		cols += "," + field
	}
	return []string{"--query-gpu=" + cols, "--format=csv,noheader,nounits"}
}

// ReadThermal runs the thermal query and parses it. ctx bounds the exec, like Read. The driver's own naming decides
// the query (see thermalVariants); the one that worked is tried first on the next read. A failure that is not the
// driver refusing a field (a missing binary, a wedged driver) is returned at once and walks nothing, so a broken
// nvidia-smi is not spawned three times per attempt.
func ReadThermal(ctx context.Context) ([]Thermal, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	bin, err := nvidiaSmiPath()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	return readThermalWith(ctx, func(ctx context.Context, args []string) (string, error) {
		out, err := winexec.CommandContext(ctx, bin, args...).Output()
		if err != nil && ctx.Err() != nil {
			return "", fmt.Errorf("nvidia-smi: %w", ctx.Err())
		}
		return string(out), err
	})
}

// readThermalWith is ReadThermal over an injected runner.
func readThermalWith(ctx context.Context, run thermalRun) ([]Thermal, error) {
	if run == nil {
		return nil, errors.New("gpuprobe: nil nvidia-smi runner")
	}
	thermalGate.mu.Lock()
	first := thermalGate.known
	thermalGate.mu.Unlock()
	order := []int{first}
	for i := range thermalVariants {
		if i != first {
			order = append(order, i)
		}
	}
	var firstErr error
	for _, i := range order {
		field := thermalVariants[i]
		out, err := run(ctx, thermalArgs(field))
		if err == nil {
			rows, perr := ParseThermal(out)
			if perr == nil {
				thermalGate.mu.Lock()
				thermalGate.known = i
				thermalGate.mu.Unlock()
				return rows, nil
			}
			err = perr // an exit 0 that answered with the refusal text is still a refusal
		}
		if firstErr == nil {
			firstErr = err
		}
		if field == "" || !refusesField(out, err, field) {
			return nil, fmt.Errorf("nvidia-smi thermal query: %w", err)
		}
	}
	return nil, fmt.Errorf("nvidia-smi thermal query: no variant of the query was accepted: %w", firstErr)
}
