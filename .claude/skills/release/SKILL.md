---
name: release
description: 准备 xone 的一次发版（vX.Y.Z）：钉版本号、整理 CHANGELOG、开 release PR，到此为止，打 tag 由用户在 GitHub Actions 的 release 按钮上做。用户说「准备发版」「开 release PR」，或者这一版要人工审阅 CHANGELOG、有不兼容变更时用。要全自动一路发到 tag 推出去、不用自己点按钮的，用 `ship` skill。
---

# 准备发版

xone 的发版分两步：

1. **release PR**（这个 skill 做）：把各模块 go.mod 里仓库内的 require 钉成新版本、CHANGELOG 的「未发布」改成这一版、README 安装命令的版本号换掉，开 PR。
2. **打 tag**（用户做）：PR 合进 main 之后，用户在 GitHub 上点 Actions → release → Run workflow，填版本号。
   按钮在 main 的最新提交上给每个模块打 tag、跑 e2e、只推 tag、再验证装得上。

**不要**自己跑 `scripts/release.sh … --tag`、不要推 tag、不要直接推 main、不要删或挪已有的 tag：
tag 推出去就被 Go 的 module proxy 永久缓存，删不掉。这些都留给按钮。

背景（为什么这样分）见 `docs/development.md` 的「release.sh」一节。

## 步骤

### 1. 定版本号

- 用户给了版本号就用它；没给就看 CHANGELOG 的「未发布」一节和上一个 tag 以来的提交，给一个建议、让用户确认：
  有新功能 → 升次版本号（v1.0.0 → v1.1.0）；只有修复 → 升修订号（v1.0.0 → v1.0.1）。
  **不兼容变更不能在 v1 里发**：要改模块路径为 `/v2`。遇到就停下来和用户商量，不要自己决定。
- 只收 `v0.*.*` / `v1.*.*`（模块路径没有 `/v2` 后缀）。
- 上一个版本：`git fetch --tags origin && git tag -l 'v*' --sort=-v:refname | head -1`（只看根模块那种不带前缀的 tag）。
- 这个版本的 tag 已经存在（`git rev-parse -q --verify refs/tags/vX.Y.Z`）就停下来告诉用户，换一个。

### 2. 从最新的 main 开分支

```bash
git fetch origin
git switch main && git pull --ff-only origin main
git status --porcelain          # 必须为空；不为空就停下来问用户
git switch -c release/vX.Y.Z
```

### 3. 检查 CHANGELOG 的「未发布」

`docs/CHANGELOG.md` 的 `## [未发布]` 下面应该已经写着这一版使用者看得见的变化（CLAUDE.md 要求改动时同一个提交里写）。

- **有内容**：通读一遍，确认按「不兼容变更 / 新增 / 修复 / 性能」归类、每条一句话、不兼容变更写了怎么迁移。
- **是空的**：用 `git log <上一个 tag>..HEAD --oneline` 找使用者看得见的变化，起草条目写进「未发布」，
  **先给用户看、确认之后再继续**。仓库内部的重构、测试、工具改动不写。不确定的不要编。

### 4. 钉版本号

```bash
scripts/release.sh vX.Y.Z --bump
```

它把所有模块（包括不发布的 example / e2e / internal/schemagen）go.mod 里仓库内的 require 改成 vX.Y.Z，
replace 留着；把「未发布」改成 `## [vX.Y.Z] - 今天`、上面再开一个空的「未发布」；README 里 `go get …@vX.Y.Z` 的版本号换掉；最后跑 `scripts/check.sh`。只改文件，不提交。

它报错就停下来，把输出给用户看，不要绕过。

### 5. 核对改动，跑测试

```bash
git diff --stat                 # 只该有各模块的 go.mod、docs/CHANGELOG.md 和 README.md（安装命令的版本号）
git diff -- '*/go.mod'          # 只该是 github.com/jupiter-eng/xone… 的版本号变成 vX.Y.Z
scripts/test.sh -count=1
```

有别的文件被改、或者测试红了，停下来查，不要提交。e2e 不用在本地跑：按钮打 tag 之前会跑一遍。

### 6. 提交、推分支、开 PR

```bash
git commit -am "release: vX.Y.Z"      # 提交信息按仓库惯例写英文，结尾带上会话要求的署名
git push -u origin release/vX.Y.Z
```

开 PR：base `main`，head `release/vX.Y.Z`，标题 `release: vX.Y.Z`，正文贴 CHANGELOG 里 `## [vX.Y.Z]` 那一节，
末尾加一段「合并之后：Actions → release → Run workflow，版本号填 vX.Y.Z」。
有 GitHub 工具就直接开；没有就把分支链接和标题、正文给用户，让用户在网页上开。

### 7. 告诉用户剩下的两件事

1. CI 绿了之后合并这个 PR（合并方式随意）。
2. 在 GitHub 上 Actions → **release** → Run workflow，分支选 `main`，版本号填 `vX.Y.Z`。

## 按钮报错时

| 报错 | 原因 | 怎么办 |
|---|---|---|
| `这个提交的 go.mod 还没钉到 vX.Y.Z` | release PR 还没合进 main，或者按钮填的版本号和 PR 里的不一样 | 先合并 PR；版本号填对 |
| `docs/CHANGELOG.md 里没有 ## [vX.Y.Z] 这一节` | 同上，或者 PR 里手改掉了那一节 | 同上 |
| `tag vX.Y.Z 已经存在` | 这个版本发过了 | 换一个版本号，重新走一遍这个 skill |
| `工作区有未提交的改动` | 流水线某一步往仓库里写了文件 | 看是哪个文件，改 workflow 把它写到 `$RUNNER_TEMP` |
| 推送 tag 时 403 / `refusing to allow` | 仓库的 Workflow permissions 不是 Read and write，或者有针对 tag 的 ruleset | Settings → Actions → General 改权限；Rules → Rulesets 给按钮放行 |
| e2e 或 `--verify` 红了 | 真实的问题 | tag 没推（e2e 红）或者已经推了（verify 红）：后者说明发出去的版本装不上，修好之后发下一个修订号 |
