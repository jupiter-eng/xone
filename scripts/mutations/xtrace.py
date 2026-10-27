# The xtrace mutations: the promises of the xtrace module. The format is in
# scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("Middleware")
mutate("closing a standalone instance leaves the global tracing alone", "xtrace/xtrace.go", "./xtrace", "TestClose",
       swap('\tif live == c {\n\t\tlive = nil\n\t}','\tlive = nil'))
# A processor registered after initialization is attached to the running provider directly; without
# that branch it goes into a pending queue nobody reads again, and the Spans are produced as usual
# and never reach the exporter
mutate("a processor registered after initialization is attached at once", "xtrace/xtrace.go", "./xtrace", "TestAddSpanProcessor",
       swap('\t\tlive.tp.RegisterSpanProcessor(sp)\n\t\treturn\n', '\t\t_ = live.tp\n'))
# Full sampling used to be a bare AlwaysSample: with an upstream's sampled=00 we sampled anyway and
# passed -01 downstream
mutate("full sampling still follows the upstream's sampling decision", "xtrace/xtrace.go", "./xtrace", "TestNew_FullSamplingRespectsUpstreamNotSampled|TestSamplerOf",
       swap('\treturn sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))',
     '\tif ratio >= 1 {\n\t\treturn sdktrace.AlwaysSample()\n\t}\n\treturn sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))'))
mutate("a sample ratio out of range is caught", "xtrace/config.go", "./xtrace", "TestNew_OutOfRangeSampleRateFails",
       swap('if math.IsNaN(c.SampleRatio) || c.SampleRatio < 0 || c.SampleRatio > 1 {', 'if math.IsNaN(c.SampleRatio) && false {'))
# *trusted.com used to match eviltrusted.com, giving an internal token to a domain anybody can
# register
mutate("a wildcard domain is accepted only as *.", "xtrace/propagator.go", "./xtrace", "TestNewHeaderPropagator",
       swap('\t\tif err := checkDomain(d); err != nil {\n\t\t\treturn nil, err\n\t\t}\n', ''))
mutate("a wildcard domain matches on the dot-separated parent domain", "xtrace/propagator.go", "./xtrace", "TestHeaderPropagator_DomainRules",
       swap('strings.HasSuffix(h, "."+parent)', 'strings.HasSuffix(h, parent)'))
# One wrong entry in OTEL_RESOURCE_ATTRIBUTES, or a random UID that cannot be looked up in a
# container, used to keep the service from starting
mutate("a resource attribute collection error still starts as usual", "xtrace/xtrace.go", "./xtrace", "TestNew_",
       swap('\t\tslog.WarnContext(ctx, "xtrace some resource attributes', '\t\tpanic(err)\n\t\tslog.WarnContext(ctx, "xtrace some resource attributes'))
mutate("a resource attribute collection error keeps whatever was collected", "xtrace/xtrace.go", "./xtrace", "TestNew_",
       swap('\t\tslog.WarnContext(ctx, "xtrace some resource attributes', '\t\tres = resource.Empty()\n\t\tslog.WarnContext(ctx, "xtrace some resource attributes'))
# With App.Name unset, service.name="" used to be written all the same, covering both OTel's
# fallback name and OTEL_SERVICE_NAME
mutate("no empty service.name is written when the application name is unset", "xtrace/xtrace.go", "./xtrace", "TestNew_",
       swap('if name := xapp.Name(); name != "" {', 'if name := xapp.Name(); true {'))
# The mutant is the old behavior itself: resource.WithService brings back the SDK's invented
# unknown_service:<executable name>, which is exactly the regression to catch
mutate("an unset application name writes no service.name at all", "xtrace/xtrace.go", "./xtrace", "TestNew_Resource_NoServiceNameWithoutAppName",
       swap('resource.WithAttributes(semconv.ServiceInstanceID(instanceID())),', 'resource.WithService(),'))
mutate("every process is given a service.instance.id", "xtrace/xtrace.go", "./xtrace", "TestNew_Resource_NoServiceNameWithoutAppName",
       swap('\t\tresource.WithAttributes(semconv.ServiceInstanceID(instanceID())),\n', ''))
# In resource.New a later option overrides an earlier one: with the configuration after the
# environment variables, whoever deploys cannot change the service name
mutate("the OTel environment variables override the XApp configuration", "xtrace/xtrace.go", "./xtrace", "TestNew_",
       swap('\t\tresource.WithAttributes(appAttributes()...),\n\t\tresource.WithFromEnv(),\n',
     '\t\tresource.WithFromEnv(),\n\t\tresource.WithAttributes(appAttributes()...),\n'))
# With tracing off a no-op Closer used to be handed back, and the registered processors (along with
# the exporter's connections and goroutines) were never closed
mutate("the registered processors are shut down even with tracing off", "xtrace/xtrace.go", "./xtrace", "TestNew_TracingDisabledStillShutsDownProcessors|TestCloseXTrace_TracingDisabledStillClosesRegisteredProcessors",
       swap('idle := sdktrace.NewTracerProvider(processors(procs)...)', 'idle := sdktrace.NewTracerProvider()'))
# With only 100ms left in the framework it used to wait out its own ShutdownTimeout all the same,
# squeezing out the components behind it
mutate("shutting tracing down honours the deadline the framework gave", "xtrace/xtrace.go", "./xtrace", "TestCloseXTrace",
       swap('return c.shutdown(ctx)', 'return c.Close()'))
mutate("shutting tracing down tightens on the caller's ctx", "xtrace/xtrace.go", "./xtrace", "TestCloseXTrace",
       swap('context.WithTimeout(parent, c.timeout)', 'context.WithTimeout(context.Background(), c.timeout)'))
# Accepted from anybody, an X-Tenant-Id sent by an internet client is carried into every call
# inside the network
mutate("a forwarded header is accepted only from a trusted peer", "xtrace/propagator.go", "./xtrace", "TestHeaderPropagator_AcceptsOnlyFromTrustedPeers|TestNew_InstalledPropagatorAccepts",
       swap('\tif !fromTrustedPeer(carrier) {\n\t\tp.warnUntrusted(carrier)', '\tif false && !fromTrustedPeer(carrier) {\n\t\tp.warnUntrusted(carrier)'))
# baggage is the same kind of thing as a forwarded header, and OTel's own accepts it from anybody
mutate("baggage is accepted only from a trusted peer", "xtrace/propagator.go", "./xtrace", "TestNew_InstalledPropagatorAcceptsBaggageOnlyFromTrustedPeers",
       swap('\tif !fromTrustedPeer(carrier) {\n\t\t// Warned once only', '\tif false {\n\t\t// Warned once only'))
mutate("what is installed is the baggage that accepts a trusted peer only", "xtrace/xtrace.go", "./xtrace", "TestNew_InstalledPropagatorAcceptsBaggageOnlyFromTrustedPeers",
       swap('&trustedBaggage{}', 'propagation.Baggage{}'))
mutate("baggage from an untrusted peer warns once only", "xtrace/propagator.go", "./xtrace", "TestTrustedBaggage",
       swap('b.warned.CompareAndSwap(false, true)', 'true'))
# A malformed forwarding rule used to be reported only when the Propagator was installed; New
# reports it all the same, so the error has to come from the step that reads the configuration
mutate("the forwarding rules are validated while the configuration is read", "xtrace/config.go", "./xtrace", "TestInitXTrace_BadForwardingRulesFailAtConfigRead",
       swap('\tif c.forwardEnabled() {\n\t\tif _, err := newHeaderPropagator(', '\tif false && c.forwardEnabled() {\n\t\tif _, err := newHeaderPropagator('))
mutate("the XTrace block is validated while the configuration is read", "xtrace/xtrace.go", "./xtrace", "TestInitXTrace_BadForwardingRulesFailAtConfigRead",
       swap('xconfig.Unmarshal(ConfigKey, &c)', 'func() error { type raw Config; return xconfig.Unmarshal(ConfigKey, (*raw)(&c)) }()'))
mutate("forwarded headers from an untrusted peer warn once only", "xtrace/propagator.go", "./xtrace", "TestHeaderPropagator_WarnsOnceForUntrustedPeerHeaders",
       swap('\tif p.warned.Load() {\n\t\treturn\n\t}\n', ''),
       swap('if p.warned.CompareAndSwap(false, true) {', 'if true {'))

section("The wording of the warnings")
# It used to name XGin.TrustedProxies outright: a process using xecho alone that added an XGin
# block accordingly failed startup with "configuration nobody reads"
mutate("the forwarding warning does not name XGin alone", "xtrace/propagator.go", "./xtrace", "TestHeaderPropagator_WarnsOnceForUntrustedPeerHeaders",
       swap("forward headers from an untrusted peer, only peers in the web server's TrustedProxies (XGin / XEcho) are trusted", "forward headers from an untrusted peer, only peers in XGin.TrustedProxies are trusted"))
mutate("the baggage warning does not name XGin alone", "xtrace/propagator.go", "./xtrace", "TestTrustedBaggage_WarnsOnceForUntrustedPeerBaggage",
       swap("baggage from an untrusted peer, only peers in the web server's TrustedProxies (XGin / XEcho) are trusted", "baggage from an untrusted peer, only peers in XGin.TrustedProxies are trusted"))
# A name that is not an HTTP field name matches nothing an inbound request carries, so it forwards
# nothing while the configuration reads as if it did -- the same silence a malformed domain used to
# have before checkDomain.
mutate("a malformed forward header name fails startup", "xtrace/propagator.go", "./xtrace", "TestNewHeaderPropagator_MalformedHeaderNameFails",
       swap('\t\tif !validHeaderName(h) {', '\t\tif false {'))
