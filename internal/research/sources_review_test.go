package research

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Tests added after an adversarial review of the text sources: the cases a one-line
// change to the adapters used to survive.

func argValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func TestSubtitleArgvPinsEveryFlagValue(t *testing.T) {
	pinYouTube(t)
	opt, root := ytOpts(t)
	if got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idPlain, opt); got.Err != "" {
		t.Fatalf("fetch failed: %s", got.Err)
	}
	calls := readCalls(t, root)
	if len(calls) != 1 {
		t.Fatalf("yt-dlp invoked %d times", len(calls))
	}
	args := calls[0]
	for flag, want := range map[string]string{
		"--retries":           "0",
		"--extractor-retries": "0",
		"--fragment-retries":  "0",
		"--socket-timeout":    "15",
		"-o":                  "%(id)s.%(ext)s",
	} {
		if got, ok := argValue(args, flag); !ok || got != want {
			t.Errorf("%s = %q (present %v), want %q", flag, got, ok, want)
		}
	}
	for _, flag := range []string{"--no-update", "--no-cache-dir", "--ignore-config", "--no-cookies", "--no-cookies-from-browser", "--skip-download", "--no-playlist"} {
		if !hasArg(args, flag) {
			t.Errorf("%s missing from %q", flag, args)
		}
	}
	// home and temp point at the same per-call directory, under the scratch root
	var home, temp string
	for i, a := range args {
		if a != "-P" || i+1 >= len(args) {
			continue
		}
		switch {
		case strings.HasPrefix(args[i+1], "home:"):
			home = strings.TrimPrefix(args[i+1], "home:")
		case strings.HasPrefix(args[i+1], "temp:"):
			temp = strings.TrimPrefix(args[i+1], "temp:")
		}
	}
	if home == "" || filepath.Clean(home) != filepath.Clean(temp) || filepath.Clean(filepath.Dir(home)) != filepath.Clean(root) {
		t.Errorf("-P home:%q temp:%q, want the same directory directly under %q", home, temp, root)
	}
	// warnings stay on: yt-dlp reports a rate limit on the subtitle download (HTTP 429) as a WARNING and exits 0
	if hasArg(args, "--no-warnings") || !hasArg(args, "--quiet") {
		t.Errorf("want --quiet without --no-warnings in %q", args)
	}
}

func TestSubtitleBinaryRunsInThePerCallDirectory(t *testing.T) {
	pinYouTube(t)
	opt, root := ytOpts(t)
	if got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idPlain, opt); got.Err != "" {
		t.Fatalf("fetch failed: %s", got.Err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "cwd.log"))
	if err != nil {
		t.Fatal(err)
	}
	cwd := strings.TrimSpace(string(raw))
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	parentReal, err := filepath.EvalSymlinks(filepath.Dir(cwd))
	if err != nil {
		t.Fatal(err)
	}
	if parentReal != rootReal || !strings.HasPrefix(filepath.Base(cwd), "ytdlp-") {
		t.Errorf("the binary ran in %q, want a ytdlp-* directory directly under %q", cwd, root)
	}
}

func TestSubtitleFormatsThisAdapterReadsAndRefuses(t *testing.T) {
	pinYouTube(t)
	opt, root := ytOpts(t)
	got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idTTML, opt)
	if got.ErrKind != ErrKindUnsupportedSubs || got.Text != "" {
		t.Errorf("a .ttml track: got %+v, want %s", got, ErrKindUnsupportedSubs)
	}
	got = Fetch(context.Background(), "https://www.youtube.com/watch?v="+idSRT, opt)
	if got.Err != "" || got.Text != "Hello from SubRip.\nSecond line." {
		t.Errorf("a .srt track: got err %q text %q", got.Err, got.Text)
	}
	if left := leftovers(t, root); len(left) != 0 {
		t.Errorf("per-call directory left behind: %v", left)
	}
}

func TestSubtitleFileOverTheReadBoundIsCutAndSaysSo(t *testing.T) {
	pinYouTube(t)
	opt, _ := ytOpts(t)
	got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idHuge, opt)
	if got.Err != "" {
		t.Fatalf("fetch failed: %s", got.Err)
	}
	if !got.Truncated || got.Bytes != 2<<20 {
		t.Errorf("Truncated=%v Bytes=%d, want true and exactly the 2 MiB read bound", got.Truncated, got.Bytes)
	}
	if got.TextBytes == 0 || got.TextBytes > 96<<10 {
		t.Errorf("TextBytes = %d, want 1..96 KiB", got.TextBytes)
	}
}

func TestCappedBufferKeepsOnlyTheHeadOf8KiB(t *testing.T) {
	var c cappedBuffer
	chunk := []byte(strings.Repeat("y", 1000))
	for i := 0; i < 1000; i++ {
		if n, err := c.Write(chunk); n != len(chunk) || err != nil {
			t.Fatalf("Write = (%d, %v), want it to report the whole chunk consumed", n, err)
		}
	}
	if c.b.Len() != 8<<10 {
		t.Errorf("kept %d bytes, want exactly 8 KiB", c.b.Len())
	}
}

func TestSubtitleChatterOnAFailureIsBounded(t *testing.T) {
	pinYouTube(t)
	opt, root := ytOpts(t)
	got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idFlood, opt)
	if got.ErrKind != ErrKindSiteBreak {
		t.Fatalf("got %+v, want %s", got, ErrKindSiteBreak)
	}
	if len(got.Err) > 400 {
		t.Errorf("error is %d bytes, the diagnostic must stay one short line", len(got.Err))
	}
	if n := len(readCalls(t, root)); n != 1 {
		t.Errorf("invoked %d times", n)
	}
}

func TestSubtitleBlankCuesAreATypedErrorNamingSubtitles(t *testing.T) {
	pinYouTube(t)
	opt, _ := ytOpts(t)
	got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idBlank, opt)
	if got.Text != "" || got.ErrKind != "empty_subtitles" {
		t.Fatalf("got err %q kind %q, want kind empty_subtitles", got.Err, got.ErrKind)
	}
	if !strings.Contains(strings.ToLower(got.Err), "subtitle") || strings.Contains(got.Err, "page") {
		t.Errorf("the message should name subtitles, not a page: %q", got.Err)
	}
}

func TestRelativeScratchRootStillFindsTheSubtitleFile(t *testing.T) {
	pinYouTube(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	opt, _ := ytOpts(t)
	opt.ScratchRoot = "rel-scratch"
	got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idPlain, opt)
	if got.Err != "" || got.Text != "Hello & welcome.\nSecond cue here." {
		t.Fatalf("a relative scratch root broke the fetch: err %q kind %q text %q", got.Err, got.ErrKind, got.Text)
	}
	ents, _ := os.ReadDir(filepath.Join(cwd, "rel-scratch"))
	for _, e := range ents {
		if e.Name() != "calls.log" && e.Name() != "envs.log" && e.Name() != "cwd.log" {
			t.Errorf("per-call directory left behind: %s", e.Name())
		}
	}
}

// ---- feeds -----------------------------------------------------------------

const foafFixture = `<?xml version="1.0"?>
<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:foaf="http://xmlns.com/foaf/0.1/">
<foaf:Person rdf:about="https://feeds.example/people/ada"><foaf:name>Ada Lovelace</foaf:name><foaf:knows rdf:resource="https://feeds.example/people/charles"/></foaf:Person>
</rdf:RDF>`

const rss1Fixture = `<?xml version="1.0"?>
<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/">
<channel rdf:about="https://feeds.example/"><title>Old Style Notes</title><link>https://feeds.example/</link></channel>
<item rdf:about="https://feeds.example/r/1"><title>RSS one</title><link>https://feeds.example/r/1</link><description>Body one.</description></item>
</rdf:RDF>`

const emptyRSS = `<rss version="2.0"><channel><title>Empty</title><link>https://feeds.example/</link></channel></rss>`

func TestFeedToTextWithNoItemsIsAnError(t *testing.T) {
	for name, body := range map[string]string{"empty rss": emptyRSS, "foaf rdf": foafFixture,
		"empty atom": `<feed xmlns="http://www.w3.org/2005/Atom"><title>None</title></feed>`} {
		if _, text, err := FeedToText([]byte(body)); err == nil {
			t.Errorf("%s parsed as a feed with text %q, want an error so the page path reads it", name, text)
		}
	}
	if _, text, err := FeedToText([]byte(rss1Fixture)); err != nil || !strings.Contains(text, "1. RSS one") {
		t.Errorf("a real RSS 1.0 feed must still parse: err %v\n%s", err, text)
	}
}

func TestSniffFeedNeedsTheRSS1NamespaceForRDF(t *testing.T) {
	if SniffFeed([]byte(foafFixture)) {
		t.Errorf("a FOAF document was sniffed as a feed")
	}
	if !SniffFeed([]byte(rss1Fixture)) {
		t.Errorf("an RSS 1.0 document was not sniffed as a feed")
	}
}

func TestFetchReadsAFeedlessRootExactlyAsBefore(t *testing.T) {
	pinDNS(t, map[string][]string{"feeds.example": {sentinelPublic}})
	for _, tc := range []struct{ name, ct, body, want string }{
		{"rdf+xml that is FOAF", "application/rdf+xml", foafFixture, "Ada Lovelace"},
		{"FOAF sniffed from application/xml", "application/xml", foafFixture, "Ada Lovelace"},
		{"rss with no items", "application/xml", emptyRSS, "Empty"},
		{"rss media type with no items", "application/rss+xml", emptyRSS, "Empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := serveFeed(t, tc.ct, tc.body, &hits)
			d := &honestDialer{toReal: srv.Listener.Addr().String()}
			got := Fetch(context.Background(), "http://feeds.example/doc", Options{Client: d.client(), Timeout: 5 * time.Second})
			if got.Err != "" {
				t.Fatalf("fetch: %s", got.Err)
			}
			if strings.Contains(got.Text, "Items: 0") || strings.HasPrefix(got.Text, "Feed:") || !strings.Contains(got.Text, tc.want) {
				t.Fatalf("not read as the plain document it was before:\n%s", got.Text)
			}
		})
	}
	// and a real RSS 1.0 feed is still a feed
	var hits atomic.Int32
	srv := serveFeed(t, "application/rdf+xml", rss1Fixture, &hits)
	d := &honestDialer{toReal: srv.Listener.Addr().String()}
	got := Fetch(context.Background(), "http://feeds.example/rss1", Options{Client: d.client(), Timeout: 5 * time.Second})
	if got.Err != "" || got.Title != "Old Style Notes" || !strings.Contains(got.Text, "1. RSS one") {
		t.Fatalf("RSS 1.0 not digested as a feed: %+v", got)
	}
}

func TestFeedSummaryIsCutAtOneThousandBytes(t *testing.T) {
	body := `<rss version="2.0"><channel><title>T</title><item><title>One</title><link>https://feeds.example/1</link><description>` +
		strings.Repeat("abcd ", 2000) + `</description></item></channel></rss>`
	_, text, err := FeedToText([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var sum string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "abcd") {
			sum = l
		}
	}
	if len(sum) < 990 || len(sum) > 1003 || !strings.HasSuffix(sum, "...") {
		t.Errorf("summary line is %d bytes, want about 1000 plus the ellipsis", len(sum))
	}
}
