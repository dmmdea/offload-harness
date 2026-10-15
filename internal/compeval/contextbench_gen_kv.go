// contextbench_gen_kv.go - the key-value memory task generator.
//
// Ported to Go from contextbench/tasks/kv_store/generate.py in
// https://github.com/RulinShao/ContextBench at commit
// fb533c18c78a82d3f1b2047a9c1700735cf829ae (Apache-2.0). The word table, the id and
// value construction and the order of random draws follow the original so the same
// seed streams the same operations. The operation mix is ported: the texts of the
// streamed operations (sentinel lines, headers, request wording) are the original's,
// byte for byte, because the stream digest has to match. The code and comments are
// written for this repository and none of the upstream's documentation or instruction
// prose is carried over.
//
// The task: SET batches stream first, then GET operations. Values are random-word
// strings with a unique tag, so they cannot be guessed, paraphrased or summarised,
// and which keys will be asked about is not revealed up front.
package compeval

import "fmt"

var cbKVWords = []string{
	"lantern", "crimson", "zebra", "orbit", "meadow", "cobalt", "thistle",
	"ember", "quartz", "willow", "harbor", "nimbus", "cedar", "pewter",
	"marble", "falcon", "indigo", "saffron", "boulder", "trellis", "glacier",
	"verdant", "copper", "lattice", "auburn", "spruce", "onyx", "cinder",
	"vellum", "garnet", "russet", "cobble", "fathom", "kindle", "mistral",
}

func cbKVValue(rng *pyRand, key string, nWords int) string {
	n := nWords
	if n < 1 {
		n = 1
	}
	words := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			words += " "
		}
		words += cbKVWords[rng.choiceIndex(len(cbKVWords))]
	}
	return fmt.Sprintf("%s #%s", words, pyHashTag(key, nWords, words))
}

// GenerateKV builds one kv_store instance.
func GenerateKV(seed int64, k CBKnobs) *CBInstance {
	rng := newPyRand(uint64(seed))
	records, queries, perChunk := k.Records, k.Queries, k.RecordsPerChunk
	keys := make([]string, records)
	values := make(map[string]string, records)
	for i := range keys {
		keys[i] = fmt.Sprintf("K%05d", i)
		values[keys[i]] = cbKVValue(rng, keys[i], k.ValueWords)
	}
	var qKeys []string
	if queries <= records {
		for _, i := range rng.sampleIndexes(records, queries) {
			qKeys = append(qKeys, keys[i])
		}
	} else {
		for i := 0; i < queries; i++ {
			qKeys = append(qKeys, keys[rng.choiceIndex(records)])
		}
	}
	gold := make([]CBKVQuery, len(qKeys))
	for i, key := range qKeys {
		gold[i] = CBKVQuery{QID: i, Key: key, Value: values[key]}
	}

	var ops []CBOp
	idx := 0
	for start := 0; start < records; start += perChunk {
		end := start + perChunk
		if end > records {
			end = records
		}
		batch := keys[start:end]
		id := fmt.Sprintf("%04d#%s", idx, pyHashTag(seed, "sb", idx))
		body := fmt.Sprintf("<<<SET-BATCH %s BEGIN -- offload this whole block this turn>>>\n", id)
		for _, key := range batch {
			body += fmt.Sprintf("SET %s = %s\n", key, values[key])
		}
		body += fmt.Sprintf("<<<SET-BATCH %s END>>>", id)
		ops = append(ops, CBOp{Index: idx, Header: fmt.Sprintf("set batch (keys %d-%d)", start, start+len(batch)-1), Body: body})
		idx++
	}
	nBatches := idx
	for _, q := range gold {
		body := fmt.Sprintf("GET %s\n(Report the value you stored for %s as an ANSWER block for that key.)", q.Key, q.Key)
		ops = append(ops, CBOp{Index: idx, Header: fmt.Sprintf("get %d", q.QID), Body: body})
		idx++
	}

	setLines := make([]string, records)
	for i, key := range keys {
		setLines[i] = fmt.Sprintf("SET %s = %s", key, values[key])
	}
	valueTok := approxTokens4(setLines)
	perValue := 0
	if records > 0 {
		perValue = valueTok / records
	}
	return &CBInstance{
		Family:     CBFamilyKV,
		Seed:       seed,
		Ops:        ops,
		QueryStart: nBatches,
		Gold:       CBGold{KV: gold},
		Meta: map[string]any{
			"task_family": CBFamilyKV, "seed": seed, "records": records, "value_words": k.ValueWords,
			"records_per_chunk": perChunk, "queries": len(gold), "n_set": records, "n_get": len(gold),
			"n_batches": nBatches, "n_actions": len(ops), "approx_value_tokens": valueTok,
			"approx_per_value_tokens": perValue,
		},
	}
}
