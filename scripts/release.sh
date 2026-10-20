#!/bin/sh
# 发布。多模块仓库的每个 module 有自己的 tag，全都打在 main 上的同一个提交上。
#
# 分两步，中间是一个普通的 PR（main 的分支保护照常生效）：
#
#   scripts/release.sh v0.1.0 --bump     # 1. 把各模块 go.mod 里仓库内的 require 改成 v0.1.0、
#                                        #    turn the CHANGELOG's Unreleased section into this version, update the version in the README install command;
#                                        #    只改文件，不提交
#   （提交、开 PR、CI 绿了合进 main）
#   scripts/release.sh v0.1.0 --tag      # 2. 在 main 的最新提交上给每个模块打 tag（不推送）
#   scripts/release.sh v0.1.0 --smoke    # 推送之前：同一个外部工程，依赖指向本地代码，起得来、关得对
#   scripts/release.sh v0.1.0 --verify   # 推送之后：用一个全新的外部工程验证装得上、跑得起来
#
# 第 2 步平时用 GitHub Actions 的 release 按钮做（.github/workflows/release.yml）：
# 同样的检查，再 --smoke，跑完只推 tag、再 --verify。
#
# 为什么 main 上的 go.mod 能直接发：开发用的 replace 一直留着，
# 而使用者的构建会忽略依赖里的 replace——他们只看 require 的版本。
# 所以发布只需要把 require 钉成要发的版本，replace 不用去掉，也就不需要
# 单独的发布提交和还原提交，tag 直接打在 main 上。
#
# --tag 发布前要一轮绿的 e2e（真实的 PG / MySQL / Redis）：默认在这一步跑 scripts/e2e.sh；
# 这个提交在别处跑绿过，或者会在推送之前另跑一遍（release 按钮就是打完 tag 紧接着跑 e2e）的话，
# 加 --e2e-passed 跳过这一步的 e2e，由你担保。
#
# 推送是单独一步：Go 的 module proxy 会永久缓存 tag，推错了删不掉，只能再发一个版本盖过去。
set -e
cd "$(dirname "$0")/.."
. scripts/lib.sh

usage() { echo "用法：scripts/release.sh vX.Y.Z (--bump | --tag [--e2e-passed] | --smoke | --verify)"; exit 1; }
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
  # 只收 v0 / v1：模块路径里没有 /v2 后缀，go mod edit 会拒收 v2 及以上的版本号
  v0.*.*|v1.*.*) ;;
  v*.*.*) echo "模块路径没有 /vN 后缀，只能发 v0 / v1，got=$VERSION"; exit 1;;
  *) echo "版本号格式应为 vX.Y.Z，got=$VERSION"; exit 1;;
esac

MOD=github.com/jupiter-eng/xone

# 要发布的子模块：签入的每个子目录 go.mod，除了这几个（根模块另打不带前缀的 tag）：
#   example             示例不是库，replace 要一直留着，这样它永远编译的是
#                       仓库当前的代码，而不是某个已发布版本
#   e2e                 真实 Web 服务测试，理由同 example
#   internal/schemagen  仓库自己的构建工具，不给使用者 import
#
# 从前这里是一张手写的「发布顺序」表，可所有 tag 都打在同一个提交上、一次推送，
# 谁先谁后根本无所谓；手写的表倒是新加一个模块就得记得改
#
# 钉版本号（PINNED）要连不发布的 example / e2e / internal/schemagen 一起：它们 require 的子模块
# 钉成 $VERSION 之后又 require 核心的 ${VERSION}，自己的 go.mod 还写着 v0.0.0 的话，
# go 会说 go.mod 要更新，check.sh 的 vet 当场就红。tag 只打 $MODS
PINNED=$(git ls-files '*/go.mod' | xargs -n1 dirname)
MODS=$(echo "$PINNED" | grep -vxE 'example|e2e|internal/schemagen')

# ---- --smoke / --verify：一个全新的外部工程装得上、跑得起来 ----
# 两个模式跑的是同一个消费者工程、同一套检查（起服务 → 请求 → SIGTERM → 逆序关闭），
# 只有依赖从哪来不同：
#
#   --verify  必须在 git push --tags 之后跑，go get 的是 module proxy 上发布出去的版本。
#             CI 跑的是工作区里的代码，模块之间还靠 replace 互指，证明不了一个真正的
#             使用者 go get 之后会发生什么——require 的版本对不对、tag 打全了没有、
#             去掉 replace 还编不编得过，全都只有这一步能回答。
#   --smoke   推送之前跑，依赖指向本地这份工作区（就是刚打了 tag 的那个提交）：
#             消费者是主模块，它的 replace 说了算，各模块 go.mod 里互指的 replace 被忽略，
#             和使用者的构建一样。--verify 曾经在 tag 推出去之后才红
#             （认钩子的 sed 对不上新的日志字段），而 tag 推出去就撤不回了——
#             同样的检查先在这里跑一遍，真有回归的话 tag 还只在本地。
#             它验不了 tag 打没打全、proxy 拉不拉得到，那些仍然交给 --verify
consumer() {
# 和发布的是同一份子模块列表，新加的模块自动进来
PKGS=$(printf "$MOD/%s\n" $MODS)
ROOT=$(pwd)
DIR=$(mktemp -d)
trap 'rm -rf "$DIR"' EXIT

if [ "$MODE" = "--smoke" ]; then
  # 工作区有改动的话，跑的就不是打了 tag 的那个提交，说一声
  [ -z "$(git status --porcelain)" ] || echo "  ⚠ 工作区有未提交的改动，跑的是改过的代码，不是 $(git log -1 --format=%h)"
  echo "== 用本地的 $(git log -1 --format='%h %s') 充当 $VERSION =="
fi

echo "== 在 $DIR 里建一个干净的消费者工程 =="
cd "$DIR"
cat > go.mod <<MOD_EOF
module verify

go 1.25
MOD_EOF

# 一个真实使用者会写的样子：起服务、连库、打点，全部走公开 API。
#
# 下面 go get 的每个模块都必须在这里 import 到：go mod tidy 会把没人 import 的
# require 删掉，那个模块就从头到尾没被编译过——xcache 和 xginswagger 从前就是
# 这样，「装得上、编得过」对它们俩什么都没证明。所以 import 由同一份列表生成，
# 每个模块一行 _ import；xgin / xmetric 另有一行具名的，同一个包两种写法 Go 是允许的
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
  echo "== require 各模块的 ${VERSION}，replace 到 $ROOT =="
  # 不走 go get：这个版本还没推，proxy 上没有。require 照样写 ${VERSION}，
  # 使用者 go.mod 里会出现的就是这几行，只是内容从本地目录来
  for p in "$MOD" $PKGS; do
    echo "  $p@$VERSION => $ROOT${p#"$MOD"}"
    go mod edit -require="$p@$VERSION" -replace="$p=$ROOT${p#"$MOD"}"
  done
else
  echo "== go get 各模块的 $VERSION =="
  # 一条 go get 拿全部模块：逐个 go get 每次都要重新解一遍整张依赖图。
  # 实测空缓存（CI 上 verify 那台新机器的情形）下 12 个模块逐个 43s，一次 23s
  set --
  for p in "$MOD" $PKGS; do
    echo "  $p@$VERSION"
    set -- "$@" "$p@$VERSION"
  done
  GOFLAGS=-mod=mod go get "$@" >/dev/null
fi

echo "== 编译 =="
go mod tidy >/dev/null
go build -o verify .
echo "  ✓ 装得上、编得过"

echo "== 起一遍：启动 → 请求 → 退出 =="
./verify --config=/dev/null >out.txt 2>&1 &
pid=$!
# 走到这里时服务通常已经退出，kill 失败是正常的：不加 || true 的话，set -e 让这次失败
# 成了整个脚本的退出码——有一次发布就这样在「✓ 验证通过」之后报了 exit 1
trap 'kill -9 "$pid" 2>/dev/null || true; rm -rf "$DIR"' EXIT

i=0
until curl -sf http://127.0.0.1:8080/ping >/dev/null 2>&1; do
  i=$((i + 1))
  [ "$i" -lt 50 ] || { echo "✗ 服务没起来"; cat out.txt; exit 1; }
  sleep 0.2
done
echo "  ✓ /ping 通了"

kill -TERM "$pid"
i=0
while kill -0 "$pid" 2>/dev/null; do
  i=$((i + 1))
  [ "$i" -lt 100 ] || { echo "✗ 收到 SIGTERM 之后没退出"; cat out.txt; exit 1; }
  sleep 0.2
done
echo "  ✓ 收到 SIGTERM 之后干净退出"

# 从前这里 grep 的是 "closing"，命中的其实是收到信号那一行
# （"shutdown signal received, closing gracefully ..."），一个组件都没关也照样过。
# hooks 在 scripts/lib.sh 里，check.sh 拿样例日志测着它
hooks starting out.txt > started.txt
hooks stopping out.txt > stopped.txt
# 两边都有的包：停止顺序必须正好是启动顺序倒过来
awk '{ l[NR] = $0 } END { for (i = NR; i > 0; i--) print l[i] }' started.txt | grep -Fxf stopped.txt > want.txt || true
grep -Fxf started.txt stopped.txt > got.txt || true
[ "$(wc -l < got.txt)" -ge 2 ] || { echo "✗ 没看到组件关闭的日志"; cat out.txt; exit 1; }
cmp -s want.txt got.txt || { echo "✗ 关闭顺序不是启动顺序的逆序"; echo "  启动：$(tr '\n' ' ' < started.txt)"; echo "  关闭：$(tr '\n' ' ' < stopped.txt)"; exit 1; }
# 日志最先起、最后关，其余组件的关闭日志才写得出去
[ "$(tail -n 1 got.txt)" = "xlog" ] || { echo "✗ 最后关的不是 xlog：$(tr '\n' ' ' < got.txt)"; exit 1; }
echo "  ✓ 组件被逆序关闭：$(tr '\n' ' ' < got.txt)"
echo
if [ "$MODE" = "--smoke" ]; then
  echo "✓ $VERSION 冒烟通过（本地代码，还没推送）"
else
  echo "✓ $VERSION 验证通过"
fi
}
case "$MODE" in --smoke|--verify) consumer; exit 0;; esac

# inrepo go.mod：列出 require 里仓库内的模块（路径@版本）。
# 只看 require：从前从整份 -json 里 grep 路径，只出现在 replace 里的
# （xgin 替换了 xtrace 却不 require 它）也被补了一条 require，发出去的 go.mod 平白多一个依赖
inrepo() {
  GOWORK=off go mod edit -json "$1" | python3 -c '
import json, sys
mod = sys.argv[1]
for e in json.load(sys.stdin).get("Require") or []:
    p = e["Path"]
    if p == mod or p.startswith(mod + "/"):
        print(p + "@" + e["Version"])' "$MOD"
}

# unpinned：仓库内的 require 里还没钉成 $VERSION 的，一行一个「模块: 依赖@版本」
unpinned() {
  for m in $PINNED; do
    for dep in $(inrepo "$m/go.mod"); do
      [ "${dep##*@}" = "$VERSION" ] || echo "  $m: $dep"
    done
  done
}

# has_entries 标题前缀：docs/CHANGELOG.md 里以它开头的那一节（到下一个 "## " 为止）
# 有没有 "- " 开头的条目。### 小标题不算条目：只有「### 修复」没有内容的一节照样是空的
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

  echo "== 1. 把各模块 go.mod 里仓库内的 require 钉成 ${VERSION}（replace 留着） =="
  # 用 go mod edit 而不是 sed：require 块的缩进、子模块路径后缀这些细节
  # 手写正则很容易弄错，而弄错的后果是发出去一个装不上的版本。
  # 原来是 v0.0.0、上一个版本，还是 go 工具自己补的伪版本，都一样改写
  for m in $PINNED; do
    for dep in $(inrepo "$m/go.mod"); do
      GOWORK=off go mod edit -require="${dep%@*}@$VERSION" "$m/go.mod"
    done
  done
  left=$(unpinned)
  [ -z "$left" ] || { echo "✗ 还有没钉好的仓库内依赖："; echo "$left"; exit 1; }
  echo "  ✓ $(echo $PINNED | wc -w) 个子模块的 go.mod 都钉到了 $VERSION"

  echo "== 2. CHANGELOG: Unreleased becomes ${VERSION}, with a new empty Unreleased above it =="
  if grep -q "^## \[$VERSION\]" docs/CHANGELOG.md; then
    echo "  CHANGELOG 里已经有 $VERSION 这一节，不动"
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
    echo "  ✓ 已改，这一版的条目写在 ## [$VERSION] 下面"
  fi

  echo "== 3. README 里的安装命令换成 $VERSION =="
  # 所有模块共用一个版本号：README 写死这一版，照抄的人各模块版本才一致
  # perl 而不是 sed -i -E：macOS 的 BSD sed 要求 -i 后面跟备份后缀，-E 被当成了后缀，
  # 扩展正则没开，\1 报 not defined in the RE，--bump 在 Mac 上停在这一步
  perl -pi -e "s#(github\.com/jupiter-eng/xone(/[a-z/]+)?)\@v[0-9]+\.[0-9]+\.[0-9]+#\${1}\@$VERSION#g" README.md
  grep -q "xone@$VERSION" README.md || { echo "✗ README 里没找到安装命令"; exit 1; }
  echo "  ✓ README 的 go get 写的是 $VERSION"

  echo "== 4. 检查 =="
  scripts/check.sh
  cat <<TIP

已改好，没有提交。接下来：

  git switch -c release/$VERSION && git commit -am "release: $VERSION" && git push -u origin release/$VERSION

开 PR 合进 main（这时 main 上的 go.mod 已经 require ${VERSION}，靠 replace 照常开发），
合进去之后在 GitHub 上点 Actions → release → Run workflow，或者本地：

  scripts/release.sh $VERSION --tag
TIP
  exit 0
fi

# ---- --tag ----
echo "== 1. 确认这个提交可以发 $VERSION =="
[ -z "$(git status --porcelain)" ] || { echo "✗ 工作区有未提交的改动，先提交或暂存"; exit 1; }
# 这一组 tag 一个都不能已经存在，全部先查完再打：只查根 tag 的话，一个残留的 xgin/$VERSION
# 会让第 3 步打到一半才失败，留下半组 tag
taken=
for t in "$VERSION" $(printf "%s/$VERSION\n" $MODS); do
  if git rev-parse -q --verify "refs/tags/$t" >/dev/null; then taken="$taken $t"; fi
done
if [ -n "$taken" ]; then
  cat <<TIP
✗ 这些 tag 已经存在：${taken# }

  - 这个版本已经推出去了（git ls-remote --tags origin 里有 ${VERSION}）：换一个版本号；
  - 是本地上一次 --tag / --smoke 失败留下的、还没推送：删掉再重跑 --tag——

      git tag -d \$(git tag --points-at $VERSION)

    根 tag $VERSION 不在、只剩子模块 tag 的话，上面那条列不出它们，直接删：git tag -d$taken
TIP
  exit 1
fi
left=$(unpinned)
[ -z "$left" ] || { echo "✗ 这个提交的 go.mod 还没钉到 ${VERSION}，先合并 scripts/release.sh $VERSION --bump 的那个 PR："; echo "$left"; exit 1; }
grep -q "^## \[$VERSION\]" docs/CHANGELOG.md || { echo "✗ docs/CHANGELOG.md 里没有 ## [$VERSION] 这一节"; exit 1; }
# --bump turned Unreleased into this section; an empty one means Unreleased held nothing before --bump
has_entries "## [$VERSION]" || { echo "✗ docs/CHANGELOG.md 的 ## [$VERSION] 一节是空的（没有 - 开头的条目）：先把这一版使用者看得见的变化写进去"; exit 1; }
echo "  ✓ $(git log -1 --format='%h %s')"

# 输出不吞掉：红了的话要看的正是那几行
echo "== 2. 跑一遍检查、测试和 e2e =="
scripts/check.sh
scripts/test.sh -count=1
# 单元测试里没有数据库；连真实服务的那一层只有 e2e 看得到，发出去之前必须绿过一次
if [ -n "$E2E_PASSED" ]; then
  echo "  ⚠ 跳过 e2e：--e2e-passed，你担保这个提交的 e2e 刚跑绿过"
else
  scripts/e2e.sh
fi
echo "  ✓ 通过"

# 发出去的 go.mod 带着 replace ../xxx：使用者的构建会忽略依赖里的 replace，
# 只按 require 的 $VERSION 去拉——那些 tag 就在这一步打。
# 去掉 replace 之后编不编得过，这里验不了（tag 还没推，依赖拉不到），交给推送之后的 --verify
echo "== 3. 在 $(git log -1 --format=%h) 上打 tag =="
git tag "$VERSION"
echo "  $VERSION"
for m in $MODS; do
  git tag "$m/$VERSION"
  echo "  $m/$VERSION"
done

cat <<TIP

已在本地打好 tag，没有推送。推送之前先用本地代码冒烟一遍
（和 --verify 同一个外部工程、同一套检查，红了的话 tag 还没出去）：

  scripts/release.sh $VERSION --smoke

冒烟红了、要改代码重来的话，先删掉这组本地 tag 再重跑 --tag：

  git tag -d \$(git tag --points-at $VERSION)

确认无误后只推这一组 tag：

  git push --atomic origin \$(git tag --points-at $VERSION | sed 's|^|refs/tags/|')

推送之后 tag 就被 module proxy 永久缓存了，删不掉，只能再发一版盖过去。
推完必须验证一遍别人装不装得上 —— CI 跑的是工作区里的代码，
证明不了发布出去的版本能装：

  scripts/release.sh $VERSION --verify
TIP
