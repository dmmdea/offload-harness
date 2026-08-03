package fleetnode

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pngBytes/jpegBytes are real magic-byte-prefixed bodies padded out so
// sniffImageExt sees a plausible image, not a bare 3-4 byte signature.
var pngBytes = append(append([]byte{}, "\x89PNG\r\n\x1a\n"...), make([]byte, 100)...)
var jpegBytes = append(append([]byte{}, "\xFF\xD8\xFF\xE0"...), make([]byte, 100)...)

// TestFetchRefs covers the task-5 brief's matrix (a)-(f) plus (g): the
// default-port decision FetchRefs' host allowlist check has to make for a ref
// URL that omits a port.
func TestFetchRefs(t *testing.T) {
	// (a) two valid PNG refs land as files; returned paths exist and are
	// absolute; keys match; content matches.
	t.Run("a_valid_png_refs_land_as_files", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(pngBytes)
		}))
		defer srv.Close()

		destDir := filepath.Join(t.TempDir(), "job-a")
		refs := map[string]string{
			"product": srv.URL + "/product-src.png",
			"logo":    srv.URL + "/logo-src.png",
		}
		paths, cleanup, err := FetchRefs(context.Background(), refs, destDir, nil, 24)
		if err != nil {
			t.Fatalf("FetchRefs: %v", err)
		}
		defer cleanup()

		if len(paths) != len(refs) {
			t.Fatalf("want %d paths, got %d: %+v", len(refs), len(paths), paths)
		}
		for key := range refs {
			p, ok := paths[key]
			if !ok {
				t.Fatalf("missing returned path for key %q", key)
			}
			if !filepath.IsAbs(p) {
				t.Fatalf("path for %q not absolute: %s", key, p)
			}
			if filepath.Ext(p) != ".png" {
				t.Fatalf("want .png ext for %q, got %s", key, p)
			}
			b, readErr := os.ReadFile(p)
			if readErr != nil {
				t.Fatalf("reading %s: %v", p, readErr)
			}
			if !bytes.Equal(b, pngBytes) {
				t.Fatalf("content mismatch for %s", key)
			}
		}
	})

	// (b) a ref answering 404 -> error naming the ref key and URL, destDir
	// removed (no partial state survives a failed set).
	t.Run("b_404_names_ref_and_url_and_removes_destDir", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}))
		defer srv.Close()

		destDir := filepath.Join(t.TempDir(), "job-b")
		refURL := srv.URL + "/missing.png"
		_, _, err := FetchRefs(context.Background(), map[string]string{"product": refURL}, destDir, nil, 24)
		if err == nil {
			t.Fatal("want error for a 404 ref")
		}
		if !strings.Contains(err.Error(), "product") || !strings.Contains(err.Error(), refURL) {
			t.Fatalf("error must name the ref key and URL, got: %v", err)
		}
		if _, statErr := os.Stat(destDir); !os.IsNotExist(statErr) {
			t.Fatalf("destDir must be removed after failure, stat err = %v", statErr)
		}
	})

	// (c) a body of capMB+1 (here 1 MiB + 1 KiB against a 1 MiB cap) ->
	// error mentioning the cap, no partial file (destDir) left behind.
	t.Run("c_body_over_cap_is_refused_and_cleaned_up", func(t *testing.T) {
		const capMB = 1
		over := make([]byte, capMB<<20+1024) // comfortably over the 1 MiB cap
		copy(over, "\x89PNG\r\n\x1a\n") // valid magic so the cap check trips first, not the sniff check
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(over)
		}))
		defer srv.Close()

		destDir := filepath.Join(t.TempDir(), "job-c")
		_, _, err := FetchRefs(context.Background(), map[string]string{"background": srv.URL + "/big.png"}, destDir, nil, capMB)
		if err == nil {
			t.Fatal("want error for an over-cap body")
		}
		if !strings.Contains(err.Error(), "cap") {
			t.Fatalf("error must mention the cap, got: %v", err)
		}
		if _, statErr := os.Stat(destDir); !os.IsNotExist(statErr) {
			t.Fatalf("destDir must be removed after cap failure, stat err = %v", statErr)
		}
	})

	// (d) a JPEG-magic body passes; an SVG/<svg body is rejected as "not an
	// allowed image type".
	t.Run("d_jpeg_passes_svg_rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/ok.jpg":
				w.Write(jpegBytes)
			case "/bad.svg":
				w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		okDir := filepath.Join(t.TempDir(), "job-d-ok")
		paths, cleanup, err := FetchRefs(context.Background(), map[string]string{"logo": srv.URL + "/ok.jpg"}, okDir, nil, 24)
		if err != nil {
			t.Fatalf("jpeg ref should pass: %v", err)
		}
		defer cleanup()
		if filepath.Ext(paths["logo"]) != ".jpg" {
			t.Fatalf("want .jpg ext, got %s", paths["logo"])
		}

		badDir := filepath.Join(t.TempDir(), "job-d-bad")
		_, _, err = FetchRefs(context.Background(), map[string]string{"logo": srv.URL + "/bad.svg"}, badDir, nil, 24)
		if err == nil || !strings.Contains(err.Error(), "not an allowed image type") {
			t.Fatalf("svg ref must be rejected as not an allowed image type, got: %v", err)
		}
	})

	// (e) a non-allowlisted, unreachable host must be refused BEFORE any
	// dial. Loopback is allowlisted, so the deny path needs a real deny
	// target: 8.8.8.8:1 plus a ctx timeout that would trip if a dial were
	// ever attempted. Assert the error is the allowlist error (not a dial or
	// context-deadline error) and that refusal is near-instant.
	t.Run("e_non_allowlisted_host_refused_before_dial", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		destDir := filepath.Join(t.TempDir(), "job-e")
		start := time.Now()
		_, _, err := FetchRefs(ctx, map[string]string{"product": "http://8.8.8.8:1/x"}, destDir, nil, 24)
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("want error for a non-allowlisted host")
		}
		if !strings.Contains(err.Error(), "not allowlisted") {
			t.Fatalf("want the allowlist error, got: %v", err)
		}
		if strings.Contains(err.Error(), "context deadline exceeded") || strings.Contains(err.Error(), "dial") {
			t.Fatalf("must be refused before any dial attempt, got: %v", err)
		}
		if elapsed > time.Second {
			t.Fatalf("allowlist refusal took %v — looks like a dial was attempted", elapsed)
		}
	})

	// (f) a redirect response is refused (CheckRedirect returns an error);
	// the redirect target must never be contacted.
	t.Run("f_redirect_refused", func(t *testing.T) {
		var target *httptest.Server
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/final.png", http.StatusMovedPermanently)
		}))
		defer srv.Close()
		target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("redirect target must never be contacted")
			w.Write(pngBytes)
		}))
		defer target.Close()

		destDir := filepath.Join(t.TempDir(), "job-f")
		_, _, err := FetchRefs(context.Background(), map[string]string{"product": srv.URL + "/start.png"}, destDir, nil, 24)
		if err == nil || !strings.Contains(err.Error(), "redirect") {
			t.Fatalf("want a redirect-refused error, got: %v", err)
		}
	})

	// (g) default-port decision: a ref URL with no explicit port must
	// resolve to the scheme's conventional default (80/443) for the
	// allowlist/extraAllow check — whitebox on hostPort since binding an
	// actual listener to :80/:443 in a test needs privileges we don't want
	// to require.
	t.Run("g_default_port_by_scheme", func(t *testing.T) {
		httpURL, _ := url.Parse("http://100.64.1.2/x")
		if h, p := hostPort(httpURL); h != "100.64.1.2" || p != "80" {
			t.Fatalf("http default port: got host=%s port=%s", h, p)
		}
		httpsURL, _ := url.Parse("https://100.64.1.2/x")
		if h, p := hostPort(httpsURL); h != "100.64.1.2" || p != "443" {
			t.Fatalf("https default port: got host=%s port=%s", h, p)
		}
		explicitURL, _ := url.Parse("http://100.64.1.2:9999/x")
		if h, p := hostPort(explicitURL); h != "100.64.1.2" || p != "9999" {
			t.Fatalf("explicit port must win over the default: got host=%s port=%s", h, p)
		}
	})
}
