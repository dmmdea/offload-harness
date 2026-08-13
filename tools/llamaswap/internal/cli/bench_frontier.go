// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored shared plumbing for the bench family: dispersion statistics,
// the comparability key, KV-depth prefill, and the community-canonical row.
// Not a command: no pp:data-source marker.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// dispersion
// ---------------------------------------------------------------------------

// benchUnstableCoefVarPct is the run-to-run spread above which a rate is
// flagged as unstable. 3% is llama-bench's own conventional eyebrow-raise: a
// clean, thermally-settled seat repeats within about 1%, so a wider spread
// means something moved during the measurement (another process, a swap, a
// clock/thermal change) and the median is describing two different machines.
const benchUnstableCoefVarPct = 3.0

// benchStat is the full dispersion picture for one rate. A median alone
// cannot tell a repeatable 40 tok/s from a 40 tok/s that bounced between 25
// and 55, and those two rows support opposite conclusions.
type benchStat struct {
	N      int     `json:"n"`
	Mean   float64 `json:"mean"`
	Stddev float64 `json:"stddev"`
	Median float64 `json:"median"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	// CoefVarPct is stddev as a percentage of the mean.
	CoefVarPct float64 `json:"coefficient_of_variation_pct"`
	Unstable   bool    `json:"unstable"`
	Note       string  `json:"note,omitempty"`
}

// benchStats computes the SAMPLE standard deviation (n-1 denominator). The
// runs are a sample of the seat's behavior, not the population of every
// request it will ever serve, so the n divisor would bias the spread low -
// in the direction that makes a noisy bench look repeatable.
func benchStats(v []float64, label string) benchStat {
	s := benchStat{N: len(v)}
	if len(v) == 0 {
		s.Note = "no successful " + label + " samples"
		return s
	}
	s.Min, s.Max = v[0], v[0]
	var sum float64
	for _, x := range v {
		sum += x
		if x < s.Min {
			s.Min = x
		}
		if x > s.Max {
			s.Max = x
		}
	}
	s.Mean = sum / float64(len(v))
	s.Median = median(v)
	if len(v) < 2 {
		s.Note = "single run: no dispersion is measurable, so this rate carries no repeatability evidence. Use --runs 3 or more"
		return s
	}
	var ss float64
	for _, x := range v {
		d := x - s.Mean
		ss += d * d
	}
	s.Stddev = math.Sqrt(ss / float64(len(v)-1))
	if s.Mean > 0 {
		s.CoefVarPct = s.Stddev / s.Mean * 100
	}
	if s.CoefVarPct > benchUnstableCoefVarPct {
		s.Unstable = true
		s.Note = fmt.Sprintf(
			"UNSTABLE: %s spread is %.1f%% of the mean (over the %.0f%% threshold) across %d runs, range %.1f-%.1f. "+
				"Something moved during the measurement; treat this row as a range, not a number",
			label, s.CoefVarPct, benchUnstableCoefVarPct, s.N, s.Min, s.Max)
	}
	return s
}

// String renders the mean +/- stddev form used in the canonical row.
func (s benchStat) String() string {
	if s.N == 0 {
		return "n/a"
	}
	if s.N < 2 {
		return fmt.Sprintf("%.2f (n=1)", s.Mean)
	}
	return fmt.Sprintf("%.2f ± %.2f", s.Mean, s.Stddev)
}

// ---------------------------------------------------------------------------
// comparability key
// ---------------------------------------------------------------------------

// benchKeyField is one property of the serving configuration. Source records
// where it was read so a disputed row can be re-derived, and Observed=false
// means the property exists but this deployment could not report it - which
// weakens the key rather than silently defaulting it.
type benchKeyField struct {
	Name     string `json:"name"`
	Value    string `json:"value,omitempty"`
	Source   string `json:"source"`
	Observed bool   `json:"observed"`
}

// benchComparabilityKey identifies the configuration a benchmark row
// describes. Two rows may only be diffed when their keys are identical.
//
// Modelled on llama-bench's own LLAMA_BENCH_KEY_PROPERTIES contract
// (tools/llama-bench/llama-bench.cpp, test::get_fields()): everything before
// the n_prompt/n_gen/n_depth test shape is a property that must match. That
// list is 32 fields on current master and grows; this key covers the ones a
// llama-swap SEAT can actually be interrogated for, plus the two that matter
// for a server bench and are absent upstream (n_ctx and the slot count, which
// divide the context and change batching). Fields upstream has and this
// deployment cannot see are carried with Observed=false and NAMED, so the
// strength of a match is visible instead of assumed.
type benchComparabilityKey struct {
	Fields     []benchKeyField `json:"fields"`
	SHA        string          `json:"comparability_sha"`
	Observed   int             `json:"observed_fields"`
	Total      int             `json:"total_fields"`
	Unobserved []string        `json:"unobserved_fields,omitempty"`
	Contract   string          `json:"contract"`
}

// benchSeatFlag names one llama-server flag and the key field it feeds.
type benchSeatFlag struct {
	field string
	names []string
}

// benchSeatFlags is the seat-command-line half of the key. Every entry is a
// real llama-server flag; an absent flag means the server default applies,
// which is recorded as "(default)" rather than as an unobserved field - the
// default IS the configuration.
var benchSeatFlags = []benchSeatFlag{
	{"n_batch", []string{"-b", "--batch-size"}},
	{"n_ubatch", []string{"-ub", "--ubatch-size"}},
	{"n_threads", []string{"-t", "--threads"}},
	{"n_ctx", []string{"-c", "--ctx-size"}},
	{"n_parallel", []string{"-np", "--parallel"}},
	{"type_k", []string{"-ctk", "--cache-type-k"}},
	{"type_v", []string{"-ctv", "--cache-type-v"}},
	{"n_gpu_layers", []string{"-ngl", "--n-gpu-layers"}},
	{"n_cpu_moe", []string{"-ncmoe", "--n-cpu-moe"}},
	{"split_mode", []string{"-sm", "--split-mode"}},
	{"main_gpu", []string{"-mg", "--main-gpu"}},
	{"tensor_split", []string{"-ts", "--tensor-split"}},
	{"devices", []string{"--device"}},
	{"flash_attn", []string{"-fa", "--flash-attn"}},
	{"rope_scaling", []string{"--rope-scaling"}},
	{"rope_freq_base", []string{"--rope-freq-base"}},
	{"draft_model", []string{"-md", "--model-draft"}},
}

// benchSeatBoolFlags are presence-only switches: the flag either appears on
// the command line or it does not.
var benchSeatBoolFlags = []benchSeatFlag{
	{"no_kv_offload", []string{"-nkvo", "--no-kv-offload"}},
	{"no_mmap", []string{"--no-mmap"}},
	{"mlock", []string{"--mlock"}},
	{"embeddings", []string{"--embeddings"}},
	{"reranking", []string{"--reranking"}},
	{"cont_batching", []string{"-cb", "--cont-batching"}},
	{"swa_full", []string{"--swa-full"}},
}

// benchHostInfo is the host half of the key, read from llama-swap's own
// /api/hardware and /api/version.
type benchHostInfo struct {
	CPU        string `json:"cpu,omitempty"`
	CPUCores   int    `json:"cpu_physical_cores,omitempty"`
	CPUThreads int    `json:"cpu_logical_threads,omitempty"`
	OS         string `json:"os,omitempty"`
	Arch       string `json:"arch,omitempty"`
	MemoryGiB  float64
	SwapVer    string `json:"llamaswap_version,omitempty"`
	SwapCommit string `json:"llamaswap_commit,omitempty"`
	GPUs       string `json:"gpus,omitempty"`
	Err        string `json:"error,omitempty"`
}

type benchHardwareEnvelope struct {
	Architecture struct {
		Name string `json:"name"`
	} `json:"architecture"`
	OperatingSystem struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"operating_system"`
	CPU struct {
		Model             string `json:"model"`
		PhysicalCoreCount int    `json:"physical_core_count"`
		LogicalThreads    int    `json:"logical_thread_count"`
	} `json:"cpu"`
	Memory struct {
		CapacityBytes int64 `json:"capacity_bytes"`
	} `json:"memory"`
}

// benchReadHost gathers the host identity. Failure is recorded, never fatal:
// a benchmark whose host line is incomplete is a weaker row, not a failed run.
func benchReadHost(ctx context.Context, flags *rootFlags, timeout time.Duration) benchHostInfo {
	var h benchHostInfo
	var hw benchHardwareEnvelope
	if err := mcGetJSON(ctx, flags, "/api/hardware", timeout, &hw); err != nil {
		h.Err = "GET /api/hardware: " + err.Error()
	} else {
		h.CPU = hw.CPU.Model
		h.CPUCores, h.CPUThreads = hw.CPU.PhysicalCoreCount, hw.CPU.LogicalThreads
		h.Arch = hw.Architecture.Name
		h.OS = strings.TrimSpace(hw.OperatingSystem.Name + " " + hw.OperatingSystem.Version)
		if hw.Memory.CapacityBytes > 0 {
			h.MemoryGiB = float64(hw.Memory.CapacityBytes) / (1024 * 1024 * 1024)
		}
	}
	var ver struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	if err := mcGetJSON(ctx, flags, "/api/version", timeout, &ver); err == nil {
		h.SwapVer, h.SwapCommit = ver.Version, ver.Commit
	}
	if gpus, err := mcGPUs(ctx, flags); err == nil && len(gpus) > 0 {
		names := make([]string, 0, len(gpus))
		for _, g := range gpus {
			names = append(names, g.Name)
		}
		h.GPUs = strings.Join(names, " + ")
	}
	return h
}

// buildComparabilityKey derives the key from the seat's live command line,
// the llama.cpp build string, the weights file, and the host.
func buildComparabilityKey(seatCmd, buildInfo, modelPath, modelQuant string, modelBytes int64, modelParams uint64, host benchHostInfo) benchComparabilityKey {
	tokens := mcSplitCmd(seatCmd)
	key := benchComparabilityKey{
		Contract: "llama-bench LLAMA_BENCH_KEY_PROPERTIES style: every configuration property that must match before two rows may be compared. " +
			"Fields marked observed=false could not be read from this deployment and are named rather than defaulted.",
	}
	add := func(name, value, source string) {
		key.Fields = append(key.Fields, benchKeyField{Name: name, Value: value, Source: source, Observed: value != ""})
	}
	addObserved := func(name, value, source string) {
		key.Fields = append(key.Fields, benchKeyField{Name: name, Value: value, Source: source, Observed: true})
	}

	// llama.cpp build. /props reports "b10356-0666ad2b2": number then commit.
	buildNumber, buildCommit := "", ""
	if i := strings.Index(buildInfo, "-"); i > 0 {
		buildNumber, buildCommit = buildInfo[:i], buildInfo[i+1:]
	} else {
		buildNumber = buildInfo
	}
	add("build_number", buildNumber, "GET /upstream/{model}/props build_info")
	add("build_commit", buildCommit, "GET /upstream/{model}/props build_info")
	add("llamaswap_version", host.SwapVer, "GET /api/version")
	add("llamaswap_commit", host.SwapCommit, "GET /api/version")

	// Host.
	add("cpu_info", host.CPU, "GET /api/hardware")
	add("cpu_cores", itoaOrEmpty(host.CPUCores), "GET /api/hardware")
	add("gpu_info", host.GPUs, "nvidia-smi per-UUID")
	add("os", host.OS, "GET /api/hardware")
	add("arch", host.Arch, "GET /api/hardware")

	// Model identity.
	add("model_filename", mcBaseName(modelPath), "seat -m/--model")
	add("model_ftype", modelQuant, "GGUF general.file_type")
	add("model_size_bytes", int64ToStrOrEmpty(modelBytes), "GGUF file size")
	add("model_n_params", uint64ToStrOrEmpty(modelParams), "GGUF tensor table")

	// Seat flags with values.
	for _, f := range benchSeatFlags {
		if v, ok := mcFlagValue(tokens, f.names...); ok {
			addObserved(f.field, v, "seat "+f.names[0])
			continue
		}
		addObserved(f.field, "(default)", "seat command line: flag absent, server default applies")
	}
	// Presence-only switches.
	for _, f := range benchSeatBoolFlags {
		on := "0"
		if _, ok := mcFlagValue(tokens, f.names...); ok {
			on = "1"
		}
		addObserved(f.field, on, "seat "+f.names[0]+" presence")
	}
	// The executable itself: two builds at the same version can still be
	// different binaries (different CUDA arch, different backend set).
	if len(tokens) > 0 {
		addObserved("server_binary", mcBaseName(tokens[0]), "seat argv[0]")
	} else {
		add("server_binary", "", "seat argv[0]")
	}

	var b strings.Builder
	for _, f := range key.Fields {
		key.Total++
		if !f.Observed {
			key.Unobserved = append(key.Unobserved, f.Name)
			continue
		}
		key.Observed++
		b.WriteString(f.Name)
		b.WriteByte('=')
		b.WriteString(f.Value)
		b.WriteByte('\n')
	}
	// The unobserved set is hashed too. A row measured with gpu_info missing
	// is NOT interchangeable with one where it was read, even if every other
	// field matches, and collapsing them would let an unverified comparison
	// pass as a verified one.
	b.WriteString("unobserved=" + strings.Join(key.Unobserved, ",") + "\n")
	key.SHA = mcSHA256(b.String())
	return key
}

// diffKeys names the fields on which two keys disagree.
func diffKeys(a, b benchComparabilityKey) []string {
	index := func(k benchComparabilityKey) map[string]benchKeyField {
		m := make(map[string]benchKeyField, len(k.Fields))
		for _, f := range k.Fields {
			m[f.Name] = f
		}
		return m
	}
	bi := index(b)
	seen := map[string]bool{}
	var out []string
	for _, f := range a.Fields {
		seen[f.Name] = true
		other, ok := bi[f.Name]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s: present in A (%s), absent in B", f.Name, valueOrUnobserved(f)))
		case f.Observed != other.Observed || f.Value != other.Value:
			out = append(out, fmt.Sprintf("%s: %s -> %s", f.Name, valueOrUnobserved(f), valueOrUnobserved(other)))
		}
	}
	for _, f := range b.Fields {
		if !seen[f.Name] {
			out = append(out, fmt.Sprintf("%s: absent in A, present in B (%s)", f.Name, valueOrUnobserved(f)))
		}
	}
	sort.Strings(out)
	return out
}

func valueOrUnobserved(f benchKeyField) string {
	if !f.Observed {
		return "<unobserved>"
	}
	return f.Value
}

func itoaOrEmpty(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func int64ToStrOrEmpty(n int64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

func uint64ToStrOrEmpty(n uint64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatUint(n, 10)
}

func mcBaseName(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ---------------------------------------------------------------------------
// KV depth
// ---------------------------------------------------------------------------

// benchDepthFiller is the fixed text repeated to build a KV-depth prefill.
// Fixed and prose-shaped on purpose: a filler of repeated single tokens
// compresses differently in the cache and would not represent a real context.
const benchDepthFiller = "The proxy records every model swap it performs, including the wall time between the request that " +
	"triggered the load and the moment the upstream process reported itself ready to serve. Operators use that record to " +
	"decide which models are worth keeping resident and which can be paid for on demand, because the cost of a swap is " +
	"paid by whichever request happens to arrive first. "

// parseDepths turns "0,4096" into []int{0, 4096}. An empty value means the
// single default depth of zero.
func parseDepths(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return []int{0}, nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("--depth %q: %q is not a token count", s, part)
		}
		if n < 0 {
			return nil, fmt.Errorf("--depth %q: a negative KV depth is meaningless", s)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return []int{0}, nil
	}
	sort.Ints(out)
	return out, nil
}

// benchTokenize asks the model's own tokenizer for a token count. Never
// estimated: a chars/4 approximation is about 2x off on Gemma tokenizers,
// which would make every depth label a fiction.
func benchTokenize(ctx context.Context, flags *rootFlags, model, text string, timeout time.Duration) (int, error) {
	var tok struct {
		Tokens []any `json:"tokens"`
	}
	if err := mcPostJSON(ctx, flags, "/upstream/"+model+"/tokenize", map[string]any{"content": text}, timeout, &tok); err != nil {
		return 0, err
	}
	return len(tok.Tokens), nil
}

// benchBuildFiller returns a prose filler whose measured token count is as
// close to target as repeating the fixed paragraph allows, plus the MEASURED
// count. Two tokenize calls: one to learn the per-copy cost, one to report
// the truth about what was built.
func benchBuildFiller(ctx context.Context, flags *rootFlags, model string, target int, timeout time.Duration) (string, int, error) {
	if target <= 0 {
		return "", 0, nil
	}
	perCopy, err := benchTokenize(ctx, flags, model, benchDepthFiller, timeout)
	if err != nil {
		return "", 0, err
	}
	if perCopy <= 0 {
		return "", 0, fmt.Errorf("the tokenizer reported %d tokens for the filler paragraph; cannot size a %d-token prefill", perCopy, target)
	}
	copies := target / perCopy
	if copies < 1 {
		copies = 1
	}
	filler := strings.Repeat(benchDepthFiller, copies)
	actual, err := benchTokenize(ctx, flags, model, filler, timeout)
	if err != nil {
		return "", 0, err
	}
	return filler, actual, nil
}

// ---------------------------------------------------------------------------
// canonical row
// ---------------------------------------------------------------------------

// benchStandardPP / benchStandardTG are the community-canonical shapes: a
// 512-token prompt for prompt processing and 128 generated tokens for
// generation. Quoting a "pp512 / tg128" row is only meaningful at these
// sizes, which is why --standard fixes them instead of taking flags.
const (
	benchStandardPP = 512
	benchStandardTG = 128
)

// benchStandardRow renders the markdown table the llama.cpp community quotes,
// with the provenance footnote that keeps it honest: these numbers come from
// the production HTTP route with the seat's chat template applied, so they
// are comparable to OTHER rows from this CLI and are NOT byte-identical to a
// `llama-bench` invocation of the same model.
func benchStandardRow(m *benchModelResult, host benchHostInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "| model | size | params | backend | ngl | test | t/s |\n")
	fmt.Fprintf(&b, "| --- | ---: | ---: | --- | ---: | --- | ---: |\n")
	size := "n/a"
	if m.ModelSizeBytes > 0 {
		size = fmt.Sprintf("%.2f GiB", float64(m.ModelSizeBytes)/(1024*1024*1024))
	}
	params := "n/a"
	if m.ModelParams > 0 {
		params = mcHumanCount(m.ModelParams)
	}
	backend := host.GPUs
	if backend == "" {
		backend = "unknown"
	}
	ngl := m.KeyField("n_gpu_layers")
	label := m.Model
	if m.ModelQuant != "" {
		label = fmt.Sprintf("%s %s", m.Model, m.ModelQuant)
	}
	for _, d := range m.Depths {
		suffix := ""
		if d.DepthRequested > 0 {
			suffix = fmt.Sprintf(" @ d%d", d.DepthRequested)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | pp%d%s | %s |\n",
			label, size, params, backend, ngl, benchStandardPP, suffix, d.PP.String())
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | tg%d%s | %s |\n",
			label, size, params, backend, ngl, benchStandardTG, suffix, d.TG.String())
	}
	fmt.Fprintf(&b, "\nbuild: %s (llama-swap %s %s)\n", m.BuildInfo, host.SwapVer, host.SwapCommit)
	if host.CPU != "" {
		fmt.Fprintf(&b, "host: %s (%d cores / %d threads), %s\n", host.CPU, host.CPUCores, host.CPUThreads, host.OS)
	}
	fmt.Fprintf(&b, "comparability_sha: %s (%d of %d fields observed)\n", m.Key.SHA, m.Key.Observed, m.Key.Total)
	fmt.Fprintf(&b, "\n> Measured through POST /v1/chat/completions - the production route - with the seat's chat template\n"+
		"> applied, and read from llama.cpp's own timings object. Directly comparable to other rows from this CLI\n"+
		"> carrying the same comparability_sha; NOT byte-identical to a `llama-bench` run of the same model, which\n"+
		"> benchmarks the library without the server, the template, or the slot scheduler.\n")
	return b.String()
}

// KeyField reads one comparability-key value for display.
func (m *benchModelResult) KeyField(name string) string {
	for _, f := range m.Key.Fields {
		if f.Name == name {
			if !f.Observed {
				return "?"
			}
			return f.Value
		}
	}
	return "?"
}

// benchKeyJSON serializes a key for the store so `bench compare` can name the
// fields that differ rather than only reporting that the hashes did.
func benchKeyJSON(k benchComparabilityKey) string {
	b, err := json.Marshal(k)
	if err != nil {
		return ""
	}
	return string(b)
}
