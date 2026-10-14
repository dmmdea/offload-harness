package research

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// RSS and Atom as a text source. A feed response is parsed with encoding/xml (no
// dependency) and its items' titles, links, dates and summaries become the text
// a seat digests. The item links are only text: nothing here follows them, so a
// feed can never widen a call's fetch list; a caller who wants an item reads it
// by listing its URL.

// Feed bounds. A feed past MaxFeedItems is cut and says so, and a summary past
// MaxFeedSummaryChars is cut at a rune boundary; the response body was already
// bounded by MaxFetchBytes before it reached the parser.
const (
	MaxFeedItems        = 50
	MaxFeedSummaryChars = 1000
	maxFeedFieldChars   = 300
	sniffWindow         = 2048
)

type feedLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Text string `xml:",chardata"`
}

// feedItem is one RSS <item> or Atom <entry>. Repeated elements are slices so a
// namespaced twin (media:title, media:content) cannot overwrite the real field.
type feedItem struct {
	Titles       []string   `xml:"title"`
	Links        []feedLink `xml:"link"`
	GUIDs        []string   `xml:"guid"`
	PubDates     []string   `xml:"pubDate"`
	Dates        []string   `xml:"date"`
	Published    []string   `xml:"published"`
	Updated      []string   `xml:"updated"`
	Descriptions []string   `xml:"description"`
	Summaries    []string   `xml:"summary"`
	Contents     []string   `xml:"content"`
	Encoded      []string   `xml:"encoded"`
}

// isFeedMediaType reports whether a Content-Type names a feed outright.
func isFeedMediaType(ct string) bool {
	return strings.Contains(ct, "rss+xml") || strings.Contains(ct, "atom+xml") || strings.Contains(ct, "rdf+xml")
}

// feedSniffable reports whether a response of this Content-Type may be a feed
// served under a generic type: XML in general, plain text, a binary stream, or
// no type at all. An HTML response is never sniffed.
func feedSniffable(ct string) bool {
	if strings.Contains(ct, "html") {
		return false
	}
	return ct == "" || strings.Contains(ct, "xml") || strings.HasPrefix(ct, "text/plain") || strings.HasPrefix(ct, "application/octet-stream")
}

const atomNS = "http://www.w3.org/2005/Atom"

// SniffFeed reports whether the body is a feed document: after an optional BOM,
// whitespace, XML declaration, comments and a doctype, the root element is rss,
// an RDF root (RSS 1.0) or an Atom feed. A page that merely mentions a feed
// element, or one that holds it below an html root, is not.
func SniffFeed(body []byte) bool {
	head := bytes.TrimLeft(bytes.TrimPrefix(body, []byte("\xef\xbb\xbf")), " \t\r\n")
	if len(head) == 0 || head[0] != '<' {
		return false
	}
	if len(head) > sniffWindow {
		head = head[:sniffWindow]
	}
	root, ok := firstStartElement(xmlDecoder(head))
	return ok && feedRoot(root)
}

func feedRoot(n xml.Name) bool {
	switch n.Local {
	case "rss":
		return true
	case "RDF":
		return true
	case "feed":
		return n.Space == atomNS
	}
	return false
}

func firstStartElement(d *xml.Decoder) (xml.Name, bool) {
	for {
		tok, err := d.RawToken()
		if err != nil {
			return xml.Name{}, false
		}
		if se, ok := tok.(xml.StartElement); ok {
			// RawToken does not translate namespaces: read the default one off the element itself
			name := se.Name
			for _, a := range se.Attr {
				if a.Name.Local == "xmlns" && a.Name.Space == "" {
					name.Space = a.Value
				}
			}
			return name, true
		}
	}
}

func xmlDecoder(b []byte) *xml.Decoder {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = false
	d.Entity = xml.HTMLEntity
	d.CharsetReader = feedCharsetReader
	return d
}

// feedCharsetReader decodes the few single-byte encodings a feed still declares.
// Anything else is an error, and the page falls back to the ordinary text path.
func feedCharsetReader(label string, in io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "utf-8", "utf8", "us-ascii", "ascii", "":
		return in, nil
	case "iso-8859-1", "latin1", "latin-1", "windows-1252", "cp1252":
		raw, err := io.ReadAll(io.LimitReader(in, MaxFetchBytes+1))
		if err != nil {
			return nil, err
		}
		var b strings.Builder
		for _, c := range raw {
			b.WriteRune(rune(c))
		}
		return strings.NewReader(b.String()), nil
	}
	return nil, fmt.Errorf("feed encoding %q is not supported", label)
}

// FeedToText turns an RSS 2.0, RSS 1.0 or Atom document into the text a seat is
// handed, and returns the feed's own title. It is a pure function of the bytes:
// no link in the document is fetched. A document cut off mid-way (the fetch
// bound) yields the items read so far.
func FeedToText(body []byte) (title, text string, err error) {
	body = trimXMLPrefix(body)
	d := xmlDecoder(body)
	var (
		stack    []string
		rootSeen bool
		items    []feedItem
		total    int
		cut      bool
	)
	for {
		tok, terr := d.Token()
		if terr != nil {
			if !rootSeen {
				return "", "", fmt.Errorf("not a feed: %w", terr)
			}
			if !errors.Is(terr, io.EOF) {
				cut = true
			}
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if !rootSeen {
				if !feedRoot(t.Name) {
					return "", "", fmt.Errorf("not a feed: root element is %q", t.Name.Local)
				}
				rootSeen = true
			}
			parent := ""
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			switch {
			case (t.Name.Local == "item" || t.Name.Local == "entry") && (parent == "channel" || parent == "feed" || parent == "rss" || parent == "RDF"):
				total++
				if len(items) >= MaxFeedItems {
					if serr := d.Skip(); serr != nil {
						cut = true
					}
					continue
				}
				var it feedItem
				if derr := d.DecodeElement(&it, &t); derr != nil {
					cut = true
					goto done
				}
				items = append(items, it)
				continue
			case t.Name.Local == "title" && title == "" && (parent == "channel" || parent == "feed"):
				var s string
				if derr := d.DecodeElement(&s, &t); derr != nil {
					cut = true
					goto done
				}
				title = inline(s, maxFeedFieldChars)
				continue
			}
			stack = append(stack, t.Name.Local)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
done:
	if !rootSeen {
		return "", "", errors.New("not a feed: no root element")
	}
	var b strings.Builder
	if title != "" {
		fmt.Fprintf(&b, "Feed: %s\n", title)
	} else {
		b.WriteString("Feed: (untitled)\n")
	}
	switch {
	case total > len(items):
		fmt.Fprintf(&b, "Items: %d of %d items shown (the rest are not included)\n", len(items), total)
	case cut:
		fmt.Fprintf(&b, "Items: %d (the feed document was cut off here)\n", len(items))
	default:
		fmt.Fprintf(&b, "Items: %d\n", len(items))
	}
	for i, it := range items {
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, firstNonEmpty(inlineAll(it.Titles, maxFeedFieldChars), "(untitled)"))
		if l := itemLink(it); l != "" {
			fmt.Fprintf(&b, "Link: %s\n", l)
		}
		if dt := inline(firstNonEmptyOf(it.PubDates, it.Published, it.Dates, it.Updated), maxFeedFieldChars); dt != "" {
			fmt.Fprintf(&b, "Date: %s\n", dt)
		}
		if sum := summaryOf(it); sum != "" {
			b.WriteString(sum)
			b.WriteByte('\n')
		}
	}
	text = normalizeWhitespace(b.String())
	if len(text) > TextCap {
		text = cutBytes(text, TextCap)
	}
	return title, text, nil
}

// itemLink picks the item's page link: RSS carries it as text, Atom as an href
// whose rel is alternate or absent (an enclosure or self link is not the item).
func itemLink(it feedItem) string {
	for _, l := range it.Links {
		if v := strings.TrimSpace(l.Text); v != "" {
			return inline(v, maxFeedFieldChars)
		}
	}
	for _, l := range it.Links {
		if l.Href != "" && (l.Rel == "" || l.Rel == "alternate") {
			return inline(l.Href, maxFeedFieldChars)
		}
	}
	for _, g := range it.GUIDs {
		if g = strings.TrimSpace(g); strings.HasPrefix(g, "http://") || strings.HasPrefix(g, "https://") {
			return inline(g, maxFeedFieldChars)
		}
	}
	return ""
}

func summaryOf(it feedItem) string {
	raw := firstNonEmptyOf(it.Summaries, it.Descriptions, it.Contents, it.Encoded)
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	_, t := HTMLToText(raw)
	return cutRunes(strings.TrimSpace(t), MaxFeedSummaryChars)
}

func firstNonEmptyOf(lists ...[]string) string {
	for _, l := range lists {
		for _, s := range l {
			if strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func inlineAll(l []string, max int) string { return inline(firstNonEmptyOf(l), max) }

// inline renders a field as one short line: markup stripped, whitespace folded.
func inline(s string, max int) string {
	if strings.ContainsAny(s, "<&") {
		_, s = HTMLToText(s)
	}
	return cutRunes(strings.Join(strings.Fields(s), " "), max)
}

// cutBytes cuts s to at most max bytes at a rune boundary.
func cutBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	n := max
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// cutRunes is cutBytes with the cut marked.
func cutRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(cutBytes(s, max)) + "..."
}

// trimXMLPrefix drops a UTF-8 byte-order mark and leading whitespace.
func trimXMLPrefix(b []byte) []byte {
	return bytes.TrimLeft(bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf}), " \t\r\n")
}
