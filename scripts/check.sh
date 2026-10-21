#!/bin/sh
# Compiles the design constraints into checks. Added while there is nothing to violate them yet --
# added later it would have to exempt a pile of what is already there, which is the same as not
# adding it.
set -e
cd "$(dirname "$0")/.."
fail() { echo "✗ $1"; exit 1; }

. scripts/lib.sh
modules=$(modules)
gosrc=$(files '*.go' | grep -v '_test\.go$')  # the non-test Go sources

# ---- 1. The core's dependency footprint ----
# A caller importing any package of the core carries every dependency constraint of the core, so the
# core has to stay extremely thin.
# Every addition starts with the question: can it be left out.
# GOWORK=off is required: inside a workspace, go list -m all merges every module's dependencies
# together.
deps=$(GOWORK=off go list -m all)
n=$(echo "$deps" | grep -vc '^github.com/jupiter-eng/xone')
[ "$n" -le 3 ] || fail "the core module graph holds $n modules, above the limit of 3 (it should be yaml and its test dependencies)"
echo "✓ the core module graph holds $n (limit 3)"

# ---- 2. The core must depend on no integration module ----
# The other way round would carry the integrations' dependencies back to everybody and splitting the
# modules would have been pointless
! echo "$deps" | grep -q '^github.com/jupiter-eng/xone/' \
  || fail "the core requires an integration module, which defeats the point of splitting them"
echo "✓ the core depends on no integration module"

# ---- 3. xhook and the base packages must have zero third-party dependencies ----
# xhook is a package every integration has to know (the other being xconfig), and one dependency of
# its own is forced onto every integration.
# xtls likewise: it is a field type inside xgorm / xredis / xhttp / xkafka's configuration structs.
# internal/web is the half the web integrations share: one third-party package slipping in is
# carried by every web integration.
# go list's output goes into a variable first: piped directly, a package that moved has go list fail,
# grep -c count 0, and the check pass without a word
for pkg in ./xhook ./xerror ./xutil ./xtls ./internal/web; do
  list=$(GOWORK=off go list -deps "$pkg") || fail "go list -deps $pkg failed (did the package move? update the list here)"
  d=$(echo "$list" | grep -E '^[^/]*\.' | grep -vc jupiter-eng || true)
  [ "$d" -eq 0 ] || fail "$pkg has $d third-party packages in it"
done
echo "✓ xhook / xerror / xutil / xtls / internal/web have zero third-party dependencies"

# xlog replaces slog.Default() and comes along with xgin / xecho / xcron alone (they are "the body
# of the process": a web service or a scheduled-job process, which want JSON logs and trace_id to
# begin with). No data integration, xtrace or xmetric may bring it in: otherwise a program using only
# xgorm would have its logging taken over again. xtrace / xmetric / xkafka install into
# internal/logext and never import xlog.
# Every package of every module is checked (./...), not the one at the module root alone: xflow,
# xonetest and internal/... inside the core bringing xlog in would take a caller's logging over just
# as well. xlog's own Deps do not hold itself, so it needs no exclusion
xlog=github.com/jupiter-eng/xone/xlog
for m in . xtrace xmetric xgorm xredis xhttp xcache xkafka; do
  out=$(cd "$m" && GOWORK=off go list -f "{{.ImportPath}}{{range .Deps}}{{if eq . \"$xlog\"}} <-xlog{{end}}{{end}}" ./...) \
    || fail "$m: go list failed (did the module move? update the list here)"
  [ -n "$out" ] || fail "$m: go list listed not one package"
  bad=$(echo "$out" | sed -n 's/ <-xlog$//p')
  [ -z "$bad" ] || fail "these packages of $m bring xlog in (the root module, the data integrations, xtrace and xmetric all must not):
$bad"
done
echo "✓ xgin / xecho / xcron alone bring xlog along"

# ---- 4. Only an integration may have an init() ----
# An integration's init registers and initializes nothing, while the core should not even register.
# What decides is "does this directory call xhook.BeforeStart", not the directory's name and not
# whether it has a go.mod: xlog has zero dependencies and stays in the core module while being an
# integration all the same.
# There are two registration entry points: a hook registers into the framework and a dialect into
# xgorm. Both only "write it down" and initialize nothing, so an init() is the right place for them.
# xhook itself has to be excluded: its package comment holds a usage example that this grep would
# take for an integration
integrations=$(grep -lE 'xhook\.BeforeStart\(|xgorm\.RegisterDialect\(' $gosrc | xargs -r -n1 dirname | sort -u | grep -vx 'xhook')
for f in $(grep -l '^func init()' $gosrc); do
  d=$(dirname "$f")
  echo "$integrations" | grep -qx "$d" || fail "$f has an init() while not being an integration (no xhook.BeforeStart)"
done
echo "✓ init() appears in the integrations alone"

# ---- 5. The upper bound on the core's public API ----
# A framework's API is permanent. Adding one has a price, and going over means cutting before
# adding.
# api <package>: counts the exported funcs and types. A failed go doc has to be reported (otherwise
# counting 0 would pass); counting 0 has grep -c exit 1, which under set -e would have a=$(...) exit
# the script without a word, hence the || true there
api() {
  doc=$(go doc -all "$1") || return 1
  echo "$doc" | grep -cE '^(func|type) ' || true
}
a=$(api .)
[ "$a" -le 15 ] || fail "the root package has $a public APIs, above the limit of 15"

# xhook / xconfig are the two packages every integration has to know, so their limits matter most:
# every export added is one more rule every third-party integration has to understand.
# This count once climbed to 15 over a few refactors with nobody stopping it, which is why it is
# pinned down now.
r=$(api ./xhook)
[ "$r" -le 6 ] || fail "xhook has $r public APIs, above the limit of 6 -- work out whether one can be cut before adding"
x=$(api ./xconfig)
[ "$x" -le 6 ] || fail "xconfig has $x public APIs, above the limit of 6"
# xonetest is for a caller's tests and is a permanent API just the same: swapping a configuration
# and running the hooks, and that is all.
# Anything adding "let me do one more thing for you" starts with the question of whether that thing
# belongs to xone.Run or to the package under test
tt=$(api ./xonetest)
[ "$tt" -le 3 ] || fail "xonetest has $tt public APIs, above the limit of 3"
# xtls is the TLS block the client integrations share: one configuration type plus two methods, one
# validating and one assembling a *tls.Config.
# It appears inside a caller's configuration structs (xredis.ClientConfig.TLS) and is a permanent API
# just the same.
# The server side (xgin's and xecho's TLS block, with ClientCAFile / MinVersion) has a different
# shape, lives in internal/web, and is not added here
tl=$(api ./xtls)
[ "$tl" -le 3 ] || fail "xtls has $tl public APIs, above the limit of 3"
echo "✓ public APIs: the root package ${a} (limit 15), xhook ${r} (limit 6), xconfig ${x} (limit 6), xonetest ${tt} (limit 3), xtls ${tl} (limit 3)"

# ---- 6. An integration must export the pure constructor New and must not reference the root
# package ----
# New keeps "zero wiring" the default path rather than the only one: a test, or an unusual case, can
# always go around the framework and build an instance directly.
# Not referencing the root package keeps the dependencies one-way -- an integration knows the
# registry, the framework knows the registry, and the two do not know each other.
[ -n "$integrations" ] || fail "not one integration was found, so step 4's criterion is broken"
for d in $integrations; do
  # A driver package imports xgorm rather than the framework, so this does not apply to it
  grep -rq 'xhook\.BeforeStart(' "$d"/*.go || continue
  ! grep -rq '"github.com/jupiter-eng/xone"' "$d"/*.go || fail "$d references the root package (xhook / xconfig alone may be imported)"
  # A package that only reads configuration and builds nothing (xapp, xflow) has no constructor to
  # speak of, so this is empty for it
  grep -rq 'xhook\.BeforeStop(' "$d"/*.go || continue
  # xcron is the exception: what it registers is the one scheduler of the process and there is no
  # "one instance" to build; its tests call the scheduler inside the package directly
  [ "$d" != xcron ] || continue
  # New[T any]( counts as well: a generic constructor is just as much an entry point for "build one
  # directly, around the framework"
  grep -rqE '^func New[(\[]' "$d"/*.go || fail "$d registers a component while having no pure constructor New"
done
echo "✓ the integrations pass ($(echo "$integrations" | wc -w) of them)"

# ---- 6.1 The examples use only what a caller can use ----
# example/'s module path sits under github.com/jupiter-eng/xone, so Go's internal rule lets it in --
# while a caller's code does not get in. An example using internal/config or internal/hook would not
# compile once a caller copied it. Swapping a configuration and running the hooks in a test is what
# xonetest is for
bad=$(files 'example/*.go' | xargs grep -ln '"github.com/jupiter-eng/xone/internal/' || true)
[ -z "$bad" ] || fail "example/ must not import internal/ (a caller cannot import it); use xonetest instead:
$bad"
echo "✓ example/ does not import internal/"

# ---- 7. Every configuration field is documented ----
# The configuration is the only interface a caller has, and a field added without documentation is
# the same as a field not added.
# The other way round, one key too many in the documentation has somebody configure something for a
# while before finding it has no effect.
#
# example/ does not count: the configuration there belongs to the examples and is written in their
# own comments and application.yml, and moving it into the framework's configuration manual would
# only make it look like part of the framework. e2e/ likewise: the business configuration block of
# the service under test belongs to the tests and is written in e2e/service/application.yml.
#
# It is checked per section and in both directions (see internal/schemagen/docs.go): every field has
# to appear in the "## Configuration" section of its own module's README (xlog/README.md and the
# like), and every section's YAML examples have to pass the schema.
# It used to grep the whole document for the field name: Name, Timeout and Enable are always found
# in some other section and always passed; [A-Za-z]+ also missed a name with a digit such as UseH2C;
# and the other direction was never checked at all.
#
# Checking per section rests on the schema knowing every field, so every yaml tag is confirmed to be
# in the schema first -- otherwise a field of a struct that was never attached to a Config would
# escape both the schema and this check
missing=""
for f in $(echo "$gosrc" | grep -vE '^(example|e2e)/' | xargs grep -hoE 'yaml:"[A-Za-z0-9]+"' | sed 's/yaml:"//; s/"//' | sort -u); do
  grep -q "\"$f\": {" config_schema.json || missing="$missing $f"
done
[ -z "$missing" ] || fail "these configuration fields are not in config_schema.json (a struct never attached to a Config?):$missing"
# schemagen is a tool module of its own, called from the root through go.work
out=$(go test -count=1 -run TestCheckDocs ./internal/schemagen 2>&1) || fail "a module README's \"## Configuration\" does not agree with the Config structs:
$out"
echo "✓ every configuration field is documented (per section, both directions)"

# ---- 8. The strings that reach run time have to be English ----
# The whole repository is English (step 8.1), while this check is the stricter one and has no
# allowlist: an error or a log lands in a caller's systems -- in their alerts, their log searches
# and their issues. A Chinese log field name would also become a JSON key and put the logging
# platform's indexes and dashboards out of step outright.
# internal/schemagen is a build tool whose output goes to whoever is changing this code, so it does
# not apply.
zh=$(GOFILES=$(files '*.go') python3 scripts/check_zh.py)
[ -z "$zh" ] || fail "the strings that reach run time are still Chinese here (an error and a log have to be English):
$zh"
echo "✓ the strings that reach run time are English"

# ---- 8.1 No Chinese anywhere in the repository ----
# The rule is in the Language section of .claude/CLAUDE.md. Everything predating it has been
# translated and the allowlist that carried that migration is gone: a file holding Chinese fails
# here, with nothing to add it to. Text an assertion needs verbatim goes in as \u escapes
# (e2e/gin_options_test.go)
python3 scripts/check_no_zh.py || fail "Chinese appeared in the repository (see the list above)"
echo "✓ no Chinese in the repository"

# ---- 9. config_schema.json agrees with the structs ----
# The schema is for the editor: completing a field in YAML, flagging a misspelling, showing the
# description on hover.
# It is generated rather than written by hand -- hundreds of fields kept in step by hand sooner or
# later disagree, and a disagreement shows up as completing a field that does not exist at all,
# which is worse than having no schema.
go run ./internal/schemagen -check || fail "config_schema.json does not agree with the Config structs; run: go run ./internal/schemagen"
echo "✓ config_schema.json agrees with the structs"

# ---- 10. Every error goes through xerror ----
# An error a module returns outwards carries the module name and the operation name, so a caller can
# ask "who reported this" rather than match a string prefix in the error message. Two ways of
# falling back to the old spelling are caught here:
#   1. fmt.Errorf("xgorm: ...") -- the prefix is written into the message, which is no structure at
#      all
#   2. xerror.Newf(..., "...err=[%v]", err) -- %v turns the error into text, errors.Is / errors.As
#      stop there, and a caller can never tell the root cause again
bad=$(grep -nE 'fmt\.Errorf\("x[a-z]+: ' $gosrc || true)
[ -z "$bad" ] || fail "these places still use an fmt.Errorf with a module prefix and have to become xerror:
$bad"

bad=$(grep -n 'xerror.Newf' $gosrc | grep 'err=\[%v\]' || true)
[ -z "$bad" ] || fail "these xerrors wrap the underlying error with %v, which breaks the error chain; use %w:
$bad"
echo "✓ every error goes through xerror (%w keeps the error chain)"

# ---- 11. No name that was removed still appears in the documentation or the examples ----
# A caller copies from the README and from example/. After a few refactors those still held names
# such as Component.Init and registry.Register that had long stopped existing, with comments
# teaching a spelling that had been withdrawn -- worse than no documentation, because it looks
# right. A public name being removed is added to this table.
#
# docs/CHANGELOG.md is not scanned, and not for any entry of the table: it is a historical record,
# and by convention every removed public name has "how to migrate from the old spelling" written
# there, which means naming it -- every entry of the table appears there sooner or later, and that is
# not a leftover.
# Every other document is checked as usual
gone='xutil\.(GetOrDefault|ToPtr)\b|xredis\.TLSConfig|Component\.Init|registry\.(Declare|Provide|Register)|ProvideEach|xconfig\.(HasKey|DecodeClients)\b|\.OnInit\(|xgin\.With(Config|Log|Trace|Metric|MetricPath|SkipPaths|RequestBodyLog|ResponseBodyLog|ZHTranslations)\('
hits=$(files '*.go' '*.md' '*.yml' ':(exclude)docs/CHANGELOG.md' | xargs grep -nE "$gone" || true)
[ -z "$hits" ] || fail "a name that was removed is still mentioned (fix the documentation, or confirm it really is still there):
$hits"
echo "✓ no removed name appears in the documentation or the examples"

# ---- 12. The mutation patterns still match the code ----
# A full mutation run takes minutes and is not run here; but "the pattern matches exactly N places"
# is a string comparison alone and takes under a second. Code that moved with the mutation left
# behind goes red here on the spot, rather than waiting for somebody to remember a full run
python3 scripts/mutate.py --dry-run || fail "a mutation pattern no longer matches the code; fix the matching line under scripts/mutations/"

# ---- 12.2 release.sh recognizes the hook logs ----
# --smoke / --verify rest on hooks() (scripts/lib.sh) recognizing each hook's start and stop in the
# log and then comparing whether the order is reversed.
# Once xlog added hostname and pid between msg and hook, a sed accepting only adjacent ones
# recognized not a single hook and went red in the --verify after the tags had been pushed. It is
# tested against a fixed sample log: whatever the log format becomes, what goes red first is here
t=$(mktemp)
cat > "$t" <<'LOG_EOF'
2026/01/02 15:04:05 INFO starting hook=xlog.initXLog stage=0
2026/01/02 15:04:05 INFO starting hook=xlog.watchLevel stage=0
{"time":"2026-01-02T15:04:05Z","level":"INFO","msg":"starting","hook":"xtrace.initXTrace","stage":10}
{"time":"2026-01-02T15:04:05Z","level":"INFO","msg":"starting","hostname":"box-1","pid":42,"hook":"xgin.initXGin","stage":30}
{"time":"2026-01-02T15:04:06Z","level":"INFO","msg":"shutdown signal received, closing gracefully","hostname":"box-1","pid":42}
{"time":"2026-01-02T15:04:06Z","level":"INFO","msg":"stopping","hostname":"box-1","pid":42,"hook":"xgin.closeXGin"}
{"time":"2026-01-02T15:04:06Z","level":"INFO","msg":"stopping","hook":"xtrace.closeXTrace"}
{"time":"2026-01-02T15:04:06Z","level":"INFO","msg":"stopping","hostname":"box-1","pid":42}{"hook":"xredis.notThisLine"}
2026/01/02 15:04:06 INFO stopping hook=xlog.closeXLog
LOG_EOF
got=$(hooks starting "$t" | tr '\n' ' ')
[ "$got" = "xlog xtrace xgin " ] || { rm -f "$t"; fail "hooks() recognized the start hooks wrong: want=[xlog xtrace xgin ] got=[$got] (did the log format change? update hooks in scripts/lib.sh)"; }
got=$(hooks stopping "$t" | tr '\n' ' ')
[ "$got" = "xgin xtrace xlog " ] || { rm -f "$t"; fail "hooks() recognized the stop hooks wrong: want=[xgin xtrace xlog ] got=[$got] (did the log format change? update hooks in scripts/lib.sh)"; }
rm -f "$t"
echo "✓ release.sh recognizes the hook logs (xlog JSON with extra fields, with adjacent fields, and the text format from before xlog)"

# ---- 12.1 Test function names are English ----
# A test name appears in go test -run, in CI's list of failures and in the mutation tables' filters,
# and English is what makes it searchable and copyable.
# perl rather than grep -P: macOS's BSD grep has no -P, the error is swallowed by the || true, and
# this check would pass there forever
zhtest=$(files '*_test.go' | xargs perl -ne 'print "$ARGV:$.:$_" if /^func (Test|Benchmark|Fuzz|Example)\w*[^\x00-\x7F]/; close ARGV if eof') \
  || fail "scanning the test function names failed"
[ -z "$zhtest" ] || fail "a test function name has to be English (describing the scenario, as TestNew_FailsFastOnTypo does):
$zhtest"
echo "✓ the test function names are English"

# ---- 13. Basic hygiene ----
unformatted=$(files '*.go' | xargs gofmt -l)
[ -z "$unformatted" ] || fail "these files are not formatted: $unformatted"
# It says nothing when it passes, and carries vet's own words out when it does not: "did not pass"
# alone would mean running it again to find out which line
for m in $modules; do
  out=$(cd "$m" && GOWORK=off go vet ./... 2>&1) || fail "$m did not pass go vet:
$out"
done
echo "✓ gofmt / go vet ($(echo "$modules" | wc -w) modules)"
