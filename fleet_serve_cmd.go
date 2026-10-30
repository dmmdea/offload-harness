// fleet-serve support: nvidia-smi probes, the VRAM sampler chooser, startup
// parameters, drain and await, fleet-measure and the advertised accelerators.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/netguard"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
	"github.com/dmmdea/offload-harness/internal/winexec"
)

// nvidiaSmiMemory shells the global VRAM query (MiB CSV) that feeds the
// fleet-serve startup GATE probe (ResolveProvider's "does nvidia-smi work at
// all" check); fleetnode.ParseSmiMemory parses it. The ONGOING 2s health
// sampler uses nvidiaSmiMemoryDevices instead (below) — a multi-GPU box needs
// the per-device breakdown that query can't provide (2 fields, no index/name;
// see ParseSmiMemory's doc comment for why this query/parser pair is frozen
// rather than extended: the pipeline's per-process footprint delta sampler
// also depends on this exact 2-field shape).
func nvidiaSmiMemory() (string, error) {
	out, err := winexec.Command("nvidia-smi", "--query-gpu=memory.total,memory.used", "--format=csv,noheader,nounits").Output()
	return string(out), err
}

// nvidiaSmiMemoryDevices shells the PER-DEVICE VRAM query (index, uuid, name,
// memory.total, memory.used — MiB CSV, one line per GPU) that feeds the
// fleet-serve 2s health sampler on a working nvidia-smi node;
// fleetnode.ParseSmiMemoryDevices parses it. This is what actually fixes the
// multi-GPU mis-report: nvidia-smi's own line order is PCI bus order, not
// CUDA device order, so which line is "first" says nothing about which
// device a render will actually use (fleetnode.HeadlineDevice picks the
// headline device from the parsed set instead of trusting enumeration order,
// and fleetnode.SelectHeadlineDevice lets an operator override that pick by
// UUID via the primary_gpu_uuid config key — the uuid field is why it's in
// this query at all: index/total-VRAM alone can't reliably identify a card
// across a reboot or reseat, but the UUID is burned into it).
// utilization.gpu is included to advertise which device is currently busiest,
// helping the PAIR operator optimize placement and scheduling.
//
// Since 0.116.0 the command itself lives in gpuprobe.NvidiaSmiRunner (the
// leaf the composite tier's placement guards read through as well): ONE
// query string, ONE PATH-or-System32 lookup, so the health sampler and the
// display-card guard can never disagree about what nvidia-smi was asked.
func nvidiaSmiMemoryDevices() (string, error) {
	return gpuprobe.NvidiaSmiRunner()()
}

// samplerKind is which VRAM sampler runFleetServe starts for a resolved
// provider Source — the exact seam that decides whether /fleet/health's
// gpu_devices[] is populated or omitted. Pulled out as its own type + pure
// selection function (chooseSamplerKind, below) specifically so that claim is
// unit-tested against the SAME decision runFleetServe ships (main_test.go),
// not a hand-copied approximation of it — runFleetServe calls
// chooseSamplerKind rather than re-implementing the branch inline.
type samplerKind int

const (
	// samplerKindDevice runs the per-device nvidia-smi query
	// (nvidiaSmiMemoryDevices/fleetnode.StartDeviceProbeSampler). gpu_devices[]
	// is ALWAYS present on this path — including a single-GPU nvidia-smi box,
	// which reports it with exactly one entry. There is no single-vs-multi-GPU
	// special case anywhere in this file: nvidia-smi always gets this sampler.
	samplerKindDevice samplerKind = iota
	// samplerKindSingle runs the plain single-value MemProbe
	// (fleetnode.StartProbeSampler) — today only the windows-generic source,
	// which has no per-adapter identity to enumerate (vram_windows.go).
	// gpu_devices[] is omitted (Snapshot.Devices stays nil) on this path.
	samplerKindSingle
	// samplerKindAmdgpuDevice runs the per-device amdgpu sysfs query
	// (fleetnode.AmdgpuSysfsDeviceProbe) through the same device sampler, so a linux-amdgpu
	// node publishes gpu_devices[] the way an nvidia-smi node does (ADR 0053 decision 4). The
	// card's identity is its sysfs index and name: there is no UUID to pin a headline to.
	samplerKindAmdgpuDevice
)

// amdgpuSource is the generic provider's Source on a Linux box that reads the amdgpu driver's
// sysfs counters: what genericMemProvider names it and what chooseSamplerKind recognises.
const amdgpuSource = "linux-amdgpu"

// genericMemProvider picks the generic GPU memory source for this OS and the
// installed tier — pulled out of runFleetServe, like chooseSamplerKind, so the
// selection that ships is the selection that is tested (fleet_verbs_test.go).
//
// Windows gets the WDDM provider and Linux the amdgpu sysfs one. A tier that is a
// unified-memory SoC (rockchip-rk3588) has neither counter, so it reads
// /proc/meminfo less the operator's reserve; procRoot is "/" in production and
// only that provider reads through it. A cpu-profile box means detect found NO
// usable GPU: it must not fleet-serve off iGPU-adjacent counters — only a working
// nvidia-smi may qualify it, so its generic source is nil.
func genericMemProvider(goos, profile string, uma bool, umaReserveGiB float64, procRoot string) fleetnode.GenericProvider {
	generic := fleetnode.GenericProvider{Probe: fleetnode.GenericWindowsProbe(uma), Source: "windows-generic"}
	if goos == "linux" {
		generic = fleetnode.GenericProvider{Probe: fleetnode.AmdgpuSysfsProbe("/sys/class/drm", uma), Source: amdgpuSource}
	}
	switch profile {
	case "rockchip-rk3588":
		generic = fleetnode.GenericProvider{Probe: fleetnode.MeminfoUMAProbe(procRoot, umaReserveGiB), Source: fleetnode.MeminfoSource}
	case "cpu":
		generic.Probe = nil
	}
	return generic
}

// chooseSamplerKind picks which sampler runFleetServe starts, given the
// resolved GPU memory provider's Source ("nvidia-smi" | "windows-generic" |
// "linux-amdgpu" | "linux-meminfo").
// nvidia-smi always gets samplerKindDevice — a single-GPU nvidia-smi box is
// NOT special-cased to the single-value sampler; it runs the per-device query
// and /fleet/health reports gpu_devices[] with one entry, same as any other
// nvidia-smi node. linux-amdgpu gets samplerKindAmdgpuDevice, the same shape from the
// driver's sysfs. Every other source (windows-generic, which has no per-adapter identity, and
// linux-meminfo, one RAM pool with no cards) falls through to samplerKindSingle.
func chooseSamplerKind(source string) samplerKind {
	switch source {
	case "nvidia-smi":
		return samplerKindDevice
	case amdgpuSource:
		return samplerKindAmdgpuDevice
	}
	return samplerKindSingle
}

// gpuArchFromName maps an nvidia-smi product name to the architecture CLASS
// the health payload advertises as gpu_arch — the dispatcher routes on arch
// classes, not product names ("NVIDIA GeForce RTX 3070 Laptop GPU" is not a
// schedulable fact; "ampere" is). Mirrors setup/detect.ps1's Get-GpuArch:
// note "RTX PRO" must match in its own right BEFORE any 50xx logic — the PRO
// branding ("RTX PRO 5000") breaks the "RTX 50" substring, and that branding
// IS the Blackwell pro generation (pre-Blackwell pro cards were "RTX A6000" /
// "RTX 6000 Ada Generation", which match neither rule). Unrecognized products
// (and an empty name from a failed query) fall back to the lowercase vendor
// "nvidia" — an honest "NVIDIA, generation unknown", never "".
func gpuArchFromName(name string) string {
	n := strings.ToUpper(name)
	switch {
	case strings.Contains(n, "RTX PRO"), strings.Contains(n, "BLACKWELL"), strings.Contains(n, "RTX 50"):
		return "blackwell"
	case strings.Contains(n, "RTX 40"):
		return "ada"
	case strings.Contains(n, "RTX 30"):
		return "ampere"
	case strings.Contains(n, "RTX 20"), strings.Contains(n, "GTX 16"):
		return "turing"
	case strings.Contains(n, "V100"):
		return "volta"
	default:
		return "nvidia"
	}
}

// nvidiaSmiGPUName returns the GPU product name feeding gpuArchFromName
// (best-effort: "" when the query fails — the probe already proved the GPU
// works, and the arch mapper turns "" into the vendor fallback).
func nvidiaSmiGPUName() string {
	out, err := winexec.Command("nvidia-smi", "--query-gpu=name", "--format=csv,noheader").Output()
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(out))
	if i := strings.IndexByte(name, '\n'); i >= 0 {
		name = strings.TrimSpace(name[:i])
	}
	return name
}

// fleetServeParams resolves + validates fleet-serve's identity/binding args —
// the testable seam (mirrors runGraphParams): flag > config for the listen
// address and node id, hostname fallback for an empty id, the shared
// netguard loopback refusal unless --listen-trusted-network, and the rule that
// a listener beyond loopback needs a fleet_auth_token. Every lane of a node requires
// the bearer, so a node that listens beyond loopback with none has no credential
// that could authorize any caller: it fails to start, naming the key and the config
// file it read (src), instead of serving a port that refuses everything (or, before
// this rule, serving it open).
func fleetServeParams(listenFlag, nodeIDFlag string, trusted bool, cfg config.Config, src config.Source, hostname func() (string, error)) (listen, nodeID string, err error) {
	listen = listenFlag
	if listen == "" {
		listen = cfg.FleetListen
	}
	if listen == "" {
		listen = "127.0.0.1:18811" // config.Default's value; belt-and-suspenders for a zeroed config
	}
	if err := netguard.Validate(listen, trusted); err != nil {
		return "", "", err
	}
	if !netguard.LoopbackAddr(listen) && cfg.FleetAuthToken == "" {
		where, fix := fleetServeConfigWhere(src)
		return "", "", fmt.Errorf("refusing to serve on %q: it is not a loopback address and fleet_auth_token is not set (%s). "+
			"Every fleet lane requires the fleet bearer on a node that listens beyond loopback, so with no token no caller could be "+
			"authorized. %s (the same value on this node and on every delegator that calls it), or listen on a loopback address", listen, where, fix)
	}
	nodeID = nodeIDFlag
	if nodeID == "" {
		nodeID = cfg.FleetNodeID
	}
	if nodeID == "" {
		if h, herr := hostname(); herr == nil && h != "" {
			nodeID = h
		} else {
			nodeID = "fleet-node"
		}
	}
	return listen, nodeID, nil
}

// fleetServeConfigWhere says which config the node read and what to do about it, for the refusal that a listener beyond
// loopback has no fleet_auth_token: a node that will not start must name the file to fix, because a box can hold two
// (LOCAL_OFFLOAD_CONFIG against the default, --config against both).
func fleetServeConfigWhere(src config.Source) (where, fix string) {
	switch {
	case src.Path == "":
		return "no config file was found, so this node runs on built-in defaults",
			"Put fleet_auth_token in a config file and point --config or LOCAL_OFFLOAD_CONFIG at it"
	case src.NotFound:
		return fmt.Sprintf("the config file %s was named but does not exist", src.Path),
			fmt.Sprintf("Create %s with fleet_auth_token set, or point --config or LOCAL_OFFLOAD_CONFIG at this node's config", src.Path)
	default:
		return fmt.Sprintf("the config this node read is %s", src.Path),
			fmt.Sprintf("Set fleet_auth_token in %s", src.Path)
	}
}

// fleetServeAuthNote is the startup line that says which lanes require the fleet bearer. Only two states can reach it,
// because fleetServeParams refuses the third (a listener beyond loopback with no token): a loopback node with no
// token, where every lane is open to this box and nobody else, and a node with a token, where every lane requires it.
func fleetServeAuthNote(listen string, cfg config.Config) string {
	if cfg.FleetAuthToken == "" {
		return fmt.Sprintf("loopback only: every fleet lane is tokenless on %s (set fleet_auth_token to require the bearer)", listen)
	}
	where := "on loopback"
	if !netguard.LoopbackAddr(listen) {
		where = fmt.Sprintf("beyond loopback on %q", listen)
	}
	return fmt.Sprintf("every lane requires the fleet bearer (fleet_auth_token is set; this node listens %s): a request without it is refused with 401; health and the jobs feed answer without it, with their private fields withheld", where)
}

// fleetServeConfig returns cfg with the node id fleet-serve resolved (fleetServeParams) set as the id the pipeline stamps
// on its result envelopes, so a node names itself one way in /fleet/health, in a queue claim and on every result.
func fleetServeConfig(cfg config.Config, nodeID string) config.Config {
	cfg.FleetNodeID = nodeID
	return cfg
}

// reportPipelineJobsSweep writes the startup sweep's outcome to w: up to three
// lines, each reported whether or not the others are, so a sweep that removed
// 22 dirs and could not remove one says both. The removed line does not call
// those dirs a crash's leftovers: a dir is also removed for its age, and the
// sweep logs each of those itself, above this summary.
func reportPipelineJobsSweep(w io.Writer, swept, kept int, serr error) {
	if serr != nil {
		fmt.Fprintf(w, "[fleet-serve] warning: pipeline-jobs sweep: %v\n", serr)
	}
	if swept > 0 {
		fmt.Fprintf(w, "[fleet-serve] swept %d orphaned pipeline-job dir(s): an earlier instance's leftovers, runs whose delegator exited, or dirs past the age bound (those are logged one by one above)\n", swept)
	}
	if kept > 0 {
		fmt.Fprintf(w, "[fleet-serve] kept %d pipeline-job dir(s) that may belong to local runs still in flight (owned by a live process, or too recent to call orphaned)\n", kept)
	}
}

// fleetServeAwait blocks until fleet-serve stops, and drains on BOTH ways out. An interrupt drains
// BEFORE closing the listener: new dispatches already 503, but pollers can still read states while
// in-flight renders finish. A Serve that returns on its own (the listener failed) leaves the same jobs
// in flight and the same card posts pending, so it drains too and then returns Serve's error: the
// process exits either way, and without the drain the cards of the jobs it was running stay open
// until the next process's orphan sweep closes them failed.
func fleetServeAwait(ctx context.Context, errCh <-chan error, ln io.Closer, jobs *fleetnode.Jobs, pair *pairworkloads.Emitter, timeout time.Duration) error {
	select {
	case err := <-errCh:
		fmt.Fprintf(os.Stderr, "[fleet-serve] server stopped (%v) — draining jobs (up to %s); survivors are marked error:\"interrupted\"\n", err, timeout)
		fleetServeDrain(jobs, pair, timeout)
		return err
	case <-ctx.Done():
		fmt.Fprintf(os.Stderr, "[fleet-serve] interrupt — draining jobs (up to %s); survivors are marked error:\"interrupted\"\n", timeout)
		fleetServeDrain(jobs, pair, timeout)
		ln.Close()
		<-errCh
		return nil
	}
}

// fleetServeDrain is fleet-serve's shutdown drain: it stops the job store (in-flight jobs finish, the
// never-started ones are dropped), then waits for the node emitter's background posts. The terminal
// frames of the jobs that just finished, and the failed frames of the dropped ones, are posted from
// goroutines the process would otherwise exit under: the card of a job that completed would be left
// open and closed failed by the next process's orphan sweep. Each post is bounded (2 s), so the wait is.
func fleetServeDrain(jobs *fleetnode.Jobs, pair *pairworkloads.Emitter, timeout time.Duration) {
	jobs.DrainAndStop(timeout)
	pair.Wait()
}

// runFleetMeasure primes an empty footprint store: one minimal render per
// configured task through the NORMAL pipeline (so the passive gpugen hook
// records exactly what fleet jobs will), then prints the store's on-disk
// records as JSON. Tasks without a cheap universal probe (voice, run-graph)
// are skipped — their footprints accumulate passively during normal use.
func runFleetMeasure(args []string) error {
	fs := flag.NewFlagSet("fleet-measure", flag.ExitOnError)
	fs.String("config", "", "config file path")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx := context.Background()
	note := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "[fleet-measure] "+format+"\n", a...) }

	// image-gen: a small fast render (512² at 8 steps). Engine-aware gate (J2):
	// an sdcpp box has no imagegen_script but absolutely serves image-gen.
	var stillPath string
	if cfg.ImageRouteConfigured() {
		note("image-gen: rendering 512x512 at 8 steps...")
		res := p.Run(ctx, core.Request{
			Task:   core.TaskGenerateImage,
			Door:   "cli:fleet-measure",
			Input:  "fleet-measure probe: a plain gray sphere on a white background",
			Params: map[string]any{"width": 512, "height": 512, "steps": 8},
		})
		if res.OK {
			var out struct {
				ImagePath string `json:"image_path"`
			}
			_ = json.Unmarshal(res.Data, &out)
			stillPath = out.ImagePath
			note("image-gen: done (%s)", out.ImagePath)
		} else {
			note("image-gen: deferred: %s", res.Reason)
		}
	} else {
		note("image-gen: skipped (no image route configured - neither imagegen_script nor the sdcpp engine)")
	}

	// video-gen: the FAST (distilled) recipe at the smallest frame count — the
	// slow native recipe is not a measurement tool. Reuses the probe image as
	// the I2V still when the image step produced one. Engine-aware gate (CT-49), like the
	// image step: a box whose only video is the sdcpp engine has no videogen_script but
	// absolutely serves video-gen, and its iGPU footprints must be recorded too.
	if cfg.VideoGenBound() {
		note("video-gen: rendering the fast recipe at 9 frames...")
		params := map[string]any{"fast": true, "frames": 9}
		req := core.Request{Task: core.TaskGenerateVideo, Door: "cli:fleet-measure", Input: "fleet-measure probe: slow gentle camera pan", Params: params}
		if stillPath != "" {
			req.Image = stillPath
		}
		if res := p.Run(ctx, req); res.OK {
			note("video-gen: done")
		} else {
			note("video-gen: deferred: %s", res.Reason)
		}
	} else {
		note("video-gen: skipped (no video route configured - neither videogen_script nor an sdcpp video family)")
	}

	// audio-gen: 5s of music (the ComfyUI ACE-Step path or the audio.cpp engine — the GPU-heavy one).
	if cfg.MusicGenBound() {
		note("audio-gen: rendering 5s of music...")
		res := p.Run(ctx, core.Request{
			Task:   core.TaskGenerateAudio,
			Door:   "cli:fleet-measure",
			Input:  "fleet-measure probe: soft ambient pad, slow tempo",
			Params: map[string]any{"kind": "music", "seconds": 5},
		})
		if res.OK {
			note("audio-gen: done")
		} else {
			note("audio-gen: deferred: %s", res.Reason)
		}
	} else {
		note("audio-gen: skipped (no music route configured - neither musicgen_script nor musicgen_engine audiocpp)")
	}
	note("audio-gen (voice): skipped — voice footprints accumulate passively during normal TTS use")
	note("run-graph: skipped — no universal probe graph; footprints accumulate passively per model_family")

	// Print the on-disk records (vram_peak_gb + observed_peak_gb/samples — the
	// observed value is what the Afterburner validation procedure compares).
	st := p.FootprintStore()
	if st == nil {
		fmt.Println("[]")
		return nil
	}
	raw, rerr := os.ReadFile(st.Path())
	if rerr != nil {
		fmt.Println("[]")
		return nil
	}
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}

// fleetServeConfigGate refuses to start a fleet NODE on a config that failed
// validation.
//
// Every other entry point degrades and warns, which is right for them: a
// one-shot CLI verb that half-works still answers, and an MCP server that exits
// takes every tool out of every session with no message anywhere. A fleet node
// is the exception. It ADVERTISES capability to other boxes and then accepts
// their dispatches, so a node whose endpoint dials a dead port does not fail
// alone — it turns every delegator's placement decision into a wasted wall,
// which is the cost this whole change exists to remove. A node that cannot
// prove its own config must not join the fleet.
//
// Only a validation FAILURE refuses. A missing file, or no file at all, is a
// legitimate fresh box; that path already warns loudly.
func fleetServeConfigGate(src config.Source) error {
	if src.LoadErr == nil {
		return nil
	}
	return fmt.Errorf("refusing to serve: config at %s %s: %w", src.Path, config.LoadFailure(src.LoadErr), src.LoadErr)
}

// fleetAccelerators picks the accelerator list health advertises: the installer
// manifest's when it lists any, else the harness config's (Coral D6), minus the
// devices that stay on the box that carries them (register E-08: the standalone
// Hailo-8L is never advertised). The source is chosen first and the filter then
// applies to what it listed, so a manifest naming only a local-only device still
// wins and advertises nothing. Pure, so the test can pin both sources.
func fleetAccelerators(manifest, cfg []string) []string {
	if len(manifest) > 0 {
		return config.FleetVisibleAccelerators(manifest)
	}
	return config.FleetVisibleAccelerators(cfg)
}
