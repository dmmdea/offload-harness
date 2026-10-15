#!/usr/bin/env python3
"""Regenerate the ContextBench parity digests the Go tests pin.

The generators in internal/compeval/contextbench_gen_*.go are ported from ContextBench
(https://github.com/RulinShao/ContextBench, Apache-2.0). Two tables pin their output as
sha256 digests of the streamed operations, taken from the original generators:

  cbParityCases  internal/compeval/contextbench_gen_test.go     12 parameter sets
  cbLevelPins    internal/compeval/contextbench_levels_test.go  every published level at seed 42

Usage:
  python3 scripts/contextbench_parity_regen.py UPSTREAM_CHECKOUT [--repo REPO_ROOT]

Only the operations digests (cbParityCases, and the opsSHA column of cbLevelPins) come from the
original generators. The goldSHA column of cbLevelPins is the digest of this port's own gold JSON:
a regression pin on the port, not something the upstream can regenerate.

UPSTREAM_CHECKOUT is a clone of the upstream repository at the commit named in the headers of
the ported Go files. The script imports that checkout's contextbench.tasks.<family>.generate
modules, runs every case, and prints one line per case:

  OK|MISSING  <table>  <case>  <op count>  <sha256>

The digest is over the operations in the wire form of chunks.jsonl, one line per operation
{"index": N, "header": "...", "body": "..."} (json.dumps with ensure_ascii=False and the default
separators). OK means the digest appears in the Go tables; MISSING means it does not, so the
port or the pin has drifted. Exit status 1 if any case is MISSING, 2 on a usage or API problem.
Only Python 3 and the upstream's own dependencies are needed; nothing is written and nothing
is fetched. If the upstream generate() parameter names change, the error lists the names it
accepts and the NAMES table below is the one place to adjust.
"""
import argparse
import hashlib
import importlib
import inspect
import json
import os
import sys

# Go knob -> the generate() parameter names to try, in order (the first one the function accepts).
NAMES = {
    "kv_store": {"records": ["records"], "value_words": ["value_words"], "queries": ["queries"],
                 "records_per_chunk": ["records_per_chunk"]},
    "log_triage": {"lines": ["lines"], "lines_per_batch": ["lines_per_batch"], "queries": ["queries"]},
    "needle_retention": {"chunks": ["chunks"], "needles_per_chunk": ["needles_per_chunk"],
                         "filler_lines": ["filler_lines_per_chunk", "filler_lines"],
                         "words_per_line": ["words_per_line"]},
    "sudoku_sketchpad": {"box": ["box"], "givens": ["givens"], "boards": ["boards"], "turns": ["turns"]},
}


def kv(records):
    return {"records": records, "value_words": 24, "queries": 24, "records_per_chunk": 100}


def log(lines, per_batch):
    return {"lines": lines, "lines_per_batch": per_batch, "queries": 24}


def needle(k, chunks):
    return {"needles_per_chunk": k, "chunks": chunks, "filler_lines": 140, "words_per_line": 10}


def sudoku(turns):
    return {"box": 4, "givens": 90, "boards": 1, "turns": turns}


# cbParityCases: (case, family, seed, knobs).
PARITY = [
    ("kv-R130-s42", "kv_store", 42, kv(130)),
    ("kv-R1000-s43", "kv_store", 43, kv(1000)),
    ("kv-more-queries-than-records", "kv_store", 7,
     {"records": 10, "value_words": 3, "queries": 30, "records_per_chunk": 4}),
    ("log-L160-s42", "log_triage", 42, log(160, 14)),
    ("log-L40-s45", "log_triage", 45, {"lines": 40, "lines_per_batch": 14, "queries": 10}),
    ("log-L1600-s49", "log_triage", 49, log(1600, 14)),
    ("needle-K4-C4-s42", "needle_retention", 42, needle(4, 4)),
    ("needle-K8-C6-s46", "needle_retention", 46,
     {"needles_per_chunk": 8, "chunks": 6, "filler_lines": 20, "words_per_line": 5}),
    ("sudoku-t30-s42", "sudoku_sketchpad", 42, sudoku(30)),
    ("sudoku-t4-s47", "sudoku_sketchpad", 47, sudoku(4)),
    ("sudoku-two-boards-by-turns", "sudoku_sketchpad", 44, sudoku(200)),
    ("sudoku-two-boards-by-count", "sudoku_sketchpad", 9, {"box": 3, "givens": 30, "boards": 2}),
]

# cbLevelPins: every published level at seed 42 (contextbench_levels.go, from configs/paper.yaml).
LEVELS = (
    [("kv_store", "R%d" % n, kv(n)) for n in (130, 260, 520, 1000, 2000, 4000, 8000, 16000)]
    + [("log_triage", "L%d" % n, log(n, b)) for n, b in
       ((160, 14), (320, 14), (640, 14), (1600, 14), (3200, 27), (6400, 54), (12800, 54))]
    + [("needle_retention", "K%d-C%d" % (k, c), needle(k, c)) for k, c in
       ((4, 4), (4, 6), (4, 8), (4, 24), (8, 48), (8, 60), (8, 62), (4, 96), (2, 192), (2, 224))]
    + [("sudoku_sketchpad", "t%d" % n, sudoku(n)) for n in (4, 8, 15, 30, 60, 100, 150)]
)


def die(msg):
    print("contextbench_parity_regen: " + msg, file=sys.stderr)
    sys.exit(2)


def chunks_of(result):
    """The operations list of whatever generate() returned: a list of dicts with a body, bare,
    inside a tuple/list, or under a "chunks" key."""
    def is_ops(x):
        return isinstance(x, list) and x and all(isinstance(c, dict) and "body" in c for c in x)
    if is_ops(result):
        return result
    if isinstance(result, dict) and is_ops(result.get("chunks")):
        return result["chunks"]
    if isinstance(result, (tuple, list)):
        for part in result:
            if is_ops(part):
                return part
    die("could not find the operations list in what generate() returned (%s)" % type(result).__name__)


def run_case(family, seed, knobs):
    mod = importlib.import_module("contextbench.tasks.%s.generate" % family)
    accepted = inspect.signature(mod.generate).parameters
    kwargs = {"seed": seed}
    for knob, value in knobs.items():
        for name in NAMES[family][knob]:
            if name in accepted:
                kwargs[name] = value
                break
        else:
            die("%s.generate() takes none of %s for the %r knob; it accepts %s"
                % (family, NAMES[family][knob], knob, sorted(accepted)))
    ops = chunks_of(mod.generate(**kwargs))
    wire = "".join(
        json.dumps({"index": op["index"], "header": op["header"], "body": op["body"]}, ensure_ascii=False) + "\n"
        for op in ops)
    return len(ops), hashlib.sha256(wire.encode("utf-8")).hexdigest()


def main(argv):
    ap = argparse.ArgumentParser(description="Regenerate the ContextBench parity digests the Go tests pin.")
    ap.add_argument("upstream", help="a clone of the upstream repository at the pinned commit")
    ap.add_argument("--repo", default=os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                    help="root of this repository (default: the parent of scripts/)")
    args = ap.parse_args(argv)
    sys.path.insert(0, args.upstream)
    pinned = ""
    for rel in ("contextbench_gen_test.go", "contextbench_levels_test.go"):
        with open(os.path.join(args.repo, "internal", "compeval", rel), encoding="utf-8") as f:
            pinned += f.read()
    missing = 0
    cases = [("cbParityCases", name, fam, seed, knobs) for name, fam, seed, knobs in PARITY]
    cases += [("cbLevelPins", "%s/%s" % (fam, level), fam, 42, knobs) for fam, level, knobs in LEVELS]
    for table, name, fam, seed, knobs in cases:
        n, digest = run_case(fam, seed, knobs)
        state = "OK" if digest in pinned else "MISSING"
        missing += state == "MISSING"
        print("%-7s %-13s %-30s %4d  %s" % (state, table, name, n, digest))
    return 1 if missing else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
