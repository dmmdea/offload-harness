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
	"offload_extract_image":         `{"image":"x","schema":[1,2]}`,
	"offload_assess_image":          `{"image":9}`,
	"offload_ocr":                   `{"image":{}}`,
	"offload_generate_image":        `{"prompt":"p","width":"wide"}`,
	"offload_run_graph":             `{"graph_path":123}`,
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

	// The drift guard: a door that is registered tomorrow has to be given a case here (or carry the signal itself).
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if staleBinaryDoors[tool.Name] {
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
// their own answer; the middleware leaves those results alone, so the signal is there once.
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

// TestOnlyToolCallsOfOtherDoorsAreTouched: tools/list and every other method pass through as they are, and the three doors that
// carry the signal themselves are passed through BEFORE any check: they already stat the file (and may hash it) for their own
// answer, so a check here would do the work twice on exactly those doors.
func TestOnlyToolCallsOfOtherDoorsAreTouched(t *testing.T) {
	captureStaleLog(t)
	var reads atomic.Int64
	exe := &staleMovableExe{cur: staleExeThen}
	s := delegateTestServer(t, staleLocalRun())
	s.exe = newExeWatch(func() (exeStamp, error) { reads.Add(1); return exe.read() })
	exe.set(staleExeNow, nil)
	reads.Store(0)

	answer := oneBlockResult(`{"x":1}`)
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) { return answer, nil }
	got, err := s.staleNotice(next)(context.Background(), "tools/list", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "offload_summarize"}})
	if err != nil || got != mcp.Result(answer) || len(answer.Content) != 1 {
		t.Fatalf("a method that is not tools/call was changed: %v %v", got, err)
	}
	for door := range staleBinaryDoors {
		got, err = throughNotice(s, door, next)
		if err != nil || got != mcp.Result(answer) || len(answer.Content) != 1 {
			t.Fatalf("%s: the result of a door that carries the signal itself was changed", door)
		}
	}
	if n := reads.Load(); n != 0 {
		t.Fatalf("the stamp was read %d times for calls that needed no check", n)
	}
	if _, err = throughNotice(s, "offload_summarize", next); err != nil {
		t.Fatal(err)
	}
	if reads.Load() == 0 {
		t.Fatal("a call of another door never read the stamp")
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

// TestEveryDoorThatCarriesStaleBinaryItselfIsInTheSkipSet pins staleBinaryDoors against the call sites: a handler that calls
// staleBinaryNote or staleBinaryFields puts the signal in its own answer, and a middleware that did not skip its door would
// carry it twice. The set is read from the source (every registered handler that makes the call, by the tool name it is
// registered under), so a fourth door that gains the keys fails here until it is listed.
func TestEveryDoorThatCarriesStaleBinaryItselfIsInTheSkipSet(t *testing.T) {
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
		if !staleBinaryDoors[tool] {
			t.Errorf("door %q carries stale_binary in its own answer and is not in staleBinaryDoors: its result would carry the signal twice", tool)
		}
	}
	for tool := range staleBinaryDoors {
		if !found[tool] {
			t.Errorf("door %q is in staleBinaryDoors but its handler no longer carries stale_binary itself: it would be left with no signal at all", tool)
		}
	}
}
