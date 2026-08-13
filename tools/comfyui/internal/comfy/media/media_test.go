package media

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Captured from `ffprobe -hide_banner -v error -print_format json -show_format
// -show_streams` on real ComfyUI outputs. These payloads are the whole point of
// the injectable runner: the tests below parse them with no ffprobe installed.
const (
	// Wan2.2 I2V export: h264 video + aac audio, NTSC-ish rational frame rate.
	fixtureVideoWithAudio = `{
  "streams": [
    {
      "index": 0,
      "codec_name": "h264",
      "codec_type": "video",
      "width": 1280,
      "height": 720,
      "r_frame_rate": "30000/1001",
      "avg_frame_rate": "30000/1001",
      "duration": "5.005000",
      "nb_frames": "150"
    },
    {
      "index": 1,
      "codec_name": "aac",
      "codec_type": "audio",
      "channels": 2,
      "r_frame_rate": "0/0",
      "avg_frame_rate": "0/0",
      "duration": "5.015000"
    }
  ],
  "format": {
    "filename": "ComfyUI_00042_.mp4",
    "nb_streams": 2,
    "format_name": "mov,mp4,m4a,3gp,3g2,mj2",
    "duration": "5.015000",
    "size": "2097152"
  }
}`

	// Silent webm: avg_frame_rate is 0/0, so the rate must fall back to
	// r_frame_rate rather than becoming 0 or dividing by zero.
	fixtureVideoNoAudio = `{
  "streams": [
    {
      "index": 0,
      "codec_name": "vp9",
      "codec_type": "video",
      "width": 832,
      "height": 480,
      "r_frame_rate": "16/1",
      "avg_frame_rate": "0/0",
      "nb_frames": "81"
    }
  ],
  "format": {
    "filename": "ComfyUI_00043_.webm",
    "nb_streams": 1,
    "format_name": "matroska,webm",
    "duration": "5.062000",
    "size": "1048576"
  }
}`

	// A still PNG. ffprobe FABRICATES 25/1 here and reports no format duration;
	// recording 25 fps for a PNG would poison a cross-model comparison.
	fixtureStillPNG = `{
  "streams": [
    {
      "index": 0,
      "codec_name": "png",
      "codec_type": "video",
      "width": 2048,
      "height": 1024,
      "r_frame_rate": "25/1",
      "avg_frame_rate": "25/1",
      "duration": "N/A",
      "nb_frames": "N/A"
    }
  ],
  "format": {
    "filename": "ComfyUI_00044_.png",
    "nb_streams": 1,
    "format_name": "png_pipe",
    "duration": "N/A",
    "size": "4194304"
  }
}`

	// Audio-only output (an audio node's wav). No video stream at all.
	fixtureAudioOnly = `{
  "streams": [
    {
      "index": 0,
      "codec_name": "pcm_s16le",
      "codec_type": "audio",
      "channels": 1,
      "duration": "3.200000"
    }
  ],
  "format": {
    "filename": "ComfyUI_00045_.wav",
    "nb_streams": 1,
    "format_name": "wav",
    "duration": "3.200000",
    "size": "102400"
  }
}`
)

func TestParseInfo(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    Info
	}{
		{
			name:    "video with audio",
			payload: fixtureVideoWithAudio,
			want: Info{
				Width: 1280, Height: 720,
				FPS: 30000.0 / 1001.0, DurationS: 5.015,
				HasAudio: true, StillImage: false,
				VideoCodec: "h264", AudioCodec: "aac",
				FormatName: "mov,mp4,m4a,3gp,3g2,mj2",
				Frames:     150, Bytes: 2097152,
			},
		},
		{
			name:    "silent webm falls back to r_frame_rate when avg is 0/0",
			payload: fixtureVideoNoAudio,
			want: Info{
				Width: 832, Height: 480,
				FPS: 16, DurationS: 5.062,
				HasAudio: false, StillImage: false,
				VideoCodec: "vp9",
				FormatName: "matroska,webm",
				Frames:     81, Bytes: 1048576,
			},
		},
		{
			name:    "still png reports no fps and no duration",
			payload: fixtureStillPNG,
			want: Info{
				Width: 2048, Height: 1024,
				FPS: 0, DurationS: 0,
				HasAudio: false, StillImage: true,
				VideoCodec: "png",
				FormatName: "png_pipe",
				Frames:     0, Bytes: 4194304,
			},
		},
		{
			name:    "audio only",
			payload: fixtureAudioOnly,
			want: Info{
				DurationS: 3.2,
				HasAudio:  true,
				// wav is not a still-image demuxer and there is no video
				// stream, so StillImage stays false.
				AudioCodec: "pcm_s16le",
				FormatName: "wav",
				Bytes:      102400,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseInfo([]byte(tc.payload))
			if err != nil {
				t.Fatalf("ParseInfo() error = %v", err)
			}
			if got.Width != tc.want.Width || got.Height != tc.want.Height {
				t.Errorf("dimensions = %dx%d, want %dx%d", got.Width, got.Height, tc.want.Width, tc.want.Height)
			}
			if math.Abs(got.FPS-tc.want.FPS) > 1e-6 {
				t.Errorf("FPS = %v, want %v", got.FPS, tc.want.FPS)
			}
			if math.Abs(got.DurationS-tc.want.DurationS) > 1e-6 {
				t.Errorf("DurationS = %v, want %v", got.DurationS, tc.want.DurationS)
			}
			if got.HasAudio != tc.want.HasAudio {
				t.Errorf("HasAudio = %v, want %v", got.HasAudio, tc.want.HasAudio)
			}
			if got.StillImage != tc.want.StillImage {
				t.Errorf("StillImage = %v, want %v", got.StillImage, tc.want.StillImage)
			}
			if got.VideoCodec != tc.want.VideoCodec || got.AudioCodec != tc.want.AudioCodec {
				t.Errorf("codecs = %q/%q, want %q/%q", got.VideoCodec, got.AudioCodec, tc.want.VideoCodec, tc.want.AudioCodec)
			}
			if got.FormatName != tc.want.FormatName {
				t.Errorf("FormatName = %q, want %q", got.FormatName, tc.want.FormatName)
			}
			if got.Frames != tc.want.Frames {
				t.Errorf("Frames = %d, want %d", got.Frames, tc.want.Frames)
			}
			if got.Bytes != tc.want.Bytes {
				t.Errorf("Bytes = %d, want %d", got.Bytes, tc.want.Bytes)
			}
		})
	}
}

func TestParseInfoErrors(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{"empty", ""},
		{"whitespace only", "   \n\t "},
		{"not json", "ffprobe: No such file or directory"},
		{"truncated json", `{"streams": [`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseInfo([]byte(tc.payload)); err == nil {
				t.Fatalf("ParseInfo(%q) = nil error, want error", tc.payload)
			}
		})
	}
}

func TestParseRational(t *testing.T) {
	tests := []struct {
		in     string
		want   float64
		wantOK bool
	}{
		{"30000/1001", 30000.0 / 1001.0, true},
		{"24/1", 24, true},
		{"16/1", 16, true},
		{"24", 24, true},
		{"23.976", 23.976, true},
		{"0/0", 0, false},
		{"1/0", 0, false},
		{"0/1", 0, false},
		{"N/A", 0, false},
		{"n/a", 0, false},
		{"", 0, false},
		{"   ", 0, false},
		{"abc", 0, false},
		{"-30/1", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := ParseRational(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ParseRational(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if ok && math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("ParseRational(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestFFprobeAvailability(t *testing.T) {
	tests := []struct {
		name          string
		look          LookPathFunc
		wantAvailable bool
		wantPath      string
		wantNote      bool
	}{
		{
			name:          "present",
			look:          func(string) (string, error) { return "/usr/bin/ffprobe", nil },
			wantAvailable: true,
			wantPath:      "/usr/bin/ffprobe",
		},
		{
			name:          "missing",
			look:          func(string) (string, error) { return "", errors.New("executable file not found in $PATH") },
			wantAvailable: false,
			wantNote:      true,
		},
		{
			name:          "empty path with nil error is still missing",
			look:          func(string) (string, error) { return "  ", nil },
			wantAvailable: false,
			wantNote:      true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FFprobeAvailability(tc.look)
			if got.Available != tc.wantAvailable {
				t.Fatalf("Available = %v, want %v", got.Available, tc.wantAvailable)
			}
			if got.Path != tc.wantPath {
				t.Errorf("Path = %q, want %q", got.Path, tc.wantPath)
			}
			if tc.wantNote && got.Note == "" {
				t.Error("missing ffprobe must carry an operator-facing note")
			}
			if !tc.wantNote && got.Note != "" {
				t.Errorf("Note = %q, want empty", got.Note)
			}
		})
	}
}

func TestFFprobeArgsIncludeJSONFormatAndStreams(t *testing.T) {
	args := FFprobeArgs(`C:\ComfyUI\output\clip.mp4`)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-print_format json", "-show_format", "-show_streams", "-v error"} {
		if !strings.Contains(joined, want) {
			t.Errorf("FFprobeArgs missing %q; got %v", want, args)
		}
	}
	if args[len(args)-1] != `C:\ComfyUI\output\clip.mp4` {
		t.Errorf("target path must be the final arg; got %q", args[len(args)-1])
	}
}

func TestProbeUsesInjectedRunner(t *testing.T) {
	var gotName string
	var gotArgs []string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotName = name
		gotArgs = args
		return []byte(fixtureVideoWithAudio), nil
	}

	info, err := Probe(context.Background(), run, "", "clip.mp4")
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if gotName != "ffprobe" {
		t.Errorf("binary = %q, want %q", gotName, "ffprobe")
	}
	if !reflect.DeepEqual(gotArgs, FFprobeArgs("clip.mp4")) {
		t.Errorf("args = %v, want %v", gotArgs, FFprobeArgs("clip.mp4"))
	}
	if info.Width != 1280 || !info.HasAudio {
		t.Errorf("parsed info = %+v, want 1280-wide with audio", info)
	}

	custom := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		gotName = name
		return []byte(fixtureStillPNG), nil
	}
	if _, err := Probe(context.Background(), custom, `C:\ffmpeg\bin\ffprobe.exe`, "still.png"); err != nil {
		t.Fatalf("Probe() with explicit binary error = %v", err)
	}
	if gotName != `C:\ffmpeg\bin\ffprobe.exe` {
		t.Errorf("explicit binary not honored; got %q", gotName)
	}
}

func TestProbeSurfacesRunnerFailure(t *testing.T) {
	run := func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("exit status 1: clip.mp4: Invalid data found when processing input")
	}
	_, err := Probe(context.Background(), run, "", filepath.Join("out", "clip.mp4"))
	if err == nil {
		t.Fatal("Probe() = nil error, want error")
	}
	if !strings.Contains(err.Error(), "clip.mp4") {
		t.Errorf("error must name the file; got %v", err)
	}
}

func TestHashReaderAndHashFile(t *testing.T) {
	// Well-known SHA-256 vectors; hardcoded so the test asserts the algorithm
	// rather than restating the implementation.
	tests := []struct {
		name      string
		content   string
		wantSHA   string
		wantBytes int64
	}{
		{
			name:      "empty",
			content:   "",
			wantSHA:   "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			wantBytes: 0,
		},
		{
			name:      "hello world",
			content:   "hello world",
			wantSHA:   "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
			wantBytes: 11,
		},
		{
			name:      "abc",
			content:   "abc",
			wantSHA:   "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
			wantBytes: 3,
		},
	}

	dir := t.TempDir()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotSHA, gotBytes, err := HashReader(strings.NewReader(tc.content))
			if err != nil {
				t.Fatalf("HashReader() error = %v", err)
			}
			if gotSHA != tc.wantSHA {
				t.Errorf("HashReader() sha = %s, want %s", gotSHA, tc.wantSHA)
			}
			if gotBytes != tc.wantBytes {
				t.Errorf("HashReader() bytes = %d, want %d", gotBytes, tc.wantBytes)
			}

			path := filepath.Join(dir, tc.name+".bin")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("writing fixture: %v", err)
			}
			fileSHA, fileBytes, err := HashFile(path)
			if err != nil {
				t.Fatalf("HashFile() error = %v", err)
			}
			if fileSHA != tc.wantSHA || fileBytes != tc.wantBytes {
				t.Errorf("HashFile() = %s/%d, want %s/%d", fileSHA, fileBytes, tc.wantSHA, tc.wantBytes)
			}
		})
	}

	if _, _, err := HashFile(filepath.Join(dir, "does-not-exist.bin")); err == nil {
		t.Error("HashFile() on a missing file = nil error, want error")
	}
	if _, _, err := HashReader(nil); err == nil {
		t.Error("HashReader(nil) = nil error, want error")
	}
}

func TestStagedName(t *testing.T) {
	const sha = "3f9a1c2b8d40aa11bb22cc33dd44ee55ff66aa77bb88cc99dd00ee11ff224433"
	tests := []struct {
		name     string
		hostPath string
		sha      string
		want     string
	}{
		{"simple", "/refs/portrait.png", sha, "portrait-3f9a1c2b8d40.png"},
		{"windows path", `D:\refs\portrait.png`, sha, "portrait-3f9a1c2b8d40.png"},
		{"spaces collapse", "/refs/my  ref shot.jpg", sha, "my_ref_shot-3f9a1c2b8d40.jpg"},
		{"no extension", "/refs/plate", sha, "plate-3f9a1c2b8d40"},
		{"dotfile", "/refs/.hidden", sha, "hidden-3f9a1c2b8d40"},
		{"unicode stem", "/refs/retrato-ñ.png", sha, "retrato-3f9a1c2b8d40.png"},
		{"trailing separators trimmed", "/refs/shot_-.png", sha, "shot-3f9a1c2b8d40.png"},
		{"empty stem falls back", "/refs/@@@.png", sha, "input-3f9a1c2b8d40.png"},
		{"short sha is used whole", "/refs/a.png", "abcd", "a-abcd.png"},
		{"missing sha yields bare name", "/refs/a.png", "", "a.png"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := StagedName(tc.hostPath, tc.sha); got != tc.want {
				t.Errorf("StagedName(%q, %q) = %q, want %q", tc.hostPath, tc.sha, got, tc.want)
			}
		})
	}

	// The load-bearing property: same base name + different content must not
	// collide, because a collision would silently rewrite what an archived run
	// referenced.
	a := StagedName("/a/input.png", "1111111111111111111111111111111111111111111111111111111111111111")
	b := StagedName("/b/input.png", "2222222222222222222222222222222222222222222222222222222222222222")
	if a == b {
		t.Fatalf("distinct content produced the same staged name: %q", a)
	}
	// ...and the same content from two host paths must map to one name.
	c := StagedName(`C:\elsewhere\input.png`, "1111111111111111111111111111111111111111111111111111111111111111")
	if a != c {
		t.Fatalf("identical content produced different names: %q vs %q", a, c)
	}
}

func TestShortSHA(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"abc", "abc"},
		{"0123456789ab", "0123456789ab"},
		{"0123456789abcdef", "0123456789ab"},
	}
	for _, tc := range tests {
		if got := ShortSHA(tc.in); got != tc.want {
			t.Errorf("ShortSHA(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateComfyFilename(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"bare filename", "portrait-3f9a1c2b8d40.png", false},
		{"subfolder relative", "refs/portrait.png", false},
		{"nested subfolder", "refs/2026/portrait.png", false},
		{"dot in name", "portrait.v2.png", false},
		{"empty", "", true},
		{"whitespace", "   ", true},
		{"windows absolute", `C:\ComfyUI\input\portrait.png`, true},
		{"windows absolute forward slash", "C:/ComfyUI/input/portrait.png", true},
		{"unix absolute", "/home/dmmde/refs/portrait.png", true},
		{"unc path", `\\server\share\portrait.png`, true},
		{"parent escape", "../portrait.png", true},
		{"embedded parent escape", "refs/../../portrait.png", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateComfyFilename(tc.in)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateComfyFilename(%q) = nil, want error", tc.in)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateComfyFilename(%q) = %v, want nil", tc.in, err)
			}
		})
	}
}
