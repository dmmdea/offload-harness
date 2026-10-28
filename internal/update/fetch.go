package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// Download streams asset into dir, hashing as it goes, and verifies size and the publisher's sha256 digest. The file
// is removed on any mismatch. It returns the path and the verified size.
func Download(ctx context.Context, hc *http.Client, a Asset, dir string, logf func(string, ...any)) (string, int64, error) {
	if !strings.HasPrefix(a.Digest, "sha256:") {
		return "", 0, fmt.Errorf("%s: the publisher gave no sha256 digest (%q); refusing to install unverified bytes", a.Name, truncate(a.Digest, 20))
	}
	want := strings.ToLower(strings.TrimPrefix(a.Digest, "sha256:"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", "local-offload-update")
	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("download %s: HTTP %d", a.Name, resp.StatusCode)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, err
	}
	path := filepath.Join(dir, a.Name)
	f, err := os.Create(path)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	pr := &progress{r: resp.Body, total: a.Size, name: a.Name, logf: logf}
	n, err := io.Copy(io.MultiWriter(f, h), pr)
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", 0, fmt.Errorf("download %s: %w", a.Name, err)
	}
	if a.Size > 0 && n != a.Size {
		_ = os.Remove(path)
		return "", 0, fmt.Errorf("%s: downloaded %d bytes, the publisher lists %d", a.Name, n, a.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		_ = os.Remove(path)
		return "", 0, fmt.Errorf("%s: sha256 %s does not match the publisher's digest %s; the file was discarded", a.Name, got, want)
	}
	return path, n, nil
}

type progress struct {
	r     io.Reader
	total int64
	read  int64
	next  int64
	name  string
	logf  func(string, ...any)
}

func (p *progress) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	if p.logf != nil && p.total > 32<<20 && p.read >= p.next {
		p.logf("  downloading %s: %d of %d MiB", p.name, p.read>>20, p.total>>20)
		p.next = p.read + p.total/4
	}
	return n, err
}

// Attest runs `gh attestation verify` on a downloaded file when gh is on PATH. A publisher with no attestation
// published is recorded as "none published"; anything else that fails is a failure (fail closed).
func Attest(ctx context.Context, repo, file string) (string, error) {
	gh, err := exec.LookPath("gh")
	if err != nil || repo == "" || strings.HasPrefix(repo, "npm:") {
		return "skipped (gh not available)", nil
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	out, err := winexec.CommandContext(cctx, gh, "attestation", "verify", file, "--repo", repo).CombinedOutput()
	if err == nil {
		return "verified", nil
	}
	low := strings.ToLower(string(out))
	switch {
	// Only the two answers that mean "this publisher attests nothing here": a bare "not found" also matches failures
	// of the check itself ("trusted root not found"), which must not pass as "none published".
	case strings.Contains(low, "no attestations found"), strings.Contains(low, "http 404"):
		return "none published", nil // the publisher does not attest this repository's releases
	case strings.Contains(low, "gh auth login"), strings.Contains(low, "authentication token"), strings.Contains(low, "http 401"):
		return "skipped (gh is not signed in)", nil
	}
	return "", fmt.Errorf("gh attestation verify failed: %s", truncate(strings.TrimSpace(string(out)), 300))
}

// Extract unpacks a .zip or .tar.gz into dir. Entries that would leave dir are refused.
func Extract(archive, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	switch {
	case strings.HasSuffix(archive, ".zip"):
		return extractZip(archive, dir)
	case strings.HasSuffix(archive, ".tar.gz"), strings.HasSuffix(archive, ".tgz"):
		return extractTarGz(archive, dir)
	}
	return fmt.Errorf("%s: not a .zip or .tar.gz", filepath.Base(archive))
}

func safeJoin(dir, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q leaves the destination", name)
	}
	return filepath.Join(dir, clean), nil
}

func extractZip(archive, dir string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		dst, err := safeJoin(dir, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		mode := f.Mode().Perm() | 0o644
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			rc.Close()
			return err
		}
		_, cerr := io.Copy(out, rc)
		rc.Close()
		if e := out.Close(); cerr == nil {
			cerr = e
		}
		if cerr != nil {
			return cerr
		}
	}
	return nil
}

func extractTarGz(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		dst, err := safeJoin(dir, h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(h.Mode)&0o777|0o644)
			if err != nil {
				return err
			}
			_, cerr := io.Copy(out, tr)
			if e := out.Close(); cerr == nil {
				cerr = e
			}
			if cerr != nil {
				return cerr
			}
		case tar.TypeSymlink:
			if runtime.GOOS == "windows" {
				continue
			}
			// A link stays inside dir: its resolved target must.
			target := h.Linkname
			resolved := target
			if !filepath.IsAbs(target) {
				resolved = filepath.Join(filepath.Dir(dst), target)
			}
			if rel, rerr := filepath.Rel(dir, resolved); rerr != nil || strings.HasPrefix(rel, "..") {
				return fmt.Errorf("archive link %q points outside the destination", h.Name)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			_ = os.Remove(dst)
			if err := os.Symlink(target, dst); err != nil {
				return err
			}
		}
	}
}

// hoistSingleDir lifts the contents of dir's only subdirectory into dir when dir holds nothing else (the
// "llama-<tag>/" wrapper some archives carry), so the live path is the directory that holds the binaries.
func hoistSingleDir(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(ents) != 1 || !ents[0].IsDir() {
		return nil
	}
	inner := filepath.Join(dir, ents[0].Name())
	sub, err := os.ReadDir(inner)
	if err != nil {
		return err
	}
	tmp := inner + ".hoist"
	if err := os.Rename(inner, tmp); err != nil {
		return err
	}
	for _, e := range sub {
		if err := os.Rename(filepath.Join(tmp, e.Name()), filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return os.Remove(tmp)
}

func sha256File(path string) (string, error) {
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

// findFile returns the first file called name under dir.
func findFile(dir, name string) string {
	var found string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if !d.IsDir() && strings.EqualFold(d.Name(), name) {
			found = p
		}
		return nil
	})
	return found
}
