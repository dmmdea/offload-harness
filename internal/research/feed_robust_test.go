package research

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Feeds as they are served: a legacy code page, a stray byte, a body that ends mid-item, links that are relative.

// fetchFeed serves body under ct from a name that resolves public and fetches it through Fetch.
func fetchFeed(t *testing.T, ct, body, path string) Fetched {
	t.Helper()
	pinDNS(t, map[string][]string{"feeds.example": {sentinelPublic}})
	var hits atomic.Int32
	srv := serveFeed(t, ct, body, &hits)
	d := &honestDialer{toReal: srv.Listener.Addr().String()}
	return Fetch(context.Background(), "http://feeds.example"+path, Options{Client: d.client(), Timeout: 5 * time.Second})
}

// A windows-1252 feed writes its curly quotes, euro sign and dashes in bytes 0x80-0x9F, which Latin-1 reads as
// control characters. 0x93 and 0x94 are the curly double quotes, 0x80 the euro sign, 0x96 the en dash.
const cp1252Item = "<item><title>\x93Quoted\x94 \x80 5 \x96 ok</title><link>https://feeds.example/1</link><description>caf\xe9 \x97 d\xe9j\xe0 vu</description></item>"

func cp1252Feed(declaration string) string {
	return declaration + "<rss version=\"2.0\"><channel><title>Notes</title>" + cp1252Item + "</channel></rss>"
}

func TestFeedWindows1252IsDecodedNotReadAsLatin1(t *testing.T) {
	want := "\u201cQuoted\u201d \u20ac 5 \u2013 ok"
	for name, body := range map[string]string{
		"windows-1252": cp1252Feed(`<?xml version="1.0" encoding="windows-1252"?>`),
		"cp1252":       cp1252Feed(`<?xml version="1.0" encoding="cp1252"?>`),
		"iso-8859-1":   cp1252Feed(`<?xml version="1.0" encoding="ISO-8859-1"?>`), // what browsers do: the label names windows-1252
	} {
		t.Run(name, func(t *testing.T) {
			_, text, err := FeedToText([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(text, "1. "+want) {
				t.Errorf("title not decoded as windows-1252, want %q in:\n%q", want, text)
			}
			for _, r := range text {
				if r >= 0x80 && r <= 0x9f {
					t.Errorf("a C1 control character U+%04X reached the text: %q", r, text)
					break
				}
			}
			if !strings.Contains(text, "caf\u00e9 \u2014 d\u00e9j\u00e0 vu") {
				t.Errorf("summary not decoded (e-acute, em dash expected):\n%q", text)
			}
		})
	}
}

func TestFeedHTTPCharsetDecidesWhenTheDocumentDeclaresNone(t *testing.T) {
	want := "\u201cQuoted\u201d \u20ac 5 \u2013 ok"
	got := fetchFeed(t, "application/rss+xml; charset=windows-1252", cp1252Feed(""), "/feed.xml")
	if got.Err != "" || !strings.Contains(got.Text, "1. "+want) {
		t.Fatalf("the HTTP charset was not honoured: err %q\n%q", got.Err, got.Text)
	}
	// and the declaration is not applied a second time to text that is already decoded
	got = fetchFeed(t, "application/rss+xml; charset=windows-1252", cp1252Feed(`<?xml version="1.0" encoding="windows-1252"?>`), "/feed.xml")
	if got.Err != "" || !strings.Contains(got.Text, "1. "+want) || strings.Contains(got.Text, "\u00e2") {
		t.Fatalf("decoded twice or not at all: err %q\n%q", got.Err, got.Text)
	}
	// HTTP names the encoding for the bytes it sends: a UTF-8 header over a document that declares windows-1252
	// is read as UTF-8, with the bad bytes marked, not decoded as the declaration says
	got = fetchFeed(t, "application/rss+xml; charset=utf-8", cp1252Feed(`<?xml version="1.0" encoding="windows-1252"?>`), "/feed.xml")
	if got.Err != "" || !strings.Contains(got.Text, "Quoted") || strings.Contains(got.Text, "\u201c") {
		t.Fatalf("the header did not win over the declaration: err %q\n%q", got.Err, got.Text)
	}
}

func rssWithTitles(n int, edit func(i int, title string) string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><title>T</title>`)
	for i := 1; i <= n; i++ {
		title := fmt.Sprintf("Item number %d", i)
		if edit != nil {
			title = edit(i, title)
		}
		fmt.Fprintf(&b, `<item><title>%s</title><link>https://feeds.example/%d</link><description>Body of item %d.</description></item>`, title, i, i)
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

func TestFeedWithAnInvalidByteKeepsEveryItem(t *testing.T) {
	for name, edit := range map[string]func(int, string) string{
		"a stray 0xFF byte": func(i int, s string) string {
			if i == 2 {
				return s + " \xff broken"
			}
			return s
		},
		"a truncated multi-byte sequence": func(i int, s string) string {
			if i == 2 {
				return s + " \xe2\x82"
			}
			return s
		},
		"a control character XML forbids": func(i int, s string) string {
			if i == 2 {
				return s + " \x0c form feed"
			}
			return s
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, text, err := FeedToText([]byte(rssWithTitles(4, edit)))
			if err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 4; i++ {
				if !strings.Contains(text, fmt.Sprintf("%d. Item number %d", i, i)) {
					t.Errorf("item %d was dropped:\n%s", i, text)
				}
			}
			if !strings.Contains(text, "Items: 4\n") || strings.Contains(text, "cut off") {
				t.Errorf("the header must count all four items and not claim a cut:\n%s", text)
			}
		})
	}
}

func TestFeedBodyCutMidItemSaysSoAndCountsHonestly(t *testing.T) {
	full := rssWithTitles(4, nil)
	cutAt := strings.Index(full, "Body of item 3") + len("Body of")
	body := full[:cutAt]
	got := fetchFeed(t, "application/rss+xml", body, "/feed.xml")
	if got.Err != "" {
		t.Fatalf("fetch: %s", got.Err)
	}
	if !got.Truncated {
		t.Errorf("a feed that ends mid-item must be marked Truncated")
	}
	for _, want := range []string{"1. Item number 1", "2. Item number 2"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("missing %q:\n%s", want, got.Text)
		}
	}
	if strings.Contains(got.Text, "3. ") {
		t.Errorf("the item that was cut off was listed:\n%s", got.Text)
	}
	head := got.Text[:strings.Index(got.Text, "\n1. ")]
	if !strings.Contains(head, "Items: 2") || !strings.Contains(head, "cut off") || strings.Contains(head, " of 3") || strings.Contains(head, "of at least") {
		t.Errorf("the header should say 2 items and that the document was cut, without counting the failed item:\n%s", head)
	}
}

// A feed longer than the read bound is cut at 2 MiB, in the middle of an item, and says so first.
func TestFeedLongerThanTheReadBoundIsReportedAsCut(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<rss version="2.0"><channel><title>Long</title>`)
	for i := 1; b.Len() < MaxFetchBytes+20000; i++ {
		fmt.Fprintf(&b, `<item><title>Entry %d</title><link>https://feeds.example/%d</link><description>%s</description></item>`, i, i, strings.Repeat("filler ", 700))
	}
	b.WriteString(`</channel></rss>`)
	got := fetchFeed(t, "application/rss+xml", b.String(), "/long.xml")
	if got.Err != "" || !got.Truncated {
		t.Fatalf("err %q truncated %v", got.Err, got.Truncated)
	}
	head := got.Text[:strings.Index(got.Text, "\n1. ")]
	if !strings.Contains(head, "cut off") || !strings.Contains(head, "at least") {
		t.Errorf("a feed past the cap that is also cut should say both, the cut first:\n%s", head)
	}
	if strings.Count(got.Text, "Link: ") != MaxFeedItems {
		t.Errorf("want %d items listed, got %d", MaxFeedItems, strings.Count(got.Text, "Link: "))
	}
}

func TestFeedRelativeLinksResolveAgainstTheFeedAddressAndXMLBase(t *testing.T) {
	rss := `<rss version="2.0"><channel><title>R</title>` +
		`<item><title>Root relative</title><link>/posts/1</link></item>` +
		`<item><title>Path relative</title><link>posts/2</link></item>` +
		`<item><title>Absolute</title><link>https://other.example/x</link></item>` +
		`<item><title>Scheme relative</title><link>//cdn.example/y</link></item>` +
		`</channel></rss>`
	got := fetchFeed(t, "application/rss+xml", rss, "/blog/feed.xml")
	if got.Err != "" {
		t.Fatal(got.Err)
	}
	for _, want := range []string{
		"Link: http://feeds.example/posts/1", "Link: http://feeds.example/blog/posts/2",
		"Link: https://other.example/x", "Link: http://cdn.example/y",
	} {
		if !strings.Contains(got.Text, want+"\n") {
			t.Errorf("missing %q:\n%s", want, got.Text)
		}
	}

	atom := `<feed xmlns="http://www.w3.org/2005/Atom" xml:base="https://static.example/root/"><title>A</title>` +
		`<entry><title>Under the feed base</title><link href="a/1"/></entry>` +
		`<entry xml:base="https://entry.example/e/"><title>Under the entry base</title><link href="a/2"/></entry>` +
		`<entry><title>Under the link base</title><link xml:base="https://link.example/l/" href="a/3"/></entry>` +
		`<entry><title>Absolute</title><link href="https://abs.example/a/4"/></entry>` +
		`</feed>`
	got = fetchFeed(t, "application/atom+xml", atom, "/atom.xml")
	if got.Err != "" {
		t.Fatal(got.Err)
	}
	for _, want := range []string{
		"Link: https://static.example/root/a/1", "Link: https://entry.example/e/a/2",
		"Link: https://link.example/l/a/3", "Link: https://abs.example/a/4",
	} {
		if !strings.Contains(got.Text, want+"\n") {
			t.Errorf("missing %q:\n%s", want, got.Text)
		}
	}

	// a document with no base of its own is relative to where it was fetched from, after redirects
	got = fetchFeed(t, "application/atom+xml", `<feed xmlns="http://www.w3.org/2005/Atom"><title>B</title><entry><title>E</title><link href="../p/9"/></entry></feed>`, "/a/b/feed.atom")
	if !strings.Contains(got.Text, "Link: http://feeds.example/a/p/9\n") {
		t.Errorf("relative to the feed address:\n%s", got.Text)
	}
}
