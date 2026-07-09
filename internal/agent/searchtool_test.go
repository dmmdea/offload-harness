package agent

import "testing"

func TestDDGRealURL(t *testing.T) {
	got := ddgRealURL("//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fpage&rut=abc")
	if got != "https://example.com/page" {
		t.Errorf("ddgRealURL = %q, want https://example.com/page", got)
	}
	// a bare URL passes through
	if got := ddgRealURL("https://plain.example.com/x"); got != "https://plain.example.com/x" {
		t.Errorf("bare url = %q", got)
	}
}

func TestParseDDG(t *testing.T) {
	html := `<a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fa">Title A</a>` +
		`<a class="result__snippet" href="x">snippet <b>A</b> here</a>` +
		`<a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fb">Title B</a>` +
		`<a class="result__snippet" href="x">snippet B</a>`
	r := parseDDG(html, 5)
	if len(r) != 2 {
		t.Fatalf("parseDDG got %d results, want 2", len(r))
	}
	if r[0]["url"] != "https://example.com/a" || r[0]["title"] != "Title A" {
		t.Errorf("result 0 = %v", r[0])
	}
	if r[0]["snippet"] != "snippet A here" {
		t.Errorf("snippet 0 = %q, want 'snippet A here'", r[0]["snippet"])
	}
	// respects the max
	if got := parseDDG(html, 1); len(got) != 1 {
		t.Errorf("max=1 got %d", len(got))
	}
}
