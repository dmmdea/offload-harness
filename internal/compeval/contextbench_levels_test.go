package compeval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// cbLevelPins pin every published level at seed 42: the operation count, the sha256 of
// the streamed operations and the sha256 of the gold. A knob change in the level table
// (a batch size, a chunk count, a record count) changes the stream and fails here.
// The digests of the four levels that cbParityCases also covers agree with those, which
// were taken from the original generator; the rest were checked once against the original
// generator for every level and every published seed (42 to 49) and are pinned as data.
// scripts/contextbench_parity_regen.py regenerates the operations digests (opsSHA) from a
// checkout of the pinned upstream commit; the gold digests are this port's own JSON and
// guard the port, not the upstream.
var cbLevelPins = []struct {
	family, level string
	nOps          int
	opsSHA        string
	goldSHA       string
}{
	{CBFamilyKV, "R130", 26, "6dbb403e4ec0c5c92a4de92a15ac196d95724bc1e2a155a6eb3cfc93cca2e5e8", "55e0d9da4761ca50b41f5c1b70ac293266c6828a70e428ab254601f6066c7b13"},
	{CBFamilyKV, "R260", 27, "89825840dfa53dd450dd907ad4ec21747da1f9b841d3b70c12cf2a51ccb5a5f2", "ef5e04e1faf7ce90401b8e64b2856614cda8721d5405246713f6611b04d8b449"},
	{CBFamilyKV, "R520", 30, "60964b6b42cb229cbbfd460fb4265cf89832d88ad7ca91227578bd076bd8ce02", "d15b6c5ddb791aa1b1152d73ba729d99e3eb4b56550b974ebd3526886c9a6137"},
	{CBFamilyKV, "R1000", 34, "9e5af984ff36ca9f8603df70e72054cf0f73366281fa6e86ba7d6955a369fd99", "82dec6ecde9c50085ff140ba380ca723e4ce2153593b684af7fc7147aa2a8f2a"},
	{CBFamilyKV, "R2000", 44, "21fc7ee53b0e71aa505c0a1a7df794a25a17fed906cfe750d4f60fcf760bb602", "4f8a415e302f308daebf1f1ffd1ce1e9c884004cef0c0504c80ca0aef454890b"},
	{CBFamilyKV, "R4000", 64, "52f7dc0978afe551a6db1e5e3ef93d773fc0f742e2f0c6687f39ed2ab0c036b1", "a48c7ca30f8c5718053ab40d7bfed5e2cc0464b3283fc2d7fcce5f8432b295da"},
	{CBFamilyKV, "R8000", 104, "462d4f07f6d774159a071d8a88d717a4132ffb3d14fdaf0ab2673fd790660075", "7a206ad38882642445b1172bd7bd4bcf943f58517b80aea1b2d570b9b7a5c476"},
	{CBFamilyKV, "R16000", 184, "447eda786e2a47d1a266d92e7d8194c35cc57081441c047c351cd229f2c1b995", "ac148ac3d9037f2c5ebf8e466a66fb6621dd4433bd4483a00a2ee873e6b112af"},
	{CBFamilyLog, "L160", 36, "b943d1a90c6f665f4e9fb2ddee4c4bee37ef66268c12758bcefd54833df81df5", "72514af497460f341ba531f391b39312a13cfc92347cddb942cc55264cd6572a"},
	{CBFamilyLog, "L320", 47, "6b49b9d01a7c19465be720bd751400d4dec4eb3d2606d94e1eed58daddb7df37", "6aa175d0812552233bacddfa0751496532f5cbc42fa2c15a0d6f746a930a9db7"},
	{CBFamilyLog, "L640", 70, "1d467633f8f3f6100feb17a543e04ae2ae9f12df5e32247d440cfb89cd31cd31", "a19cd37cf437455bce597a807acf1a71c000fe760ea55cfe10d7e2cce712e93c"},
	{CBFamilyLog, "L1600", 139, "8614661ec3375bbcbc6893a5a405a916755724fbd6289ef298a6662433efd2f5", "cfead9fdbb24c9a02a3fc78cd75ec8b5546a4a83305d4bfc600f47db285ce5fe"},
	{CBFamilyLog, "L3200", 143, "826c2bf0342f871e312f3efe84fcbf833e23ca4346d4dffb8d6b351e14d361c2", "9e0e6c72d8b11a5c8d7fde6a8ec90d30e6a43ee1f360b38fa9a509d6d3650ad7"},
	{CBFamilyLog, "L6400", 143, "95a3da62b43bb2bf4be7c78a839b5dc104ca1fd4d1b85e2d4e09d15cb8c2e8cd", "0a5781e73414fdb8768673a028b90c62f4fb6134e232c52fbd96063dc41c6240"},
	{CBFamilyLog, "L12800", 262, "2722fb18ea23382888aa57d3b8219795a546014c199500b73cb8246e3ecc99a9", "8b13f4949edc2b1a9f408633e88b298548c4968737eec150aec8c824ee3c49a0"},
	{CBFamilyNeedle, "K4-C4", 4, "62ae7b633a0e37446c9b81e1f7072ae1ff2cb170da0c5d5bdb3326075097ba22", "ee37982410eaf40f4c48c7c2b8bcecb9664ede5e70395cfeddd5a4739338f609"},
	{CBFamilyNeedle, "K4-C6", 6, "27457cdf16b9af8935f52bbbb95b4bda258a10f2b8680d03ef20caa23f028ffb", "469d0aa16559e295b8a3a6b2574ab6762683bd7cf3f4c6877dfa409002e335c1"},
	{CBFamilyNeedle, "K4-C8", 8, "8f6fc18b9649b9fe01559d56997adf64052dc539d7dd6702ff7f4430469e8897", "40c83fac284d38888ef9b21ee3d4ff9fce24d953087515a2fcef19547631403e"},
	{CBFamilyNeedle, "K4-C24", 24, "c6e1e7ad28324a7428210d94323e1396d9d71f8f4e2998152086349d054e4f73", "24fd56d7905054d61cea5ee672e0fd7d1125a97c154d1c2fb4245f922b2a6d22"},
	{CBFamilyNeedle, "K8-C48", 48, "1d048f53083f6433c3c85d2de0efc872bea04693506a6582520356fc15be7bb6", "e344233e0b4a52f041f7e842cb29b73246365589fc458e3c396b437d447914fd"},
	{CBFamilyNeedle, "K8-C60", 60, "b48d83c2f4ca6e7020d7590681d71a1f82571a8b49299c401efa6ccbbcc803e5", "cce13bde5081b0c38369362ed644b24eb99a1c12281082e57a764ce5c2ea86f5"},
	{CBFamilyNeedle, "K8-C62", 62, "404320a801bb05afaaaa0185a43e3c5a813f1017e9be843d5cbb3a5249352c86", "560620d614ca3fee9c92a8e4f777eebecb22d05a49b12e61f05cacc37bcac0a5"},
	{CBFamilyNeedle, "K4-C96", 96, "4dcad5dfdee78887b3cd8614ec91a2d05dc80aacc868ed68f3bdc2035d568756", "65c2054e8ccabad560b68d79747829710f0349fa73d2c99e9781521586282d2f"},
	{CBFamilyNeedle, "K2-C192", 192, "0d7ebdf5338a26c84fc49b77386726820e91be931275753d0ab762610b44fee6", "c6f08d3dea787fbd772f8d6175d73086ee795ba53e8b275c7749645f59d38b18"},
	{CBFamilyNeedle, "K2-C224", 224, "e2284c0ac85c916825d73086255d8e70940983e67cda668ce303c0654990d2a1", "a4d34e704be9782adfe3f03c8c625d8f0488b2cef2aa6ab3b4d22d8d2b44590a"},
	{CBFamilySudoku, "t4", 4, "d6fdfe4f193a53e17995cfd9b4a70673730eed136f8c0b8cb332c9af68af5cc9", "b0c65d22d6e9af90caf2cfddab3987d9daf12193a6209c886197108dee04648a"},
	{CBFamilySudoku, "t8", 8, "7941aa1b328b80b01a6f4d41c8be58657e947f4d3fc43fcfc5cb983bb67a1114", "8f3b83e7aa0867d8579f1fdb4577855e6b5432f2dd9ca7c71a63f38ea1001be5"},
	{CBFamilySudoku, "t15", 15, "92d6f1c811004c9883c9bfc69cf0b3c4919105736f64e1dcfa984f19e569c30a", "55811770b3e861cd503f2c07700c7d300ee8d7764c2b2bc2a44a3d3d725c4fd4"},
	{CBFamilySudoku, "t30", 30, "6cf1bb9a3d4bc22208dd53e022694cc4d562978b232103139c292ff8fee7e883", "4c68b12f0734f10433c059179d9ba372d2148bf781269f7c0b7bdcfd9f0d8773"},
	{CBFamilySudoku, "t60", 60, "e17e8185ec2a987fadd5a0e4eb51e049231fe7fc3c745dd327c2166fc3b566a0", "dbd23fb6b2fe7bfd5b53715dd71e9ad32537f7d9c910aeffa5f01bfda0b5384d"},
	{CBFamilySudoku, "t100", 100, "1797cd9756d39a9e8077aa5daf23d8b3cad4aa366c20662fb5731f4a1d04089c", "ee84663087081c2fac414ce1a497bec80b129696d7ef8e3167fd1373d32ee3e0"},
	{CBFamilySudoku, "t150", 150, "83b7d9b48b47ebaaa6c7621854d730144163df13b2ba343dfa839818d6bdad49", "8adf7a11a0c5a30ad8348de4324e5b58d90c611a84ad0b868dd668c20e511468"},
}

func TestContextBenchEveryPublishedLevelIsPinned(t *testing.T) {
	want := 0
	for _, f := range CBFamilies {
		want += len(CBLevelNames(f))
	}
	if len(cbLevelPins) != want {
		t.Fatalf("%d levels are pinned but the table has %d: pin the new level or the removed one", len(cbLevelPins), want)
	}
	for _, p := range cbLevelPins {
		t.Run(p.family+"/"+p.level, func(t *testing.T) {
			in, err := GenerateCB(p.family, p.level, 42)
			if err != nil {
				t.Fatal(err)
			}
			if len(in.Ops) != p.nOps {
				t.Fatalf("%d ops, want %d", len(in.Ops), p.nOps)
			}
			o := sha256.Sum256(in.OpsJSONL())
			if got := hex.EncodeToString(o[:]); got != p.opsSHA {
				t.Fatalf("ops digest %s, want %s", got, p.opsSHA)
			}
			gj, err := json.Marshal(in.Gold)
			if err != nil {
				t.Fatal(err)
			}
			g := sha256.Sum256(gj)
			if got := hex.EncodeToString(g[:]); got != p.goldSHA {
				t.Fatalf("gold digest %s, want %s", got, p.goldSHA)
			}
		})
	}
}

// cbUncompactedTokens is the size of the transcript a perfect agent builds when nothing is
// compacted: the instruction, every operation delivered as a tool result and, for
// sudoku_sketchpad, the whole board panel the task makes the agent print after each move.
func cbUncompactedTokens(t *testing.T, in *CBInstance) int {
	t.Helper()
	msgs := cbPreamble(in, CBDeliveryToolResult)
	var boards []map[string]string
	var versions []int
	if in.Family == CBFamilySudoku {
		versions, boards = cbFold(in)
	}
	for i := range in.Ops {
		msgs = append(msgs, cbOpMsgs(in, CBDeliveryToolResult, i)...)
		if in.Family == CBFamilySudoku {
			// Operation i produces gold version i+1; version 0 is the starting panel.
			msgs = append(msgs, agent.Msg{Role: "assistant", Content: cbTestPanelText(in.Gold.Side, boards[i+1], versions[i+1])})
		}
	}
	return agent.EstimateTokens(msgs)
}

// The default level is where the context has to be managed at all: the smallest published
// level whose uncompacted transcript is over the usable budget of record. The sudoku stream
// alone never gets there (12k tokens at t150); the panels the agent re-prints do.
func TestContextBenchDefaultLevelIsTheSmallestOneThatOverflowsTheBudget(t *testing.T) {
	usable := CBDefaultBudget - CBDefaultReserve
	for _, family := range CBFamilies {
		def, err := CBLookupLevel(family, "")
		if err != nil {
			t.Fatal(err)
		}
		names := CBLevelNames(family)
		at := -1
		for i, n := range names {
			if n == def.Name {
				at = i
			}
		}
		if at < 0 {
			t.Fatalf("%s: default level %s is not a published level", family, def.Name)
		}
		in, err := GenerateCB(family, def.Name, 42)
		if err != nil {
			t.Fatal(err)
		}
		got := cbUncompactedTokens(t, in)
		t.Logf("%s/%s: %d tokens uncompacted (usable %d)", family, def.Name, got, usable)
		if got <= usable {
			t.Errorf("%s/%s: the default level's transcript is %d tokens, within the usable budget %d: nothing would be compacted", family, def.Name, got, usable)
		}
		if at == 0 {
			continue
		}
		smaller, err := GenerateCB(family, names[at-1], 42)
		if err != nil {
			t.Fatal(err)
		}
		got = cbUncompactedTokens(t, smaller)
		t.Logf("%s/%s: %d tokens uncompacted (the next smaller level)", family, names[at-1], got)
		if got > usable {
			t.Errorf("%s/%s: the next smaller level already overflows (%d > %d tokens), so %s is not the smallest default", family, names[at-1], got, usable, def.Name)
		}
	}
}
