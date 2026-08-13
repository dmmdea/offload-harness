// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

// Package measure wraps the machine-level measurements the CLI's benchmark,
// VRAM, and fit commands need: per-GPU memory read from nvidia-smi, and the
// KV-cache arithmetic that turns a GGUF header into a VRAM estimate.
//
// Two rules are structural here, both from recorded scars:
//
//   - GPUs are identified by UUID, never by nvidia-smi's index. Indices are
//     unstable across reboots and are inverted relative to torch's ordering
//     on this box; a benchmark attributed to the wrong card is worse than no
//     benchmark. Operator-facing role labels ("fast-card", "utility-card")
//     are a presentation layer over the UUID, loaded from CLI config.
//   - Memory is reported as an explicit (baseline, after, delta) triple.
//     Reporting a card's total used memory as if it were one model's usage
//     is what fabricated a 15,924-vs-6,150 MiB constraint once already.
package measure

import (
	"context"
	"encoding/csv"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// GPU is one accelerator as nvidia-smi reports it.
type GPU struct {
	UUID     string `json:"uuid"`
	Name     string `json:"name"`
	Role     string `json:"role,omitempty"`
	UsedMiB  int    `json:"used_mib"`
	TotalMiB int    `json:"total_mib"`
}

// Label renders the operator-facing identity of a card: the configured role
// when one exists, otherwise the model name plus a short UUID suffix so two
// identical cards are still distinguishable.
func (g GPU) Label() string {
	if g.Role != "" {
		return g.Role
	}
	return fmt.Sprintf("%s (%s)", g.Name, ShortUUID(g.UUID))
}

// ShortUUID trims a GPU-xxxxxxxx-... UUID to its first field, which is
// unique in practice and fits a table column.
func ShortUUID(uuid string) string {
	s := strings.TrimPrefix(uuid, "GPU-")
	if i := strings.Index(s, "-"); i > 0 {
		return s[:i]
	}
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// Delta is the measured change in one card's used memory across an action.
// All three quantities are always present: a delta without its endpoints is
// unauditable, and an endpoint without its delta invites the total-as-usage
// misread.
type Delta struct {
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	Role        string `json:"role,omitempty"`
	BaselineMiB int    `json:"baseline_mib"`
	AfterMiB    int    `json:"after_mib"`
	DeltaMiB    int    `json:"delta_mib"`
	TotalMiB    int    `json:"total_mib"`
}

// SMIUnavailableError signals that nvidia-smi could not be run at all, as
// distinct from a card-level failure. Callers degrade to "VRAM unmeasured"
// rather than reporting zeros as measurements.
type SMIUnavailableError struct{ Err error }

func (e *SMIUnavailableError) Error() string {
	return fmt.Sprintf("nvidia-smi unavailable: %v", e.Err)
}
func (e *SMIUnavailableError) Unwrap() error { return e.Err }

// PerUUID returns every visible GPU keyed by UUID, with roles applied from
// the supplied uuid->role map (nil is fine).
func PerUUID(ctx context.Context, roles map[string]string) ([]GPU, error) {
	out, err := runSMI(ctx, "--query-gpu=uuid,name,memory.used,memory.total", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, err
	}
	return parseGPUCSV(out, roles)
}

func runSMI(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "nvidia-smi", args...)
	hideWindow(cmd)
	stdout, err := cmd.Output()
	if err != nil {
		return "", &SMIUnavailableError{Err: err}
	}
	return string(stdout), nil
}

func parseGPUCSV(raw string, roles map[string]string) ([]GPU, error) {
	r := csv.NewReader(strings.NewReader(strings.TrimSpace(raw)))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parsing nvidia-smi csv: %w", err)
	}
	var gpus []GPU
	for _, rec := range records {
		if len(rec) < 4 {
			continue
		}
		used, err1 := strconv.Atoi(strings.Fields(strings.TrimSpace(rec[2]))[0])
		total, err2 := strconv.Atoi(strings.Fields(strings.TrimSpace(rec[3]))[0])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("parsing nvidia-smi memory fields %q/%q", rec[2], rec[3])
		}
		uuid := strings.TrimSpace(rec[0])
		gpus = append(gpus, GPU{
			UUID:     uuid,
			Name:     strings.TrimSpace(rec[1]),
			Role:     roles[uuid],
			UsedMiB:  used,
			TotalMiB: total,
		})
	}
	if len(gpus) == 0 {
		return nil, fmt.Errorf("nvidia-smi returned no GPU rows")
	}
	// Stable UUID ordering: index order is exactly what this package refuses
	// to depend on, so do not inherit it as a display order either.
	sort.Slice(gpus, func(i, j int) bool { return gpus[i].UUID < gpus[j].UUID })
	return gpus, nil
}

// DeltaAround measures per-UUID used memory before and after fn, returning
// one Delta per card seen in BOTH samples. fn's error is returned as-is
// after the second measurement is taken, so a failed action still reports
// what it left behind on the cards.
func DeltaAround(ctx context.Context, roles map[string]string, fn func() error) ([]Delta, error) {
	before, err := PerUUID(ctx, roles)
	if err != nil {
		return nil, err
	}
	fnErr := fn()
	after, err := PerUUID(ctx, roles)
	if err != nil {
		return nil, err
	}
	return Deltas(before, after), fnErr
}

// Deltas joins two samples by UUID. Cards present in only one sample are
// dropped rather than reported with an invented endpoint.
func Deltas(before, after []GPU) []Delta {
	idx := make(map[string]GPU, len(before))
	for _, g := range before {
		idx[g.UUID] = g
	}
	var out []Delta
	for _, a := range after {
		b, ok := idx[a.UUID]
		if !ok {
			continue
		}
		out = append(out, Delta{
			UUID:        a.UUID,
			Name:        a.Name,
			Role:        a.Role,
			BaselineMiB: b.UsedMiB,
			AfterMiB:    a.UsedMiB,
			DeltaMiB:    a.UsedMiB - b.UsedMiB,
			TotalMiB:    a.TotalMiB,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UUID < out[j].UUID })
	return out
}

// TotalDelta sums per-card deltas. A model split across two cards shows up
// only in the sum, so both the per-card rows and this total are reported.
func TotalDelta(ds []Delta) int {
	sum := 0
	for _, d := range ds {
		sum += d.DeltaMiB
	}
	return sum
}
