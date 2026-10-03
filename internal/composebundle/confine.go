package composebundle

import (
	"fmt"
	"html"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	// attrRef finds a reference attribute anywhere in the text, inside or outside a tag, so a scan never
	// depends on parsing HTML the way the compiler's parser does. An attribute name follows whitespace,
	// a quote, '<' or '/' (HTML allows `<img/src=...>`).
	attrRef = regexp.MustCompile(`(?i)(?:^|[\s"'</])(src|href|poster|data|background|srcset|xlink:href|data-composition-src|data-composition-file)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)
	// CSS strings may hold escaped quotes, and a hex escape in an unquoted url swallows one following
	// space (`url(http\3a //host/x)` is one token to a browser), so both forms match escapes whole.
	cssURLRef = regexp.MustCompile(`(?i)url\(\s*(?:"((?:\\.|[^"\\])*)"|'((?:\\.|[^'\\])*)'|((?:\\[0-9a-fA-F]{1,6}\s?|\\[^0-9a-fA-F]|[^)\s\\"'])*))\s*\)`)
	importRef = regexp.MustCompile(`(?i)@import\s+(?:"((?:\\.|[^"\\])*)"|'((?:\\.|[^'\\])*)')`)
	// image-set() (and -webkit-image-set()) names an image by a bare string as well as by url(); the
	// argument list may hold one level of parentheses (a url() inside it).
	imageSetRef  = regexp.MustCompile(`(?i)image-set\(((?:[^()]|\([^()]*\))*)\)`)
	cssStringRef = regexp.MustCompile(`"((?:\\.|[^"\\])*)"|'((?:\\.|[^'\\])*)'`)
	// A <base> element re-roots every relative path in the document, and srcdoc embeds a second
	// document whose references this scan would not see: both are refused outright.
	baseElem   = regexp.MustCompile(`(?i)<base[\s/>]`)
	srcdocAttr = regexp.MustCompile(`(?i)(?:^|[\s"'</])srcdoc\s*=`)
	cssEscape  = regexp.MustCompile(`\\([0-9a-fA-F]{1,6})\s?|\\(.)`)
	// urlScheme is RFC 3986 scheme syntax: only a prefix of this shape before a colon is a scheme.
	urlScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)
)

// cssUnescape decodes CSS escapes (`\2e`, `\.`) the way a CSS parser does before resolving a URL.
func cssUnescape(s string) string {
	return cssEscape.ReplaceAllStringFunc(s, func(m string) string {
		sub := cssEscape.FindStringSubmatch(m)
		if sub[1] != "" {
			var r rune
			fmt.Sscanf(sub[1], "%x", &r)
			return string(r)
		}
		return sub[2]
	})
}

// htmlExts are scanned as HTML wherever they are; any other file is scanned as HTML when a
// composition names it (data-composition-src or -file), since HyperFrames reads that file as one.
var htmlExts = map[string]bool{".html": true, ".htm": true, ".xhtml": true, ".svg": true}

// Confine checks every reference the project's HTML, SVG and CSS make, and those of every file a
// composition attribute names whatever its extension, and returns one error listing each that points
// outside the project (file:line and why). entry is the composition the render starts from; it must be
// a regular file inside the project.
func Confine(dir, entry string) error {
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if entry == "" {
		entry = "index.html"
	}
	if err := checkName(filepath.ToSlash(entry)); err != nil {
		return fmt.Errorf("composition: %w", err)
	}
	if fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(entry))); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("the project has no composition file %q", entry)
	}
	var problems []string
	scannedHTML := map[string]bool{}
	queue := []string{path.Clean(filepath.ToSlash(entry))}
	scan := func(rel string, css bool) error {
		b, rerr := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if rerr != nil {
			return rerr
		}
		ps, comps := refsOutside(rel, string(b), css)
		problems = append(problems, ps...)
		queue = append(queue, comps...)
		return nil
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.Type()&fs.ModeSymlink != 0 {
			problems = append(problems, rel+": a symlink")
			return nil
		}
		switch ext := strings.ToLower(filepath.Ext(p)); {
		case ext == ".css":
			return scan(rel, true)
		case htmlExts[ext]:
			scannedHTML[rel] = true
			return scan(rel, false)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// The composition files: the entry, and every project file a composition attribute names, read as
	// HTML whatever the extension (the compiler reads them so), following the names they make in turn.
	for len(queue) > 0 {
		rel := queue[0]
		queue = queue[1:]
		if scannedHTML[rel] {
			continue
		}
		scannedHTML[rel] = true
		if fi, serr := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); serr != nil || !fi.Mode().IsRegular() {
			continue // a missing name is HyperFrames' own lint finding; an escaping one is already a problem
		}
		if err := scan(rel, false); err != nil {
			return err
		}
	}
	if len(problems) > 0 {
		if len(problems) > 20 {
			problems = append(problems[:20], fmt.Sprintf("… and %d more", len(problems)-20))
		}
		return fmt.Errorf("the project refers outside itself (a fleet render only reads files inside the project): %s", strings.Join(problems, "; "))
	}
	return nil
}

// lineIndex answers which line an offset is on: one pass over the text, then a binary search per
// question, so a file with many references costs linear time, not quadratic.
type lineIndex []int

func newLineIndex(s string) lineIndex {
	var idx lineIndex
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			idx = append(idx, i)
		}
	}
	return idx
}

func (l lineIndex) line(offset int) int { return sort.SearchInts(l, offset) + 1 }

// refsOutside returns "file:line: ref (why)" for each reference in text that leaves the project, and
// the project paths its composition attributes name (for Confine to read as compositions in turn).
func refsOutside(file, text string, cssOnly bool) (problems, comps []string) {
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			problems = append(problems, s)
		}
	}
	lines := newLineIndex(text)
	check := func(ref string, at func() int, srcset bool) {
		refs := []string{ref}
		if srcset {
			refs = nil
			for _, part := range strings.Split(ref, ",") {
				if f := strings.Fields(part); len(f) > 0 {
					refs = append(refs, f[0])
				}
			}
		}
		for _, r := range refs {
			if why := outside(file, r); why != "" {
				add(fmt.Sprintf("%s:%d: %q (%s)", file, at(), r, why))
			}
		}
	}
	if !cssOnly {
		for _, re := range []struct {
			re  *regexp.Regexp
			why string
		}{{baseElem, "a <base> element re-roots every relative path"}, {srcdocAttr, "an iframe srcdoc embeds a document this check cannot see"}} {
			for _, m := range re.re.FindAllStringIndex(text, -1) {
				add(fmt.Sprintf("%s:%d: %s", file, lines.line(m[0]), re.why))
			}
		}
		for _, m := range attrRef.FindAllStringSubmatchIndex(text, -1) {
			attr := strings.ToLower(text[m[2]:m[3]])
			v, ok := firstGroup(text, m, 4)
			if !ok {
				continue
			}
			// An attribute value is read after its character references are decoded (`&#46;&#46;/`
			// is `../`), so it is checked decoded.
			v = html.UnescapeString(v)
			off := m[0]
			check(v, func() int { return lines.line(off) }, attr == "srcset")
			if attr == "data-composition-src" || attr == "data-composition-file" {
				comps = append(comps, compositionPaths(file, v)...)
			}
		}
	}
	scanCSS := func(src string) {
		idx := lines
		if len(src) != len(text) || src != text {
			idx = newLineIndex(src)
		}
		for _, re := range []*regexp.Regexp{cssURLRef, importRef} {
			for _, m := range re.FindAllStringSubmatchIndex(src, -1) {
				if v, ok := firstGroup(src, m, 2); ok {
					off := m[0]
					check(cssUnescape(v), func() int { return idx.line(off) }, false)
				}
			}
		}
		for _, m := range imageSetRef.FindAllStringSubmatchIndex(src, -1) {
			args := src[m[2]:m[3]]
			off := m[0]
			for _, s := range cssStringRef.FindAllStringSubmatchIndex(args, -1) {
				if v, ok := firstGroup(args, s, 2); ok {
					check(cssUnescape(v), func() int { return idx.line(off) }, false)
				}
			}
		}
	}
	scanCSS(text)
	if !cssOnly {
		// A style attribute's value is decoded before it is parsed as CSS (`url(&quot;../x&quot;)` is
		// `url("../x")` there), while a <style> block is raw text: scanning both readings covers both.
		if dec := html.UnescapeString(text); dec != text {
			scanCSS(dec)
		}
	}
	return problems, comps
}

// firstGroup is the text of the first alternative that matched, from submatch group `from` on.
func firstGroup(src string, m []int, from int) (string, bool) {
	for g := from; g+1 < len(m); g += 2 {
		if m[g] >= 0 {
			return src[m[g]:m[g+1]], true
		}
	}
	return "", false
}

// compositionPaths is the project path a composition attribute names, as the compiler reads it (from
// the project root) and as a browser would (from the file); a name that is a URL or leaves the project
// names nothing to read (the latter is already a problem).
func compositionPaths(file, v string) []string {
	r := strings.ReplaceAll(strings.TrimSpace(v), `\`, "/")
	if i := strings.IndexAny(r, "?#"); i >= 0 {
		r = r[:i]
	}
	if r == "" || strings.Contains(r, ":") || strings.HasPrefix(r, "/") {
		return nil
	}
	if dec, err := url.PathUnescape(r); err == nil {
		r = dec
	}
	var out []string
	for _, p := range []string{path.Clean(r), path.Clean(path.Join(path.Dir(file), r))} {
		if !escapes(p) {
			out = append(out, p)
		}
	}
	return out
}

// escapes reports whether a cleaned slash path leaves the project root.
func escapes(p string) bool {
	return p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/")
}

// outside returns why ref leaves the project, or "" when it stays inside (or is not a fetch at all).
func outside(file, ref string) string {
	r := strings.TrimSpace(ref)
	if r == "" || strings.HasPrefix(r, "#") {
		return ""
	}
	// `\\host\share` is a UNC path to a Windows node's compiler, and `//host` a protocol-relative URL;
	// neither belongs in a composition (a web address is written with https://).
	if len(r) >= 2 && (r[0] == '/' || r[0] == '\\') && (r[1] == '/' || r[1] == '\\') {
		return "a UNC or protocol-relative reference; write https:// for a web address"
	}
	r = strings.ReplaceAll(r, `\`, "/") // browsers read a backslash as a slash in a web URL
	low := strings.ToLower(r)
	for _, p := range []string{"data:", "blob:", "javascript:", "about:", "mailto:", "tel:"} {
		if strings.HasPrefix(low, p) {
			return ""
		}
	}
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
		u, err := url.Parse(r)
		if err != nil || u.Hostname() == "" {
			return "an unreadable URL"
		}
		if why := nonPublicHost(u.Hostname()); why != "" {
			return why
		}
		return ""
	}
	if i := strings.Index(r, ":"); i > 0 && !strings.ContainsAny(r[:i], "/?#") && urlScheme.MatchString(r[:i]) {
		if i == 1 {
			return "an absolute filesystem path"
		}
		return "a " + strings.ToLower(r[:i]) + ": URL"
	}
	if strings.HasPrefix(r, "/") {
		return "a root-relative path; use a path relative to the file"
	}
	p := r
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if dec, err := url.PathUnescape(p); err == nil {
		p = dec
	}
	p = strings.ReplaceAll(p, `\`, "/")
	if escapes(path.Clean(path.Join(path.Dir(file), p))) {
		return "it resolves outside the project"
	}
	// HyperFrames' compiler resolves a reference that does not start with "../" against the project
	// root (only the leading-"../" ones are rewritten relative to their composition), so a path that
	// climbs from the root leaves the project too.
	if !strings.HasPrefix(p, "../") && p != ".." && escapes(path.Clean(p)) {
		return "it resolves outside the project from the project root, where the compiler resolves it"
	}
	return ""
}

// cgnat is the shared address range (100.64/10) a tailnet hands out.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

// nonPublicHost names why host is not a public internet host, or returns "".
func nonPublicHost(host string) string {
	h := strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	for i := 0; i < len(h); i++ {
		if h[i] >= 0x80 {
			// A browser maps fullwidth digits and other look-alikes to ASCII before resolving.
			return "a non-ASCII host name; write it in ASCII (punycode for an international name)"
		}
	}
	ip := net.ParseIP(h)
	if ip == nil {
		ip = legacyIPv4(h)
	}
	if ip == nil && endsInNumber(h) {
		// A browser parses such a host as an address and refuses it when that fails.
		return "an unreadable numeric host"
	}
	if ip != nil {
		switch {
		case ip.IsLoopback(), ip.IsPrivate(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsUnspecified(), ip.IsMulticast(), ip.IsInterfaceLocalMulticast():
			return "a non-public address"
		case cgnat.Contains(ip):
			return "an address in the shared 100.64/10 range (the tailnet uses it)"
		}
		return ""
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return "a loopback name"
	}
	for _, suf := range []string{".ts.net", ".local", ".internal", ".lan", ".home", ".corp", ".intranet", ".home.arpa"} {
		if strings.HasSuffix(h, suf) {
			return "a private name (" + suf + ")"
		}
	}
	if !strings.Contains(h, ".") {
		return "a dotless name (a LAN or tailnet host)"
	}
	return ""
}

// legacyIPv4 reads a host the way a browser's (and Node's) URL parser does when it is made of numbers:
// one to four dot-separated parts, each decimal, octal (a leading 0) or hex (0x), the last filling the
// remaining bytes. "127.1", "0x7f.0.0.1" and "0177.0.0.1" are all 127.0.0.1. It returns nil for a host
// that is not of that form.
func legacyIPv4(h string) net.IP {
	parts := strings.Split(h, ".")
	if len(parts) > 4 {
		return nil
	}
	nums := make([]uint64, len(parts))
	for i, p := range parts {
		n, ok := ipv4Number(p)
		if !ok {
			return nil
		}
		nums[i] = n
	}
	for _, n := range nums[:len(nums)-1] {
		if n > 255 {
			return nil
		}
	}
	last := nums[len(nums)-1]
	if last >= 1<<(8*uint(5-len(nums))) {
		return nil
	}
	v := last
	for i, n := range nums[:len(nums)-1] {
		v += n << (8 * uint(3-i))
	}
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// ipv4Number parses one part of a legacy IPv4 host: decimal, octal with a leading 0, or hex with 0x.
func ipv4Number(p string) (uint64, bool) {
	if p == "" {
		return 0, false
	}
	base := 10
	switch {
	case strings.HasPrefix(p, "0x") || strings.HasPrefix(p, "0X"):
		p, base = p[2:], 16
		if p == "" {
			return 0, true
		}
	case len(p) > 1 && p[0] == '0':
		p, base = p[1:], 8
	}
	n, err := strconv.ParseUint(p, base, 32)
	return n, err == nil
}

// endsInNumber is the URL parser's test for a host it must read as an address: the last label is all
// digits, or 0x followed by hex digits only.
func endsInNumber(h string) bool {
	parts := strings.Split(h, ".")
	last := parts[len(parts)-1]
	if last == "" {
		return false
	}
	if strings.HasPrefix(last, "0x") || strings.HasPrefix(last, "0X") {
		for _, c := range last[2:] {
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
		return true
	}
	for _, c := range last {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
