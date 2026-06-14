package main

import (
	"reflect"
	"testing"
)

func TestHoistGlobalConfig(t *testing.T) {
	cases := []struct {
		name     string
		in       []string
		wantSub  string
		wantArgs []string
		wantOK   bool
	}{
		{"leading --config space", []string{"--config", "c.json", "triage", "f.txt"}, "triage", []string{"--config", "c.json", "f.txt"}, true},
		{"leading --config equals", []string{"--config=c.json", "classify", "x"}, "classify", []string{"--config", "c.json", "x"}, true},
		{"leading -config single dash", []string{"-config", "c.json", "models"}, "models", []string{"--config", "c.json"}, true},
		{"trailing --config untouched", []string{"triage", "f.txt", "--config", "c.json"}, "triage", []string{"f.txt", "--config", "c.json"}, true},
		{"no global config", []string{"summarize", "f.txt", "--json"}, "summarize", []string{"f.txt", "--json"}, true},
		{"config but no subcommand", []string{"--config", "c.json"}, "", nil, false},
		{"empty", []string{}, "", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub, args, ok := hoistGlobalConfig(tc.in)
			if ok != tc.wantOK || sub != tc.wantSub || !reflect.DeepEqual(args, tc.wantArgs) {
				t.Fatalf("hoistGlobalConfig(%v) = (%q, %v, %v); want (%q, %v, %v)",
					tc.in, sub, args, ok, tc.wantSub, tc.wantArgs, tc.wantOK)
			}
		})
	}
}
