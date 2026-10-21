#!/usr/bin/env python3
# Mutation testing: break the code behind each promise and see whether a test fails.
#
#   scripts/mutate.py                 break, compile and test, with as many in parallel as there are
#                                     CPUs by default
#   scripts/mutate.py -j 2            two at a time
#   scripts/mutate.py --only xgorm    only the ones under a table / module / path prefix (several
#                                     may be given)
#   scripts/mutate.py -k timeout      only the ones whose name holds this text (several may be given)
#   scripts/mutate.py --dry-run       only apply the patterns in memory, running no go, in under a
#                                     second (the filters still apply)
#
# A mutation that survives = a promise with no teeth: the code says it, the documentation says it,
# and breaking it tells nobody.
# This repository has had more than twenty problems found by external reviews, and classified
# afterwards the vast majority were "the promise is there, the test is not". Rather than wait for
# somebody to find them every time, it says so itself.
#
# A full run takes a while and is not in the CI of every commit: it runs nightly, and by hand after a
# batch of safety- or lifecycle-related code has changed. --dry-run is different: it checks only
# "does each mutation's pattern match exactly N places", check.sh runs it every time, and a refactor
# that moved the code is known on the spot.
#
# Only a promise "our own code guards" belongs here. A property a dependency guarantees, such as "a
# ctx can cap a whole logical request", has no line here to break, and forcing a mutation for it
# would only distort the "survived = a missing test" signal -- such a promise is guarded by a test
# alone, which protects against a regression on upgrading the dependency rather than against our own
# mistakes.
#
# The mutation tables live under scripts/mutations/, one file per module, and how they are written is
# in the __init__.py there.
#
# The broken file is never written into the working tree: the modified copy goes into a temporary
# directory and is swapped into the compilation through go test -overlay.
# Measured (go 1.25):
#   - the compilation, the set of vet checks go test brings along, and another module depended on
#     through a replace all honour the overlay -- running under ./xgin, the stand-in for
#     xlog/config.go is compiled in as well;
#   - the build cache goes by the stand-in's content: a run with the overlay followed by one without
#     gets the original code, and several parallel runs sharing one cache do not disturb each other;
#   - runtime.Caller still reports the original path; only the compiler's and vet's errors hold the
#     stand-in's path, which is turned back into the original path when printed;
#   - only the compilation is swapped: source a test reads from disk at run time does not see the
#     stand-in. No test currently reads a mutated source file (what schemagen reads is another
#     module's Config, while what is mutated in it is schema.go / docs.go).
# So the working tree need not be clean, the code may be changed while it runs, and an interrupted
# run leaves nothing to write back.
import argparse
import concurrent.futures
import importlib
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import time

ROOT = pathlib.Path(__file__).resolve().parent.parent
sys.dont_write_bytecode = True  # leave no __pycache__ under scripts/
sys.path.insert(0, str(ROOT / "scripts"))
import mutations  # noqa: E402
from mutations import Stale  # noqa: E402


def load():
    """Reads scripts/mutations/*.py in file name order and returns [(table name, Row)]"""
    rows = []
    for path in sorted((ROOT / "scripts" / "mutations").glob("[!_]*.py")):
        mutations.section("")
        before = len(mutations.ROWS)
        importlib.import_module(f"mutations.{path.stem}")
        rows += [(path.stem, r) for r in mutations.ROWS[before:]]
    return rows


def table_of(module):
    """Which table a row belongs in: the Go module the test directory belongs to -- core for the
    root module, and the module directory's name otherwise"""
    d = (ROOT / module).resolve()
    while not (d / "go.mod").exists():
        d = d.parent
    return "core" if d == ROOT else d.name


def picked(table, r, only, keys):
    def under(p):
        p = p.removeprefix("./").rstrip("/")
        return any(s == p or s.startswith(p + "/") for s in (table, r.module.removeprefix("./"), r.file))
    return (not only or any(map(under, only))) and (not keys or any(k in r.name for k in keys))


def apply(r):
    """Applies the edits in memory and returns (the modified content, None); a stale pattern
    returns (None, an explanation)"""
    orig = (ROOT / r.file).read_text(encoding="utf-8")
    try:
        s = orig
        for e in r.edits:
            s = e(s)
    except Stale as err:
        return None, f" (the mutation did not apply; the place it breaks may no longer be there)\n      {err}"
    # Changing nothing is as serious as "broken and nobody noticed": this promise was never checked
    # this round, while the script used to report green all the same. A refactor that moved a piece of
    # code is exactly how a mutation quietly goes stale
    if s == orig:
        return None, " (the mutation changed nothing; the pattern is stale)"
    return s, None


def run(r, mutated, tmp):
    """Runs one mutation and returns (killed / survived / broken, the lines printed under the
    result)"""
    fake = tmp / r.file
    fake.parent.mkdir(parents=True)
    fake.write_text(mutated, encoding="utf-8")
    overlay = tmp / "overlay.json"
    overlay.write_text(json.dumps({"Replace": {str(ROOT / r.file): str(fake)}}))

    def go_test(*args):
        return subprocess.run(["go", "test", "-count=1", f"-overlay={overlay}", *args, "./..."],
                              cwd=ROOT / r.module, env=dict(os.environ, GOWORK="off"),
                              capture_output=True, text=True, errors="replace")  # a test's output may hold bytes that are not UTF-8

    # It compiles without running first (-exec true: the test binary is built as usual, along with
    # the set of vet checks go test brings, and handed to true to be "executed"). A mutation that does
    # not compile used to count as "killed": the test went red, but it went red over declared and not
    # used rather than a test noticing a change in behavior -- that promise was likewise never checked
    # this round, which is as serious as a stale one and is reported separately
    build = go_test("-exec", "true")
    if build.returncode != 0:
        lines = (build.stdout + build.stderr).replace(str(fake), r.file).splitlines()
        return "broken", [l for l in lines if not l.startswith(("ok ", "FAIL", "? "))][:3]
    # -timeout is required: some mutations hang a test rather than fail it (swapping a ctx for
    # Background, say, leaves the step waiting for a signal waiting forever). Without an upper bound
    # one such mutation would hang the whole round there. Hanging means the test noticed it as well
    # and counts as killed
    if go_test("-timeout", "90s", "-run", r.filt).returncode == 0:
        return "survived", []
    return "killed", []


MARK = {"killed": "✓", "survived": "✗", "stale": "?", "broken": "!"}
NOTE = {"survived": " -- broken and every test still passed",
        "broken": " (it does not compile once mutated and was never checked this round)"}


def main():
    sys.stdout.reconfigure(line_buffering=True)  # the progress has to be visible one by one through a tee or into a file too
    ap = argparse.ArgumentParser(description="Mutation testing: break each promise and see whether a test fails")
    ap.add_argument("--dry-run", action="store_true", help="check the patterns alone, running no go")
    ap.add_argument("-j", type=int, default=os.cpu_count() or 1, metavar="N", help="how many run in parallel, the number of CPUs by default")
    ap.add_argument("--only", action="append", default=[], metavar="PREFIX",
                    help="only the ones under this table (core, xgorm, clickhouse and so on), module or path prefix")
    ap.add_argument("-k", action="append", default=[], metavar="TEXT", help="only the ones whose name holds this text")
    args = ap.parse_args()
    t0 = time.monotonic()

    rows = load()
    misplaced = [f"{t}.py: {r.name} (runs in {r.module} and belongs in {table_of(r.module)}.py)"
                 for t, r in rows if table_of(r.module) != t]
    if misplaced:
        sys.exit("✗ these mutations are in the wrong table:\n  " + "\n  ".join(misplaced))
    rows = [(t, r) for t, r in rows if picked(t, r, args.only, args.k)]
    if not rows:
        sys.exit("✗ the filters left no mutation at all")

    got = dict.fromkeys(MARK, 0)
    # Every pattern is checked in memory first: a stale one need not queue for a compilation, and a
    # dry run ends here
    plan = []
    for t, r in rows:
        mutated, why = apply(r)
        if why:
            print(f"  ? {t} · {r.name}{why}")
            got["stale"] += 1
        else:
            plan.append((t, r, mutated))

    if args.dry_run:
        got["killed"] += len(plan)
    else:
        # The results are printed in the table's order rather than the order they finish in: the
        # output of two rounds can be diffed directly
        with tempfile.TemporaryDirectory(prefix="xone-mutate-") as tmp, \
                concurrent.futures.ThreadPoolExecutor(max(1, args.j)) as pool:
            futures = [pool.submit(run, r, mutated, pathlib.Path(tmp, str(i)))
                       for i, (_, r, mutated) in enumerate(plan)]
            header = None
            try:
                for (t, r, _), f in zip(plan, futures):
                    if header != (t, r.section):
                        header = (t, r.section)
                        print(f"== {t} · {r.section} ==")
                    status, detail = f.result()
                    got[status] += 1
                    print(f"  {MARK[status]} {r.name}{NOTE.get(status, '')}")
                    for line in detail:
                        print(f"      {line}")
            except KeyboardInterrupt:
                # go test and the script share a process group and Ctrl-C has reached them already;
                # whatever has not started must not start now
                pool.shutdown(wait=False, cancel_futures=True)
                sys.exit(130)

    total = sum(got.values())
    took = f"{time.monotonic() - t0:.1f}s"
    if got["killed"] == total:
        if args.dry_run:
            print(f"✓ all {total} mutation patterns match exactly ({took})")
        else:
            print(f"\n✓ all {total} promises are watched by a test ({took}, -j {args.j})")
        return
    print()
    if got["survived"]:
        print(f"✗ {got['survived']} were broken and nobody noticed")
    if got["stale"]:
        print(f"✗ {got['stale']} mutation patterns are stale and were never checked this round "
              "(most likely a refactor moved that code)")
    if got["broken"]:
        print(f"✗ {got['broken']} do not compile once mutated, so the tests went red over the "
              "compilation rather than a change in behavior; change the mutation to make it compile")
    print(f"  -- {total} in total ({took})")
    sys.exit(1)


if __name__ == "__main__":
    main()
