package research

import (
	"context"
	"strings"
	"testing"
	"time"
)

// yt-dlp reports a rate limit on the subtitle download as a WARNING and exits 0 with no file. That must not read
// as a video that has no subtitles.
func TestSubtitleRateLimitWarningIsInTheNoSubtitlesReason(t *testing.T) {
	pinYouTube(t)
	opt, _ := ytOpts(t)
	got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idWarn429, opt)
	if got.ErrKind != ErrKindNoSubtitles || got.Text != "" {
		t.Fatalf("got %+v, want %s", got, ErrKindNoSubtitles)
	}
	if !strings.Contains(got.Err, "429") || !strings.Contains(got.Err, "Too Many Requests") {
		t.Errorf("the reason should carry yt-dlp's last line so a rate limit is visible: %q", got.Err)
	}
	// a video that is really without subtitles writes nothing, and the reason does not invent output
	got = Fetch(context.Background(), "https://www.youtube.com/watch?v="+idNoSubs, opt)
	if got.ErrKind != ErrKindNoSubtitles || strings.Contains(got.Err, "(no output)") {
		t.Errorf("a silent exit 0 should read plainly: %+v", got)
	}
}

// The strict count: the run is ended once the binary has logged its start, so the cancel never races the launch.
func TestSubtitleRunEndsWhenTheCallerCancelsAfterTheBinaryStarted(t *testing.T) {
	pinYouTube(t)
	opt, root := ytOpts(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer cancel()
		for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			if len(readCallsQuiet(root)) > 0 {
				return
			}
		}
	}()
	start := time.Now()
	got := Fetch(ctx, "https://www.youtube.com/watch?v="+idHang, opt)
	if took := time.Since(start); took > 12*time.Second {
		t.Fatalf("fetch outlived the cancellation: %v", took)
	}
	if got.ErrKind != ErrKindTimeout || got.Text != "" {
		t.Fatalf("got %+v, want %s", got, ErrKindTimeout)
	}
	if n := len(readCalls(t, root)); n != 1 {
		t.Errorf("binary invoked %d times, want exactly 1", n)
	}
	if left := leftovers(t, root); len(left) != 0 {
		t.Errorf("per-call directory left behind after a cancellation: %v", left)
	}
}

// readCallsQuiet is readCalls for a goroutine: no *testing.T, and a missing or half-written log is no calls yet.
func readCallsQuiet(root string) [][]string {
	out, _ := readCallsOf(root)
	return out
}

// ---- srv3 cut by the read bound ----------------------------------------------

func TestSRV3ToTextOfATruncatedFileKeepsTheCuesRead(t *testing.T) {
	full := `<?xml version="1.0" encoding="utf-8" ?><timedtext format="3"><body>` +
		`<p t="0" d="900"><s>first cue here</s></p>` +
		`<p t="1000" d="900"><s>second cue here</s></p>` +
		`<p t="2000" d="900"><s>third cue is cut o`
	text, err := SRV3ToText([]byte(full))
	if err != nil {
		t.Fatalf("a file cut mid-cue lost everything: %v", err)
	}
	if text != "first cue here\nsecond cue here" {
		t.Errorf("text = %q, want the two complete cues and not the cut one", text)
	}
	// cut before the first cue is complete: nothing was read, and that is still an error
	if _, err := SRV3ToText([]byte(`<?xml version="1.0"?><timedtext format="3"><body><p t="0" d="9"><s>cut o`)); err == nil {
		t.Errorf("a file with no complete cue parsed")
	}
	// not srv3 at all stays an error
	if _, err := SRV3ToText([]byte("not xml at all <<<")); err == nil {
		t.Errorf("garbage parsed as srv3")
	}
}

// A track that ends inside a cue without the read bound having cut it (an interrupted download) is marked too.
func TestSubtitleSRV3EndingInsideACueIsMarkedTruncated(t *testing.T) {
	pinYouTube(t)
	opt, _ := ytOpts(t)
	got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idCutSRV3, opt)
	if got.Err != "" || got.ErrKind != "" {
		t.Fatalf("a track cut mid-cue was refused: %s (%s)", got.Err, got.ErrKind)
	}
	if !got.Truncated || got.Text != "first cue here\nsecond cue here" {
		t.Errorf("Truncated=%v text=%q, want true and the two complete cues", got.Truncated, got.Text)
	}
}

func TestSubtitleSRV3OverTheReadBoundIsCutNotLost(t *testing.T) {
	pinYouTube(t)
	opt, _ := ytOpts(t)
	got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idHugeSRV3, opt)
	if got.Err != "" || got.ErrKind != "" {
		t.Fatalf("a long srv3 track was refused: %s (%s)", got.Err, got.ErrKind)
	}
	if !got.Truncated || got.Bytes != 2<<20 {
		t.Errorf("Truncated=%v Bytes=%d, want true and exactly the 2 MiB read bound", got.Truncated, got.Bytes)
	}
	if !strings.HasPrefix(got.Text, "cue number 0 speaks") || got.TextBytes == 0 {
		t.Errorf("the cues read before the cut should be the text: %.80q (%d bytes)", got.Text, got.TextBytes)
	}
}

// ---- the other YouTube shapes -------------------------------------------------

func TestLiveEmbedAndMusicURLsReachTheBinaryWithTheCanonicalURL(t *testing.T) {
	pinYouTube(t)
	for _, raw := range []string{
		"https://www.youtube.com/live/" + idPlain,
		"https://www.youtube.com/embed/" + idPlain + "?start=5",
		"https://www.youtube-nocookie.com/embed/" + idPlain,
		"https://music.youtube.com/watch?v=" + idPlain + "&list=RDAMVM",
	} {
		opt, root := ytOpts(t)
		got := Fetch(context.Background(), raw, opt)
		if got.Err != "" || got.Text != "Hello & welcome.\nSecond cue here." {
			t.Errorf("%s: %+v", raw, got)
			continue
		}
		calls := readCalls(t, root)
		if len(calls) != 1 || calls[0][len(calls[0])-1] != "https://www.youtube.com/watch?v="+idPlain {
			t.Errorf("%s: the binary was not given the canonical watch URL: %q", raw, calls)
		}
	}
}
