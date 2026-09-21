package gpuactivity

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
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
	// gpuprobe.DisplayCardUUIDs, the one rule both surfaces read.
	DisplayActive bool `json:"display_active,omitempty"`
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
	out, err := smiRun(sctx, "--query-gpu=index,uuid,name,utilization.gpu,memory.used,memory.total,display_active", "--format=csv,noheader,nounits")
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
