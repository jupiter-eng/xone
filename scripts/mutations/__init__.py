# The mutation tables: one file per Go module (core.py is the root module), read in by
# scripts/mutate.py in file name order.
#
# Each mutation is one line, mutate(name, file, dir, test filter, edits...):
#   file          the source file to break, relative to the repository root
#   dir           where go test ./... runs (usually the module the file belongs to, but it may be
#                 another module depending on it, such as verifying an xtrace promise in ./example)
#   test filter   handed to go test -run
#   edits         swap() / cut() only, which bring their own "the pattern matches exactly N places"
#                 assertion
#
# The line goes in the file of the module "dir" belongs to, which mutate.py verifies.
# The comment above each one says which real incident this promise guards against.
from typing import NamedTuple


class Stale(Exception):
    """The mutation pattern did not match: this promise was never checked this round"""


def swap(old, new, count=1):
    """Replace old with new, asserting that it appears exactly count times.

    The assertion is the point, not an afterthought: once a refactor moves a piece of code, the
    pattern quietly stops matching, that promise is never checked again, and the whole round still
    reports green -- which has really happened twice in this repository.
    A wrong number of matches has to be caught too: one match too many means two places were broken,
    and a red test then says nothing about which promise is doing the work.
    """
    def edit(s):
        if (n := s.count(old)) != count:
            raise Stale(f"the pattern matched {n} places, expected {count}; this mutation has to follow the code:\n{old}")
        return s.replace(old, new)
    return edit


def cut(start, end_after):
    """Delete the whole block from start through the end of end_after"""
    def edit(s):
        if (n := s.count(start)) != 1:
            raise Stale(f"the start matched {n} places, expected 1:\n{start}")
        i = s.index(start)
        if (j := s.find(end_after, i)) < 0:
            raise Stale(f"the end could not be found:\n{end_after}")
        return s[:i] + s[j + len(end_after):]
    return edit


class Row(NamedTuple):
    name: str
    file: str
    module: str
    filt: str
    edits: tuple
    section: str


ROWS = []
_section = ""


def section(title):
    global _section
    _section = title


def mutate(name, file, module, filt, *edits):
    ROWS.append(Row(name, file, module, filt, edits, _section))
