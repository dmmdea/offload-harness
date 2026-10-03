package gpuactivity

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// GPU is one card as nvidia-smi reports it at the moment of the sample.
type GPU struct {
	Index       int    `json:"index"`
	UUID        string `json:"uuid,omitempty"`
	Name        string `json:"name"`
	UtilPct     int    `json:"util_pct"`
	UtilKnown   bool   `json:"util_known"`
	MemUsedMiB  int    `json:"mem_used_mib"`
	MemTotalMiB int    `json:"mem_total_mib"`
	// DisplayActive is nvidia-smi's display_active: this card drives a screen,
	// so its utilization is never a lease holder's work — see
	// gpuprobe.DisplayCardUUIDs, the load-attribution rule every load surface reads.
	DisplayActive bool `json:"display_active,omitempty"`
	// DisplayAttached is nvidia-smi's display_attached: a monitor is plugged into this
	// card. It holds with the screen asleep, when display_active reads Disabled on every
	// card, so the card table reads it to keep the allocator off the monitor's card
	// (gpuprobe.ScreenCardUUIDs). The verdict's load attribution does NOT: an attached
	// monitor is not a display in use.
	DisplayAttached bool `json:"display_attached,omitempty"`
}

// GPUProcess is one process nvidia-smi lists on a card. On Windows (WDDM) the
// per-process memory is `[N/A]`; UsedKnown says so instead of reporting 0.
type GPUProcess struct {
	PID       int    `json:"pid"`
	Name      string `json:"name"`
	UsedMiB   int    `json:"used_mib,omitempty"`
	UsedKnown bool   `json:"used_known"`
	GPUUUID   string `json:"gpu_uuid,omitempty"`
}

// smiTimeout bounds one nvidia-smi invocation: a status call must never hang on
// a wedged driver.
const smiTimeout = 4 * time.Second

// smiRun is the nvidia-smi runner, a variable so tests inject output.
var smiRun = func(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "nvidia-smi", args...).Output()
	return string(out), err
}

// SampleGPUs queries every card's utilization and memory. An absent or failing
// nvidia-smi is an error the caller reports, never a row of zeros.
func SampleGPUs(ctx context.Context) ([]GPU, error) {
	sctx, cancel := context.WithTimeout(ctx, smiTimeout)
	defer cancel()
	// Through the shared fallback: a driver that does not know display_attached refuses the
	// whole query, and the sample is then taken without it (display_active alone, as before).
	out, err := gpuprobe.RunDisplayAware(func(withAttached bool) (string, error) {
		cols := "index,uuid,name,utilization.gpu,memory.used,memory.total,display_active"
		if withAttached {
			cols += ",display_attached"
		}
		return smiRun(sctx, "--query-gpu="+cols, "--format=csv,noheader,nounits")
	})
	if err != nil {
		return nil, err
	}
	return ParseGPUs(out), nil
}

// SampleProcesses lists the processes holding GPU memory.
func SampleProcesses(ctx context.Context) ([]GPUProcess, error) {
	sctx, cancel := context.WithTimeout(ctx, smiTimeout)
	defer cancel()
	// process_name LAST: a Windows path may itself contain ", ".
	out, err := smiRun(sctx, "--query-compute-apps=pid,used_memory,gpu_uuid,process_name", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, err
	}
	return ParseProcesses(out), nil
}

// ParseGPUs parses `index, uuid, name, utilization.gpu, memory.used, memory.total`
// rows. Rows that do not parse are skipped; `[N/A]` utilization is kept as
// unknown rather than zero (an unknown must never read as idle).
func ParseGPUs(out string) []GPU {
	var gpus []GPU
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) < 6 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		idx, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		used, uerr := strconv.Atoi(f[4])
		total, terr := strconv.Atoi(f[5])
		if uerr != nil || terr != nil || total <= 0 {
			continue
		}
		g := GPU{Index: idx, UUID: f[1], Name: f[2], MemUsedMiB: used, MemTotalMiB: total}
		if u, err := strconv.Atoi(f[3]); err == nil && u >= 0 && u <= 100 {
			g.UtilPct, g.UtilKnown = u, true
		}
		if len(f) >= 7 {
			// Only an exact "Enabled" is a yes: a driver that does not report the
			// field answers "[Not Supported]", which must never read as "this is the
			// operator's screen".
			g.DisplayActive = strings.EqualFold(f[6], "Enabled")
		}
		if len(f) >= 8 {
			g.DisplayAttached = strings.EqualFold(f[7], "Yes") // only an exact Yes
		}
		gpus = append(gpus, g)
	}
	return gpus
}

// ParseProcesses parses `pid, used_memory, gpu_uuid, process_name` rows.
func ParseProcesses(out string) []GPUProcess {
	var procs []GPUProcess
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.SplitN(line, ",", 4)
		if len(f) < 4 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		p := GPUProcess{PID: pid, GPUUUID: f[2], Name: f[3]}
		if m, merr := strconv.Atoi(f[1]); merr == nil {
			p.UsedMiB, p.UsedKnown = m, true
		}
		procs = append(procs, p)
	}
	return procs
}

// CardsReadingOf is the look at a lease's cards the term check takes
// (gpulease.TermSignals.Cards): any card of the lease, or any card at all for a whole-node lease
// (devices empty), at or above the verdict's busy threshold is CardsWorking. The display card is
// skipped (its load is the desktop's, never the lease's). Otherwise the cards are CardsIdle only
// when every card the lease is judged by was actually read and found quiet; a card whose
// utilisation is [N/A], a lease card the sample does not list, and a sample with no card to judge
// by at all are CardsUnreadable, because an unknown must never read as idle. devices are lease
// ids (lower-case GPU uuids).
func CardsReadingOf(gpus []GPU, devices []string) gpulease.CardsReading {
	devs := make([]gpuprobe.Device, 0, len(gpus))
	for _, g := range gpus {
		devs = append(devs, gpuprobe.Device{UUID: g.UUID, DisplayActive: g.DisplayActive})
	}
	display := gpuprobe.DisplayCardUUIDs(devs)
	var mine map[string]bool
	if len(devices) > 0 {
		mine = map[string]bool{}
		for _, d := range devices {
			mine[strings.ToLower(strings.TrimSpace(d))] = true
		}
	}
	read, unread := 0, false
	listed := map[string]bool{}
	for _, g := range gpus {
		id := strings.ToLower(g.UUID)
		listed[id] = true
		if mine != nil && !mine[id] {
			continue
		}
		if display[g.UUID] {
			continue
		}
		if !g.UtilKnown {
			unread = true
			continue
		}
		if g.UtilPct >= utilBusyPct {
			return gpulease.CardsWorking
		}
		read++
	}
	for d := range mine {
		if !listed[d] {
			unread = true
		}
	}
	if read == 0 || unread {
		return gpulease.CardsUnreadable
	}
	return gpulease.CardsIdle
}

// UtilWorking reports whether a lease's cards are busy: CardsReadingOf is CardsWorking. An
// unknown reading is not work, and neither is no reading.
func UtilWorking(gpus []GPU, devices []string) bool {
	return CardsReadingOf(gpus, devices) == gpulease.CardsWorking
}
