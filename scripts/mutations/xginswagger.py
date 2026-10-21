# The xginswagger mutations: the promises of the xginswagger module. The format is in
# scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("HTTP servers")
# Filling in the metadata has to wait for the first request: the title defaults to App.Name, which
# waits for xapp's start hook, so a Register before xone.Run that filled it in on the spot would
# produce the wrong title
mutate("the Swagger metadata waits until XApp has been read", "xginswagger/swagger.go", "./xginswagger", "TestRegister",
       swap('\tc, _ := fileConfig()\n\n', '\tc, _ := fileConfig()\n\tif info != nil {\n\t\tfill(info, c)\n\t}\n\n'),
       swap('once.Do(func() { fill(info, c) })', 'once.Do(func() {})'))
mutate("Swagger is mounted under the configured prefix", "xginswagger/swagger.go", "./xginswagger", "TestRegister",
       swap('e.GET(c.URLPrefix+route, serve)', 'e.GET(route, serve)'))
# The defaults are pre-filled in the struct: a default for Schemes would override the annotations'
# @schemes even when nothing was written
mutate("Swagger keeps the annotations' schemes when Schemes is unset", "xginswagger/swagger.go", "./xginswagger", "TestConfig_KeepsAnnotationSchemesWhenUnset",
       swap('\treturn Config{}\n', '\treturn Config{Schemes: []string{"https", "http"}}\n'))
# Validation lives in Validate alone and is reached through xconfig.Unmarshal. The mutation swaps
# &c for an identical type with no methods: decoding works as before and Validate is never called --
# exactly the shape of forgetting to export it, or writing the method signature wrong
mutate("a malformed Swagger prefix fails startup", "xginswagger/swagger.go", "./xginswagger", "TestLoadConfig",
       swap('xconfig.Unmarshal(ConfigKey, &c)', 'func() error { type raw Config; return xconfig.Unmarshal(ConfigKey, (*raw)(&c)) }()'))
