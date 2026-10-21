#!/usr/bin/env python3
# check.sh step 8: finds Chinese inside the strings that reach run time and prints one "file:line"
# per place.
# The files to check come from the GOFILES environment variable (whitespace separated).
#
# A file of its own rather than a heredoc inside check.sh: macOS's /bin/sh (bash 3.2) tries to pair
# the backticks and quotes inside a heredoc while parsing $(... <<'EOF' ...), the regexps below are
# certain to hold some, and the whole of check.sh would then report a syntax error and not run one
# step
import os
import re

# The CJK range is written as escapes so that this file holds no Chinese itself
zh = re.compile(r'[\u4e00-\u9fff]')
# The whole file is scanned once, recognizing tokens left to right: a comment, a "..." (escapes
# included), a `...` (a raw string, which may span lines) and a '...' (a rune, recognized only so
# that a '"' is not taken for the start of a string).
# The scan continues after a token has been recognized, so a quote inside a comment and a // inside a
# string are never mistaken for one.
# It used to split('//') line by line and then look for "...": a string holding http:// was cut at
# the //, and a backtick string was not looked at at all, so Chinese inside one went through
token = re.compile(r'//[^\n]*|/\*.*?\*/|"(?:[^"\\\n]|\\.)*"|`[^`]*`|\'(?:[^\'\\\n]|\\.)*\'', re.S)
for f in os.environ['GOFILES'].split():
    if f.endswith('_test.go'):
        continue
    # A build tool is not bound by this rule: its output goes to whoever is changing this code and
    # never lands in a caller's logs, which is the whole reason the rule exists
    if f.startswith('internal/schemagen/'):
        continue
    src = open(f, encoding='utf-8').read()
    seen = set()
    for m in token.finditer(src):
        if m.group()[0] in '"`' and zh.search(m.group()):
            line = src.count('\n', 0, m.start()) + 1
            if line not in seen:
                seen.add(line)
                print(f"{f}:{line}")
