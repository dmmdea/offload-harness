package mcpserver

import (
	"fmt"
	"regexp"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// deferredTrue is the literal a caller reads in a description to learn that a lane can hand the work back.
var deferredTrue = regexp.MustCompile(`deferred:\s*true`)

// sentencesWith counts the sentences of s that hold the literal, as the production splitter cuts them.
func sentencesWith(re *regexp.Regexp, s string) int {
	n := 0
	for _, sent := range splitSentences(s) {
		if re.MatchString(sent) {
			n++
		}
	}
	return n
}

// deferralLosses compares one tool's full and compact forms: for every description (the tool's own and each parameter's, at any
// depth) that says `deferred:true`, the compact one says it as many times, in as many sentences. It returns the mentions it
// compared and one message per loss.
func deferralLosses(ft, ct *mcp.Tool) (compared int, losses []string) {
	fd, cd := descriptionsByPath(ft), descriptionsByPath(ct)
	paths := make([]string, 0, len(fd))
	for p := range fd {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		fn, cn := len(deferredTrue.FindAllString(fd[p], -1)), len(deferredTrue.FindAllString(cd[p], -1))
		if fn == 0 {
			continue
		}
		compared += fn
		if fn != cn {
			losses = append(losses, fmt.Sprintf("%s %s: `deferred:true` is said %d time(s) in the full description and %d in the compact one", ft.Name, p, fn, cn))
		}
		if fs, cs := sentencesWith(deferredTrue, fd[p]), sentencesWith(deferredTrue, cd[p]); fs != cs {
			losses = append(losses, fmt.Sprintf("%s %s: %d deferral sentence(s) in the full description, %d in the compact one", ft.Name, p, fs, cs))
		}
	}
	return compared, losses
}

// TestCompactKeepsTheDeferralProtocolOfEveryTool: a lane that cannot do the work returns deferred:true and the caller does the
// work itself. Every description that says so in the full list says so in the compact one, as many times: the count of the
// literal `deferred:true`, and of the sentences that hold it, is the same in the compact description of every tool and every
// parameter as in the full one, on every surface. A tool whose deferral sentence is cut leaves the caller to read an empty
// answer as an answer.
func TestCompactKeepsTheDeferralProtocolOfEveryTool(t *testing.T) {
	checked, tools := 0, map[string]bool{}
	for _, surface := range gateSurfaces() {
		compact := map[string]*mcp.Tool{}
		for _, tool := range listTools(t, surface.cfg("compact")) {
			compact[tool.Name] = tool
		}
		for _, ft := range listTools(t, surface.cfg("full")) {
			n, losses := deferralLosses(ft, compact[ft.Name])
			checked += n
			if n > 0 {
				tools[ft.Name] = true
			}
			for _, l := range losses {
				t.Errorf("%s: %s", surface.name, l)
			}
		}
	}
	if checked < 40 || len(tools) < 25 {
		t.Fatalf("only %d mentions of deferred:true in %d tools were compared: the check is blind", checked, len(tools))
	}
}

// TestDeferralComparisonSeesALoss proves the comparison is not vacuous: a pair whose compact form lost a deferral sentence, or
// a deferral mention inside a sentence it kept, is reported, and an identical pair is not.
func TestDeferralComparisonSeesALoss(t *testing.T) {
	full := &mcp.Tool{Name: "synthetic_defer_tool", Description: "Do the first useful thing for the caller here. Returns {x}. On any failure it returns deferred:true and you do it yourself."}
	cut := &mcp.Tool{Name: full.Name, Description: "Do the first useful thing for the caller here."}
	if n, losses := deferralLosses(full, cut); n != 1 || len(losses) != 2 {
		t.Fatalf("a dropped deferral sentence was not reported (compared %d, losses %q)", n, losses)
	}
	if n, losses := deferralLosses(full, full); n != 1 || len(losses) != 0 {
		t.Fatalf("an identical pair was reported (compared %d, losses %q)", n, losses)
	}
	twice := &mcp.Tool{Name: full.Name, Description: "Do the first useful thing for the caller here. It returns deferred:true when it cannot, and again deferred:true when busy."}
	once := &mcp.Tool{Name: full.Name, Description: "Do the first useful thing for the caller here. It returns deferred:true when it cannot."}
	if _, losses := deferralLosses(twice, once); len(losses) != 1 {
		t.Fatalf("a mention lost inside a kept sentence was not reported: %q", losses)
	}
}
