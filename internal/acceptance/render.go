package acceptance

// The pieces of `local-offload acceptance --render` that need no config: the known-answer
// assets, the subprocess runner and the checks on what a call left behind. The route table
// itself lives beside the command (acceptance_render.go), because it reads the config and the
// CLI verbs.

import (
	"bytes"
	"context"
	_ "embed" // the assets below
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// sign.png: 720x240, the text "OFFLOAD HARNESS 780M" and a red circle.
//
//go:embed assets/sign.png
var signPNG []byte

// jfk.wav is the public-domain sample whisper.cpp ships: a US president's inaugural address
// ("ask not what your country can do for you").
//
//go:embed assets/jfk.wav
var speechWAV []byte

// Paragraph is the text the summarize and classify calls read. The words are what a summary
// of it must name.
const Paragraph = "The old lighthouse on Cape Verdant was built in 1892 to warn ships away from the rocks. " +
	"Its keeper, Marta Olsen, climbed 212 steps every evening to light the lamp. " +
	"In 1974 the lamp was automated, and the tower became a small museum about the sea."

// ParagraphWords are the words of Paragraph a summary may name (lower case).
var ParagraphWords = []string{"lighthouse", "keeper", "lamp", "museum", "ships", "olsen", "verdant"}

// AgentContract is the one-subtask contract the agent call sends through `delegate`.
const AgentContract = `{"goal":"What is 17 times 23? Answer with the number only.","output_schema":{"properties":{"reply":{"type":"string"}}}}`

// Assets are the files a render run writes into its output folder.
type Assets struct {
	Sign      string // the PNG
	Speech    string // the WAV
	Mask      string // white square on black, the size of Sign
	Paragraph string // text file
	Contract  string // the agent contract
}

// AssetPaths names the asset files under dir without writing anything.
func AssetPaths(dir string) Assets {
	return Assets{
		Sign:      filepath.Join(dir, "sign.png"),
		Speech:    filepath.Join(dir, "speech.wav"),
		Mask:      filepath.Join(dir, "mask.png"),
		Paragraph: filepath.Join(dir, "paragraph.txt"),
		Contract:  filepath.Join(dir, "agent-contract.json"),
	}
}

// WriteAssets writes the assets into dir (created if missing) and returns their paths. The mask is
// built from the PNG's own size: a white square over its middle half.
func WriteAssets(dir string) (Assets, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Assets{}, err
	}
	a := AssetPaths(dir)
	cfg, err := png.DecodeConfig(bytes.NewReader(signPNG))
	if err != nil {
		return a, fmt.Errorf("embedded sign.png: %w", err)
	}
	mask := image.NewGray(image.Rect(0, 0, cfg.Width, cfg.Height))
	for y := cfg.Height / 4; y < cfg.Height*3/4; y++ {
		for x := cfg.Width / 4; x < cfg.Width*3/4; x++ {
			mask.SetGray(x, y, color.Gray{Y: 255})
		}
	}
	var mb bytes.Buffer
	if err := png.Encode(&mb, mask); err != nil {
		return a, err
	}
	for path, b := range map[string][]byte{
		a.Sign: signPNG, a.Speech: speechWAV, a.Mask: mb.Bytes(),
		a.Paragraph: []byte(Paragraph + "\n"), a.Contract: []byte(AgentContract + "\n"),
	} {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return a, err
		}
	}
	return a, nil
}

// Outcome is one finished subprocess.
type Outcome struct {
	Stdout, Stderr string
	Exit           int
	Wall           time.Duration
	TimedOut       bool
	StartErr       error // the program could not be started
}

// Run executes exe with args and captures both streams. The whole process tree is ended when
// the timeout passes. The caller's environment is kept: the call must go through the same doors
// (config, lease, admission) a real caller does.
func Run(ctx context.Context, timeout time.Duration, exe string, args ...string) Outcome {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := winexec.KillTreeOnCancel(winexec.CommandContext(cctx, exe, args...))
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	cmd.WaitDelay = 15 * time.Second
	start := time.Now()
	err := cmd.Run()
	o := Outcome{Stdout: so.String(), Stderr: se.String(), Wall: time.Since(start)}
	var ee *exec.ExitError
	switch {
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		o.TimedOut, o.Exit = true, -1
	case errors.As(err, &ee):
		o.Exit = ee.ExitCode()
	case err != nil:
		o.StartErr = err
	}
	return o
}

// Tail is the last n bytes of s, trimmed, for a failure detail.
func Tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	return strings.ReplaceAll(s, "\n", " ")
}

// JSONObject decodes the first JSON object in s into v: a verb may print a note before its JSON.
func JSONObject(s string, v any) error {
	i := strings.Index(s, "{")
	if i < 0 {
		return errors.New("no JSON object on stdout")
	}
	return json.NewDecoder(strings.NewReader(s[i:])).Decode(v)
}

// HasAll reports the first of words that text (compared lower case) lacks, or "" when it has all.
func HasAll(text string, words ...string) string {
	t := strings.ToLower(text)
	for _, w := range words {
		if !strings.Contains(t, strings.ToLower(w)) {
			return w
		}
	}
	return ""
}

// HasAny returns the first of words that text (compared lower case) contains, or "".
func HasAny(text string, words ...string) string {
	t := strings.ToLower(text)
	for _, w := range words {
		if strings.Contains(t, strings.ToLower(w)) {
			return w
		}
	}
	return ""
}

// PNGSize decodes the PNG at path and returns its size.
func PNGSize(path string) (w, h int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return 0, 0, fmt.Errorf("%s does not decode as a PNG: %w", filepath.Base(path), err)
	}
	b := img.Bounds()
	return b.Dx(), b.Dy(), nil
}

// NotBlank decodes the PNG at path and requires a picture: the file over 20 KB, and more than 8
// distinct colours on a 16 by 16 grid of samples. It returns the distinct count.
func NotBlank(path string) (int, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if fi.Size() <= 20*1024 {
		return 0, fmt.Errorf("%s is %d bytes, a picture is over 20 KB", filepath.Base(path), fi.Size())
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return 0, fmt.Errorf("%s does not decode as a PNG: %w", filepath.Base(path), err)
	}
	b := img.Bounds()
	seen := map[[4]uint32]bool{}
	for gy := 0; gy < 16; gy++ {
		for gx := 0; gx < 16; gx++ {
			r, g, bl, a := img.At(b.Min.X+(2*gx+1)*b.Dx()/32, b.Min.Y+(2*gy+1)*b.Dy()/32).RGBA()
			seen[[4]uint32{r, g, bl, a}] = true
		}
	}
	if len(seen) <= 8 {
		return len(seen), fmt.Errorf("%s looks blank: %d distinct colours on a 16x16 grid, need more than 8", filepath.Base(path), len(seen))
	}
	return len(seen), nil
}

// Probe runs ffprobe on path and requires a stream of the given kind ("video" or "audio") and a
// positive duration. It returns the duration in seconds.
func Probe(ctx context.Context, ffprobe, path, kind string) (float64, error) {
	if ffprobe == "" {
		return 0, errors.New("ffprobe not found next to ffmpeg_path or on PATH")
	}
	o := Run(ctx, 2*time.Minute, ffprobe, "-v", "error", "-show_entries", "stream=codec_type,duration:format=duration", "-of", "json", path)
	if o.StartErr != nil || o.Exit != 0 {
		return 0, fmt.Errorf("ffprobe %s failed: %s", filepath.Base(path), Tail(o.Stderr+fmt.Sprint(o.StartErr), 200))
	}
	var out struct {
		Streams []struct {
			Type     string `json:"codec_type"`
			Duration string `json:"duration"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal([]byte(o.Stdout), &out); err != nil {
		return 0, fmt.Errorf("ffprobe %s: unreadable answer: %w", filepath.Base(path), err)
	}
	dur, _ := strconv.ParseFloat(out.Format.Duration, 64)
	found := false
	for _, s := range out.Streams {
		if s.Type != kind {
			continue
		}
		found = true
		if d, err := strconv.ParseFloat(s.Duration, 64); err == nil && dur <= 0 {
			dur = d
		}
	}
	if !found {
		return 0, fmt.Errorf("%s has no %s stream", filepath.Base(path), kind)
	}
	if dur <= 0 {
		return 0, fmt.Errorf("%s has a %s stream but no duration", filepath.Base(path), kind)
	}
	return dur, nil
}
