# check.sh / test.sh / release.sh 共用的函数。调用方先 cd 到仓库根目录再 . scripts/lib.sh
#
# 查的是仓库里的文件：签入的，加上新建了还没 git add 的。不用 find / grep -r：
# .gitignore 挡住的副本（比如 .claude/worktrees 下的工作树）不算，也不该被检查。
# 删了还没暂存的要排掉——index 里还有它，磁盘上已经没了，交给 gofmt 就是一个
# 藏在 $(...) 里的失败，set -e 让脚本一声不吭地退出
files() {
  git ls-files -co --exclude-standard -- "$@" | while IFS= read -r f; do
    if [ -e "$f" ]; then echo "$f"; fi
  done
}

# 仓库里的全部 Go 模块目录（根模块是 .），取法同 files：新加的模块在 git add 之前
# check.sh 已经在查它，test.sh 也得跑它的测试
modules() {
  files go.mod '*/go.mod' | xargs -n1 dirname | sort
}

# hooks 事件 日志文件：按出现顺序列出跑过这个事件（starting / stopping）的钩子所在的包，一行一个。
# release.sh 的 --smoke / --verify 拿它比对启动和关闭的顺序；放在这里是为了让 check.sh
# 能拿固定的样例日志测它——日志格式一变，红的是 check.sh，而不是推完 tag 之后的 --verify
#
# 框架每跑一个钩子记一行 starting / stopping，字段 hook=包名.函数名。
# 日志前半截是 xlog 装好之前的 slog 默认格式（... INFO starting hook=xlog.initXLog），
# 后半截是 xlog 的 JSON（"msg":"stopping",…,"hook":"xredis.closeXRedis"），两种都认。
# JSON 里 msg 和 hook 之间不一定挨着：xlog 给每条日志带的 hostname、pid 就插在中间，
# 挨着才认的话 --verify 一个钩子都认不出来。[^}]* 保证两者在同一条日志里
# 取到包名为止：一个包可能登记多个启动钩子，配对的单位是包
hooks() {
  sed -n -e "s/.*\"msg\":\"$1\"[^}]*\"hook\":\"\([^\"]*\)\".*/\1/p" \
         -e "s/.* INFO $1 hook=\([^ ]*\).*/\1/p" "$2" | cut -d. -f1 | uniq
}
