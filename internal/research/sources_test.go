package research

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ytOpts is an Options that turns the subtitle adapter on with the fake binary
// and a private scratch root, over a dialer that never leaves the machine.
func ytOpts(t *testing.T) (Options, string) {
	t.Helper()
	root := t.TempDir()
	d := &honestDialer{}
	return Options{YtDlpPath: fakeBinary(t), ScratchRoot: root, Client: d.client(), Timeout: 20 * time.Second}, root
}

func pinYouTube(t *testing.T) {
	t.Helper()
	pinDNS(t, map[string][]string{
		"www.youtube.com": {sentinelPublic},
		"youtube.com":     {sentinelPublic},
		"m.youtube.com":   {sentinelPublic},
		"youtu.be":         {sentinelPublic},
	})
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// ---- URL classes -------------------------------------------------------

func TestClassifyURLRoutesOnlyTheNamedYouTubeShapes(t *testing.T) {
	yes := map[string]string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ":              "dQw4w9WgXcQ",
		"https://youtube.com/watch?v=dQw4w9WgXcQ&t=10s&list=PLabc": "dQw4w9WgXcQ",
		"http://m.youtube.com/watch?v=dQw4w9WgXcQ":                 "dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ?t=5":                         "dQw4w9WgXcQ",
		"https://www.youtube.com/shorts/dQw4w9WgXcQ":               "dQw4w9WgXcQ",
		"https://WWW.YouTube.com/shorts/dQw4w9WgXcQ/":              "dQw4w9WgXcQ",
	}
	for raw, id := range yes {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		kind, got := ClassifyURL(u)
		if kind != SourceKindYouTube || got != id {
			t.Errorf("%s: got (%v, %q), want (youtube, %q)", raw, kind, got, id)
		}
	}
	no := []string{
		"https://youtube.com.evil.example/watch?v=dQw4w9WgXcQ",
		"https://evilyoutube.com/watch?v=dQw4w9WgXcQ",
		"https://www.youtube.com.evil.example/shorts/dQw4w9WgXcQ",
		"https://youtu.be.evil.example/dQw4w9WgXcQ",
		"https://notyoutu.be/dQw4w9WgXcQ",
		"https://www.youtube.com@evil.example/watch?v=dQw4w9WgXcQ",
		"https://user:pw@www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://www.youtube.com:8443/watch?v=dQw4w9WgXcQ",
		"https://www.youtube.com/playlist?list=PLabc",
		"https://www.youtube.com/channel/UCabc",
		"https://www.youtube.com/@someone",
		"https://www.youtube.com/watch",
		"https://www.youtube.com/watch?v=short",
		"https://www.youtube.com/watch?v=dQw4w9WgXc%3B",
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ%2Fx",
		"https://www.youtube.com/shorts/",
		"https://youtu.be/",
		"https://music.youtube.com/watch?v=dQw4w9WgXcQ",
		"ftp://youtu.be/dQw4w9WgXcQ",
		"https://example.com/watch?v=dQw4w9WgXcQ",
	}
	for _, raw := range no {
		u, err := url.Parse(raw)
		if err != nil {
			continue // an unparsable URL never reaches the matcher
		}
		if kind, id := ClassifyURL(u); kind != SourceKindPage || id != "" {
			t.Errorf("%s: routed to the subtitle adapter (%v, %q); only the three named shapes may be", raw, kind, id)
		}
	}
}

// ---- the subtitle adapter ------------------------------------------------

func TestSubtitleDigestFromFakeYtDlp(t *testing.T) {
	pinYouTube(t)
	opt, root := ytOpts(t)
	got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idPlain+"&list=PLignored", opt)
	if got.Err != "" {
		t.Fatalf("fetch failed: %s (%s)", got.Err, got.ErrKind)
	}
	want := "Hello & welcome.\nSecond cue here."
	if got.Text != want {
		t.Fatalf("text = %q, want %q", got.Text, want)
	}
	if got.TextBytes != len(want) || got.Bytes == 0 || got.Status != 0 || !strings.Contains(got.Title, idPlain) {
		t.Fatalf("metadata off: %+v", got)
	}
	calls := readCalls(t, root)
	if len(calls) != 1 {
		t.Fatalf("yt-dlp invoked %d times, want exactly 1", len(calls))
	}
	args := calls[0]
	for _, flag := range []string{
		"--skip-download", "--write-subs", "--write-auto-subs", "--sub-langs=en.*", "--sub-format=vtt/srv3/best",
		"--no-cookies", "--no-cookies-from-browser", "--ignore-config", "--no-playlist", "--socket-timeout",
		"--retries", "--extractor-retries", "--no-cache-dir",
	} {
		if !hasArg(args, flag) {
			t.Errorf("flag %q missing from %q", flag, args)
		}
	}
	// the URL handed to the binary is the canonical one, after a literal "--", so a caller's list= or t=
	// parameter and a caller's text can never be read as an option
	if n := len(args); n < 2 || args[n-2] != "--" || args[n-1] != "https://www.youtube.com/watch?v="+idPlain {
		t.Errorf("tail of argv = %q, want [-- canonical URL]", args[max(0, len(args)-2):])
	}
	for _, a := range args {
		if strings.Contains(a, "cookies=") || a == "--cookies" || a == "--cookies-from-browser" || a == "--netrc" {
			t.Errorf("credential-bearing flag %q passed", a)
		}
	}
	if left := leftovers(t, root); len(left) != 0 {
		t.Errorf("per-call directory left behind: %v", left)
	}
}

func TestSubtitleDigestUsesTheCallersLanguagesAndRefusesAnOptionShapedValue(t *testing.T) {
	pinYouTube(t)
	opt, root := ytOpts(t)
	opt.SubLangs = "es.*,en"
	if got := Fetch(context.Background(), "https://youtu.be/"+idPlain, opt); got.Err != "" {
		t.Fatalf("fetch failed: %s", got.Err)
	}
	calls := readCalls(t, root)
	if len(calls) != 1 || !hasArg(calls[0], "--sub-langs=es.*,en") {
		t.Fatalf("languages not passed: %q", calls)
	}
	for _, bad := range []string{"en --exec x", "en;ls", "$(id)", "", strings.Repeat("e", 200)} {
		opt2, root2 := ytOpts(t)
		opt2.SubLangs = bad
		got := Fetch(context.Background(), "https://youtu.be/"+idPlain, opt2)
		if bad == "" {
			// empty means the default, which is valid
			if got.Err != "" || !hasArg(readCalls(t, root2)[0], "--sub-langs=en.*") {
				t.Errorf("empty sub_langs must mean the default en.*: %+v", got)
			}
			continue
		}
		if got.ErrKind != ErrKindBadLangs || got.Text != "" {
			t.Errorf("sub_langs %q: got %+v, want %s", bad, got, ErrKindBadLangs)
		}
		if n := len(readCalls(t, root2)); n != 0 {
			t.Errorf("sub_langs %q: binary invoked %d times", bad, n)
		}
	}
}

func TestSubtitleAdapterIsOffWhenThePathIsEmpty(t *testing.T) {
	pinYouTube(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><head><title>Watch page</title></head><body><p>an ordinary page body</p></body></html>"))
	}))
	defer srv.Close()
	root := t.TempDir()
	d := &honestDialer{toReal: srv.Listener.Addr().String()}
	// no YtDlpPath: the URL is read exactly as before this change existed
	got := Fetch(context.Background(), "http://www.youtube.com/watch?v="+idPlain, Options{ScratchRoot: root, Client: d.client(), Timeout: 5 * time.Second})
	if got.Err != "" || !strings.Contains(got.Text, "an ordinary page body") {
		t.Fatalf("the unchanged page path did not run: %+v", got)
	}
	if got.ErrKind != "" {
		t.Errorf("ErrKind = %q on a page fetch", got.ErrKind)
	}
	if n := len(readCalls(t, root)); n != 0 {
		t.Errorf("binary invoked %d times with no path configured", n)
	}
	if len(d.dialed()) == 0 {
		t.Errorf("the page was not fetched over HTTP")
	}
}

func TestSubtitleAdapterNeverSearchesPath(t *testing.T) {
	pinYouTube(t)
	for _, p := range []string{"yt-dlp", "./yt-dlp", "bin/yt-dlp"} {
		opt, root := ytOpts(t)
		opt.YtDlpPath = p
		got := Fetch(context.Background(), "https://youtu.be/"+idPlain, opt)
		if got.ErrKind != ErrKindYtDlpPath || got.Text != "" {
			t.Errorf("path %q: got %+v, want %s", p, got, ErrKindYtDlpPath)
		}
		if n := len(readCalls(t, root)); n != 0 {
			t.Errorf("path %q: binary invoked", p)
		}
	}
	// an absolute path that does not exist is a typed error as well, not a fallback to the page
	opt, _ := ytOpts(t)
	opt.YtDlpPath = filepath.Join(t.TempDir(), "no-such-binary")
	if got := Fetch(context.Background(), "https://youtu.be/"+idPlain, opt); got.ErrKind != ErrKindYtDlpPath {
		t.Errorf("missing binary: got %+v", got)
	}
}

func TestSubtitleSiteBreakIsATypedErrorAndNeverRetried(t *testing.T) {
	pinYouTube(t)
	opt, root := ytOpts(t)
	got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idBreak, opt)
	if got.ErrKind != ErrKindSiteBreak || got.Text != "" || got.Err == "" {
		t.Fatalf("got %+v, want a %s error and no text", got, ErrKindSiteBreak)
	}
	if !strings.Contains(got.Err, "exit") {
		t.Errorf("the error should say the binary exited non-zero: %q", got.Err)
	}
	if n := len(readCalls(t, root)); n != 1 {
		t.Fatalf("binary invoked %d times after a site break, want exactly 1 (no retry)", n)
	}
	if left := leftovers(t, root); len(left) != 0 {
		t.Errorf("per-call directory left behind after a failure: %v", left)
	}
	// the batch keeps its shape: a broken video beside a good one
	good := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idPlain, opt)
	if good.Err != "" {
		t.Errorf("the next URL was affected: %+v", good)
	}
}

func TestSubtitleNoFileIsATypedError(t *testing.T) {
	pinYouTube(t)
	opt, root := ytOpts(t)
	got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idNoSubs, opt)
	if got.ErrKind != ErrKindNoSubtitles || got.Text != "" {
		t.Fatalf("got %+v, want %s", got, ErrKindNoSubtitles)
	}
	if n := len(readCalls(t, root)); n != 1 {
		t.Errorf("binary invoked %d times, want 1", n)
	}
}

func TestSubtitleRunIsBoundedByTheCallersDeadline(t *testing.T) {
	pinYouTube(t)
	opt, root := ytOpts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	start := time.Now()
	got := Fetch(ctx, "https://www.youtube.com/watch?v="+idHang, opt)
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("fetch outlived the caller's deadline: %v", took)
	}
	if got.ErrKind != ErrKindTimeout || got.Text != "" {
		t.Fatalf("got %+v, want %s", got, ErrKindTimeout)
	}
	if n := len(readCalls(t, root)); n != 1 {
		t.Errorf("binary invoked %d times, want 1", n)
	}
	if left := leftovers(t, root); len(left) != 0 {
		t.Errorf("per-call directory left behind after a timeout: %v", left)
	}
}

func TestSubtitleTrackChoiceIsDeterministic(t *testing.T) {
	pinYouTube(t)
	opt, _ := ytOpts(t)
	got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idMultiple, opt)
	if got.Err != "" || got.Text != "PREFERRED TRACK" {
		t.Fatalf("got %+v, want the plain en track over en-orig", got)
	}
}

func TestSubtitleSRV3(t *testing.T) {
	pinYouTube(t)
	opt, _ := ytOpts(t)
	got := Fetch(context.Background(), "https://www.youtube.com/shorts/"+idSRV3, opt)
	if got.Err != "" || got.Text != "welcome to the show\ntoday we cook" {
		t.Fatalf("got %+v", got)
	}
}

// ---- SSRF: the guard runs before any process is started ------------------

func TestSubtitleAdapterRefusesAHostnameThatResolvesPrivate(t *testing.T) {
	for _, tc := range []struct {
		host, ip, raw string
	}{
		{"www.youtube.com", "10.1.2.5", "https://www.youtube.com/watch?v=" + idPlain},
		{"youtu.be", "127.0.0.1", "https://youtu.be/" + idPlain},
		{"www.youtube.com", "169.254.169.254", "https://www.youtube.com/shorts/" + idPlain},
		{"m.youtube.com", "100.64." + "0.9", "https://m.youtube.com/watch?v=" + idPlain},
		{"www.youtube.com", "::1", "https://www.youtube.com/watch?v=" + idPlain},
		{"www.youtube.com", "192.168.1.20", "https://www.youtube.com/watch?v=" + idPlain},
	} {
		t.Run(tc.host+"_"+tc.ip, func(t *testing.T) {
			// the canonical host the binary would contact is www.youtube.com: it resolves private too
			pinDNS(t, map[string][]string{tc.host: {tc.ip}, "www.youtube.com": {tc.ip}})
			opt, root := ytOpts(t)
			got := Fetch(context.Background(), tc.raw, opt)
			if got.Err == "" || got.Text != "" {
				t.Fatalf("not refused: %+v", got)
			}
			if n := len(readCalls(t, root)); n != 0 {
				t.Errorf("binary invoked %d times for a host that resolves to %s", n, tc.ip)
			}
		})
	}
}

// The canonical host is checked even when the URL the caller wrote is fine:
// youtu.be public, www.youtube.com (what the binary would be given) private.
func TestSubtitleAdapterChecksTheCanonicalHostToo(t *testing.T) {
	pinDNS(t, map[string][]string{"youtu.be": {sentinelPublic}, "www.youtube.com": {"10.1.2.5"}})
	opt, root := ytOpts(t)
	got := Fetch(context.Background(), "https://youtu.be/"+idPlain, opt)
	if got.Err == "" || got.Text != "" {
		t.Fatalf("not refused: %+v", got)
	}
	if n := len(readCalls(t, root)); n != 0 {
		t.Errorf("binary invoked %d times", n)
	}
}

// ---- the egress allowlist still applies ----------------------------------

// A lookalike host is not a YouTube URL: it takes the ordinary page path, under
// the same guard, and the binary is never started for it.
func TestLookalikeHostsNeverReachTheBinary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("a plain page on a lookalike host"))
	}))
	defer srv.Close()
	pinDNS(t, map[string][]string{"youtube.com.evil.example": {sentinelPublic}, "youtu.be.evil.example": {sentinelPublic}})
	opt, root := ytOpts(t)
	d := &honestDialer{toReal: srv.Listener.Addr().String()}
	opt.Client = d.client()
	for _, raw := range []string{
		"http://youtube.com.evil.example/watch?v=" + idPlain,
		"http://youtu.be.evil.example/" + idPlain,
	} {
		got := Fetch(context.Background(), raw, opt)
		if got.Err != "" || !strings.Contains(got.Text, "lookalike host") {
			t.Errorf("%s: %+v", raw, got)
		}
	}
	if n := len(readCalls(t, root)); n != 0 {
		t.Errorf("binary invoked %d times for lookalike hosts", n)
	}
}

// The adapters add no HTTP client of their own: the bare-client allowlist
// (the egress layer's lint, bare_http_client_allowlist_test.go) lists exactly
// one in this package, Fetch's, and a second one anywhere here would dial a
// caller-named host outside netguard's pinned transport.
func TestAdaptersBuildNoHTTPClientOfTheirOwn(t *testing.T) {
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		src := string(raw)
		count := strings.Count(src, "http.Client{") + strings.Count(src, "http.DefaultClient") +
			strings.Count(src, "http.Get(") + strings.Count(src, "http.Post(") + strings.Count(src, "http.NewRequest(") +
			strings.Count(src, "http.Transport{") + strings.Count(src, "net.Dial")
		want := 0
		if name == "fetch.go" {
			want = 1 // the one client Fetch builds, wrapped in netguard.PublicTransport before the first dial
		}
		if count != want {
			t.Errorf("%s builds %d HTTP client or dial constructs, the reviewed count is %d: the adapters must go through Fetch", name, count, want)
		}
	}
}

// ---- rolling captions and text conversion ---------------------------------

func TestRollingCaptionsAreDeduplicated(t *testing.T) {
	want := "welcome to the show\ntoday we cook\npasta & sauce"
	if got := VTTToText(fakeRollingVTT); got != want {
		t.Fatalf("VTTToText = %q, want %q", got, want)
	}
	// and end to end through the adapter
	pinYouTube(t)
	opt, _ := ytOpts(t)
	got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idRolling, opt)
	if got.Err != "" || got.Text != want {
		t.Fatalf("adapter text = %q (%s), want %q", got.Text, got.Err, want)
	}
}

func TestVTTToTextStripsEverythingThatIsNotSpeech(t *testing.T) {
	src := "\ufeffWEBVTT - some title\nX-TIMESTAMP-MAP=MPEGTS:900000,LOCAL:00:00:00.000\n\n" +
		"STYLE\n::cue { color: red }\n\nREGION\nid:r1\n\n" +
		"NOTE hidden\n\n" +
		"intro\n00:00:01.000 --> 00:00:02.000 align:middle\n<c.yellow><b>Bold</b></c> and <00:00:01.500>timed &lt;ok&gt;&nbsp;now\r\n\r\n" +
		"00:01:00.000 --> 00:01:02.000\n\n" +
		"00:01:03.000 --> 00:01:04.000\nlast line"
	want := "Bold and timed <ok> now\nlast line"
	if got := VTTToText(src); got != want {
		t.Fatalf("VTTToText = %q, want %q", got, want)
	}
	for _, leak := range []string{"-->", "WEBVTT", "align:", "STYLE", "hidden", "color", "<c", "00:0"} {
		if strings.Contains(VTTToText(src), leak) {
			t.Errorf("output kept %q", leak)
		}
	}
}

func TestProgressiveRevealKeepsTheFullerLine(t *testing.T) {
	src := "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nhello\n\n00:00:02.000 --> 00:00:03.000\nhello there\n\n00:00:03.000 --> 00:00:04.000\nhello there friend\n\n00:00:04.000 --> 00:00:05.000\nnext sentence\n"
	if got, want := VTTToText(src), "hello there friend\nnext sentence"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSRV3ToTextRejectsGarbage(t *testing.T) {
	if _, err := SRV3ToText([]byte("not xml at all <<<")); err == nil {
		t.Error("garbage parsed as srv3")
	}
}

// ---- digest parity --------------------------------------------------------

// The subtitle text and the same words pasted as a page must reach the digest
// stage as the same bytes. The model is not called: the text compared is the
// context document Build hands the seat.
func TestSubtitleTextAndPastedPageGiveTheSameDigestInput(t *testing.T) {
	pinYouTube(t)
	opt, _ := ytOpts(t)
	sub := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idRolling, opt)
	if sub.Err != "" {
		t.Fatalf("subtitle fetch: %s", sub.Err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(sub.Text))
	}))
	defer srv.Close()
	pinDNS(t, map[string][]string{"paste.example": {sentinelPublic}})
	d := &honestDialer{toReal: srv.Listener.Addr().String()}
	page := Fetch(context.Background(), "http://paste.example/", Options{Client: d.client(), Timeout: 5 * time.Second})
	if page.Err != "" {
		t.Fatalf("page fetch: %s", page.Err)
	}

	specsSub, _ := Build(Request{Goal: "summarize the talk"}, []Fetched{sub})
	specsPage, _ := Build(Request{Goal: "summarize the talk"}, []Fetched{page})
	if len(specsSub) != 1 || len(specsPage) != 1 {
		t.Fatalf("specs: %d and %d", len(specsSub), len(specsPage))
	}
	a := specsSub[0].AgentContract.Context[0].Text
	b := specsPage[0].AgentContract.Context[0].Text
	if a == "" || !bytes.Equal([]byte(a), []byte(b)) {
		t.Fatalf("digest input differs:\nsubtitles: %q\npasted:    %q", a, b)
	}
}

// ---- RSS and Atom ---------------------------------------------------------

const rssFixture = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel>
<title>Seat Notes</title><link>https://feeds.example/</link><description>Weekly notes</description>
<item><title>First &amp; foremost</title><link>https://feeds.example/posts/1</link><pubDate>Tue, 13 Oct 2026 10:00:00 GMT</pubDate>
<description><![CDATA[<p>Alpha <b>beta</b> gamma.</p><script>evil()</script>]]></description></item>
<item><title>Second</title><link>https://feeds.example/posts/2</link><pubDate>Mon, 12 Oct 2026 08:30:00 GMT</pubDate><description>Plain summary two.</description></item>
</channel></rss>`

const atomFixture = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom"><title>Atom Notes</title><updated>2026-10-12T09:00:00Z</updated>
<entry><title>Entry one</title><link rel="enclosure" href="https://feeds.example/file.mp3"/><link rel="alternate" href="https://feeds.example/a/1"/>
<updated>2026-10-12T09:00:00Z</updated><summary>Summary one.</summary></entry>
<entry><title>Entry two</title><link href="https://feeds.example/a/2"/><published>2026-10-11T09:00:00Z</published>
<content type="html">&lt;p&gt;Content &lt;i&gt;two&lt;/i&gt; body.&lt;/p&gt;</content></entry>
</feed>`

func TestFeedToTextRSS(t *testing.T) {
	title, text, err := FeedToText([]byte(rssFixture))
	if err != nil {
		t.Fatal(err)
	}
	if title != "Seat Notes" {
		t.Errorf("title %q", title)
	}
	for _, want := range []string{
		"Feed: Seat Notes", "1. First & foremost", "Link: https://feeds.example/posts/1", "Date: Tue, 13 Oct 2026 10:00:00 GMT",
		"Alpha beta gamma.", "2. Second", "Plain summary two.", "Link: https://feeds.example/posts/2",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "evil()") || strings.Contains(text, "<p>") {
		t.Errorf("markup or script survived:\n%s", text)
	}
}

func TestFeedToTextAtom(t *testing.T) {
	title, text, err := FeedToText([]byte(atomFixture))
	if err != nil {
		t.Fatal(err)
	}
	if title != "Atom Notes" {
		t.Errorf("title %q", title)
	}
	for _, want := range []string{
		"1. Entry one", "Link: https://feeds.example/a/1", "Date: 2026-10-12T09:00:00Z", "Summary one.",
		"2. Entry two", "Link: https://feeds.example/a/2", "Date: 2026-10-11T09:00:00Z", "Content two body.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "file.mp3") {
		t.Errorf("an enclosure link was taken for the entry link:\n%s", text)
	}
}

func TestFeedToTextBoundsItemsAndSummaries(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<rss version="2.0"><channel><title>Big</title>`)
	for i := 0; i < MaxFeedItems+30; i++ {
		fmt.Fprintf(&b, `<item><title>Item %d</title><link>https://feeds.example/%d</link><description>%s</description></item>`, i, i, strings.Repeat("word ", 2000))
	}
	b.WriteString(`</channel></rss>`)
	_, text, err := FeedToText([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, fmt.Sprintf("Item %d\n", MaxFeedItems-1)) || strings.Contains(text, fmt.Sprintf("Item %d\n", MaxFeedItems)) {
		t.Errorf("item cap not applied")
	}
	if !strings.Contains(text, fmt.Sprintf("%d of %d items", MaxFeedItems, MaxFeedItems+30)) {
		t.Errorf("the cut is not stated:\n%s", text[:min(400, len(text))])
	}
	if len(text) > TextCap {
		t.Errorf("text is %d bytes, above the %d cap", len(text), TextCap)
	}
}

func TestFeedToTextRefusesNonFeeds(t *testing.T) {
	for _, body := range []string{"", "plain text", "<html><body><rss>not a feed</rss></body></html>", "<?xml version=\"1.0\"?><svg xmlns=\"http://www.w3.org/2000/svg\"/>", "<rss"} {
		if _, _, err := FeedToText([]byte(body)); err == nil {
			t.Errorf("%q parsed as a feed", body)
		}
	}
}

func serveFeed(t *testing.T, ct, body string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchReadsFeedsByContentTypeAndBySniffing(t *testing.T) {
	pinDNS(t, map[string][]string{"feeds.example": {sentinelPublic}})
	for _, tc := range []struct{ name, ct, body, wantTitle string }{
		{"rss media type", "application/rss+xml; charset=utf-8", rssFixture, "Seat Notes"},
		{"atom media type", "application/atom+xml", atomFixture, "Atom Notes"},
		{"generic xml, sniffed", "text/xml", rssFixture, "Seat Notes"},
		{"application/xml, sniffed", "application/xml", atomFixture, "Atom Notes"},
		{"no content type, sniffed", "", rssFixture, "Seat Notes"},
		{"text/plain, sniffed", "text/plain", "\ufeff\n  " + atomFixture, "Atom Notes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := serveFeed(t, tc.ct, tc.body, &hits)
			d := &honestDialer{toReal: srv.Listener.Addr().String()}
			got := Fetch(context.Background(), "http://feeds.example/feed.xml", Options{Client: d.client(), Timeout: 5 * time.Second})
			if got.Err != "" {
				t.Fatalf("fetch: %s", got.Err)
			}
			if got.Title != tc.wantTitle || !strings.Contains(got.Text, "Link: https://feeds.example/") || !strings.Contains(got.Text, "1. ") {
				t.Fatalf("not digested as a feed: title %q\n%s", got.Title, got.Text)
			}
			if n := hits.Load(); n != 1 {
				t.Errorf("the server saw %d requests: item links must not be followed", n)
			}
		})
	}
}

func TestFetchLeavesOrdinaryXMLAndHTMLAlone(t *testing.T) {
	pinDNS(t, map[string][]string{"feeds.example": {sentinelPublic}})
	for _, tc := range []struct{ name, ct, body, want string }{
		{"sitemap", "application/xml", `<?xml version="1.0"?><urlset><url><loc>https://feeds.example/a</loc></url></urlset>`, "https://feeds.example/a"},
		{"html mentioning rss", "text/html", `<html><body><p>Subscribe to our <rss>feed</rss> today</p></body></html>`, "Subscribe to our"},
		{"a feed word in prose", "text/plain", "This post explains the <feed> element of Atom.", "explains the"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := serveFeed(t, tc.ct, tc.body, &hits)
			d := &honestDialer{toReal: srv.Listener.Addr().String()}
			got := Fetch(context.Background(), "http://feeds.example/x", Options{Client: d.client(), Timeout: 5 * time.Second})
			if got.Err != "" || !strings.Contains(got.Text, tc.want) || strings.Contains(got.Text, "Feed: ") {
				t.Fatalf("changed behaviour: %+v", got)
			}
		})
	}
}

func TestFeedURLOnAPrivateHostIsRefusedBeforeAnyDial(t *testing.T) {
	var hits atomic.Int32
	srv := serveFeed(t, "application/rss+xml", rssFixture, &hits)
	pinDNS(t, map[string][]string{"feeds.example": {"10.1.2.3"}})
	d := &honestDialer{toReal: srv.Listener.Addr().String()}
	got := Fetch(context.Background(), "http://feeds.example/feed.xml", Options{Client: d.client(), Timeout: 5 * time.Second})
	if got.Err == "" || got.Text != "" {
		t.Fatalf("not refused: %+v", got)
	}
	if len(d.dialed()) != 0 || hits.Load() != 0 {
		t.Errorf("dialed %v, server saw %d", d.dialed(), hits.Load())
	}
}

// A feed that redirects to a private address is refused on the hop, the same
// as a page: the feed parser sits after the fetch, never in front of it.
func TestFeedRedirectToPrivateIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://internal-feed.example/feed.xml", http.StatusFound)
	}))
	defer srv.Close()
	pinDNS(t, map[string][]string{"feeds.example": {sentinelPublic}, "internal-feed.example": {"192.168.1.9"}})
	d := &honestDialer{toReal: srv.Listener.Addr().String()}
	got := Fetch(context.Background(), "http://feeds.example/feed.xml", Options{Client: d.client(), Timeout: 5 * time.Second})
	if got.Err == "" || got.Text != "" {
		t.Fatalf("not refused: %+v", got)
	}
}
