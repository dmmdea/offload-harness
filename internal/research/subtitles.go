package research

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// YouTube subtitles as a text source. The operator names a yt-dlp executable
// (config ytdlp_path; empty = this adapter is off and a YouTube URL is read as
// an ordinary page). The binary is run once per URL with flags that fetch
// subtitles and nothing else, in a per-call directory that is removed after the
// call, under the caller's deadline, with no retry. The subtitle file it writes
// is converted to plain text and joins the ordinary digest path exactly as a
// fetched page's text does.

// Error kinds a source can fail with. They ride on Fetched.ErrKind so a caller
// can tell a broken site from an absent track without parsing prose.
const (
	// ErrKindYtDlpPath: the configured binary is not an absolute path, is missing, or will not start.
	ErrKindYtDlpPath = "ytdlp_path"
	// ErrKindBadLangs: the sub_langs value is not a plain language list.
	ErrKindBadLangs = "bad_sub_langs"
	// ErrKindSiteBreak: the binary ran and exited non-zero (the site changed, asked for a login, or refused).
	ErrKindSiteBreak = "subtitle_site_break"
	// ErrKindNoSubtitles: the binary exited zero and wrote no subtitle file in the asked languages.
	ErrKindNoSubtitles = "no_subtitles"
	// ErrKindUnsupportedSubs: a subtitle file came back in a format this adapter does not read.
	ErrKindUnsupportedSubs = "unsupported_subtitle_format"
	// ErrKindEmptySubtitles: a subtitle file was read and held no speech text (blank cues only).
	ErrKindEmptySubtitles = "empty_subtitles"
	// ErrKindTimeout: the caller's deadline (or the per-fetch timeout) ended the run.
	ErrKindTimeout = "subtitle_timeout"
)

// DefaultSubLangs is the subtitle language selection when the caller gives none.
const DefaultSubLangs = "en.*"

const (
	ytDlpDefaultTimeout = 60 * time.Second
	ytDlpSocketTimeout  = "15"
	ytDlpOutputCap      = 8 << 10
)

// SourceKind is the class a URL is routed to.
type SourceKind int

const (
	// SourceKindPage is everything else: the ordinary fetch (and the feed parser when the response is a feed).
	SourceKindPage SourceKind = iota
	// SourceKindYouTube is a watch, youtu.be or shorts URL.
	SourceKindYouTube
)

var reVideoID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// ClassifyURL is the explicit matcher: youtube.com/watch?v=, youtu.be/<id> and
// youtube.com/shorts/<id> on exactly youtube.com, www.youtube.com, m.youtube.com
// and youtu.be, over http or https, on the default port, with no userinfo and a
// video id of the real shape. Anything else, a lookalike host included, is a
// page. It returns the video id for a YouTube URL.
func ClassifyURL(u *url.URL) (SourceKind, string) {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Port() != "" {
		return SourceKindPage, ""
	}
	host := strings.ToLower(u.Hostname())
	id := ""
	switch host {
	case "youtube.com", "www.youtube.com", "m.youtube.com":
		switch {
		case u.Path == "/watch":
			id = u.Query().Get("v")
		case strings.HasPrefix(u.Path, "/shorts/"):
			id = strings.TrimSuffix(strings.TrimPrefix(u.Path, "/shorts/"), "/")
		}
	case "youtu.be", "www.youtu.be":
		id = strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/")
	}
	if !reVideoID.MatchString(id) {
		return SourceKindPage, ""
	}
	return SourceKindYouTube, id
}

var reSubLangs = regexp.MustCompile(`^[A-Za-z0-9_.*,-]{1,64}$`)

// fetchSubtitles runs the configured yt-dlp for one validated YouTube URL.
// Fetch has already passed the URL the caller wrote through ValidateURL; the
// canonical URL the binary is given is validated here as well, because its host
// (www.youtube.com) is the one the binary will actually contact.
func fetchSubtitles(ctx context.Context, raw, id string, opt Options) Fetched {
	out := Fetched{URL: raw}
	start := time.Now()
	fail := func(kind, format string, a ...any) Fetched {
		out.ErrKind = kind
		out.Err = kind + ": " + fmt.Sprintf(format, a...)
		out.Elapsed = time.Since(start)
		return out
	}

	bin := opt.YtDlpPath
	if !filepath.IsAbs(bin) {
		return fail(ErrKindYtDlpPath, "ytdlp_path must be an absolute path (the harness never searches PATH), got %q", bin)
	}
	if fi, err := os.Stat(bin); err != nil || fi.IsDir() {
		return fail(ErrKindYtDlpPath, "ytdlp_path %q is not an executable file", bin)
	}
	langs := strings.TrimSpace(opt.SubLangs)
	if langs == "" {
		langs = DefaultSubLangs
	}
	if !reSubLangs.MatchString(langs) {
		return fail(ErrKindBadLangs, "sub_langs must be a comma-separated language list (letters, digits, . * _ - ,), got %q", opt.SubLangs)
	}

	canonical := "https://www.youtube.com/watch?v=" + id
	if _, err := ValidateURL(ctx, canonical); err != nil {
		out.Err = err.Error()
		out.Elapsed = time.Since(start)
		return out
	}
	out.FinalURL = canonical

	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = ytDlpDefaultTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	root := opt.ScratchRoot
	if root == "" {
		root = filepath.Join(os.TempDir(), "local-offload-research")
	}
	// The binary runs with the per-call directory as its working directory and is given the same
	// directory as -P home:, which it resolves against that working directory: a relative root
	// would send its output to <dir>/<dir>. The path is made absolute before any of it is used.
	root, err := filepath.Abs(root)
	if err != nil {
		return fail(ErrKindYtDlpPath, "scratch root: %v", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fail(ErrKindYtDlpPath, "scratch root: %v", err)
	}
	dir, err := os.MkdirTemp(root, "ytdlp-")
	if err != nil {
		return fail(ErrKindYtDlpPath, "scratch directory: %v", err)
	}
	defer os.RemoveAll(dir)

	args := []string{
		"--ignore-config", "--no-playlist", "--skip-download",
		"--write-subs", "--write-auto-subs", "--sub-langs=" + langs, "--sub-format=vtt/srv3/best",
		"--no-cookies", "--no-cookies-from-browser", "--no-cache-dir", "--no-update",
		"--no-progress", "--no-warnings", "--quiet", "--ignore-no-formats-error",
		"--socket-timeout", ytDlpSocketTimeout,
		"--retries", "0", "--extractor-retries", "0", "--fragment-retries", "0",
		"-P", "home:" + dir, "-P", "temp:" + dir, "-o", "%(id)s.%(ext)s",
		"--", canonical,
	}
	cmd := exec.CommandContext(rctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = ytDlpEnv(dir)
	cmd.WaitDelay = 2 * time.Second
	var diag cappedBuffer
	cmd.Stdout, cmd.Stderr = &diag, &diag
	runErr := cmd.Run()

	if rerr := rctx.Err(); rerr != nil {
		return fail(ErrKindTimeout, "yt-dlp did not finish before the deadline (%v)", rerr)
	}
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			return fail(ErrKindSiteBreak, "yt-dlp exit status %d: %s", ee.ExitCode(), diag.tail())
		}
		return fail(ErrKindYtDlpPath, "cannot run yt-dlp: %v", runErr)
	}

	name, ext, ok := pickSubtitleFile(dir, id)
	if name == "" {
		return fail(ErrKindNoSubtitles, "yt-dlp wrote no subtitle file for languages %q", langs)
	}
	if !ok {
		return fail(ErrKindUnsupportedSubs, "yt-dlp wrote %q, a format this adapter does not read (vtt, srv3, srt only)", filepath.Ext(name))
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return fail(ErrKindNoSubtitles, "reading the subtitle file: %v", err)
	}
	data, cut, err := readCapped(f, MaxFetchBytes)
	f.Close()
	if err != nil {
		return fail(ErrKindNoSubtitles, "reading the subtitle file: %v", err)
	}
	out.Truncated = cut
	out.Bytes = len(data)

	var text string
	switch ext {
	case ".srv3":
		text, err = SRV3ToText(data)
		out.ContentType = "application/xml"
		if err != nil {
			return fail(ErrKindUnsupportedSubs, "the srv3 subtitle file did not parse: %v", err)
		}
	default:
		text = VTTToText(string(data))
		out.ContentType = "text/vtt"
	}
	out.Title = fmt.Sprintf("YouTube video %s subtitles", id)
	finishText(&out, text)
	if out.Text == "" {
		// the shared empty-text message speaks of a page: a subtitle file that held no speech says so
		out.ErrKind = ErrKindEmptySubtitles
		out.Err = ErrKindEmptySubtitles + ": the subtitle file held no speech text (blank cues only)"
	}
	out.Elapsed = time.Since(start)
	return out
}

// readCapped reads at most max bytes of r, reading one byte past the bound to know whether the
// source was longer, and says so in cut. It never holds more than max+1 bytes.
func readCapped(r io.Reader, max int) (data []byte, cut bool, err error) {
	data, err = io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > max {
		return data[:max], true, nil
	}
	return data, false, nil
}

// finishText applies the bounds every text source shares with a fetched page:
// the text cap, trimming, and the empty-text refusal.
func finishText(out *Fetched, text string) {
	if len(text) > TextCap {
		text = cutBytes(text, TextCap)
		out.Truncated = true
	}
	text = strings.TrimSpace(text)
	if text == "" {
		out.Err = "page stripped to empty text"
		return
	}
	out.Text = text
	out.TextBytes = len(text)
}

// pickSubtitleFile chooses the track to digest: of the files named for this
// video, a readable format wins over an unreadable one, and among those the
// shortest name (the plain language code over its -orig or regional twins), then
// the alphabetically first. name is empty when there is no subtitle file at all;
// ok is false when only formats this adapter cannot read are there.
func pickSubtitleFile(dir, id string) (name, ext string, ok bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", "", false
	}
	var readable, other []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, id+".") {
			continue
		}
		switch strings.ToLower(filepath.Ext(n)) {
		case ".vtt", ".srv3", ".srt":
			readable = append(readable, n)
		default:
			other = append(other, n)
		}
	}
	byLen := func(s []string) {
		sort.Slice(s, func(i, j int) bool {
			if len(s[i]) != len(s[j]) {
				return len(s[i]) < len(s[j])
			}
			return s[i] < s[j]
		})
	}
	if len(readable) > 0 {
		byLen(readable)
		return readable[0], strings.ToLower(filepath.Ext(readable[0])), true
	}
	if len(other) > 0 {
		byLen(other)
		return other[0], strings.ToLower(filepath.Ext(other[0])), false
	}
	return "", "", false
}

// ytDlpEnv is the child's whole environment: the few variables an executable needs to start and
// find its certificates, the operator's proxy settings, and the per-call directory as home and temp
// so nothing is read from or left in a user profile.
func ytDlpEnv(dir string) []string {
	var env []string
	for _, k := range []string{"PATH", "SystemRoot", "SYSTEMROOT", "WINDIR", "ComSpec", "PATHEXT", "LANG", "LC_ALL",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
		"http_proxy", "https_proxy", "no_proxy", "all_proxy"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env,
		"HOME="+dir, "USERPROFILE="+dir, "APPDATA="+dir, "LOCALAPPDATA="+dir, "XDG_CONFIG_HOME="+dir, "XDG_CACHE_HOME="+dir,
		"TMP="+dir, "TEMP="+dir, "TMPDIR="+dir,
		"PYTHONUTF8=1", "PYTHONNOUSERSITE=1", "PYTHONDONTWRITEBYTECODE=1", "NO_COLOR=1")
}

// cappedBuffer keeps the head of what a child writes, so a chatty failure cannot grow without bound.
type cappedBuffer struct{ b bytes.Buffer }

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := ytDlpOutputCap - c.b.Len(); room > 0 {
		c.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// tail is the last non-empty line of the output, printable, short: the line yt-dlp ends on is its error.
func (c *cappedBuffer) tail() string {
	lines := strings.Split(strings.TrimSpace(c.b.String()), "\n")
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			last = s
			break
		}
	}
	last = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, last)
	if last == "" {
		return "(no output)"
	}
	return cutRunes(last, 300)
}

// ---- subtitle text -------------------------------------------------------

var reCueTags = regexp.MustCompile(`<[^>]*>`)

// VTTToText converts WebVTT (or SubRip) to plain speech text: the header, NOTE, STYLE and
// REGION blocks, cue identifiers, timing lines and settings, inline timestamps and tags are
// dropped, entities are decoded, and the rolling repetition automatic captions carry is
// removed (a line equal to the one before it is dropped, and a line that only extends the
// one before it replaces it).
func VTTToText(src string) string {
	src = strings.TrimPrefix(src, "\ufeff")
	src = strings.ReplaceAll(strings.ReplaceAll(src, "\r\n", "\n"), "\r", "\n")
	var lines []string
	for _, block := range strings.Split(src, "\n\n") {
		rows := strings.Split(block, "\n")
		timing := -1
		for i, r := range rows {
			if strings.Contains(r, "-->") {
				timing = i
				break
			}
		}
		if timing < 0 {
			continue
		}
		// The rows after the timing line are the cue's text. A row holding only a space (automatic
		// captions have them) is not a block separator, which is an empty row, so it stays in its block.
		for _, r := range rows[timing+1:] {
			lines = append(lines, cleanCaptionLine(r))
		}
	}
	return joinCaptionLines(lines)
}

func cleanCaptionLine(r string) string {
	r = html.UnescapeString(reCueTags.ReplaceAllString(r, ""))
	return strings.Join(strings.Fields(r), " ")
}

// joinCaptionLines removes the rolling repetition and joins the lines.
func joinCaptionLines(lines []string) string {
	var out []string
	for _, l := range lines {
		if l == "" {
			continue
		}
		if n := len(out); n > 0 {
			prev := out[n-1]
			switch {
			case l == prev:
				continue
			case len(l) > len(prev) && strings.HasPrefix(l, prev) && l[len(prev)] == ' ':
				out[n-1] = l
				continue
			}
		}
		out = append(out, l)
	}
	return normalizeWhitespace(strings.Join(out, "\n"))
}

// SRV3ToText converts YouTube's srv3 timed-text XML (<timedtext><body><p><s>word</s>...</p>) to plain text
// with the same rolling-caption handling as VTTToText.
func SRV3ToText(data []byte) (string, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = false
	d.Entity = xml.HTMLEntity
	var lines []string
	rootSeen := false
	for {
		tok, err := d.Token()
		if err != nil {
			if errors.Is(err, io.EOF) && rootSeen {
				break
			}
			return "", fmt.Errorf("not srv3 timed text: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if !rootSeen {
			if se.Name.Local != "timedtext" {
				return "", fmt.Errorf("not srv3 timed text: root element is %q", se.Name.Local)
			}
			rootSeen = true
			continue
		}
		if se.Name.Local != "p" {
			continue
		}
		var b strings.Builder
		depth := 1
		for depth > 0 {
			t, err := d.Token()
			if err != nil {
				return "", fmt.Errorf("not srv3 timed text: %w", err)
			}
			switch v := t.(type) {
			case xml.StartElement:
				depth++
			case xml.EndElement:
				depth--
			case xml.CharData:
				b.Write(v)
			}
		}
		lines = append(lines, strings.Join(strings.Fields(b.String()), " "))
	}
	return joinCaptionLines(lines), nil
}
