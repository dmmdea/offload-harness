package piishape

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// The shapes and wrappers here are the axes along which the whole-span class was found to break after it was
// first written. Each one is a way a real line differs from the lines the first generator built:
//
//   - a last cookie that ends in an equals sign (base64 padding, or no value at all), right in front of the quote
//     that closes the string the header sits in, with another field or argument behind it;
//   - a bare value with a shell escape (a dollar sign, a backtick) or a backslash in it;
//   - a quoted value with a line break in it, and one that is never closed (a truncated line);
//   - a Go header printed with the Go-syntax verb, a list opened by an equals sign or a hash rocket, and a list that
//     is cut in its last element;
//   - text that has been escaped three and four times (JSON in a shell string in JSON).

// openEndShapes are the shapes of a quoted value that is never closed.
var openEndShapes = map[string]bool{
	"truncated quoted body with a space": true,
	"truncated quoted body":              true,
}

// reviewShapes are the cookies of those axes, built from pieces at run time like the others.
func reviewShapes() []cookieShape {
	al := func(seed, n int) string { return valueFixture(seed, n, alnum) }
	A, B := al(831, 16), al(832, 16)
	return []cookieShape{
		{"short, base64 padding", "p=QUJD==", nil, false},
		{"short, one padding sign", "p=QUJD=", nil, false},
		{"empty last cookie", "flag=", nil, false},
		{"empty quoted value", `e=""`, nil, false},
		{"bare ending in padding", "t=" + A + B + "==", []string{A, B}, true},
		{"bare with a dollar sign", "d=" + A + "$HOME" + B, []string{A, B}, true},
		{"bare with a backtick", "d=" + A + "`id`" + B, []string{A, B}, true},
		{"bare with a backslash path", "d=" + A + `\users\x` + B, []string{A, B}, true},
		{"quoted body with a line feed", `m="` + A + "\n" + B + `"`, []string{A, B}, true},
		{"quoted body with a carriage return and line feed", `m="` + A + "\r\n" + B + `"`, []string{A, B}, true},
		{"quoted body with a fold", `m="` + A + "\r\n " + B + `"`, []string{A, B}, true},
		{"truncated quoted body with a space", `m="` + A + " " + B, []string{A, B}, true},
		{"truncated quoted body", `m="` + A + B, []string{A, B}, true},
	}
}

// escLayer is one layer of escaping: how a string is escaped to stand inside it, and how a text is wrapped in it.
type escLayer struct {
	name string
	esc  func(string) string
	wrap func(string) string
}

var (
	jsonLayer = escLayer{"json", enc, func(s string) string {
		b, _ := json.Marshal(map[string]string{"x": s})
		return string(b)
	}}
	shellLayer = escLayer{"shell", shellEsc, func(s string) string {
		return `curl -d "` + shellEsc(s) + `" https://host.invalid/p`
	}}
)

// layeredWrapper wraps the line in the layers, the first one innermost, and says what the span is after the
// same escaping: the quotes of a cookie value then carry 1, 3, 7 or 15 backslashes.
func layeredWrapper(layers ...escLayer) cookieWrapper {
	names := make([]string, len(layers))
	for i, l := range layers {
		names[i] = l.name
	}
	return cookieWrapper{
		name:       "layers " + strings.Join(names, "+"),
		envelope:   layers[len(layers)-1].name == "json",
		openOK:     true,
		fourLayers: len(layers) >= 4,
		build: func(body string, _ []string) (string, string) {
			w := ckName + ": " + body
			text, want := "one\n"+w+"\nthree", w
			for _, l := range layers {
				text, want = l.wrap(text), l.esc(want)
			}
			return text, want
		},
	}
}

// layerChains are the chains of two and three layers, and four of four.
func layerChains() [][]escLayer {
	var out [][]escLayer
	var grow func(chain []escLayer, n int)
	grow = func(chain []escLayer, n int) {
		if len(chain) == n {
			out = append(out, append([]escLayer{}, chain...))
			return
		}
		for _, l := range []escLayer{jsonLayer, shellLayer} {
			grow(append(chain, l), n)
		}
	}
	grow(nil, 2)
	grow(nil, 3)
	out = append(out,
		[]escLayer{jsonLayer, jsonLayer, jsonLayer, jsonLayer},
		[]escLayer{shellLayer, shellLayer, shellLayer, shellLayer},
		[]escLayer{jsonLayer, shellLayer, jsonLayer, shellLayer},
		[]escLayer{shellLayer, jsonLayer, shellLayer, jsonLayer})
	return out
}

// goHeaderSpan is the span of a header printed with the Go-syntax verb: from the name to the quote that closes the
// last element, so the closing brace of the slice and the one of the map are out.
func goHeaderSpan(t string) string { return t[strings.Index(t, sckName) : len(t)-2] }

// reviewWrappers are the wrappers of those axes.
func reviewWrappers() []cookieWrapper {
	ws := []cookieWrapper{
		{name: "args, header last in its string", envelope: true, openOK: true, build: func(body string, _ []string) (string, string) {
			w := ckName + ": " + body
			b, _ := json.Marshal(map[string]string{"content": "GET /\n" + w, "path": "/x"})
			return string(b), enc(w)
		}},
		{name: "json object, cookie value last", openOK: true, build: func(body string, _ []string) (string, string) {
			w := ckName + `":"` + enc(body)
			return `{"headers":{"` + w + `","Accept":"x"},"url":"u"}`, w
		}},
		{name: "curl, double quotes, then data", openOK: true, build: func(body string, _ []string) (string, string) {
			w := ckName + ": " + shellEsc(body)
			return `curl -H "` + w + `" -d "a=b" -H "X-Next: y" https://host.invalid/p`, w
		}},
		{name: "args of a curl command, then data", envelope: true, openOK: true, build: func(body string, _ []string) (string, string) {
			w := ckName + ": " + shellEsc(body)
			return argsOf(`curl -H "` + w + `" -d "a=b" -H "X-Next: y" https://host.invalid/p`), enc(w)
		}},
		{name: "json list, equals opener", build: func(_ string, elems []string) (string, string) {
			w := sckName + `=[` + quotedList(elems, ",")
			return "headers " + w + "] and more", w
		}},
		{name: "list, hash rocket opener", build: func(_ string, elems []string) (string, string) {
			w := sckName + `" => [` + quotedList(elems, ", ")
			return `{"` + w + `]}`, w
		}},
		{name: "json list cut in its last element", build: func(_ string, elems []string) (string, string) {
			w := sckName + `":[` + quotedList(elems, ",")
			w = w[:len(w)-1]
			return `{"` + w, w
		}},
		{name: "python list cut in its last element", plainOnly: true, build: func(_ string, elems []string) (string, string) {
			w := sckName + `': ['` + strings.Join(elems, `', '`)
			return `{'` + w, w
		}},
		{name: "Go header, printed with the Go-syntax verb", build: func(_ string, elems []string) (string, string) {
			t := fmt.Sprintf("%#v", http.Header{sckName: elems})
			return t, goHeaderSpan(t)
		}},
		{name: "Go map, printed with the Go-syntax verb", build: func(_ string, elems []string) (string, string) {
			t := fmt.Sprintf("%#v", map[string][]string{sckName: elems})
			return t, goHeaderSpan(t)
		}},
		{name: "Go header in a test message", build: func(_ string, elems []string) (string, string) {
			t := fmt.Sprintf("%#v", http.Header{sckName: elems})
			return "    x_test.go:42: headers = " + t + ", want nothing", goHeaderSpan(t)
		}},
		{name: "Go header in args", envelope: true, build: func(_ string, elems []string) (string, string) {
			t := fmt.Sprintf("%#v", http.Header{sckName: elems})
			return argsOf(t), enc(goHeaderSpan(t))
		}},
	}
	for _, chain := range layerChains() {
		ws = append(ws, layeredWrapper(chain...))
	}
	return ws
}
