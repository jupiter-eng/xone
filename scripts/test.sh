#!/bin/sh
# Runs the whole repository's tests. go test ./... does not cross module boundaries, so the modules
# are run one by one.
#
#   scripts/test.sh                  # with the race detector, which is what CI runs
#   XONE_TEST_RACE=0 scripts/test.sh # without it, about twice as fast, for the loop while coding
#   XONE_TEST_JOBS=1 scripts/test.sh # one module at a time
# The module list comes from scripts/lib.sh's modules(), which check.sh shares: the checked-in ones
# plus the newly created ones that have not been git added, with the deleted but unstaged ones left
# out. Going by the checked-in ones alone would have check.sh already checking a newly added module
# while its tests were not run here.
#
# One module going red still runs the rest, and they are reported together at the end: which modules
# are red is visible at once, rather than fixing one, running again and finding the next.
#
# The modules do not depend on each other and several run at a time: compiling with -race takes most
# of it, and run one after another the CPU spends most of its time waiting.
# Each module's output goes into a file of its own and is printed in module order once everything has
# run, so nothing interleaves.
# How many run at a time defaults to the number of CPUs and XONE_TEST_JOBS changes it (1 being the
# old one-by-one run)
set -e
cd "$(dirname "$0")/.."
. scripts/lib.sh

jobs=${XONE_TEST_JOBS:-$(getconf _NPROCESSORS_ONLN)}
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

# The race detector is on and XONE_TEST_RACE=0 turns it off, for a quick loop while writing code.
# It is most of what a warm run costs: measured on this repository, xbeego 12.1s -> 3.6s and
# xgin 11.6s -> 6.5s, and much of that is not the cases at all but the test binaries' start -- a
# -race binary that runs no case takes about 1.5s per package, which is 44% of xgin's run and 37%
# of xbeego's.
# Turning it off is for the loop and not for the gate: CI runs this script as it is, and e2e builds
# its processes with -race too, so a race nobody saw locally is still caught before it is merged.
# $race is deliberately unquoted where it is used: empty, it adds no argument at all
race=-race
if [ "${XONE_TEST_RACE:-1}" != 1 ]; then
  race=
  echo "── the race detector is off (XONE_TEST_RACE=0); CI and e2e still run with it"
fi
export race

# One sh per module: $1 is the module directory and the rest is handed to go test as it is. A red one
# leaves a .failed marker behind and its exit code is not passed out: xargs stops early on a non-zero
# one, which would no longer be "run the rest"
modules | xargs -I{} -P "$jobs" sh -c '
  m=$1; shift
  log="$0/$(printf %s "$m" | tr / _)"
  (cd "$m" && GOWORK=off go test $race "$@" ./...) >"$log.log" 2>&1 || : >"$log.failed"
' "$out" {} "$@"

failed=
for m in $(modules); do
  log="$out/$(printf %s "$m" | tr / _)"
  echo "── $m"
  cat "$log.log"
  [ ! -e "$log.failed" ] || failed="$failed $m"
done
if [ -n "$failed" ]; then
  echo
  echo "✗ the tests of these modules did not pass:$failed"
  exit 1
fi
