package research

import (
	"context"
	"strings"
	"testing"
)

// sub_langs reaches the binary as one argument. On Windows the binary may be a .cmd wrapper, which cmd.exe reads, and
// there & | < > ^ % " and a space are not data: the value is checked against a plain language list first, so none of
// them can get that far. (Go refuses some of them for a batch file itself; this does not rely on that.)

var cmdMetacharacters = map[string]string{
	"ampersand": "&", "pipe": "|", "less-than": "<", "greater-than": ">", "caret": "^", "percent": "%",
	"double quote": `"`, "space": " ", "tab": "\t", "newline": "\n", "single quote": "'", "semicolon": ";",
	"dollar": "$", "backtick": "`", "parenthesis": "(", "exclamation mark": "!", "backslash": `\`, "equals": "=",
}

func TestSubLangsPatternRefusesEveryCmdAndShellMetacharacter(t *testing.T) {
	for name, c := range cmdMetacharacters {
		for _, v := range []string{"en" + c, c + "en", "en" + c + "calc", "en,es" + c + "x", c} {
			if reSubLangs.MatchString(v) {
				t.Errorf("%s: %q passes the sub_langs pattern", name, v)
			}
		}
	}
	for _, ok := range []string{"en", "en.*", "es.*,en", "en-US,pt_BR", "all", "en.*,-live_chat", "a", strings.Repeat("e", 64)} {
		if !reSubLangs.MatchString(ok) {
			t.Errorf("%q is a language list and must pass", ok)
		}
	}
	if reSubLangs.MatchString("") || reSubLangs.MatchString(strings.Repeat("e", 65)) {
		t.Errorf("empty and over-long values must not pass the pattern")
	}
}

func TestSubLangsWithAMetacharacterNeverReachesTheBinary(t *testing.T) {
	pinYouTube(t)
	for name, c := range cmdMetacharacters {
		opt, root := ytOpts(t)
		opt.SubLangs = "en" + c + "calc"
		got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idPlain, opt)
		if got.ErrKind != ErrKindBadLangs || got.Text != "" {
			t.Errorf("%s: got %+v, want %s", name, got, ErrKindBadLangs)
		}
		if n := len(readCalls(t, root)); n != 0 {
			t.Errorf("%s: binary invoked %d times", name, n)
		}
	}
}
