package research

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
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

// xmlNS is the namespace the xml: prefix (xml:base) stands for.
const xmlNS = "http://www.w3.org/XML/1998/namespace"

type feedLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Base string `xml:"http://www.w3.org/XML/1998/namespace base,attr"`
	Text string `xml:",chardata"`
}

// feedItem is one RSS <item> or Atom <entry>. Repeated elements are slices so a
// namespaced twin (media:title, media:content) cannot overwrite the real field.
type feedItem struct {
	Base         string     `xml:"http://www.w3.org/XML/1998/namespace base,attr"`
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

const (
	atomNS = "http://www.w3.org/2005/Atom"
	rss1NS = "http://purl.org/rss/1.0/"
)

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
	// the window may end inside a character, and a comment before the root may hold a stray byte: neither
	// is a reason to call the document something else
	root, ok := firstStartElement(xmlDecoder(cleanXMLText(head)))
	return ok && feedRoot(root)
}

// feedRoot reports whether a document's root element is a feed's: an rss element, an Atom feed
// (the Atom namespace is the element's own or the default one it declares), or an RDF root that
// declares the RSS 1.0 namespace. A bare rdf:RDF is not enough: RDF/XML, OWL and SKOS files are
// rooted there too and are not feeds.
func feedRoot(se xml.StartElement) bool {
	switch se.Name.Local {
	case "rss":
		return true
	case "RDF":
		return declaresNS(se, rss1NS)
	case "feed":
		return se.Name.Space == atomNS || declaresDefaultNS(se, atomNS)
	}
	return false
}

// declaresDefaultNS reports whether the element carries xmlns="<ns>". RawToken does not translate
// namespaces, so the declaration is read off the element itself.
func declaresDefaultNS(se xml.StartElement, ns string) bool {
	for _, a := range se.Attr {
		if a.Name.Space == "" && a.Name.Local == "xmlns" && a.Value == ns {
			return true
		}
	}
	return false
}

// declaresNS is declaresDefaultNS or a prefixed declaration (xmlns:p="<ns>").
func declaresNS(se xml.StartElement, ns string) bool {
	if declaresDefaultNS(se, ns) {
		return true
	}
	for _, a := range se.Attr {
		if a.Name.Space == "xmlns" && a.Value == ns {
			return true
		}
	}
	return false
}

func firstStartElement(d *xml.Decoder) (xml.StartElement, bool) {
	for {
		tok, err := d.RawToken()
		if err != nil {
			return xml.StartElement{}, false
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se, true
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

// feedEncoding is what a charset label means to this parser.
type feedEncoding int

const (
	encUnknown feedEncoding = iota
	// encUTF8 is UTF-8 and plain ASCII, which are read as they are (invalid bytes are replaced first).
	encUTF8
	// encWindows1252 is windows-1252, and the labels the web platform reads as it: browsers decode iso-8859-1 and
	// latin1 as windows-1252, because bytes 0x80-0x9F are curly quotes, the euro sign and dashes in every document
	// that uses them and C1 control characters in none.
	encWindows1252
)

// feedEncodingOf classifies a charset label; an empty label is UTF-8, the default of XML.
func feedEncodingOf(label string) feedEncoding {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "utf-8", "utf8", "us-ascii", "ascii", "":
		return encUTF8
	case "iso-8859-1", "iso8859-1", "iso_8859-1", "latin1", "latin-1", "l1", "windows-1252", "cp1252", "x-cp1252":
		return encWindows1252
	}
	return encUnknown
}

// feedCharsetReader decodes the few single-byte encodings a feed still declares.
// Anything else is an error, and the page falls back to the ordinary text path.
func feedCharsetReader(label string, in io.Reader) (io.Reader, error) {
	switch feedEncodingOf(label) {
	case encUTF8:
		return in, nil
	case encWindows1252:
		return charmap.Windows1252.NewDecoder().Reader(io.LimitReader(in, MaxFetchBytes+1)), nil
	}
	return nil, fmt.Errorf("feed encoding %q is not supported", label)
}

// passThroughCharset is the decoder's charset reader once the body has already been turned into UTF-8: the
// encoding the document declares is no longer the encoding of the bytes, and decoding twice would garble them.
func passThroughCharset(_ string, in io.Reader) (io.Reader, error) { return in, nil }

var reDeclaredEncoding = regexp.MustCompile(`(?i)^<\?xml[^>]*?encoding\s*=\s*["']([^"']+)["']`)

// declaredEncoding is the encoding the XML declaration names, "" when there is none.
func declaredEncoding(body []byte) string {
	head := body
	if len(head) > 256 {
		head = head[:256]
	}
	if m := reDeclaredEncoding.FindSubmatch(head); m != nil {
		return string(m[1])
	}
	return ""
}

// cleanXMLText makes UTF-8 bytes the XML parser can read to the end: a byte or sequence that is not valid UTF-8
// becomes U+FFFD, and a character XML forbids (a control character other than tab, line feed and carriage return,
// U+FFFE and U+FFFF) becomes a space. Either one would otherwise stop encoding/xml at that item, silently, and
// every later item would be lost.
func cleanXMLText(b []byte) []byte {
	b = bytes.ToValidUTF8(b, []byte(string(utf8.RuneError)))
	return bytes.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return r
		case r < 0x20:
			return ' '
		case r == 0xFFFE || r == 0xFFFF:
			return ' '
		}
		return r
	}, b)
}

// prepareFeedBody turns a feed response body into UTF-8 text the parser can read to the end. The encoding is the
// one the HTTP Content-Type names when it names one this parser knows (HTTP is authoritative for the bytes it sends),
// else the one the XML declaration names, else UTF-8.
func prepareFeedBody(body []byte, httpCharset string) ([]byte, error) {
	body = trimXMLPrefix(body)
	enc := encUnknown
	if strings.TrimSpace(httpCharset) != "" {
		enc = feedEncodingOf(httpCharset)
	}
	if enc == encUnknown {
		label := declaredEncoding(body)
		if enc = feedEncodingOf(label); enc == encUnknown {
			return nil, fmt.Errorf("feed encoding %q is not supported", label)
		}
	}
	if enc == encWindows1252 {
		dec, err := charmap.Windows1252.NewDecoder().Bytes(body)
		if err != nil {
			return nil, fmt.Errorf("feed is not windows-1252: %w", err)
		}
		body = dec
	}
	return cleanXMLText(body), nil
}

// FeedToText turns an RSS 2.0, RSS 1.0 or Atom document into the text a seat is
// handed, and returns the feed's own title. It is a pure function of the bytes:
// no link in the document is fetched. A document cut off mid-way (the fetch
// bound) yields the items read so far. A document with no item or entry is an
// error, so an empty feed or an RDF file that is not RSS stays an ordinary document.
func FeedToText(body []byte) (title, text string, err error) {
	r, err := parseFeed(body, "", nil)
	return r.Title, r.Text, err
}

// feedParse is a parsed feed. Cut says the parse stopped before the end of the document (the body ended inside
// an element, or a syntax error ended it), so the items after that point are missing.
type feedParse struct {
	Title string
	Text  string
	Cut   bool
}

// feedEntry is one decoded item with the base its relative links resolve against.
type feedEntry struct {
	feedItem
	base *url.URL
}

// xmlBaseOf is the xml:base an element carries, "" when it has none.
func xmlBaseOf(se xml.StartElement) string {
	for _, a := range se.Attr {
		if a.Name.Local == "base" && (a.Name.Space == xmlNS || a.Name.Space == "xml") {
			return strings.TrimSpace(a.Value)
		}
	}
	return ""
}

// resolveBase applies an xml:base value to the base in force: an absolute value replaces it, a relative one is
// resolved against it. An unparsable value changes nothing.
func resolveBase(base *url.URL, ref string) *url.URL {
	if ref == "" {
		return base
	}
	u, err := url.Parse(ref)
	if err != nil {
		return base
	}
	if base == nil {
		return u
	}
	return base.ResolveReference(u)
}

// parseFeed is FeedToText with the HTTP charset (empty when the response named none) and the address the document
// was fetched from (nil when unknown), the base its relative item links resolve against.
func parseFeed(body []byte, httpCharset string, docBase *url.URL) (feedParse, error) {
	body, perr := prepareFeedBody(body, httpCharset)
	if perr != nil {
		return feedParse{}, perr
	}
	d := xmlDecoder(body)
	d.CharsetReader = passThroughCharset // the body is UTF-8 now, whatever the declaration says
	var (
		stack    []string
		bases    = []*url.URL{docBase} // the xml:base in force, one entry per element on stack, plus the document's
		rootSeen bool
		title    string
		items    []feedEntry
		total    int
		cut      bool
	)
	for {
		tok, terr := d.Token()
		if terr != nil {
			if !rootSeen {
				return feedParse{}, fmt.Errorf("not a feed: %w", terr)
			}
			// an error with the root still open is a document that ended early; after the root closed it is trailing junk
			if !errors.Is(terr, io.EOF) && len(stack) > 0 {
				cut = true
			}
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if !rootSeen {
				if !feedRoot(t) {
					return feedParse{}, fmt.Errorf("not a feed: root element is %q", t.Name.Local)
				}
				rootSeen = true
			}
			parent := ""
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			switch {
			case (t.Name.Local == "item" || t.Name.Local == "entry") && (parent == "channel" || parent == "feed" || parent == "rss" || parent == "RDF"):
				if len(items) >= MaxFeedItems {
					// past the cap an item is only counted, and counted once it has been read to its end
					if serr := d.Skip(); serr != nil {
						cut = true
						goto done
					}
					total++
					continue
				}
				var it feedItem
				if derr := d.DecodeElement(&it, &t); derr != nil {
					cut = true
					goto done
				}
				total++
				items = append(items, feedEntry{feedItem: it, base: bases[len(bases)-1]})
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
			bases = append(bases, resolveBase(bases[len(bases)-1], xmlBaseOf(t)))
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
				bases = bases[:len(bases)-1]
			}
		}
	}
done:
	if !rootSeen {
		return feedParse{}, errors.New("not a feed: no root element")
	}
	if len(items) == 0 {
		// A root that holds no item is not a source: Fetch reads the document as it always did
		return feedParse{}, errors.New("not a feed: no item or entry found")
	}
	var b strings.Builder
	if title != "" {
		fmt.Fprintf(&b, "Feed: %s\n", title)
	} else {
		b.WriteString("Feed: (untitled)\n")
	}
	// A cut is said first: the count of a document that stopped early is a count of what was read, never of the feed.
	switch {
	case cut && total > len(items):
		fmt.Fprintf(&b, "Items: %d of at least %d items shown (the feed document was cut off here; the rest are not included)\n", len(items), total)
	case cut:
		fmt.Fprintf(&b, "Items: %d (the feed document was cut off here; later items are not included)\n", len(items))
	case total > len(items):
		fmt.Fprintf(&b, "Items: %d of %d items shown (the rest are not included)\n", len(items), total)
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
		if sum := summaryOf(it.feedItem); sum != "" {
			b.WriteString(sum)
			b.WriteByte('\n')
		}
	}
	text := normalizeWhitespace(b.String())
	if len(text) > TextCap {
		text = cutBytes(text, TextCap)
	}
	return feedParse{Title: title, Text: text, Cut: cut}, nil
}

// itemLink picks the item's page link: RSS carries it as text, Atom as an href
// whose rel is alternate or absent (an enclosure or self link is not the item).
// A relative link is resolved against the base in force where it appears: the
// link's own xml:base, the item's, the enclosing elements', and last the
// address the feed was fetched from. Nothing here is fetched.
func itemLink(it feedEntry) string {
	itemBase := resolveBase(it.base, strings.TrimSpace(it.Base))
	resolve := func(l feedLink, raw string) string {
		raw = strings.TrimSpace(raw)
		u, err := url.Parse(raw)
		if err != nil || u.IsAbs() {
			return raw
		}
		base := resolveBase(itemBase, strings.TrimSpace(l.Base))
		if base == nil {
			return raw
		}
		return base.ResolveReference(u).String()
	}
	for _, l := range it.Links {
		if v := strings.TrimSpace(l.Text); v != "" {
			return inline(resolve(l, v), maxFeedFieldChars)
		}
	}
	for _, l := range it.Links {
		if l.Href != "" && (l.Rel == "" || l.Rel == "alternate") {
			return inline(resolve(l, l.Href), maxFeedFieldChars)
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
