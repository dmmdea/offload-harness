package research

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// forumHTML is a realistic Q&A / forum page: real content wrapped in the class
// names, ids, handles, icon names, script text and share/follow/helpful UI
// strings a fetched page carries. Regression source: research contracts built
// their acceptance from exactly these tokens (register C-65).
const forumHTML = `<html><head><title>Phone will not reach full charge speed after the update</title></head><body>
<div class="lightbox-image-container wp--lightbox-image-height" id="AdThrive_Sidebar_1_desktop">
<span class="show-closing-animation icon-facebook-f">Facebook-f</span> <a href="/u/handle56125">handle56125</a>
<script>document.addEventListener('click', function(){ lightbox(); })</script>
<h1>Phone will not reach full charge speed after the update</h1>
<p>After installing the latest firmware the battery only reaches a slow trickle charge, even with the original charger and cable. Battery health reports normal capacity.</p>
<p>The fix that worked for most owners was to clear the charging controller cache from recovery mode, then restart and let the battery drain completely before charging again.</p>
<h2>Why the charging speed drops</h2>
<p>The update resets the charging controller calibration, so the device falls back to a conservative current limit until the battery gauge relearns its capacity.</p>
<div>Was this helpful? Follow Share Reply Anonymous</div>
<div class="prefers-reduced-motion">Follow this question</div>
<div>Follow question anonymous helpful helpful helpful helpful follow follow question question</div>
<a href="/threads/vendor-ix-apex-ln2-guide-for-all-scenarios.4711/">vendor-ix-apex-ln2-guide-for-all-scenar</a>
<div>Helpful helpful follow question anonymous detected windows follow question</div>
</div></body></html>`

func forumText(t *testing.T) string {
	t.Helper()
	_, text := HTMLToText(forumHTML)
	return text
}

// Regression tokens: the shapes the old miner chose on chrome-heavy pages
// (CSS classes, sidebar ids, script names, handles, a slug cut at 40
// characters, icon names, and UI words that dominate forum pages).
var chromeTokens = []string{
	"lightbox-image-container", "wp--lightbox-image-height", "adthrive_sidebar_1_desktop", "adthrive",
	"show-closing-animation", "prefers-reduced-motion", "addeventlistener", "handle56125",
	"vendor-ix-apex-ln2-guide-for-all-scenar", "facebook-f", "facebook", "helpful", "follow", "question",
	"anonymous", "detected", "windows", "container", "sidebar", "lightbox",
}

func anchorSet(t *testing.T, text, goal string) []string {
	t.Helper()
	chk := AnchorCheck(text, goal)
	if chk == "" {
		t.Fatalf("no anchor check for a page with real prose:\n%s", text)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(chk, "regex:(?i)("), ")")
	inner = strings.TrimPrefix(inner, "?P<docanchor>")
	return strings.Split(inner, "|")
}

func TestAnchorNeverChosenFromChrome(t *testing.T) {
	text := forumText(t)
	got := anchorSet(t, text, "Summarize the fix")
	plain := regexp.MustCompile(`^[a-z]{6,24}$`)
	for _, g := range got {
		for _, bad := range chromeTokens {
			if strings.EqualFold(g, bad) {
				t.Fatalf("chrome token %q chosen as an anchor: %v", g, got)
			}
		}
		if !plain.MatchString(g) {
			t.Fatalf("anchor %q is not a whole plain word: %v", g, got)
		}
	}
	joined := " " + strings.Join(got, " ") + " "
	if !strings.Contains(joined, " charging ") && !strings.Contains(joined, " battery ") {
		t.Fatalf("real content words missing from anchors: %v", got)
	}
}

func TestAnchorIsOneAnyOfCheck(t *testing.T) {
	specs, _ := Build(Request{Goal: "Summarize the fix"}, []Fetched{{URL: "https://x/q", Text: forumText(t)}})
	acc := specs[0].AgentContract.Acceptance
	regexes := 0
	for _, a := range acc {
		if strings.HasPrefix(a, "regex:") {
			regexes++
			if !strings.Contains(a, "(?P<docanchor>") {
				t.Fatalf("the page check must keep the docanchor tag (quarantine keys on it): %q", a)
			}
		}
	}
	if regexes != 1 {
		t.Fatalf("want exactly ONE page regex (any-of), got %d in %v", regexes, acc)
	}
}

func TestFaithfulSummaryPassesOffTopicFails(t *testing.T) {
	specs, _ := Build(Request{Goal: "Summarize the fix"}, []Fetched{{URL: "https://x/q", Text: forumText(t)}})
	contract := specs[0].AgentContract
	wire := func(out string) core.AgentWireResult {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: out, Structured: json.RawMessage(out)}
	}
	faithful := `{"summary":"Clearing the charging controller cache from recovery mode, then draining the battery, restores the speed.","key_facts":["The update resets calibration"],"verdict":"the cache clear restores the speed"}`
	if fails := delegate.EvalAcceptance(contract, wire(faithful)); len(fails) != 0 {
		t.Fatalf("a faithful summary must pass: %v (acceptance=%v)", fails, contract.Acceptance)
	}
	if fails := delegate.EvalAcceptance(contract, wire(`{"summary":"The firmware resets the calibration so the current is limited.","key_facts":["Slow trickle charge"],"verdict":"the update limits the charge current"}`)); len(fails) != 0 {
		t.Fatalf("second faithful phrasing must pass: %v (acceptance=%v)", fails, contract.Acceptance)
	}
	offTopic := `{"summary":"The latest stable Go release is available from the download page; follow the installation guide.","key_facts":["Go 1.26"],"verdict":"a new release is out"}`
	if fails := delegate.EvalAcceptance(contract, wire(offTopic)); len(fails) == 0 {
		t.Fatalf("an off-topic answer must fail; acceptance=%v", contract.Acceptance)
	}
	chromeOnly := `{"summary":"Helpful, follow, question, anonymous; lightbox-image-container.","key_facts":["helpful"],"verdict":"helpful"}`
	if fails := delegate.EvalAcceptance(contract, wire(chromeOnly)); len(fails) == 0 {
		t.Fatalf("restating only chrome must fail; acceptance=%v", contract.Acceptance)
	}
}

const markdownPage = `# Retiming video with ffmpeg

[Home](/) | [Docs](/docs) | [Share](#) | [Follow](#)

Changing playback speed means rewriting presentation timestamps: the setpts filter rescales every frame, and the atempo filter stretches the audio so it stays in sync.

## Keeping the framerate constant

Add an fps filter after setpts so duplicated or dropped frames land on a stable timeline, and reset the timestamps before concatenation.

.wp-block-image_container { display: flex; } const handleClick = () => window.open(url);
icon-arrow-right icon-share-alt btn-primary navbar-toggler
Was this helpful? Yes No
`

func TestMarkdownPageAnchorsComeFromProse(t *testing.T) {
	got := anchorSet(t, markdownPage, "Explain how to retime")
	for _, g := range got {
		for _, bad := range []string{"navbar", "toggler", "primary", "handleclick", "container", "helpful", "arrow", "share"} {
			if strings.Contains(g, bad) {
				t.Fatalf("chrome token %q chosen: %v", g, got)
			}
		}
	}
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "timestamps") && !strings.Contains(joined, "framerate") {
		t.Fatalf("expected content words, got %v", got)
	}
}

func TestAnchorNeverComesFromTheGoalAndNeverTruncates(t *testing.T) {
	text := "The transporter bundles a handle for every buffer.\nThe transporter is small and the transporter is fast, while checkpointing copies buffers repeatedly through pinned staging memory."
	for _, g := range anchorSet(t, text, "Explain the transporter and checkpointing") {
		if g == "transporter" || g == "checkpointing" {
			t.Fatalf("goal word %q chosen: parrot-passable", g)
		}
	}
	const giant = "supercalifragilisticexpialidocious"
	long := "Considerations about internationalization appear here: " + giant + " " + giant + " in the surrounding paragraph text of the page."
	for w := range proseWords(long) {
		if len(w) > maxWordLen {
			t.Fatalf("over-long token %q must be skipped, not cut", w)
		}
		if w != giant && strings.HasPrefix(giant, w) {
			t.Fatalf("token %q looks truncated", w)
		}
	}
}

func TestThinOrChromeOnlyPageHasNoAnchor(t *testing.T) {
	if got := AnchorCheck("Hello world.\nShort page.", "x"); got != "" {
		t.Fatalf("thin page must yield no check, got %q", got)
	}
	chrome := "lightbox-image-container wp--lightbox-image-height AdThrive_Sidebar_1_desktop\naddEventListener show-closing-animation\nHelpful Follow Share Reply Anonymous Question"
	if got := AnchorCheck(chrome, "x"); got != "" {
		t.Fatalf("a page of chrome alone must yield no check, got %q", got)
	}
}
