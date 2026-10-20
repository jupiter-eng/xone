#!/usr/bin/env python3
# check.sh step 8.1: the repository contains no Chinese (see .claude/CLAUDE.md, "Language").
#
# Files written before that rule still contain Chinese; they are listed in
# scripts/zh_allowlist.txt. The list only ever shrinks, and this script enforces that in both
# directions:
#
#   - a tracked file with Chinese that is NOT on the list fails the check (nothing new slips in);
#   - a listed file that no longer has Chinese, or no longer exists, fails as a stale entry.
#
# The second direction is the one that keeps the list honest. Without it the list outlives the
# migration, nobody notices it is half fiction, and "the allowlist says so" stops meaning anything.
#
# Detection covers Han characters plus CJK punctuation and fullwidth forms: a line reading
# "see above（the table）" has no Han character and is still Chinese typography.
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
ALLOWLIST = ROOT / "scripts" / "zh_allowlist.txt"
CJK = re.compile(r"[　-〿㐀-䶿一-鿿豈-﫿！-｠]")


def tracked_files():
    out = subprocess.run(
        ["git", "ls-files", "-z"], cwd=ROOT, capture_output=True, text=True, check=True
    ).stdout
    return [p for p in out.split("\0") if p]


def has_cjk(path):
    # Binary files and anything that is not UTF-8 cannot carry Chinese prose; skip them rather
    # than failing the whole check on one PNG.
    try:
        return bool(CJK.search((ROOT / path).read_text(encoding="utf-8")))
    except (UnicodeDecodeError, FileNotFoundError, IsADirectoryError, OSError):
        return False


def read_allowlist():
    if not ALLOWLIST.exists():
        return set()
    entries = set()
    for line in ALLOWLIST.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if line and not line.startswith("#"):
            entries.add(line)
    return entries


def main():
    allowed = read_allowlist()
    files = tracked_files()
    present = set(files)

    offenders = sorted(p for p in files if p not in allowed and has_cjk(p))
    stale = sorted(p for p in allowed if p not in present or not has_cjk(p))

    if offenders:
        print("These files contain Chinese and are not in scripts/zh_allowlist.txt:")
        for p in offenders:
            print(f"  {p}")
        print("Translate them, or — only for a file that predates the rule — add them to the list.")
    if stale:
        print("These entries in scripts/zh_allowlist.txt are stale (translated or gone already):")
        for p in stale:
            print(f"  {p}")
        print("Delete those lines: the list only shrinks.")

    return 1 if offenders or stale else 0


if __name__ == "__main__":
    sys.exit(main())
