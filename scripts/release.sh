#!/bin/sh
# Release. Each module of this multi-module repository has tags of its own, and all of them go onto
# the same commit on main.
#
# Two steps, with an ordinary PR in between (main's branch protection applies as usual):
#
#   scripts/release.sh v0.1.0 --bump     # 1. pin the in-repository require lines of every module's
#                                        #    go.mod to v0.1.0, turn the CHANGELOG's Unreleased
#                                        #    section into this version, update the version in the
#                                        #    README install command; it only changes files, it
#                                        #    does not commit
#   (commit, open the PR, CI goes green, merge into main)
#   scripts/release.sh v0.1.0 --tag      # 2. tag every module on main's latest commit (no push)
#   scripts/release.sh v0.1.0 --smoke    # before the push: the same external project with its
#                                        #    dependencies pointing at the local code; it comes up
#                                        #    and shuts down in the right order
#   scripts/release.sh v0.1.0 --verify   # after the push: a brand new external project proves the
#                                        #    release installs and runs
#
# Day to day, step 2 is done with the release button in GitHub Actions
# (.github/workflows/release.yml): the same checks, then --smoke, and once that is through it
# pushes the tags only and runs --verify.
#
# Why main's go.mod can be released as it is: the replace lines development uses stay in, and a
# caller's build ignores a replace sitting in a dependency -- it goes by the version in require
# alone. So a release only has to pin require to the version being released, replace does not have
# to be taken out, and that means no separate release commit and revert commit: the tags go
# straight onto main.
#
# --tag wants one green e2e run (real PG / MySQL / Redis) before the release: it runs scripts/e2e.sh
# at this step by default. When this commit has gone green elsewhere, or e2e runs once more before
# the push (the release button runs it right after the tags are made), --e2e-passed skips the e2e
# here, and whoever passes it vouches for that.
#
# The push is a step of its own: Go's module proxy caches a tag for good, and a wrong one cannot be
# deleted, only covered by another release.
set -e
cd "$(dirname "$0")/.."
. scripts/lib.sh

usage() { echo "usage: scripts/release.sh vX.Y.Z (--bump | --tag [--e2e-passed] | --smoke | --verify)"; exit 1; }
VERSION="$1"
[ -n "$VERSION" ] || usage
shift
MODE=
E2E_PASSED=
for a; do
  case "$a" in
    --bump|--tag|--smoke|--verify) [ -z "$MODE" ] || usage; MODE=$a;;
    --e2e-passed) E2E_PASSED=1;;
    *) usage;;
  esac
done
[ -n "$MODE" ] || usage
case "$VERSION" in
  # v0 / v1 only: the module paths carry no /v2 suffix, and go mod edit refuses a version of v2 or
  # above
  v0.*.*|v1.*.*) ;;
  v*.*.*) echo "the module paths have no /vN suffix, only v0 / v1 can be released, got=$VERSION"; exit 1;;
  *) echo "the version has to look like vX.Y.Z, got=$VERSION"; exit 1;;
esac

MOD=github.com/jupiter-eng/xone

# The submodules to release: every checked-in subdirectory go.mod except these (the root module
# gets a tag of its own without a prefix):
#   example             an example is not a library, its replace lines stay in for good so that it
#                       always compiles against the repository's current code and never against
#                       some released version
#   e2e                 tests against real services, for the same reason as example
#   internal/schemagen  the repository's own build tool, not for a caller to import
#
# This used to be a hand-written "release order" table, but all the tags go onto the same commit
# and are pushed in one go, so which comes first makes no difference; the hand-written table, on
# the other hand, had to be remembered whenever a module was added
#
# Pinning (PINNED) has to cover example / e2e / internal/schemagen, which are not released: once
# the submodules they require are pinned to $VERSION and they in turn require the core's
# ${VERSION}, a go.mod of their own still saying v0.0.0 has go report that go.mod needs updating
# and check.sh's vet goes red on the spot. Only $MODS is tagged
PINNED=$(git ls-files '*/go.mod' | xargs -n1 dirname)
MODS=$(echo "$PINNED" | grep -vxE 'example|e2e|internal/schemagen')

# ---- --smoke / --verify: a brand new external project installs it and runs it ----
# Both modes run the same consumer project and the same checks (start the service -> request ->
# SIGTERM -> shut down in reverse order) and differ only in where the dependencies come from:
#
#   --verify  has to run after git push --tags: go get takes the released version off the module
#             proxy. CI runs the code in the working tree, where the modules still point at each
#             other through replace, which proves nothing about what a real caller meets after a
#             go get -- whether the require versions are right, whether every tag was made, and
#             whether it still compiles without the replace lines are questions only this step
#             answers.
#   --smoke   runs before the push with the dependencies pointing at this working tree (which is
#             the commit that was just tagged): the consumer is the main module and its replace
#             lines are the ones that count, so the replace lines the modules point at each other
#             with are ignored, the same as in a caller's build. --verify once went red after the
#             tags had been pushed (the sed recognizing the hooks no longer matched the new log
#             fields) and a pushed tag cannot be taken back -- the same checks run here first, so a
#             regression finds the tags still local.
#             It cannot tell whether every tag was made or whether the proxy serves them; those
#             stay with --verify
consumer() {
# The same submodule list the release uses, so a newly added module comes along by itself
PKGS=$(printf "$MOD/%s\n" $MODS)
ROOT=$(pwd)
DIR=$(mktemp -d)
trap 'rm -rf "$DIR"' EXIT

if [ "$MODE" = "--smoke" ]; then
  # Changes in the working tree mean this is not the commit that was tagged; say so
  [ -z "$(git status --porcelain)" ] || echo "  ⚠ the working tree has uncommitted changes, so this runs the changed code and not $(git log -1 --format=%h)"
  echo "== the local $(git log -1 --format='%h %s') stands in for $VERSION =="
fi

echo "== building a clean consumer project in $DIR =="
cd "$DIR"
cat > go.mod <<MOD_EOF
module verify

go 1.25
MOD_EOF

# What a real caller would write: start the service, connect to the databases, record metrics, all
# through the public API.
#
# Every module the go get below takes has to be imported here: go mod tidy drops a require nobody
# imports and that module is then never compiled at all -- which is what xcache and xginswagger
# used to be, so "it installs and compiles" proved nothing about either. The imports therefore come
# from the same list, one _ import per module; xgin / xmetric have a named line as well, and Go
# allows the same package both ways
{
  cat <<'GO_EOF'
package main

import (
	"github.com/gin-gonic/gin"

	"github.com/jupiter-eng/xone"
	"github.com/jupiter-eng/xone/xgin"
	"github.com/jupiter-eng/xone/xmetric"
GO_EOF
  printf '\t_ "%s"\n' $PKGS
  cat <<'GO_EOF'
)

func main() {
	xone.MustRun(xgin.New().WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) {
			xmetric.CounterInc("ping_total")
			c.String(200, "pong")
		})
	}))
}
GO_EOF
} > main.go

if [ "$MODE" = "--smoke" ]; then
  echo "== require ${VERSION} of every module, replaced with $ROOT =="
  # Not through go get: this version has not been pushed and the proxy does not have it. require
  # still says ${VERSION}, which is the line a caller's go.mod will hold, only its content comes
  # from a local directory
  for p in "$MOD" $PKGS; do
    echo "  $p@$VERSION => $ROOT${p#"$MOD"}"
    go mod edit -require="$p@$VERSION" -replace="$p=$ROOT${p#"$MOD"}"
  done
else
  echo "== go get $VERSION of every module =="
  # Every module in one go get: one go get per module resolves the whole dependency graph again
  # each time. Measured on an empty cache (what verify's fresh machine looks like in CI), 12
  # modules take 43s one by one and 23s in one go
  set --
  for p in "$MOD" $PKGS; do
    echo "  $p@$VERSION"
    set -- "$@" "$p@$VERSION"
  done
  GOFLAGS=-mod=mod go get "$@" >/dev/null
fi

echo "== compiling =="
go mod tidy >/dev/null
go build -o verify .
echo "  ✓ it installs and compiles"

echo "== one run: start -> request -> exit =="
./verify --config=/dev/null >out.txt 2>&1 &
pid=$!
# By the time this runs the service has usually exited already and the kill fails, which is normal:
# without the || true, set -e makes that failure the whole script's exit code -- one release
# reported exit 1 right after the "verified" line
trap 'kill -9 "$pid" 2>/dev/null || true; rm -rf "$DIR"' EXIT

i=0
until curl -sf http://127.0.0.1:8080/ping >/dev/null 2>&1; do
  i=$((i + 1))
  [ "$i" -lt 50 ] || { echo "✗ the service did not come up"; cat out.txt; exit 1; }
  sleep 0.2
done
echo "  ✓ /ping answered"

kill -TERM "$pid"
i=0
while kill -0 "$pid" 2>/dev/null; do
  i=$((i + 1))
  [ "$i" -lt 100 ] || { echo "✗ it did not exit after the SIGTERM"; cat out.txt; exit 1; }
  sleep 0.2
done
echo "  ✓ it exited cleanly after the SIGTERM"

# This used to grep for "closing", which matched the line saying the signal had arrived
# ("shutdown signal received, closing gracefully ..."), so it passed with not one component closed.
# hooks lives in scripts/lib.sh and check.sh tests it against a sample log
hooks starting out.txt > started.txt
hooks stopping out.txt > stopped.txt
# The packages both sides hold: the stop order has to be exactly the start order reversed
awk '{ l[NR] = $0 } END { for (i = NR; i > 0; i--) print l[i] }' started.txt | grep -Fxf stopped.txt > want.txt || true
grep -Fxf started.txt stopped.txt > got.txt || true
[ "$(wc -l < got.txt)" -ge 2 ] || { echo "✗ no component shutdown lines in the log"; cat out.txt; exit 1; }
cmp -s want.txt got.txt || { echo "✗ the shutdown order is not the reverse of the start order"; echo "  start: $(tr '\n' ' ' < started.txt)"; echo "  stop:  $(tr '\n' ' ' < stopped.txt)"; exit 1; }
# The log starts first and closes last, which is what lets the other components' shutdown lines be
# written at all
[ "$(tail -n 1 got.txt)" = "xlog" ] || { echo "✗ xlog is not the last one closed: $(tr '\n' ' ' < got.txt)"; exit 1; }
echo "  ✓ the components were closed in reverse order: $(tr '\n' ' ' < got.txt)"
echo
if [ "$MODE" = "--smoke" ]; then
  echo "✓ $VERSION passed the smoke run (local code, not pushed yet)"
else
  echo "✓ $VERSION verified"
fi
}
case "$MODE" in --smoke|--verify) consumer; exit 0;; esac

# inrepo go.mod: lists the in-repository modules in require (path@version).
# require only: this used to grep the whole -json output for the path, so one appearing in a
# replace alone (xgin replaces xtrace without requiring it) had a require line added as well, and
# the released go.mod carried a dependency it never had
inrepo() {
  GOWORK=off go mod edit -json "$1" | python3 -c '
import json, sys
mod = sys.argv[1]
for e in json.load(sys.stdin).get("Require") or []:
    p = e["Path"]
    if p == mod or p.startswith(mod + "/"):
        print(p + "@" + e["Version"])' "$MOD"
}

# unpinned: the in-repository require lines not pinned to $VERSION yet, one
# "module: dep@version" per line
unpinned() {
  for m in $PINNED; do
    for dep in $(inrepo "$m/go.mod"); do
      [ "${dep##*@}" = "$VERSION" ] || echo "  $m: $dep"
    done
  done
}

# has_entries <heading prefix>: whether the section of docs/CHANGELOG.md starting with it (up to
# the next "## ") holds an entry starting with "- ". A ### subheading is not an entry: a section
# holding only "### Fixed" and nothing under it is empty all the same
has_entries() {
  awk -v h="$1" '
    index($0, h) == 1 { s = 1; next }
    s && /^## / { exit }
    s && /^- / { found = 1; exit }
    END { exit !found }' docs/CHANGELOG.md
}

if [ "$MODE" = "--bump" ]; then
  # Turning an empty Unreleased section into a version number ships a version with no changes
  # written down: the caller cannot tell what changed, which means either the CHANGELOG was
  # forgotten or there was nothing to release. Checked before any file is touched, so a refusal
  # leaves everything as it was. When the section already exists (a --bump rerun) step 2 leaves the
  # CHANGELOG alone, and Unreleased is not checked here either.
  if ! grep -q "^## \[$VERSION\]" docs/CHANGELOG.md && ! has_entries "## [Unreleased]"; then
    echo "✗ the ## [Unreleased] section of docs/CHANGELOG.md is empty (no lines starting with -): write this version's caller-visible changes there first, or do not release"
    exit 1
  fi

  echo "== 1. pin the in-repository require lines of every module's go.mod to ${VERSION} (replace stays) =="
  # go mod edit rather than sed: the indentation of a require block and the submodule path suffixes
  # are easy to get wrong in a hand-written regexp, and getting them wrong releases a version that
  # cannot be installed.
  # v0.0.0, the previous version and a pseudo-version the go tool filled in itself are all
  # rewritten the same way
  for m in $PINNED; do
    for dep in $(inrepo "$m/go.mod"); do
      GOWORK=off go mod edit -require="${dep%@*}@$VERSION" "$m/go.mod"
    done
  done
  left=$(unpinned)
  [ -z "$left" ] || { echo "✗ some in-repository dependencies are still unpinned:"; echo "$left"; exit 1; }
  echo "  ✓ the go.mod of all $(echo $PINNED | wc -w) submodules is pinned to $VERSION"

  echo "== 2. CHANGELOG: Unreleased becomes ${VERSION}, with a new empty Unreleased above it =="
  if grep -q "^## \[$VERSION\]" docs/CHANGELOG.md; then
    echo "  the CHANGELOG already has a $VERSION section, leaving it alone"
  else
    python3 - "$VERSION" "$(date +%Y-%m-%d)" <<'PY'
import sys
v, day = sys.argv[1], sys.argv[2]
p = "docs/CHANGELOG.md"
s = open(p).read()
head = "## [Unreleased]\n"
assert s.count(head) == 1, "no unique '## [Unreleased]' found in the CHANGELOG"
open(p, "w").write(s.replace(head, "## [Unreleased]\n\n## [%s] - %s\n" % (v, day), 1))
PY
    echo "  ✓ done, this version's entries go under ## [$VERSION]"
  fi

  echo "== 3. the install command in the README becomes $VERSION =="
  # Every module shares one version number: the README pins this one so that whoever copies it ends
  # up with the same version everywhere
  # perl rather than sed -i -E: macOS's BSD sed wants a backup suffix after -i and took -E for one,
  # so extended regexps were off, \1 reported "not defined in the RE", and --bump stopped here on a
  # Mac
  perl -pi -e "s#(github\.com/jupiter-eng/xone(/[a-z/]+)?)\@v[0-9]+\.[0-9]+\.[0-9]+#\${1}\@$VERSION#g" README.md
  grep -q "xone@$VERSION" README.md || { echo "✗ no install command found in the README"; exit 1; }
  echo "  ✓ the README's go get says $VERSION"

  echo "== 4. checks =="
  scripts/check.sh
  cat <<TIP

The files have been changed, nothing was committed. Next:

  git switch -c release/$VERSION && git commit -am "release: $VERSION" && git push -u origin release/$VERSION

Open a PR and merge it into main (main's go.mod then requires ${VERSION} while development carries
on through replace); once it is in, press Actions -> release -> Run workflow on GitHub, or locally:

  scripts/release.sh $VERSION --tag
TIP
  exit 0
fi

# ---- --tag ----
echo "== 1. making sure $VERSION can be released from this commit =="
[ -z "$(git status --porcelain)" ] || { echo "✗ the working tree has uncommitted changes, commit or stash them first"; exit 1; }
# Not one tag of this set may exist already and all of them are checked before any is made: going
# by the root tag alone would have a leftover xgin/$VERSION fail step 3 halfway through, leaving
# half a set of tags behind
taken=
for t in "$VERSION" $(printf "%s/$VERSION\n" $MODS); do
  if git rev-parse -q --verify "refs/tags/$t" >/dev/null; then taken="$taken $t"; fi
done
if [ -n "$taken" ]; then
  cat <<TIP
✗ these tags already exist: ${taken# }

  - this version has been pushed already (git ls-remote --tags origin lists ${VERSION}): pick
    another version number;
  - they are left over from a local --tag / --smoke that failed and were never pushed: delete them
    and run --tag again --

      git tag -d \$(git tag --points-at $VERSION)

    when the root tag $VERSION is gone and only submodule tags are left, that line lists none of
    them; delete them directly: git tag -d$taken
TIP
  exit 1
fi
left=$(unpinned)
[ -z "$left" ] || { echo "✗ this commit's go.mod is not pinned to ${VERSION} yet, merge the PR from scripts/release.sh $VERSION --bump first:"; echo "$left"; exit 1; }
grep -q "^## \[$VERSION\]" docs/CHANGELOG.md || { echo "✗ docs/CHANGELOG.md has no ## [$VERSION] section"; exit 1; }
# --bump turned Unreleased into this section; an empty one means Unreleased held nothing before --bump
has_entries "## [$VERSION]" || { echo "✗ the ## [$VERSION] section of docs/CHANGELOG.md is empty (no lines starting with -): write this version's caller-visible changes there first"; exit 1; }
echo "  ✓ $(git log -1 --format='%h %s')"

# The output is not swallowed: when it goes red those lines are exactly the ones to read
echo "== 2. one run of the checks, the tests and e2e =="
scripts/check.sh
scripts/test.sh -count=1
# The unit tests have no database; the layer that talks to real services is visible to e2e alone
# and it has to have gone green once before a release
if [ -n "$E2E_PASSED" ]; then
  echo "  ⚠ skipping e2e: --e2e-passed vouches that this commit's e2e has just gone green"
else
  scripts/e2e.sh
fi
echo "  ✓ passed"

# The released go.mod carries replace ../xxx: a caller's build ignores a replace in a dependency
# and goes by the $VERSION in require -- those are the tags made at this step.
# Whether it compiles without the replace lines cannot be checked here (the tags are not pushed, so
# the dependencies cannot be fetched); that is for --verify after the push
echo "== 3. tagging $(git log -1 --format=%h) =="
git tag "$VERSION"
echo "  $VERSION"
for m in $MODS; do
  git tag "$m/$VERSION"
  echo "  $m/$VERSION"
done

cat <<TIP

The tags have been made locally, nothing was pushed. Before the push, smoke test the local code
(the same external project and the same checks as --verify; a red run leaves the tags local):

  scripts/release.sh $VERSION --smoke

When the smoke run goes red and the code has to change, delete this set of local tags before
running --tag again:

  git tag -d \$(git tag --points-at $VERSION)

Once that is confirmed, push this set of tags only:

  git push --atomic origin \$(git tag --points-at $VERSION | sed 's|^|refs/tags/|')

After the push the tags are cached by the module proxy for good: they cannot be deleted, only
covered by another release.
Once pushed, it has to be verified that the release installs -- CI runs the code in the working
tree, which proves nothing about the released version:

  scripts/release.sh $VERSION --verify
TIP
