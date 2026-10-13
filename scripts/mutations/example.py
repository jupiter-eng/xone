# example 的变异：module example 里的承诺。写法见 scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("启动与退出")
mutate("退出时生产者还在投递也不崩", "example/consumer/queue.go", "./example", "TestQueue",
       swap('''\tselect {
\tcase <-q.done: // 已经关了，丢掉这条
\tcase q.ch <- m:
\t}''', '\tq.ch <- m'),
       swap('\t\tclose(q.done)', '\t\tclose(q.ch)'))

section("中间件")
# 两个模块靠 TrustedPeer() 这个方法名接头，各自的单元测试只看得见自己那一半。
# 只改一边的名字，另一边的测试照样全绿——只有 example 里那条端到端的看得见
mutate("xgin 与 xtrace 的可信记号对得上", "xtrace/propagator.go", "./example", "TestXGinAndXTrace_ForwardHeadersOnlyFromTrustedPeers",
       swap('\tt, ok := c.(interface{ TrustedPeer() bool })\n\treturn ok && t.TrustedPeer()', '\tt, ok := c.(interface{ TrustedUpstream() bool })\n\treturn ok && t.TrustedUpstream()'))

section("示例")
# select 在几路同时就绪时随机挑：只靠 select，退出信号到了 worker 还在取消息
mutate("ctx 取消之后不再取消息", "example/consumer/queue.go", "./example", "TestQueue",
       swap('\tif ctx.Err() != nil {\n\t\treturn Message{}, false\n\t}\n\tselect {', '\tselect {'))
# 这两项配错都不报错，服务只是「正常地什么都不做」
mutate("Workers 不大于 0 启动失败", "example/consumer/conf/conf.go", "./example", "TestLoad_",
       swap('if c.Workers <= 0 {', 'if false {'))
mutate("MessageTimeout 不大于 0 启动失败", "example/consumer/conf/conf.go", "./example", "TestLoad_",
       swap('if c.MessageTimeout <= 0 {', 'if false {'))
# 漏登记停止钩子：退出时最后一批改动刷不下去，直接调 closeXKV 的测试照样全绿
mutate("xkv 登记了停止钩子", "example/component/xkv/xkv.go", "./example", "TestRegister_",
       swap('\txhook.BeforeStop(closeXKV) // 档位跟着上面那个启动钩子\n', ''))
# FlushInterval 为 0 时 time.NewTicker 在后台协程里 panic，进程直接死掉
mutate("刷盘间隔不大于 0 启动失败", "example/component/xkv/xkv.go", "./example", "TestInitXKV|TestNew_",
       swap('if c.FlushInterval <= 0 {', 'if false {'))
mutate("直接调 New 也校验配置", "example/component/xkv/xkv.go", "./example", "TestNew_",
       swap('\tif err := c.Validate(); err != nil {\n\t\treturn nil, nil, err\n\t}\n', ''))
# 刷盘先清脏标记再写：写失败了不还回去，这批改动连 Close 那次也刷不下去
mutate("刷盘失败不丢数据", "example/component/xkv/xkv.go", "./example", "TestStore",
       swap('\t\ts.mu.Lock()\n\t\ts.dirty = true\n\t\ts.mu.Unlock()\n', ''))
