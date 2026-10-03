// Package composebundle carries a HyperFrames project between machines: Pack turns a project
// directory into a gzip-compressed tar, Extract unpacks one into a fresh directory, and Confine
// checks that the project only refers to files inside itself (ADR 0070).
//
// A project is trusted code: HyperFrames' Chrome runs without a sandbox and executes the project's
// JavaScript. The fleet's project door (fleetnode compose-project) accepts one only from a holder of
// the fleet token, and these three functions are the rest of its defence:
//
//   - Extract writes regular files and directories only (no symlink, hard link, device or FIFO), never
//     outside the target (no absolute path, drive letter, UNC path, `..`, backslash, NUL or colon in a
//     name), and within caps on the file count, each file and the total, counted on the bytes actually
//     written, not on the headers.
//   - Confine reads every reference an .html/.htm/.svg/.css file makes (src, href, poster, srcset,
//     data, xlink:href, data-composition-src/-file, CSS url() and @import) and refuses one that points
//     outside the project: a root-relative or absolute path, a relative path that resolves above the
//     project root, a file: or other non-web URL, or an http(s) URL to a host that is not public
//     (loopback, private, the shared 100.64/10 range the tailnet uses, link-local, a .ts.net, .local or
//     .internal name, or a name with no dot). HyperFrames' compiler copies an asset referenced outside
//     the project into its output; that copy is how a project could read a file off the render node,
//     and Confine is what closes it. JavaScript is not parsed: at render time the page can reach only
//     the project and its compiled copy (HyperFrames' file server confines every path to them).
package composebundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Limits bound a bundle. The zero value means the defaults below.
type Limits struct {
	MaxFiles     int   // files in the bundle
	MaxFileBytes int64 // one file, uncompressed
	MaxTotal     int64 // all files, uncompressed
}

const (
	DefaultMaxFiles     = 4096
	DefaultMaxFileBytes = 256 << 20
	DefaultMaxTotal     = 512 << 20
)

func (l Limits) withDefaults() Limits {
	if l.MaxFiles <= 0 {
		l.MaxFiles = DefaultMaxFiles
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = DefaultMaxFileBytes
	}
	if l.MaxTotal <= 0 {
		l.MaxTotal = DefaultMaxTotal
	}
	return l
}

// skipDirs are never packed: version control, dependency trees and a kit project's own renders.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "renders": true, ".hyperframes": true}

// Pack writes the project under dir as a gzip-compressed tar: regular files only, with forward-slash
// names relative to dir, in sorted order. Symlinks are refused rather than followed or skipped, so
// what is sent is exactly what the sender sees.
func Pack(dir string, lim Limits) ([]byte, error) {
	lim = lim.withDefaults()
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	var total int64
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if p == root {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			rel, _ := filepath.Rel(root, p)
			return fmt.Errorf("%s is a symlink: a project bundle carries regular files only", filepath.ToSlash(rel))
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if info.Size() > lim.MaxFileBytes {
			rel, _ := filepath.Rel(root, p)
			return fmt.Errorf("%s is %d bytes, over the %d-byte limit for one file", filepath.ToSlash(rel), info.Size(), lim.MaxFileBytes)
		}
		total += info.Size()
		if total > lim.MaxTotal {
			return fmt.Errorf("the project is over the %d-byte limit", lim.MaxTotal)
		}
		files = append(files, p)
		if len(files) > lim.MaxFiles {
			return fmt.Errorf("the project has more than %d files", lim.MaxFiles)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, p := range files {
		rel, _ := filepath.Rel(root, p)
		name := filepath.ToSlash(rel)
		if err := checkName(name); err != nil {
			return nil, err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(b); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// PackHTML is the bundle for a single-file composition: one index.html.
func PackHTML(html string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "index.html", Mode: 0o644, Size: int64(len(html)), Typeflag: tar.TypeReg}); err != nil {
		return nil, err
	}
	if _, err := io.WriteString(tw, html); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var windowsReserved = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[0-9]|lpt[0-9])(\..*)?$`)

// checkName refuses a bundle member name that could land outside the target or be read two ways.
func checkName(name string) error {
	switch {
	case name == "" || name == ".":
		return errors.New("an empty member name")
	case strings.ContainsAny(name, "\\:\x00"):
		return fmt.Errorf("member %q: backslashes, colons and NUL are refused in a name", name)
	case strings.HasPrefix(name, "/"):
		return fmt.Errorf("member %q is an absolute path", name)
	}
	clean := path.Clean(name)
	if clean != strings.TrimSuffix(name, "/") {
		return fmt.Errorf("member %q is not a clean relative path", name)
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("member %q climbs out of the project", name)
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == ".." || seg == "." || seg == "" || windowsReserved.MatchString(seg) || strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
			return fmt.Errorf("member %q has a refused path segment %q", name, seg)
		}
	}
	return nil
}

// Extract unpacks a bundle into dir, which must exist and be empty.
func Extract(bundle []byte, dir string, lim Limits) error {
	lim = lim.withDefaults()
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("extract target %s is not empty", root)
	}
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		return fmt.Errorf("the bundle is not gzip data: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := 0
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading the bundle: %w", err)
		}
		if err := checkName(hdr.Name); err != nil {
			return err
		}
		name := path.Clean(hdr.Name)
		dest := filepath.Join(root, filepath.FromSlash(name))
		if rel, rerr := filepath.Rel(root, dest); rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return fmt.Errorf("member %q resolves outside the project", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
			continue
		case tar.TypeReg, tar.TypeRegA:
		default:
			return fmt.Errorf("member %q is a %s: a project bundle carries regular files and directories only", hdr.Name, typeName(hdr.Typeflag))
		}
		files++
		if files > lim.MaxFiles {
			return fmt.Errorf("the bundle has more than %d files", lim.MaxFiles)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return fmt.Errorf("member %q: %w", hdr.Name, err)
		}
		// The caps count the bytes written, never the header's claim.
		n, cerr := io.Copy(f, io.LimitReader(tr, lim.MaxFileBytes+1))
		f.Close()
		if cerr != nil {
			return fmt.Errorf("member %q: %w", hdr.Name, cerr)
		}
		if n > lim.MaxFileBytes {
			return fmt.Errorf("member %q is over the %d-byte limit for one file", hdr.Name, lim.MaxFileBytes)
		}
		total += n
		if total > lim.MaxTotal {
			return fmt.Errorf("the bundle unpacks to more than %d bytes", lim.MaxTotal)
		}
	}
	if files == 0 {
		return errors.New("the bundle holds no files")
	}
	return nil
}

func typeName(t byte) string {
	switch t {
	case tar.TypeSymlink:
		return "symlink"
	case tar.TypeLink:
		return "hard link"
	case tar.TypeChar, tar.TypeBlock:
		return "device"
	case tar.TypeFifo:
		return "FIFO"
	}
	return fmt.Sprintf("type %q entry", t)
}

var (
	attrRef   = regexp.MustCompile(`(?i)(?:^|[\s"'<])(src|href|poster|data|background|srcset|xlink:href|data-composition-src|data-composition-file)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)
	cssURLRef = regexp.MustCompile(`(?i)url\(\s*(?:"([^"]*)"|'([^']*)'|([^)\s]*))\s*\)`)
	importRef = regexp.MustCompile(`(?i)@import\s+(?:"([^"]*)"|'([^']*)')`)
	// A <base> element re-roots every relative path in the document, and srcdoc embeds a second
	// document whose references this scan would not see: both are refused outright.
	baseElem   = regexp.MustCompile(`(?i)<base[\s/>]`)
	srcdocAttr = regexp.MustCompile(`(?i)(?:^|[\s"'<])srcdoc\s*=`)
	cssEscape  = regexp.MustCompile(`\\([0-9a-fA-F]{1,6})\s?|\\(.)`)
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

// Confine checks every reference the project's .html/.htm/.svg/.css files make and returns one error
// listing each that points outside the project (file:line and why). entry is the composition the
// render starts from; it must be a regular file inside the project.
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
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			rel, _ := filepath.Rel(root, p)
			problems = append(problems, filepath.ToSlash(rel)+": a symlink")
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if ext != ".html" && ext != ".htm" && ext != ".svg" && ext != ".css" && ext != ".xhtml" {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, p)
		problems = append(problems, refsOutside(filepath.ToSlash(rel), string(b), ext == ".css")...)
		return nil
	})
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		if len(problems) > 20 {
			problems = append(problems[:20], fmt.Sprintf("… and %d more", len(problems)-20))
		}
		return fmt.Errorf("the project refers outside itself (a fleet render only reads files inside the project): %s", strings.Join(problems, "; "))
	}
	return nil
}

// refsOutside returns "file:line: ref (why)" for each reference in text that leaves the project.
func refsOutside(file, text string, cssOnly bool) []string {
	var out []string
	lineOf := func(offset int) int { return strings.Count(text[:offset], "\n") + 1 }
	if !cssOnly {
		for _, re := range []struct {
			re  *regexp.Regexp
			why string
		}{{baseElem, "a <base> element re-roots every relative path"}, {srcdocAttr, "an iframe srcdoc embeds a document this check cannot see"}} {
			for _, m := range re.re.FindAllStringIndex(text, -1) {
				out = append(out, fmt.Sprintf("%s:%d: %s", file, lineOf(m[0]), re.why))
			}
		}
	}
	check := func(ref string, offset int, srcset bool) {
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
				out = append(out, fmt.Sprintf("%s:%d: %q (%s)", file, lineOf(offset), r, why))
			}
		}
	}
	if !cssOnly {
		for _, m := range attrRef.FindAllStringSubmatchIndex(text, -1) {
			attr := strings.ToLower(text[m[2]:m[3]])
			for g := 4; g <= 8; g += 2 {
				if m[g] >= 0 {
					// An attribute value is read after its character references are decoded
					// (`&#46;&#46;/` is `../`), so it is checked decoded too.
					check(html.UnescapeString(text[m[g]:m[g+1]]), m[0], attr == "srcset")
					break
				}
			}
		}
	}
	for _, re := range []*regexp.Regexp{cssURLRef, importRef} {
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			for g := 2; g < len(m); g += 2 {
				if m[g] >= 0 {
					v := cssUnescape(text[m[g]:m[g+1]])
					if !cssOnly {
						v = html.UnescapeString(v) // inside an HTML style attribute or block
					}
					check(v, m[0], false)
					break
				}
			}
		}
	}
	return out
}

// outside returns why ref leaves the project, or "" when it stays inside (or is not a fetch at all).
func outside(file, ref string) string {
	r := strings.TrimSpace(ref)
	if r == "" || strings.HasPrefix(r, "#") {
		return ""
	}
	r = strings.ReplaceAll(r, `\`, "/") // browsers read a backslash as a slash in a web URL
	low := strings.ToLower(r)
	for _, p := range []string{"data:", "blob:", "javascript:", "about:", "mailto:", "tel:"} {
		if strings.HasPrefix(low, p) {
			return ""
		}
	}
	if strings.HasPrefix(r, "//") {
		r = "https:" + r
		low = strings.ToLower(r)
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
	if i := strings.Index(r, ":"); i > 0 && !strings.ContainsAny(r[:i], "/?#") {
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
	resolved := path.Clean(path.Join(path.Dir(file), p))
	if resolved == ".." || strings.HasPrefix(resolved, "../") || strings.HasPrefix(resolved, "/") {
		return "it resolves outside the project"
	}
	return ""
}

// cgnat is the shared address range (100.64/10) a tailnet hands out.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

// nonPublicHost names why host is not a public internet host, or returns "".
func nonPublicHost(host string) string {
	h := strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if ip := net.ParseIP(h); ip != nil {
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
