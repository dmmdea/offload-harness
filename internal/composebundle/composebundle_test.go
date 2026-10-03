package composebundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	for _, h := range []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.10", net.IPv4(100, 64, 0, 1).String(), net.IPv4(100, 127, 255, 254).String(), "169.254.1.1", "fd00::1", "localhost", "a.localhost", "x.ts.net", "nas.local", "box", "[::1]"} {
		if nonPublicHost(h) == "" {
			t.Errorf("%s must be non-public", h)
		}
	}
	for _, h := range []string{"cdn.jsdelivr.net", "fonts.googleapis.com", "1.1.1.1", net.IPv4(100, 128, 0, 1).String(), "example.com"} {
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
