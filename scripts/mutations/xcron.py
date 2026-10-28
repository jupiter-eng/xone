# The xcron mutations: the promises of the xcron module. The format is in
# scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("Parsing and validation")
# cronexpr v1.1.3 accepts 6 or 7 fields (a year and seconds) and quietly drops anything beyond 8;
# for a timezone prefix it reports a syntax error in the minute field
mutate("a spec takes 5 fields only", "xcron/schedule.go", "./xcron", "TestParse_RejectsWithClearReason",
       swap('n := len(strings.Fields(spec)); n != 5 {', 'n := len(strings.Fields(spec)); n < 5 {'))
mutate("a spec with a timezone prefix points at WithLocation", "xcron/schedule.go", "./xcron", "TestParse_RejectsWithClearReason",
       swap('if strings.HasPrefix(spec, "CRON_TZ=") || strings.HasPrefix(spec, "TZ=") {', 'if false {'))
mutate("@every takes neither 0 nor a negative value", "xcron/schedule.go", "./xcron", "TestParse_RejectsWithClearReason",
       swap('\t\tif d <= 0 {\n', '\t\tif false {\n'))
# February 30th parses successfully and Next returns the zero value: uncaught, the job is
# registered and never runs once
mutate("a spec that never comes round is an error", "xcron/schedule.go", "./xcron", "TestParse_RejectsWithClearReason",
       swap('if e.Next(time.Now()).IsZero() {', 'if false {'))
mutate("a duplicate name is an error", "xcron/scheduler.go", "./xcron", "TestAdd_DuplicateNameIsConfigError",
       swap('if slices.ContainsFunc(s.jobs, func(x *job) bool { return x.name == j.name }) {', 'if false {'))
mutate("an unnamed job takes the function's name", "xcron/option.go", "./xcron", "TestAdd_DuplicateNameIsConfigError",
       swap('\tif !o.named {\n', '\tif false {\n'))
mutate("an Add after shutdown has begun is an error", "xcron/scheduler.go", "./xcron", "TestAdd_AfterStopIsRegisterError",
       swap('\tif s.state == stopped {\n\t\ts.mu.Unlock()\n\t\treturn xerror.Newf("xcron", "register"',
            '\tif false {\n\t\ts.mu.Unlock()\n\t\treturn xerror.Newf("xcron", "register"'))

section("Timezones")
mutate("the default timezone is UTC", "xcron/option.go", "./xcron", "TestOptions_DefaultLocationIsUTC",
       swap('options{loc: time.UTC}', 'options{loc: time.Local}'))
mutate("the next time is computed in WithLocation's timezone", "xcron/schedule.go", "./xcron", "TestNext_UsesLocation",
       swap('return s.Next(from.In(loc))', 'return s.Next(from)'))
# Aimed at the call site: next itself is right, and the scheduling loop not passing the job's
# timezone in is just as wrong
mutate("the scheduling loop uses the job's timezone", "xcron/scheduler.go", "./xcron", "TestScheduler_PassesLocationToSchedule",
       swap('at := next(j.sched, j.o.loc, prev)', 'at := next(j.sched, time.UTC, prev)'))
# When NTP steps the clock back, going by time.Now() runs the time that just ran once more
mutate("a clock stepping back does not run the same time twice", "xcron/schedule.go", "./xcron", "TestNext_UsesLocationAndNeverRepeatsAfterClockStepsBack",
       swap('\tif from.Before(prev) {\n', '\tif false {\n'))

section("Each run")
mutate("each run gets a root Span", "xcron/run.go", "./xcron", "TestExecute_InjectsRootSpanAndJobField",
       swap('ctx, span := otel.Tracer(tracerName).Start(parent, "cron "+name, trace.WithSpanKind(trace.SpanKindInternal))',
            '_ = otel.Tracer\n\tctx, span := parent, trace.SpanFromContext(parent)'))
mutate("each run's logs carry the job field", "xcron/run.go", "./xcron", "TestExecute_InjectsRootSpanAndJobField",
       swap('ctx = xlog.CtxWithKV(ctx, map[string]any{"job": name})', 'ctx = xlog.CtxWithKV(ctx, nil)'))
mutate("a scheduled run carries the job's name", "xcron/scheduler.go", "./xcron", "TestScheduler_RunsOnSchedule",
       swap('return execute(lt.ctx, j.name, j.o.timeout, j.fn)', 'return execute(lt.ctx, "", j.o.timeout, j.fn)'))
mutate("a failed run is marked on the Span", "xcron/run.go", "./xcron", "TestExecute_FailureLogsWarnAndMarksSpan",
       swap('\t\t\tspan.SetStatus(codes.Error, "cron job failed")\n', ''))
# An uncaught panic takes the whole process down along with one job
mutate("a panic in a job is caught", "xcron/run.go", "./xcron", "TestExecute_RecoversPanic|TestScheduler_PanicDoesNotStop",
       swap('if r := recover(); r != nil {', 'if r := any(nil); r != nil {'))
mutate("WithTimeout cancels the ctx", "xcron/run.go", "./xcron", "TestExecute_TimeoutCancelsCtx",
       swap('\tif timeout > 0 {\n', '\tif false {\n'))
mutate("the first run at startup is governed by WithTimeout too", "xcron/scheduler.go", "./xcron", "TestTimeout_AppliesToFirstRuns",
       swap('execute(ctx, j.name, j.o.timeout, j.fn); err != nil {', 'execute(ctx, j.name, 0, j.fn); err != nil {'))

section("Scheduling")
mutate("a run is skipped while the previous one is unfinished", "xcron/scheduler.go", "./xcron", "TestScheduler_SkipsOverlappingRunAndWarns",
       swap('\tif !j.o.overlap && j.running.Load() > 0 {\n', '\tif false {\n'))
mutate("AllowOverlap allows them to overlap", "xcron/scheduler.go", "./xcron", "TestScheduler_AllowOverlapRunsConcurrently",
       swap('\tif !j.o.overlap && j.running.Load() > 0 {\n', '\tif j.running.Load() > 0 {\n'))
mutate("RunOnStart runs once as soon as it is up", "xcron/scheduler.go", "./xcron", "TestStart_RunOnStartRunsNow",
       swap('lt.schedule(j, j.o.first == firstAsync) // the first run of a RunOnStartAndWait', 'lt.schedule(j, false) // the first run of a RunOnStartAndWait'))
mutate("an Add after startup is scheduled on the spot", "xcron/scheduler.go", "./xcron", "TestAdd_AfterStartSchedulesImmediately",
       swap('\t\tlt.schedule(j, j.o.first == firstAsync)\n\t\ts.mu.Unlock()\n', '\t\ts.mu.Unlock()\n'))
mutate("a failed first run of RunOnStartAndWait fails startup", "xcron/scheduler.go", "./xcron", "TestStart_RunOnStartAndWaitFailureFailsStartup",
       swap('if err := firstRuns(ctx, jobs); err != nil {', 'if err := firstRuns(ctx, jobs); false && err != nil {'))
mutate("the first runs of RunOnStartAndWait run in parallel and are all waited for", "xcron/scheduler.go", "./xcron", "TestStart_RunOnStartAndWait",
       swap('\twg.Wait()\n\tif err := errors.Join(errs...); err != nil {', '\tif err := errors.Join(errs...); err != nil {'))
# The first run takes the start hook's ctx: a shutdown signal during startup has to be able to
# stop it
mutate("the first run of RunOnStartAndWait honours the shutdown signal", "xcron/scheduler.go", "./xcron", "TestStart_RunOnStartAndWaitHonorsStartCtx",
       swap('execute(ctx, j.name, j.o.timeout, j.fn); err != nil {', 'execute(context.WithoutCancel(ctx), j.name, j.o.timeout, j.fn); err != nil {'))
# The zero time means there is no next time point, and the wait to it is a large negative duration:
# the timer fires at once, the next round works out the same zero time, and the loop burns a core.
# cronexpr searches the years 1970..2099, which "0 0 29 2 *" runs past in 2096
mutate("a job with no next time point gives up instead of spinning", "xcron/scheduler.go", "./xcron", "TestScheduler_NoNextTimePointStopsTheLoop",
       cut('\t\tif at.IsZero() {', '"job", j.name)\n\t\t\treturn\n\t\t}\n'))
mutate("the scheduler comes up at StageServer", "xcron/xcron.go", "./xcron", "TestHooks_StartAtServerStage",
       swap('xhook.At(xhook.StageServer)', 'xhook.At(xhook.StageBusiness)'))

section("Shutdown")
mutate("a first run added during startup follows startup cancellation", "xcron/scheduler.go", "./xcron", "TestStart_DynamicallyAddedFirstRunHonorsStartCtx",
       swap('context.AfterFunc(ctx, lt.cancel)', 'context.AfterFunc(context.WithoutCancel(ctx), lt.cancel)'))
mutate("completed startup leaves job cancellation to the stop hook", "xcron/scheduler.go", "./xcron", "TestStart_CompletedStartupLeavesCancellationToStop",
       swap('\tdefer detachStartup()\n', '\t_ = detachStartup\n'))
mutate("failed startup waits for dynamically added jobs to finish", "xcron/scheduler.go", "./xcron", "TestStart_FailureWaitsForDynamicallyAddedJobs",
       swap('\t\tlt.wg.Wait()\n\t\treturn err', '\t\treturn err'))
mutate("stopping cancels the ctx of the runs in flight", "xcron/scheduler.go", "./xcron", "TestStop_CancelsInFlightAndWaitsForThem",
       swap('\tlt.cancel()\n\tdone := make(chan struct{})\n', '\tdone := make(chan struct{})\n'))
mutate("stopping waits for the runs in flight to return", "xcron/scheduler.go", "./xcron", "TestStop_CancelsInFlightAndWaitsForThem",
       swap('go func() { lt.wg.Wait(); close(done) }()', 'close(done)'))
# Aimed at the call site: without recording a run into the wg when it begins, stop does not know
# anybody is still running
mutate("a run started at its time counts as in flight", "xcron/scheduler.go", "./xcron", "TestStop_CancelsInFlightAndWaitsForThem",
       swap('\tlt.begin(j)\n\tgo lt.exec(j)\n', '\tgo execute(lt.ctx, j.name, j.o.timeout, j.fn)\n'))
mutate("the scheduling loop exits once stopped", "xcron/scheduler.go", "./xcron", "TestStop_NoNewRunsAfterStop",
       swap('\t\tcase <-lt.ctx.Done():\n\t\t\tt.Stop()\n', '\t\tcase <-make(chan struct{}):\n\t\t\tt.Stop()\n'))
mutate("a stop timeout names the jobs still running", "xcron/scheduler.go", "./xcron", "TestStop_DeadlineNamesStuckJobs",
       swap('\tbusy := s.busy()\n', '\tvar busy []string\n'))
