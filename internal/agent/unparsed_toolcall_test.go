package agent

import (
	"context"
	"errors"
	"testing"
)

// TestUnparsedToolCallInContentIsAnError: a server that returns the model's tool
// call as plain text (parser mismatch) must not be read as a final answer.
func TestUnparsedToolCallInContentIsAnError(t *testing.T) {
	full := mkTools("list_dir", "read_file")
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", Content: "The user wants the file. </think>\n<tool_call>\n<function=list_dir>\n<parameter=path>\n.\n</parameter>\n</function>\n</tool_call>"}, FinishReason: "stop"},
	}}
	l := NewLoop(client, full, 3)
	res, err := l.Run(context.Background(), "digest the document")
	if !errors.Is(err, ErrUnparsedToolCall) {
		t.Fatalf("want ErrUnparsedToolCall, got err=%v output=%q", err, res.Output)
	}
	if res.StopReason != "unparsed_tool_call" {
		t.Fatalf("stop reason %q", res.StopReason)
	}
	if res.Output != "" {
		t.Fatalf("the unparsed text must not be surfaced as an answer: %q", res.Output)
	}
}

func TestUnparsedToolCallMarker(t *testing.T) {
	cases := map[string]string{
		"plain answer with no calls":                       "",
		"a <tool_call>{\"name\":\"x\"}</tool_call> block":  "<tool_call>",
		"<function=list_dir><parameter=path>.</parameter>": "<function=",
		"<|python_tag|>list_dir()":                         "<|python_tag|>",
		"mentions the word tool_call in prose":             "",
	}
	for in, want := range cases {
		if got := unparsedToolCallMarker(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
