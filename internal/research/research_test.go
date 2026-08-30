package research

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTMLToTextStripsChromeAndKeepsProse(t *testing.T) {
	src := `<!doctype html><html><head><title>LMCache &amp; friends</title><style>p{color:red}</style>
<script>alert(1)</script></head><body><nav><a href="/">Home</a><a href="/blog">Blog</a></nav>
<header>Site header</header><main><h1>MP mode</h1><p>The L1 pool lives in <b>/dev/shm</b>.</p>
<pre>lmcache server --l1-size-gb 20</pre><ul><li>one</li><li>two</li></ul></main>
<footer>© 2026</footer><!-- hidden --></body></html>`
	title, text := HTMLToText(src)
	if title != "LMCache & friends" {
		t.Fatalf("title %q", title)
	}
	for _, want := range []string{"MP mode", "The L1 pool lives in /dev/shm.", "lmcache server --l1-size-gb 20", "one\n\ntwo"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text missing %q:\n%s", want, text)
		}
	}
	for _, drop := range []string{"alert(1)", "color:red", "Home", "Site header", "© 2026", "hidden"} {
		if strings.Contains(text, drop) {
			t.Fatalf("text kept %q:\n%s", drop, text)
		}
	}
}

func TestValidateURLRefusesNonPublic(t *testing.T) {
	ctx := context.Background()
	bad := []string{
		"ftp://example.com/x", "file:///etc/passwd", "http://localhost:11436/v1/models",
		"http://127.0.0.1:8000/", "http://10.0.0.79/", "http://192.168.1.1/", "http://172.16.5.5/",
		"http://[::1]/", "http://169.254.169.254/latest/meta-data/", "http://100.65.3.91:18811/fleet/health",
		"http://qube.local/", "http://fleet.internal/", "http://",
	}
	for _, u := range bad {
		if _, err := ValidateURL(ctx, u); err == nil {
			t.Errorf("%s: accepted, want refusal", u)
		}
	}
	if _, err := ValidateURL(ctx, "https://93.184.216.34/"); err != nil {
		t.Errorf("public literal refused: %v", err)
	}
}

func TestFetchGuardsRedirectsAndStrips(t *testing.T) {
	// A public-looking server is impossible offline, so exercise the pipeline
	// through the guard bypass a test client cannot get: Fetch on a loopback
	// server must be REFUSED (the guard runs first) — proving no test can
	// accidentally read a local service through this lane.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><p>secret</p></body></html>"))
	}))
	defer srv.Close()
	got := Fetch(context.Background(), srv.URL, Options{})
	if got.Err == "" || got.Text != "" {
		t.Fatalf("loopback fetch not refused: %+v", got)
	}
}

func TestAnchorNeverComesFromTheGoal(t *testing.T) {
	text := strings.Repeat("The CudaIPCWrapper bundles a handle. CudaIPCWrapper is small. ", 3) + "pickle_dumps path copies four times. pickle_dumps again."
	goal := "Explain the CudaIPCWrapper and the transfer paths"
	a := Anchor(text, goal)
	if a == "" || strings.Contains(strings.ToLower(goal), strings.ToLower(a)) {
		t.Fatalf("anchor %q is empty or parrot-passable", a)
	}
	if a != "pickle_dumps" {
		t.Fatalf("anchor %q, want the identifier-shaped token absent from the goal", a)
	}
}

func TestBuildContractsShape(t *testing.T) {
	fetched := []Fetched{
		{URL: "https://docs.lmcache.ai/mp/", FinalURL: "https://docs.lmcache.ai/mp/", Title: "MP", Text: "The lmcache_driven path uses CUDA IPC. lmcache_driven is the default. engine_driven copies. engine_driven is slower."},
		{URL: "https://example.com/404", Err: "http 404"},
	}
	specs, sources := Build(Request{Goal: "List the transfer modes named."}, fetched)
	if len(specs) != 1 || len(sources) != 2 {
		t.Fatalf("specs=%d sources=%d", len(specs), len(sources))
	}
	if sources[1].Skipped != "http 404" || sources[0].Skipped != "" {
		t.Fatalf("skip bookkeeping wrong: %+v", sources)
	}
	c := specs[0].AgentContract
	if !strings.Contains(c.Goal, "already been fetched") || !strings.Contains(c.Goal, "01-docs.lmcache.ai.txt") {
		t.Fatalf("goal must name the materialized file: %q", c.Goal)
	}
	if len(c.Context) != 1 || c.Context[0].Name != "01-docs.lmcache.ai.txt" || strings.ContainsAny(c.Context[0].Name, `/\`) {
		t.Fatalf("context doc naming: %+v", c.Context)
	}
	var sch struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(c.OutputSchema, &sch); err != nil || sch.Properties["key_facts"] == nil {
		t.Fatalf("default schema not applied: %s", c.OutputSchema)
	}
	joined := strings.Join(c.Acceptance, " ")
	if !strings.Contains(joined, "min_items:key_facts:1") {
		t.Fatalf("shape check missing: %v", c.Acceptance)
	}
	if !strings.HasPrefix(c.Acceptance[0], "regex:(?i)(") || !strings.Contains(c.Acceptance[0], "lmcache_driven") || strings.Contains(c.Acceptance[0], "transfer") {
		t.Fatalf("anchor acceptance must be a page-only alternation: %v", c.Acceptance)
	}
	if strings.Contains(strings.ToLower(c.Goal), "do not try to open") || !strings.Contains(c.Goal, "read that file") {
		t.Fatalf("goal must tell the seat to READ the materialized file: %q", c.Goal)
	}
}

func TestDocNameFlat(t *testing.T) {
	for i, tc := range []struct{ url, want string }{
		{"https://www.Example.com/a/b?c=d", "01-example.com.txt"},
		{"https://blog.lmcache.ai/en/2026/", "02-blog.lmcache.ai.txt"},
		{"", "03-source.txt"},
	} {
		if got := DocName(i, tc.url, tc.url); got != tc.want {
			t.Errorf("%d: %q want %q", i, got, tc.want)
		}
	}
}

func TestAnchorsSkipHexBlobsAndBuildAlternation(t *testing.T) {
	text := "image sha256:4449f856653602317e4101a76fce599c7fcd58ccec2e539951fce5f73083179e 4449f856653602317e4101a76fce599c7fcd58ccec2e539951fce5f73083179e. Use uv_pip_install twice: uv_pip_install. The rocm7 wheel; rocm7 again."
	got := Anchors(text, "install commands", 3)
	for _, g := range got {
		if len(g) >= 20 && strings.Trim(g, "0123456789abcdef") == "" {
			t.Fatalf("hex blob chosen as anchor: %v", got)
		}
	}
	chk := AnchorCheck(text, "install commands")
	if !strings.HasPrefix(chk, "regex:(?i)(") || !strings.Contains(chk, "uv_pip_install") {
		t.Fatalf("AnchorCheck %q", chk)
	}
}
