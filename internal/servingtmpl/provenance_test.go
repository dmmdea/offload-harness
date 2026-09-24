package servingtmpl

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediaseat"
	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

var stampedAt = time.Date(2026, 9, 14, 11, 22, 33, 0, time.UTC)

func basis() SpecBasis {
	return SpecBasis{
		HarnessVersion:      "0.123.0",
		TierID:              "ampere-16",
		TemplateSHA256:      strings.Repeat("a", 64),
		ProfilesEntrySHA256: strings.Repeat("b", 64),
		Render:              RenderBasis{RAMTier: "mid", FallbackBackend: ""},
		Params:              BasisOf(params()),
	}
}

// TestParamsBasisMirrorsParams is the gate that keeps the hashed set CLOSED
// rather than merely closed-today. A field added to Params and not to
// ParamsBasis would fall silently outside the spec hash: the config would render
// differently and the provenance stamp would swear nothing changed. That is the
// exact failure mode this row exists to end, so it fails the build instead.
func TestParamsBasisMirrorsParams(t *testing.T) {
	names := func(v any) []string {
		rt := reflect.TypeOf(v)
		out := make([]string, 0, rt.NumField())
		for i := 0; i < rt.NumField(); i++ {
			out = append(out, rt.Field(i).Name)
		}
		return out
	}
	got, want := names(ParamsBasis{}), names(Params{})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParamsBasis no longer mirrors Params one field for one field.\n"+
			"  Params:      %v\n  ParamsBasis: %v\n"+
			"Add the field to ParamsBasis, to BasisOf and to ParamsBasis.Params(), or the spec hash stops covering it.",
			want, got)
	}
	// The mirror must also carry values, not just names: a field added to
	// ParamsBasis but forgotten in BasisOf would pass the name check and hash a
	// zero value forever.
	p := params()
	p.Home, p.GOOS, p.Backend = "/srv/offload", "linux", "cuda"
	p.GPUEnv = []string{"CUDA_VISIBLE_DEVICES=0"}
	p.Seats = []mediaseat.Seat{{Kind: "vision", Name: "vlm", Model: "m.gguf", Residency: "swap"}}
	p.VLLMSeat = &vllmseat.Spec{ID: "seat", Unit: "u", Port: 18797, MaxModelLen: 131072}
	p.VLLMRuntime = vllmseat.Runtime{User: "someone", ProxyHost: "203.0.113.9"}
	p.IncludeQ38, p.IncludeQ359B, p.IncludeMimo9B, p.DisableCUDAGraphs = true, true, true, true
	// The composite tier’s display layer (ADR 0039) rides in the hashed set too:
	// left nil here, a BasisOf that forgot to carry it would round-trip cleanly
	// and hash nil forever on the one tier that actually sets it.
	p.DisplayLayer = &config.LayerSpec{
		Name: "display", Tier: "blackwell-16", Devices: []string{"GPU-abc"},
		Seats:   []config.LayerSeat{{Role: "agent", Model: "qwen3.8-27b-display", Device: "1", CtxTokens: 65536}},
		Dormant: true, DisplayDevice: "GPU-abc", DisplayFloorGiB: 4, Guards: []string{"display_floor"},
	}
	if rt := BasisOf(p).Params(); !reflect.DeepEqual(rt, p) {
		t.Errorf("BasisOf/Params() is not a lossless round trip -- a field is dropped in one direction:\n got %+v\nwant %+v", rt, p)
	}
}

// TestSpecHashIsSensitiveToEveryInput is the hash-sensitivity table: each input
// of the closed set, mutated alone, must move the hash. A hash that ignores one
// of its documented inputs is worse than no hash -- it asserts sameness over a
// difference, which is how a stale config gets certified as current.
func TestSpecHashIsSensitiveToEveryInput(t *testing.T) {
	base, _, err := SpecHash(basis())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		field string
		mut   func(*SpecBasis)
	}{
		{"harness_version", func(b *SpecBasis) { b.HarnessVersion = "0.118.1" }},
		{"tier_id", func(b *SpecBasis) { b.TierID = "ampere-8" }},
		{"template_sha256", func(b *SpecBasis) { b.TemplateSHA256 = strings.Repeat("c", 64) }},
		{"profiles_entry_sha256", func(b *SpecBasis) { b.ProfilesEntrySHA256 = strings.Repeat("d", 64) }},
		{"render.ram_tier", func(b *SpecBasis) { b.Render.RAMTier = "high" }},
		{"render.fallback_backend", func(b *SpecBasis) { b.Render.FallbackBackend = "cuda" }},
		{"params.llama_bin", func(b *SpecBasis) { b.Params.LlamaBin = "/other/bin" }},
		{"params.models_dir", func(b *SpecBasis) { b.Params.ModelsDir = "/other/models" }},
		{"params.listen", func(b *SpecBasis) { b.Params.Listen = "0.0.0.0:11436" }},
		{"params.ctx_size", func(b *SpecBasis) { b.Params.Ctx = 131072 }},
		{"params.kv_type", func(b *SpecBasis) { b.Params.KVType = "f16" }},
		{"params.flash_attn", func(b *SpecBasis) { b.Params.FlashAttn = "off" }},
		{"params.moe_26b", func(b *SpecBasis) { b.Params.MoE26B = "--n-cpu-moe 14 -ngl 999" }},
		{"params.threads", func(b *SpecBasis) { b.Params.Threads = 16 }},
		{"params.include_26b", func(b *SpecBasis) { b.Params.Include26B = false }},
		{"params.include_qwen38", func(b *SpecBasis) { b.Params.IncludeQ38 = true }},
		{"params.include_qwen35_4b", func(b *SpecBasis) { b.Params.IncludeQ354B = true }},
		{"params.include_qwen35_9b", func(b *SpecBasis) { b.Params.IncludeQ359B = true }},
		{"params.include_mimo_9b", func(b *SpecBasis) { b.Params.IncludeMimo9B = true }},
		{"params.seats", func(b *SpecBasis) {
			b.Params.Seats = []mediaseat.Seat{{Kind: "vision", Name: "vlm", Model: "m.gguf", Residency: "swap"}}
		}},
		{"params.home", func(b *SpecBasis) { b.Params.Home = "/srv/offload" }},
		{"params.goos", func(b *SpecBasis) { b.Params.GOOS = "windows" }},
		{"params.gpu_env", func(b *SpecBasis) { b.Params.GPUEnv = []string{"CUDA_VISIBLE_DEVICES=0"} }},
		{"params.backend", func(b *SpecBasis) { b.Params.Backend = "vulkan" }},
		{"params.disable_cuda_graphs", func(b *SpecBasis) { b.Params.DisableCUDAGraphs = true }},
		{"params.vllm_seat", func(b *SpecBasis) { b.Params.VLLMSeat = &vllmseat.Spec{ID: "seat", MaxModelLen: 131072} }},
		{"params.vllm_runtime", func(b *SpecBasis) { b.Params.VLLMRuntime = vllmseat.Runtime{User: "someone"} }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			b := basis()
			tc.mut(&b)
			got, _, err := SpecHash(b)
			if err != nil {
				t.Fatal(err)
			}
			if got == base {
				t.Errorf("changing %s did not change the spec hash -- that input is not covered by the closed set", tc.field)
			}
		})
	}
}

// TestSpecHashIgnoresWhatIsNotAnInput is the other half of the table: the hash
// must NOT move on things that are not render inputs. rendered_at is the one
// that matters -- it rides in the stamp, and a spec hash that included it would
// differ on every render of an identical config, making the fleet-wide string
// comparison in /fleet/health meaningless.
func TestSpecHashIgnoresWhatIsNotAnInput(t *testing.T) {
	b := basis()
	one, err := Stamp("models: {}\n", b, stampedAt)
	if err != nil {
		t.Fatal(err)
	}
	two, err := Stamp("models: {}\n", b, stampedAt.Add(72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	s1, ok1 := ParseStamp(one)
	s2, ok2 := ParseStamp(two)
	if !ok1 || !ok2 {
		t.Fatal("stamped configs did not parse back")
	}
	if s1.RenderedAt == s2.RenderedAt {
		t.Fatal("the two stamps carry the same rendered_at -- the case under test did not happen")
	}
	if s1.SpecSHA256 != s2.SpecSHA256 {
		t.Errorf("rendered_at moved the spec hash (%s vs %s); it is stamp metadata, not a render input", short(s1.SpecSHA256), short(s2.SpecSHA256))
	}
	if s1.BodySHA256 != s2.BodySHA256 {
		t.Errorf("rendered_at moved the body hash (%s vs %s)", short(s1.BodySHA256), short(s2.BodySHA256))
	}
}

// TestStampKeepsTheBodyByteIdentical is the stamp/body separation: the block is
// prepended, and what comes back out is the rendered bytes exactly. If the stamp
// rewrote so much as a newline, body_sha256 would certify the stamp's own
// handiwork and a hand edit would be undetectable.
func TestStampKeepsTheBodyByteIdentical(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"lf", "models:\n  a:\n    cmd: x\n"},
		{"crlf", "models:\r\n  a:\r\n    cmd: x\r\n"},
		{"no-trailing-newline", "models:\n  a:\n    cmd: x"},
		{"leading-comment", "# a template comment\nmodels: {}\n"},
		{"blank-first-line", "\nmodels: {}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stamped, err := Stamp(tc.body, basis(), stampedAt)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(stamped, tc.body) {
				t.Fatalf("the stamped file does not end with the rendered body verbatim:\n%q", stamped)
			}
			st, ok := ParseStamp(stamped)
			if !ok {
				t.Fatal("a freshly stamped config did not parse back as stamped")
			}
			if st.Body != tc.body {
				t.Errorf("body round trip lost bytes:\n got %q\nwant %q", st.Body, tc.body)
			}
			if got := SHA256Hex([]byte(st.Body)); got != st.BodySHA256 {
				t.Errorf("body_sha256 %s does not hash the body it sits above (%s)", short(st.BodySHA256), short(got))
			}
			want, _, err := SpecHash(basis())
			if err != nil {
				t.Fatal(err)
			}
			if st.SpecSHA256 != want {
				t.Errorf("spec_sha256 = %s, want %s", short(st.SpecSHA256), short(want))
			}
			if st.TierID != "ampere-16" || st.RenderedBy != "0.123.0" || st.RenderedAt != "2026-09-14T11:22:33Z" {
				t.Errorf("stamp metadata lost: tier=%q rendered_by=%q rendered_at=%q", st.TierID, st.RenderedBy, st.RenderedAt)
			}
			var decoded SpecBasis
			if err := json.Unmarshal([]byte(st.BasisJSON), &decoded); err != nil {
				t.Fatalf("stamped basis is not decodable JSON: %v", err)
			}
			if decoded.Params.Ctx != params().Ctx {
				t.Errorf("stamped basis lost params.ctx_size: %d", decoded.Params.Ctx)
			}
		})
	}
}

// TestUnstampedConfigParsesAsUnstamped: a config written before this version, or
// by hand, must be reported as UNSTAMPED and never mistaken for a stamped one
// whose block happens to be missing keys.
func TestUnstampedConfigParsesAsUnstamped(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"plain", "models:\n  a: {}\n"},
		{"other-comment", "# some other generator\nmodels: {}\n"},
		{"lead-only", stampLead + "\nmodels: {}\n"},
		{"missing-basis", stampLead + "\n# spec_sha256: x\n# body_sha256: y\nmodels: {}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := ParseStamp(tc.text); ok {
				t.Errorf("%q parsed as stamped; a half-written block is not provenance", tc.name)
			}
			if r := AgainstRender(tc.text, basis(), "models: {}\n"); r.State != StateUnstamped {
				t.Errorf("state = %s, want UNSTAMPED", r.State)
			}
		})
	}
}

// TestAgainstRenderVerdicts covers the four states over one stamped file. The
// ORDER is asserted too: a file that is both hand-edited and stale must report
// HAND-EDITED, because re-rendering a hand-edited config destroys the edit.
func TestAgainstRenderVerdicts(t *testing.T) {
	body := "models:\n  a:\n    cmd: llama-server --ctx-size 32768\n    ttl: 300\n"
	stamped, err := Stamp(body, basis(), stampedAt)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("match", func(t *testing.T) {
		r := AgainstRender(stamped, basis(), body)
		if r.State != StateMatch {
			t.Fatalf("state = %s (%s), want MATCH", r.State, r.Detail)
		}
		if len(r.Keys) != 0 {
			t.Errorf("MATCH named changed keys %v", r.Keys)
		}
	})

	t.Run("match-across-a-version-bump", func(t *testing.T) {
		// A release that did not change this tier's output must not read as
		// drift; a verdict wired to the version puts the whole fleet STALE
		// forever after the next bump.
		fresh := basis()
		fresh.HarnessVersion = "0.119.7"
		r := AgainstRender(stamped, fresh, body)
		if r.State != StateMatch {
			t.Fatalf("state = %s (%s), want MATCH across a version bump", r.State, r.Detail)
		}
		if !strings.Contains(r.Detail, "0.119.7") {
			t.Errorf("MATCH did not report the version delta: %q", r.Detail)
		}
	})

	t.Run("stale-names-the-key", func(t *testing.T) {
		fresh := basis()
		fresh.Params.Ctx = 131072
		fresh.ProfilesEntrySHA256 = strings.Repeat("e", 64)
		r := AgainstRender(stamped, fresh, strings.Replace(body, "32768", "131072", 1))
		if r.State != StateStale {
			t.Fatalf("state = %s (%s), want STALE", r.State, r.Detail)
		}
		if !contains(r.Keys, "params.ctx_size") {
			t.Errorf("STALE keys %v do not name params.ctx_size -- a verdict that cannot say WHAT moved is not actionable", r.Keys)
		}
		if !contains(r.Keys, "profiles_entry_sha256") {
			t.Errorf("STALE keys %v do not name profiles_entry_sha256", r.Keys)
		}
		if contains(r.Keys, "params.kv_type") {
			t.Errorf("STALE named an unchanged key: %v", r.Keys)
		}
	})

	t.Run("seed-moved-but-text-unchanged-is-stale-not-match", func(t *testing.T) {
		// The re-derive pins the per-box half it cannot know, so a seed field
		// that half covers can move without changing the replayed text. Calling
		// that MATCH is the certified-stale-as-current failure this row exists
		// to end -- but the detail must say the served config is fine.
		fresh := basis()
		fresh.ProfilesEntrySHA256 = strings.Repeat("f", 64)
		r := AgainstRender(stamped, fresh, body)
		if r.State != StateStale {
			t.Fatalf("state = %s (%s), want STALE when a seed moved", r.State, r.Detail)
		}
		if !contains(r.Keys, "profiles_entry_sha256") {
			t.Errorf("keys %v do not name profiles_entry_sha256", r.Keys)
		}
		if !strings.Contains(r.Detail, "nothing served is wrong") {
			t.Errorf("detail must separate a stale STAMP from a stale CONFIG: %q", r.Detail)
		}
	})

	t.Run("stale-with-no-named-key-is-said-plainly", func(t *testing.T) {
		// Every hashed input identical, output different: only the renderer's own
		// code can do that, and the report must say so rather than name nothing.
		r := AgainstRender(stamped, basis(), body+"# renderer changed\n")
		if r.State != StateStale {
			t.Fatalf("state = %s, want STALE", r.State)
		}
		if len(r.Keys) != 0 {
			t.Errorf("keys = %v, want none", r.Keys)
		}
		if !strings.Contains(r.Detail, "renderer's code changed") {
			t.Errorf("detail does not explain a keyless STALE: %q", r.Detail)
		}
	})

	t.Run("untenderable-tier-is-stale-not-match", func(t *testing.T) {
		fresh := basis()
		fresh.ProfilesEntrySHA256 = ""
		r := AgainstRender(stamped, fresh, "")
		if r.State != StateStale {
			t.Fatalf("state = %s, want STALE when the tier can no longer be rendered", r.State)
		}
		if !strings.Contains(r.Detail, "no longer in the tier table") {
			t.Errorf("detail = %q", r.Detail)
		}
	})

	t.Run("hand-edited-body", func(t *testing.T) {
		edited := strings.Replace(stamped, "ttl: 300", "ttl: 1800", 1)
		r := AgainstRender(edited, basis(), body)
		if r.State != StateHandEdited {
			t.Fatalf("state = %s (%s), want HAND-EDITED", r.State, r.Detail)
		}
	})

	t.Run("hand-edited-body-beats-stale", func(t *testing.T) {
		// Both conditions true at once. Re-rendering would silently discard the
		// operator's edit, so the edit is what must be reported.
		edited := strings.Replace(stamped, "ttl: 300", "ttl: 1800", 1)
		fresh := basis()
		fresh.Params.Ctx = 131072
		r := AgainstRender(edited, fresh, strings.Replace(body, "32768", "131072", 1))
		if r.State != StateHandEdited {
			t.Fatalf("state = %s, want HAND-EDITED to win over STALE", r.State)
		}
	})

	t.Run("hand-edited-stamp", func(t *testing.T) {
		// The basis itself doctored to match a doctored body: the stamp is then
		// internally inconsistent, and without the spec-hash-over-basis check
		// this would re-derive clean and report MATCH.
		doctored := strings.Replace(stamped, `"ctx_size":32768`, `"ctx_size":131072`, 1)
		if doctored == stamped {
			t.Fatal("the basis line does not carry ctx_size -- the case under test did not happen")
		}
		r := AgainstRender(doctored, basis(), body)
		if r.State != StateHandEdited {
			t.Fatalf("state = %s (%s), want HAND-EDITED for a doctored stamp", r.State, r.Detail)
		}
	})
}

// TestEveryShippedTemplateStampsAndSelfVerifies is the whole-surface gate: every
// template this binary can ship renders, stamps, and verifies MATCH against its
// own render. A template whose output the stamp cannot round-trip would ship a
// config that reads HAND-EDITED the moment it is installed.
func TestEveryShippedTemplateStampsAndSelfVerifies(t *testing.T) {
	checked := 0
	for _, name := range templateNames(t) {
		t.Run(name, func(t *testing.T) {
			tmpl := readTemplate(t, name)
			p := params()
			out, err := Render(string(tmpl), p)
			if err != nil {
				t.Skipf("%s does not render under the baseline params: %v", name, err)
			}
			b := basis()
			b.Params = BasisOf(p)
			b.TemplateSHA256 = SHA256Hex(tmpl)
			stamped, err := Stamp(out, b, stampedAt)
			if err != nil {
				t.Fatalf("%s: stamping refused: %v", name, err)
			}
			if vs := Audit(stamped); len(vs) != 0 {
				t.Errorf("%s: the stamp broke the rule audit (the block must be inert yaml):\n%s", name, Violations(vs))
			}
			r := AgainstRender(stamped, b, out)
			if r.State != StateMatch {
				t.Fatalf("%s: a freshly stamped config does not verify against its own render: %s -- %s (keys %v)", name, r.State, r.Detail, r.Keys)
			}
			checked++
		})
	}
	if checked == 0 {
		t.Fatal("no template rendered -- the gate would silently pass on nothing")
	}
}

// TestReportLineIsGreppable pins the CLI line shape p1-audit.sh and the docs
// both key on: "<path>: <STATE>(<keys>) -- <detail>".
func TestReportLineIsGreppable(t *testing.T) {
	r := Report{State: StateStale, Keys: []string{"params.ctx_size"}, Detail: "why"}
	if got, want := r.Line("/x/llama-swap.yaml"), "/x/llama-swap.yaml: STALE(params.ctx_size) -- why"; got != want {
		t.Errorf("Line() = %q, want %q", got, want)
	}
	if got, want := (Report{State: StateMatch}).Line("/x"), "/x: MATCH"; got != want {
		t.Errorf("Line() = %q, want %q", got, want)
	}
}

// TestCanonicalEntrySHAIgnoresFormattingNotContent: reformatting profiles.json
// must not mark every box stale, but a value change must be caught.
func TestCanonicalEntrySHAIgnoresFormattingNotContent(t *testing.T) {
	a, err := CanonicalEntrySHA([]byte(`{"ctx_size":32768,"kv_type":"q8_0"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := CanonicalEntrySHA([]byte("{\n  \"kv_type\" : \"q8_0\",\n  \"ctx_size\": 32768\n}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("reformatting the tier entry changed its hash (%s vs %s)", short(a), short(b))
	}
	c, err := CanonicalEntrySHA([]byte(`{"ctx_size":131072,"kv_type":"q8_0"}`))
	if err != nil {
		t.Fatal(err)
	}
	if c == a {
		t.Error("changing ctx_size did not change the tier entry hash")
	}
	if empty, err := CanonicalEntrySHA(nil); err != nil || empty != "" {
		t.Errorf("an absent entry must hash to the empty string, got %q (%v)", empty, err)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestStampNeverReintroducesAnUnsubstitutedToken is the regression gate for the
// defect CI caught and review did not: the stamp writes the render basis into
// the config, the basis legitimately carries a tier's `__OFFLOAD_HOME__` seat
// paths unsubstituted, and a rendered serving config must contain NO
// `__TOKEN__` anywhere. Render refuses to emit one and setup/render.tests.ps1
// greps every rendered config for exactly this pattern on every tier -- four
// tiers went red.
//
// The escape must not cost anything: the basis still decodes to the same struct
// and the spec hash is unchanged, both asserted here.
func TestStampNeverReintroducesAnUnsubstitutedToken(t *testing.T) {
	b := basis()
	b.Params.Home = "__OFFLOAD_HOME__"
	b.Params.Seats = []mediaseat.Seat{{
		Kind: "vision", Name: "vlm", Model: "m.gguf", Residency: "swap",
		Bin:    "__OFFLOAD_HOME__/bin/llama-server.exe",
		LibDir: "__OFFLOAD_HOME__/bin",
	}}
	stamped, err := Stamp("models: {}\n", b, stampedAt)
	if err != nil {
		t.Fatal(err)
	}
	if tokenRe.MatchString(stamped) {
		t.Errorf("the stamped config carries an unsubstituted token -- Render refuses such a config and the installer self-test greps for it:\n%s", tokenRe.FindString(stamped))
	}
	st, ok := ParseStamp(stamped)
	if !ok {
		t.Fatal("the escaped stamp did not parse back")
	}
	got, err := st.Basis()
	if err != nil {
		t.Fatal(err)
	}
	// The escape is TRANSPORT only: the decoded basis must be the original,
	// token for token, or the re-derive would diff against a mangled record.
	if got.Params.Home != "__OFFLOAD_HOME__" || got.Params.Seats[0].Bin != "__OFFLOAD_HOME__/bin/llama-server.exe" {
		t.Errorf("the escape changed the basis: home=%q bin=%q", got.Params.Home, got.Params.Seats[0].Bin)
	}
	want, _, err := SpecHash(b)
	if err != nil {
		t.Fatal(err)
	}
	if st.SpecSHA256 != want {
		t.Errorf("the escape moved the spec hash (%s, want %s)", short(st.SpecSHA256), short(want))
	}
	if again, _, err := SpecHash(got); err != nil || again != want {
		t.Errorf("the decoded basis does not re-hash to the stamped spec hash (%s, want %s)", short(again), short(want))
	}
	// A run of three underscores must not leave a doubled pair behind.
	if strings.Contains(escapeDoubleUnderscore([]byte("a___b____c")), "__") {
		t.Error("escapeDoubleUnderscore left a doubled underscore in a longer run")
	}
}
