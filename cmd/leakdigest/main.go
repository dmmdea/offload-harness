// Command leakdigest builds and checks the digest file of the leak gate and is
// the local scanner behind it.
//
// The repository is public; the list of names it must never hold is a secret.
// The committed form of that list is a file of keyed hashes, so this command
// is run by the operator with the plaintext list and the key, both of which
// live outside the repository:
//
//	leakdigest genkey [-out FILE]
//	leakdigest gen     -plain LIST -out DIGEST [-key-file FILE] [-exempt FILE] [-dir DIR]
//	leakdigest check   -plain LIST [-digest DIGEST] [-key-file FILE] [-exempt FILE] [-dir DIR]
//	leakdigest scan    -plain LIST [-dir DIR | -ref REV [-repo DIR]] [-files FILE] [-digest DIGEST]
//	leakdigest patterns -plain LIST [-into FILE]
//
// scan is the per-chunk acceptance tool and the parity check: it runs the same
// matcher, fold, fail-closed rules and shape rules as the gate, over a work
// tree (-dir) or the blobs of a commit (-ref), and prints plaintext names, so it
// is local only and never run in CI. The library behind it is
// internal/leakgate.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dmmdea/offload-harness/internal/leakgate"
)

const usage = `usage:
  leakdigest genkey   [-out FILE] [-force]
  leakdigest gen      -plain LIST -out DIGEST [-key-file FILE] [-exempt FILE] [-dir DIR]
  leakdigest check    -plain LIST [-digest DIGEST] [-key-file FILE] [-exempt FILE] [-dir DIR]
  leakdigest scan     -plain LIST [-dir DIR | -ref REV [-repo DIR]] [-files FILE] [-digest DIGEST]
  leakdigest patterns -plain LIST [-into FILE]

The plaintext list is one entry per line, mode<TAB>text; '#' starts a comment.
The key is 64 hex characters: -key-file, else OFFLOAD_LEAK_GATE_KEY, else
OFFLOAD_LEAK_GATE_KEY_FILE, else the default key file in the user config
directory. The exempt file is one row per line, reason<TAB>path (reasons: binary,
oversize, longrun, symlink, submodule); the blob id is read from the git index of
-dir (check needs the same -exempt file as gen). scan exits 0 only with no
findings and no fatal records. genkey -out writes the file with mode 0600; on
Windows the file inherits its folder's access list, so keep it under the user
profile and never on a synced drive.
`

const defaultDigest = "testdata/leak-gate-digests.json"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

// run is main with its streams and environment passed in. Exit codes: 0 clean,
// 1 findings or a stale digest file, 2 a usage or input error.
func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	code := 0
	switch args[0] {
	case "genkey":
		err = cmdGenkey(args[1:], stdout)
	case "gen":
		err = cmdGen(args[1:], stdout, getenv)
	case "check":
		code, err = cmdCheck(args[1:], stdout, getenv)
	case "scan":
		code, err = cmdScan(args[1:], stdout)
	case "patterns":
		err = cmdPatterns(args[1:], stdout)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "leakdigest: unknown command %q\n%s", args[0], usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "leakdigest:", err)
		return 2
	}
	return code
}

func newFlags(name string) *flag.FlagSet {
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	return fl
}

func parseFlags(fl *flag.FlagSet, args []string) error {
	if err := fl.Parse(args); err != nil {
		return fmt.Errorf("%s: %v", fl.Name(), err)
	}
	if fl.NArg() != 0 {
		return fmt.Errorf("%s: unexpected argument %q", fl.Name(), fl.Arg(0))
	}
	return nil
}

// ---- genkey

// userConfigDir is os.UserConfigDir, replaceable in tests so that a developer's
// real key file is never picked up.
var userConfigDir = os.UserConfigDir

func defaultKeyFile() string {
	dir, err := userConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "offload-harness", "leak-gate.key")
}

func cmdGenkey(args []string, stdout io.Writer) error {
	fl := newFlags("genkey")
	out := fl.String("out", "", "write the key to this file (mode 0600) instead of printing it")
	force := fl.Bool("force", false, "overwrite an existing key file")
	if err := parseFlags(fl, args); err != nil {
		return err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("genkey: %v", err)
	}
	key := hex.EncodeToString(raw) + "\n"
	if *out == "" {
		_, err := io.WriteString(stdout, key)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o700); err != nil {
		return fmt.Errorf("genkey: %v", err)
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if *force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(*out, flags, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return errors.New("genkey: the key file exists (a key is never overwritten silently; pass -force)")
		}
		return fmt.Errorf("genkey: %v", err)
	}
	if _, err := io.WriteString(f, key); err != nil {
		f.Close()
		return fmt.Errorf("genkey: %v", err)
	}
	return f.Close()
}

// ---- gen and check

type deriveFlags struct {
	plain, keyFile, exempt, dir string
}

func (d *deriveFlags) register(fl *flag.FlagSet) {
	fl.StringVar(&d.plain, "plain", "", "the plaintext list")
	fl.StringVar(&d.keyFile, "key-file", "", "a key file (wins over the environment)")
	fl.StringVar(&d.exempt, "exempt", "", "exempt rows: reason<TAB>path per line")
	fl.StringVar(&d.dir, "dir", ".", "the work tree whose git index gives the exempt blob ids")
}

func readList(path string) ([]leakgate.Entry, error) {
	if path == "" {
		return nil, errors.New("-plain is required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the plaintext list: %v", err)
	}
	es, err := leakgate.ParseList(b)
	if err != nil {
		return nil, fmt.Errorf("the plaintext list: %v", err)
	}
	return es, nil
}

func resolveKey(keyFile string, getenv func(string) string) ([]byte, error) {
	if keyFile != "" {
		raw, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, errors.New("the key file cannot be read")
		}
		key, err := leakgate.DecodeKey(strings.TrimSpace(string(raw)))
		if err != nil {
			return nil, errors.New("the key file is malformed")
		}
		return key, nil
	}
	keys, err := leakgate.ResolveKey(getenv, os.ReadFile, defaultKeyFile())
	if err != nil {
		return nil, err
	}
	if keys.Key == nil {
		return nil, errors.New("no gate key: pass -key-file, set " + leakgate.EnvKey + " or " + leakgate.EnvKeyFile + ", or run genkey -out")
	}
	return keys.Key, nil
}

// readExempt reads reason<TAB>path rows and fills in each blob id from the git
// index of dir.
func readExempt(path, dir string) ([]leakgate.ExemptRow, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the exempt file: %v", err)
	}
	indexed, err := lsFiles(dir)
	if err != nil {
		return nil, err
	}
	blobs := map[string]string{}
	for _, tf := range indexed {
		blobs[tf.Path] = tf.Blob
	}
	var rows []leakgate.ExemptRow
	for i, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimRight(raw, "\r")
		if t := strings.TrimSpace(line); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		why, p, ok := strings.Cut(line, "\t")
		why, p = strings.TrimSpace(why), strings.TrimSpace(p)
		if !ok || why == "" || p == "" {
			return nil, fmt.Errorf("exempt file line %d: expected reason<TAB>path", i+1)
		}
		blob, found := blobs[p]
		if !found {
			return nil, fmt.Errorf("exempt file line %d: the path is not tracked in %s", i+1, dir)
		}
		rows = append(rows, leakgate.ExemptRow{Path: p, Blob: blob, Why: why})
	}
	return rows, nil
}

func derive(d *deriveFlags, getenv func(string) string) (*leakgate.DigestFile, error) {
	es, err := readList(d.plain)
	if err != nil {
		return nil, err
	}
	key, err := resolveKey(d.keyFile, getenv)
	if err != nil {
		return nil, err
	}
	exempt, err := readExempt(d.exempt, d.dir)
	if err != nil {
		return nil, err
	}
	df, err := leakgate.BuildDigest(key, es, nil, exempt)
	if err != nil {
		return nil, err
	}
	return df, nil
}

func cmdGen(args []string, stdout io.Writer, getenv func(string) string) error {
	fl := newFlags("gen")
	var d deriveFlags
	d.register(fl)
	out := fl.String("out", "", "the digest file to write")
	if err := parseFlags(fl, args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("gen: -out is required")
	}
	df, err := derive(&d, getenv)
	if err != nil {
		return fmt.Errorf("gen: %v", err)
	}
	b, err := df.Marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return fmt.Errorf("gen: %v", err)
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		return fmt.Errorf("gen: %v", err)
	}
	fmt.Fprintf(stdout, "wrote %d entries and %d exempt rows to %s\n", len(df.Entries), len(df.Exempt), *out)
	return nil
}

func cmdCheck(args []string, stdout io.Writer, getenv func(string) string) (int, error) {
	fl := newFlags("check")
	var d deriveFlags
	d.register(fl)
	digest := fl.String("digest", defaultDigest, "the committed digest file")
	if err := parseFlags(fl, args); err != nil {
		return 0, err
	}
	want, err := derive(&d, getenv)
	if err != nil {
		return 0, fmt.Errorf("check: %v", err)
	}
	wantBytes, err := want.Marshal()
	if err != nil {
		return 0, err
	}
	have, err := os.ReadFile(*digest)
	if err != nil {
		return 0, fmt.Errorf("check: reading the committed digest file: %v", err)
	}
	// A checkout with CRLF line endings holds the same file.
	have = bytes.ReplaceAll(have, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(have, wantBytes) {
		fmt.Fprintf(stdout, "stale: %s differs from the file derived from the list and the key (run gen)\n", *digest)
		return 1, nil
	}
	fmt.Fprintf(stdout, "ok: %s matches the list and the key\n", *digest)
	return 0, nil
}

// ---- patterns

func cmdPatterns(args []string, stdout io.Writer) error {
	fl := newFlags("patterns")
	plain := fl.String("plain", "", "the plaintext list")
	into := fl.String("into", "", "replace the marked block in this file (appended when absent)")
	if err := parseFlags(fl, args); err != nil {
		return err
	}
	es, err := readList(*plain)
	if err != nil {
		return err
	}
	block := leakgate.PatternBlock(es)
	if *into == "" {
		_, err := io.WriteString(stdout, block)
		return err
	}
	existing, err := os.ReadFile(*into)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("patterns: %v", err)
	}
	out := leakgate.ReplaceBlock(string(existing), block)
	if err := os.WriteFile(*into, []byte(out), 0o644); err != nil {
		return fmt.Errorf("patterns: %v", err)
	}
	fmt.Fprintf(stdout, "updated %s\n", *into)
	return nil
}

// ---- git plumbing (the library spawns nothing)

func git(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		return nil, fmt.Errorf("git %s: %v: %s", args[0], err, msg)
	}
	return out, nil
}

func lsFiles(dir string) ([]leakgate.TrackedFile, error) {
	out, err := git(dir, "ls-files", "-z", "-s")
	if err != nil {
		return nil, err
	}
	return leakgate.ParseLsFiles(out)
}

// parseLsTree parses `git ls-tree -r -z`: "<mode> <type> <object><TAB><path>".
func parseLsTree(b []byte) ([]leakgate.TrackedFile, error) {
	var out []leakgate.TrackedFile
	for len(b) > 0 {
		nul := bytes.IndexByte(b, 0)
		if nul < 0 {
			return nil, errors.New("ls-tree output: unterminated record")
		}
		rec := string(b[:nul])
		b = b[nul+1:]
		meta, path, ok := strings.Cut(rec, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, errors.New("ls-tree output: malformed record")
		}
		out = append(out, leakgate.TrackedFile{Mode: fields[0], Blob: fields[2], Path: path})
	}
	return out, nil
}

// refFS reads the blobs of one commit through a single `git cat-file --batch`.
type refFS struct {
	mu    sync.Mutex
	cmd   *exec.Cmd
	in    io.WriteCloser
	out   *bufio.Reader
	blobs map[string]string
}

func newRefFS(repo string, files []leakgate.TrackedFile) (*refFS, error) {
	cmd := exec.Command("git", "-C", repo, "cat-file", "--batch")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("git cat-file: %v", err)
	}
	r := &refFS{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 1<<20), blobs: map[string]string{}}
	for _, tf := range files {
		r.blobs[tf.Path] = tf.Blob
	}
	return r, nil
}

func (r *refFS) Close() {
	r.in.Close()
	r.cmd.Wait()
}

func (r *refFS) Read(path string, max int64) ([]byte, int64, error) {
	sha, ok := r.blobs[path]
	if !ok {
		return nil, 0, fs.ErrNotExist
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := io.WriteString(r.in, sha+"\n"); err != nil {
		return nil, 0, err
	}
	header, err := r.out.ReadString('\n')
	if err != nil {
		return nil, 0, err
	}
	f := strings.Fields(header)
	if len(f) == 2 && f[1] == "missing" {
		return nil, 0, fs.ErrNotExist
	}
	var size int64
	if len(f) != 3 || f[1] != "blob" {
		return nil, 0, fmt.Errorf("unexpected cat-file header")
	}
	if _, err := fmt.Sscanf(f[2], "%d", &size); err != nil {
		return nil, 0, err
	}
	if size > max {
		if _, err := io.CopyN(io.Discard, r.out, size+1); err != nil {
			return nil, 0, err
		}
		return nil, size, nil
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r.out, data); err != nil {
		return nil, 0, err
	}
	if _, err := r.out.ReadByte(); err != nil { // the LF after the body
		return nil, 0, err
	}
	return data, size, nil
}

// ---- scan

func cmdScan(args []string, stdout io.Writer) (int, error) {
	fl := newFlags("scan")
	plain := fl.String("plain", "", "the plaintext list")
	dir := fl.String("dir", "", "scan the tracked files of this work tree (default: .)")
	ref := fl.String("ref", "", "scan the blobs of this commit instead of a work tree")
	repo := fl.String("repo", ".", "with -ref: the repository to read")
	files := fl.String("files", "", "restrict the scan to a file list (path, old=>new, ?path)")
	digest := fl.String("digest", "", "apply the exempt rows of this digest file (no key needed)")
	if err := parseFlags(fl, args); err != nil {
		return 0, err
	}
	if *dir != "" && *ref != "" {
		return 0, errors.New("scan: -dir and -ref are exclusive")
	}
	es, err := readList(*plain)
	if err != nil {
		return 0, err
	}
	m, err := leakgate.NewPlainMatcher(leakgate.WithCanary(es))
	if err != nil {
		return 0, err
	}
	if err := m.SelfTest(); err != nil {
		return 0, fmt.Errorf("scan: %v", err)
	}
	var opts leakgate.ScanOptions
	opts.Shapes = true
	if *digest != "" {
		b, err := os.ReadFile(*digest)
		if err != nil {
			return 0, fmt.Errorf("scan: reading the digest file: %v", err)
		}
		df, err := leakgate.ParseDigest(b)
		if err != nil {
			return 0, fmt.Errorf("scan: %v", err)
		}
		opts.Exempt = df.Exempt
	}

	var tracked []leakgate.TrackedFile
	var fsys leakgate.FS
	root := ""
	if *ref != "" {
		out, err := git(*repo, "ls-tree", "-r", "-z", *ref)
		if err != nil {
			return 0, fmt.Errorf("scan: %v", err)
		}
		if tracked, err = parseLsTree(out); err != nil {
			return 0, fmt.Errorf("scan: %v", err)
		}
		rfs, err := newRefFS(*repo, tracked)
		if err != nil {
			return 0, fmt.Errorf("scan: %v", err)
		}
		defer rfs.Close()
		fsys = rfs
	} else {
		root = *dir
		if root == "" {
			root = "."
		}
		if tracked, err = lsFiles(root); err != nil {
			return 0, fmt.Errorf("scan: %v", err)
		}
		fsys = leakgate.DirFS{Root: root}
	}
	if len(tracked) == 0 {
		return 0, errors.New("scan: no tracked files (the gate would be blind)")
	}

	if *files != "" {
		text, err := os.ReadFile(*files)
		if err != nil {
			return 0, fmt.Errorf("scan: reading the file list: %v", err)
		}
		specs, err := leakgate.ParseFileList(string(text))
		if err != nil {
			return 0, fmt.Errorf("scan: the file list: %v", err)
		}
		set := map[string]bool{}
		for _, tf := range tracked {
			set[tf.Path] = true
		}
		if root != "" {
			if err := rejectUntracked(specs, set, root); err != nil {
				return 0, fmt.Errorf("scan: %v", err)
			}
		}
		keep, err := leakgate.ResolveFileList(specs, set)
		if err != nil {
			return 0, fmt.Errorf("scan: %v", err)
		}
		opts.Filter = func(p string) bool { return keep[p] }
	}

	rep := leakgate.ScanTree(m, tracked, fsys, opts)
	return printReport(stdout, rep), nil
}

// rejectUntracked refuses a listed path that exists on disk but is not tracked:
// scanning only the index would silently skip it (a vacuous pass).
func rejectUntracked(specs []leakgate.FileSpec, tracked map[string]bool, root string) error {
	for _, s := range specs {
		for _, p := range []string{s.Path, s.Old, s.New} {
			if p == "" || tracked[p] {
				continue
			}
			if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p))); err == nil {
				return fmt.Errorf("a listed path exists on disk but is not tracked (git add it so it is scanned): %s", p)
			}
		}
	}
	return nil
}

func printReport(w io.Writer, rep leakgate.Report) int {
	paths := map[string]bool{}
	for _, f := range rep.Findings {
		fmt.Fprintln(w, f.String())
		paths[f.Path] = true
	}
	for _, f := range rep.Fatals {
		where := f.Path
		if f.Line > 0 {
			where = fmt.Sprintf("%s:%d", f.Path, f.Line)
		}
		if where == "" {
			fmt.Fprintf(w, "FATAL %s\n", f.Reason)
		} else {
			fmt.Fprintf(w, "FATAL %s %s\n", where, f.Reason)
		}
	}
	c := rep.Classes
	fmt.Fprintf(w, "scanned %d of %d tracked files; findings %d in %d files; fatal %d; classes: runs>96=%d binaries=%d png-nonstd-chunks=%d png-after-iend=%d png-compressed-text=%d png-no-iend=%d oversize=%d symlink=%d submodule=%d\n",
		rep.Scanned, rep.Total, len(rep.Findings), len(paths), len(rep.Fatals),
		c.Runs96, c.Binaries, c.PNGNonStd, c.PNGAfterIEND, c.PNGCompressed, c.PNGNoIEND, c.Oversize, c.Symlink, c.Submodule)
	if len(rep.Findings) == 0 && len(rep.Fatals) == 0 {
		return 0
	}
	return 1
}
