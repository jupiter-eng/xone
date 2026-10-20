---
name: ship
description: 一条龙发版（vX.Y.Z）：钉版本号、开 PR、等 CI、合并、触发打 tag、核对装得上，全程不停。用户说「一条龙发版」「发 v1.1.0 一条龙」「ship v1.1.0」「自动发版」时用。只想做到开 PR 为止、自己点按钮打 tag 的，用 `release` skill。
---

# 一条龙发版

从定版本号一路做到 tag 推出去、外部工程装得上，中途不停下来问。

**tag 推出去就被 Go 的 module proxy 和 sum.golang.org 永久缓存，删不掉、改不了。**
整条流程只有「推送 tag」这一步不可逆，而它在 GitHub Actions 里跑、不经过你的手——
所以**阶段 0 的检查就是唯一的安全网，一条都不能跳**。检查没过就停下来告诉用户，不要绕过。

分两个 skill 是有意的：需要人工审阅 CHANGELOG、或者这一版有不兼容变更时，用 `release`（只到开 PR）。

## 阶段 0：开工前检查

全部通过才往下走。任何一条不过，停下来把原话给用户看。

```bash
cd "$(git rev-parse --show-toplevel)"
V=vX.Y.Z                                    # 用户给的版本号

# 1. 版本号格式：只收 v0 / v1（模块路径没有 /vN 后缀）
echo "$V" | grep -qE '^v[01]\.[0-9]+\.[0-9]+$' || echo "✗ 版本号只收 v0 / v1"

# 2. 工作区干净、在 main 上、和远端同步
git fetch -q origin --tags
[ "$(git branch --show-current)" = main ] && [ -z "$(git status --porcelain)" ] \
  && [ "$(git rev-parse main)" = "$(git rev-parse origin/main)" ]

# 3. 这组 tag 一个都不能已经存在（本地和远端都查）
#    挡的是：在 GitHub 上建 Release 会顺带创建同名 tag。一旦某个版本号的 tag 进过
#    sum.golang.org，那个版本号就作废了——module zip 包含 .github/，所以任何后续
#    改动都会让 zip hash 和 sumdb 的记录对不上，只能换版本号。
git ls-remote --tags origin | grep -E "refs/tags/($V|[a-z/]+/$V)$" && echo "✗ 这组 tag 已经存在，换版本号"

# 4. CHANGELOG 的 Unreleased 一节有条目（release.sh --bump 会拒绝空的）
awk '/^## \[Unreleased\]/{s=1;next} s&&/^## /{exit} s&&/^- /{f=1} END{exit !f}' docs/CHANGELOG.md \
  || echo "✗ CHANGELOG 的 Unreleased 是空的，先写这一版使用者看得见的变化"

# 5. 推 tag 的凭证在：org 禁用了 GITHUB_TOKEN 的写权限，release.yml 读的是这个 secret
gh secret list | grep -q RELEASE_TOKEN || echo "✗ 仓库 secret RELEASE_TOKEN 没配，workflow 推不了 tag"

# 6. main 上的 release.yml 确实在用它（workflow_dispatch 跑的是 main 上的那一份）
git show origin/main:.github/workflows/release.yml | grep -q 'secrets.RELEASE_TOKEN' \
  || echo "✗ main 上的 release.yml 还在用 github.token，推 tag 会 403"
```

不兼容变更不能在 v1 里发（要改模块路径为 `/v2`）。看出来有，停下来和用户商量，不要自己决定版本号。

## 阶段 1：开 release PR

```bash
git switch -c "release/$V"
scripts/release.sh "$V" --bump          # 钉各模块 go.mod、CHANGELOG 开新节、README 版本号，末尾跑 check.sh
git diff --stat                         # 只该有各模块 go.mod、docs/CHANGELOG.md、README.md
scripts/test.sh -count=1                # 必须 EXIT=0
git commit -am "release: $V"            # 结尾带上会话要求的署名
git push -u origin "release/$V"
gh pr create --base main --head "release/$V" --title "release: $V" --body-file <正文>
```

正文贴 CHANGELOG 里 `## [$V]` 那一节。`--bump` 或测试报错就停下，把输出给用户看。

**commit 不要用 `git commit -F - <<EOF` 接 heredoc 串在 `&&` 链里**——heredoc 会让链的短路失效，
前面的步骤失败了后面照样跑。消息写进临时文件再 `-F 文件`。

## 阶段 2：等 PR 的检查

```bash
gh pr checks <PR号> --watch
```

这个 PR 改了 go.mod，所以除 `check` / `core-go-floor` 外还会触发 **e2e**（`e2e.yml` 按 `**/go.mod` 过滤）。

**e2e 红了先别急着报故障**：里面有几条按毫秒卡的时序断言（如
`TestMySQL_SIGTERMWhileSilentAtStartup_ExitsImmediately` 要求信号后 200ms 内退出），
在共享 runner 上会偶尔超。做法：

1. 先看测试自己打印的数字（`gh run view <id> --log-failed | grep 数字：`）。
   超出阈值不多（几十 ms）是抖动；差一个数量级（比如卡到 ReadTimeout）是真问题。
2. 抖动 → `gh run rerun <id> --failed` 重跑一次。**再红就停下**，不要反复重跑掩盖问题。
3. `check` / `core-go-floor` 红 → 直接停，那是代码问题。

## 阶段 3：合并

```bash
gh pr merge <PR号> --merge
git fetch -q origin && git switch -q main && git pull --ff-only -q origin main
```

合完确认 main 上的 go.mod 钉的是 `$V`、CHANGELOG 有 `## [$V]` 那一节——
这两条是下一步 `release.sh --tag` 开头就查的。

## 阶段 4：触发打 tag

```bash
gh workflow run release.yml --ref main -f version="$V"
sleep 5 && gh run list --workflow=release.yml --limit 1 --json databaseId --jq '.[0].databaseId'
gh run watch <run-id> --exit-status
```

按钮里的步骤依次是：`release.sh --tag`（检查 + 单测 + 本地打 13 个 tag）→ e2e → `--smoke`
→ **推送 tag** → `--verify`（全新工程里 `go get`）。

推送在第 8 步，e2e 在第 6 步：**e2e 红了 tag 不会离开 runner**，重跑安全（同阶段 2 的判断）。
但 `--verify` 红了说明 tag 已经推出去、而发出去的版本装不上——那时不能重跑，
要停下来告诉用户，修好之后发下一个修订号。

## 阶段 5：核对

```bash
git fetch -q origin --tags
git ls-remote --tags origin | sed 's#.*refs/tags/##' | sort        # 13 个：根 + 12 个子模块
for m in "" /xgin /xgorm /xredis /xkafka /xcron /xecho /xcache /xhttp /xmetric /xtrace /xginswagger /xgorm/clickhouse; do
  curl -s -o /dev/null -w "$m %{http_code}\n" "https://proxy.golang.org/github.com/jupiter-eng/xone$m/@v/$V.info"
done                                                               # 全要 200
```

只有根 tag、子模块缺着，是「半套」——使用者照 README 装会失败。真发生了就停下来告诉用户，
不要自己手动补 tag：补出来的 tag 绕过了 e2e 和 smoke。

有 `retract` 的版本顺带验一下生效：`go list -m -versions github.com/jupiter-eng/xone`
里不该出现被撤回的那个（加 `-retracted` 才看得到）。

最后把这些报给用户：PR 链接、release run 链接、13 个 tag、proxy 状态、安装命令。

## 失败了怎么收场

| 停在哪 | 远端留下了什么 | 怎么接着来 |
|---|---|---|
| 阶段 0 | 什么都没有 | 修好那一条，重跑 |
| 阶段 1、2 | 一个 release PR | 在同一个 PR 上改，或关掉 PR 删分支重来 |
| 阶段 3 之后、tag 推出去之前 | PR 已合进 main | main 上 go.mod 已经钉好，直接从阶段 4 重新触发 |
| tag 已推出去 | **tag 和 proxy 记录撤不回** | 这个版本号作废。在根 `go.mod` 里 `retract` 它，发下一个修订号 |
