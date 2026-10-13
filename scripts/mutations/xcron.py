# xcron 的变异：module xcron 里的承诺。写法见 scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("解析与校验")
# cronexpr v1.1.3 收 6 / 7 段（年、秒），8 段以上悄悄丢掉；时区前缀它报的是 minute field 的语法错
mutate("spec 只收 5 段", "xcron/schedule.go", "./xcron", "TestParse_RejectsWithClearReason",
       swap('n := len(strings.Fields(spec)); n != 5 {', 'n := len(strings.Fields(spec)); n < 5 {'))
mutate("spec 带时区前缀时指向 WithLocation", "xcron/schedule.go", "./xcron", "TestParse_RejectsWithClearReason",
       swap('if strings.HasPrefix(spec, "CRON_TZ=") || strings.HasPrefix(spec, "TZ=") {', 'if false {'))
mutate("@every 不收 0 和负数", "xcron/schedule.go", "./xcron", "TestParse_RejectsWithClearReason",
       swap('\t\tif d <= 0 {\n', '\t\tif false {\n'))
# 2 月 30 日解析成功、Next 返回零值：不拦的话任务登记上了却一次都不跑
mutate("永远不会到的 spec 报错", "xcron/schedule.go", "./xcron", "TestParse_RejectsWithClearReason",
       swap('if e.Next(time.Now()).IsZero() {', 'if false {'))
mutate("名字重复报错", "xcron/scheduler.go", "./xcron", "TestAdd_DuplicateNameIsConfigError",
       swap('if slices.ContainsFunc(s.jobs, func(x *job) bool { return x.name == j.name }) {', 'if false {'))
mutate("不写名字就取函数名", "xcron/option.go", "./xcron", "TestAdd_DuplicateNameIsConfigError",
       swap('\tif !o.named {\n', '\tif false {\n'))
mutate("退出开始之后 Add 报错", "xcron/scheduler.go", "./xcron", "TestAdd_AfterStopIsRegisterError",
       swap('\tif s.state == stopped {\n\t\ts.mu.Unlock()\n\t\treturn xerror.Newf("xcron", "register"',
            '\tif false {\n\t\ts.mu.Unlock()\n\t\treturn xerror.Newf("xcron", "register"'))

section("时区")
mutate("默认时区是 UTC", "xcron/option.go", "./xcron", "TestOptions_DefaultLocationIsUTC",
       swap('options{loc: time.UTC}', 'options{loc: time.Local}'))
mutate("按 WithLocation 的时区算下一个时间点", "xcron/schedule.go", "./xcron", "TestNext_UsesLocation",
       swap('return s.Next(from.In(loc))', 'return s.Next(from)'))
# 打在调用点上：next 本身是对的，调度循环不把任务的时区传进去也一样错
mutate("调度循环用的是任务的时区", "xcron/scheduler.go", "./xcron", "TestScheduler_PassesLocationToSchedule",
       swap('at := next(j.sched, j.o.loc, prev)', 'at := next(j.sched, time.UTC, prev)'))
# NTP 往回拨时钟时，按 time.Now() 算会把刚跑过的时间点再跑一次
mutate("时钟往回拨不重复跑", "xcron/schedule.go", "./xcron", "TestNext_UsesLocationAndNeverRepeatsAfterClockStepsBack",
       swap('\tif from.Before(prev) {\n', '\tif false {\n'))

section("每次执行")
mutate("每次执行一个根 Span", "xcron/run.go", "./xcron", "TestExecute_InjectsRootSpanAndJobField",
       swap('ctx, span := otel.Tracer(tracerName).Start(parent, "cron "+name, trace.WithSpanKind(trace.SpanKindInternal))',
            '_ = otel.Tracer\n\tctx, span := parent, trace.SpanFromContext(parent)'))
mutate("每次执行的日志带 job 字段", "xcron/run.go", "./xcron", "TestExecute_InjectsRootSpanAndJobField",
       swap('ctx = xlog.CtxWithKV(ctx, map[string]any{"job": name})', 'ctx = xlog.CtxWithKV(ctx, nil)'))
mutate("调度起来的执行带的是任务名", "xcron/scheduler.go", "./xcron", "TestScheduler_RunsOnSchedule",
       swap('return execute(lt.ctx, j.name, j.o.timeout, j.fn)', 'return execute(lt.ctx, "", j.o.timeout, j.fn)'))
mutate("失败的执行标在 Span 上", "xcron/run.go", "./xcron", "TestExecute_FailureLogsWarnAndMarksSpan",
       swap('\t\t\tspan.SetStatus(codes.Error, "cron job failed")\n', ''))
# panic 不接住的话整个进程跟着一个任务一起死
mutate("任务 panic 被接住", "xcron/run.go", "./xcron", "TestExecute_RecoversPanic|TestScheduler_PanicDoesNotStop",
       swap('if r := recover(); r != nil {', 'if r := any(nil); r != nil {'))
mutate("WithTimeout 取消 ctx", "xcron/run.go", "./xcron", "TestExecute_TimeoutCancelsCtx",
       swap('\tif timeout > 0 {\n', '\tif false {\n'))
mutate("启动时的第一次也受 WithTimeout 管", "xcron/scheduler.go", "./xcron", "TestTimeout_AppliesToFirstRuns",
       swap('execute(ctx, j.name, j.o.timeout, j.fn); err != nil {', 'execute(ctx, j.name, 0, j.fn); err != nil {'))

section("调度")
mutate("上一次没跑完就跳过", "xcron/scheduler.go", "./xcron", "TestScheduler_SkipsOverlappingRunAndWarns",
       swap('\tif !j.o.overlap && j.running.Load() > 0 {\n', '\tif false {\n'))
mutate("AllowOverlap 允许重叠", "xcron/scheduler.go", "./xcron", "TestScheduler_AllowOverlapRunsConcurrently",
       swap('\tif !j.o.overlap && j.running.Load() > 0 {\n', '\tif j.running.Load() > 0 {\n'))
mutate("RunOnStart 起来就跑一次", "xcron/scheduler.go", "./xcron", "TestStart_RunOnStartRunsNow",
       swap('lt.schedule(j, j.o.first == firstAsync) // RunOnStartAndWait', 'lt.schedule(j, false) // RunOnStartAndWait'))
mutate("起来之后 Add 的当场开始调度", "xcron/scheduler.go", "./xcron", "TestAdd_AfterStartSchedulesImmediately",
       swap('\t\tlt.schedule(j, j.o.first == firstAsync)\n\t\ts.mu.Unlock()\n', '\t\ts.mu.Unlock()\n'))
mutate("RunOnStartAndWait 的第一次失败让启动失败", "xcron/scheduler.go", "./xcron", "TestStart_RunOnStartAndWaitFailureFailsStartup",
       swap('if err := firstRuns(ctx, jobs); err != nil {', 'if err := firstRuns(ctx, jobs); false && err != nil {'))
mutate("RunOnStartAndWait 的第一次并行跑、全部等完", "xcron/scheduler.go", "./xcron", "TestStart_RunOnStartAndWait",
       swap('\twg.Wait()\n\tif err := errors.Join(errs...); err != nil {', '\tif err := errors.Join(errs...); err != nil {'))
# 第一次执行收启动钩子的 ctx：启动期间的退出信号要能叫停它
mutate("RunOnStartAndWait 的第一次听退出信号", "xcron/scheduler.go", "./xcron", "TestStart_RunOnStartAndWaitHonorsStartCtx",
       swap('execute(ctx, j.name, j.o.timeout, j.fn); err != nil {', 'execute(context.WithoutCancel(ctx), j.name, j.o.timeout, j.fn); err != nil {'))
mutate("调度器在 StageServer 起来", "xcron/xcron.go", "./xcron", "TestHooks_StartAtServerStage",
       swap('xhook.At(xhook.StageServer)', 'xhook.At(xhook.StageBusiness)'))

section("退出")
mutate("停的时候取消在途执行的 ctx", "xcron/scheduler.go", "./xcron", "TestStop_CancelsInFlightAndWaitsForThem",
       swap('\tlt.cancel()\n\tdone := make(chan struct{})\n', '\tdone := make(chan struct{})\n'))
mutate("停的时候等在途执行返回", "xcron/scheduler.go", "./xcron", "TestStop_CancelsInFlightAndWaitsForThem",
       swap('go func() { lt.wg.Wait(); close(done) }()', 'close(done)'))
# 打在调用点上：发起执行时不记进 wg，stop 就不知道还有人在跑
mutate("到点发起的执行算在途", "xcron/scheduler.go", "./xcron", "TestStop_CancelsInFlightAndWaitsForThem",
       swap('\tlt.begin(j)\n\tgo lt.exec(j)\n', '\tgo execute(lt.ctx, j.name, j.o.timeout, j.fn)\n'))
mutate("停了之后调度循环退出", "xcron/scheduler.go", "./xcron", "TestStop_NoNewRunsAfterStop",
       swap('\t\tcase <-lt.ctx.Done():\n\t\t\tt.Stop()\n', '\t\tcase <-make(chan struct{}):\n\t\t\tt.Stop()\n'))
mutate("停止超时点名还在跑的任务", "xcron/scheduler.go", "./xcron", "TestStop_DeadlineNamesStuckJobs",
       swap('\tbusy := s.busy()\n', '\tvar busy []string\n'))
