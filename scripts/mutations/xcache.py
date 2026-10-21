# The xcache mutations: the promises of the xcache module. The format is in
# scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("Configuration")
mutate("xcache spreads its own defaults over several instances", "xcache/config.go", "./xcache", "TestConfig_SingleInstanceForm",
       swap('xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)', 'xconfig.UnmarshalClients(ConfigKey, func() ClientConfig { return ClientConfig{} })'))

section("Startup and shutdown")
# One xerror per module boundary: another layer with new would push the config / connect New
# reports inside, and the op errors.As takes out would always be new
mutate("the error of building an instance is not wrapped again", "internal/xclient/xclient.go", "./xcache", "TestInstall",
       swap('\t\terr = named(module, name, err)', '\t\terr = xerror.Newf(module, "new", "instance %q: %w", name, err)'))

section("Clients")
mutate("an unconfigured xcache still tells the registry it started", "xcache/xcache.go", "./xcache", "TestInitXCache_CSaysNotConfiguredRatherThanTooEarly",
       swap('\t\treturn xclient.Build(ctx, reg, nil, build)\n', '\t\treturn nil\n'))
mutate("MaxCost is how many entries fit", "xcache/xcache.go", "./xcache", "TestNew",
       swap('IgnoreInternalCost: true,','IgnoreInternalCost: false,'))
# ristretto's Close together with concurrent reads and writes produces send on closed channel, and
# the framework cannot stop a caller holding the native *Cache
mutate("closing the cache while somebody reads or writes does not crash", "xcache/xcache.go", "./xcache", "TestClose", swap('closerFunc(c.Clear)', 'closerFunc(c.Close)'))
mutate("the cache's DefaultTTL cannot be negative", "xcache/config.go", "./xcache", "TestValidate",
       swap('\tif c.DefaultTTL < 0 {', '\tif false {'))
# A misspelled name quietly returned 0, and 0 means "never expires" in ristretto
mutate("DefaultTTL panics when the instance cannot be found", "xcache/xcache.go", "./xcache", "TestDefaultTTL",
       swap('{ return reg.Get(name...).ttl }', '{ inst, _ := reg.Lookup(name...); return inst.ttl }'))
# Validating while the configuration is read rests on the instance type implementing Validate:
# with an identical type with no methods, a misconfigured value is let through all the way to New,
# and the error no longer names the file or the instance
mutate("the XCache block is validated while the configuration is read", "xcache/config.go", "./xcache", "TestConfig_InvalidValuesFailAtLoad",
       swap('clients, err := xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)\n\treturn Config{Clients: clients}, err',
            'type raw ClientConfig\n\tm, err := xconfig.UnmarshalClients(ConfigKey, func() raw { return raw(DefaultClientConfig()) })\n\tclients := map[string]ClientConfig{}\n\tfor k, v := range m {\n\t\tclients[k] = ClientConfig(v)\n\t}\n\treturn Config{Clients: clients}, err'))
# ristretto does not count by default, and with Metrics off every count is 0: Metric: true shows
# nothing either
mutate("the cache's Metric switch is passed to ristretto", "xcache/xcache.go", "./xcache", "TestNew_MetricFlagPassedToRistretto|TestCacheCollector",
       swap('Metrics: cfg.Metric,', 'Metrics: false,'))
mutate("a cache instance with Metric off is not exported", "xcache/xcache.go", "./xcache", "TestInstall_ExportsOnlyMetricEnabled_SurvivesXmetricReinstall", swap('\t\tif inst.metric {\n', '\t\tif true {\n'))
mutate("the cache metrics are still exported after xmetric is reinstalled", "xcache/xcache.go", "./xcache", "TestInstall_ExportsOnlyMetricEnabled_SurvivesXmetricReinstall",
       swap('func installMetrics() {\n\tif _, err := xmetric.RegisterAs(',
     'var installed bool\n\nfunc installMetrics() {\n\tif installed {\n\t\treturn\n\t}\n\tinstalled = true\n\tif _, err := xmetric.RegisterAs('))
# A failed type assertion has to count as a miss: returning anything but the assertion's ok turns
# the zero value of the wrong type into a hit
mutate("a Get with the wrong type counts as a miss", "xcache/xcache.go", "./xcache", "TestGet_TypeMismatchIsMiss",
       swap('\t\treturn zero, false\n\t}\n\treturn typed, true', '\t}\n\treturn typed, true'))
mutate("a cache instance's logs carry its name", "xcache/xcache.go", "./xcache", "TestInstall_LogsCarryInstanceName",
       swap('"xcache created", "name", name,', '"xcache created", "name", "",'))
