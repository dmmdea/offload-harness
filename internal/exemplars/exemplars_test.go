package exemplars

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
)

// helper: create a temp dir and return its path.
func tmpDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "exemplars-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// ---- Append -------------------------------------------------------------

func TestAppend_CreatesFile(t *testing.T) {
	dir := tmpDir(t)
	if err := Append(dir, "summarize", "v1", "Hello world text", []byte(`"summary"`), 0.9); err != nil {
		t.Fatalf("Append error: %v", err)
	}
	if _, err := os.Stat(sidecarPath(dir, "summarize")); err != nil {
		t.Fatalf("sidecar not created: %v", err)
	}
}

func TestAppend_MultipleEntries(t *testing.T) {
	dir := tmpDir(t)
	inputs := []string{"alpha text", "beta text", "gamma text"}
	for _, inp := range inputs {
		if err := Append(dir, "classify", "labels-v1", inp, []byte(`"cat"`), 0.7); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	pairs, err := loadSidecar(sidecarPath(dir, "classify"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 3 {
		t.Fatalf("expected 3 pairs, got %d", len(pairs))
	}
	for i, p := range pairs {
		if p.Input != inputs[i] {
			t.Errorf("pair %d: input mismatch %q vs %q", i, p.Input, inputs[i])
		}
	}
}

func TestAppend_ConcurrencySafe(t *testing.T) {
	dir := tmpDir(t)
	var wg sync.WaitGroup
	n := 20
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = Append(dir, "triage", "", "concurrent input", []byte(`"ok"`), 0.5)
		}(i)
	}
	wg.Wait()
	pairs, err := loadSidecar(sidecarPath(dir, "triage"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != n {
		t.Fatalf("expected %d pairs after concurrent appends, got %d", n, len(pairs))
	}
}

// ---- Select -------------------------------------------------------------

func TestSelect_EmptySidecar(t *testing.T) {
	dir := tmpDir(t)
	report, err := Select(dir, "summarize")
	if err != nil {
		t.Fatalf("Select on empty sidecar: %v", err)
	}
	_ = report // just "no pairs" message, no error
}

func TestSelect_WritesSelectedJSON(t *testing.T) {
	dir := tmpDir(t)
	// Append enough diverse pairs.
	sentences := []struct{ inp, out string }{
		{"The quick brown fox jumps over the lazy dog", "animals"},
		{"Machine learning models need training data", "tech"},
		{"Cooking pasta requires boiling water and sauce", "food"},
		{"Stock markets fluctuate based on economic indicators", "finance"},
		{"Physical exercise improves cardiovascular health", "health"},
		{"Solar panels convert sunlight into electricity", "energy"},
		{"Programming languages differ in syntax and semantics", "tech"},
		{"Climate change affects global weather patterns", "environment"},
	}
	for _, s := range sentences {
		if err := Append(dir, "classify", "k-v1", s.inp, []byte(`"`+s.out+`"`), 0.8); err != nil {
			t.Fatal(err)
		}
	}
	report, err := Select(dir, "classify")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	t.Log(report)

	// Verify selected.json exists and is valid JSON array.
	data, err := os.ReadFile(selectedPath(dir, "classify"))
	if err != nil {
		t.Fatalf("selected.json missing: %v", err)
	}
	var selected []Pair
	if err := json.Unmarshal(data, &selected); err != nil {
		t.Fatalf("selected.json invalid JSON: %v", err)
	}
	if len(selected) == 0 {
		t.Fatal("selected.json is empty")
	}
	if len(selected) > len(sentences) {
		t.Fatalf("selected has more entries (%d) than sidecar (%d)", len(selected), len(sentences))
	}
}

func TestSelect_LargeSidecarCapsAt50(t *testing.T) {
	dir := tmpDir(t)
	// Append 80 pairs.
	for i := 0; i < 80; i++ {
		inp := "input sentence number varied words document text extra padding"
		if err := Append(dir, "extract", "", inp, []byte(`"val"`), float64(i)/100); err != nil {
			t.Fatal(err)
		}
	}
	_, err := Select(dir, "extract")
	if err != nil {
		t.Fatalf("Select large: %v", err)
	}
	data, _ := os.ReadFile(selectedPath(dir, "extract"))
	var sel []Pair
	json.Unmarshal(data, &sel)
	if len(sel) > targetSize {
		t.Fatalf("expected <= %d selected, got %d", targetSize, len(sel))
	}
}

// ---- Retrieve -----------------------------------------------------------

func TestRetrieve_MissingFile(t *testing.T) {
	dir := tmpDir(t)
	result := Retrieve(dir, "summarize", "anything", 3)
	if result != nil {
		t.Fatalf("expected nil for missing file, got %v", result)
	}
}

func TestRetrieve_ReturnsRelevant(t *testing.T) {
	dir := tmpDir(t)
	pairs := []struct{ inp, out string }{
		{"natural language processing text analysis", "nlp"},
		{"deep learning neural networks image classification", "cv"},
		{"stock market price prediction financial", "finance"},
		{"recipe cooking ingredients dinner lunch", "food"},
		{"sentiment analysis opinion mining reviews", "nlp"},
	}
	for _, p := range pairs {
		if err := Append(dir, "classify", "", p.inp, []byte(`"`+p.out+`"`), 0.9); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Select(dir, "classify"); err != nil {
		t.Fatal(err)
	}

	// Query related to NLP — should surface the NLP-related pairs.
	results := Retrieve(dir, "classify", "text analysis natural language", 2)
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	if len(results) > 2 {
		t.Fatalf("expected <= 2, got %d", len(results))
	}
	// The first result should be one of the NLP ones.
	found := false
	for _, r := range results {
		if r.Output == `"nlp"` {
			found = true
		}
	}
	if !found {
		t.Logf("results: %+v", results)
		t.Error("expected an NLP pair in top results for NLP query")
	}
}

func TestRetrieve_NoDuplicates(t *testing.T) {
	dir := tmpDir(t)
	// Append identical pairs — diversity gate should deduplicate in Retrieve.
	for i := 0; i < 5; i++ {
		if err := Append(dir, "triage", "", "exact same sentence every time", []byte(`"low"`), 0.5); err != nil {
			t.Fatal(err)
		}
	}
	// Also add a distinct one.
	if err := Append(dir, "triage", "", "completely different quantum physics subject matter", []byte(`"high"`), 0.9); err != nil {
		t.Fatal(err)
	}
	if _, err := Select(dir, "triage"); err != nil {
		t.Fatal(err)
	}
	results := Retrieve(dir, "triage", "exact same sentence", 5)
	// Check no two results are identical inputs.
	seen := map[string]bool{}
	for _, r := range results {
		if seen[r.Input] {
			t.Fatalf("duplicate input returned by Retrieve: %q", r.Input)
		}
		seen[r.Input] = true
	}
}

func TestRetrieve_KZeroReturnsNil(t *testing.T) {
	dir := tmpDir(t)
	if err := Append(dir, "summarize", "", "some input", []byte(`"out"`), 0.8); err != nil {
		t.Fatal(err)
	}
	Select(dir, "summarize")
	if r := Retrieve(dir, "summarize", "some input", 0); r != nil {
		t.Fatalf("expected nil for k=0, got %v", r)
	}
}

// ---- BM25 internals -----------------------------------------------------

func TestBM25Score_RelevantHigherThanIrrelevant(t *testing.T) {
	docs := [][]string{
		tokenise("machine learning deep neural networks"),
		tokenise("pasta sauce tomato cooking dinner"),
		tokenise("machine learning model inference speed"),
	}
	idx := buildIndex(docs)
	query := tokenise("machine learning")

	scoreML0 := idx.score(0, query)
	scoreFood := idx.score(1, query)
	scoreML2 := idx.score(2, query)

	if scoreML0 <= scoreFood {
		t.Errorf("ML doc should score higher than food doc: %.4f vs %.4f", scoreML0, scoreFood)
	}
	if scoreML2 <= scoreFood {
		t.Errorf("ML doc 2 should score higher than food doc: %.4f vs %.4f", scoreML2, scoreFood)
	}
}

func TestJaccard(t *testing.T) {
	a := tokenSet(tokenise("apple banana cherry"))
	b := tokenSet(tokenise("apple banana durian"))
	j := jaccard(a, b)
	// intersection={apple,banana}, union={apple,banana,cherry,durian} => 2/4 = 0.5
	if j < 0.49 || j > 0.51 {
		t.Errorf("expected jaccard ~0.5, got %.4f", j)
	}
	// identical sets
	c := tokenSet(tokenise("foo bar"))
	d := tokenSet(tokenise("foo bar"))
	if jaccard(c, d) != 1.0 {
		t.Error("identical sets should have jaccard=1")
	}
	// disjoint
	e := tokenSet(tokenise("alpha"))
	f := tokenSet(tokenise("beta"))
	if jaccard(e, f) != 0.0 {
		t.Error("disjoint sets should have jaccard=0")
	}
}
