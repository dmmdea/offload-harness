package grounding

import (
	"sort"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestCheckNumbersByValue pins the number rule: a JSON number is grounded iff the
// source writes that value, in either locale, and never because its digits sit
// inside another number. Every row is an extract; ok is always true.
func TestCheckNumbersByValue(t *testing.T) {
	cases := []struct {
		name  string
		input string
		data  string
		want  bool
	}{
		// the defect: a correct amount the source writes with separators
		{"EN thousands and decimals", "Total: 2,354.40 USD", `{"total":2354.4}`, true},
		{"ES thousands and decimal comma", "Total: 2.354,40 EUR", `{"total":2354.4}`, true},
		{"decimal comma alone", "Importe: 185,50 EUR", `{"importe":185.5}`, true},
		{"decimal dot alone", "Amount: 185.50 USD", `{"amount":185.5}`, true},
		{"plain integer", "Amount: 4200 USD", `{"amount":4200}`, true},
		{"trailing zero decimals are the integer", "Amount: 4200.00 USD", `{"amount":4200}`, true},
		{"integer with EN thousands", "Units: 1,250,000", `{"units":1250000}`, true},
		{"integer with ES thousands", "Unidades: 1.250.000", `{"units":1250000}`, true},
		{"ES millions and decimals", "Total 1.234.567,89 EUR", `{"total":1234567.89}`, true},
		{"EN millions and decimals", "Total 1,234,567.89 USD", `{"total":1234567.89}`, true},
		{"four-digit decimal", "Pi is about 3,1415", `{"v":3.1415}`, true},
		{"leading-zero decimal", "Ratio 0.125", `{"v":0.125}`, true},
		{"leading-zero decimal comma", "Ratio 0,125", `{"v":0.125}`, true},

		// the defect: a wrong number grounded through the substring shortcut
		{"0 is not in 4200", "Amount: 4200 USD", `{"amount":0}`, false},
		{"42 is not in 4200", "Amount: 4200 USD", `{"amount":42}`, false},
		{"420 is not in 4200", "Amount: 4200 USD", `{"amount":420}`, false},
		{"200 is not in 4200", "Amount: 4200 USD", `{"amount":200}`, false},
		{"7 is not in 7.5", "Weight: 7.5 kg", `{"w":7}`, false},
		{"5 is not in 7.5", "Weight: 7.5 kg", `{"w":5}`, false},
		{"absent number", "Amount: 4200 USD", `{"amount":99}`, false},
		{"zero when the source says zero", "Quantity: 0 items", `{"qty":0}`, true},

		// a distractor that IS in the source: grounding checks presence, not which one
		{"another number of the source", "Amount: 4200 USD, fee: 35 USD", `{"amount":35}`, true},
		{"its own number among several", "Amount: 4200 USD, fee: 35 USD", `{"amount":4200,"fee":35}`, true},
		{"one good one absent", "Amount: 4200 USD, fee: 35 USD", `{"amount":4200,"fee":36}`, false},

		// separators are never read as a digits-only number
		{"ES amount is not 235440", "Total: 2.354,40 EUR", `{"total":235440}`, false},
		{"EN amount is not 235440", "Total: 2,354.40 USD", `{"total":235440}`, false},
		{"ES amount is not 2.3544", "Total: 2.354,40 EUR", `{"total":2.3544}`, false},
		{"EN amount is not 23544", "Total: 2,354.40 USD", `{"total":23544}`, false},
		{"decimal comma is not 18550", "Importe: 185,50 EUR", `{"importe":18550}`, false},
		{"decimal comma is not 1855", "Importe: 185,50 EUR", `{"importe":1855}`, false},
		{"four-digit decimal is not 31415", "Pi is about 3,1415", `{"v":31415}`, false},
		{"leading zero is never thousands", "Ratio 0,125", `{"v":125}`, false},
		{"two-digit decimal is not thousands", "Units: 1,25", `{"v":125}`, false},
		{"EN millions are not decimals", "Units: 1,250,000", `{"v":1.25}`, false},

		// one separator and exactly three digits is ambiguous: both readings count
		{"ambiguous comma as thousands", "Consumption: 1,234 kWh", `{"kwh":1234}`, true},
		{"ambiguous comma as decimal", "Consumption: 1,234 kWh", `{"kwh":1.234}`, true},
		{"ambiguous dot as thousands", "Consumo: 1.234 kWh", `{"kwh":1234}`, true},
		{"ambiguous dot as decimal", "Consumo: 1.234 kWh", `{"kwh":1.234}`, true},
		{"ambiguous is still not the digits alone", "Consumption: 1,234 kWh", `{"kwh":234}`, false},
		{"ambiguous is still not its neighbour", "Consumption: 1,234 kWh", `{"kwh":1235}`, false},
		{"four digits before the mark is a decimal", "Reading 1234.567", `{"v":1234.567}`, true},
		{"four digits before the mark is not thousands", "Reading 1234.567", `{"v":1234567}`, false},

		// sign: reNum never sees it, so a number is compared by magnitude (as before)
		{"negative against a dot", "Temperature: -12.5 C", `{"t":-12.5}`, true},
		{"negative against a comma", "Temperatura: -12,5 C", `{"t":-12.5}`, true},
		{"sign is not checked", "Temperature: -12.5 C", `{"t":12.5}`, true},
		{"negative of an absent number", "Temperature: -12.5 C", `{"t":-13.5}`, false},

		// currency and unit adjacent tokens
		{"euro prefix, ES grouping", "Total €1.200,00", `{"total":1200}`, true},
		{"dollar prefix, EN grouping", "Total $1,200.00", `{"total":1200}`, true},
		{"pound prefix", "Total £12,480.50 incl. VAT", `{"total":12480.5}`, true},
		{"euro suffix", "Total 12.480,50 € con IVA", `{"total":12480.5}`, true},
		{"percent with decimal comma", "Growth 12,5 %", `{"growth":12.5}`, true},
		{"percent glued", "Battery: 18%", `{"battery":18}`, true},
		{"percent is not its digits", "Growth 12,5 %", `{"growth":125}`, false},

		// trailing punctuation belongs to the sentence, not the number
		{"trailing full stop", "It was paid in full for 1,099.", `{"paid":1099}`, true},
		{"trailing comma", "Due 480.00, less the deposit.", `{"due":480}`, true},
		{"trailing full stop on a decimal", "Weight is 2.75.", `{"w":2.75}`, true},
		{"trailing full stop is not thousands", "Weight is 2.75.", `{"w":275}`, false},

		// a token that is no number in either locale reads as its digit groups
		{"dotted date day", "Shipped on 18.06.2026.", `{"day":18}`, true},
		{"dotted date year", "Shipped on 18.06.2026.", `{"year":2026}`, true},
		{"dotted date is not its digits joined", "Shipped on 18.06.2026.", `{"n":1806}`, false},
		{"version part", "Release 3.1.2 is out", `{"minor":1}`, true},
		{"version is not a number", "Release 3.1.2 is out", `{"n":312}`, false},
		{"comma list member", "Items 3,4,5 shipped", `{"n":4}`, true},
		{"comma list is not 345", "Items 3,4,5 shipped", `{"n":345}`, false},
		{"hyphenated id part", "Order 55-20931 shipped", `{"n":20931}`, true},
		{"hyphenated id is not joined", "Order 55-20931 shipped", `{"n":5520931}`, false},
		{"leading zeros are the same value", "Serial 0048127", `{"n":48127}`, true},
		{"invalid grouping is not joined", "Ref 12,34.5", `{"n":1234.5}`, false},

		// comparison is exact: float noise is a different float, and a last-digit
		// error in a long identifier is an error
		{"float noise is not the same float", "Total 0.3", `{"t":0.30000000000000004}`, false},
		{"epoch ms last digit", "ts=1759190400123", `{"v":1759190400124}`, false},
		{"epoch ms exact", "ts=1759190400123", `{"v":1759190400123}`, true},
		{"phone with country code", "tel 5215512345678", `{"v":5215512345680}`, false},
		{"13 decimals differ", "x 0.1234567890123", `{"v":0.1234567890124}`, false},
		{"long string id last digit", "Card 4111111111111111", `{"v":"4111111111111112"}`, false},
		{"17-digit string id last digit", "Card 12345678901234567", `{"v":"12345678901234568"}`, false},
		{"19-digit string id last digit", "Order 1234567890123456789", `{"v":"1234567890123456780"}`, false},
		{"17-digit string id exact", "Card 12345678901234567", `{"v":"12345678901234567"}`, true},

		// Indian lakh/crore grouping reads as one number
		{"lakh grouping", "Amount ₹1,23,456", `{"v":123456}`, true},
		{"crore grouping with decimals", "Amount 12,34,567.89", `{"v":1234567.89}`, true},
		{"lakh grouping wrong value", "Amount ₹1,23,456", `{"v":123457}`, false},

		// two-part tokens are decimals only; their components are not source values
		// (precision over recall, the same rule as "7 is not in 7.5")
		{"two-part version is a decimal", "Release 3.1 is out", `{"major":3,"minor":1}`, false},
		{"hh.mm is a decimal", "At 9.30 sharp", `{"h":9,"m":30}`, false},
		{"comma pair is a decimal", "qty 5,10", `{"a":5,"b":10}`, false},
		{"two-part version as a value", "Release 3.1 is out", `{"v":3.1}`, true},
		{"neighbouring integer", "Total 1000000000", `{"t":1000000001}`, false},
		{"neighbouring decimal", "Total 2354.40", `{"t":2354.41}`, false},

		// shapes: nested objects and arrays, mixed leaves, non-checkable leaves
		{"nested number", "Qty 3 and 5", `{"a":{"b":[{"qty":3},{"qty":5}]}}`, true},
		{"nested absent number", "Qty 3 and 5", `{"a":{"b":[{"qty":3},{"qty":6}]}}`, false},
		{"string and number leaves", "Invoice for Acme: 4.200,00 EUR", `{"customer":"Acme","total":4200}`, true},
		{"absent string beside a good number", "Invoice for Acme: 4.200,00 EUR", `{"customer":"Globex","total":4200}`, false},
		{"booleans are not checked", "Amount: 185,50 EUR", `{"paid":true,"amount":185.5}`, true},
		{"empty string is absent", "Amount: 185,50 EUR", `{"memo":"","amount":185.5}`, true},
		{"empty string does not rescue a bad number", "Amount: 185,50 EUR", `{"memo":"","amount":18550}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, ok := Check(core.TaskExtract, tc.input, []byte(tc.data))
			if !ok || g != tc.want {
				t.Fatalf("Check(%q, %s) = (grounded=%v, ok=%v); want (grounded=%v, ok=true)", tc.input, tc.data, g, ok, tc.want)
			}
		})
	}
}

// TestCheckStringLeavesWithNumbers: a string keeps the verbatim-phrase check, and a
// string that is not a phrase of the source is grounded iff every number in it is a
// value the source writes (so the locale of the string need not match the source's).
func TestCheckStringLeavesWithNumbers(t *testing.T) {
	cases := []struct {
		name  string
		input string
		data  string
		want  bool
	}{
		{"ES number as written", "Total: 2.354,40 EUR", `{"total":"2.354,40"}`, true},
		{"EN number as written", "Total: 2,354.40 USD", `{"total":"2,354.40"}`, true},
		{"amount and currency as written", "Importe: 185,50 EUR", `{"importe":"185,50 EUR"}`, true},
		{"amount with another currency spelling", "Importe: 185,50 €", `{"importe":"185,50 EUR"}`, true},
		{"EN string against an ES source", "Total: 2.354,40 EUR", `{"total":"2,354.40 EUR"}`, true},
		{"ES string against an EN source", "Total: 2,354.40 USD", `{"total":"2.354,40 USD"}`, true},
		{"wrong amount in a string", "Importe: 185,50 EUR", `{"importe":"186,50 EUR"}`, false},
		{"digits-only misreading in a string", "Total: 2.354,40 EUR", `{"total":"235440 EUR"}`, false},
		{"case and spacing are normalized", "Customer:   ACME   Corp", `{"customer":"acme corp"}`, true},
		{"phrase not in the source", "Customer: Acme Corp", `{"customer":"Globex Industries"}`, false},
		{"ISO date as written", "Issued 2026-03-03, due 2026-04-02", `{"issued":"2026-03-03"}`, true},
		{"dotted date as written", "Shipped 18.06.2026.", `{"shipped":"18.06.2026"}`, true},
		{"code with digits as written", "Order 55-20931 shipped", `{"order":"55-20931"}`, true},
		{"percent string in another spacing", "Growth 12,5% this year", `{"growth":"12,5 %"}`, true},
		{"wrong code prefix", "Order ABC-0042", `{"v":"ABC-42"}`, false},
		{"wrong code prefix, same digits", "Order ABC-42", `{"v":"XYZ-42"}`, false},
		{"code as written", "Order ABC-0042", `{"v":"ABC-0042"}`, true},
		{"zero-padded code is not its value", "Serial 7", `{"v":"007"}`, false},
		{"zero-padded code as written", "Serial 007", `{"v":"007"}`, true},
		{"wrong percent string", "Growth 12,5% this year", `{"growth":"13,5 %"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, ok := Check(core.TaskExtract, tc.input, []byte(tc.data))
			if !ok || g != tc.want {
				t.Fatalf("Check(%q, %s) = (grounded=%v, ok=%v); want (grounded=%v, ok=true)", tc.input, tc.data, g, ok, tc.want)
			}
		})
	}
}

// TestCheckSummarizeNumbersByValue: a summary's numbers follow the same rule as an
// extract's, so restating "2.354,40" as "2,354.40" is not an invented fact.
func TestCheckSummarizeNumbersByValue(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		summary string
		want    bool
	}{
		{"same locale", "Total: 2.354,40 EUR due on 2026-04-02.", "The total was 2.354,40 EUR.", true},
		{"other locale", "Total: 2.354,40 EUR due on 2026-04-02.", "The total was 2,354.40 EUR.", true},
		{"sentence-final number", "Paid 1.099,00 EUR in full", "It cost 1,099.", true},
		{"invented number", "Total: 2.354,40 EUR", "The total was 999 EUR.", false},
		{"digits-only misreading", "Total: 2.354,40 EUR", "The total was 235440 EUR.", false},
		{"substring of a source number", "Amount: 4200 USD", "The fee was 0 USD.", false},
		{"no numbers", "Total: 2.354,40 EUR", "An invoice was issued.", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := `{"summary":"` + tc.summary + `","bullets":[]}`
			g, ok := Check(core.TaskSummarize, tc.input, []byte(data))
			if !ok || g != tc.want {
				t.Fatalf("Check(%q, %s) = (grounded=%v, ok=%v); want (grounded=%v, ok=true)", tc.input, data, g, ok, tc.want)
			}
		})
	}
}

// TestCheckFieldsNumbersByValue: the per-field variant names the offenders under the
// same value rule, so the corrective re-prompt does not accuse a correct amount.
func TestCheckFieldsNumbersByValue(t *testing.T) {
	input := "Invoice for Acme. Total: 2.354,40 EUR, paid 185,50 EUR, qty 4200."
	data := []byte(`{"customer":"Acme","total":2354.4,"paid":185.5,"qty":0,"tax":99}`)
	bad, ok := CheckFields(core.TaskExtract, input, data)
	if !ok {
		t.Fatal("expected ok=true for extract")
	}
	if len(bad) != 2 || bad[0] != "qty" || bad[1] != "tax" {
		t.Fatalf("expected [qty tax] ungrounded, got %v", bad)
	}
}

// TestNumberValues pins the token parser directly: the set of values one reNum token
// can denote. Sets are compared sorted, so the order a reading is found in is free.
func TestNumberValues(t *testing.T) {
	cases := []struct {
		tok  string
		want []float64
	}{
		{"4200", []float64{4200}},
		{"0048127", []float64{48127}},
		{"0", []float64{0}},
		{"18.4", []float64{18.4}},
		{"185,50", []float64{185.5}},
		{"2,354.40", []float64{2354.4}},
		{"2.354,40", []float64{2354.4}},
		{"12,480.50", []float64{12480.5}},
		{"1.234.567,89", []float64{1234567.89}},
		{"1,250,000", []float64{1250000}},
		{"1.250.000", []float64{1250000}},
		{"1,234", []float64{1.234, 1234}},
		{"1.234", []float64{1.234, 1234}},
		{"123,456", []float64{123.456, 123456}},
		{"1234,567", []float64{1234.567}},
		{"0,500", []float64{0.5}},
		{"3,1415", []float64{3.1415}},
		{"12,5", []float64{12.5}},
		{"480.00,", []float64{480}},
		{"1,099.", []float64{1.099, 1099}},
		{"2.75.", []float64{2.75}},
		// no reading in either locale: the digit groups
		{"18.06.2026", []float64{6, 18, 2026}},
		{"3.1.2", []float64{1, 2, 3}},
		{"3,4,5", []float64{3, 4, 5}},
		{"1,23,456", []float64{123456}},
		{"12,34,567.89", []float64{1234567.89}},
		{"1,23,45", []float64{1, 23, 45}},
		{"3.1", []float64{3.1}},
		{"1.234,567.89", []float64{89, 234, 567, 1}},
		{"12,34.5", []float64{5, 12, 34}},
		{"1,2345.6", []float64{1, 2345, 6}},
		{"1,000,00", []float64{0, 0, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.tok, func(t *testing.T) {
			got := numberValues(tc.tok)
			want := append([]float64(nil), tc.want...)
			sort.Float64s(got)
			sort.Float64s(want)
			if len(got) != len(want) {
				t.Fatalf("numberValues(%q) = %v; want %v", tc.tok, got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("numberValues(%q) = %v; want %v", tc.tok, got, want)
				}
			}
		})
	}
}

// TestCheckLegacyVerdicts is the regression table: verdicts the string-comparison
// implementation gave on non-locale input, which the value comparison must not change.
// Every row also passes on the string-comparison implementation it replaced.
func TestCheckLegacyVerdicts(t *testing.T) {
	src := "The RTX 3070 Mobile has 8GB of GDDR6 VRAM and runs Gemma-4 at 70-83 tokens per second."
	cases := []struct {
		name string
		task core.TaskType
		data string
		want bool
		ok   bool
	}{
		{"phrase verbatim", core.TaskExtract, `{"gpu":"RTX 3070 Mobile"}`, true, true},
		{"phrase differing in case and spacing", core.TaskExtract, `{"gpu":"rtx  3070   mobile"}`, true, true},
		{"phrase with a number inside", core.TaskExtract, `{"mem":"8GB of GDDR6"}`, true, true},
		{"string number present", core.TaskExtract, `{"mem":"8"}`, true, true},
		{"string number absent", core.TaskExtract, `{"mem":"16"}`, false, true},
		{"number present", core.TaskExtract, `{"vram_gb":8}`, true, true},
		{"range ends are numbers of the source", core.TaskExtract, `{"lo":70,"hi":83}`, true, true},
		{"number absent", core.TaskExtract, `{"vram_gb":24}`, false, true},
		{"string absent", core.TaskExtract, `{"gpu":"RTX 4090"}`, false, true},
		{"one absent leaf spoils the rest", core.TaskExtract, `{"gpu":"RTX 3070 Mobile","vram_gb":24}`, false, true},
		{"nested leaves", core.TaskExtract, `{"card":{"name":"RTX 3070 Mobile","mem":[8]}}`, true, true},
		{"booleans only", core.TaskExtract, `{"mobile":true}`, false, false},
		{"empty strings only", core.TaskExtract, `{"gpu":""}`, false, false},
		{"empty object", core.TaskExtract, `{}`, false, false},
		{"not an object", core.TaskExtract, `[1,2]`, false, false},
		{"summary numbers present", core.TaskSummarize, `{"summary":"70-83 tok/s with 8GB","bullets":["GDDR6"]}`, true, true},
		{"summary number invented", core.TaskSummarize, `{"summary":"999 tok/s","bullets":[]}`, false, true},
		{"summary without numbers", core.TaskSummarize, `{"summary":"A mobile GPU.","bullets":[]}`, true, true},
		{"empty summary", core.TaskSummarize, `{"summary":"","bullets":[]}`, false, false},
		{"classify is not checkable", core.TaskClassify, `{"label":"hardware"}`, false, false},
		{"triage is not checkable", core.TaskTriage, `{"decision":"yes"}`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, ok := Check(tc.task, src, []byte(tc.data))
			if ok != tc.ok || (ok && g != tc.want) {
				t.Fatalf("Check = (grounded=%v, ok=%v); want (grounded=%v, ok=%v)", g, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestSummarizeLongIDs pins that a summary quoting an identifier must quote it digit for digit.
func TestSummarizeLongIDs(t *testing.T) {
	for _, tc := range []struct {
		data string
		want bool
	}{
		{`{"summary":"Card 4111111111111111 was charged.","bullets":[]}`, true},
		{`{"summary":"Card 4111111111111112 was charged.","bullets":[]}`, false},
	} {
		g, ok := Check(core.TaskSummarize, "Card 4111111111111111 billed", []byte(tc.data))
		if !ok || g != tc.want {
			t.Fatalf("Check(%s) = (%v, %v); want grounded=%v", tc.data, g, ok, tc.want)
		}
	}
}
