package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// mixedSchemaJSON names every scalar kind and a list whose items are strings:
// the prompt has to say each type, or a seat reads the field NAME ("numbers")
// and answers numbers where the schema wants strings (register C-80).
const mixedSchemaJSON = `{"type":"object","properties":{` +
	`"key_facts":{"type":"array","items":{"type":"string"}},` +
	`"numbers":{"type":"array","items":{"type":"string"}},` +
	`"ratios":{"type":"array","items":{"type":"number"}},` +
	`"score":{"type":"number"},` +
	`"count":{"type":"integer"},` +
	`"ok":{"type":"boolean"},` +
	`"label":{"type":"string","enum":["low","high"]},` +
	`"verdict":{"type":"string"}},` +
	`"required":["key_facts","numbers","ratios","score","count","ok","label","verdict"]}`

const mixedAnswerJSON = `{"key_facts":["a"],"numbers":["1"],"ratios":[0.5],"score":1.5,"count":2,"ok":true,"label":"low","verdict":"v"}`

// wantFieldTypes is what every re-pack prompt must say about the mixed schema.
var wantFieldTypes = []string{
	`"key_facts" (array of strings)`,
	`"numbers" (array of strings)`,
	`"ratios" (array of numbers)`,
	`"score" (number)`,
	`"count" (integer)`,
	`"ok" (boolean)`,
	`"label" (string, one of "low", "high")`,
	`"verdict" (string)`,
}

func mixedContract() core.AgentContract {
	c := testContract()
	c.OutputSchema = json.RawMessage(mixedSchemaJSON)
	return c
}

// promptOf returns the system and user messages of a captured completion body.
func promptOf(t *testing.T, body map[string]any) (system, user string) {
	t.Helper()
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		content, _ := mm["content"].(string)
		switch mm["role"] {
		case "system":
			system = content
		case "user":
			user = content
		}
	}
	if system == "" || user == "" {
		t.Fatalf("captured body has no system/user messages: %v", body["messages"])
	}
	return system, user
}

func checkRepackPrompt(t *testing.T, lane string, body map[string]any) {
	t.Helper()
	system, user := promptOf(t, body)
	for _, want := range wantFieldTypes {
		if !strings.Contains(user, want) {
			t.Errorf("%s: the user prompt does not say %s:\n%s", lane, want, user)
		}
	}
	// "numbers unquoted" read together with a field called `numbers` is the
	// instruction that produced bare JSON numbers for a list of strings.
	if strings.Contains(system, "numbers unquoted") {
		t.Errorf("%s: the system prompt still says \"numbers unquoted\":\n%s", lane, system)
	}
	if !strings.Contains(system, `never 42`) {
		t.Errorf("%s: the system prompt does not tell the seat that a string field stays quoted even when it holds a number:\n%s", lane, system)
	}
}

// TestRepackPromptsNameEachFieldsTypeAndItsItemType: the grammar lane, the
// grammar-free chat lane and a vLLM seat's schema lane all put each field's type
// (and a list's item type) in the prompt.
func TestRepackPromptsNameEachFieldsTypeAndItsItemType(t *testing.T) {
	t.Run("grammar lane", func(t *testing.T) {
		fake := &agentFake{
			rosterIDs:    []string{agentTestSeat},
			loop:         func(int64) string { return doneChat("Prose answer about a few facts and numbers.") },
			repack:       func(int64) string { return mixedAnswerJSON },
			repackBodies: make(chan map[string]any, 4),
		}
		srv := fake.server(t)
		defer srv.Close()
		wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, mixedContract())))
		if wire.Deferred {
			t.Fatalf("deferred (%s): %s", wire.DeferClass, wire.Reason)
		}
		select {
		case body := <-fake.repackBodies:
			checkRepackPrompt(t, "grammar lane", body)
		case <-time.After(2 * time.Second):
			t.Fatal("no grammar request recorded")
		}
	})

	t.Run("chat lane", func(t *testing.T) {
		fake := &agentFake{
			rosterIDs:    []string{agentTestSeat},
			loop:         func(int64) string { return doneChat("Prose answer about a few facts and numbers.") },
			repack:       func(int64) string { return `{"wrong":"shape"}` },
			chatFallback: func(int64) string { return doneChat(mixedAnswerJSON) },
			chatBodies:   make(chan map[string]any, 4),
		}
		srv := fake.server(t)
		defer srv.Close()
		wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, mixedContract())))
		if wire.Deferred {
			t.Fatalf("deferred (%s): %s", wire.DeferClass, wire.Reason)
		}
		select {
		case body := <-fake.chatBodies:
			checkRepackPrompt(t, "chat lane", body)
		case <-time.After(2 * time.Second):
			t.Fatal("no chat-lane request recorded")
		}
	})

	t.Run("vLLM schema lane", func(t *testing.T) {
		fake := &agentFake{
			rosterIDs:    []string{agentTestSeat},
			loop:         func(int64) string { return doneChat("Prose answer about a few facts and numbers.") },
			chatFallback: func(int64) string { return doneChat(mixedAnswerJSON) },
			chatBodies:   make(chan map[string]any, 4),
		}
		srv := fake.server(t)
		defer srv.Close()
		p := agentTestPipeline(t, srv.URL)
		cfg := p.Cfg()
		cfg.VLLMSeats = []string{agentTestSeat}
		p.cfg = cfg
		wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, mixedContract())))
		if wire.Deferred {
			t.Fatalf("deferred (%s): %s", wire.DeferClass, wire.Reason)
		}
		select {
		case body := <-fake.chatBodies:
			checkRepackPrompt(t, "vLLM schema lane", body)
		case <-time.After(2 * time.Second):
			t.Fatal("no vLLM request recorded")
		}
	})
}
