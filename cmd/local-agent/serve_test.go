package main

import "testing"

// TestValidateListenAddr locks the loopback guard: the unauthenticated agent
// endpoint must bind loopback only, unless the operator explicitly opts into
// exposing it. Empty-host forms (":18800") bind ALL interfaces under Go's
// net.Listen, so they are treated as non-loopback and refused.
func TestValidateListenAddr(t *testing.T) {
	loopback := []string{
		"127.0.0.1:18800",
		"[::1]:18800",
		"localhost:18800",
	}
	nonLocal := []string{
		"0.0.0.0:18800",
		"192.168.1.5:18800",
		":18800", // empty host = bind all interfaces
	}

	for _, addr := range loopback {
		if err := validateListenAddr(addr, false); err != nil {
			t.Errorf("validateListenAddr(%q, false) = %v, want nil (loopback is allowed)", addr, err)
		}
	}
	for _, addr := range nonLocal {
		if err := validateListenAddr(addr, false); err == nil {
			t.Errorf("validateListenAddr(%q, false) = nil, want refusal (non-loopback)", addr)
		}
		// The override must let every refused address through (the caller,
		// not the validator, is responsible for emitting the loud warning).
		if err := validateListenAddr(addr, true); err != nil {
			t.Errorf("validateListenAddr(%q, true) = %v, want nil (override allows non-loopback)", addr, err)
		}
	}
}

func TestLastUserMessage(t *testing.T) {
	msgs := []openAIMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "reply"},
		{Role: "user", Content: "latest goal"},
	}
	if got := lastUserMessage(msgs); got != "latest goal" {
		t.Errorf("lastUserMessage = %q, want %q", got, "latest goal")
	}
	if got := lastUserMessage([]openAIMessage{{Role: "system", Content: "x"}}); got != "" {
		t.Errorf("no user message should give empty, got %q", got)
	}
	if got := lastUserMessage(nil); got != "" {
		t.Errorf("nil messages should give empty, got %q", got)
	}
}

func TestNewChatCompletion(t *testing.T) {
	r := newChatCompletion("id1", "local-agent", "hello world", 123)
	if r.Object != "chat.completion" {
		t.Errorf("Object = %q, want chat.completion", r.Object)
	}
	if r.Model != "local-agent" || r.ID != "id1" || r.Created != 123 {
		t.Errorf("envelope wrong: %+v", r)
	}
	if len(r.Choices) != 1 {
		t.Fatalf("want 1 choice, got %d", len(r.Choices))
	}
	c := r.Choices[0]
	if c.Message.Role != "assistant" || c.Message.Content != "hello world" || c.FinishReason != "stop" {
		t.Errorf("choice wrong: %+v", c)
	}
}
