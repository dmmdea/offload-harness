package mcpserver

// stale_notice_test.go: the stale-binary notice on every tool result of every door (D-36).
//
// The guard of stalebinary.go reached three doors (agent_delegate, offload_research, offload_status). A session that called
// any other door after a deploy was never told its server runs an old image. These tests run over the BUILT server and an
// in-memory transport, the path a real client takes, so the middleware chain is exercised and not bypassed.

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// staleOtherDoorArgs gives every door that does NOT carry the stale signal itself an argument object of the wrong type: the
// handler turns it into a deferred "bad arguments" result before it touches the pipeline (TestAllHandlersSurfaceBadArguments),
// so a call is cheap, deterministic and still a complete tools/call result for the middleware to annotate.
var staleOtherDoorArgs = map[string]string{
	"offload_summarize":             `{"text":123}`,
	"offload_classify":              `{"text":"x","labels":"not-an-array"}`,
	"offload_extract":               `{"text":"x","schema":"not-an-object"}`,
	"offload_triage":                `{"text":true,"question":"q"}`,
	"offload_vqa":                   `{"image":1,"question":"q"}`,
	"offload_video_describe":        `{"video":[],"question":"q"}`,
	"offload_video_watch":           `{"video":1,"question":"q"}`,
	"offload_transcribe":            `{"audio":{},"hq":"yes"}`,
	"offload_forecast":              `{"series":{},"prediction_length":"x"}`,
	"offload_extract_image":         `{"image":"x","schema":[1,2]}`,
	"offload_assess_image":          `{"image":9}`,
	"offload_ocr":                   `{"image":{}}`,
	"offload_generate_image":        `{"prompt":"p","width":"wide"}`,
	"offload_run_graph":             `{"graph_path":123}`,
	"offload_run_template":          `{"action":5}`,
	"offload_generate_svg":          `{"kind":"gauge","spec":"not-an-object"}`,
	"offload_generate_video":        `{"prompt":"p","frames":"many"}`,
	"offload_animate_character":     `{"ref":1}`,
	"offload_generate_audio":        `{"text":"t","seconds":"ten"}`,
	"offload_edit_image":            `{"image":1,"ops":[]}`,
	"offload_inpaint_image":         `{"image":"a.png","mask":"m.png","prompt":"p","denoise":"deep"}`,
	"offload_upscale_image":         `{"image":"a.png","scale":"double"}`,
	"offload_edit_image_generative": `{"image":"a.png","prompt":"p","steps":"many"}`,
	"offload_media":                 `{"op":["not","a","string"]}`,
	"offload_compose_video":         `{"template":"title-card","variables":"not-an-object"}`,
	"offload_nim":                   `{"prompt":"p","max_tokens":"lots"}`,
	"agent_rig":                     `{"seat":5}`,
	"agent_run":                     `{"goal":"g","max_steps":"all"}`,
	"offload_ask":                   `{"question":"q","paths":"not-an-array"}`,
	"offload_review_diff":           `{"diff":5}`,
}

// staleKeyedDoors are the doors whose handler puts stale_binary / stale_binary_guard in its own answer on the path that runs
// the work. The middleware does not know them by name (a result is left alone when it already carries the key, whatever door
// it came from); the tests do, to give each of them the cases the other doors get. TestEveryDoorThatCarriesStaleBinaryItselfIsInTheKeyedList
// pins the list against the call sites.
var staleKeyedDoors = map[string]bool{
	"offload_status":   true,
	"agent_delegate":   true,
	"offload_research": true,
}

// staleKeyedEarlyCases are calls of the keyed doors that END BEFORE their handler reaches the line that adds the key: a refusal
// of the arguments or of the contract, a status call for a section that does not exist. Their results carry no key of their
// own, so on a stale server the notice is theirs to carry like any other door's.
var staleKeyedEarlyCases = []struct{ name, door, args string }{
	{"delegate with no subtask", "agent_delegate", `{"subtasks":[]}`},
	{"delegate with arguments of the wrong type", "agent_delegate", `{"subtasks":"all of them"}`},
	{"delegate with a pin reason on a route that takes none", "agent_delegate", `{"subtasks":[{"goal":"x"}],"route":"auto","pin_reason":"because"}`},
	{"research with no goal", "offload_research", `{"urls":["https://docs.example/a"]}`},
	{"research with no url", "offload_research", `{"goal":"digest the page","urls":[]}`},
	{"research with arguments of the wrong type", "offload_research", `{"goal":7}`},
	{"status for a section that does not exist", "offload_status", `{"section":"no-such-section"}`},
	{"status with an argument it does not take", "offload_status", `{"verbose":true}`},
}

// otherDoorNames is the sorted key set, for stable subtests.
func otherDoorNames() []string {
	names := make([]string, 0, len(staleOtherDoorArgs))
	for n := range staleOtherDoorArgs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// doorSession opens a client session over an in-memory transport against the BUILT server: the same chain of middleware a
// real client meets.
func doorSession(t *testing.T, s *Server) *mcp.ClientSession {
	t.Helper()
	srv := s.buildServer("test")
	clientT, serverT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "stale-notice", Version: "1"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name, args string) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("tools/call %s: %v", name, err)
	}
	return res
}

// blockTexts is the text of every content block of a result; any block that is not text fails the test.
func blockTexts(t *testing.T, res *mcp.CallToolResult) []string {
	t.Helper()
	out := make([]string, len(res.Content))
	for i, c := range res.Content {
		tc, ok := c.(*mcp.TextContent)
		if !ok {
			t.Fatalf("content[%d] is %T, want text", i, c)
		}
		out[i] = tc.Text
	}
	return out
}

// installOn puts a server on an installed image at a fresh path, the way serverOnInstalledImage does for a server it builds
// itself: it holds that startup identity and its guard watches the file at the path.
func installOn(t *testing.T, s *Server, version string) string {
	t.Helper()
	captureStaleLog(t)
	path := t.TempDir() + "/local-offload.exe"
	st := installImage(t, path, version)
	s.startup = &st
	s.exe = newExeWatch(func() (exeStamp, error) { return stampOf(path) })
	return path
}

// movableStaleServer is a server whose guard sees a moved stamp and holds no startup identity (the stamp alone decides): the
// fixture of the tests that drive the middleware directly.
func movableStaleServer(t *testing.T) *Server {
	t.Helper()
	captureStaleLog(t)
	exe := &staleMovableExe{cur: staleExeThen}
	s := delegateTestServer(t, staleLocalRun())
	s.exe = newExeWatch(exe.read)
	exe.set(staleExeNow, nil)
	return s
}

// throughNotice runs one tools/call for the named tool through the middleware, with next standing in for the rest of the chain.
func throughNotice(s *Server, tool string, next mcp.MethodHandler) (mcp.Result, error) {
	return s.staleNotice(next)(context.Background(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: tool}})
}

func oneBlockResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// TestAStaleServerAppendsTheNoticeToEveryOtherDoor: after a swap of the installed bytes every door but the three that carry the
// signal themselves answers with its own result untouched and the notice as a second text block.
func TestAStaleServerAppendsTheNoticeToEveryOtherDoor(t *testing.T) {
	s, _, path := serverOnInstalledImage(t, "0.183.0")
	cs := doorSession(t, s)

	// The drift guard: a door that is registered tomorrow has to be given a case here (or be one of the keyed doors, whose
	// cases are staleKeyedEarlyCases and the success-path test).
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if staleKeyedDoors[tool.Name] {
			continue
		}
		if _, ok := staleOtherDoorArgs[tool.Name]; !ok {
			t.Errorf("door %q is registered and has no stale-notice case: add it to staleOtherDoorArgs", tool.Name)
		}
	}

	before := map[string]string{}
	for _, name := range otherDoorNames() {
		texts := blockTexts(t, callTool(t, cs, name, staleOtherDoorArgs[name]))
		if len(texts) != 1 {
			t.Fatalf("%s: a server that has not been swapped answered with %d content blocks, want 1", name, len(texts))
		}
		before[name] = texts[0]
	}

	swapImage(t, path, imageBody("0.184.0"))
	for _, name := range otherDoorNames() {
		t.Run(name, func(t *testing.T) {
			res := callTool(t, cs, name, staleOtherDoorArgs[name])
			texts := blockTexts(t, res)
			if len(texts) != 2 {
				t.Fatalf("%d content blocks after the swap, want the door's own answer and the notice: %q", len(texts), texts)
			}
			if texts[0] != before[name] {
				t.Errorf("the door's own block changed:\n got %q\nwant %q", texts[0], before[name])
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(texts[0]), &m); err != nil {
				t.Errorf("content[0] is no longer a JSON object: %v", err)
			}
			notice := texts[1]
			for _, want := range []string{"stale_binary: ", buildinfo.Version, "changed on disk", "/mcp", "0.184.0"} {
				if !strings.Contains(notice, want) {
					t.Errorf("the notice %q does not say %q", notice, want)
				}
			}
		})
	}
}

// TestAFreshServerAddsNoBlock: nothing was swapped, or the file was rewritten with the same bytes (a redeploy of the same
// build): the answer is the door's own and nothing more.
func TestAFreshServerAddsNoBlock(t *testing.T) {
	s, _, path := serverOnInstalledImage(t, "0.183.0")
	cs := doorSession(t, s)
	for _, name := range otherDoorNames() {
		if n := len(callTool(t, cs, name, staleOtherDoorArgs[name]).Content); n != 1 {
			t.Errorf("%s: %d content blocks on a server that was not swapped, want 1", name, n)
		}
	}
	swapImage(t, path, imageBody("0.183.0")) // a redeploy of the same build: a later mtime, equal bytes
	for _, name := range otherDoorNames() {
		if n := len(callTool(t, cs, name, staleOtherDoorArgs[name]).Content); n != 1 {
			t.Errorf("%s: %d content blocks after a redeploy of the same bytes, want 1", name, n)
		}
	}
}

// TestTheThreeDoorsThatCarryTheSignalAreNotDoubled: agent_delegate, offload_research and offload_status put stale_binary in
// their own answer when they ran the work; the middleware sees the key there and leaves those results alone, so the signal is
// there once.
func TestTheThreeDoorsThatCarryTheSignalAreNotDoubled(t *testing.T) {
	s := staleResearchServer(t)
	path := installOn(t, s, "0.183.0")
	swapImage(t, path, imageBody("0.184.0"))
	cs := doorSession(t, s)
	for name, args := range map[string]string{
		"agent_delegate":   staleDelegateArgs,
		"offload_research": staleResearchArgs,
		"offload_status":   `{"section":"brief"}`,
	} {
		t.Run(name, func(t *testing.T) {
			texts := blockTexts(t, callTool(t, cs, name, args))
			if len(texts) != 1 {
				t.Fatalf("%d content blocks, want the door's own answer alone: %q", len(texts), texts)
			}
			if n := strings.Count(texts[0], `"stale_binary":`); n != 1 {
				t.Fatalf("the answer carries stale_binary %d times, want once: %.300s", n, texts[0])
			}
		})
	}
}

// TestTheEarlyReturnsOfTheKeyedDoorsCarryTheNotice: the three doors put the key in the answer only on the path that ran the
// work. A refusal of the arguments, of the contract or of a section answers before that line, with no key, and a stale server
// must still say so there: every tool result of every door.
func TestTheEarlyReturnsOfTheKeyedDoorsCarryTheNotice(t *testing.T) {
	s := staleResearchServer(t)
	path := installOn(t, s, "0.183.0")
	swapImage(t, path, imageBody("0.184.0"))
	cs := doorSession(t, s)
	for _, c := range staleKeyedEarlyCases {
		t.Run(c.name, func(t *testing.T) {
			texts := blockTexts(t, callTool(t, cs, c.door, c.args))
			if len(texts) != 2 {
				t.Fatalf("%d content blocks after the swap, want the door's own refusal and the notice: %q", len(texts), texts)
			}
			if !strings.Contains(texts[0], `"deferred":true`) || strings.Contains(texts[0], `"stale_binary`) {
				t.Errorf("content[0] = %.300s, want the door's own deferral with no stale key in it", texts[0])
			}
			if !strings.HasPrefix(texts[1], "stale_binary: ") || !strings.Contains(texts[1], "/mcp") {
				t.Errorf("content[1] = %.200s, want the stale_binary notice", texts[1])
			}
		})
	}
}

// TestAFreshServerAddsNoBlockToTheKeyedDoorsEarlyReturns: the same calls on a server that was not swapped stay a single block,
// so the cases above are early returns and the notice is not an artefact of them.
func TestAFreshServerAddsNoBlockToTheKeyedDoorsEarlyReturns(t *testing.T) {
	s := staleResearchServer(t)
	path := installOn(t, s, "0.183.0")
	cs := doorSession(t, s)
	check := func(stage string) {
		for _, c := range staleKeyedEarlyCases {
			texts := blockTexts(t, callTool(t, cs, c.door, c.args))
			if len(texts) != 1 || !strings.Contains(texts[0], `"deferred":true`) {
				t.Errorf("%s: %s: %q, want the door's own deferral alone", stage, c.name, texts)
			}
		}
	}
	check("not swapped")
	swapImage(t, path, imageBody("0.183.0")) // a redeploy of the same build: a later mtime, equal bytes
	check("redeployed with equal bytes")
}

// TestTheKeyedDoorsUnderAnInvalidConfigCarryTheNotice: the config gate answers for agent_delegate and offload_research before
// their handlers run, with a deferral that has no key. A stale server with a config the loader refused is the case the guard
// exists for (a server that runs old behaviour and cannot work), and it must say so on those two doors as on any other.
func TestTheKeyedDoorsUnderAnInvalidConfigCarryTheNotice(t *testing.T) {
	statusSamplesGPU = false
	t.Cleanup(func() { statusSamplesGPU = true })
	s := New(pipeline.New(config.Default(), nil, nil, nil)).WithConfigError(badConfig)
	path := installOn(t, s, "0.183.0")
	cs := doorSession(t, s)
	doors := map[string]string{"agent_delegate": staleDelegateArgs, "offload_research": staleResearchArgs}
	for door, args := range doors {
		if n := len(callTool(t, cs, door, args).Content); n != 1 {
			t.Errorf("%s: %d content blocks on a server that was not swapped, want the config deferral alone", door, n)
		}
	}
	swapImage(t, path, imageBody("0.184.0"))
	for door, args := range doors {
		t.Run(door, func(t *testing.T) {
			texts := blockTexts(t, callTool(t, cs, door, args))
			if len(texts) != 2 || !strings.Contains(texts[0], "config invalid") || !strings.HasPrefix(texts[1], "stale_binary: ") {
				t.Fatalf("blocks %q, want the config deferral and the notice", texts)
			}
		})
	}
}

// TestAKeyedDoorOnABlindGuardCarriesTheGuardOnceOnEveryAnswer: a door that ran the work puts stale_binary_guard in its answer
// whenever the guard is not watching (not only on the middleware's every-Nth cadence), and the middleware recognises that key
// alone as the door's own signal: the guard state is not added a second time beside it.
func TestAKeyedDoorOnABlindGuardCarriesTheGuardOnceOnEveryAnswer(t *testing.T) {
	captureStaleLog(t)
	s := delegateTestServer(t, staleLocalRun())
	s.exe = newExeWatchFrom(exeStamp{}, errors.New("no executable path"), nil)
	cs := doorSession(t, s)
	for i := 1; i <= 3; i++ {
		texts := blockTexts(t, callTool(t, cs, "agent_delegate", staleDelegateArgs))
		if len(texts) != 1 {
			t.Fatalf("call %d: %d content blocks, want the door's own answer alone: %q", i, len(texts), texts)
		}
		if n := strings.Count(texts[0], `"stale_binary_guard":`); n != 1 {
			t.Fatalf("call %d: the answer carries stale_binary_guard %d times, want once: %.300s", i, n, texts[0])
		}
	}
}

// TestTheNoticeIsOnEveryCallNotTheFirstN: a stale server stays stale until it is reconnected, and a cap would bring the
// silent failure back for every call after it.
func TestTheNoticeIsOnEveryCallNotTheFirstN(t *testing.T) {
	s, _, path := serverOnInstalledImage(t, "0.183.0")
	swapImage(t, path, imageBody("0.184.0"))
	cs := doorSession(t, s)
	for i := 1; i <= 6; i++ {
		texts := blockTexts(t, callTool(t, cs, "offload_summarize", staleOtherDoorArgs["offload_summarize"]))
		if len(texts) != 2 || !strings.HasPrefix(texts[1], "stale_binary: ") {
			t.Fatalf("call %d: %q, want the door's answer and the notice", i, texts)
		}
	}
}

// TestARebuildOfTheSameVersionReadsSensibly: builds embed their worktree path, so a redeploy of the same version can hold other
// bytes. The notice is still the note (the server is stale), and it names the installed build as the very version the server
// runs instead of claiming it is older.
func TestARebuildOfTheSameVersionReadsSensibly(t *testing.T) {
	s, _, path := serverOnInstalledImage(t, buildinfo.Version)
	swapImage(t, path, append(imageBody(buildinfo.Version), []byte("rebuilt in another worktree")...))
	cs := doorSession(t, s)
	texts := blockTexts(t, callTool(t, cs, "offload_summarize", staleOtherDoorArgs["offload_summarize"]))
	if len(texts) != 2 {
		t.Fatalf("%d blocks for a rebuild of the same version with other bytes, want the answer and the notice", len(texts))
	}
	for _, want := range []string{"is running " + buildinfo.Version, "the installed build is " + buildinfo.Version, "changed on disk"} {
		if !strings.Contains(texts[1], want) {
			t.Errorf("the notice %q does not say %q", texts[1], want)
		}
	}
	if strings.Contains(texts[1], "older") || strings.Contains(texts[1], "newer") {
		t.Errorf("the notice %q ranks two builds of one version", texts[1])
	}
}

// TestAnErrorResultStillCarriesTheNotice: a result flagged IsError is still a result of a stale server.
func TestAnErrorResultStillCarriesTheNotice(t *testing.T) {
	s := movableStaleServer(t)
	failed := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: `{"deferred":false}`}}}
	got, err := throughNotice(s, "offload_summarize", func(context.Context, string, mcp.Request) (mcp.Result, error) { return failed, nil })
	if err != nil {
		t.Fatal(err)
	}
	res, _ := got.(*mcp.CallToolResult)
	if res == nil || !res.IsError {
		t.Fatalf("the result lost its error flag: %+v", got)
	}
	texts := blockTexts(t, res)
	if len(texts) != 2 || texts[0] != `{"deferred":false}` || !strings.HasPrefix(texts[1], "stale_binary: ") {
		t.Fatalf("blocks %q, want the door's own error answer and the notice", texts)
	}
}

// TestAHandlerErrorAndATypedNilResultPassThroughUntouched: the SDK hands the middleware the handler's (*CallToolResult, error)
// as a (Result, error), so a handler that returns (nil, err) arrives as an interface that holds a nil pointer: the type
// assertion succeeds and a dereference would panic.
func TestAHandlerErrorAndATypedNilResultPassThroughUntouched(t *testing.T) {
	s := movableStaleServer(t)
	boom := errors.New("handler failed")
	for name, next := range map[string]mcp.MethodHandler{
		"error with a typed-nil result": func(context.Context, string, mcp.Request) (mcp.Result, error) {
			var none *mcp.CallToolResult
			return none, boom
		},
		"no error and a typed-nil result": func(context.Context, string, mcp.Request) (mcp.Result, error) {
			var none *mcp.CallToolResult
			return none, nil
		},
		"an error and no result at all": func(context.Context, string, mcp.Request) (mcp.Result, error) { return nil, boom },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("the middleware panicked on %s: %v", name, r)
				}
			}()
			got, err := throughNotice(s, "offload_summarize", next)
			if name != "no error and a typed-nil result" && !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the handler's own error", err)
			}
			if res, ok := got.(*mcp.CallToolResult); ok && res != nil {
				t.Fatalf("a result appeared from nothing: %+v", res)
			}
		})
	}
}

// TestAResultWithNoContentPassesThroughUntouched: with no first block there is no answer to leave alone and put a notice
// behind, so the result is returned as it is (the SDK itself turns a nil Content into an empty one before it gets here).
func TestAResultWithNoContentPassesThroughUntouched(t *testing.T) {
	s := movableStaleServer(t)
	empty := &mcp.CallToolResult{Content: []mcp.Content{}}
	got, err := throughNotice(s, "offload_summarize", func(context.Context, string, mcp.Request) (mcp.Result, error) { return empty, nil })
	if err != nil || got != mcp.Result(empty) || len(empty.Content) != 0 {
		t.Fatalf("a result with no content was changed: %+v, %v", got, err)
	}
}

// TestTheNoticeNeverMutatesTheResultTheHandlerReturned: a handler may return a result it keeps (a cached answer); appending to
// its Content in place would grow it on every call.
func TestTheNoticeNeverMutatesTheResultTheHandlerReturned(t *testing.T) {
	s := movableStaleServer(t)
	shared := oneBlockResult(`{"summary":"cached"}`)
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) { return shared, nil }
	for i := 0; i < 3; i++ {
		got, err := throughNotice(s, "offload_summarize", next)
		if err != nil {
			t.Fatal(err)
		}
		res, _ := got.(*mcp.CallToolResult)
		if res == nil || len(res.Content) != 2 {
			t.Fatalf("call %d: %+v, want the answer and the notice", i, got)
		}
		if res == shared {
			t.Fatalf("call %d returned the handler's own result, which the notice then changed", i)
		}
	}
	if len(shared.Content) != 1 {
		t.Fatalf("the handler's result holds %d blocks after three calls, want it unchanged at 1", len(shared.Content))
	}
}

// staleMiddlewareFixture is a server whose guard sees a moved stamp and counts how often the stamp is read.
func staleMiddlewareFixture(t *testing.T) (*Server, *atomic.Int64) {
	t.Helper()
	captureStaleLog(t)
	var reads atomic.Int64
	exe := &staleMovableExe{cur: staleExeThen}
	s := delegateTestServer(t, staleLocalRun())
	s.exe = newExeWatch(func() (exeStamp, error) { reads.Add(1); return exe.read() })
	exe.set(staleExeNow, nil)
	reads.Store(0)
	return s, &reads
}

// staleSampleDoors are door names the middleware is driven under: the three that put the key in their answer, and others.
var staleSampleDoors = []string{"agent_delegate", "offload_research", "offload_status", "offload_summarize", "agent_run"}

// TestOnlyToolCallsAreTouched: tools/list and every other method pass through as they are.
func TestOnlyToolCallsAreTouched(t *testing.T) {
	s, reads := staleMiddlewareFixture(t)
	answer := oneBlockResult(`{"x":1}`)
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) { return answer, nil }
	for _, method := range []string{"tools/list", "initialize", "ping", "prompts/list"} {
		got, err := s.staleNotice(next)(context.Background(), method, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "offload_summarize"}})
		if err != nil || got != mcp.Result(answer) || len(answer.Content) != 1 {
			t.Fatalf("%s was changed: %v %v", method, got, err)
		}
	}
	if n := reads.Load(); n != 0 {
		t.Fatalf("the stamp was read %d times for methods that are not tool calls", n)
	}
}

// TestAResultThatAlreadyCarriesTheSignalIsLeftAloneWhateverTheDoor: a result whose first block is a JSON object with
// stale_binary or stale_binary_guard at its top level is a door's own signal, and it is left alone WITHOUT a check: that door
// already stat the file (and may have hashed it, up to installedReadWait) for its answer, and the signal must be there once.
// The decision is the result's, not the door's name: a fourth door that gains the key is covered the day it gains it.
func TestAResultThatAlreadyCarriesTheSignalIsLeftAloneWhateverTheDoor(t *testing.T) {
	s, reads := staleMiddlewareFixture(t)
	for name, body := range map[string]string{
		"the note":                   `{"summary":{},"stale_binary":"this MCP server is running 0.1.0 from an executable image that has changed"}`,
		"the guard alone":            `{"summary":{},"stale_binary_guard":"inactive: no executable path"}`,
		"both":                       `{"stale_binary":"n","stale_binary_guard":"g"}`,
		"the key first":              `{"stale_binary":"n","results":[]}`,
		"after a deep value":         `{"results":[{"a":{"b":[1,2,{"c":3}]}}],"stale_binary":"n"}`,
		"after a string that quotes": `{"text":"the \"stale_binary\": key","stale_binary_guard":"g"}`,
	} {
		for _, door := range staleSampleDoors {
			answer := oneBlockResult(body)
			got, err := throughNotice(s, door, func(context.Context, string, mcp.Request) (mcp.Result, error) { return answer, nil })
			if err != nil || got != mcp.Result(answer) || len(answer.Content) != 1 {
				t.Errorf("%s under %s: a result that carries the signal itself was changed: %+v %v", name, door, got, err)
			}
		}
	}
	if n := reads.Load(); n != 0 {
		t.Fatalf("the stamp was read %d times for results that carry the signal and needed no check", n)
	}
}

// TestAResultThatOnlyMentionsTheKeyIsNotTheSignal: the key nested in a value, quoted in a string, or in text that is not a JSON
// object is not the door's signal, under any door name; the notice is appended and the stamp is read.
func TestAResultThatOnlyMentionsTheKeyIsNotTheSignal(t *testing.T) {
	for name, body := range map[string]string{
		"nested in a value":   `{"results":[{"stale_binary":"x"}]}`,
		"nested in an object": `{"process":{"stale_binary_guard":"x"}}`,
		"quoted in a string":  `{"text":"look for \"stale_binary\": in the status"}`,
		"in text, not JSON":   `a note about stale_binary and "stale_binary": values`,
		"in an array":         `[{"stale_binary":"x"}]`,
		"a prefix of the key": `{"stale_binary_x":"x"}`,
		"no key at all":       `{"x":1}`,
	} {
		for _, door := range staleSampleDoors {
			t.Run(name+"/"+door, func(t *testing.T) {
				s, reads := staleMiddlewareFixture(t)
				answer := oneBlockResult(body)
				got, err := throughNotice(s, door, func(context.Context, string, mcp.Request) (mcp.Result, error) { return answer, nil })
				res, _ := got.(*mcp.CallToolResult)
				if err != nil || res == nil || len(res.Content) != 2 {
					t.Fatalf("%+v %v, want the answer and the notice", got, err)
				}
				if res.Content[0].(*mcp.TextContent).Text != body || !strings.HasPrefix(res.Content[1].(*mcp.TextContent).Text, "stale_binary: ") {
					t.Fatalf("blocks %q, want the door's own text first and the notice second", blockTexts(t, res))
				}
				if reads.Load() == 0 {
					t.Fatal("the stamp was never read")
				}
			})
		}
	}
}

// TestAConfigInvalidDeferredResultCarriesTheNoticeToo: the middleware sits outside the config gate, so the deferral a server
// with an invalid config answers with is still a result of a stale server; offload_status keeps its single signal.
func TestAConfigInvalidDeferredResultCarriesTheNoticeToo(t *testing.T) {
	statusSamplesGPU = false
	t.Cleanup(func() { statusSamplesGPU = true })
	s := New(pipeline.New(config.Default(), nil, nil, nil)).WithConfigError(badConfig)
	path := installOn(t, s, "0.183.0")
	swapImage(t, path, imageBody("0.184.0"))
	cs := doorSession(t, s)

	texts := blockTexts(t, callTool(t, cs, "agent_run", `{"goal":"anything"}`))
	if len(texts) != 2 || !strings.Contains(texts[0], "config invalid") || !strings.HasPrefix(texts[1], "stale_binary: ") {
		t.Fatalf("blocks %q, want the config deferral and the notice", texts)
	}
	status := blockTexts(t, callTool(t, cs, "offload_status", `{"section":"brief"}`))
	if len(status) != 1 || strings.Count(status[0], `"stale_binary":`) != 1 || !strings.HasPrefix(status[0], `{"config_error":`) {
		t.Fatalf("status blocks %q, want one block with config_error first and stale_binary once", status)
	}
}

// TestAnUnreadableStampAccusesNothingButSaysTheGuardIsBlind: a stat that fails proves nothing about the image, so there is no
// stale notice; the guard is blind while it fails, and the first call that finds it so says that.
func TestAnUnreadableStampAccusesNothingButSaysTheGuardIsBlind(t *testing.T) {
	captureStaleLog(t)
	exe := &staleMovableExe{cur: staleExeThen}
	s := delegateTestServer(t, staleLocalRun())
	s.exe = newExeWatch(exe.read)
	exe.set(exeStamp{}, errors.New("permission denied"))
	cs := doorSession(t, s)

	texts := blockTexts(t, callTool(t, cs, "offload_summarize", staleOtherDoorArgs["offload_summarize"]))
	if len(texts) != 2 || !strings.HasPrefix(texts[1], "stale_binary_guard: unreadable:") || !strings.Contains(texts[1], "permission denied") {
		t.Fatalf("blocks %q, want the door's answer and the guard's own state", texts)
	}
	for _, x := range texts {
		if strings.HasPrefix(x, "stale_binary: ") {
			t.Fatalf("an unreadable stamp accused: %q", x)
		}
	}
	exe.set(staleExeThen, nil)
	if n := len(callTool(t, cs, "offload_summarize", staleOtherDoorArgs["offload_summarize"]).Content); n != 1 {
		t.Fatalf("%d blocks once the stamp reads again, want the door's answer alone", n)
	}
}

// TestTheGuardStateRidesOnTheFirstCallAndThenEveryNth: a guard that never started (or cannot look) is a standing condition of
// the process; the stale notice is on every call, this one is not.
func TestTheGuardStateRidesOnTheFirstCallAndThenEveryNth(t *testing.T) {
	captureStaleLog(t)
	s := delegateTestServer(t, staleLocalRun())
	s.exe = newExeWatchFrom(exeStamp{}, errors.New("no executable path"), nil)
	cs := doorSession(t, s)
	for i := 1; i <= 2*staleGuardEvery+1; i++ {
		texts := blockTexts(t, callTool(t, cs, "offload_summarize", staleOtherDoorArgs["offload_summarize"]))
		wantBlock := (i-1)%staleGuardEvery == 0
		if wantBlock {
			if len(texts) != 2 || !strings.HasPrefix(texts[1], "stale_binary_guard: inactive:") || !strings.Contains(texts[1], "no executable path") {
				t.Fatalf("call %d: %q, want the guard state beside the door's answer", i, texts)
			}
		} else if len(texts) != 1 {
			t.Fatalf("call %d: %q, want the door's answer alone", i, texts)
		}
	}
}

// TestTheNoticeIsTheStaleBinaryNoteVerbatim: one definition. The block says what offload_status says under stale_binary, word
// for word, so the two signals cannot disagree.
func TestTheNoticeIsTheStaleBinaryNoteVerbatim(t *testing.T) {
	s, _, path := serverOnInstalledImage(t, "0.183.0")
	swapImage(t, path, imageBody("0.184.0"))
	cs := doorSession(t, s)
	texts := blockTexts(t, callTool(t, cs, "offload_summarize", staleOtherDoorArgs["offload_summarize"]))
	status := blockTexts(t, callTool(t, cs, "offload_status", `{"section":"brief"}`))
	var m map[string]any
	if err := json.Unmarshal([]byte(status[0]), &m); err != nil {
		t.Fatal(err)
	}
	note, _ := m["stale_binary"].(string)
	if note == "" || len(texts) != 2 || texts[1] != "stale_binary: "+note {
		t.Fatalf("notice %q, status stale_binary %q: want \"stale_binary: \" and the note verbatim", texts, note)
	}
}

// TestAStalledReadOfTheInstalledImageDoesNotHoldAnOtherDoorAndTheNextCallCarriesTheVerdict: the notice waits for the installed
// file's read at most installedReadWait (stale_bounded_test.go), after the door's result exists; the moved stamp stands while
// the read has not finished, and the next call uses what it found.
func TestAStalledReadOfTheInstalledImageDoesNotHoldAnOtherDoorAndTheNextCallCarriesTheVerdict(t *testing.T) {
	s, _, path := serverOnInstalledImage(t, "0.183.0")
	swapImage(t, path, imageBody("0.183.0")) // equal bytes: once the read lands the server is fresh
	release, _ := stalledHashes(t, 1500*time.Millisecond)
	cs := doorSession(t, s)

	began := time.Now()
	texts := blockTexts(t, callTool(t, cs, "offload_summarize", staleOtherDoorArgs["offload_summarize"]))
	if took := time.Since(began); took > time.Second {
		t.Fatalf("the call waited %s for a read that was stalled", took)
	}
	if len(texts) != 2 || !strings.Contains(texts[1], "did not finish") {
		t.Fatalf("blocks %q, want the moved stamp's notice that says the read did not finish", texts)
	}
	release()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if n := len(callTool(t, cs, "offload_summarize", staleOtherDoorArgs["offload_summarize"]).Content); n == 1 {
			return // the finished read settled it: equal bytes, fresh
		}
		if time.Now().After(deadline) {
			t.Fatal("the finished read never settled the answer")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestToolsListIsTheSameOnAStaleServer: the middleware touches tools/call results only; the tool surface is byte-identical.
func TestToolsListIsTheSameOnAStaleServer(t *testing.T) {
	list := func(s *Server) string {
		res, err := doorSession(t, s).ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	fresh, _, _ := serverOnInstalledImage(t, "0.183.0")
	stale, _, path := serverOnInstalledImage(t, "0.183.0")
	swapImage(t, path, imageBody("0.184.0"))
	if a, b := list(fresh), list(stale); a != b {
		t.Fatalf("tools/list differs on a stale server:\n fresh %.200s\n stale %.200s", a, b)
	}
}

// TestTheStaleBinaryNoteNamesMcpReconnect: the way out a stale server's note gives is the one that works for a stdio server
// the client will not reconnect by itself: /mcp, Reconnect (or a restart of the client).
func TestTheStaleBinaryNoteNamesMcpReconnect(t *testing.T) {
	s := movableStaleServer(t)
	note := s.staleBinaryNote(buildinfo.Version)
	if !strings.Contains(note, "/mcp") || !strings.Contains(strings.ToLower(note), "restart") {
		t.Fatalf("stale_binary = %q, want it to name /mcp and still say restart", note)
	}
}

// TestEveryDoorThatCarriesStaleBinaryItselfIsInTheKeyedList pins staleKeyedDoors against the call sites: a handler that calls
// staleBinaryNote or staleBinaryFields puts the signal in its own answer. The middleware needs no list (it recognises the key
// in the result), but the tests give each of those doors the early-return and invalid-config cases the other doors get, so a
// fourth door that gains the keys fails here until it is listed and given them. The set is read from the source (every
// registered handler that makes the call, by the tool name it is registered under).
func TestEveryDoorThatCarriesStaleBinaryItselfIsInTheKeyedList(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	callers := map[string][]string{}  // handler method -> files that call the stale helpers inside it
	registered := map[string]string{} // handler method -> tool name it is registered under
	own := map[string]bool{"stalebinary.go": true, "stale_notice.go": true}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || own[f] {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "staleBinaryNote" || sel.Sel.Name == "staleBinaryFields") {
					callers[fn.Name.Name] = append(callers[fn.Name.Name], f)
				}
				return true
			})
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "AddTool" {
				return true
			}
			handler, ok := call.Args[1].(*ast.SelectorExpr)
			if !ok {
				return true
			}
			lit, ok := call.Args[0].(*ast.UnaryExpr)
			if !ok {
				return true
			}
			comp, ok := lit.X.(*ast.CompositeLit)
			if !ok {
				return true
			}
			for _, el := range comp.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Name" {
					if bl, ok := kv.Value.(*ast.BasicLit); ok {
						if name, err := strconv.Unquote(bl.Value); err == nil {
							registered[handler.Sel.Name] = name
						}
					}
				}
			}
			return true
		})
	}
	if len(registered) < 25 {
		t.Fatalf("the scan found only %d registered handlers: the AddTool shape changed and this pin reads nothing", len(registered))
	}
	found := map[string]bool{}
	for handler := range callers {
		tool, ok := registered[handler]
		if !ok {
			t.Errorf("%s calls staleBinaryNote/staleBinaryFields but is not a registered tool handler: the pin cannot tell which door it is", handler)
			continue
		}
		found[tool] = true
	}
	for tool := range found {
		if !staleKeyedDoors[tool] {
			t.Errorf("door %q carries stale_binary in its own answer and is not in staleKeyedDoors: add it, with its cases in staleKeyedEarlyCases and the success-path tests", tool)
		}
	}
	for tool := range staleKeyedDoors {
		if !found[tool] {
			t.Errorf("door %q is in staleKeyedDoors but its handler no longer carries stale_binary itself: drop it from the list and the cases that assume the key", tool)
		}
	}
}
