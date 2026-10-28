package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/winexec"
	"gopkg.in/yaml.v3"
)

// The side-port smoke gate (design 5.3 steps 5-7, Ruling R6): the staged llama-server loads one of the node's own
// seats on a side port, answers one completion, and its speed and VRAM are measured back to back against the
// previous generation's llama-server, all under the node's GPU lease with the seats drained and unloaded.
//
// The lease is taken by the harness's own wrapper (`gpu reserve --wait --drain --unload-seat -- <this binary>
// update --gate-child ...`) instead of a second copy of its queue, heartbeat, drain, warm-back and release code:
// the measurement runs as the wrapped command, so every rule the rest of the binary follows holds here too.

// Gate thresholds (Ruling R6).
const (
	// MaxSlowdown is the most a decode or prefill rate may fall below the previous generation's.
	MaxSlowdown = 0.20
	// MaxVRAMGrowth is the most the seat's VRAM may grow over the previous generation's.
	MaxVRAMGrowth = 0.10
	// vramNoiseMiB is the smallest growth that can reject: a desktop that moved a window between the two runs is not a
	// regression. Both figures are still recorded.
	vramNoiseMiB = 256.0
)

// Gate defaults (config.json update.gate.*).
const (
	DefaultLeaseWait       = 30 * time.Minute
	DefaultSidePort        = 11499
	DefaultLoadTimeout     = 5 * time.Minute
	DefaultPostFlipTimeout = 5 * time.Minute
)

// forbiddenPorts are never used for a side port (the house rule on ports).
var forbiddenPorts = map[int]bool{80: true, 443: true, 3000: true, 5000: true, 8000: true, 6443: true}

// GateConfig is `update.gate` of the node's config.json. Every field is optional.
type GateConfig struct {
	// Enabled false turns the smoke gate (and the post-flip watch) off; default on.
	Enabled *bool `json:"enabled"`
	// LeaseWait is how long the gate queues for the GPU lease ("30m"); past it the answer is inconclusive.
	LeaseWait string `json:"lease_wait"`
	// SidePort is the loopback port the gate's llama-server listens on (default 11499).
	SidePort int `json:"side_port"`
	// LoadTimeout is how long a llama-server may take to answer /health ("5m").
	LoadTimeout string `json:"load_timeout"`
	// PostFlipTimeout is how long the first real seat load after the flip is watched ("5m").
	PostFlipTimeout string `json:"post_flip_timeout"`
}

// On reports whether the gate is enabled.
func (c GateConfig) On() bool { return c.Enabled == nil || *c.Enabled }

func dur(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(s)); err == nil && d > 0 {
		return d
	}
	return def
}

// LeaseWaitOr is the lease queue budget.
func (c GateConfig) LeaseWaitOr() time.Duration { return dur(c.LeaseWait, DefaultLeaseWait) }

// LoadTimeoutOr is the /health budget of one llama-server.
func (c GateConfig) LoadTimeoutOr() time.Duration { return dur(c.LoadTimeout, DefaultLoadTimeout) }

// PostFlipTimeoutOr is the post-flip watch budget.
func (c GateConfig) PostFlipTimeoutOr() time.Duration {
	return dur(c.PostFlipTimeout, DefaultPostFlipTimeout)
}

// PortOr is the side port, refusing a forbidden or invalid one.
func (c GateConfig) PortOr() (int, error) {
	p := c.SidePort
	if p == 0 {
		p = DefaultSidePort
	}
	if p < 1024 || p > 65535 || forbiddenPorts[p] {
		return 0, fmt.Errorf("update.gate.side_port %d is not usable (forbidden ports 80/443/3000/5000/8000/6443; use 1024-65535)", p)
	}
	return p, nil
}

// SmokeGate is the Gate for llama. Other components pass.
type SmokeGate struct {
	Self       string // this binary (re-run under the lease)
	ConfigPath string // config file the lease wrapper reads, "" = its default
	YAML       string // the rendered llama-swap yaml
	Cfg        GateConfig
	Logf       func(format string, args ...any)
}

// Name implements Gate.
func (*SmokeGate) Name() string { return "side-port smoke under the GPU lease" }

func (g *SmokeGate) logf(f string, a ...any) {
	if g.Logf != nil {
		g.Logf(f, a...)
	}
}

// smokeJob is what the lease-held child needs; it is handed over as a file. It never carries the seat's environment
// (a yaml's env may hold a token): the child reads the seat again from YAML, and Seat travels without its Env.
type smokeJob struct {
	Seat        *Seat
	YAML        string
	Live        string
	Candidate   string
	StagedDir   string
	PreviousDir string
	Port        int
	LoadTimeout time.Duration
}

// Run implements Gate.
func (g *SmokeGate) Run(ctx context.Context, in GateInput) GateResult {
	if in.Component != Llama {
		return GateResult{Verdict: GatePass, Reason: "no smoke gate for " + in.Component}
	}
	port, err := g.Cfg.PortOr()
	if err != nil {
		return GateResult{Verdict: GateInconclusive, Reason: err.Error()}
	}
	seat, err := PickSeat(g.YAML, in.Live)
	if err != nil {
		return GateResult{Verdict: GatePass, Reason: "no seat to smoke (" + err.Error() + "); offline self-check passed"}
	}
	id := ""
	if in.Candidate != nil {
		id = in.Candidate.ID
	}
	if g.Self == "" {
		return GateResult{Verdict: GateInconclusive, Reason: "cannot locate this binary to run the smoke under the lease"}
	}
	tmp, err := os.MkdirTemp("", "lo-gate-")
	if err != nil {
		return GateResult{Verdict: GateInconclusive, Reason: "temp dir: " + err.Error()}
	}
	defer os.RemoveAll(tmp)
	jobPath, resPath := filepath.Join(tmp, "job.json"), filepath.Join(tmp, "result.json")
	bare := *seat
	bare.Env = nil
	b, _ := json.Marshal(smokeJob{Seat: &bare, YAML: g.YAML, Live: in.Live, Candidate: id, StagedDir: in.StagedDir,
		PreviousDir: in.PreviousDir, Port: port, LoadTimeout: g.Cfg.LoadTimeoutOr()})
	if err := os.WriteFile(jobPath, b, 0o600); err != nil {
		return GateResult{Verdict: GateInconclusive, Reason: "temp file: " + err.Error()}
	}

	wait := g.Cfg.LeaseWaitOr()
	args := []string{"gpu", "reserve", "--class", "text", "--for", (2*g.Cfg.LoadTimeoutOr() + 20*time.Minute).String(),
		"--reason", "update smoke gate: llama " + id, "--origin", "local-offload update",
		"--wait", wait.String(), "--drain", "--unload-seat"}
	if g.ConfigPath != "" {
		args = append(args, "--config", g.ConfigPath)
	}
	args = append(args, "--", g.Self, "update", "--gate-child", jobPath, "--gate-result", resPath)
	g.logf("gate: taking the GPU lease (waiting up to %s, seats drained and unloaded), then measuring %s against the previous generation on side port %d", wait, id, port)
	cmd := winexec.CommandContext(ctx, g.Self, args...)
	tail := &lineTail{logf: g.logf}
	cmd.Stdout, cmd.Stderr = tail, tail
	runErr := cmd.Run()
	tail.flush()

	raw, rerr := os.ReadFile(resPath)
	if rerr != nil {
		why := "the GPU lease was not obtained within " + wait.String() + " or the seats could not be drained (the gate never ran)"
		if _, serr := os.Stat(resPath + ".started"); serr == nil {
			why = "the lease-held measurement stopped before it had an answer"
		}
		if runErr != nil {
			why += ": " + runErr.Error()
		}
		if t := tail.last(); t != "" {
			why += " [" + t + "]"
		}
		return GateResult{Verdict: GateInconclusive, Reason: why + "; nothing changed, retry later"}
	}
	var res GateResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return GateResult{Verdict: GateInconclusive, Reason: "unreadable gate result: " + err.Error()}
	}
	return res
}

// lineTail forwards each complete line to logf and remembers the last non-empty one.
type lineTail struct {
	logf func(string, ...any)
	buf  []byte
	prev string
}

func (t *lineTail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		t.line(string(t.buf[:i]))
		t.buf = t.buf[i+1:]
	}
	return len(p), nil
}

func (t *lineTail) line(s string) {
	s = strings.TrimRight(s, "\r")
	if strings.TrimSpace(s) == "" {
		return
	}
	t.prev = s
	if t.logf != nil {
		t.logf("  %s", s)
	}
}

func (t *lineTail) flush() {
	if len(t.buf) > 0 {
		t.line(string(t.buf))
		t.buf = nil
	}
}

func (t *lineTail) last() string { return truncate(strings.TrimSpace(t.prev), 160) }

// RunGateChild is the lease-held half: `update --gate-child <job> --gate-result <file>`. It measures, prints the
// table, and writes the verdict to the result file (the parent reads only that file).
func RunGateChild(ctx context.Context, jobPath, resPath string, w io.Writer) error {
	b, err := os.ReadFile(jobPath)
	if err != nil {
		return err
	}
	var job smokeJob
	if err := json.Unmarshal(b, &job); err != nil {
		return err
	}
	if job.YAML != "" && job.Seat != nil {
		if s, perr := PickSeat(job.YAML, job.Live); perr == nil && s.ID == job.Seat.ID {
			job.Seat = s // the seat's environment comes from the yaml, never from the job file
		}
	}
	_ = os.WriteFile(resPath+".started", []byte("1"), 0o600)
	res := measureGate(ctx, job, w)
	out, _ := json.Marshal(res)
	return os.WriteFile(resPath, out, 0o600)
}

// ---------------------------------------------------------------------------------------------------------------
// The seat the gate copies.

// Seat is one llama-server seat of the rendered llama-swap yaml.
type Seat struct {
	ID    string
	Argv  []string // the expanded cmd line, argv[Exe] is the llama-server program
	Exe   int
	Env   []string
	Model string
	Size  int64
}

// command is the seat's command line with this generation's llama-server, bound to 127.0.0.1:port.
func (s *Seat) command(bin string, port int) []string {
	var out []string
	for i := 0; i < len(s.Argv); i++ {
		a := s.Argv[i]
		switch {
		case i == s.Exe:
			out = append(out, bin)
		case a == "--port" || a == "--host":
			i++
		case strings.HasPrefix(a, "--port=") || strings.HasPrefix(a, "--host="):
		default:
			out = append(out, a)
		}
	}
	return append(out, "--port", strconv.Itoa(port), "--host", "127.0.0.1")
}

// splitArgs splits a cmd line on blanks, honouring single and double quotes (no escapes: Windows paths use
// backslashes).
func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	var q rune
	has := false
	for _, r := range s {
		switch {
		case q != 0:
			if r == q {
				q = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			q, has = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if has || cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
		}
	}
	if has || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func flagValue(argv []string, names ...string) string {
	for i, a := range argv {
		for _, n := range names {
			if a == n && i+1 < len(argv) {
				return argv[i+1]
			}
			if strings.HasPrefix(a, n+"=") {
				return strings.TrimPrefix(a, n+"=")
			}
		}
	}
	return ""
}

func hasAnyFlag(argv []string, names ...string) bool {
	for _, a := range argv {
		for _, n := range names {
			if a == n || strings.HasPrefix(a, n+"=") {
				return true
			}
		}
	}
	return false
}

// PickSeat chooses the seat the gate and the post-flip watch copy: among the yaml's llama-server chat seats that
// run from the live path (embedding and reranking seats cannot answer a chat completion), a resident one (ttl 0
// or below) first, else the one with the smallest model file, so the load is the cheapest representative one.
func PickSeat(yamlPath, live string) (*Seat, error) {
	if yamlPath == "" {
		return nil, errors.New("no llama-swap yaml configured")
	}
	raw, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("%s not readable", filepath.Base(yamlPath))
	}
	var cfg swapYAML
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("%s does not parse as yaml", filepath.Base(yamlPath))
	}
	names := make([]string, 0, len(cfg.Models))
	for n := range cfg.Models {
		names = append(names, n)
	}
	sort.Strings(names)
	liveNorm := normPath(live)
	var best *Seat
	bestRes := false
	for _, n := range names {
		m := cfg.Models[n]
		cmd := expandMacros(m.Cmd, cfg.Macros)
		if !strings.Contains(normPath(cmd), "llama-server") || !strings.Contains(normPath(cmd), liveNorm+"/") {
			continue
		}
		argv := splitArgs(cmd)
		if hasAnyFlag(argv, "--embedding", "--embeddings", "--rerank", "--reranking") {
			continue
		}
		exe := -1
		for i, a := range argv {
			if strings.Contains(normPath(a), "llama-server") {
				exe = i
				break
			}
		}
		model := flagValue(argv, "-m", "--model")
		if exe < 0 || model == "" {
			continue
		}
		st, serr := os.Stat(model)
		if serr != nil || st.IsDir() {
			continue
		}
		s := &Seat{ID: n, Argv: argv, Exe: exe, Env: m.Env, Model: model, Size: st.Size()}
		res := m.TTL != nil && *m.TTL <= 0
		if best == nil || (res && !bestRes) || (res == bestRes && s.Size < best.Size) {
			best, bestRes = s, res
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no chat seat of %s runs %s", filepath.Base(yamlPath), live)
	}
	return best, nil
}

// ---------------------------------------------------------------------------------------------------------------
// One measured run.

// sample is what one llama-server generation measured.
type sample struct {
	LoadS      float64
	PrefillTPS float64
	DecodeTPS  float64
	VRAMMiB    float64
	HaveVRAM   bool
	Text       string
	Fault      string // "" when the run completed
	Bad        bool   // the fault is the build's (no load, wrong output), not the harness's
}

const (
	readyPrompt = "Reply with the single word: ready"
	// timedPrompt is a fixed ~500 token prefill plus a request for 64 tokens of decode: a one-word answer is too
	// short to time.
	timedPromptHead = "The quick brown fox jumps over the lazy dog while the five boxing wizards jump quickly. "
	timedPromptTail = "Now count upward from 1 to 100, separated by commas, with no other text."
)

func timedPrompt() string { return strings.Repeat(timedPromptHead, 40) + timedPromptTail }

var localHTTP *http.Client

// localClient is the loopback client for the side-port server and the node's own llama-swap (the publisher client
// of Env refuses loopback by design).
func localClient() *http.Client {
	if localHTTP == nil {
		localHTTP = &http.Client{}
	}
	return localHTTP
}

type chatResult struct {
	Status  int
	Text    string
	Prefill float64
	Decode  float64
	Err     string
	// Soft is a failure that says nothing about the build: the request timed out, or the server reported it ran out
	// of memory (another process holds the VRAM).
	Soft bool
}

// chat sends one completion to url: the gate's side-port server (sideChatURL) or llama-swap's route as the fence
// returned it (WatchFirstLoad). It never builds a llama-swap route itself.
func chat(ctx context.Context, url, model, prompt string, maxTokens int, noCache bool, timeout time.Duration) chatResult {
	body := map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": maxTokens, "temperature": 0, "stream": false}
	if noCache {
		body["cache_prompt"] = false
	}
	b, _ := json.Marshal(body)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return chatResult{Err: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := localClient().Do(req)
	if err != nil {
		var ne net.Error
		return chatResult{Err: err.Error(), Soft: errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	r := chatResult{Status: resp.StatusCode}
	if resp.StatusCode != http.StatusOK {
		r.Err = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(data)), 160))
		r.Soft = outOfMemory(string(data))
		return r
	}
	var cr struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Timings struct {
			PromptPerSecond    float64 `json:"prompt_per_second"`
			PredictedPerSecond float64 `json:"predicted_per_second"`
		} `json:"timings"`
	}
	if err := json.Unmarshal(data, &cr); err != nil {
		r.Err = "unreadable reply: " + err.Error()
		return r
	}
	if len(cr.Choices) > 0 {
		r.Text = strings.TrimSpace(cr.Choices[0].Message.Content)
		if r.Text == "" {
			r.Text = strings.TrimSpace(cr.Choices[0].Message.ReasoningContent)
		}
	}
	r.Prefill, r.Decode = cr.Timings.PromptPerSecond, cr.Timings.PredictedPerSecond
	return r
}

// outOfMemory reports whether a server's answer says it could not get the memory to load (VRAM held by another
// process), as opposed to a build that crashes or answers wrongly.
func outOfMemory(s string) bool {
	low := strings.ToLower(s)
	for _, m := range []string{"out of memory", "cudamalloc failed", "failed to allocate", "unable to allocate", "insufficient memory"} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

func median(v []float64) float64 {
	var xs []float64
	for _, x := range v {
		if x > 0 {
			xs = append(xs, x)
		}
	}
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	return xs[len(xs)/2]
}

// vramUsedMiB sums memory.used over the node's NVIDIA cards (nvidia-smi), false when there is no reading.
func vramUsedMiB(ctx context.Context) (float64, bool) {
	smi, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return 0, false
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := winexec.CommandContext(cctx, smi, "--query-gpu=memory.used", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, false
	}
	total, n := 0.0, 0
	for _, ln := range strings.Split(string(out), "\n") {
		if v, perr := strconv.ParseFloat(strings.TrimSpace(ln), 64); perr == nil {
			total += v
			n++
		}
	}
	return total, n > 0
}

func portFree(p int) bool {
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// freePort is the configured side port, or the next free one (skipping the forbidden ports).
func freePort(p int) (int, error) {
	for q := p; q < p+25 && q < 65536; q++ {
		if !forbiddenPorts[q] && portFree(q) {
			return q, nil
		}
	}
	return 0, fmt.Errorf("no free side port from %d", p)
}

// measure runs the seat once with the llama-server in dir.
func measure(ctx context.Context, label, dir string, seat *Seat, port int, loadTimeout time.Duration) (s sample) {
	bin := binaryIn(defs[Llama], dir)
	if bin == "" {
		return sample{Fault: "no llama-server under " + dir, Bad: true}
	}
	p, err := freePort(port)
	if err != nil {
		return sample{Fault: err.Error()}
	}
	base := "http://127.0.0.1:" + strconv.Itoa(p)
	baseline, haveBase := vramUsedMiB(ctx)

	logf, lerr := os.CreateTemp("", "lo-gate-server-*.log")
	if lerr != nil {
		return sample{Fault: "temp log: " + lerr.Error()}
	}
	defer os.Remove(logf.Name())
	defer logf.Close()
	argv := seat.command(bin, p)
	cmd := winexec.Command(argv[0], argv[1:]...)
	cmd.Dir = filepath.Dir(bin)
	cmd.Env = append(os.Environ(), seat.Env...)
	if runtime.GOOS != "windows" {
		cmd.Env = append(cmd.Env, "LD_LIBRARY_PATH="+filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("LD_LIBRARY_PATH"))
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return sample{Fault: "start: " + err.Error(), Bad: true}
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	defer func() { // every idle model unloads: the side-port server never outlives the measurement
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(30 * time.Second):
		}
	}()
	logTail := func() string {
		b, _ := os.ReadFile(logf.Name())
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) > 3 {
			lines = lines[len(lines)-3:]
		}
		return truncate(strings.Join(lines, " | "), 300)
	}

	// /health ok
	deadline := time.Now().Add(loadTimeout)
	ready := false
	for !ready {
		select {
		case <-exited:
			tail := logTail() // out of memory says another process holds the card, not that the build is bad
			return sample{Fault: "the " + label + " llama-server exited while loading: " + tail, Bad: !outOfMemory(tail)}
		case <-ctx.Done():
			return sample{Fault: "cancelled"}
		default:
		}
		if time.Now().After(deadline) {
			// A slow load is not a verdict on the build (a busy disk, a card another process holds): inconclusive, so
			// nothing is promoted and the next run measures again.
			return sample{Fault: fmt.Sprintf("the %s llama-server did not answer /health within %s: %s", label, loadTimeout, logTail())}
		}
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		req, _ := http.NewRequestWithContext(rctx, http.MethodGet, base+"/health", nil)
		if resp, herr := localClient().Do(req); herr == nil {
			ready = resp.StatusCode == http.StatusOK
			_ = resp.Body.Close()
		}
		cancel()
		if !ready {
			time.Sleep(500 * time.Millisecond)
		}
	}
	s.LoadS = time.Since(start).Seconds()

	// One fixed completion: the answer must exist.
	r := chat(ctx, sideChatURL(base), seat.ID, readyPrompt, 32, false, 3*time.Minute)
	if r.Err != "" {
		s.Fault, s.Bad = "completion failed: "+r.Err, !r.Soft
		return s
	}
	if r.Text == "" {
		s.Fault, s.Bad = "the completion text is empty", true
		return s
	}
	s.Text = r.Text

	// Three timed runs of the fixed long prompt, median taken (a tiny prompt's rates are noise).
	var pre, dec []float64
	for i := 0; i < 3; i++ {
		t := chat(ctx, sideChatURL(base), seat.ID, timedPrompt(), 64, true, 3*time.Minute)
		if t.Err != "" {
			s.Fault, s.Bad = "timed completion failed: "+t.Err, !t.Soft
			return s
		}
		pre, dec = append(pre, t.Prefill), append(dec, t.Decode)
	}
	s.PrefillTPS, s.DecodeTPS = median(pre), median(dec)
	if used, ok := vramUsedMiB(ctx); ok && haveBase {
		s.VRAMMiB, s.HaveVRAM = used-baseline, true
	}
	return s
}

// judge turns the two samples into the verdict and the numbers recorded in the generation's meta file.
func judge(cand sample, prev *sample) GateResult {
	nums := map[string]float64{"cand_load_s": round1(cand.LoadS)}
	if cand.Fault != "" {
		if cand.Bad {
			return GateResult{Verdict: GateReject, Reason: "candidate: " + cand.Fault, Numbers: nums}
		}
		return GateResult{Verdict: GateInconclusive, Reason: "candidate: " + cand.Fault, Numbers: nums}
	}
	nums["cand_prefill_tps"], nums["cand_decode_tps"] = round1(cand.PrefillTPS), round1(cand.DecodeTPS)
	if cand.HaveVRAM {
		nums["cand_vram_mib"] = round1(cand.VRAMMiB)
	}
	if prev == nil {
		return GateResult{Verdict: GatePass, Reason: "candidate loads and answers (no previous generation to compare)", Numbers: nums}
	}
	nums["prev_load_s"] = round1(prev.LoadS)
	if prev.Fault != "" {
		return GateResult{Verdict: GatePass, Reason: "candidate loads and answers; the previous generation could not be measured (" + prev.Fault + ")", Numbers: nums}
	}
	nums["prev_prefill_tps"], nums["prev_decode_tps"] = round1(prev.PrefillTPS), round1(prev.DecodeTPS)
	if prev.HaveVRAM {
		nums["prev_vram_mib"] = round1(prev.VRAMMiB)
	}
	var bad []string
	if prev.DecodeTPS > 0 && cand.DecodeTPS > 0 && cand.DecodeTPS < prev.DecodeTPS*(1-MaxSlowdown) {
		bad = append(bad, fmt.Sprintf("decode %.1f tok/s is %.0f%% below the previous %.1f tok/s (limit %.0f%%)",
			cand.DecodeTPS, 100*(1-cand.DecodeTPS/prev.DecodeTPS), prev.DecodeTPS, 100*MaxSlowdown))
	}
	if prev.PrefillTPS > 0 && cand.PrefillTPS > 0 && cand.PrefillTPS < prev.PrefillTPS*(1-MaxSlowdown) {
		bad = append(bad, fmt.Sprintf("prefill %.1f tok/s is %.0f%% below the previous %.1f tok/s (limit %.0f%%)",
			cand.PrefillTPS, 100*(1-cand.PrefillTPS/prev.PrefillTPS), prev.PrefillTPS, 100*MaxSlowdown))
	}
	if cand.HaveVRAM && prev.HaveVRAM && prev.VRAMMiB > 0 && cand.VRAMMiB > prev.VRAMMiB*(1+MaxVRAMGrowth) && cand.VRAMMiB-prev.VRAMMiB > vramNoiseMiB {
		bad = append(bad, fmt.Sprintf("VRAM %.0f MiB is %.0f%% above the previous %.0f MiB (limit %.0f%%)",
			cand.VRAMMiB, 100*(cand.VRAMMiB/prev.VRAMMiB-1), prev.VRAMMiB, 100*MaxVRAMGrowth))
	}
	if len(bad) > 0 {
		return GateResult{Verdict: GateReject, Reason: strings.Join(bad, "; "), Numbers: nums}
	}
	return GateResult{Verdict: GatePass, Reason: "candidate loads, answers and is within " + fmt.Sprintf("%.0f%% speed and %.0f%% VRAM of the previous generation", 100*MaxSlowdown, 100*MaxVRAMGrowth), Numbers: nums}
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

func (s sample) row(label string) string {
	if s.Fault != "" {
		return fmt.Sprintf("gate %-9s FAILED: %s", label, s.Fault)
	}
	v := "n/a"
	if s.HaveVRAM {
		v = fmt.Sprintf("%.0f MiB", s.VRAMMiB)
	}
	return fmt.Sprintf("gate %-9s load %5.1f s   prefill %8.1f tok/s   decode %7.1f tok/s   vram %s   answer %q",
		label, s.LoadS, s.PrefillTPS, s.DecodeTPS, v, truncate(s.Text, 24))
}

// measureGate runs the candidate, then the previous generation, back to back on the same seat.
func measureGate(ctx context.Context, job smokeJob, w io.Writer) GateResult {
	fmt.Fprintf(w, "gate: seat %s (%s), side port %d\n", job.Seat.ID, filepath.Base(job.Seat.Model), job.Port)
	cand := measure(ctx, "candidate", job.StagedDir, job.Seat, job.Port, job.LoadTimeout)
	fmt.Fprintln(w, cand.row("candidate"))
	var prev *sample
	if job.PreviousDir != "" && cand.Fault == "" {
		time.Sleep(3 * time.Second) // let the first server's memory go back before the baseline of the second
		p := measure(ctx, "previous", job.PreviousDir, job.Seat, job.Port, job.LoadTimeout)
		fmt.Fprintln(w, p.row("previous"))
		prev = &p
	}
	res := judge(cand, prev)
	fmt.Fprintf(w, "gate: %s\n", res.Reason)
	return res
}
