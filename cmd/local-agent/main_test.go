package main

import (
	"strings"
	"testing"
)

func TestSplitObjectiveFlagsEitherSide(t *testing.T) {
	vf := map[string]bool{"root": true, "model": true, "max-steps": true, "egress-host": true}
	cases := []struct {
		name string
		args []string
		obj  string
		want string // expected flags, space-joined
	}{
		{"objective first then flags", []string{"do the thing", "--root", "/x", "--json"}, "do the thing", "--root /x --json"},
		{"flags first then objective", []string{"--root", "/x", "--json", "do the thing"}, "do the thing", "--root /x --json"},
		{"interleaved", []string{"--model", "m", "do it", "--json"}, "do it", "--model m --json"},
		{"equals form", []string{"go", "--root=/x", "--json"}, "go", "--root=/x --json"},
		{"repeatable egress-host", []string{"go", "--egress-host", "a.com", "--egress-host", "b.com"}, "go", "--egress-host a.com --egress-host b.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj, flags := splitObjective(c.args, vf)
			if obj != c.obj {
				t.Errorf("objective = %q, want %q", obj, c.obj)
			}
			if got := strings.Join(flags, " "); got != c.want {
				t.Errorf("flags = %q, want %q", got, c.want)
			}
		})
	}
}

// I-3: --profile and --two-tier are mutually exclusive. two-tier sets the
// architect/editor toolsets itself, so a non-default --profile there is silently
// ignored — reject the combination instead.
func TestValidateFlagCombo(t *testing.T) {
	cases := []struct {
		name    string
		twoTier bool
		profile string
		wantErr bool
	}{
		{"two-tier + build profile rejected", true, "build", true},
		{"two-tier + edit profile rejected", true, "edit", true},
		{"two-tier + general default ok", true, "general", false},
		{"two-tier + empty profile ok", true, "", false},
		{"single-loop + build ok", false, "build", false},
		{"single-loop + general ok", false, "general", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateFlagCombo(c.twoTier, c.profile)
			if c.wantErr && err == nil {
				t.Errorf("validateFlagCombo(%v, %q) = nil, want error", c.twoTier, c.profile)
			}
			if !c.wantErr && err != nil {
				t.Errorf("validateFlagCombo(%v, %q) = %v, want nil", c.twoTier, c.profile, err)
			}
		})
	}
}

func TestMultiFlag(t *testing.T) {
	var m multiFlag
	_ = m.Set("a.com")
	_ = m.Set("b.com")
	if len(m) != 2 || m[0] != "a.com" || m[1] != "b.com" {
		t.Errorf("multiFlag accumulation = %v, want [a.com b.com]", m)
	}
	if m.String() != "a.com,b.com" {
		t.Errorf("multiFlag.String() = %q, want a.com,b.com", m.String())
	}
}
