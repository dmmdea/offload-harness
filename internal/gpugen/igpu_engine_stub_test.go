package gpugen

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The iGPU runners (render/sdcpp-video.mjs, sdcpp-animate.mjs, audiocpp-generate.mjs) are tested
// here END TO END through Generate and ClassifyErr: the real runner script runs under node, and the
// engine it spawns is THIS test binary acting as the engine (TestMain below), so no engine binary,
// shell or GPU is needed and the same stub works on Windows and Linux. A spec file named by the
// GPUGEN_IGPU_ENGINE_SPEC env var says what the engine does: replay a captured log, exit with a
// code, die of a signal, write the file a real engine would, or hang.

const engineSpecEnv = "GPUGEN_IGPU_ENGINE_SPEC"

type stubWrite struct {
	Kind    string  `json:"kind"` // "video" | "wav"
	Seconds float64 `json:"seconds"`
	Silent  bool    `json:"silent"`
	Black   bool    `json:"black"`
	Frozen  bool    `json:"frozen"`
	FFmpeg  string  `json:"ffmpeg"`
}

type stubEngine struct {
	LogFile string     `json:"logFile"`
	Log     []string   `json:"log"`
	Exit    int        `json:"exit"`
	Signal  string     `json:"signal"` // die of this signal after the log (unix only)
	Hang    bool       `json:"hang"`
	PidFile string     `json:"pidFile"`
	Write   *stubWrite `json:"write"`
}

// stubSpec holds the main engine and, for the animate runner, the depth engine (argv[1] == "depth").
type stubSpec struct {
	Main  stubEngine `json:"main"`
	Depth stubEngine `json:"depth"`
}

func TestMain(m *testing.M) {
	if p := os.Getenv(engineSpecEnv); p != "" {
		os.Exit(runStubEngine(p))
	}
	os.Exit(m.Run())
}

func argAfter(argv []string, flag string) string {
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}

func runStubEngine(specPath string) int {
	b, err := os.ReadFile(specPath)
	if err != nil {
		os.Stderr.WriteString("stub engine: " + err.Error() + "\n")
		return 97
	}
	var sp stubSpec
	if err := json.Unmarshal(b, &sp); err != nil {
		os.Stderr.WriteString("stub engine: " + err.Error() + "\n")
		return 97
	}
	argv := os.Args[1:]
	e := sp.Main
	if len(argv) > 0 && argv[0] == "depth" {
		e = sp.Depth
	}
	if e.PidFile != "" {
		_ = os.WriteFile(e.PidFile, []byte(strconv.Itoa(os.Getpid())), 0o644)
	}
	if e.LogFile != "" {
		if lb, err := os.ReadFile(e.LogFile); err == nil {
			os.Stderr.Write(lb)
		}
	}
	for _, l := range e.Log {
		os.Stderr.WriteString(l + "\n")
	}
	if w := e.Write; w != nil {
		switch w.Kind {
		case "video":
			out := argAfter(argv, "-o")
			size := argAfter(argv, "-W") + "x" + argAfter(argv, "-H")
			fps := argAfter(argv, "--fps")
			frames := argAfter(argv, "--video-frames")
			src := "testsrc2=s=" + size + ":r=" + fps
			if w.Black {
				src = "color=c=black:s=" + size + ":r=" + fps
			} else if w.Frozen {
				src = "color=c=0x2060c0:s=" + size + ":r=" + fps
			}
			cmd := exec.Command(w.FFmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", src, "-frames:v", frames, "-an", out)
			if o, err := cmd.CombinedOutput(); err != nil {
				os.Stderr.WriteString("stub engine: ffmpeg failed: " + string(o) + "\n")
				return 99
			}
		case "wav":
			_ = os.WriteFile(argAfter(argv, "--out"), pcmWav(w.Seconds, w.Silent), 0o644)
		}
	}
	if e.Signal != "" {
		selfSignal(e.Signal)
	}
	if e.Hang {
		time.Sleep(time.Hour)
	}
	return e.Exit
}

// pcmWav is a mono PCM16 wav at 24 kHz: a 440 Hz tone, or zeros when silent.
func pcmWav(seconds float64, silent bool) []byte {
	const rate = 24000
	n := int(seconds * rate)
	pcm := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := 0
		if !silent {
			v = int(math.Sin(2*math.Pi*440*float64(i)/rate) * 9000)
		}
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(v)))
	}
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(pcm)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], 1)
	binary.LittleEndian.PutUint32(h[24:], rate)
	binary.LittleEndian.PutUint32(h[28:], rate*2)
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(pcm)))
	return append(h, pcm...)
}

// ---------------------------------------------------------------- the harness side

// igpuBox is one test's sandbox: a work dir, a private TEMP for the runner, the spec file, ffmpeg.
type igpuBox struct {
	t       *testing.T
	root    string
	work    string
	priv    string
	spec    stubSpec
	ffmpeg  string // real ffmpeg ("" = none)
	fakeBin string // a file that exists, standing in for ffmpeg/ffprobe when none is needed
}

func runnerScript(name string) string {
	p, err := filepath.Abs(filepath.Join("..", "..", "render", name))
	if err != nil {
		panic(err)
	}
	return p
}

func fixturePath(name string) string {
	p, err := filepath.Abs(filepath.Join("..", "..", "render", "testdata", name))
	if err != nil {
		panic(err)
	}
	return p
}

func newIGPUBox(t *testing.T) *igpuBox {
	t.Helper()
	requireNode(t)
	root := t.TempDir()
	b := &igpuBox{t: t, root: root, work: filepath.Join(root, "work"), priv: filepath.Join(root, "tmp")}
	for _, d := range []string{b.work, b.priv} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		if _, perr := exec.LookPath("ffprobe"); perr == nil {
			b.ffmpeg = p
		}
	}
	// A directory with an ffmpeg and an ffprobe that EXIST (copies of this binary): the runners resolve
	// both before any engine starts, and a typed failure that fires earlier never runs them.
	fake := filepath.Join(root, "fake")
	if err := os.MkdirAll(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	ext := filepath.Ext(self)
	for _, n := range []string{"ffmpeg", "ffprobe"} {
		data, err := os.ReadFile(self)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fake, n+ext), data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	b.fakeBin = filepath.Join(fake, "ffmpeg"+ext)
	return b
}

func (b *igpuBox) needFFmpeg() {
	b.t.Helper()
	if b.ffmpeg == "" {
		b.t.Skip("no ffmpeg/ffprobe on this host; this row measures a real clip or wav")
	}
}

// file writes a placeholder file in the work dir and returns its path.
func (b *igpuBox) file(name string) string {
	p := filepath.Join(b.work, name)
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		b.t.Fatal(err)
	}
	return p
}

// env is the runner's environment: a private TEMP, a closed llama-swap port, the engine spec.
func (b *igpuBox) env() []string {
	sp, _ := json.Marshal(b.spec)
	specPath := filepath.Join(b.work, "engine.spec.json")
	if err := os.WriteFile(specPath, sp, 0o644); err != nil {
		b.t.Fatal(err)
	}
	ff := b.fakeBin
	if b.ffmpeg != "" {
		ff = b.ffmpeg
	}
	return []string{"TEMP=" + b.priv, "TMP=" + b.priv, "TMPDIR=" + b.priv, "LLAMA_SWAP_API=http://127.0.0.1:9",
		"FFMPEG_PATH=" + ff, "IGPU_PARENT_POLL_MS=250", engineSpecEnv + "=" + specPath}
}

func (b *igpuBox) selfBin() string {
	p, err := os.Executable()
	if err != nil {
		b.t.Fatal(err)
	}
	return p
}

// videoRun is the argv of sdcpp-video.mjs for a 5-frame 64x64 clip; `over` replaces a flag's value
// (or appends the flag), `extra` is appended before the `--` terminator.
func (b *igpuBox) videoArgs(out string, over map[string]string, extra ...string) []string {
	flags := map[string]string{
		"--sd-bin": b.selfBin(), "--model": b.file("model.gguf"), "--vae": b.file("vae.safetensors"), "--t5xxl": b.file("t5.gguf"),
		"--backend": "vulkan0", "--frames": "5", "--width": "64", "--height": "64", "--fps": "8", "--seed": "7",
	}
	for k, v := range over {
		flags[k] = v
	}
	var args []string
	for _, k := range []string{"--sd-bin", "--model", "--vae", "--t5xxl", "--backend", "--frames", "--width", "--height", "--fps", "--seed"} {
		args = append(args, k, flags[k])
	}
	args = append(args, "--no-lock")
	args = append(args, extra...)
	return append(args, "--", out, b.file("still.png"), "--- Intro --- a calm sea")
}

func (b *igpuBox) audioArgs(out, kind string, over map[string]string, extra ...string) []string {
	flags := map[string]string{"--kind": kind, "--bin": b.selfBin(), "--family": "ace_step", "--model": b.file("model.gguf"),
		"--backend": "vulkan", "--device": "0", "--seed": "5"}
	for k, v := range over {
		flags[k] = v
	}
	var args []string
	for _, k := range []string{"--kind", "--bin", "--family", "--model", "--backend", "--device", "--seed"} {
		args = append(args, k, flags[k])
	}
	args = append(args, "--no-lock")
	args = append(args, extra...)
	return append(args, "--", out, "--- Intro --- warm lo-fi bed")
}

// run drives the runner through Generate (a 120 s budget) and returns its error; nil = the runner
// wrote the output.
func (b *igpuBox) run(script string, args []string, out string) error {
	return b.runFor(script, args, out, 120*time.Second)
}

func (b *igpuBox) runFor(script string, args []string, out string, timeout time.Duration) error {
	b.t.Helper()
	_, err := Generate(context.Background(), Spec{Exe: "node", Script: runnerScript(script), Args: args, Env: b.env(), Out: out,
		Timeout: timeout, SkipFreeComfy: true, OwnProcessGroup: true})
	return err
}

// rawFinalLine runs the runner directly and returns its last FAILED line (to measure it).
func (b *igpuBox) rawFinal(script string, args []string) string {
	b.t.Helper()
	cmd := exec.Command("node", append([]string{runnerScript(script)}, args...)...)
	cmd.Env = append(os.Environ(), b.env()...)
	out, _ := cmd.CombinedOutput()
	var last string
	for _, l := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		if strings.Contains(l, " FAILED: ") {
			last = l
		}
	}
	return last
}
