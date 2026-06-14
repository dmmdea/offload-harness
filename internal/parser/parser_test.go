package parser

import (
	"strings"
	"testing"
)

func TestExtractPlain(t *testing.T) {
	d, err := Extract(`{"a":1,"b":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(d), `"a"`) {
		t.Errorf("got %s", d)
	}
}

func TestExtractFenced(t *testing.T) {
	d, err := Extract("Sure, here you go:\n```json\n{\"label\":\"tech\"}\n```\nhope that helps")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(d), `"label"`) {
		t.Errorf("got %s", d)
	}
}

func TestExtractProseWrapped(t *testing.T) {
	d, err := Extract(`The answer is {"decision":"yes","reason":"ok"} as shown.`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(d), `"decision"`) {
		t.Errorf("got %s", d)
	}
}

func TestExtractTrailingComma(t *testing.T) {
	if _, err := Extract(`{"a":1,"b":2,}`); err != nil {
		t.Fatalf("trailing-comma repair failed: %v", err)
	}
}

func TestExtractNoJSON(t *testing.T) {
	if _, err := Extract("just some prose, no object here"); err == nil {
		t.Error("expected ErrNoJSON")
	}
}
