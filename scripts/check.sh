#!/bin/sh
# 把设计约束编译成检查。趁还没东西可违反的时候加上——
# 晚了就得豁免一堆存量，那等于没加。
set -e
cd "$(dirname "$0")/.."
fail() { echo "✗ $1"; exit 1; }

. scripts/lib.sh
modules=$(modules)
gosrc=$(files '*.go' | grep -v '_test\.go$')  # 非测试的 Go 源文件

# ---- 1. 核心的依赖足迹 ----
# 使用者 import 任何一个核心里的包，都会背上核心的全部依赖约束，所以核心必须极瘦。
# 每加一个都要先问：能不能不加。
# GOWORK=off 是必须的：工作区里 go list -m all 会把所有模块的依赖并在一起。
deps=$(GOWORK=off go list -m all)
n=$(echo "$deps" | grep -vc '^github.com/jupiter-eng/xone')
[ "$n" -le 3 ] || fail "核心模块图 $n 个模块，超过上限 3（当前应为 yaml 及其测试依赖）"
echo "✓ 核心模块图 $n 个（上限 3）"

# ---- 2. 核心不得依赖任何集成模块 ----
# 反过来就把集成的依赖又带回给所有人了，分模块也就白分了
! echo "$deps" | grep -q '^github.com/jupiter-eng/xone/' \
  || fail "核心 require 了集成模块，分模块的意义没了"
echo "✓ 核心不依赖任何集成模块"

# ---- 3. xhook 与基础包必须零第三方依赖 ----
# xhook 是每个集成都要认识的包（另一个是 xconfig），一旦它有依赖，所有集成都被迫背上。
# xtls 同理：它是 xgorm / xredis / xhttp / xkafka 配置结构体里的一个字段类型。
# internal/web 是各 Web 集成共用的那一半：混进一个三方包，每个 Web 集成都得背上
# go list 的输出先落进变量：直接接管道的话，包挪走了、go list 报错，grep -c 数出 0，检查静默通过
for pkg in ./xhook ./xerror ./xutil ./xtls ./internal/web; do
  list=$(GOWORK=off go list -deps "$pkg") || fail "go list -deps $pkg 失败（包挪走了？同步这里的列表）"
  d=$(echo "$list" | grep -E '^[^/]*\.' | grep -vc jupiter-eng || true)
  [ "$d" -eq 0 ] || fail "$pkg 混进了 $d 个第三方包"
done
echo "✓ xhook / xerror / xutil / xtls / internal/web 零第三方依赖"

# xlog 会换掉 slog.Default()，只跟着 xgin / xecho / xcron 来（它们是「进程的主体」：Web 服务、定时任务进程，
# 要的就是 JSON 日志和 trace_id）。数据类集成、xtrace、xmetric 都不许把它带进来：
# 否则只用 xgorm 的程序又被接管了日志。xtrace / xmetric / xkafka 往 internal/logext 注入，不 import xlog。
# 查每个模块的全部包（./...），不只是模块根上那一个：核心里的 xflow、xonetest、internal/... 带进 xlog，
# 使用者 import 它们同样被接管日志。xlog 自己的 Deps 里没有它自己，不用单独排掉
xlog=github.com/jupiter-eng/xone/xlog
for m in . xtrace xmetric xgorm xredis xhttp xcache xkafka; do
  out=$(cd "$m" && GOWORK=off go list -f "{{.ImportPath}}{{range .Deps}}{{if eq . \"$xlog\"}} <-xlog{{end}}{{end}}" ./...) \
    || fail "$m: go list 失败（模块挪走了？同步这里的列表）"
  [ -n "$out" ] || fail "$m: go list 一个包都没列出来"
  bad=$(echo "$out" | sed -n 's/ <-xlog$//p')
  [ -z "$bad" ] || fail "$m 里这些包把 xlog 带进来了（根模块、数据类集成、xtrace、xmetric 都不该带）：
$bad"
done
echo "✓ 只有 xgin / xecho / xcron 带着 xlog"

# ---- 4. 只有集成包可以有 init() ----
# 集成包的 init 只登记不初始化；核心自己则连登记都不该有。
# 判据是「这个目录调没调 xhook.BeforeStart」，不是目录名也不是有没有 go.mod：
# xlog 零依赖留在核心模块里，同样是集成包。
# 两个登记入口：钩子登记进框架，方言登记进 xgorm。两者都只是「记下来」，
# 不初始化任何东西，所以放在 init() 里是对的
# xhook 自己要排掉：包注释里写着用法示例，会被这个 grep 认成集成包
integrations=$(grep -lE 'xhook\.BeforeStart\(|xgorm\.RegisterDialect\(' $gosrc | xargs -r -n1 dirname | sort -u | grep -vx 'xhook')
for f in $(grep -l '^func init()' $gosrc); do
  d=$(dirname "$f")
  echo "$integrations" | grep -qx "$d" || fail "$f 有 init()，但它不是集成包（没有 xhook.BeforeStart）"
done
echo "✓ init() 只出现在集成包里"

# ---- 5. 核心公开 API 数量上限 ----
# 框架的 API 是永久的。让「加一个」有代价，超了就得先砍再加。
# api 包：数导出的 func / type。go doc 失败要报出来（不然数成 0 就过了）；
# 数到 0 时 grep -c 退出码是 1，set -e 下 a=$(...) 会让脚本静默退出，所以那一步 || true
api() {
  doc=$(go doc -all "$1") || return 1
  echo "$doc" | grep -cE '^(func|type) ' || true
}
a=$(api .)
[ "$a" -le 15 ] || fail "根包公开 API $a 个，超过上限 15"

# xhook / xconfig 是每个集成都必须认识的那两个包，所以它们的上限最要紧：
# 每加一个导出，就是一条所有第三方集成都得跟着理解的规矩。
# 这个数曾经在几轮重构里一路涨到 15 而没人拦，所以现在把它钉死。
r=$(api ./xhook)
[ "$r" -le 6 ] || fail "xhook 公开 API $r 个，超过上限 6——先想清楚能不能砍掉一个再加"
x=$(api ./xconfig)
[ "$x" -le 6 ] || fail "xconfig 公开 API $x 个，超过上限 6"
# xonetest 是给使用者测试用的，同样是永久 API：换配置、跑钩子，就这些。
# 想往里加「再帮你做一件事」的，先问那件事是不是该由 xone.Run 或被测包自己做
tt=$(api ./xonetest)
[ "$tt" -le 3 ] || fail "xonetest 公开 API $tt 个，超过上限 3"
# xtls 是各客户端集成共用的 TLS 块：一个配置类型，外加校验和装出 *tls.Config 两个方法。
# 它出现在使用者的配置结构体里（xredis.ClientConfig.TLS），同样是永久 API。
# 服务端那一侧（xgin、xecho 的 TLS 块，带 ClientCAFile / MinVersion）形状不同，在 internal/web 里，不往这里加
tl=$(api ./xtls)
[ "$tl" -le 3 ] || fail "xtls 公开 API $tl 个，超过上限 3"
echo "✓ 公开 API：根包 ${a}（上限 15）、xhook ${r}（上限 6）、xconfig ${x}（上限 6）、xonetest ${tt}（上限 3）、xtls ${tl}（上限 3）"

# ---- 6. 集成包必须导出纯构造器 New，且不得引用根包 ----
# New 保证「零装配」永远只是默认路径，不是唯一路径：
# 测试和特殊场景始终可以绕开框架直接造实例。
# 不引用根包保证依赖是单向的——集成认识 registry，框架认识 registry，彼此不认识。
[ -n "$integrations" ] || fail "一个集成包都没找到，第 4 步的判据失效了"
for d in $integrations; do
  # 驱动包 import 的是 xgorm 而不是框架，这条对它不适用
  grep -rq 'xhook\.BeforeStart(' "$d"/*.go || continue
  ! grep -rq '"github.com/jupiter-eng/xone"' "$d"/*.go || fail "$d 引用了根包（只能 import xhook / xconfig）"
  # 只读配置、不造任何东西的包（如 xapp、xflow）没有构造器可言，这条对它是空的
  grep -rq 'xhook\.BeforeStop(' "$d"/*.go || continue
  # xcron 例外：它登记的是进程里唯一的那个调度器，没有「一个实例」可造；
  # 测试直接调包内的调度器
  [ "$d" != xcron ] || continue
  # New[T any]( 也算：泛型构造器同样是「绕开框架直接造一个」的入口
  grep -rqE '^func New[(\[]' "$d"/*.go || fail "$d 登记了组件，却没有纯构造器 New"
done
echo "✓ 集成包检查通过（$(echo "$integrations" | wc -w) 个）"

# ---- 6.1 示例只用使用者用得到的东西 ----
# example/ 的 module 路径在 github.com/jupiter-eng/xone 之下，Go 的 internal 规则
# 因此放它进来——但使用者的代码进不来。示例用了 internal/config、internal/hook，
# 使用者照抄就编译不过。测试里要换配置、跑钩子，用 xonetest
bad=$(files 'example/*.go' | xargs grep -ln '"github.com/jupiter-eng/xone/internal/' || true)
[ -z "$bad" ] || fail "example/ 不能 import internal/（使用者 import 不到），改用 xonetest：
$bad"
echo "✓ example/ 不 import internal/"

# ---- 7. 配置字段都写进文档 ----
# 配置是使用者唯一的操作界面，加了字段却没写文档，等于没加。
# 反过来文档里多写一个不存在的 key，会让人配了半天发现不生效。
#
# example/ 不算：那里的配置是示例自带的，写在示例自己的注释和 application.yml 里，
# 搬进框架的配置手册反而会让人以为它是框架的一部分。e2e/ 同理：被测服务的业务配置块
# 是测试自己的，写在 e2e/service/application.yml 里
#
# 按节查，两个方向都查（见 internal/schemagen/docs.go）：每个字段要出现在它那个模块
# README 的「## 配置」一节里（xlog/README.md 之类），每一节的 YAML 示例要过得了 schema。
# 从前是在整份文档里 grep 字段名：Name、Timeout、Enable 总能在别的节里找到，
# 永远通过；[A-Za-z]+ 还漏掉了 UseH2C 这种带数字的名字；反方向根本没查。
#
# 按节查依赖 schema 认得全部字段，所以先确认每个 yaml 标签都进了 schema——
# 否则一个没挂到 Config 上的结构体的字段会同时躲过 schema 和这里
missing=""
for f in $(echo "$gosrc" | grep -vE '^(example|e2e)/' | xargs grep -hoE 'yaml:"[A-Za-z0-9]+"' | sed 's/yaml:"//; s/"//' | sort -u); do
  grep -q "\"$f\": {" config_schema.json || missing="$missing $f"
done
[ -z "$missing" ] || fail "这些配置字段没进 config_schema.json（结构体没挂到 Config 上？）：$missing"
# schemagen 是独立的工具 module，从根目录经 go.work 调它
out=$(go test -count=1 -run TestCheckDocs ./internal/schemagen 2>&1) || fail "模块 README 的「## 配置」与 Config 结构体对不上：
$out"
echo "✓ 配置字段都写进文档了（按节，双向）"

# ---- 8. 运行期字符串必须是英文 ----
# 注释写给读这份代码的人，用中文；但错误和日志会落进使用者的系统——
# 进他们的告警、他们的日志检索、他们的 issue。中文的日志字段名还会变成
# JSON 的 key，让日志平台的索引和看板直接对不上。
# internal/schemagen 是构建工具，输出给的是改这份代码的人，不适用。
zh=$(GOFILES=$(files '*.go') python3 scripts/check_zh.py)
[ -z "$zh" ] || fail "这些地方的运行期字符串还是中文（错误和日志要用英文）：
$zh"
echo "✓ 运行期字符串都是英文"

# ---- 9. config_schema.json 与结构体一致 ----
# schema 给 IDE 用：YAML 里字段补全、拼错标红、悬停看说明。
# 它是生成的，不是手写的——上百个字段手工同步迟早对不上，
# 而对不上的表现是补全出一个根本不存在的字段，比没有 schema 更糟。
go run ./internal/schemagen -check || fail "config_schema.json 与 Config 结构体对不上，跑一次：go run ./internal/schemagen"
echo "✓ config_schema.json 与结构体一致"

# ---- 10. 错误一律走 xerror ----
# 模块对外返回的错误都带模块名和操作名，调用方因此能问「这是谁报的」，
# 而不必去匹配错误消息里的字符串前缀。两种退回旧写法的情况在这里拦下：
#   1. fmt.Errorf("xgorm: ...")  —— 前缀写进消息，等于没有结构
#   2. xerror.Newf(..., "...err=[%v]", err) —— %v 把错误变成文本，
#      errors.Is / errors.As 到此为止，调用方再也判断不了根因
bad=$(grep -nE 'fmt\.Errorf\("x[a-z]+: ' $gosrc || true)
[ -z "$bad" ] || fail "这些地方还在用带模块前缀的 fmt.Errorf，应改成 xerror：
$bad"

bad=$(grep -n 'xerror.Newf' $gosrc | grep 'err=\[%v\]' || true)
[ -z "$bad" ] || fail "这些 xerror 用 %v 包装底层错误，错误链会断，应改成 %w：
$bad"
echo "✓ 错误都走 xerror（%w 保住错误链）"

# ---- 11. 文档和示例里不再出现已经删掉的名字 ----
# 使用者照着 README 和 example/ 抄。几轮重构之后，那里还留着 Component.Init、
# registry.Register 这些早就不存在的名字，注释里还在教一个已经撤掉的写法——
# 比没有文档更糟，因为它看上去是对的。删掉一个公开名字时，把它加进这张表
#
# docs/CHANGELOG.md 不扫，而且是对整张表都不扫：它是历史记录，按约定每个删掉的公开名字都要在那里
# 写明「旧写法怎么迁移」，也就是必然点名——表里的每一条迟早都会在那里出现一次，这不是残留。
# 别的文档照查
gone='xutil\.(GetOrDefault|ToPtr)\b|xredis\.TLSConfig|Component\.Init|registry\.(Declare|Provide|Register)|ProvideEach|xconfig\.(HasKey|DecodeClients)\b|\.OnInit\(|xgin\.With(Config|Log|Trace|Metric|MetricPath|SkipPaths|RequestBodyLog|ResponseBodyLog|ZHTranslations)\('
hits=$(files '*.go' '*.md' '*.yml' ':(exclude)docs/CHANGELOG.md' | xargs grep -nE "$gone" || true)
[ -z "$hits" ] || fail "还在提已经删掉的名字（改文档，或者确认它真的还在）：
$hits"
echo "✓ 文档和示例里没有已经删掉的名字"

# ---- 12. 变异模式都还对得上代码 ----
# 全量变异测试要几分钟，不在这里跑；但「模式恰好匹配 N 处」只是字符串比对，
# 不到一秒。代码挪走了、变异没跟着挪，这里当场就红，不用等下一次想起来跑全量
python3 scripts/mutate.py --dry-run || fail "有变异的模式对不上代码了，改 scripts/mutations/ 下对应的那条"

# ---- 12.2 release.sh 认得出钩子日志 ----
# --smoke / --verify 靠 hooks()（scripts/lib.sh）从日志里认出每个钩子的启停，再比对是不是逆序。
# xlog 在 msg 和 hook 之间加 hostname、pid 之后，只认挨着的 sed 一个都认不出来，
# 红在 tag 推出去之后的 --verify 上。拿固定的样例日志测一遍：日志格式再变，先红的是这里
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
[ "$got" = "xlog xtrace xgin " ] || { rm -f "$t"; fail "hooks() 认错了启动钩子：want=[xlog xtrace xgin ] got=[$got]（改了日志格式？同步 scripts/lib.sh 的 hooks）"; }
got=$(hooks stopping "$t" | tr '\n' ' ')
[ "$got" = "xgin xtrace xlog " ] || { rm -f "$t"; fail "hooks() 认错了停止钩子：want=[xgin xtrace xlog ] got=[$got]（改了日志格式？同步 scripts/lib.sh 的 hooks）"; }
rm -f "$t"
echo "✓ release.sh 认得出钩子日志（xlog JSON 带额外字段、字段挨着、xlog 之前的文本格式）"

# ---- 12.1 测试函数名用英文 ----
# 测试名会出现在 go test -run、CI 的失败列表和变异表的过滤里，写英文才好搜、好复制
# 用 perl 而不是 grep -P：macOS 的 BSD grep 没有 -P，报错又被 || true 吞掉，这条检查在那里永远通过
zhtest=$(files '*_test.go' | xargs perl -ne 'print "$ARGV:$.:$_" if /^func (Test|Benchmark|Fuzz|Example)\w*[^\x00-\x7F]/; close ARGV if eof') \
  || fail "扫描测试函数名失败"
[ -z "$zhtest" ] || fail "测试函数名要用英文（描述场景，如 TestNew_FailsFastOnTypo）：
$zhtest"
echo "✓ 测试函数名都是英文"

# ---- 13. 基本卫生 ----
unformatted=$(files '*.go' | xargs gofmt -l)
[ -z "$unformatted" ] || fail "有文件未格式化：$unformatted"
# 过了不出声，不过的话把 vet 的原话带出来：只说「未通过」得自己再跑一遍才知道是哪一行
for m in $modules; do
  out=$(cd "$m" && GOWORK=off go vet ./... 2>&1) || fail "$m go vet 未通过：
$out"
done
echo "✓ gofmt / go vet（$(echo "$modules" | wc -w) 个模块）"
