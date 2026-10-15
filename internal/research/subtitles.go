package research

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/dmmdea/offload-harness/internal/winexec"
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
	// ErrKindYtDlpPath: the configured binary is not an absolute path, is missing, does not match the pinned
	// ytdlp_sha256 (or that value is not a sha256), or will not start.
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
	// ErrKindURLRefused: the canonical URL the binary would be given did not pass the public-web guard.
	ErrKindURLRefused = "url_refused"
)

// DefaultSubLangs is the subtitle language selection when the caller gives none.
const DefaultSubLangs = "en.*"

const (
	ytDlpDefaultTimeout = 60 * time.Second
	// ytDlpMaxTimeout caps one run whatever fetch_timeout_sec asks for. The sweep below treats a scratch directory
	// older than this (plus slack) as abandoned, which is only true while no run can outlive it.
	ytDlpMaxTimeout    = 15 * time.Minute
	ytDlpSocketTimeout = "15"
	ytDlpOutputCap     = 8 << 10

	// scratchPrefix names the per-call directories this adapter makes; the sweep touches only directories that
	// carry it, directly under the scratch root it was given.
	scratchPrefix = "ytdlp-"
	// scratchSweepAge: a directory last touched longer ago than the longest run, with room for the run's cleanup.
	scratchSweepAge = ytDlpMaxTimeout + 5*time.Minute
)

// ytDlpRunCap is the longest one run may be given (a var so a test can shorten it); it is ytDlpMaxTimeout.
var ytDlpRunCap = ytDlpMaxTimeout

// removeRetryWaits are the pauses between attempts to remove the scratch directory. On Windows a directory cannot
// be removed while a process still has it as its working directory or a file in it open, and a process that has just
// been ended takes a moment to let go; the total stays a few hundred milliseconds.
var removeRetryWaits = []time.Duration{25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond}

// SourceKind is the class a URL is routed to.
type SourceKind int

const (
	// SourceKindPage is everything else: the ordinary fetch (and the feed parser when the response is a feed).
	SourceKindPage SourceKind = iota
	// SourceKindYouTube is a watch, youtu.be, shorts, live or embed URL on youtube.com (www, m or music) or an
	// embed URL on youtube-nocookie.com.
	SourceKindYouTube
)

var reVideoID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// ClassifyURL is the explicit matcher for a video: youtube.com/watch?v=, and
// /shorts/<id>, /live/<id> and /embed/<id>, on exactly youtube.com, www.youtube.com,
// m.youtube.com and music.youtube.com; /embed/<id> on youtube-nocookie.com and
// www.youtube-nocookie.com; and youtu.be/<id> (also www.youtu.be). Over http or
// https, on the default port, with no userinfo and a video id of the real shape.
// Anything else, a lookalike host or a trailing-dot name included, is a page. It
// returns the video id for a YouTube URL. The binary is always given the canonical
// www.youtube.com/watch?v=<id>, whichever of these the caller wrote.
func ClassifyURL(u *url.URL) (SourceKind, string) {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Port() != "" {
		return SourceKindPage, ""
	}
	host := strings.ToLower(u.Hostname())
	// idAfter is the path segment after prefix, with at most one trailing slash.
	idAfter := func(prefix string) string {
		if !strings.HasPrefix(u.Path, prefix) {
			return ""
		}
		return strings.TrimSuffix(strings.TrimPrefix(u.Path, prefix), "/")
	}
	id := ""
	switch host {
	case "youtube.com", "www.youtube.com", "m.youtube.com", "music.youtube.com":
		if u.Path == "/watch" {
			id = u.Query().Get("v")
		}
		for _, prefix := range []string{"/shorts/", "/live/", "/embed/"} {
			if id == "" {
				id = idAfter(prefix)
			}
		}
	case "youtube-nocookie.com", "www.youtube-nocookie.com":
		id = idAfter("/embed/")
	case "youtu.be", "www.youtu.be":
		id = idAfter("/")
	}
	// "videoseries" and "live_stream" are eleven letters, so they have the shape of an id: they are the embed paths of a
	// playlist and of a channel's live stream, not videos
	if id == "videoseries" || id == "live_stream" || !reVideoID.MatchString(id) {
		return SourceKindPage, ""
	}
	return SourceKindYouTube, id
}

var reSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// hashFile is the sha256 (lower-case hex) of the file at path; a var so a test can see when it is called.
var hashFile = func(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// removeAll removes the per-call scratch directory (a var so a test can make removal fail).
var removeAll = os.RemoveAll

// removeScratch removes dir, retrying briefly: the child may still be letting go of it.
func removeScratch(dir string) error {
	err := removeAll(dir)
	for _, wait := range removeRetryWaits {
		if err == nil {
			break
		}
		time.Sleep(wait)
		err = removeAll(dir)
	}
	return err
}

// sweepStaleScratch removes the ytdlp-* directories under root that nothing can still be using: the ones a killed
// harness, or a child that outlived its run, left behind. Only directories (not links or files) whose name carries
// scratchPrefix and whose last change is older than scratchSweepAge go; it is best effort, and a directory that is
// still held is tried again at the next call.
func sweepStaleScratch(root string) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-scratchSweepAge)
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), scratchPrefix) {
			continue
		}
		if fi, err := e.Info(); err != nil || fi.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}

var reSubLangs = regexp.MustCompile(`^[A-Za-z0-9_.*,-]{1,64}$`)

// fetchSubtitles runs the configured yt-dlp for one validated YouTube URL.
// Fetch has already passed the URL the caller wrote through ValidateURL; the
// canonical URL the binary is given is validated here as well, because its host
// (www.youtube.com) is the one the binary will actually contact.
func fetchSubtitles(ctx context.Context, raw, id string, opt Options) (res Fetched) {
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
	// ytdlp_path is trusted as written unless the operator pinned a hash: then the file is hashed before every run
	// and a different one is never started. (A file swapped between this check and the start is not caught: the
	// pin is against a wrong or replaced binary, not against a writer who races each call.)
	if want := strings.ToLower(strings.TrimSpace(opt.YtDlpSHA256)); want != "" {
		if !reSHA256.MatchString(want) {
			return fail(ErrKindYtDlpPath, "ytdlp_sha256 must be a sha256, 64 hexadecimal digits, got %q", cutRunes(opt.YtDlpSHA256, 80))
		}
		got, herr := hashFile(bin)
		if herr != nil {
			return fail(ErrKindYtDlpPath, "cannot hash ytdlp_path %q to check it against ytdlp_sha256: %v", bin, herr)
		}
		if got != want {
			return fail(ErrKindYtDlpPath, "ytdlp_path %q does not match ytdlp_sha256 (the file hashes to %s), so it was not run", bin, got)
		}
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
		return fail(ErrKindURLRefused, "%v", err)
	}
	out.FinalURL = canonical

	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = ytDlpDefaultTimeout
	}
	timeout = min(timeout, ytDlpRunCap)
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
	sweepStaleScratch(root)
	dir, err := os.MkdirTemp(root, scratchPrefix)
	if err != nil {
		return fail(ErrKindYtDlpPath, "scratch directory: %v", err)
	}
	// The directory goes whatever the outcome. A removal that still fails after the brief retries is not dropped: it is
	// recorded on the result (beside the text, when there is one), and the sweep above takes the directory at a later call.
	defer func() {
		if rerr := removeScratch(dir); rerr != nil {
			res.CleanupErr = "scratch directory not removed: " + cutRunes(rerr.Error(), 300)
		}
	}()

	args := []string{
		"--ignore-config", "--no-playlist", "--skip-download",
		"--write-subs", "--write-auto-subs", "--sub-langs=" + langs, "--sub-format=vtt/srv3/best",
		"--no-cookies", "--no-cookies-from-browser", "--no-cache-dir", "--no-update",
		"--no-progress", "--quiet", "--ignore-no-formats-error", // no --no-warnings: a 429 on the subtitle download is only a WARNING
		"--socket-timeout", ytDlpSocketTimeout,
		"--retries", "0", "--extractor-retries", "0", "--fragment-retries", "0",
		"-P", "home:" + dir, "-P", "temp:" + dir, "-o", "%(id)s.%(ext)s",
		"--", canonical,
	}
	// winexec.CommandContext starts the child hidden: a console program started by a process with no console of its
	// own would otherwise open a window of its own on the operator's desktop (TestEveryChildProcessStartIsHidden).
	cmd := winexec.CommandContext(rctx, bin, args...)
	// A deadline ends the whole tree the binary started, not only the program launched: the documented Windows shape
	// is a wrapper .cmd, whose interpreter is a grandchild that would otherwise keep running with the per-call
	// directory as its working directory.
	winexec.KillTreeOnCancel(cmd)
	cmd.Dir = dir
	cmd.Env = ytDlpEnv(dir)
	cmd.WaitDelay = 2 * time.Second
	var diag cappedBuffer
	cmd.Stdout, cmd.Stderr = &diag, &diag
	runErr := cmd.Run()

	if rerr := rctx.Err(); rerr != nil {
		return fail(ErrKindTimeout, "yt-dlp did not finish before the deadline (%v)", rerr)
	}
	// exec reports ErrWaitDelay when the program exited 0 but a child of it still held the output pipe once
	// WaitDelay ran out. The program succeeded: what is on disk is read like any other exit 0.
	if errors.Is(runErr, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0 {
		runErr = nil
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
		// yt-dlp exits 0 when a subtitle download is refused (a rate limit is only a WARNING), so what it printed is
		// the difference between a video without subtitles and one that could not be read right now
		if strings.TrimSpace(diag.b.String()) != "" {
			return fail(ErrKindNoSubtitles, "yt-dlp wrote no subtitle file for languages %q: %s", langs, diag.tail())
		}
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
		var srv3Cut bool
		text, srv3Cut, err = srv3ToText(data)
		out.ContentType = "application/xml"
		if err != nil {
			return fail(ErrKindUnsupportedSubs, "the srv3 subtitle file did not parse: %v", err)
		}
		// a track cut by the read bound ends inside a cue: the cues before it are the text, and the cut is said
		if srv3Cut {
			out.Truncated = true
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
// with the same rolling-caption handling as VTTToText. A file that ends inside an element (the read bound cut it)
// yields the cues read before the cut; one with no complete cue, or no timedtext root, is an error.
func SRV3ToText(data []byte) (string, error) {
	text, _, err := srv3ToText(data)
	return text, err
}

// srv3ToText is SRV3ToText that also says whether the file was cut off before its end.
func srv3ToText(data []byte) (text string, cut bool, err error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = false
	d.Entity = xml.HTMLEntity
	var lines []string
	rootSeen := false
read:
	for {
		tok, terr := d.Token()
		if terr != nil {
			if !rootSeen {
				return "", false, fmt.Errorf("not srv3 timed text: %w", terr)
			}
			cut = !errors.Is(terr, io.EOF)
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if !rootSeen {
			if se.Name.Local != "timedtext" {
				return "", false, fmt.Errorf("not srv3 timed text: root element is %q", se.Name.Local)
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
			t, terr := d.Token()
			if terr != nil {
				// the file ended inside this cue: it is not a cue, and nothing after it was read
				cut = true
				break read
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
	if cut && len(lines) == 0 {
		return "", false, errors.New("not srv3 timed text: the file ends before its first cue is complete")
	}
	return joinCaptionLines(lines), cut, nil
}
