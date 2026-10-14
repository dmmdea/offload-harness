package research

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fake yt-dlp. The configured binary path in these tests is the test
// binary itself: TestMain notices the subtitles-only flag set and plays the
// part of yt-dlp instead of running the tests. Nothing here ever touches the
// network, and no real yt-dlp is ever run by this package's tests.
//
// What the fake does is chosen by the video id in the last argument, and every
// invocation appends its argument vector to calls.log in the scratch ROOT (the
// parent of the per-call directory it was told to write into), so a test can
// count invocations and read the flags back.

func TestMain(m *testing.M) {
	if isFakeYtDlpInvocation(os.Args) {
		os.Exit(runFakeYtDlp(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func isFakeYtDlpInvocation(args []string) bool {
	has := false
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "-test.") {
			return false
		}
		if a == "--skip-download" {
			has = true
		}
	}
	return has
}

const argSep = "\x1f"

// Video ids the fake understands (all 11 characters, the shape of a real id).
const (
	idPlain    = "PLAINcaps01"
	idRolling  = "ROLLINGcap1"
	idSRV3     = "SRV3captn01"
	idMultiple = "MULTIlang01"
	idBreak    = "BREAKexit01"
	idNoSubs   = "NOSUBSfile1"
	idHang     = "SLEEPforevr"
)

const fakePlainVTT = "WEBVTT\n\nNOTE this is a note\nspanning lines\n\n00:00:01.000 --> 00:00:03.000\n<v Host>Hello &amp; welcome.</v>\n\n2\n00:00:03.500 --> 00:00:05.000 line:90%\n<i>Second</i> cue here.\n"

// fakeRollingVTT is the shape YouTube's automatic captions come in: every cue
// repeats the previous cue's last line, adds one line with per-word timing
// tags, and is followed by a ten-millisecond cue that only repeats it.
const fakeRollingVTT = "WEBVTT\nKind: captions\nLanguage: en\n\n" +
	"00:00:00.160 --> 00:00:02.230 align:start position:0%\n \nwelcome<00:00:00.640><c> to</c><00:00:00.800><c> the</c><00:00:00.960><c> show</c>\n\n" +
	"00:00:02.230 --> 00:00:02.240 align:start position:0%\nwelcome to the show\n \n\n" +
	"00:00:02.240 --> 00:00:04.710 align:start position:0%\nwelcome to the show\ntoday<00:00:02.800><c> we</c><00:00:02.960><c.colorE5E5E5> cook</c>\n\n" +
	"00:00:04.710 --> 00:00:04.720 align:start position:0%\ntoday we cook\n \n\n" +
	"00:00:04.720 --> 00:00:06.000 align:start position:0%\ntoday we cook\npasta &amp; sauce\n"

const fakeSRV3 = `<?xml version="1.0" encoding="utf-8" ?><timedtext format="3"><body>` +
	`<p t="160" d="2070" w="1"><s ac="0">welcome</s><s t="480" ac="0"> to</s><s t="640" ac="0"> the show</s></p>` +
	`<p t="2230" d="10" a="1">` + "\n" + `</p>` +
	`<p t="2240" d="2470" w="1"><s>today we cook</s></p>` +
	`<p t="4710" d="10" a="1"><s>today we cook</s></p>` +
	`</body></timedtext>`

func runFakeYtDlp(args []string) int {
	var dir, target string
	for i, a := range args {
		if a == "-P" && i+1 < len(args) && strings.HasPrefix(args[i+1], "home:") {
			dir = strings.TrimPrefix(args[i+1], "home:")
		}
		if a == "--" && i+1 < len(args) {
			target = args[i+1]
		}
	}
	if dir == "" || target == "" {
		os.Stderr.WriteString("fake yt-dlp: no -P home: or no -- URL in " + strings.Join(args, " ") + "\n")
		return 64
	}
	f, err := os.OpenFile(filepath.Join(filepath.Dir(dir), "calls.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = f.WriteString(strings.Join(args, argSep) + "\n")
		_ = f.Close()
	}
	if e, err := os.OpenFile(filepath.Join(filepath.Dir(dir), "envs.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		_, _ = e.WriteString(strings.Join([]string{"probe=" + os.Getenv("RESEARCH_ENV_PROBE"), "home=" + os.Getenv("HOME"), "temp=" + os.Getenv("TEMP"), "dir=" + dir}, argSep) + "\n")
		_ = e.Close()
	}
	id := target[strings.LastIndex(target, "v=")+2:]
	write := func(name, body string) { _ = os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600) }
	switch id {
	case idPlain:
		write(id+".en.vtt", fakePlainVTT)
	case idRolling:
		write(id+".en.vtt", fakeRollingVTT)
	case idSRV3:
		write(id+".en.srv3", fakeSRV3)
	case idMultiple:
		write(id+".en-orig.vtt", "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nORIGINAL TRACK\n")
		write(id+".en.vtt", "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nPREFERRED TRACK\n")
	case idBreak:
		os.Stderr.WriteString("ERROR: [youtube] " + id + ": Sign in to confirm you're not a bot\n")
		return 1
	case idNoSubs:
		// exit 0 and no subtitle file: the video has no captions in the asked languages
	case idHang:
		time.Sleep(30 * time.Second)
	}
	return 0
}

// readCalls returns the recorded argument vectors, one per invocation.
func readCalls(t *testing.T, root string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "calls.log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line != "" {
			out = append(out, strings.Split(line, argSep))
		}
	}
	return out
}

// leftovers lists what is in the scratch root besides the call log: a per-call
// directory still there after the call is a leak.
func leftovers(t *testing.T, root string) []string {
	t.Helper()
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if e.Name() != "calls.log" && e.Name() != "envs.log" {
			out = append(out, e.Name())
		}
	}
	return out
}

func fakeBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}
