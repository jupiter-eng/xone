# xmetric 的变异：module xmetric 里的承诺。写法见 scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("客户端")
# 乱序的桶能通过启动，然后在第一次 Observe 时 panic——在业务请求里；
# 不合规的 Namespace 不报错，导出时被悄悄转义，看板按原名查不到
mutate("指标配置说不通时启动就失败", "xmetric/xmetric.go", "./xmetric", "TestNew|TestInitXMetric",
       swap('\tif err := cfg.Validate(); err != nil {', '\tif err := cfg.Validate(); false && err != nil {'))
# New 照样校验，所以只看「启动失败」测不出来：要看错误出自读配置那一步
mutate("XMetric 块在读配置时就校验", "xmetric/xmetric.go", "./xmetric", "TestInitXMetric",
       swap('xconfig.Unmarshal(ConfigKey, &c)', 'func() error { type raw Config; return xconfig.Unmarshal(ConfigKey, (*raw)(&c)) }()'))
mutate("直方图的桶必须严格递增", "xmetric/config.go", "./xmetric", "TestNew",
       swap('if i > 0 && v <= b[i-1] {', 'if false && i > 0 && v <= b[i-1] {'))
mutate("桶写成空列表要启动失败", "xmetric/config.go", "./xmetric", "TestNew|TestInitXMetric",
       swap('\tif len(b) == 0 {\n', '\tif false {\n'))
mutate("Namespace 必须是合法的指标名前缀", "xmetric/config.go", "./xmetric", "TestNew",
       swap('if c.Namespace != "" && !nameRE.MatchString(c.Namespace) {', 'if false {'))
# le 通过启动、第一次 HistogramObserve 在业务请求里 panic；撞上框架指标的变量标签，
# 那组指标注册失败、只打一条错误日志
mutate("常量标签名不能是保留的标签名", "xmetric/config.go", "./xmetric", "TestNew",
       swap('if owner, ok := reservedLabels[k]; ok {', 'if owner, ok := reservedLabels[k]; false && ok {'))
# Config{} 的桶是 nil，原样交给 Prometheus 就是它的 DefBuckets 而不是本包的默认值
mutate("没给桶时用本包的默认值", "xmetric/xmetric.go", "./xmetric", "TestNew", swap('\tcfg = cfg.withDefaults()\n', ''))
# client_golang 现成的 collector 没有 Opts 可填，直接注册在 Registry 上就一个常量标签都不带：
# 按 env 过滤的看板查 go_goroutines{env="prod"} 什么都查不到
mutate("Go 运行时指标也带常量标签", "xmetric/xmetric.go", "./xmetric", "TestNew_RuntimeAndProcessMetricsHaveConstLabels",
       swap('withLabels.Register(promcollectors.NewGoCollector())', 'reg.Register(promcollectors.NewGoCollector())'))
mutate("进程指标也带常量标签", "xmetric/xmetric.go", "./xmetric", "TestNew_RuntimeAndProcessMetricsHaveConstLabels",
       swap('withLabels.Register(promcollectors.NewProcessCollector(', 'reg.Register(promcollectors.NewProcessCollector('))
# 不拦的话 version 撞上 go_info 自带的常量标签，报的是 client_golang 的 wrapping 错误，关掉 GoMetrics 又不报
mutate("常量标签名不能是 go_info 的 version", "xmetric/config.go", "./xmetric", "TestNew",
       swap('\t"version": "go_info in the Go runtime metrics",\n', ''))
