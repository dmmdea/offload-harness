package gpulease

// The reader audit behind `gpu doctor` (plan P3, invariant I7).
//
// A reader that predates lease record v2 reads a directory holding only card-scoped
// leases as a FREE card. So before this host may write one, every reader of its lease
// directory has to understand the format. The reader audit finds the ones it can reach
// and says which of them carry the format's signature:
//
//   - binaries found BY NAME: the running one, the install directory (the backups the
//     node-swap rollback restores sit beside it), PATH entries and every scan root:
//     local-offload*, offload-harness* and local-agent* (the agent binary links this
//     package through modelaffinity and is deployed beside the harness);
//   - binaries found BY CONTENT under a scan root: any other executable of a few MiB or
//     more that carries the import path of a package that reads the lease (a wrapper
//     copy a media repository made under a name of its own);
//   - the image of every running harness process and of every lease holder or waiter;
//   - every Node `gpu-lock.mjs` under a scan root.
//
// VERSION CANNOT TELL: the build version is the same string before and after the format
// change inside one release, so the signature is what is looked for. It is a literal that
// every binary built from this tree which LINKS this package carries (an init function
// pins it, so the linker keeps it whether or not Audit is reachable; a test builds each
// such binary to prove it) and that render/gpu-lock.mjs exports; a pre-format file does
// not contain it. Found binaries are never executed.
//
// WHAT IT CANNOT REACH, and says so: a directory nobody scanned (the report lists the ones
// the walk did not enter: past the depth cap, node_modules, .git), a copy under another
// name that is neither under a scan root nor running as a lease holder, a Node reader not
// called gpu-lock.mjs, and a binary a packer has compressed.
//
// FAIL CLOSED. Finding no binary at all, a root, entry or file that cannot be read or
// examined, or a process table that cannot be listed is a reason, never a pass: the audit
// is only as good as what it reached, and the marker it writes switches the writer on.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// FormatSignature is the literal a format-aware binary or Node reader carries.
const FormatSignature = "gpu-lease-format-2/per-epoch-fence"

// formatSignatureKept is why every binary that links this package carries FormatSignature.
// A constant reaches a binary only through code the linker keeps; when only Audit referred
// to it, a binary that linked the lease package for the reading it does (the agent binary,
// through modelaffinity) carried no signature and a doctor run called it a pre-format
// reader. An init function is always kept, so this one pins the literal into every
// linking binary; TestEveryBinaryThatLinksTheLeaseReaderCarriesTheSignature builds each
// one to prove it.
var formatSignatureKept string

func init() { formatSignatureKept = FormatSignature }

// BuildMarkerPrefix precedes the compiled-in version in a binary that carries a build
// marker (the version itself is read back by the audit; see internal/buildinfo).
const BuildMarkerPrefix = "offload-build-version="

// Item kinds.
const (
	KindBinary     = "binary"
	KindNodeReader = "node-reader"
)

// ProcImage is the image of one running process worth auditing.
type ProcImage struct {
	PID  int
	Path string
	// ReadPath is what to open to read the image (on Linux /proc/<pid>/exe, which reads
	// a replaced or deleted binary too). Empty = Path.
	ReadPath string
	// Why is what made it interesting: "harness process", "lease holder", "waiter".
	Why string
}

// AuditOptions say where to look.
type AuditOptions struct {
	// Roots are directories searched for harness binaries and gpu-lock.mjs readers.
	Roots []string
	// SelfPath is the running executable (always audited).
	SelfPath string
	// Files are audited by name, without walking anything (PATH entries).
	Files []string
	// Images are running processes' images; ImagesErr is set when the process table
	// could not be listed.
	Images    []ProcImage
	ImagesErr error
	// MaxDepth bounds the directory walk below each root (default 3).
	MaxDepth int
}

// AuditItem is one file the audit judged.
type AuditItem struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	// Found is how it was reached: "self", "scan <root>", "running (why)"; PIDs lists the
	// running processes behind a "running" entry.
	Found []string `json:"found"`
	PIDs  []int    `json:"pids,omitempty"`
	Size  int64    `json:"size"`
	// Aware is true when the file carries FormatSignature.
	Aware bool `json:"aware"`
	// Version is the compiled-in build version a binary's marker states ("" = the
	// binary predates the marker).
	Version string `json:"version,omitempty"`
	Err     string `json:"error,omitempty"`
	// linksLease is set when the file carries a harness identity marker: it is a build of
	// this module that can read the lease directory (see harnessIdentityMarkers).
	linksLease bool
}

// AuditReport is the audit's whole answer.
type AuditReport struct {
	Items []AuditItem `json:"items"`
	// Reasons are why the audit is not green; empty when it is.
	Reasons []string `json:"reasons,omitempty"`
	// NotSearched lists the directories the walk did not enter. It is information, not a
	// finding: a gap that is listed can be closed (--scan, a deeper cap), a gap that is
	// not listed reads as a pass.
	NotSearched []string `json:"not_searched,omitempty"`
	Green       bool     `json:"green"`
}

// Audit scans the options and judges every file it reaches.
func Audit(opts AuditOptions) AuditReport {
	depth := opts.MaxDepth
	if depth <= 0 {
		depth = DefaultAuditDepth
	}
	items := map[string]*AuditItem{}
	var reasons, notSearched []string
	key := func(p string) string {
		p = filepath.Clean(p)
		if runtime.GOOS == "windows" {
			p = strings.ToLower(p)
		}
		return p
	}
	add := func(path, readPath, kind, found string, pid int) {
		k := key(path)
		it := items[k]
		if it == nil {
			it = &AuditItem{Path: path, Kind: kind}
			items[k] = it
			scanInto(it, readPath)
		}
		it.Found = appendUnique(it.Found, found)
		if pid > 0 {
			it.PIDs = appendUniqueInt(it.PIDs, pid)
		}
	}

	if opts.SelfPath != "" {
		add(opts.SelfPath, opts.SelfPath, KindBinary, "self", 0)
	}
	for _, root := range opts.Roots {
		fi, err := os.Stat(root)
		if err != nil || !fi.IsDir() {
			if err == nil {
				err = fmt.Errorf("not a directory")
			}
			reasons = append(reasons, fmt.Sprintf("scan root %s could not be read: %v", root, err))
			continue
		}
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				reasons = append(reasons, fmt.Sprintf("could not read %s: %v", p, werr))
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				// What is not entered is LISTED, so a gap can be closed (--scan, --depth)
				// instead of reading as a pass.
				if skipDir(d.Name()) && p != root {
					notSearched = appendUnique(notSearched, fmt.Sprintf("%s (a %s directory is never searched)", p, d.Name()))
					return fs.SkipDir
				}
				if dirLevels(root, p) > depth {
					notSearched = appendUnique(notSearched, fmt.Sprintf("%s (more than %d levels below the scan root)", p, depth))
					return fs.SkipDir
				}
				return nil
			}
			info, ierr := statEntry(d)
			if ierr != nil {
				// FAIL CLOSED: an entry that cannot be examined may be a harness binary.
				reasons = append(reasons, fmt.Sprintf("%s could not be examined: %v", p, ierr))
				return nil
			}
			switch {
			case strings.EqualFold(d.Name(), "gpu-lock.mjs"):
				add(p, p, KindNodeReader, "scan "+root, 0)
			case IsHarnessBinaryName(d.Name(), info.Size()):
				add(p, p, KindBinary, "scan "+root, 0)
			case sniffable(d.Name(), info):
				// A harness binary under a name of its own (a wrapper copy made for a media
				// repository): found by what it carries, not what it is called.
				if known := items[key(p)]; known != nil {
					known.Found = appendUnique(known.Found, "scan "+root)
					return nil
				}
				probe := &AuditItem{Path: p, Kind: KindBinary}
				scanInto(probe, p)
				switch {
				case probe.Err != "":
					reasons = append(reasons, fmt.Sprintf("%s could not be read to tell whether it is a harness binary: %s", p, probe.Err))
				case probe.linksLease:
					probe.Found = []string{"scan " + root}
					items[key(p)] = probe
				}
			}
			return nil
		})
	}
	for _, f := range opts.Files {
		kind := KindBinary
		if strings.EqualFold(filepath.Base(f), "gpu-lock.mjs") {
			kind = KindNodeReader
		}
		add(f, f, kind, "named file", 0)
	}
	for _, im := range opts.Images {
		read := im.ReadPath
		if read == "" {
			read = im.Path
		}
		if im.Path == "" {
			reasons = append(reasons, fmt.Sprintf("running pid %d: its image path could not be read", im.PID))
			continue
		}
		add(im.Path, read, KindBinary, "running ("+im.Why+")", im.PID)
	}
	if opts.ImagesErr != nil {
		reasons = append(reasons, fmt.Sprintf("the process table could not be listed (%v), so running images were not audited", opts.ImagesErr))
	}

	rep := AuditReport{}
	for _, it := range items {
		rep.Items = append(rep.Items, *it)
	}
	sort.Slice(rep.Items, func(i, j int) bool {
		if rep.Items[i].Kind != rep.Items[j].Kind {
			return rep.Items[i].Kind < rep.Items[j].Kind
		}
		return rep.Items[i].Path < rep.Items[j].Path
	})
	binaries := 0
	for _, it := range rep.Items {
		if it.Kind == KindBinary {
			binaries++
		}
		switch {
		case it.Err != "":
			reasons = append(reasons, fmt.Sprintf("%s could not be scanned: %s", it.Path, it.Err))
		case !it.Aware:
			what := "binary"
			if it.Kind == KindNodeReader {
				what = "Node reader"
			}
			reasons = append(reasons, fmt.Sprintf("%s %s predates the per-epoch fence (no format signature)", what, it.Path))
		}
	}
	if binaries == 0 {
		reasons = append(reasons, "no harness binary was found: an audit that reached nothing proves nothing")
	}
	rep.Reasons = reasons
	sort.Strings(notSearched)
	rep.NotSearched = notSearched
	rep.Green = len(reasons) == 0
	return rep
}

// DefaultAuditDepth is how many directory levels below a scan root the walk enters.
const DefaultAuditDepth = 3

// dirLevels is how many directory levels p is below root (0 for the root itself).
func dirLevels(root, p string) int {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." {
		return 0
	}
	return strings.Count(rel, string(filepath.Separator)) + 1
}

// statEntry reads a directory entry's info. A variable so a test can make one fail.
var statEntry = func(d fs.DirEntry) (fs.FileInfo, error) { return d.Info() }

func skipDir(name string) bool { return name == "node_modules" || name == ".git" }

// IsHarnessBinaryName reports whether a file name is one of the harness binary's names:
// local-offload*, offload-harness* or local-agent* (the agent binary links the lease
// package through modelaffinity and is deployed beside the harness), as an executable, a
// node-swap backup (`<exe>.bak-<suffix>`) or an extensionless Unix binary, and big enough
// to be one. A copy under any OTHER name is found by content (sniffable, below).
func IsHarnessBinaryName(name string, size int64) bool {
	l := strings.ToLower(name)
	if !strings.HasPrefix(l, "local-offload") && !strings.HasPrefix(l, "offload-harness") && !strings.HasPrefix(l, "local-agent") {
		return false
	}
	if size < 1024 {
		return false
	}
	return strings.HasSuffix(l, ".exe") || strings.Contains(l, ".bak-") || !strings.Contains(l, ".")
}

// sniffMinSize is the smallest executable read to look for a harness identity marker: a Go
// binary that links the lease reader is many MiB, and the launchers and stubs below this are
// not worth a read apiece.
const sniffMinSize = 1 << 20

// harnessIdentityMarkers are import paths of the packages that read the lease directory.
// Every harness binary that can read the lease carries one in its symbol tables (function
// names embed the import path), whatever the file is called and whether or not it
// predates the format signature, which is what lets the audit tell "a harness binary that
// predates the fence" from "some other executable". The module was renamed on 2026-07-04
// and the lease package arrived on 2026-07-25, so no binary that links gpulease carries
// an older module path.
var harnessIdentityMarkers = []string{
	"github.com/dmmdea/offload-harness/internal/gpulease",
	"github.com/dmmdea/offload-harness/internal/gpulock",
}

// sniffable reports whether an executable under a scan root with no harness name is big
// enough, and executable-shaped enough, to be read for an identity marker.
func sniffable(name string, info fs.FileInfo) bool {
	if info.Size() < sniffMinSize {
		return false
	}
	l := strings.ToLower(name)
	if strings.HasSuffix(l, ".exe") || strings.Contains(l, ".exe.bak-") {
		return true
	}
	return runtime.GOOS != "windows" && info.Mode()&0o111 != 0 && !strings.Contains(l, ".")
}

// openAudited opens a file for the audit. A variable so a test can make one unreadable on
// any platform.
var openAudited = func(path string) (*os.File, error) { return os.Open(path) }

var buildMarkerRE = regexp.MustCompile(regexp.QuoteMeta(BuildMarkerPrefix) + `(\d+\.\d+\.\d+[0-9A-Za-z.+-]*)`)

// scanInto reads the file in chunks, looking for the signature, a harness identity marker
// and the build marker.
func scanInto(it *AuditItem, path string) {
	f, err := openAudited(path)
	if err != nil {
		it.Err = err.Error()
		return
	}
	defer f.Close()
	if fi, serr := f.Stat(); serr == nil {
		it.Size = fi.Size()
	}
	sig := []byte(FormatSignature)
	longest := len(sig)
	for _, m := range harnessIdentityMarkers {
		if len(m) > longest {
			longest = len(m)
		}
	}
	const chunk = 1 << 20
	overlap := longest + 96
	buf := make([]byte, chunk+overlap)
	carry := 0
	for {
		n, rerr := f.Read(buf[carry : carry+chunk])
		data := buf[:carry+n]
		if bytes.Contains(data, sig) {
			it.Aware = true
		}
		if !it.linksLease {
			for _, m := range harnessIdentityMarkers {
				if bytes.Contains(data, []byte(m)) {
					it.linksLease = true
					break
				}
			}
		}
		if it.Version == "" {
			if m := buildMarkerRE.FindSubmatch(data); m != nil {
				it.Version = string(m[1])
			}
		}
		if rerr == io.EOF {
			return
		}
		if rerr != nil {
			it.Err = rerr.Error()
			return
		}
		if len(data) > overlap {
			copy(buf, data[len(data)-overlap:])
			carry = overlap
		} else {
			carry = len(data)
		}
	}
}

func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

func appendUniqueInt(list []int, n int) []int {
	for _, x := range list {
		if x == n {
			return list
		}
	}
	return append(list, n)
}

// ---------------------------------------------------------------------------
// The marker
// ---------------------------------------------------------------------------

// WriteReaderAudit records the audit's verdict as the reader-audit marker that
// ApplyCardScopedConfig reads. A report that is not green writes a RED marker: a stale
// green left from an earlier audit must not survive a newer binary appearing.
func (m *Manager) WriteReaderAudit(r AuditReport) error {
	result := "red"
	if r.Green {
		result = "green"
	}
	binaries, readers := 0, 0
	for _, it := range r.Items {
		if it.Kind == KindNodeReader {
			readers++
		} else {
			binaries++
		}
	}
	b, err := json.MarshalIndent(map[string]any{
		"result":       result,
		"checked_at":   m.now().UTC().Format(time.RFC3339),
		"signature":    FormatSignature,
		"binaries":     binaries,
		"node_readers": readers,
		"reasons":      r.Reasons,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.gpuDir(), 0o777); err != nil {
		return err
	}
	tmp := m.readerAuditPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o666); err != nil {
		return fmt.Errorf("gpulease: writing the reader audit marker: %w", err)
	}
	if err := renameReplacing(tmp, m.readerAuditPath()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("gpulease: publishing the reader audit marker: %w", err)
	}
	return nil
}

// ReaderAuditPath is where the marker lives (for the doctor's output).
func (m *Manager) ReaderAuditPath() string { return m.readerAuditPath() }

// ReaderAuditResult is the marker's verdict for display: "green", "red", "absent" or
// "unreadable".
func (m *Manager) ReaderAuditResult() string {
	b, err := os.ReadFile(m.readerAuditPath())
	if os.IsNotExist(err) {
		return "absent"
	}
	if err != nil {
		return "unreadable"
	}
	var a struct {
		Result string `json:"result"`
	}
	if json.Unmarshal(b, &a) != nil || (a.Result != "green" && a.Result != "red") {
		return "unreadable"
	}
	return a.Result
}
