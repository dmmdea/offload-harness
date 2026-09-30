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
		"http://[::1]/", "http://169.254.169.254/latest/meta-data/", "http://100.64." + "1.7:18811/fleet/health",
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

func TestBuildContractsShape(t *testing.T) {
	fetched := []Fetched{
		{URL: "https://docs.lmcache.ai/mp/", FinalURL: "https://docs.lmcache.ai/mp/", Title: "MP", Text: "The driven path uses shared memory transfers between processes.\nThe engine copies every buffer through pinned staging memory, which is slower than the shared handle path."},
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
	// Presence is declared in the schema; items are asked for only by a caller's
	// own mark (build_test.go), so the default carries no min_items: a digest of a
	// page with nothing to say is complete with empty lists (its one statement,
	// the verdict, is build_verdict_test.go's).
	if joined := strings.Join(c.Acceptance, " "); strings.Contains(joined, "min_items:") {
		t.Fatalf("the default schema must not carry an automatic items check: %v", c.Acceptance)
	}
	if !strings.Contains(string(c.OutputSchema), `"required":["key_facts","numbers","quotes","verdict"]`) {
		t.Fatalf("default schema must declare every digest field required: %s", c.OutputSchema)
	}
	if !strings.HasPrefix(c.Acceptance[0], "regex:(?i)(?P<docanchor>") || !strings.Contains(c.Acceptance[0], "pinned") || strings.Contains(c.Acceptance[0], "modes") {
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
