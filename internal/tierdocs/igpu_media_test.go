package tierdocs

import "testing"

// The CT-49 iGPU engine keys are media keys, a nested seed object renders as JSON, and the README media
// column names the engines a tier binds (CT-51 I5).
func TestIGPUEngineSeedKeysAreMediaAndRenderReadably(t *testing.T) {
	for _, k := range []string{"animategen_engine", "animategen_sdcpp_bin", "animategen_script", "audiocpp_bin", "audiocpp_voice_model"} {
		if !mediaSeedKey(k) {
			t.Errorf("%s must be a media seed key", k)
		}
	}
	if got := valueString(map[string]any{"b": float64(2), "a": map[string]any{"x": "y"}}); got != `{"a":{"x":"y"},"b":2}` {
		t.Errorf("a nested seed object renders as %q", got)
	}
	seed := map[string]any{
		"videogen_families": map[string]any{"fastwan": map[string]any{"engine": "sdcpp"}},
		"animategen_engine": "sdcpp", "voicegen_engine": "audiocpp", "musicgen_engine": "audiocpp",
	}
	if got, want := mediaSummary(seed), "`sdcpp` video+animate, `audiocpp` voice+music"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if got := mediaSummary(map[string]any{"compose_script": "x"}); got != "compose only" {
		t.Errorf("a compose-only tier summary = %q", got)
	}
	if got := mediaSummary(map[string]any{"imagegen_engine": "sdcpp", "animategen_engine": "sdcpp"}); got != "`sdcpp`" {
		t.Errorf("an image engine still names the tier: %q", got)
	}
}
