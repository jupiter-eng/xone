# xtrace 的变异：module xtrace 里的承诺。写法见 scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("中间件")
mutate("关独立实例不影响全局链路", "xtrace/xtrace.go", "./xtrace", "TestClose",
       swap('\tif live == c {\n\t\tlive = nil\n\t}','\tlive = nil'))
# 初始化之后登记的处理器直接挂到在跑的 provider 上；漏了这一支，它就进了
# 再也没人读的待办队列，Span 照常产生、永远到不了上报端
mutate("初始化之后登记的处理器立即挂上", "xtrace/xtrace.go", "./xtrace", "TestAddSpanProcessor",
       swap('\t\tlive.tp.RegisterSpanProcessor(sp)\n\t\treturn\n', '\t\t_ = live.tp\n'))
# 全采样原先是裸的 AlwaysSample：上游 sampled=00，我们照样采、再以 -01 往下传
mutate("全采样也听上游的采样决定", "xtrace/xtrace.go", "./xtrace", "TestNew_FullSamplingRespectsUpstreamNotSampled|TestSamplerOf",
       swap('\treturn sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))',
     '\tif ratio >= 1 {\n\t\treturn sdktrace.AlwaysSample()\n\t}\n\treturn sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))'))
mutate("采样率越界要拦住", "xtrace/config.go", "./xtrace", "TestNew_OutOfRangeSampleRateFails",
       swap('if math.IsNaN(c.SampleRatio) || c.SampleRatio < 0 || c.SampleRatio > 1 {', 'if math.IsNaN(c.SampleRatio) && false {'))
# *trusted.com 原先会匹配 eviltrusted.com，内部令牌发给了谁都能注册的域名
mutate("通配域名只认星点开头", "xtrace/propagator.go", "./xtrace", "TestNewHeaderPropagator",
       swap('\t\tif err := checkDomain(d); err != nil {\n\t\t\treturn nil, err\n\t\t}\n', ''))
mutate("通配域名按点分隔的父域匹配", "xtrace/propagator.go", "./xtrace", "TestHeaderPropagator_DomainRules",
       swap('strings.HasSuffix(h, "."+parent)', 'strings.HasSuffix(h, parent)'))
# OTEL_RESOURCE_ATTRIBUTES 写错一项、容器里查不到随机 UID，原先都让服务起不来
mutate("资源属性采集出错照常启动", "xtrace/xtrace.go", "./xtrace", "TestNew_",
       swap('\t\tslog.WarnContext(ctx, "xtrace some resource attributes', '\t\tpanic(err)\n\t\tslog.WarnContext(ctx, "xtrace some resource attributes'))
mutate("资源属性采集出错时用采到的那部分", "xtrace/xtrace.go", "./xtrace", "TestNew_",
       swap('\t\tslog.WarnContext(ctx, "xtrace some resource attributes', '\t\tres = resource.Empty()\n\t\tslog.WarnContext(ctx, "xtrace some resource attributes'))
# App.Name 没配时原先照样写进 service.name=""，把 OTel 的兜底名和 OTEL_SERVICE_NAME 一起盖掉
mutate("没配应用名时不写空的 service.name", "xtrace/xtrace.go", "./xtrace", "TestNew_",
       swap('if name := xapp.Name(); name != "" {', 'if name := xapp.Name(); true {'))
mutate("没配应用名时落到 OTel 的兜底名", "xtrace/xtrace.go", "./xtrace", "TestNew_",
       swap('\t\tresource.WithService(),\n', ''))
# resource.New 里后面的选项压过前面的：配置排在环境变量后面，部署方就改不动服务名
mutate("OTel 环境变量压过 XApp 配置", "xtrace/xtrace.go", "./xtrace", "TestNew_",
       swap('\t\tresource.WithAttributes(appAttributes()...),\n\t\tresource.WithFromEnv(),\n',
     '\t\tresource.WithFromEnv(),\n\t\tresource.WithAttributes(appAttributes()...),\n'))
# 链路关着时原先交回空操作的 Closer，登记的处理器（连同 exporter 的连接和协程）没人关
mutate("链路关着时照样关掉登记的处理器", "xtrace/xtrace.go", "./xtrace", "TestNew_TracingDisabledStillShutsDownProcessors|TestCloseXTrace_TracingDisabledStillClosesRegisteredProcessors",
       swap('idle := sdktrace.NewTracerProvider(processors(procs)...)', 'idle := sdktrace.NewTracerProvider()'))
# 框架只剩 100ms 时照样等满自己的 ShutdownTimeout，后面的组件被挤掉
mutate("链路关闭听框架给的截止时间", "xtrace/xtrace.go", "./xtrace", "TestCloseXTrace",
       swap('return c.shutdown(ctx)', 'return c.Close()'))
mutate("链路关闭在调用方的 ctx 上收紧", "xtrace/xtrace.go", "./xtrace", "TestCloseXTrace",
       swap('context.WithTimeout(parent, c.timeout)', 'context.WithTimeout(context.Background(), c.timeout)'))
# 照单全收的话，公网客户端发一个 X-Tenant-Id 就被带进内网的每一次调用
mutate("透传 Header 只收可信对端发来的", "xtrace/propagator.go", "./xtrace", "TestHeaderPropagator_AcceptsOnlyFromTrustedPeers|TestNew_InstalledPropagatorAccepts",
       swap('\tif !fromTrustedPeer(carrier) {\n\t\tp.warnUntrusted(carrier)', '\tif false && !fromTrustedPeer(carrier) {\n\t\tp.warnUntrusted(carrier)'))
# baggage 和透传头是同一种东西，OTel 自带的那个谁发来的都收
mutate("baggage 只收可信对端发来的", "xtrace/propagator.go", "./xtrace", "TestNew_InstalledPropagatorAcceptsBaggageOnlyFromTrustedPeers",
       swap('\tif !fromTrustedPeer(carrier) {\n\t\t// 只告警一次', '\tif false {\n\t\t// 只告警一次'))
mutate("装的是只收可信对端的 baggage", "xtrace/xtrace.go", "./xtrace", "TestNew_InstalledPropagatorAcceptsBaggageOnlyFromTrustedPeers",
       swap('&trustedBaggage{}', 'propagation.Baggage{}'))
mutate("不可信对端带来 baggage 时只告警一次", "xtrace/propagator.go", "./xtrace", "TestTrustedBaggage",
       swap('b.warned.CompareAndSwap(false, true)', 'true'))
# 透传规则写错原先要等装 Propagator 才报；New 照样会报，所以要看错误出自读配置那一步
mutate("透传规则在读配置时就校验", "xtrace/config.go", "./xtrace", "TestInitXTrace_BadForwardingRulesFailAtConfigRead",
       swap('\tif c.forwardEnabled() {\n\t\tif _, err := newHeaderPropagator(', '\tif false && c.forwardEnabled() {\n\t\tif _, err := newHeaderPropagator('))
mutate("XTrace 块在读配置时就校验", "xtrace/xtrace.go", "./xtrace", "TestInitXTrace_BadForwardingRulesFailAtConfigRead",
       swap('xconfig.Unmarshal(ConfigKey, &c)', 'func() error { type raw Config; return xconfig.Unmarshal(ConfigKey, (*raw)(&c)) }()'))
mutate("不可信对端带来透传头时只告警一次", "xtrace/propagator.go", "./xtrace", "TestHeaderPropagator_WarnsOnceForUntrustedPeerHeaders",
       swap('\tif p.warned.Load() {\n\t\treturn\n\t}\n', ''),
       swap('if p.warned.CompareAndSwap(false, true) {', 'if true {'))

section("告警文案")
# 原先写死 XGin.TrustedProxies：只用 xecho 的进程照着加一个 XGin 块，启动就报「没人读的配置」
mutate("透传告警不只点 XGin", "xtrace/propagator.go", "./xtrace", "TestHeaderPropagator_WarnsOnceForUntrustedPeerHeaders",
       swap("forward headers from an untrusted peer, only peers in the web server's TrustedProxies (XGin / XEcho) are trusted", "forward headers from an untrusted peer, only peers in XGin.TrustedProxies are trusted"))
mutate("baggage 告警不只点 XGin", "xtrace/propagator.go", "./xtrace", "TestTrustedBaggage_WarnsOnceForUntrustedPeerBaggage",
       swap("baggage from an untrusted peer, only peers in the web server's TrustedProxies (XGin / XEcho) are trusted", "baggage from an untrusted peer, only peers in XGin.TrustedProxies are trusted"))
