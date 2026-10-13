# xginswagger 的变异：module xginswagger 里的承诺。写法见 scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("HTTP 服务")
# 元信息要推迟到第一次访问才填：标题默认取的 App.Name 要等 xapp 的启动钩子，
# Register 早于 xone.Run 时在那一刻就填，标题就不对
mutate("Swagger 元信息等 XApp 读好再填", "xginswagger/swagger.go", "./xginswagger", "TestRegister",
       swap('\tc, _ := fileConfig()\n\n', '\tc, _ := fileConfig()\n\tif info != nil {\n\t\tfill(info, c)\n\t}\n\n'),
       swap('once.Do(func() { fill(info, c) })', 'once.Do(func() {})'))
mutate("Swagger 挂在配置的前缀下", "xginswagger/swagger.go", "./xginswagger", "TestRegister",
       swap('e.GET(c.URLPrefix+route, serve)', 'e.GET(route, serve)'))
# 默认值是预填进结构体的：默认给了 Schemes，没写它也会盖掉注解里的 @schemes
mutate("Swagger 没写 Schemes 就沿用注解", "xginswagger/swagger.go", "./xginswagger", "TestConfig_KeepsAnnotationSchemesWhenUnset",
       swap('\treturn Config{}\n', '\treturn Config{Schemes: []string{"https", "http"}}\n'))
# 校验只剩 Validate 这一处，靠 xconfig.Unmarshal 调到它。变异把 &c 换成一个没有方法的
# 同构类型：解码照旧，Validate 不再被调——这正是忘了导出、或者方法签名写错时的形状
mutate("Swagger 前缀格式不对要启动失败", "xginswagger/swagger.go", "./xginswagger", "TestLoadConfig",
       swap('xconfig.Unmarshal(ConfigKey, &c)', 'func() error { type raw Config; return xconfig.Unmarshal(ConfigKey, (*raw)(&c)) }()'))
