package composebundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// rawBundle builds a gzip tar from explicit headers, so tests can send what Pack never would.
type member struct {
	name string
	typ  byte
	body string
	link string
}

func rawBundle(t *testing.T, ms ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range ms {
		typ := m.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: m.name, Typeflag: typ, Mode: 0o644, Linkname: m.link}
		if typ == tar.TypeReg {
			h.Size = int64(len(m.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(m.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestPackThenExtractRoundTripsAProject(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "index.html", `<link href="css/a.css" rel="stylesheet"><img src="assets/x.png">`)
	writeFile(t, src, "css/a.css", `body{background:url("../assets/x.png")}`)
	writeFile(t, src, "assets/x.png", "PNG")
	writeFile(t, src, ".git/config", "secret-ish")
	writeFile(t, src, "node_modules/gsap/index.js", "x")
	writeFile(t, src, "renders/old.mp4", "x")
	b, err := Pack(src, Limits{})
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	dst := t.TempDir()
	if err := Extract(b, dst, Limits{}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, rel := range []string{"index.html", "css/a.css", "assets/x.png"} {
		want, _ := os.ReadFile(filepath.Join(src, filepath.FromSlash(rel)))
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s did not round-trip: %v", rel, err)
		}
	}
	for _, rel := range []string{".git/config", "node_modules/gsap/index.js", "renders/old.mp4"} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(rel))); err == nil {
			t.Errorf("%s must not be packed", rel)
		}
	}
	if err := Confine(dst, "index.html"); err != nil {
		t.Errorf("a clean project must pass Confine: %v", err)
	}
}

func TestExtractRefusesEveryUnsafeMember(t *testing.T) {
	// want is the refusal each case must get: several rules overlap (an absolute name also has an empty
	// segment), so a case that only asserted "refused" would pass with its own rule gone.
	for name, tc := range map[string]struct {
		ms   []member
		want string
	}{
		"absolute path":     {[]member{{name: "/etc/cron.d/x", body: "x"}}, "is an absolute path"},
		"climbs out":        {[]member{{name: "../x.html", body: "x"}}, "climbs out of the project"},
		"climbs out deeper": {[]member{{name: "a/../../x.html", body: "x"}}, "not a clean relative path"},
		"backslash":         {[]member{{name: `a\..\..\x`, body: "x"}}, "backslashes, colons and NUL"},
		"drive letter":      {[]member{{name: "C:/Windows/x", body: "x"}}, "backslashes, colons and NUL"},
		"colon (ADS)":       {[]member{{name: "index.html:evil", body: "x"}}, "backslashes, colons and NUL"},
		"reserved name":     {[]member{{name: "assets/con.txt", body: "x"}}, `refused path segment "con.txt"`},
		"trailing dot":      {[]member{{name: "assets/x.", body: "x"}}, `refused path segment "x."`},
		"trailing space":    {[]member{{name: "assets/x ", body: "x"}}, `refused path segment "x "`},
		"symlink":           {[]member{{name: "x", typ: tar.TypeSymlink, link: "/etc/passwd"}}, "regular files and directories only"},
		"hard link":         {[]member{{name: "index.html", body: "x"}, {name: "y", typ: tar.TypeLink, link: "index.html"}}, "regular files and directories only"},
		"device":            {[]member{{name: "dev", typ: tar.TypeChar}}, "regular files and directories only"},
		"fifo":              {[]member{{name: "pipe", typ: tar.TypeFifo}}, "regular files and directories only"},
		"duplicate member":  {[]member{{name: "index.html", body: "a"}, {name: "index.html", body: "b"}}, `member "index.html": `},
		"not clean":         {[]member{{name: "a/./b.html", body: "x"}}, "not a clean relative path"},
		"double slash":      {[]member{{name: "a//b.html", body: "x"}}, "not a clean relative path"},
	} {
		dst := t.TempDir()
		err := Extract(rawBundle(t, tc.ms...), dst, Limits{})
		if err == nil {
			t.Errorf("%s: Extract accepted it", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: refused for the wrong reason: %v (want %q)", name, err, tc.want)
		}
	}
}

// inside is the zip-slip check on the joined path; checkName refuses every escaping name before it is
// reached, so it is tested here directly.
func TestInsideRefusesAPathOutsideTheRoot(t *testing.T) {
	root := t.TempDir()
	if !inside(root, filepath.Join(root, "a", "b.html")) {
		t.Error("a path under the root must be inside")
	}
	for _, p := range []string{filepath.Join(root, ".."), filepath.Join(root, "..", "x"), filepath.Join(filepath.Dir(root), "sibling", "x")} {
		if inside(root, p) {
			t.Errorf("%s is outside %s", p, root)
		}
	}
}

// A NUL cannot travel in a tar Go writes, so the name rule is checked on its own.
func TestCheckNameRefusesNUL(t *testing.T) {
	if err := checkName("a\x00b"); err == nil {
		t.Fatal("a NUL in a member name must be refused")
	}
}

func TestExtractCapsCountTheBytesWritten(t *testing.T) {
	big := strings.Repeat("x", 100)
	if err := Extract(rawBundle(t, member{name: "a.bin", body: big}), t.TempDir(), Limits{MaxFileBytes: 50}); err == nil || !strings.Contains(err.Error(), "limit for one file") {
		t.Errorf("per-file cap: %v", err)
	}
	if err := Extract(rawBundle(t, member{name: "a", body: big}, member{name: "b", body: big}), t.TempDir(), Limits{MaxTotal: 150}); err == nil || !strings.Contains(err.Error(), "unpacks to more than") {
		t.Errorf("total cap: %v", err)
	}
	if err := Extract(rawBundle(t, member{name: "a", body: "1"}, member{name: "b", body: "2"}, member{name: "c", body: "3"}), t.TempDir(), Limits{MaxFiles: 2}); err == nil || !strings.Contains(err.Error(), "more than 2 files") {
		t.Errorf("file-count cap: %v", err)
	}
}

// Directory entries count against a cap of their own, and the decompressed stream is bounded as a whole
// (review M3: 20,000 directories from a 118 KB gzip, none counted).
func TestExtractCapsEveryEntryAndTheStream(t *testing.T) {
	var ms []member
	for i := 0; i < 30; i++ {
		ms = append(ms, member{name: fmt.Sprintf("d%02d", i), typ: tar.TypeDir})
	}
	ms = append(ms, member{name: "index.html", body: "x"})
	if err := Extract(rawBundle(t, ms...), t.TempDir(), Limits{MaxFiles: 10}); err == nil || !strings.Contains(err.Error(), "more than 20 entries") {
		t.Errorf("directory entries must count against the entry cap (twice MaxFiles): %v", err)
	}
	cr := &capReader{r: strings.NewReader(strings.Repeat("x", 100)), max: 50}
	if _, err := io.ReadAll(cr); err == nil || !strings.Contains(err.Error(), "more than 50 bytes") {
		t.Errorf("the stream cap must stop the read: %v", err)
	}
}

func TestExtractRefusesBadInput(t *testing.T) {
	if err := Extract([]byte("not gzip"), t.TempDir(), Limits{}); err == nil {
		t.Error("non-gzip input accepted")
	}
	if err := Extract(rawBundle(t), t.TempDir(), Limits{}); err == nil {
		t.Error("an empty bundle accepted")
	}
	dst := t.TempDir()
	writeFile(t, dst, "already.txt", "x")
	if err := Extract(rawBundle(t, member{name: "index.html", body: "x"}), dst, Limits{}); err == nil {
		t.Error("a non-empty target accepted")
	}
}

func project(t *testing.T, files map[string]string) string {
	t.Helper()
	d := t.TempDir()
	for rel, body := range files {
		writeFile(t, d, rel, body)
	}
	return d
}

func TestConfineAcceptsWhatAKitProjectUses(t *testing.T) {
	d := project(t, map[string]string{
		"index.html": `<!doctype html><html><head>
<link rel="stylesheet" href="css/tokens.css">
<script src="assets/gsap.min.js"></script>
<script src="https://cdn.jsdelivr.net/npm/gsap@3.14.2/dist/gsap.min.js"></script>
<link href="https://fonts.googleapis.com/css2?family=Inter" rel="stylesheet">
</head><body>
<a href="#top">top</a>
<img src="data:image/png;base64,AAAA">
<img srcset="assets/a.png 1x, assets/a@2x.png 2x" src="assets/a.png">
<div data-composition-src="compositions/intro.html" style="background:url('assets/bg.png')"></div>
<div style="background:url(&quot;assets/bg.png&quot;);background-image:image-set(&quot;assets/a.png&quot; 1x)"></div>
<style>.step::before{content:"Step 1: go"} .q{content:"\201C"} .i{background-image:image-set(url(assets/a.png) 1x, "assets/a@2x.png" 2x)}</style>
<script>const data = {a:1}; const tl = gsap.timeline(); tl.to(".x", {x: 10});</script>
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;700" rel="stylesheet">
<video src="assets/clip.mp4?v=1#t=2" poster="assets/poster.jpg"></video>
<svg><use xlink:href="assets/sprite.svg#icon"></use></svg>
</body></html>`,
		"css/tokens.css":          `@import "fonts.css"; .a{background:url(../assets/bg.png)} .b{background:url("data:image/svg+xml,<svg/>")}`,
		"css/fonts.css":           `@font-face{src:url("../assets/fonts/inter.woff2")}`,
		"compositions/intro.html": `<img src="../assets/logo.png">`,
	})
	if err := Confine(d, "index.html"); err != nil {
		t.Fatalf("a self-contained project must pass: %v", err)
	}
}

func TestConfineRefusesEveryWayOut(t *testing.T) {
	for name, body := range map[string]string{
		"parent path":            `<img src="../secret.png">`,
		"deep parent path":       `<img src="a/../../secret.png">`,
		"root-relative":          `<img src="/etc/passwd">`,
		"drive path":             `<img src="C:/Users/x/secret.png">`,
		"backslash parent":       `<img src="..\secret.png">`,
		"file URL":               `<img src="file:///etc/passwd">`,
		"percent-encoded parent": `<img src="%2e%2e/secret.png">`,
		"entity-encoded parent":  `<img src="&#46;&#46;/secret.png">`,
		"loopback":               `<script src="http://127.0.0.1:18791/v1/x"></script>`,
		"localhost":              `<script src="//localhost/x.js"></script>`,
		"private range":          `<img src="http://10.20.30.40/x.png">`,
		"tailnet range":          `<img src="http://` + net.IPv4(100, 105, 1, 2).String() + `/x.png">`,
		"tailnet name":           `<img src="https://box.example.ts.net/x.png">`,
		"dotless host":           `<img src="http://some-box:18811/fleet/media/x.png">`,
		"srcset candidate":       `<img srcset="ok.png 1x, ../secret.png 2x">`,
		"composition src":        `<div data-composition-src="../other/index.html"></div>`,
		"inline style url":       `<div style="background:url(../secret.png)"></div>`,
		"css escape in url":      `<div style="background:url(\2e\2e/secret.png)"></div>`,
		"base element":           `<base href="/"><img src="x.png">`,
		"base element, inner":    `<base href="sub/"><img src="x.png">`,
		"srcdoc":                 `<iframe srcdoc="&lt;img src=&quot;../x&quot;&gt;"></iframe>`,
		"legacy background":      `<body background="../secret.png">`,
		// A CSS hex escape swallows one following space, so these are single url tokens to a browser.
		"css escape and its space": `<div style="background:url(http\3a //127.0.0.1/x.png)"></div>`,
		"css dots and a space":     `<style>.a{background:url(\2e\2e /secret.png)}</style>`,
		"escaped quote in string":  `<style>.a{background:url("x\"y/../../../secret.png")}</style>`,
		// A style attribute is decoded before it is parsed as CSS: &quot; quotes the url there.
		"entity-quoted url":     `<div style="background:url(&quot;../secret.png&quot;)"></div>`,
		"image-set string":      `<style>.a{background-image:-webkit-image-set("../secret.png" 1x)}</style>`,
		"image-set in an attr":  `<div style="background-image:image-set(&quot;../secret.png&quot; 1x)"></div>`,
		"image-set after a url": `<style>.a{background-image:image-set(url(ok.png) 1x, "../secret.png" 2x)}</style>`,
		// HTML allows '/' between a tag name and an attribute (review H1a; the compiler copied these).
		"slash-separated src":  `<img/src="../../secret.txt">`,
		"slash-separated href": `<link/href="../x.css" rel="stylesheet">`,
		// The compiler resolves a reference that does not start with "../" from the project root (H1c).
		"climbs from the root": `<img src="x/../../secret.txt">`,
		// A UNC path to a Windows node's compiler, and a protocol-relative URL (L1).
		"unc path":          `<img src="\\public.example.com\share\x.png">`,
		"protocol-relative": `<script src="//cdn.example.com/x.js"></script>`,
		// Hosts a browser and Node read as loopback (M6).
		"short loopback":     `<script src="http://127.1:18791/x.js"></script>`,
		"hex loopback":       `<script src="http://0x7f.0.0.1:18791/x.js"></script>`,
		"octal loopback":     `<script src="http://0177.0.0.1/x.js"></script>`,
		"decimal loopback":   `<script src="http://2130706433/x.js"></script>`,
		"fullwidth loopback": `<script src="http://１２７.０.０.１/x.js"></script>`,
	} {
		d := project(t, map[string]string{"index.html": body})
		err := Confine(d, "index.html")
		if err == nil {
			t.Errorf("%s: Confine accepted %s", name, body)
			continue
		}
		if !strings.Contains(err.Error(), "index.html:1:") {
			t.Errorf("%s: the error must name file and line: %v", name, err)
		}
	}
	css := project(t, map[string]string{"index.html": `<link href="s.css" rel="stylesheet">`, "s.css": `@import "../../x.css";`})
	if err := Confine(css, "index.html"); err == nil || !strings.Contains(err.Error(), "s.css:1:") {
		t.Errorf("a CSS @import out of the project must be refused with its file: %v", err)
	}
	sheet := project(t, map[string]string{"index.html": `<link href="s.css" rel="stylesheet">`, "s.css": `.a{background:image-set("../x.png" 1x)} .b{background:url(\2e\2e /y.png)}`})
	if err := Confine(sheet, "index.html"); err == nil || !strings.Contains(err.Error(), `"../x.png"`) || !strings.Contains(err.Error(), `"../y.png"`) {
		t.Errorf("a stylesheet's image-set string and escaped url must both be refused: %v", err)
	}
	// The drive-path rule names itself; the scheme rule would refuse it too, for another reason.
	drive := project(t, map[string]string{"index.html": `<img src="C:/Users/x/secret.png">`})
	if err := Confine(drive, "index.html"); err == nil || !strings.Contains(err.Error(), "an absolute filesystem path") {
		t.Errorf("a drive path must be refused as an absolute filesystem path: %v", err)
	}
}

// The compiler reads a data-composition-src file as a composition whatever its name (review H1b), and a
// composition nested deeper resolves its non-"../" references from the project root (H1c).
func TestConfineReadsEveryComposition(t *testing.T) {
	d := project(t, map[string]string{
		"index.html": `<div data-composition-src="parts/evil.tpl"></div>`,
		"parts/evil.tpl": `<template><div data-composition-id="x">
<img src="../../secret.txt"></div></template>`,
	})
	if err := Confine(d, "index.html"); err == nil || !strings.Contains(err.Error(), "parts/evil.tpl:2:") {
		t.Errorf("a composition file with another extension must be read and refused: %v", err)
	}
	nested := project(t, map[string]string{
		"index.html":              `<div data-composition-src="a/b/inner.part"></div>`,
		"a/b/inner.part":          `<div data-composition-src="deeper.frag"></div>`,
		"a/b/deeper.frag":         `<img src="x/../../../secret.txt">`,
		"compositions/intro.html": `<img src="../assets/logo.png">`,
		"assets/logo.png":         "png",
	})
	if err := Confine(nested, "index.html"); err == nil || !strings.Contains(err.Error(), "deeper.frag:1:") {
		t.Errorf("a composition named by a composition must be read too: %v", err)
	}
	// An entry with another extension is read as the composition it is.
	entry := project(t, map[string]string{"main.part": `<img src="../outside.png">`})
	if err := Confine(entry, "main.part"); err == nil || !strings.Contains(err.Error(), "main.part:1:") {
		t.Errorf("the entry is read whatever its extension: %v", err)
	}
}

// Line numbers come from an index, not a recount per reference: a file full of references stays linear
// (review M4: 4 MB took 23 s with the recount).
func TestConfineIsLinearInTheFileSize(t *testing.T) {
	var b strings.Builder
	for b.Len() < 3<<20 {
		b.WriteString("<img src=\"../x.png\">\n")
	}
	d := project(t, map[string]string{"index.html": b.String()})
	start := time.Now()
	if err := Confine(d, "index.html"); err == nil {
		t.Fatal("every reference leaves the project")
	}
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("Confine took %v over a 3 MB file: the line lookup is not linear", took)
	}
	idx := newLineIndex("a\nbb\n\nc")
	for off, want := range map[int]int{0: 1, 1: 1, 2: 2, 4: 2, 5: 3, 6: 4} {
		if got := idx.line(off); got != want {
			t.Errorf("line(%d) = %d, want %d", off, got, want)
		}
	}
}

// Overlapping rules each name themselves: without a check of its own reason, a UNC path would pass for
// a root-relative one and a legacy loopback for an unreadable host.
func TestConfineNamesWhyAReferenceLeaves(t *testing.T) {
	for body, want := range map[string]string{
		`<img src="\\public.example.com\share\x.png">`:   "UNC or protocol-relative",
		`<script src="//cdn.example.com/x.js"></script>`: "UNC or protocol-relative",
		`<script src="http://127.1/x.js"></script>`:      "a non-public address",
		`<script src="http://１２７.０.０.１/x.js"></script>`:  "non-ASCII host",
		`<script src="http://1.2.3.4.5/x.js"></script>`:  "unreadable numeric host",
	} {
		d := project(t, map[string]string{"index.html": body})
		if err := Confine(d, "index.html"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want a refusal naming %q, got %v", body, want, err)
		}
	}
	// Inside from its own file, outside from the project root, where the compiler resolves it.
	nested := project(t, map[string]string{"index.html": "<p>x</p>", "a/b/c.html": `<img src="x/../../../secret.txt">`})
	if err := Confine(nested, "index.html"); err == nil || !strings.Contains(err.Error(), "from the project root") {
		t.Errorf("a nested file's climb from the root must be refused as such: %v", err)
	}
}

// A failure that is the bundle's own (a member under a name it wrote as a file) is refused as the
// bundle's; only a write the machine cannot make carries ErrIO, which a node answers as its own failure.
func TestExtractTellsTheBundlesFaultFromTheMachines(t *testing.T) {
	for _, ms := range [][]member{
		{{name: "a", body: "x"}, {name: "a/b.html", body: "y"}},
		{{name: "a", body: "x"}, {name: "a", typ: tar.TypeDir}},
	} {
		err := Extract(rawBundle(t, ms...), t.TempDir(), Limits{})
		if err == nil || errors.Is(err, ErrIO) || !strings.Contains(err.Error(), "the bundle wrote") {
			t.Errorf("%v: want the bundle's own conflict, not ErrIO: %v", ms, err)
		}
	}
	f, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := (ioWriter{f}).Write([]byte("x")); !errors.Is(err, ErrIO) {
		t.Errorf("a write the machine cannot make must carry ErrIO: %v", err)
	}
}

// The decompressed stream is bounded as a whole, so a header the caps on file bodies never count (a PAX
// record here) cannot run on: it stops at the stream bound, not at whatever the name rule says later.
func TestExtractBoundsTheStreamItself(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: strings.Repeat("a", 20000), Mode: 0o644, Size: 1, Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte("x"))
	tw.Close()
	gz.Close()
	err := Extract(buf.Bytes(), t.TempDir(), Limits{MaxFiles: 1, MaxEntries: 2, MaxTotal: 10})
	if err == nil || !strings.Contains(err.Error(), "unpacks to more than") {
		t.Fatalf("a 20 KB PAX record against a 12 KB stream bound must stop at the bound: %v", err)
	}
}

func TestConfineRefusesASymlink(t *testing.T) {
	d := project(t, map[string]string{"index.html": "<p>x</p>"})
	if err := os.Symlink(filepath.Join(d, "index.html"), filepath.Join(d, "link.html")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	if err := Confine(d, "index.html"); err == nil || !strings.Contains(err.Error(), "link.html: a symlink") {
		t.Fatalf("Confine must refuse a symlink in the project: %v", err)
	}
}

func TestConfineChecksTheComposition(t *testing.T) {
	d := project(t, map[string]string{"index.html": `<p>x</p>`})
	if err := Confine(d, "missing.html"); err == nil {
		t.Error("a missing composition accepted")
	}
	if err := Confine(d, "../index.html"); err == nil {
		t.Error("a composition outside the project accepted")
	}
	writeFile(t, d, "assets/a.png", "x")
	if err := Confine(d, "assets"); err == nil {
		t.Error("a directory accepted as the composition")
	}
	if err := Confine(d, ""); err != nil {
		t.Errorf("the default composition is index.html: %v", err)
	}
}

func TestNonPublicHost(t *testing.T) {
	for _, h := range []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.10", net.IPv4(100, 64, 0, 1).String(), net.IPv4(100, 127, 255, 254).String(), "169.254.1.1", "fd00::1", "localhost", "a.localhost", "x.ts.net", "nas.local", "box", "[::1]",
		// Legacy numeric forms a browser and Node resolve to loopback or a private host (review M6), a
		// fullwidth look-alike, an IPv4-mapped IPv6 loopback, and numeric hosts no browser accepts.
		"127.1", "0x7f.0.0.1", "0177.0.0.1", "2130706433", "0x7f000001", "10.1", "0xa.0x1.0x2.0x3", "１２７.０.０.１", "[::ffff:127.0.0.1]", "1.2.3.4.5", "999.1.1.1"} {
		if nonPublicHost(h) == "" {
			t.Errorf("%s must be non-public", h)
		}
	}
	for _, h := range []string{"cdn.jsdelivr.net", "fonts.googleapis.com", "1.1.1.1", net.IPv4(100, 128, 0, 1).String(), "example.com", "8.8.8.8", "0x8.0x8.0x8.0x8", "1.2.3.example.com"} {
		if why := nonPublicHost(h); why != "" {
			t.Errorf("%s must be public, got %q", h, why)
		}
	}
}

func TestPackRefusesASymlink(t *testing.T) {
	d := project(t, map[string]string{"index.html": "x"})
	if err := os.Symlink(filepath.Join(d, "index.html"), filepath.Join(d, "link.html")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	if _, err := Pack(d, Limits{}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Pack must refuse a symlink: %v", err)
	}
}

// A Windows junction or a FIFO is not a regular file: Pack refuses it rather than leaving it out (review
// L3: a junctioned assets/ vanished from the bundle with no error).
func TestPackRefusesAnIrregularFile(t *testing.T) {
	d := project(t, map[string]string{"index.html": "x"})
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(d, "assets"), t.TempDir())
	} else {
		cmd = exec.Command("mkfifo", filepath.Join(d, "pipe"))
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot create an irregular file here: %v: %s", err, out)
	}
	if _, err := Pack(d, Limits{}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Pack must refuse an irregular file, not skip it: %v", err)
	}
}

func TestPackHTML(t *testing.T) {
	b, err := PackHTML("<p>hi</p>")
	if err != nil {
		t.Fatal(err)
	}
	d := t.TempDir()
	if err := Extract(b, d, Limits{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(d, "index.html")); string(got) != "<p>hi</p>" {
		t.Fatalf("got %q", got)
	}
}
