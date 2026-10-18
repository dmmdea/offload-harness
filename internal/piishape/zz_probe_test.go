package piishape

import (
	"regexp/syntax"
	"strings"
	"testing"
	"time"
)

func TestZZProbeSizeAndTime(t *testing.T) {
	re := cookieClass().Re
	sre, err := syntax.Parse(cookieHeader, syntax.Perl)
	if err != nil {
		t.Fatal(err)
	}
	prog, err := syntax.Compile(sre.Simplify())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("program instructions: %d", len(prog.Inst))
	timeIt := func(name, s string) {
		t0 := time.Now()
		re.FindAllStringIndex(s, -1)
		d := time.Since(t0)
		t.Logf("%-28s %7d bytes: %8.1f ms  (%.2f s/MiB)", name, len(s), float64(d.Microseconds())/1000, d.Seconds()*float64(1<<20)/float64(len(s)))
	}
	rep := func(unit string, n int) string { return strings.Repeat(unit, n/len(unit)+1)[:n] }
	u14 := "t-" + ckName + `:['\="`
	u24 := ckName + `:[\="` + "0123456789ab" + `"`
	for _, n := range []int{4 << 10, 8 << 10, 16 << 10} {
		timeIt("14-byte unit", rep(u14, n))
	}
	for _, n := range []int{4 << 10, 8 << 10, 16 << 10} {
		timeIt("24-byte unit", rep(u24, n))
	}
	bs := "\\"
	toks := []string{ckName + ": ", `" => [`, `"`, `=` + strings.Repeat(bs, 15) + `"`, `: `, `a=`, `]`, strings.Repeat(bs, 4), `u0026`, `|`, sckName + `":[`, strings.Repeat(bs, 3) + `"`, `'`, bs, `=` + strings.Repeat(bs, 3) + `"`, `]`, `[]string{`}
	u240 := strings.Join(toks, "")
	for _, n := range []int{4 << 10, 16 << 10} {
		timeIt("mixed unit", rep(u240, n))
	}
	f4 := "'a=" + strings.Repeat(bs, 7) + `"a`
	timeIt("F4 unit 256 KiB", ckName+":["+rep(f4, 256<<10))
	timeIt("benign go-like 256 KiB", rep("func f(x int) string { return fmt.Sprintf(\"%d: %s\", x, name) }\n", 256<<10))
}
