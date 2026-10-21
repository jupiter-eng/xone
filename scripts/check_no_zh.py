#!/usr/bin/env python3
# check.sh step 8.1: the repository contains no Chinese (see .claude/CLAUDE.md, "Language").
#
# The migration that translated everything predating the rule is over, and the allowlist that
# carried it (scripts/zh_allowlist.txt) is gone with it: a tracked file holding Chinese now fails
# the check outright, with nothing to add it to.
#
# Where a test has to assert on such text -- a library's translated validation message, say -- it is
# written as \u escapes, so the assertion keeps the exact string while the file itself stays clean
# (see e2e/gin_options_test.go).
#
# Detection covers Han characters plus CJK punctuation and fullwidth forms: a line can carry a
# fullwidth comma or fullwidth parentheses without a single Han character and still be Chinese
# typography. The ranges are spelled as code points and never as literal characters, because a
# checker that trips over its own source is a checker someone will delete.
#
# Only tracked (and staged) files are scanned, so a brand-new file has to be `git add`-ed before
# this check can see it. That is exactly how this script shipped broken once: it was run before
# the file was added, passed, and went red in CI.
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
CJK_RANGES = (
    (0x3000, 0x303F),  # CJK punctuation
    (0x3400, 0x4DBF),  # CJK extension A
    (0x4E00, 0x9FFF),  # CJK unified ideographs
    (0xF900, 0xFAFF),  # compatibility ideographs
    (0xFF01, 0xFF60),  # fullwidth forms
)
CJK = re.compile("[" + "".join(f"{chr(lo)}-{chr(hi)}" for lo, hi in CJK_RANGES) + "]")


def tracked_files():
    out = subprocess.run(
        ["git", "ls-files", "-z"], cwd=ROOT, capture_output=True, text=True, check=True
    ).stdout
    return [p for p in out.split("\0") if p]


def first_hit(path):
    # Binary files and anything that is not UTF-8 cannot carry Chinese prose; skip them rather
    # than failing the whole check on one PNG.
    try:
        text = (ROOT / path).read_text(encoding="utf-8")
    except (UnicodeDecodeError, FileNotFoundError, IsADirectoryError, OSError):
        return None
    for n, line in enumerate(text.splitlines(), 1):
        if CJK.search(line):
            return n, line.strip()
    return None


def main():
    offenders = []
    for p in sorted(tracked_files()):
        hit = first_hit(p)
        if hit:
            offenders.append((p, *hit))

    if offenders:
        print("These files contain Chinese:")
        for p, n, line in offenders:
            print(f"  {p}:{n}: {line[:100]}")
        print("Translate them. Text an assertion needs verbatim goes in as \\u escapes instead "
              "(see e2e/gin_options_test.go).")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
