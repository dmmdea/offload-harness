package gpulease

import "testing"

func TestHeartbeatHelperInFindsAHelperBehindAnInterpreterInEitherSeparatorStyle(t *testing.T) {
	for _, c := range []struct {
		cmd  string
		want string
	}{
		{"node render/comfy-render.mjs out.png a cat", "comfy-render.mjs"},
		{`node D:\x y\render\Comfy-Generate.mjs --batch jobs.jsonl`, "comfy-generate.mjs"},
		{"node /opt/h/render/comfy-run-graph.mjs --graph g.json", "comfy-run-graph.mjs"},
		{"bash bench.sh", ""},
		{"node render/sdcpp-generate.mjs o.png", ""},
		{"", ""},
	} {
		got, ok := HeartbeatHelperInCommand(c.cmd)
		if ok != (c.want != "") || got != c.want {
			t.Errorf("%q: got (%q, %v), want %q", c.cmd, got, ok, c.want)
		}
	}
}
