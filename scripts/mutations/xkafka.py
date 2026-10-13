# xkafka 的变异：module xkafka 里的承诺。写法见 scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("配置")
mutate("xkafka 多实例铺的是自己的默认值", "xkafka/config.go", "./xkafka", "TestConfig_SingleClusterFormKeepsDefaults|TestConfig_MultiClusterForm",
       swap('xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)', 'xconfig.UnmarshalClients(ConfigKey, func() ClientConfig { return ClientConfig{} })'))
# franz-go 的默认是 earliest：新组上线把 topic 里的全部历史处理一遍
mutate("新消费组默认从 latest 开始", "xkafka/config.go", "./xkafka", "TestResetOffset_DefaultIsLatest",
       swap('Consumer: ConsumerConfig{ResetOffset: "latest"},', 'Consumer: ConsumerConfig{ResetOffset: "earliest"},'))
mutate("ResetOffset 交给了消费者的客户端", "xkafka/consume.go", "./xkafka", "TestResetOffset_LatestSkipsOldRecords",
       swap('kgo.ConsumeResetOffset(resetOffset(inst.cfg.Consumer.ResetOffset)),', 'kgo.ConsumeResetOffset(resetOffset("earliest")),'))
mutate("Acks 配 leader 时替使用者关掉幂等写", "xkafka/xkafka.go", "./xkafka", "TestNew_AppliesProducerSettings",
       swap('opts = append(opts, kgo.RequiredAcks(kgo.LeaderAck()), kgo.DisableIdempotentWrite())', 'opts = append(opts, kgo.DisableIdempotentWrite())'))
mutate("Linger 显式交给 franz-go（0 就是不等）", "xkafka/xkafka.go", "./xkafka", "TestNew_AppliesProducerSettings",
       swap('\t\tkgo.ProducerLinger(cfg.Producer.Linger),\n', ''))
mutate("DialTimeout 交给 franz-go", "xkafka/xkafka.go", "./xkafka", "TestNew_AppliesProducerSettings",
       swap('\t\tkgo.DialTimeout(cfg.DialTimeout),\n', ''))
mutate("ClientID 没配时用 XApp.Name", "xkafka/xkafka.go", "./xkafka", "TestBuild_ClientIDDefaultsToAppName",
       swap('c.ClientID = xapp.Name()', 'c.ClientID = xapp.Version()'))
mutate("SASL 的密码不进校验的报错", "xkafka/config.go", "./xkafka", "TestConfig_Validate",
       swap('if s.Username == "" || s.Password == "" {', 'if s.Username == "" {'))

section("启动与退出")
mutate("xkafka 没配也让注册表知道启动过了", "xkafka/xkafka.go", "./xkafka", "TestInitXKafka_CSaysUnconfiguredNotTooEarly",
       swap('\t\treturn xclient.Build(ctx, reg, nil, build)\n', '\t\treturn nil\n'))
# franz-go v1.21.7 的 Ping 在对端不回话时不听 ctx，等满 RequestTimeoutOverhead（10s）。打在调用点上
mutate("探测不等一个不回话的 broker", "xkafka/xkafka.go", "./xkafka", "TestNew_SilentBrokerBoundedByDialTimeout|TestNew_ExitSignalAbortsProbe",
       swap('xclient.Probe(ctx, probePolicy(cfg), probe(client))', 'xclient.Probe(ctx, probePolicy(cfg), client.Ping)'))
mutate("认证失败不重试", "xkafka/xkafka.go", "./xkafka", "TestAuthFailed",
       swap('\t\tAuthFailed: authFailed,\n', ''))
mutate("连不上的报错点名地址", "xkafka/xkafka.go", "./xkafka", "TestNew_UnreachableBrokerFailsWithAddress|TestNew_SilentBrokerBoundedByDialTimeout",
       swap('"cannot reach %s: %w", addrs, err)', '"cannot reach brokers: %w", err)'))
# Close 让缓冲里的消息全部失败（v1.21.7 实测 10 条丢 10 条）
mutate("停止钩子先 Flush 再 Close", "xkafka/xkafka.go", "./xkafka", "TestCloseXKafka_FlushesBeforeClose",
       swap('if err := inst.client.Flush(ctx); err != nil {', 'if err := error(nil); err != nil && inst.client != nil {'))
mutate("客户端在 StageClient、消费者在 StageServer", "xkafka/xkafka.go", "./xkafka", "TestHooks_Stages",
       swap('xhook.BeforeStart(startConsumers, xhook.At(xhook.StageServer))', 'xhook.BeforeStart(startConsumers, xhook.At(xhook.StageClient))'))

section("生产")
mutate("链路上下文写进消息头", "xkafka/hook.go", "./xkafka", "TestNew_ProducesAndInjectsTraceContext|TestConsume_TraceContinuesFromProducer",
       swap('\totel.GetTextMapPropagator().Inject(ctx, headerCarrier{r: r})\n', ''))
mutate("Trace 关着也注入", "xkafka/hook.go", "./xkafka", "TestNew_TraceOffStillInjectsContext",
       swap('\t\tr.Context = ctx // 结果出来时从这里取回 Span\n\t}\n\t// Trace 关着也注入', '\t\tr.Context = ctx // 结果出来时从这里取回 Span\n\t} else {\n\t\treturn\n\t}\n\t// Trace 关着也注入'))
# 死信带着原消息的头再写一次：追加的话有两个 traceparent
mutate("消息头同名覆盖不追加", "xkafka/hook.go", "./xkafka", "TestHeaderCarrier",
       swap('\t\tif h.Key == key {\n\t\t\tc.r.Headers[i].Value = []byte(value)', '\t\tif false && h.Key == key {\n\t\t\tc.r.Headers[i].Value = []byte(value)'))
mutate("生产失败记 WARN", "xkafka/hook.go", "./xkafka", "TestNew_ProduceFailureLogsWarnWithoutValue",
       swap('slog.WarnContext(r.Context, "kafka produce failed", append(attrs, "error", err)...)', '_ = attrs'))
mutate("Log 关掉时不记生产失败", "xkafka/hook.go", "./xkafka", "TestNew_LogOffSilencesProduceFailures",
       swap('if err != nil && h.log {', 'if err != nil {'))
mutate("franz-go 的日志接到 slog", "xkafka/xkafka.go", "./xkafka", "TestNew_AppliesProducerSettings",
       swap('\t\tkgo.WithLogger(slogLogger{name: name}),\n', ''))

section("消费：提交")
# franz-go 默认的自动提交提交「上一次 poll 取到的」，不管处理完没有（v1.21.7 实测）
mutate("只提交处理完的（AutoCommitMarks）", "xkafka/consume.go", "./xkafka", "TestConsume_MarksOnlyAfterProcessing",
       swap('\t\tkgo.AutoCommitMarks(),\n', ''))
mutate("处理完才标记", "xkafka/worker.go", "./xkafka", "TestConsume_MarksOnlyAfterProcessing|TestConsume_ProcessesAndCommits",
       swap('\tc.cl.MarkCommitRecords(r)\n', ''))
mutate("没处理完的不标记", "xkafka/worker.go", "./xkafka", "TestConsume_MarksOnlyAfterProcessing|TestConsume_DeadLetterFailureBlocksWithoutMarking",
       swap('\tif res == outcomeAborted {\n\t\tc.observe(r.Topic, res, time.Since(start))\n\t\treturn false\n\t}', '\tif false {\n\t}'))
# 「没处理完的那条之后的也不处理」：一条消息只在 stopping 取消之后才会没处理完，之后的每一条
# 进 xutil.Retry 都一次不试、同样不标记——run 里的 return 是等价变异，承诺由 TestConsume_DeadLetterFailureBlocksWithoutMarking 钉着

section("消费：并发与顺序")
mutate("同一分区里一条一条处理", "xkafka/consume.go", "./xkafka", "TestConsume_OrderedWithinPartition",
       swap('\t\t\tcase w.recs <- p.Records:\n', '\t\t\tcase w.recs <- p.Records[:len(p.Records)/2]:\n\t\t\t\tw2 := newWorker(c, p.Partition)\n\t\t\t\tgo w2.run()\n\t\t\t\tw2.recs <- p.Records[len(p.Records)/2:]\n'))
mutate("每个分区一个协程", "xkafka/consume.go", "./xkafka", "TestConsume_ParallelAcrossPartitions",
       swap('\t\tgo w.run()\n', '\t\tgo func() { c.mu.Lock(); defer c.mu.Unlock(); w.run() }()\n'))

section("消费：重试、死信、panic、超时")
mutate("失败了按 WithRetry 重试", "xkafka/worker.go", "./xkafka", "TestConsume_RetriesThenSucceeds|TestConsume_RetryExhaustedGoesToDeadLetterWithHeaders",
       swap('xutil.Retry(w.stopping, c.o.retries+1, ', 'xutil.Retry(w.stopping, 1, '))
mutate("重试之间按 retryInterval 退避", "xkafka/worker.go", "./xkafka", "TestConsume_RetryBackoffWaitsAndStopInterruptsIt",
       swap('c.o.timeout, retryInterval, func(attempt context.Context) error {', 'c.o.timeout, 0, func(attempt context.Context) error {'))
mutate("退出时不再等重试的退避", "xkafka/worker.go", "./xkafka", "TestConsume_RetryBackoffWaitsAndStopInterruptsIt",
       swap('xutil.Retry(w.stopping, c.o.retries+1, ', 'xutil.Retry(context.Background(), c.o.retries+1, '))
mutate("每次失败记 WARN", "xkafka/worker.go", "./xkafka", "TestConsume_RetriesThenSucceeds|TestConsume_LogFields",
       swap('\t\tif last != nil && c.log {\n\t\t\tslog.WarnContext(ctx, "kafka message failed"', '\t\tif false {\n\t\t\tslog.WarnContext(ctx, "kafka message failed"'))
mutate("Log 关掉时不记每次失败", "xkafka/worker.go", "./xkafka", "TestConsume_LogOffKeepsSkipLogs",
       swap('\t\tif last != nil && c.log {\n', '\t\tif last != nil {\n'))
mutate("重试用完写死信", "xkafka/worker.go", "./xkafka", "TestConsume_RetryExhaustedGoesToDeadLetterWithHeaders|TestConsume_PermanentErrorSkipsRetries",
       swap('\tif !w.deadLetter(ctx, r, err, fields) {\n\t\treturn outcomeAborted\n\t}\n', ''))
mutate("死信写不进去一直重试", "xkafka/worker.go", "./xkafka", "TestConsume_DeadLetterFailureBlocksWithoutMarking",
       swap('\t\tcase <-w.stopping.Done():\n\t\t\treturn false\n\t\tcase <-time.After(rand.N(wait + 1)):\n\t\t}', '\t\tcase <-w.stopping.Done():\n\t\t\treturn false\n\t\tcase <-time.After(rand.N(wait + 1)):\n\t\t}\n\t\treturn true'))
mutate("死信带原消息的头", "xkafka/worker.go", "./xkafka", "TestConsume_RetryExhaustedGoesToDeadLetterWithHeaders|TestDLQRecord",
       swap('\theaders := slices.Clone(r.Headers)\n', '\tvar headers []kgo.RecordHeader\n'))
mutate("死信的错误头截短", "xkafka/worker.go", "./xkafka", "TestConsume_RetryExhaustedGoesToDeadLetterWithHeaders|TestDLQRecord",
       swap('truncate(strings.ToValidUTF8(cause.Error(), "?"), maxDLQError)', 'strings.ToValidUTF8(cause.Error(), "?")'))
mutate("死信关掉时记 ERROR 跳过", "xkafka/worker.go", "./xkafka", "TestConsume_DeadLetterDisabledSkipsWithError",
       swap('\t\tslog.ErrorContext(ctx, "kafka message skipped, retries exhausted and dead lettering is off",', '\t\tslog.DebugContext(ctx, "kafka message skipped, retries exhausted and dead lettering is off",'))
mutate("处理函数 panic 被接住", "xkafka/worker.go", "./xkafka", "TestConsume_PanicIsRecoveredAndRetried",
       swap('if p := recover(); p != nil {', 'if p := any(nil); p != nil {'))
mutate("WithTimeout 管住每一次调用", "xkafka/worker.go", "./xkafka", "TestConsume_TimeoutCancelsHandlerCtx",
       swap('ctx, cancel = context.WithDeadline(hctx, dl)', 'ctx, cancel = context.WithDeadline(hctx, dl.Add(time.Hour))'))
mutate("Permanent 不重试", "xkafka/worker.go", "./xkafka", "TestConsume_PermanentErrorSkipsRetries",
       swap('\t\treturn last\n\t})', '\t\tif errors.Unwrap(last) != nil {\n\t\t\treturn errors.Unwrap(last)\n\t\t}\n\t\treturn last\n\t})'))

section("消费：退出与再均衡")
# 退出时取消在途处理函数的 ctx：它里面的每一次写库、调下游都会一进去就被拒绝
mutate("退出时不取消在途处理函数的 ctx", "xkafka/worker.go", "./xkafka", "TestStop_WaitsForInFlightAndCommits",
       swap('\tdefer context.AfterFunc(c.hard, cancel)()\n', '\tdefer context.AfterFunc(w0(attempt), cancel)()\n')
       , swap('// panicked 把 recover', 'func w0(ctx context.Context) context.Context { return ctx }\n\n// panicked 把 recover'))
mutate("停止预算用完时取消在途处理函数", "xkafka/worker.go", "./xkafka", "TestStop_DeadlineNamesBusyPartitionsAndCancelsHandler",
       swap('\tdefer context.AfterFunc(c.hard, cancel)()\n', ''))
mutate("停止预算用完时点名还在处理的分区", "xkafka/consume.go", "./xkafka", "TestStop_DeadlineNamesBusyPartitionsAndCancelsHandler",
       swap('\tbusy := c.busy()\n', '\tvar busy []string\n'))
mutate("停的时候等在途的消息", "xkafka/consume.go", "./xkafka", "TestStop_WaitsForInFlightAndCommits",
       swap('\tfor _, w := range ws {\n\t\t<-w.done\n\t}\n', ''))
mutate("收回分区之前提交", "xkafka/consume.go", "./xkafka", "TestRebalance_CommitsBeforeRevoke|TestConsume_ProcessesAndCommits|TestStop_WaitsForInFlightAndCommits",
       swap('\tif err := cl.CommitMarkedOffsets(ctx); err != nil {\n\t\tslog.WarnContext(ctx, "kafka commit on partition revoke failed', '\tif err := error(nil); err != nil {\n\t\tslog.WarnContext(ctx, "kafka commit on partition revoke failed'))
mutate("收回分区时等在途的那一条", "xkafka/consume.go", "./xkafka", "TestRebalance_WaitsForInFlightBeforeRevoke",
       swap('\tc.kill(revoked[c.topic])\n', '\tgo c.kill(revoked[c.topic])\n'))
# 「退出时排着的消息不再开始处理」由 xutil.Retry 进门先看 stopping 兜着（取消了一次都不试），
# run 循环里那一句 stopping 检查只是少开一个 Span、少记一个 aborted；改掉它是等价变异，不在这里列

section("消费：登记")
mutate("Consume 的 group 必填", "xkafka/consume.go", "./xkafka", "TestConsume_ValidationErrors",
       swap('\tcase strings.TrimSpace(group) == "":\n', '\tcase group == "":\n'))
mutate("同一对 topic + group 不能登记两次", "xkafka/consume.go", "./xkafka", "TestConsume_DuplicateIsConfigError",
       swap('if slices.ContainsFunc(m.consumers, c.same) {', 'if false {'))
mutate("退出开始之后 Consume 报 register", "xkafka/consume.go", "./xkafka", "TestConsume_AfterStopIsRegisterError",
       swap('\tif m.state == stopped {\n\t\treturn xerror.Newf("xkafka", "register"', '\tif false {\n\t\treturn xerror.Newf("xkafka", "register"'))
mutate("起来之后 Consume 的当场开始", "xkafka/consume.go", "./xkafka", "TestConsume_AfterStartStartsImmediately",
       swap('\t\tc.launch()\n\t}\n\tm.consumers = append(m.consumers, c)', '\t}\n\tm.consumers = append(m.consumers, c)'))
mutate("WithClient 选集群", "xkafka/consume.go", "./xkafka", "TestConsume_WithClientPicksCluster|TestConsume_UnknownClientFailsStart",
       swap('inst, ok := lookup(c.o.client)', 'inst, ok := lookup(DefaultName)'))
mutate("死信 topic 不能是自己", "xkafka/option.go", "./xkafka", "TestConsume_ValidationErrors",
       swap('\tif o.dlq == topic {\n', '\tif false {\n'))
mutate("topic 不存在时记 WARN", "xkafka/consume.go", "./xkafka", "TestConsume_MissingTopicWarnsAndWaits",
       swap('\tc.warnIfMissing(ctx)\n', ''))

section("消费：可观测")
mutate("链路接着生产方", "xkafka/worker.go", "./xkafka", "TestConsume_TraceContinuesFromProducer",
       swap('ctx := otel.GetTextMapPropagator().Extract(c.base, headerCarrier{r: r})', 'ctx := c.base'))
mutate("失败的消息标在 Span 上", "xkafka/worker.go", "./xkafka", "TestConsume_FailedMessageMarksSpanWithoutErrorText",
       swap('trace.SpanFromContext(ctx).SetStatus(codes.Error, "kafka message failed")', 'trace.SpanFromContext(ctx).SetStatus(codes.Unset, "")'))
mutate("处理函数的 ctx 带着日志字段", "xkafka/worker.go", "./xkafka", "TestConsume_LogFields",
       swap('hctx := logext.WithKV(ctx, map[string]any{', 'hctx := logext.WithKV(ctx, nil)\n\t_ = (map[string]any{'))
mutate("日志里的 key 只记可打印的", "xkafka/log.go", "./xkafka", "TestKeyAttrs",
       swap('\tif !printable(key) {\n', '\tif false {\n'))
mutate("消费耗时指标", "xkafka/worker.go", "./xkafka", "TestConsume_MetricObserved",
       swap('\tc.cl.MarkCommitRecords(r)\n\tc.observe(r.Topic, res, time.Since(start))\n', '\tc.cl.MarkCommitRecords(r)\n'))
mutate("Metric 关掉不记", "xkafka/metric.go", "./xkafka", "TestConsume_MetricOffRecordsNothing",
       swap('\tif !c.metric {\n', '\tif false {\n'))
