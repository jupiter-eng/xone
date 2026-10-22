# The xmetric mutations: the promises of the xmetric module. The format is in
# scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("Clients")
# Buckets out of order pass startup and then panic at the first Observe -- inside a business
# request; a non-conforming Namespace produces no error and is quietly escaped on export, so a
# dashboard looking for the original name finds nothing
mutate("a metric configuration that does not add up fails at startup", "xmetric/xmetric.go", "./xmetric", "TestNew|TestInitXMetric",
       swap('\tif err := cfg.Validate(); err != nil {', '\tif err := cfg.Validate(); false && err != nil {'))
# New validates all the same, so "startup failed" alone proves nothing: the error has to come from
# the step that reads the configuration
mutate("the XMetric block is validated while the configuration is read", "xmetric/xmetric.go", "./xmetric", "TestInitXMetric",
       swap('xconfig.Unmarshal(ConfigKey, &c)', 'func() error { type raw Config; return xconfig.Unmarshal(ConfigKey, (*raw)(&c)) }()'))
mutate("histogram buckets must be strictly increasing", "xmetric/config.go", "./xmetric", "TestNew",
       swap('if i > 0 && v <= b[i-1] {', 'if false && i > 0 && v <= b[i-1] {'))
mutate("buckets written as an empty list fail startup", "xmetric/config.go", "./xmetric", "TestNew|TestInitXMetric",
       swap('\tif len(b) == 0 {\n', '\tif false {\n'))
mutate("Namespace must be a valid metric name prefix", "xmetric/config.go", "./xmetric", "TestNew",
       swap('if c.Namespace != "" && !nameRE.MatchString(c.Namespace) {', 'if false {'))
# le passes startup and panics at the first HistogramObserve inside a business request; colliding
# with a framework metric's variable label makes that group fail to register, with one error log and
# nothing else
mutate("a constant label name cannot be a reserved label name", "xmetric/config.go", "./xmetric", "TestNew",
       swap('if owner, ok := reservedLabels[k]; ok {', 'if owner, ok := reservedLabels[k]; false && ok {'))
# Config{}'s buckets are nil, and handing them to Prometheus as they are means its DefBuckets
# rather than this package's defaults
mutate("this package's defaults are used when no buckets are given", "xmetric/xmetric.go", "./xmetric", "TestNew", swap('\tcfg = cfg.withDefaults()\n', ''))
# client_golang's ready-made collectors have no Opts to fill in, and registered on the Registry
# directly they carry not one constant label: a dashboard filtering by env finds nothing for
# go_goroutines{env="prod"}
mutate("the Go runtime metrics carry the constant labels too", "xmetric/xmetric.go", "./xmetric", "TestNew_RuntimeAndProcessMetricsHaveConstLabels",
       swap('withLabels.Register(promcollectors.NewGoCollector())', 'reg.Register(promcollectors.NewGoCollector())'))
mutate("the process metrics carry the constant labels too", "xmetric/xmetric.go", "./xmetric", "TestNew_RuntimeAndProcessMetricsHaveConstLabels",
       swap('withLabels.Register(promcollectors.NewProcessCollector(', 'reg.Register(promcollectors.NewProcessCollector('))
# Uncaught, version collides with go_info's own constant label and what is reported is
# client_golang's wrapping error, which disappears again with GoMetrics off
mutate("a constant label name cannot be go_info's version", "xmetric/config.go", "./xmetric", "TestNew",
       swap('\t"version": "go_info in the Go runtime metrics",\n', ''))

section("The application identity")
# One name per signal: without this the logs and the spans say which application they came from and
# the metrics do not, so a dashboard cannot tell two services apart at all
mutate("the application identity reaches the constant labels", "xmetric/xmetric.go", "./xmetric", "TestInitXMetric_CarriesTheAppIdentityAsConstLabels",
       swap('return install(withIdentity(c))', 'return install(c)'))
mutate("a configured label wins over the application identity", "xmetric/xmetric.go", "./xmetric", "TestInitXMetric_AConfiguredLabelWinsOverTheAppIdentity",
       swap('\t\tif _, ok := labels[l.key]; ok {\n\t\t\tcontinue\n\t\t}\n', ''))
# A constant service_name="" would be attached to every metric, go_* and process_* included
mutate("an identity nobody configured attaches no empty label", "xmetric/xmetric.go", "./xmetric", "TestInitXMetric_NoIdentityAddsNoEmptyLabel",
       swap('\t\tif l.value == "" {\n\t\t\tcontinue\n\t\t}\n', ''))
