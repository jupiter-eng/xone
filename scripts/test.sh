#!/bin/sh
# 跑全仓库的测试。go test ./... 不跨模块边界，所以按模块逐个跑。
# 模块列表和 check.sh 共用 scripts/lib.sh 的 modules()：签入的，加上新建了
# 还没 git add 的，排掉删了还没暂存的。只认签入的话，新加的模块在 add 之前
# check.sh 已经在查它，这里却不跑它的测试
#
# 一个模块红了也接着跑完其余的，最后一起报：一次看全哪些模块红了，
# 不用修一个、重跑一遍、再发现下一个
#
# 模块之间互不依赖，同时跑几个：带 -race 的编译占了大头，串行时 CPU 大半在等。
# 每个模块的输出先写进自己的文件，全跑完再按模块顺序打出来，不会交错。
# 同时跑几个默认是 CPU 核数，XONE_TEST_JOBS 可以改（比如 1 就是原来的逐个跑）
set -e
cd "$(dirname "$0")/.."
. scripts/lib.sh

jobs=${XONE_TEST_JOBS:-$(getconf _NPROCESSORS_ONLN)}
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

# 每个模块一个 sh：$1 是模块目录，其余原样交给 go test。红了留一个 .failed 记号，
# 退出码不往外传：xargs 遇到非 0 会提前收手，那就不是「跑完其余的」了
modules | xargs -I{} -P "$jobs" sh -c '
  m=$1; shift
  log="$0/$(printf %s "$m" | tr / _)"
  (cd "$m" && GOWORK=off go test -race "$@" ./...) >"$log.log" 2>&1 || : >"$log.failed"
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
  echo "✗ 这些模块的测试没过：$failed"
  exit 1
fi
