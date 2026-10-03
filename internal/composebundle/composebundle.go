// Package composebundle carries a HyperFrames project between machines: Pack turns a project
// directory into a gzip-compressed tar, Extract unpacks one into a fresh directory, and Confine
// checks that the project only refers to files inside itself (ADR 0071).
//
// A project is trusted code: HyperFrames' Chrome runs without a sandbox and executes the project's
// JavaScript. The fleet's project door (fleetnode compose-project) accepts one only from a holder of
// the fleet token, and these three functions are the rest of its defence:
//
//   - Extract writes regular files and directories only (no symlink, hard link, device or FIFO), never
//     outside the target (no absolute path, drive letter, UNC path, `..`, backslash, NUL or colon in a
//     name), and within caps on the entries (directories included), the files, each file and the total,
//     counted on the bytes actually written, not on the headers, with the whole decompressed stream
//     bounded too.
//   - Confine (confine.go) reads every reference the project's HTML, SVG and CSS make, and those of any
//     file a composition attribute names whatever its extension (src, href, poster, srcset, data,
//     xlink:href, data-composition-src/-file, CSS url(), @import and image-set strings, in a raw-text
//     scan that does not depend on parsing HTML as the compiler's parser does), and refuses one that
//     points outside the project: a root-relative, absolute, UNC or protocol-relative path, a relative
//     path that climbs above the project root from its file or from the root (where HyperFrames'
//     compiler resolves one that does not start with "../"), a file: or other non-web URL, or an
//     http(s) URL to a host that is not public (loopback in any legacy numeric form, private, the
//     shared 100.64/10 range the tailnet uses, link-local, a .ts.net, .local or .internal name, a name
//     with no dot, or a non-ASCII name). HyperFrames' compiler copies an asset referenced outside the
//     project into its output and reads a composition file by any path; those are how a project could
//     read a file off the render node, and Confine is what closes them. JavaScript is not parsed: at
//     render time the page can reach only the project and its compiled copy (HyperFrames' file server
//     confines every path to them).
package composebundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	MaxEntries   int   // entries of any kind, directories included (default: twice MaxFiles)
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
	if l.MaxEntries <= 0 {
		l.MaxEntries = 2 * l.MaxFiles
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = DefaultMaxFileBytes
	}
	if l.MaxTotal <= 0 {
		l.MaxTotal = DefaultMaxTotal
	}
	return l
}

// ErrIO marks an Extract failure that is the target machine's own (a directory it could not create, a
// write that failed, a disk that filled), never the bundle's: a node answers it as its own failure, not
// as a refused bundle.
var ErrIO = errors.New("i/o")

// ioWriter tags a write failure with ErrIO, so io.Copy's error says which side failed.
type ioWriter struct{ f *os.File }

func (w ioWriter) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	if err != nil {
		err = fmt.Errorf("%w: %v", ErrIO, err)
	}
	return n, err
}

// capReader fails once more than max bytes have been read: the whole decompressed stream (headers and
// padding included) is bounded, not only the file bodies the caps count.
type capReader struct {
	r   io.Reader
	max int64
	n   int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.n >= c.max {
		return 0, fmt.Errorf("the bundle unpacks to more than %d bytes", c.max)
	}
	if rem := c.max - c.n; int64(len(p)) > rem {
		p = p[:rem]
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
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
			// A Windows junction, a socket or a device: refused like a symlink, never skipped, so
			// what is sent is what the sender sees.
			rel, _ := filepath.Rel(root, p)
			return fmt.Errorf("%s is not a regular file (a junction, socket or device): a project bundle carries regular files only", filepath.ToSlash(rel))
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
		return fmt.Errorf("%w: %v", ErrIO, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrIO, err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("extract target %s is not empty", root)
	}
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		return fmt.Errorf("the bundle is not gzip data: %w", err)
	}
	defer gz.Close()
	// The whole decompressed stream is bounded: the file bodies within MaxTotal, plus a header (with
	// any PAX record) and block padding for each entry.
	tr := tar.NewReader(&capReader{r: gz, max: lim.MaxTotal + int64(lim.MaxEntries+1)*4096})
	files, nentries := 0, 0
	var total int64
	// The names this bundle wrote as files: a later member under one of them, or a directory entry
	// with one's name, is the bundle's own conflict, refused as such before the filesystem reports it
	// as if it were this machine's failure (ErrIO).
	fileNames := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading the bundle: %w", err)
		}
		// Every entry counts, directories included: empty directory entries cost a gzip almost
		// nothing, and no other cap would bound them.
		nentries++
		if nentries > lim.MaxEntries {
			return fmt.Errorf("the bundle has more than %d entries", lim.MaxEntries)
		}
		if err := checkName(hdr.Name); err != nil {
			return err
		}
		name := path.Clean(hdr.Name)
		dest := filepath.Join(root, filepath.FromSlash(name))
		if !inside(root, dest) {
			return fmt.Errorf("member %q resolves outside the project", hdr.Name)
		}
		for p := path.Dir(name); p != "."; p = path.Dir(p) {
			if fileNames[p] {
				return fmt.Errorf("member %q is under %q, which the bundle wrote as a file", hdr.Name, p)
			}
		}
		if hdr.Typeflag == tar.TypeDir && fileNames[name] {
			return fmt.Errorf("member %q is a directory where the bundle wrote a file", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return fmt.Errorf("%w: member %q: %v", ErrIO, hdr.Name, err)
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
			return fmt.Errorf("%w: member %q: %v", ErrIO, hdr.Name, err)
		}
		f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("member %q: %w", hdr.Name, err) // a duplicate member: the bundle's fault
			}
			return fmt.Errorf("%w: member %q: %v", ErrIO, hdr.Name, err)
		}
		// The caps count the bytes written, never the header's claim.
		n, cerr := io.Copy(ioWriter{f}, io.LimitReader(tr, lim.MaxFileBytes+1))
		closeErr := f.Close()
		if cerr != nil {
			return fmt.Errorf("member %q: %w", hdr.Name, cerr)
		}
		if closeErr != nil {
			return fmt.Errorf("%w: member %q: %v", ErrIO, hdr.Name, closeErr)
		}
		if n > lim.MaxFileBytes {
			return fmt.Errorf("member %q is over the %d-byte limit for one file", hdr.Name, lim.MaxFileBytes)
		}
		fileNames[name] = true
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

// inside reports whether dest, the path a member would be written to, stays under root. checkName
// already refuses every name that could leave the project, so this zip-slip check on the joined path is
// the second line: it holds if the name rule is ever loosened.
func inside(root, dest string) bool {
	rel, err := filepath.Rel(root, dest)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
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
