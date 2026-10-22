---
name: ship
description: Release end to end (vX.Y.Z): pin the version, open the PR, wait for CI, merge, trigger the tagging, and confirm it installs, without stopping along the way. Use it when the user says "release it end to end", "ship v1.1.0" or "release it automatically". For stopping at the PR and pressing the tagging button yourself, use the `release` skill.
---

# Releasing end to end

From deciding the version all the way to the tags being pushed and an external project installing
it, without stopping to ask along the way.

**Once a tag is pushed it is cached forever by Go's module proxy and sum.golang.org, and can be
neither removed nor changed.** "Pushing the tags" is the one irreversible step of the whole flow,
and it runs inside GitHub Actions, out of your hands -- so **the checks in stage 0 are the only
safety net there is, and not one of them may be skipped**. If a check does not pass, stop and tell
the user rather than going around it.

Splitting this into two skills is deliberate: when the CHANGELOG needs reviewing by hand, or this
version carries an incompatible change, use `release` (which stops at the PR).

## Stage 0: checks before starting

Every one of them has to pass before going on. If any does not, stop and show the user the output
verbatim.

```bash
cd "$(git rev-parse --show-toplevel)"
V=vX.Y.Z                                    # the version the user gave

# 1. The version format: v0 / v1 only (the module path has no /vN suffix)
echo "$V" | grep -qE '^v[01]\.[0-9]+\.[0-9]+$' || echo "✗ only v0 / v1 are accepted"

# 2. A clean working tree, on main, in sync with the remote
git fetch -q origin --tags
[ "$(git branch --show-current)" = main ] && [ -z "$(git status --porcelain)" ] \
  && [ "$(git rev-parse main)" = "$(git rev-parse origin/main)" ]

# 3. Not one of this set of tags may exist already (checking locally and on the remote)
#    What this guards against: creating a Release on GitHub creates a tag of the same name along
#    with it. Once a version's tag has been through sum.golang.org that version is spent -- the
#    module zip includes .github/, so any later change makes the zip hash disagree with the sumdb's
#    record and the only way out is another version.
git ls-remote --tags origin | grep -E "refs/tags/($V|[a-z/]+/$V)$" && echo "✗ this set of tags already exists, pick another version"

# 4. The CHANGELOG's Unreleased section has entries (release.sh --bump refuses an empty one)
awk '/^## \[Unreleased\]/{s=1;next} s&&/^## /{exit} s&&/^- /{f=1} END{exit !f}' docs/CHANGELOG.md \
  || echo "✗ the CHANGELOG's Unreleased is empty; write this version's caller-visible changes first"

# 5. The credentials for pushing tags are in place: the org has disabled GITHUB_TOKEN's write
#    permission, and release.yml reads this secret
gh secret list | grep -q RELEASE_TOKEN || echo "✗ the repository secret RELEASE_TOKEN is not set and the workflow cannot push tags"

# 6. The release.yml on main really does use it (workflow_dispatch runs the copy on main)
git show origin/main:.github/workflows/release.yml | grep -q 'secrets.RELEASE_TOKEN' \
  || echo "✗ the release.yml on main still uses github.token and pushing tags will 403"
```

An incompatible change cannot go out in v1 (the module path has to become `/v2`). If you see one,
stop and discuss it with the user rather than deciding the version yourself.

## Stage 1: open the release PR

```bash
git switch -c "release/$V"
scripts/release.sh "$V" --bump          # pins the modules' go.mod, opens a new CHANGELOG section, replaces the README version, and runs check.sh at the end
git diff --stat                         # only the modules' go.mod, docs/CHANGELOG.md and README.md
scripts/test.sh -count=1                # EXIT=0 is required
git commit -am "release: $V"            # ending with the attribution this session asks for
git push -u origin "release/$V"
gh pr create --base main --head "release/$V" --title "release: $V" --body-file <the body>
```

The body is the `## [$V]` section of the CHANGELOG. If `--bump` or the tests report an error, stop
and show the user the output.

**Do not commit with `git commit -F - <<EOF` and a heredoc inside an `&&` chain** -- a heredoc
breaks the chain's short-circuiting, and the later steps run even after an earlier one has failed.
Write the message into a temporary file and use `-F <file>`.

## Stage 2: wait for the PR's checks

```bash
gh pr checks <PR number> --watch
```

This PR changes go.mod, so besides `check` / `core-go-floor` it also triggers **e2e** (`e2e.yml`
filters on `**/go.mod`).

**Do not report a failure the moment e2e goes red**: it holds a few timing assertions measured in
milliseconds (`TestMySQL_SIGTERMWhileSilentAtStartup_ExitsImmediately` requires an exit within 200ms
of the signal, for one), which occasionally overrun on a shared runner. What to do:

1. Look first at the numbers the test printed itself (`gh run view <id> --log-failed`; the cases log
   their measured numbers).
   Overrunning the threshold by a little (tens of milliseconds) is jitter; being off by an order of
   magnitude (stuck until ReadTimeout, say) is a real problem.
2. Jitter → re-run once with `gh run rerun <id> --failed`. **If it goes red again, stop** rather
   than re-running repeatedly to paper over the problem.
3. `check` / `core-go-floor` red → stop outright, that is a problem in the code.

## Stage 3: merge

```bash
gh pr merge <PR number> --merge
git fetch -q origin && git switch -q main && git pull --ff-only -q origin main
```

Once merged, confirm that the go.mod on main is pinned to `$V` and that the CHANGELOG has the
`## [$V]` section -- those two are the first things the next step, `release.sh --tag`, checks.

## Stage 4: trigger the tagging

```bash
gh workflow run release.yml --ref main -f version="$V"
sleep 5 && gh run list --workflow=release.yml --limit 1 --json databaseId --jq '.[0].databaseId'
gh run watch <run-id> --exit-status
```

The steps behind the button, in order: `release.sh --tag` (checks + unit tests + 13 tags created
locally) → e2e → `--smoke` → **pushing the tags** → `--verify` (`go get` in a brand new project).

The push is step 8 and e2e is step 6: **when e2e goes red the tags never leave the runner**, so
re-running is safe (judged as in stage 2). But a red `--verify` means the tags are already pushed
and the released version cannot be installed -- that one must not be re-run; stop, tell the user,
and release the next patch once it is fixed.

## Stage 5: confirm

```bash
git fetch -q origin --tags
git ls-remote --tags origin | sed 's#.*refs/tags/##' | sort        # 14 of them: the root + 13 submodules
for m in "" /xgin /xgorm /xredis /xkafka /xcron /xecho /xbeego /xcache /xhttp /xmetric /xtrace /xginswagger /xgorm/clickhouse; do
  curl -s -o /dev/null -w "$m %{http_code}\n" "https://proxy.golang.org/github.com/jupiter-eng/xone$m/@v/$V.info"
done                                                               # all have to be 200
```

Only the root tag with the submodules missing is "half a release" -- a caller following the README
will fail to install. If that really happens, stop and tell the user rather than adding the missing
tags by hand: a tag added that way went around e2e and smoke.

For a version with a `retract`, confirm along the way that it took effect: the retracted version
must not appear in `go list -m -versions github.com/jupiter-eng/xone` (it only shows with
`-retracted`).

Finally report these to the user: the PR link, the release run link, the 13 tags, the proxy status,
and the install command.

## How to recover from a failure

| Where it stopped | What is left on the remote | How to carry on |
|---|---|---|
| stage 0 | nothing at all | fix that one check and run again |
| stages 1 and 2 | one release PR | change it in the same PR, or close the PR, delete the branch and start over |
| after stage 3, before the tags are pushed | the PR is merged into main | the go.mod on main is already pinned, so trigger stage 4 again directly |
| the tags are pushed | **the tags and the proxy's records cannot be withdrawn** | this version is spent. `retract` it in the root `go.mod` and release the next patch |
