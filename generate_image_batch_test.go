package main

// `generate-image --batch` with a family, a results file and --resume (P0 plan S3; ADR 0083 decision 12), end to end
// through the verb: a real pipeline, the real render-helper protocol (a node stub that reads the jobs file and writes the
// results file the way render/comfy-generate.mjs does, and writes real PNGs), the real results file. What decides where a
// batch runs, and the tickets, are mediaremote's tests; this file is the CLI's: the flags, the rows on disk, the exit codes.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/imagegen"
	"github.com/dmmdea/offload-harness/internal/mediaremote"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// pngBatchStub is a --batch runner that renders every job as a real PNG of the size the job names (64x32 when it names
// none), counts the renders (one line per job in $STUB_COUNT) and records its argv ($STUB_ARGV).
const pngBatchStub = `import {readFileSync, writeFileSync, appendFileSync} from "node:fs";
import {deflateSync} from "node:zlib";
const a = process.argv.slice(2);
if (process.env.STUB_ARGV) writeFileSync(process.env.STUB_ARGV, JSON.stringify(a));
// What the caller's results file held the moment the render began (the rows of the jobs that ended before it).
if (process.env.STUB_WATCH && process.env.STUB_WATCH_COPY) { try { writeFileSync(process.env.STUB_WATCH_COPY, readFileSync(process.env.STUB_WATCH)); } catch {} }
const jobs = readFileSync(a[a.indexOf("--batch") + 1], "utf8").split("\n").filter(l => l.trim());
const table = new Uint32Array(256).map((_, n) => { let c = n; for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1; return c >>> 0; });
const crc = (b) => { let c = 0xffffffff; for (const x of b) c = table[(c ^ x) & 255] ^ (c >>> 8); return (c ^ 0xffffffff) >>> 0; };
const chunk = (t, d) => { const len = Buffer.alloc(4); len.writeUInt32BE(d.length); const td = Buffer.concat([Buffer.from(t), d]); const c = Buffer.alloc(4); c.writeUInt32BE(crc(td)); return Buffer.concat([len, td, c]); };
const png = (w, h) => { const ihdr = Buffer.alloc(13); ihdr.writeUInt32BE(w, 0); ihdr.writeUInt32BE(h, 4); ihdr[8] = 8; ihdr[9] = 2;
  return Buffer.concat([Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]), chunk("IHDR", ihdr), chunk("IDAT", deflateSync(Buffer.alloc((1 + w * 3) * h))), chunk("IEND", Buffer.alloc(0))]); };
const rows = jobs.map((l, i) => {
  const j = JSON.parse(l);
  if (process.env.STUB_COUNT) appendFileSync(process.env.STUB_COUNT, j.out + "\n");
  writeFileSync(j.out, png(j.width || 64, j.height || 32));
  return JSON.stringify({i, out: j.out, seed: j.seed, ok: true, ms: 1});
});
writeFileSync(a[a.indexOf("--results") + 1], rows.join("\n") + "\n");
`

// pngBatchHome is a hermetic home with the PNG stub as the image runner, a default binding ("stub-family") and one named
// family ("alt") with its own checkpoint, card pin and license.
func pngBatchHome(t *testing.T) (home, cfgPath, countPath, argvPath string) {
	t.Helper()
	requireNode(t)
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("COMFY_API", "http://127.0.0.1:9") // gpugen frees the ComfyUI it is told about: never one that is running
	stub := filepath.Join(home, "png-batch-stub.mjs")
	if err := os.WriteFile(stub, []byte(pngBatchStub), 0o644); err != nil {
		t.Fatal(err)
	}
	countPath, argvPath = filepath.Join(home, "renders.txt"), filepath.Join(home, "argv.json")
	t.Setenv("STUB_COUNT", countPath)
	t.Setenv("STUB_ARGV", argvPath)
	cfgPath = filepath.Join(home, "config.json")
	cfgJSON := `{"endpoint":"http://127.0.0.1:1","state_dir":` + strconvQuote(filepath.ToSlash(home)) + `,"gpu_wait_ms":5000,"media_dir":` + strconvQuote(filepath.ToSlash(filepath.Join(home, "media"))) +
		`,"ledger_path":` + strconvQuote(filepath.ToSlash(filepath.Join(home, "ledger.jsonl"))) + `,"imagegen_script":` + strconvQuote(filepath.ToSlash(stub)) +
		`,"imagegen_family":"stub-family","imagegen_ckpt":"default.safetensors","imagegen_license":"Default License","imagegen_commercial_use":true,
		"imagegen_families":{"alt":{"license":"Alt License","commercial_use":false,"imagegen_family":"alt","imagegen_ckpt":"alt.safetensors"}}}`
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return home, cfgPath, countPath, argvPath
}

func writeJobs(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rendersSoFar(t *testing.T, countPath string) int {
	t.Helper()
	b, err := os.ReadFile(countPath)
	if err != nil {
		return 0
	}
	return len(strings.Fields(string(b)))
}

type resultRow struct {
	I       int    `json:"i"`
	Out     string `json:"out"`
	Seed    int    `json:"seed"`
	OK      bool   `json:"ok"`
	Ms      int64  `json:"ms"`
	Error   string `json:"error"`
	Node    string `json:"node"`
	Family  string `json:"family"`
	SHA256  string `json:"sha256"`
	Verdict string `json:"verdict"`
}

func readResults(t *testing.T, path string) []resultRow {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the results file: %v", err)
	}
	var rows []resultRow
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r resultRow
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("results line %q: %v", l, err)
		}
		rows = append(rows, r)
	}
	return rows
}

func hashOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// The verb end to end: a verdict and a hash per job in the results file at the path asked for, the pictures at exactly the
// outs given, a job that cannot be placed failing alone (exit 4), and --resume skipping only what proves itself.
func TestGenerateImageBatchResultsFileAndResume(t *testing.T) {
	home, cfgPath, countPath, _ := pngBatchHome(t)
	blocker := filepath.Join(home, "afile")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outs := []string{filepath.Join(home, "pics", "one.png"), filepath.Join(home, "pics", "two.png"), filepath.Join(blocker, "three.png")}
	jobs := filepath.Join(home, "jobs.jsonl")
	writeJobs(t, jobs,
		`{"prompt":"a red bike","out":`+strconvQuote(filepath.ToSlash(outs[0]))+`,"seed":101,"width":64,"height":32,"negative":"people"}`,
		`{"prompt":"a blue car","out":`+strconvQuote(filepath.ToSlash(outs[1]))+`,"seed":102,"width":96,"height":48}`,
		`{"prompt":"a green boat","out":`+strconvQuote(filepath.ToSlash(outs[2]))+`,"seed":103}`)
	results := filepath.Join(home, "r", "results.jsonl")
	watched := filepath.Join(home, "watched.jsonl")
	t.Setenv("STUB_WATCH", results+".partial") // the rows are kept beside the target until the batch ends
	t.Setenv("STUB_WATCH_COPY", watched)
	run := func(extra ...string) (string, error) {
		var err error
		out := captureStdout(t, func() {
			err = runGenerateImage(append([]string{"--batch", jobs, "--config", cfgPath, "--results", results, "--json"}, extra...))
		})
		return out, err
	}

	// 1. The first run: two pictures, one job that cannot be placed.
	out, err := run()
	if code, line := exitStatus(err); code != imagegen.BatchExitJobsFailed || !strings.Contains(line, "1 of 3 jobs failed") {
		t.Fatalf("a batch with a failed job must end with exit %d, got err=%v (exit %d, %q)", imagegen.BatchExitJobsFailed, err, code, line)
	}
	rows := readResults(t, results)
	if len(rows) != 3 {
		t.Fatalf("results rows = %+v", rows)
	}
	for i := 0; i < 2; i++ {
		r := rows[i]
		if r.I != i || r.Out != filepath.ToSlash(outs[i]) || !r.OK || r.Verdict != "rendered" || r.Seed != 101+i || r.Node == "" || r.Family != "stub-family" || r.SHA256 != hashOf(t, outs[i]) {
			t.Errorf("row %d = %+v, want a rendered row with the job's out and seed, the node, the default family and the hash of the file", i, r)
		}
	}
	if r := rows[2]; r.OK || r.Verdict != "failed" || !strings.Contains(r.Error, "cannot make the folder for out") || r.SHA256 != "" {
		t.Errorf("row 2 = %+v, want failed naming the folder", r)
	}
	if n := rendersSoFar(t, countPath); n != 2 {
		t.Fatalf("%d render(s), want 2 (the unplaceable job costs no render)", n)
	}
	// The row of the job that failed before the render began was already on disk when the render began: a batch that dies
	// mid-run leaves the jobs that ended (in <results>.partial; the results file itself is replaced only when the batch ends).
	if seen, _ := os.ReadFile(watched); !strings.Contains(string(seen), `"verdict":"failed"`) {
		t.Errorf("the partial results file held %q when the render began, want the failed job's row already in it", seen)
	}
	if _, err := os.Stat(results + ".partial"); err == nil {
		t.Errorf("the partial file was left behind by a batch that ended")
	}
	var printed struct {
		Result struct {
			Count, Succeeded, Failed int
			Mode                     string
			Items                    []map[string]any
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &printed); err != nil || printed.Result.Count != 3 || printed.Result.Succeeded != 2 || printed.Result.Failed != 1 || printed.Result.Mode != "warm" || len(printed.Result.Items) != 3 {
		t.Errorf("the printed JSON keeps its shape and adds the mode: %v %+v", err, printed.Result)
	}
	if v := printed.Result.Items[0]["verdict"]; v != "rendered" || printed.Result.Items[0]["sha256"] != rows[0].SHA256 || printed.Result.Items[0]["license"] != "Default License" {
		t.Errorf("a printed item carries the verdict, the hash and the binding's license: %v", printed.Result.Items[0])
	}

	// 2. --resume: the two pictures are there and prove themselves; nothing is rendered, the failed job fails again.
	out, err = run("--resume")
	if code, _ := exitStatus(err); code != imagegen.BatchExitJobsFailed {
		t.Fatalf("resume of a batch with one unplaceable job: %v", err)
	}
	var resumed struct {
		Result struct {
			Skipped int `json:"skipped"`
			Items   []map[string]any
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &resumed); err != nil || resumed.Result.Skipped != 2 || len(resumed.Result.Items) != 3 {
		t.Fatalf("the printed JSON counts what was skipped: %v %+v", err, resumed.Result)
	}
	for i := 0; i < 2; i++ {
		it := resumed.Result.Items[i]
		if it["verdict"] != "skipped-existing" || it["license"] != "Default License" || it["commercial_use"] != true || it["family"] != "stub-family" {
			t.Errorf("a skipped item carries its verdict and the binding's family and license like any other: %v", it)
		}
	}
	rows = readResults(t, results)
	for i := 0; i < 2; i++ {
		if r := rows[i]; r.Verdict != "skipped-existing" || !r.OK || r.Ms != 0 || r.Node != "" || r.SHA256 != hashOf(t, outs[i]) {
			t.Errorf("resumed row %d = %+v, want skipped-existing with the file's hash and no node", i, r)
		}
	}
	if n := rendersSoFar(t, countPath); n != 2 {
		t.Errorf("resume rendered again: %d render(s) in all, want still 2", n)
	}

	// 3. One picture lost, one cut short: those two render again, and only those.
	if err := os.Remove(outs[0]); err != nil {
		t.Fatal(err)
	}
	full, _ := os.ReadFile(outs[1])
	if err := os.WriteFile(outs[1], full[:len(full)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = run("--resume")
	if code, _ := exitStatus(err); code != imagegen.BatchExitJobsFailed {
		t.Fatalf("third run: %v", err)
	}
	rows = readResults(t, results)
	if rows[0].Verdict != "rendered" || rows[1].Verdict != "rendered" || rows[2].Verdict != "failed" {
		t.Errorf("rows = %+v, want the lost and the cut picture rendered again", rows)
	}
	if n := rendersSoFar(t, countPath); n != 4 {
		t.Errorf("%d render(s) in all, want 4 (2 + the 2 that did not prove themselves)", n)
	}
	if hashOf(t, outs[1]) != rows[1].SHA256 {
		t.Errorf("the re-rendered picture's hash is not the row's")
	}

	// 4. Without --resume everything that can render renders.
	if _, err = run(); err == nil {
		t.Errorf("the unplaceable job still fails the batch")
	}
	if n := rendersSoFar(t, countPath); n != 6 {
		t.Errorf("%d render(s) in all, want 6 (no flag, no skipping)", n)
	}
}

// --family reaches the renderer: its checkpoint is what the runner is asked for, the family's license is on the printed
// result, and a job without an out gets a name that depends on the family (the same prompt and seed under two families
// are two files).
func TestGenerateImageBatchFamilyReachesTheRunner(t *testing.T) {
	home, cfgPath, countPath, argvPath := pngBatchHome(t)
	jobs := filepath.Join(home, "jobs.jsonl")
	writeJobs(t, jobs, `{"prompt":"a red bike","seed":7}`)
	run := func(family string) (string, map[string]any) {
		args := []string{"--batch", jobs, "--config", cfgPath, "--json"}
		if family != "" {
			args = append(args, "--family", family)
		}
		var err error
		out := captureStdout(t, func() { err = runGenerateImage(args) })
		if err != nil {
			t.Fatalf("family %q: %v", family, err)
		}
		var res struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return out, res.Result
	}

	_, alt := run("alt")
	raw, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	var argv []string
	_ = json.Unmarshal(raw, &argv)
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--ckpt alt.safetensors") || !strings.Contains(joined, "--family alt") {
		t.Errorf("the runner was not asked for the family's binding: %s", joined)
	}
	if alt["family"] != "alt" || alt["license"] != "Alt License" || alt["commercial_use"] != false {
		t.Errorf("the printed result must carry the family's name and license: %v", alt)
	}
	altOut := alt["items"].([]any)[0].(map[string]any)["out"]

	_, def := run("")
	if def["family"] != "stub-family" || def["license"] != "Default License" {
		t.Errorf("no family is the default binding: %v", def)
	}
	if defOut := def["items"].([]any)[0].(map[string]any)["out"]; defOut == altOut {
		t.Errorf("the same prompt and seed under two families wrote one file: %v", defOut)
	}
	if n := rendersSoFar(t, countPath); n != 2 {
		t.Errorf("%d render(s), want 2", n)
	}
}

// The batch flags that need a batch, and the ones a batch refuses, are refused by name before anything runs.
func TestGenerateImageBatchFlagRefusals(t *testing.T) {
	home, cfgPath, countPath, _ := pngBatchHome(t)
	jobs := filepath.Join(home, "jobs.jsonl")
	writeJobs(t, jobs, `{"prompt":"a red bike"}`)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"--results alone":        {[]string{"a prompt", "--config", cfgPath, "--results", filepath.Join(home, "r.jsonl")}, "--results belongs to --batch"},
		"--resume alone":         {[]string{"a prompt", "--config", cfgPath, "--resume"}, "--resume belongs to --batch"},
		"--jobs alone":           {[]string{"a prompt", "--config", cfgPath, "--jobs", "2"}, "--jobs belongs to --batch"},
		"a negative --jobs":      {[]string{"--batch", jobs, "--config", cfgPath, "--jobs", "-3"}, "--jobs -3"},
		"--transparent":          {[]string{"--batch", jobs, "--config", cfgPath, "--transparent"}, "--transparent"},
		"an unknown family":      {[]string{"--batch", jobs, "--config", cfgPath, "--family", "nope", "--results", filepath.Join(home, "r.jsonl")}, `unknown image family "nope"`},
		"an unwritable results":  {[]string{"--batch", jobs, "--config", cfgPath, "--results", filepath.Join(home, "jobs.jsonl", "r.jsonl")}, "--results"},
		"a route nobody knows":   {[]string{"--batch", jobs, "--config", cfgPath, "--route", "everywhere"}, "unrecognized route"},
		"remote without a fleet": {[]string{"--batch", jobs, "--config", cfgPath, "--route", "remote"}, "delegate_remotes"},
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			captureStdout(t, func() { err = runGenerateImage(tc.args) })
			var ec *exitCodeError
			if err == nil || errors.As(err, &ec) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal containing %q, got %v", tc.want, err)
			}
		})
	}
	if n := rendersSoFar(t, countPath); n != 0 {
		t.Errorf("%d render(s) for refused commands", n)
	}
	// A refused batch leaves no results file behind.
	if _, err := os.Stat(filepath.Join(home, "r.jsonl")); err == nil {
		t.Errorf("a results file was left by a command that was refused")
	}
}

// --results never names the jobs file (it would be overwritten), and a command the batch refuses leaves an earlier run's
// results exactly as they were: the node, the time and the hash of every picture are only there.
func TestGenerateImageBatchResultsFileIsNeverTheJobsFileNorLostToARefusal(t *testing.T) {
	home, cfgPath, countPath, _ := pngBatchHome(t)
	jobs := filepath.Join(home, "jobs.jsonl")
	writeJobs(t, jobs, `{"prompt":"a red bike"}`)
	jobsBefore, _ := os.ReadFile(jobs)

	var err error
	captureStdout(t, func() { err = runGenerateImage([]string{"--batch", jobs, "--config", cfgPath, "--results", jobs}) })
	if err == nil || !strings.Contains(err.Error(), "same file as --batch") {
		t.Fatalf("--results naming the jobs file must be refused by name, got %v", err)
	}
	if after, _ := os.ReadFile(jobs); string(after) != string(jobsBefore) {
		t.Errorf("the jobs file was changed by a refused command: %q", after)
	}
	// the same file spelled another way
	if err := os.MkdirAll(filepath.Join(home, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := home + string(filepath.Separator) + "sub" + string(filepath.Separator) + ".." + string(filepath.Separator) + "jobs.jsonl"
	captureStdout(t, func() { err = runGenerateImage([]string{"--batch", jobs, "--config", cfgPath, "--results", alias}) })
	if err == nil || !strings.Contains(err.Error(), "same file as --batch") {
		t.Fatalf("an alias of the jobs file must be refused too, got %v", err)
	}

	// the same file under another name (a hard link), which no spelling shows
	link := filepath.Join(home, "link.jsonl")
	if lerr := os.Link(jobs, link); lerr == nil {
		captureStdout(t, func() { err = runGenerateImage([]string{"--batch", jobs, "--config", cfgPath, "--results", link}) })
		if err == nil || !strings.Contains(err.Error(), "same file as --batch") {
			t.Fatalf("a hard link to the jobs file must be refused too, got %v", err)
		}
	}

	results := filepath.Join(home, "r.jsonl")
	const earlier = "an earlier run's record\n"
	if err := os.WriteFile(results, []byte(earlier), 0o644); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() {
		err = runGenerateImage([]string{"--batch", jobs, "--config", cfgPath, "--family", "nope", "--resume", "--results", results})
	})
	if err == nil || !strings.Contains(err.Error(), `unknown image family "nope"`) {
		t.Fatalf("want the unknown family refused, got %v", err)
	}
	if raw, rerr := os.ReadFile(results); rerr != nil || string(raw) != earlier {
		t.Errorf("a refused re-run destroyed the earlier results: %q (%v)", raw, rerr)
	}
	if _, serr := os.Stat(results + ".partial"); serr == nil {
		t.Errorf("a refused command left a partial results file")
	}
	if n := rendersSoFar(t, countPath); n != 0 {
		t.Errorf("%d render(s) for refused commands", n)
	}
}

// refine_fallbacks counts the warm items whose refiner fell back, so it is reported only where it was measured: a batch that
// ran as tickets, one at a time, or not at all refined nothing here, and a 0 would claim every job was refined or opted out.
func TestBatchPayloadReportsRefineFallbacksOnlyWhereTheyWereMeasured(t *testing.T) {
	rows := []mediaremote.BatchRow{
		{I: 0, Out: "a", OK: true, Verdict: mediaremote.VerdictRendered},
		{I: 1, Out: "b", OK: true, Verdict: mediaremote.VerdictRendered},
	}
	items := []pipeline.ImageBatchItem{{Out: "a", OK: true, RefineFallback: "the refiner timed out"}, {Out: "b", OK: true}}
	for mode, want := range map[string]bool{
		mediaremote.ModeWarm: true, mediaremote.ModeTickets: false, mediaremote.ModeSerial: false, mediaremote.ModeNone: false,
	} {
		b := &mediaremote.Batch{Rows: rows, Mode: mode, WarmJobs: []int{0, 1}}
		p := batchPayload(b, config.FamilyInfo{}, true, items, true)
		got, has := p["refine_fallbacks"]
		if has != want {
			t.Errorf("mode %q: refine_fallbacks present=%v, want %v", mode, has, want)
		}
		if mode == mediaremote.ModeWarm && got != 1 {
			t.Errorf("warm: refine_fallbacks = %v, want the 1 item that fell back", got)
		}
	}
	// no refiner configured: absent, as it always was
	if _, has := batchPayload(&mediaremote.Batch{Rows: rows, Mode: mediaremote.ModeWarm, WarmJobs: []int{0, 1}}, config.FamilyInfo{}, true, items, false)["refine_fallbacks"]; has {
		t.Errorf("refine_fallbacks reported with no refiner configured")
	}
}

// Where the recipe was never compared (serial), the printed batch must not stamp THIS machine's family and licence on a
// picture some node rendered: the item carries the node's own words, and the header names no family at all.
func TestBatchPayloadSerialCarriesNoLocalFamilyOrLicense(t *testing.T) {
	no := false
	local := config.FamilyInfo{Name: "local-family", License: "Local License", CommercialUse: &no}
	node := true
	rows := []mediaremote.BatchRow{
		{I: 0, Out: "a", OK: true, Verdict: mediaremote.VerdictRendered, Family: "node-family", License: "Node License", CommercialUse: &node},
		{I: 1, Out: "b", OK: true, Verdict: mediaremote.VerdictRendered},
	}
	p := batchPayload(&mediaremote.Batch{Rows: rows, Mode: mediaremote.ModeSerial}, local, true, nil, false)
	for _, k := range []string{"family", "license", "commercial_use"} {
		if _, has := p[k]; has {
			t.Errorf("a serial batch's header names %q from this machine's binding", k)
		}
	}
	items := p["items"].([]map[string]any)
	if items[0]["license"] != "Node License" || items[0]["family"] != "node-family" || items[0]["commercial_use"] != true {
		t.Errorf("item 0 = %v, want the node's own family and licence", items[0])
	}
	if _, has := items[1]["license"]; has {
		t.Errorf("item 1 = %v, a node that said nothing must not be given this machine's licence", items[1])
	}
	// every other mode keeps stamping the binding it compared, as items always carried
	p = batchPayload(&mediaremote.Batch{Rows: rows[1:], Mode: mediaremote.ModeTickets}, local, true, nil, false)
	if p["family"] != "local-family" || p["license"] != "Local License" || p["items"].([]map[string]any)[0]["license"] != "Local License" {
		t.Errorf("a ticket batch must keep the local binding's family and licence, got %v", p)
	}
}
