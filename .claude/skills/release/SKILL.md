---
name: release
description: Prepare a xone release (vX.Y.Z): pin the version, tidy up the CHANGELOG, open the release PR, and stop there -- the tagging is done by the user on the release button in GitHub Actions. Use it when the user says "prepare a release" or "open the release PR", or when this version needs the CHANGELOG reviewed by hand or carries an incompatible change. For a fully automatic run all the way to the tags being pushed, with no button to press, use the `ship` skill.
---

# Preparing a release

A xone release has two steps:

1. **The release PR** (what this skill does): pin the in-repository requires of each module's go.mod
   to the new version, turn the CHANGELOG's Unreleased section into this version's, replace the
   version in the README's install command, and open the PR.
2. **Tagging** (what the user does): once the PR is merged into main, the user presses Actions →
   release → Run workflow on GitHub and fills in the version.
   The button tags every module on main's latest commit, runs e2e, pushes the tags alone, and then
   verifies that it installs.

**Do not** run `scripts/release.sh … --tag` yourself, do not push tags, do not push to main
directly, and do not delete or move an existing tag: once a tag is pushed it is cached forever by
Go's module proxy and cannot be removed. All of that is left to the button.

The background (why it is split this way) is in the "release.sh" section of `docs/development.md`.

## Steps

### 1. Decide the version

- Use the version the user gave; without one, look at the CHANGELOG's Unreleased section and the
  commits since the last tag, suggest one and have the user confirm it: a new feature → raise the
  minor (v1.0.0 → v1.1.0); fixes only → raise the patch (v1.0.0 → v1.0.1).
  **An incompatible change cannot go out in v1**: the module path has to become `/v2`. Stop and
  discuss it with the user rather than deciding alone.
- Only `v0.*.*` / `v1.*.*` are accepted (the module path has no `/vN` suffix).
- The previous version: `git fetch --tags origin && git tag -l 'v*' --sort=-v:refname | head -1`
  (looking only at the unprefixed tags of the root module).
- If this version's tag already exists (`git rev-parse -q --verify refs/tags/vX.Y.Z`), stop and tell
  the user to pick another one.

### 2. Branch off the latest main

```bash
git fetch origin
git switch main && git pull --ff-only origin main
git status --porcelain          # must be empty; if it is not, stop and ask the user
git switch -c release/vX.Y.Z
```

### 3. Check the CHANGELOG's Unreleased section

The `## [Unreleased]` section of `docs/CHANGELOG.md` should already hold this version's
caller-visible changes (CLAUDE.md requires them to be written in the same commit as the change).

- **It has content**: read it through and confirm that it is grouped into incompatible changes /
  added / fixed / performance, that each entry is one sentence, and that every incompatible change
  says how to migrate.
- **It is empty**: find the caller-visible changes with `git log <the previous tag>..HEAD --oneline`,
  draft the entries into the Unreleased section, and **show them to the user and wait for
  confirmation before going on**. Refactoring, tests and tooling inside the repository are left out.
  Do not invent anything you are unsure about.

### 4. Pin the version

```bash
scripts/release.sh vX.Y.Z --bump
```

It changes the in-repository requires of every module's go.mod (the unreleased example / e2e /
internal/schemagen included) to vX.Y.Z while keeping the replaces; turns the Unreleased section into
`## [vX.Y.Z] - <today>` with a fresh empty Unreleased section above it; replaces the version in the
README's `go get …@vX.Y.Z`; and finally runs `scripts/check.sh`. It only changes files and commits
nothing.

If it reports an error, stop, show the user the output, and do not go around it.

### 5. Go over the changes and run the tests

```bash
git diff --stat                 # only the modules' go.mod, docs/CHANGELOG.md and README.md (the version in the install command)
git diff -- '*/go.mod'          # only the versions of github.com/jupiter-eng/xone… becoming vX.Y.Z
scripts/test.sh -count=1
```

If another file was changed, or the tests went red, stop and look into it rather than committing.
e2e does not have to be run locally: the button runs it before tagging.

### 6. Commit, push the branch, open the PR

```bash
git commit -am "release: vX.Y.Z"      # the commit message follows the repository's conventions, ending with the attribution this session asks for
git push -u origin release/vX.Y.Z
```

Open the PR: base `main`, head `release/vX.Y.Z`, title `release: vX.Y.Z`, body the `## [vX.Y.Z]`
section of the CHANGELOG, with a closing paragraph saying "after merging: Actions → release → Run
workflow, with vX.Y.Z as the version".
Open it directly if a GitHub tool is available; otherwise hand the user the branch link, the title
and the body to open it in the browser.

### 7. Tell the user about the two things left

1. Merge this PR once CI is green (any merge method).
2. On GitHub, Actions → **release** → Run workflow, with `main` as the branch and `vX.Y.Z` as the
   version.

## When the button reports an error

| What it reports | Cause | What to do |
|---|---|---|
| this commit's go.mod is not pinned to vX.Y.Z yet | the release PR is not merged into main yet, or the version filled into the button differs from the PR's | merge the PR first; fill in the right version |
| `docs/CHANGELOG.md` has no `## [vX.Y.Z]` section | the same, or that section was edited by hand in the PR | the same |
| these tags already exist | this version has been released | pick another version and go through this skill again |
| the working tree has uncommitted changes | some step of the pipeline wrote a file into the repository | find out which file and change the workflow to write it into `$RUNNER_TEMP` |
| 403 / `refusing to allow` while pushing the tags | the repository's Workflow permissions are not Read and write, or a ruleset covers tags | Settings → Actions → General for the permissions; Rules → Rulesets to let the button through |
| e2e or `--verify` went red | a real problem | the tags were not pushed (e2e red) or were already pushed (verify red): the latter means the released version cannot be installed, so fix it and release the next patch |
