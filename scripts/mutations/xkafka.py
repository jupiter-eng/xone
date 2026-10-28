# The xkafka mutations: the promises of the xkafka module. The format is in
# scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("Configuration")
mutate("xkafka spreads its own defaults over several instances", "xkafka/config.go", "./xkafka", "TestConfig_SingleClusterFormKeepsDefaults|TestConfig_MultiClusterForm",
       swap('xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)', 'xconfig.UnmarshalClients(ConfigKey, func() ClientConfig { return ClientConfig{} })'))
# franz-go defaults to earliest: a new group going live works through the topic's whole history
mutate("a new consumer group starts from latest by default", "xkafka/config.go", "./xkafka", "TestResetOffset_DefaultIsLatest",
       swap('Consumer: ConsumerConfig{ResetOffset: "latest"},', 'Consumer: ConsumerConfig{ResetOffset: "earliest"},'))
mutate("ResetOffset is handed to the consumer's client", "xkafka/consume.go", "./xkafka", "TestResetOffset_LatestSkipsOldRecords",
       swap('kgo.ConsumeResetOffset(resetOffset(inst.cfg.Consumer.ResetOffset)),', 'kgo.ConsumeResetOffset(resetOffset("earliest")),'))
mutate("Acks set to leader turns idempotent writes off for the caller", "xkafka/xkafka.go", "./xkafka", "TestNew_AppliesProducerSettings",
       swap('opts = append(opts, kgo.RequiredAcks(kgo.LeaderAck()), kgo.DisableIdempotentWrite())', 'opts = append(opts, kgo.DisableIdempotentWrite())'))
mutate("Linger is handed to franz-go explicitly (0 means no wait)", "xkafka/xkafka.go", "./xkafka", "TestNew_AppliesProducerSettings",
       swap('\t\tkgo.ProducerLinger(cfg.Producer.Linger),\n', ''))
mutate("DialTimeout is handed to franz-go", "xkafka/xkafka.go", "./xkafka", "TestNew_AppliesProducerSettings",
       cut('\tif cfg.DialTimeout > 0 {', '\t}\n'))
# franz-go fills its own 10s in only where the option was never given: a 0 handed over is taken as
# it stands and dials with no deadline at all, which is not what the configuration documents
mutate("a DialTimeout of 0 leaves the option out rather than handing a 0 over", "xkafka/xkafka.go", "./xkafka", "TestNew_DialTimeoutZeroLeavesItToFranzGo",
       swap('if cfg.DialTimeout > 0 {', 'if cfg.DialTimeout >= 0 {'))
# Aimed at the call site: the consumers are built from the ClientConfig build keeps, so an id settled
# anywhere else would not reach them
mutate("an unset ClientID uses XApp.Name", "xkafka/xkafka.go", "./xkafka", "TestBuild_ClientIDDefaultsToAppName",
       swap('c.ClientID = xapp.Name()', 'c.ClientID = xapp.Version()'))
# Nothing is invented for a name nobody configured, so saying the consequence out loud once is the
# whole promise: without it the brokers quietly see franz-go's kgo and nobody is told
mutate("a client id nobody configured is one WARN at startup", "xkafka/xkafka.go", "./xkafka", "TestBuild_NoClientIDAnywhereWarnsAndInventsNothing",
       swap('slog.WarnContext(ctx, "xkafka no client id configured', 'slog.DebugContext(ctx, "xkafka no client id configured'))
mutate("the SASL password does not reach the validation error", "xkafka/config.go", "./xkafka", "TestConfig_Validate",
       swap('if s.Username == "" || s.Password == "" {', 'if s.Username == "" {'))

# How long a partition stops being consumed after a process is killed outright. Left out of the
# client options, a caller lowering it for a faster take-over silently keeps franz-go's 45s
mutate("the session timeout reaches franz-go", "xkafka/consume.go", "./xkafka", "TestSessionTimeout_ReachesTheClient",
       cut("\tif d := inst.cfg.Consumer.SessionTimeout; d != 0 {", "\t}\n"))

section("Startup and shutdown")
mutate("an unconfigured xkafka still tells the registry it started", "xkafka/xkafka.go", "./xkafka", "TestInitXKafka_CSaysUnconfiguredNotTooEarly",
       swap('\t\treturn xclient.Build(ctx, reg, nil, build)\n', '\t\treturn nil\n'))
# franz-go v1.21.7's Ping ignores the ctx while the peer stays silent and waits out
# RequestTimeoutOverhead (10s). Aimed at the call site
mutate("the probe does not wait for a silent broker", "xkafka/xkafka.go", "./xkafka", "TestNew_SilentBrokerBoundedByDialTimeout|TestNew_ExitSignalAbortsProbe",
       swap('xclient.Probe(ctx, probePolicy(cfg), probe(client))', 'xclient.Probe(ctx, probePolicy(cfg), client.Ping)'))
mutate("an authentication failure is not retried", "xkafka/xkafka.go", "./xkafka", "TestAuthFailed",
       swap('\t\tAuthFailed: authFailed,\n', ''))
mutate("the error for an unreachable broker names the address", "xkafka/xkafka.go", "./xkafka", "TestNew_UnreachableBrokerFailsWithAddress|TestNew_SilentBrokerBoundedByDialTimeout",
       swap('"cannot reach %s: %w", addrs, err)', '"cannot reach brokers: %w", err)'))
# Close fails every buffered message (measured on v1.21.7: 10 of 10 lost)
mutate("the stop hook Flushes before it Closes", "xkafka/xkafka.go", "./xkafka", "TestCloseXKafka_FlushesBeforeClose",
       swap('if err := inst.client.Flush(ctx); err != nil {', 'if err := error(nil); err != nil && inst.client != nil {'))
mutate("the client is at StageClient and the consumers at StageServer", "xkafka/xkafka.go", "./xkafka", "TestHooks_Stages",
       swap('xhook.BeforeStart(startConsumers, xhook.At(xhook.StageServer))', 'xhook.BeforeStart(startConsumers, xhook.At(xhook.StageClient))'))

section("Producing")
mutate("the trace context is written into the message headers", "xkafka/hook.go", "./xkafka", "TestNew_ProducesAndInjectsTraceContext|TestConsume_TraceContinuesFromProducer",
       swap('\totel.GetTextMapPropagator().Inject(ctx, headerCarrier{r: r})\n', ''))
mutate("it injects even with Trace off", "xkafka/hook.go", "./xkafka", "TestNew_TraceOffStillInjectsContext",
       swap('\t\tr.Context = ctx // the Span is taken back from here once the result arrives\n\t}\n\t// Injected even with Trace off', '\t\tr.Context = ctx // the Span is taken back from here once the result arrives\n\t} else {\n\t\treturn\n\t}\n\t// Injected even with Trace off'))
# A dead letter writes the original message's headers once more: appending would leave two
# traceparents
mutate("a header of the same name is replaced, not appended", "xkafka/hook.go", "./xkafka", "TestHeaderCarrier",
       swap('\t\tif h.Key == key {\n\t\t\tc.r.Headers[i].Value = []byte(value)', '\t\tif false && h.Key == key {\n\t\t\tc.r.Headers[i].Value = []byte(value)'))
mutate("a failed produce is logged at WARN", "xkafka/hook.go", "./xkafka", "TestNew_ProduceFailureLogsWarnWithoutValue",
       swap('slog.WarnContext(r.Context, "kafka produce failed", append(attrs, "error", err)...)', '_ = attrs'))
mutate("with Log off a failed produce is not logged", "xkafka/hook.go", "./xkafka", "TestNew_LogOffSilencesProduceFailures",
       swap('if err != nil && h.log {', 'if err != nil {'))
mutate("franz-go's logs are connected to slog", "xkafka/xkafka.go", "./xkafka", "TestNew_AppliesProducerSettings",
       swap('\t\tkgo.WithLogger(slogLogger{name: name}),\n', ''))

section("Consuming: committing")
# franz-go's default autocommit commits "whatever the last poll fetched", handled or not (measured
# on v1.21.7)
mutate("only what has been handled is committed (AutoCommitMarks)", "xkafka/consume.go", "./xkafka", "TestConsume_MarksOnlyAfterProcessing",
       swap('\t\tkgo.AutoCommitMarks(),\n', ''))
mutate("a record is marked only once it has been handled", "xkafka/worker.go", "./xkafka", "TestConsume_MarksOnlyAfterProcessing|TestConsume_ProcessesAndCommits",
       swap('\tc.cl.MarkCommitRecords(r)\n', ''))
mutate("a record that was not handled is not marked", "xkafka/worker.go", "./xkafka", "TestConsume_MarksOnlyAfterProcessing|TestConsume_DeadLetterFailureBlocksWithoutMarking",
       swap('\tif res == outcomeAborted {\n\t\tc.observe(r.Topic, res, time.Since(start))\n\t\treturn false\n\t}', '\tif false {\n\t}'))
# "Nothing after an unhandled record is handled either": a message is only left unhandled once
# stopping has been cancelled, and every record after it enters xutil.Retry without a single attempt
# and is likewise not marked -- the return inside run is an equivalent mutation, and the promise is
# pinned by TestConsume_DeadLetterFailureBlocksWithoutMarking

section("Consuming: concurrency and order")
mutate("records of one partition are handled one at a time", "xkafka/consume.go", "./xkafka", "TestConsume_OrderedWithinPartition",
       swap('\t\t\tcase w.recs <- p.Records:\n', '\t\t\tcase w.recs <- p.Records[:len(p.Records)/2]:\n\t\t\t\tw2 := newWorker(c, p.Partition)\n\t\t\t\tgo w2.run()\n\t\t\t\tw2.recs <- p.Records[len(p.Records)/2:]\n'))
mutate("one goroutine per partition", "xkafka/consume.go", "./xkafka", "TestConsume_ParallelAcrossPartitions",
       swap('\t\tgo w.run()\n', '\t\tgo func() { c.mu.Lock(); defer c.mu.Unlock(); w.run() }()\n'))

section("Consuming: retries, dead letters, panics, timeouts")
mutate("a failure is retried as WithRetry says", "xkafka/worker.go", "./xkafka", "TestConsume_RetriesThenSucceeds|TestConsume_RetryExhaustedGoesToDeadLetterWithHeaders",
       swap('xutil.Retry(w.stopping, c.o.retries+1, ', 'xutil.Retry(w.stopping, 1, '))
mutate("retries back off by retryInterval", "xkafka/worker.go", "./xkafka", "TestConsume_RetryBackoffWaitsAndStopInterruptsIt",
       swap('c.o.timeout, retryInterval, func(attempt context.Context) error {', 'c.o.timeout, 0, func(attempt context.Context) error {'))
mutate("a retry backoff is no longer waited out on exit", "xkafka/worker.go", "./xkafka", "TestConsume_RetryBackoffWaitsAndStopInterruptsIt",
       swap('xutil.Retry(w.stopping, c.o.retries+1, ', 'xutil.Retry(context.Background(), c.o.retries+1, '))
mutate("every failure is logged at WARN", "xkafka/worker.go", "./xkafka", "TestConsume_RetriesThenSucceeds|TestConsume_LogFields",
       swap('\t\tif last != nil && c.log {\n\t\t\tslog.WarnContext(ctx, "kafka message failed"', '\t\tif false {\n\t\t\tslog.WarnContext(ctx, "kafka message failed"'))
mutate("with Log off the individual failures are not logged", "xkafka/worker.go", "./xkafka", "TestConsume_LogOffKeepsSkipLogs",
       swap('\t\tif last != nil && c.log {\n', '\t\tif last != nil {\n'))
mutate("running out of retries writes a dead letter", "xkafka/worker.go", "./xkafka", "TestConsume_RetryExhaustedGoesToDeadLetterWithHeaders|TestConsume_PermanentErrorSkipsRetries",
       swap('\tif !w.deadLetter(ctx, r, err, fields) {\n\t\treturn outcomeAborted\n\t}\n', ''))
mutate("a dead letter that cannot be written is retried forever", "xkafka/worker.go", "./xkafka", "TestConsume_DeadLetterFailureBlocksWithoutMarking",
       swap('\t\tcase <-w.stopping.Done():\n\t\t\treturn false\n\t\tcase <-time.After(rand.N(wait + 1)):\n\t\t}', '\t\tcase <-w.stopping.Done():\n\t\t\treturn false\n\t\tcase <-time.After(rand.N(wait + 1)):\n\t\t}\n\t\treturn true'))
mutate("a dead letter carries the original message's headers", "xkafka/worker.go", "./xkafka", "TestConsume_RetryExhaustedGoesToDeadLetterWithHeaders|TestDLQRecord",
       swap('\theaders := slices.Clone(r.Headers)\n', '\tvar headers []kgo.RecordHeader\n'))
mutate("a dead letter's error header is truncated", "xkafka/worker.go", "./xkafka", "TestConsume_RetryExhaustedGoesToDeadLetterWithHeaders|TestDLQRecord",
       swap('truncate(strings.ToValidUTF8(cause.Error(), "?"), maxDLQError)', 'strings.ToValidUTF8(cause.Error(), "?")'))
mutate("with dead lettering off it is skipped with an ERROR", "xkafka/worker.go", "./xkafka", "TestConsume_DeadLetterDisabledSkipsWithError",
       swap('\t\tslog.ErrorContext(ctx, "kafka message skipped, retries exhausted and dead lettering is off",', '\t\tslog.DebugContext(ctx, "kafka message skipped, retries exhausted and dead lettering is off",'))
# worker.go recovers in two places (here and in run), so the next line is part of the pattern
mutate("a panic in the handler is caught", "xkafka/worker.go", "./xkafka", "TestConsume_PanicIsRecoveredAndRetried",
       swap('if p := recover(); p != nil {\n\t\t\terr = panicked(p)', 'if p := any(nil); p != nil {\n\t\t\terr = panicked(p)'))
mutate("WithTimeout bounds every single call", "xkafka/worker.go", "./xkafka", "TestConsume_TimeoutCancelsHandlerCtx",
       swap('ctx, cancel = context.WithDeadline(hctx, dl)', 'ctx, cancel = context.WithDeadline(hctx, dl.Add(time.Hour))'))
# The Permanent layer is stripped rather than errors.Unwrap'd: worker.go does not import errors any more
mutate("a Permanent error is not retried", "xkafka/worker.go", "./xkafka", "TestConsume_PermanentErrorSkipsRetries",
       swap('\t\treturn last\n\t})', '\t\tif u, ok := last.(interface{ Unwrap() error }); ok {\n\t\t\treturn u.Unwrap()\n\t\t}\n\t\treturn last\n\t})'))
# The outcome of a message goes by the stopping ctx and never by comparing error values: comparing two
# interface values panics when their dynamic type is not comparable (validator.ValidationErrors, mongo's
# WriteException), which used to kill the process from inside the partition goroutine
mutate("a message is only left unhandled once stopping has been cancelled", "xkafka/worker.go", "./xkafka", "TestConsume_UncomparableHandlerErrorStillDeadLetters|TestConsume_UncomparablePermanentErrorDeadLetters",
       swap('case attempts == 0 || w.stopping.Err() != nil:', 'case attempts == 0 || err != errorValue(w, r):'),
       swap('// call makes one call to the handler.', 'func errorValue(w *worker, r *kgo.Record) error { return nil }\n\n// call makes one call to the handler.'))

section("Consuming: shutdown and rebalancing")
mutate("the shutdown deadline cancels handlers while polling waits for a rebalance", "xkafka/consume.go", "./xkafka", "TestStop_DeadlineCancelsHandlersDuringRebalance",
       swap('\tc.pollCancel()\n', '\tc.pollCancel()\n\t<-c.pollDone\n'))
# Cancelling the ctx of a handler in flight on exit: every write to the database and every call
# downstream inside it would be refused the moment it begins
mutate("the ctx of a handler in flight is not cancelled on exit", "xkafka/worker.go", "./xkafka", "TestStop_WaitsForInFlightAndCommits",
       swap('\tdefer context.AfterFunc(c.hard, cancel)()\n', '\tdefer context.AfterFunc(w0(attempt), cancel)()\n')
       , swap('// panicked turns a recovered value', 'func w0(ctx context.Context) context.Context { return ctx }\n\n// panicked turns a recovered value'))
mutate("a handler in flight is cancelled once the stop budget runs out", "xkafka/worker.go", "./xkafka", "TestStop_DeadlineNamesBusyPartitionsAndCancelsHandler",
       swap('\tdefer context.AfterFunc(c.hard, cancel)()\n', ''))
mutate("the partitions still being handled are named when the stop budget runs out", "xkafka/consume.go", "./xkafka", "TestStop_DeadlineNamesBusyPartitionsAndCancelsHandler",
       swap('\tbusy := c.busy()\n', '\tvar busy []string\n'))
mutate("stopping waits for the messages in flight", "xkafka/consume.go", "./xkafka", "TestStop_WaitsForInFlightAndCommits",
       swap('\tfor _, w := range ws {\n\t\t<-w.done\n\t}\n', ''))
mutate("it commits before partitions are revoked", "xkafka/consume.go", "./xkafka", "TestRebalance_CommitsBeforeRevoke|TestConsume_ProcessesAndCommits|TestStop_WaitsForInFlightAndCommits",
       swap('\tif err := cl.CommitMarkedOffsets(ctx); err != nil {\n\t\tslog.WarnContext(ctx, "kafka commit on partition revoke failed', '\tif err := error(nil); err != nil {\n\t\tslog.WarnContext(ctx, "kafka commit on partition revoke failed'))
mutate("revoking a partition waits for the record in flight", "xkafka/consume.go", "./xkafka", "TestRebalance_WaitsForInFlightBeforeRevoke",
       swap('\tc.kill(revoked[c.topic])\n', '\tgo c.kill(revoked[c.topic])\n'))
# "No queued message starts being handled on exit" is caught by xutil.Retry looking at stopping on
# entry (cancelled, it does not make a single attempt); the stopping check in the run loop only
# saves opening a Span and recording an aborted, so breaking it is an equivalent mutation and is not
# listed here

# A goroutine that exits while still in the partition table leaves poll dispatching into a channel nobody
# reads: it takes workerQueue batches and then blocks for good, which stops every partition of this consumer
mutate("a partition goroutine that has exited leaves the partition table", "xkafka/consume.go", "./xkafka", "TestConsume_PanicInConsumingCodeReleasesThePartition",
       swap('\tif c.assigned[w.partition] == w {\n\t\tdelete(c.assigned, w.partition)\n\t}\n', ''))
mutate("a panic in the consuming code does not take the process down", "xkafka/worker.go", "./xkafka", "TestConsume_PanicInConsumingCodeReleasesThePartition",
       swap('if p := recover(); p != nil {\n\t\t\tslog.ErrorContext(w.c.base,', 'if p := any(nil); p != nil {\n\t\t\tslog.ErrorContext(w.c.base,'))

section("Consuming: registration")
mutate("Consume requires a group", "xkafka/consume.go", "./xkafka", "TestConsume_ValidationErrors",
       swap('\tcase strings.TrimSpace(group) == "":\n', '\tcase group == "":\n'))
mutate("the same topic + group pair cannot be registered twice", "xkafka/consume.go", "./xkafka", "TestConsume_DuplicateIsConfigError",
       swap('if slices.ContainsFunc(m.consumers, c.same) {', 'if false {'))
mutate("a Consume after shutdown has begun reports register", "xkafka/consume.go", "./xkafka", "TestConsume_AfterStopIsRegisterError",
       swap('\tif m.state == stopped {\n\t\treturn xerror.Newf("xkafka", "register"', '\tif false {\n\t\treturn xerror.Newf("xkafka", "register"'))
mutate("a Consume after startup begins on the spot", "xkafka/consume.go", "./xkafka", "TestConsume_AfterStartStartsImmediately",
       swap('\t\tc.launch()\n\t}\n\tm.consumers = append(m.consumers, c)', '\t}\n\tm.consumers = append(m.consumers, c)'))
mutate("WithClient picks the cluster", "xkafka/consume.go", "./xkafka", "TestConsume_WithClientPicksCluster|TestConsume_UnknownClientFailsStart",
       swap('inst, ok := lookup(c.o.client)', 'inst, ok := lookup(DefaultName)'))
mutate("the dead letter topic cannot be the topic itself", "xkafka/option.go", "./xkafka", "TestConsume_ValidationErrors",
       swap('\tif o.dlq == topic {\n', '\tif false {\n'))
mutate("a missing topic is logged at WARN", "xkafka/consume.go", "./xkafka", "TestConsume_MissingTopicWarnsAndWaits",
       swap('\tc.warnIfMissing(ctx)\n', ''))

section("Consuming: observability")
mutate("the trace continues from the producer", "xkafka/worker.go", "./xkafka", "TestConsume_TraceContinuesFromProducer",
       swap('ctx := otel.GetTextMapPropagator().Extract(c.base, headerCarrier{r: r})', 'ctx := c.base'))
mutate("a failed message is marked on the Span", "xkafka/worker.go", "./xkafka", "TestConsume_FailedMessageMarksSpanWithoutErrorText",
       swap('trace.SpanFromContext(ctx).SetStatus(codes.Error, "kafka message failed")', 'trace.SpanFromContext(ctx).SetStatus(codes.Unset, "")'))
mutate("the handler's ctx carries the log fields", "xkafka/worker.go", "./xkafka", "TestConsume_LogFields",
       swap('hctx := logext.WithKV(ctx, map[string]any{', 'hctx := logext.WithKV(ctx, nil)\n\t_ = (map[string]any{'))
mutate("only a printable key is logged", "xkafka/log.go", "./xkafka", "TestKeyAttrs",
       swap('\tif !printable(key) {\n', '\tif false {\n'))
mutate("the consume duration metric", "xkafka/worker.go", "./xkafka", "TestConsume_MetricObserved",
       swap('\tc.cl.MarkCommitRecords(r)\n\tc.observe(r.Topic, res, time.Since(start))\n', '\tc.cl.MarkCommitRecords(r)\n'))
mutate("with Metric off nothing is recorded", "xkafka/metric.go", "./xkafka", "TestConsume_MetricOffRecordsNothing",
       swap('\tif !c.metric {\n', '\tif false {\n'))
# A partition whose goroutine has gone keeps being fetched and dropped, nothing is marked and
# nothing is counted, and it mends itself only on the next rebalance -- which may be days away. One
# line at the moment it happens is the wrong place to say that; without repeating it the state is
# invisible to anybody who looks later. Saying it per batch would be a flood of its own.
mutate("a partition with no goroutine keeps saying so, and not once per batch", "xkafka/consume.go", "./xkafka", "TestConsume_DroppedRecordsKeepSayingSo",
       swap('\tif last, ok := c.lastDropWarn[partition]; ok && now.Sub(last) < dropWarnInterval {\n\t\tc.mu.Unlock()\n\t\treturn\n\t}\n', ''))
