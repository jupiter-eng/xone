# xcache 的变异：module xcache 里的承诺。写法见 scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("配置")
mutate("xcache 多实例铺的是自己的默认值", "xcache/config.go", "./xcache", "TestConfig_SingleInstanceForm",
       swap('xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)', 'xconfig.UnmarshalClients(ConfigKey, func() ClientConfig { return ClientConfig{} })'))

section("启动与退出")
# 一个模块边界一个 xerror：再按 new 包一层，New 报的 config / connect
# 就被压进里层，errors.As 取出来的 op 永远是 new
mutate("建实例的错误不再套一层", "internal/xclient/xclient.go", "./xcache", "TestInstall",
       swap('\t\terr = named(module, name, err)', '\t\terr = xerror.Newf(module, "new", "instance %q: %w", name, err)'))

section("客户端")
mutate("xcache 没配也让注册表知道启动过了", "xcache/xcache.go", "./xcache", "TestInitXCache_CSaysNotConfiguredRatherThanTooEarly",
       swap('\t\treturn xclient.Build(ctx, reg, nil, build)\n', '\t\treturn nil\n'))
mutate("MaxCost 就是能存多少条", "xcache/xcache.go", "./xcache", "TestNew",
       swap('IgnoreInternalCost: true,','IgnoreInternalCost: false,'))
# ristretto 的 Close 与并发读写一起跑会 send on closed channel，
# 而拿着原生 *Cache 的调用方框架拦不住
mutate("关缓存时有人在读写也不崩", "xcache/xcache.go", "./xcache", "TestClose", swap('closerFunc(c.Clear)', 'closerFunc(c.Close)'))
mutate("缓存的 DefaultTTL 不能为负", "xcache/config.go", "./xcache", "TestValidate",
       swap('\tif c.DefaultTTL < 0 {', '\tif false {'))
# 名字写错时静默返回 0，而 0 在 ristretto 里是「永不过期」
mutate("DefaultTTL 取不到实例就 panic", "xcache/xcache.go", "./xcache", "TestDefaultTTL",
       swap('{ return reg.Get(name...).ttl }', '{ inst, _ := reg.Lookup(name...); return inst.ttl }'))
# 读配置时就校验靠的是实例类型实现了 Validate：换成一个没有方法的同构类型，
# 配错的值就一路放到 New 才报，报错里也不再有文件和实例名
mutate("XCache 块在读配置时就校验", "xcache/config.go", "./xcache", "TestConfig_InvalidValuesFailAtLoad",
       swap('clients, err := xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)\n\treturn Config{Clients: clients}, err',
            'type raw ClientConfig\n\tm, err := xconfig.UnmarshalClients(ConfigKey, func() raw { return raw(DefaultClientConfig()) })\n\tclients := map[string]ClientConfig{}\n\tfor k, v := range m {\n\t\tclients[k] = ClientConfig(v)\n\t}\n\treturn Config{Clients: clients}, err'))
# ristretto 默认不计数，Metrics 为 nil 时每个计数都是 0：Metric: true 也什么都看不到
mutate("缓存的 Metric 开关传给 ristretto", "xcache/xcache.go", "./xcache", "TestNew_MetricFlagPassedToRistretto|TestCacheCollector",
       swap('Metrics: cfg.Metric,', 'Metrics: false,'))
mutate("Metric 关掉的缓存实例不导出", "xcache/xcache.go", "./xcache", "TestInstall_ExportsOnlyMetricEnabled_SurvivesXmetricReinstall", swap('\t\tif inst.metric {\n', '\t\tif true {\n'))
mutate("xmetric 重装后缓存指标照样导出", "xcache/xcache.go", "./xcache", "TestInstall_ExportsOnlyMetricEnabled_SurvivesXmetricReinstall",
       swap('func installMetrics() {\n\tif _, err := xmetric.RegisterAs(',
     'var installed bool\n\nfunc installMetrics() {\n\tif installed {\n\t\treturn\n\t}\n\tinstalled = true\n\tif _, err := xmetric.RegisterAs('))
# 断言失败要当作没命中：返回断言出来的 ok 之外的值，错类型的零值就当成了命中
mutate("Get 类型对不上当作没命中", "xcache/xcache.go", "./xcache", "TestGet_TypeMismatchIsMiss",
       swap('\t\treturn zero, false\n\t}\n\treturn typed, true', '\t}\n\treturn typed, true'))
mutate("缓存实例的日志带着名字", "xcache/xcache.go", "./xcache", "TestInstall_LogsCarryInstanceName",
       swap('"xcache created", "name", name,', '"xcache created", "name", "",'))
