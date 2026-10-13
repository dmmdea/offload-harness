package sttremote

// words_reliable_test.go pins D-38 #169 on the remote route: a node that judged its transcript's
// word times (words_reliable / words_drift_sec in its answer) has that verdict re-published in the
// answer THIS box builds in materialize. Before it, nodeResult had no field for either, so a
// drifted transcript transcribed on a fleet node reached the asker without the flag.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/sttclient"
)

// nodeAnswerWith is the node's transcribeResult JSON with extra top-level keys merged in (the
// verdict a newer node adds, or nothing for an older one).
func nodeAnswerWith(t *testing.T, segs []sttclient.Segment, truncated bool, extra map[string]any) core.Result {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(sttData(segs, truncated, nodeDir+"up-1.segments.json"), &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range extra {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return core.Result{OK: true, Data: b, Meta: core.Meta{Model: "whisper-stt"}}
}

func TestRemoteRepublishesTheNodesWordDriftVerdict(t *testing.T) {
	setBusy(t, false)
	for _, c := range []struct {
		name      string
		extra     map[string]any
		truncated bool
		want      map[string]string // key -> raw JSON, "" = the key must be absent
	}{
		{"drifted", map[string]any{"words_reliable": false, "words_drift_sec": 58.8}, false,
			map[string]string{"words_reliable": "false", "words_drift_sec": "58.8"}},
		{"aligned", map[string]any{"words_reliable": true}, false,
			map[string]string{"words_reliable": "true", "words_drift_sec": ""}},
		{"drifted and the node cut its inline list", map[string]any{"words_reliable": false, "words_drift_sec": 12.5}, true,
			map[string]string{"words_reliable": "false", "words_drift_sec": "12.5"}},
		{"an older node that sends no verdict", nil, false,
			map[string]string{"words_reliable": "", "words_drift_sec": ""}},
	} {
		t.Run(c.name, func(t *testing.T) {
			node := newFakeNode(t, "node-c", sttTasks, nodeAnswerWith(t, sampleSegments, c.truncated, c.extra))
			if c.truncated {
				full, _ := json.MarshalIndent(sampleSegments, "", "  ")
				node.media["up-1.segments.json"] = full
			}
			cfg := askerCfg(t, node, fakeFFmpeg(t, false))
			got := Run(context.Background(), cfg, &localRunner{}, sttReq(srcAudio(t), nil), "remote")
			if !got.OK {
				t.Fatalf("result = %+v", got)
			}
			var data map[string]json.RawMessage
			if err := json.Unmarshal(got.Data, &data); err != nil {
				t.Fatal(err)
			}
			for key, want := range c.want {
				have, present := data[key]
				switch {
				case want == "" && present:
					t.Errorf("%s = %s in the answer, want the key absent (the node sent none)", key, have)
				case want != "" && !present:
					t.Errorf("%s is absent from the answer (keys: %v), want %s (the node's verdict, re-published)", key, keysOf(data), want)
				case want != "" && string(have) != want:
					t.Errorf("%s = %s in the answer, want %s (the node's verdict, re-published)", key, have, want)
				}
			}
		})
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
