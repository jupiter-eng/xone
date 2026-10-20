# xone project conventions

xone is a Go framework that integrates third-party libraries: one place to read configuration,
staged initialization, reverse-order shutdown, and the caller gets the **native client**
(`*gorm.DB`, `*redis.Client`, `*gin.Engine`). **The reasoning and the tables are in
`docs/development.md`**; this file lists only what to follow.

## Language

- **English only. The repository must contain no Chinese**: code, comments, README and docs, test
  and benchmark names, failure messages, scripts, YAML, commit messages and PR descriptions.
- Error / panic messages, log messages and log field names are English for the reason they always
  were: they land in the caller's logging platform and alerts.
- Test and benchmark names describe the scenario (`TestNew_FailsFastOnTypo`).
- Examples in docs and comments use neutral values (`Europe/Berlin`, `prod,eu`), and never describe
  a "previous version / behavior change" that has not been released.
- **Migration in progress.** Files written before this rule still contain Chinese. Never add new
  Chinese, and translate what you touch: editing a function means translating its comments in the
  same change. Generated files (`config_schema.json`) follow their source and are regenerated, not
  hand-edited.

## Writing code

- No premature optimization: only act when a benchmark measured it and the result is no more complex
  than before; a cache layer, an object pool or `unsafe` needs an explanation of why nothing else works.
- Errors crossing a module boundary are always `*xerror.Error`:
  `xerror.Newf("xgorm", "connect", "cannot reach %s: %w", addr, err)`.
  - Wrap the underlying error with `%w`, never `%v`; wrap once per module boundary and keep
    intermediate errors plain; do not repeat the module name in the message.
  - `op` comes from this set only: `config` `new` `connect` `close` `register` `start` `stop` `execute`.
- Adopting or upgrading a library: measure every behavior sentence that goes into the docs with
  code first; confirm the default of every field left unset one by one; write the numbers together
  with the dependency version into the comments and that module's README under `## Behavior (measured)`.
- Design principles (in full in `docs/architecture.md`):
  - Keep the caller-facing interface minimal and push complexity into the framework — ask first what
    the caller would have to learn.
  - `init()` only registers, it never initializes; the framework runs the real initialization by stage.
  - Every integration with a third-party dependency is its own Go module; only dependency-free ones
    stay in the core.
  - Every integration exports a pure constructor `New`: it touches no globals and reads no files;
    only a blocking one takes a `ctx`.
  - The only entry points into the framework are `xhook.BeforeStart` / `BeforeStop`: business code
    writes no stage (`StageBusiness`), client-style integrations write `xhook.At(xhook.StageClient)`;
    a stop hook writes no stage, it inherits the start hook it pairs with. An integration imports
    `xhook` / `xconfig` only, never the root package.
- Exit signals: the signal is taken over before the configuration is read; hooks receive a `ctx` and
  the framework rechecks between hooks; after the first signal the default disposition is restored.
  All three are required.

## Configuration and docs

- Defaults are pre-filled in the struct, an unknown field is an error, an unset `${VAR}` is an error,
  and validation lives in `Validate()`.
- Adding or changing a Config field (including its comment): run `go run ./internal/schemagen`, then
  write it into that module's README under `## Configuration` (`check.sh` verifies both directions).
- Each module's documentation is the `README.md` in its own directory: `## Configuration` holds the
  reference only, measured numbers go under `## Behavior (measured)`, log / metric / span names under
  `## Observability`, and new error texts under `## Troubleshooting`. `docs/` holds only what spans
  modules: config loading rules (`config.md`), the summary table (`behavior.md`), global conventions
  (`observability.md`), common errors (`troubleshooting.md`). After moving a heading, grep for
  `README.md#` / `docs/` references (Go comments have them too).
- When removing an exported name, add it to the `gone` table in `scripts/check.sh`.
- The first ```go block in the root README is compiled by `example/readme_test.go`; update it when
  the public API changes.
- Changelog: nothing is recorded before the first version tag; after that, every caller-visible
  change goes into the `Unreleased` section of `docs/CHANGELOG.md` in the same commit.

## Mutation testing

- After changing safety- or lifecycle-related code, run `scripts/mutate.py --only <module>` once; a
  surviving mutant is a promise with no teeth.
- Mutation tables live in `scripts/mutations/<module>.py` (`core.py` for the root module), one
  `mutate(name, file, dir, test filter, edits...)` per line; edits use `swap()` / `cut()` only, never
  a bare `s.replace()`.
- When a refactor moves code, move its mutations with it (`--dry-run` reports patterns that no longer
  match); mutate the call site, not only the function being called.

## Common commands

```bash
scripts/test.sh                      # full test run (per module, GOWORK=off, with -race)
scripts/check.sh                     # architecture + dependency boundaries + docs + gofmt/vet (includes mutate.py --dry-run)
scripts/mutate.py [--only X] [-k X] [-j N] [--dry-run]   # mutation testing, runs through an overlay, no clean worktree required
scripts/e2e.sh [--load] [-run X]     # tests against real services: brings up PG / MySQL / Redis / ClickHouse; in CI it is .github/workflows/e2e.yml
docker compose -f e2e/compose.yml up -d --wait   # when those services are not installed locally
scripts/release.sh vX.Y.Z --bump     # release step 1: pin the version, update CHANGELOG and the install version in README, merge into main via PR (follow .claude/skills/release)
scripts/release.sh vX.Y.Z --tag      # step 2: tag on main (no push); day to day, use the release button in GitHub Actions
go test -run=NONE -bench=. -benchtime=100000x ./xlog/ ./xflow/ ./xgin/middleware/   # run before and after touching hot code
```
