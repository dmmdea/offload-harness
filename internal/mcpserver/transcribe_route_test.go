package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/sttremote"
)

// offload_transcribe takes the same route vocabulary as the vision tools (ADR 0072), with one
// difference: the default is auto, because every fleet node serves the same whisper family and a
// spill therefore costs no quality. These tests drive the handler through its stt seam so no whisper,
// ffmpeg or node is involved: they pin what the handler passes down.

type sttCall struct {
	route string
	req   core.Request
}

// transcribeServer returns a server whose stt seam records its calls and answers with res.
func transcribeServer(t *testing.T, res core.Result) (*Server, *[]sttCall) {
	t.Helper()
	cfg := config.Default()
	cfg.Home = t.TempDir()
	cfg.MediaDir = t.TempDir()
	s := New(pipeline.New(cfg, nil, nil, nil))
	var calls []sttCall
	s.sttRun = func(_ context.Context, _ config.Config, _ sttremote.Runner, req core.Request, route string) core.Result {
		calls = append(calls, sttCall{route, req})
		return res
	}
	return s, &calls
}

func TestTranscribeSchemaCarriesTheRouteAndNamesTheDefault(t *testing.T) {
	var tool string
	var schema map[string]any
	for _, tl := range listTools(t, config.Default()) {
		if tl.Name != "offload_transcribe" {
			continue
		}
		tool = tl.Description
		b, _ := json.Marshal(tl.InputSchema)
		_ = json.Unmarshal(b, &schema)
	}
	if tool == "" {
		t.Fatal("offload_transcribe is not registered")
	}
	route, _ := schema["properties"].(map[string]any)["route"].(map[string]any)
	if route == nil {
		t.Fatalf("the schema has no route property: %v", schema["properties"])
	}
	var enum []string
	for _, e := range route["enum"].([]any) {
		enum = append(enum, e.(string))
	}
	if strings.Join(enum, ",") != "local,auto,remote" {
		t.Errorf("route enum = %v, want local, auto, remote", enum)
	}
	desc, _ := route["description"].(string)
	for _, want := range []string{"auto", "DEFAULT", "same whisper family", "no quality", "meta.node", "engine:npu"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the route description does not say %q: %s", want, desc)
		}
	}
	if !strings.Contains(tool, "route") || !strings.Contains(tool, "fleet node") {
		t.Errorf("the tool description must tell the caller a fleet node can transcribe it: %s", tool)
	}
	if req, _ := schema["required"].([]any); len(req) != 1 || req[0] != "audio" {
		t.Errorf("required = %v: route must stay optional", schema["required"])
	}
}

// No route named is auto; every named route reaches the lane as given.
func TestTranscribeRouteDefaultsToAutoAndPassesWhatWasNamed(t *testing.T) {
	for in, want := range map[string]string{"": "auto", "  ": "auto", "auto": "auto", "local": "local", "remote": "remote", "bogus": "bogus"} {
		s, calls := transcribeServer(t, core.Result{OK: true, Data: json.RawMessage(`{"gist":"x"}`)})
		args := `{"audio":"a.wav","language":"es","hq":true,"route":"` + in + `"}`
		if _, err := s.handleTranscribe(context.Background(), callReq(args)); err != nil {
			t.Fatal(err)
		}
		if len(*calls) != 1 || (*calls)[0].route != want {
			t.Errorf("route %q reached the lane as %v, want %q", in, *calls, want)
			continue
		}
		req := (*calls)[0].req
		if req.Task != core.TaskTranscribe || req.Door != "offload_transcribe" || req.Audio != "a.wav" || req.Params["language"] != "es" || req.Params["hq"] != true {
			t.Errorf("request = %+v, want the transcribe request the handler always built", req)
		}
	}
	// And with no route key at all.
	s, calls := transcribeServer(t, core.Result{OK: true, Data: json.RawMessage(`{}`)})
	if _, err := s.handleTranscribe(context.Background(), callReq(`{"audio":"a.wav"}`)); err != nil || len(*calls) != 1 || (*calls)[0].route != "auto" {
		t.Fatalf("no route key: calls %v err %v, want auto", *calls, err)
	}
}

// The select projection is applied to whatever the lane returns, local or remote.
func TestTranscribeSelectProjectsTheLanesResult(t *testing.T) {
	s, _ := transcribeServer(t, core.Result{OK: true, Data: json.RawMessage(`{"gist":"g","language":"en","segments":[{"id":0}],"srt_path":"x.srt"}`)})
	res, err := s.handleTranscribe(context.Background(), callReq(`{"audio":"a.wav","route":"remote","select":["gist","srt_path"]}`))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := decodeResult(t, res)["result"].(map[string]any)
	if len(data) != 2 || data["gist"] != "g" || data["srt_path"] != "x.srt" {
		t.Fatalf("data = %v, want only the selected fields", data)
	}
}

// engine:npu is this box's own Hailo sidecar and never travels: a route the caller NAMED that is not
// local is refused, as OCR does; an omitted route (the auto default) is not a request to travel.
func TestTranscribeNPURefusesANamedNonLocalRoute(t *testing.T) {
	for _, route := range []string{"auto", "remote", "AUTO"} {
		s, calls := transcribeServer(t, core.Result{OK: true})
		res, err := s.handleTranscribe(context.Background(), callReq(`{"audio":"a.wav","engine":"npu","route":"`+route+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		out := decodeResult(t, res)
		if out["deferred"] != true || !strings.Contains(out["reason"].(string), "applies to engine gpu only") {
			t.Errorf("route %q with engine npu: %v, want the refusal naming engine gpu", route, out)
		}
		if len(*calls) != 0 {
			t.Errorf("route %q with engine npu reached the whisper lane", route)
		}
	}
	for _, args := range []string{`{"audio":"a.wav","engine":"npu"}`, `{"audio":"a.wav","engine":"npu","route":"local"}`, `{"audio":"a.wav","engine":"npu","route":""}`, `{"audio":"a.wav","engine":"npu","route":"  "}`} {
		s, calls := transcribeServer(t, core.Result{OK: true})
		res, err := s.handleTranscribe(context.Background(), callReq(args))
		if err != nil {
			t.Fatal(err)
		}
		out := decodeResult(t, res)
		if out["deferred"] != true || !strings.Contains(out["reason"].(string), "hailo-8l") {
			t.Errorf("%s: %v, want the npu path (no accelerator on this fixture) and not the route refusal", args, out)
		}
		if len(*calls) != 0 {
			t.Errorf("%s reached the whisper lane", args)
		}
	}
}
