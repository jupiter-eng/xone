# The core mutations: the promises of the root module (the core). The format is in
# scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("Configuration")
mutate("logs are rendered in the configured timezone", "xlog/xlog.go", ".", "TestNew_RendersTimeInConfiguredTimezone|TestNew_FailsWhenTimezoneCannotLoad",
       swap('ReplaceAttr: inLocation(loc)}', 'ReplaceAttr: nil}\n\t_ = loc'))
# Another Run in the same process: with location written only when it is configured, the previous
# run's timezone stays behind
mutate("an unset timezone clears the previous one", "xlog/xlog.go", ".", "TestInitXLog_TimezoneClearedWhenLaterRunLeavesItUnset",
       swap('\tloc, _ := parseLocation(c.Timezone)\n\tlocation.Store(loc)\n',
            '\tif loc, _ := parseLocation(c.Timezone); loc != nil {\n\t\tlocation.Store(loc)\n\t}\n'))
mutate("UseHandler clears the previous timezone", "xlog/xlog.go", ".", "TestInitXLog_TimezoneClearedWhenLaterRunLeavesItUnset",
       swap('\t\tlocation.Store(nil) // the timestamps are decided by that handler, so xlog has no time zone\n', ''))
mutate("a timezone that cannot be loaded fails outright", "xlog/xlog.go", ".", "TestNew_FailsWhenTimezoneCannotLoad",
       swap('''\tloc, err := time.LoadLocation(name)
\tif err != nil {''', '''\tloc, err := time.LoadLocation(name)
\tif false {'''))
# File names go down to the minute: 0s really means one file per minute and 30s has two periods
# collide on one name -- both are configuring A and getting B
mutate("a rotation period under one minute fails startup", "xlog/config.go", ".", "TestNew_FailsFastOnRotationPeriodUnderOneMinute",
       swap('if c.RotateTime < time.Minute {', 'if c.RotateTime < 0 {'))
mutate("a negative MaxAge fails startup", "xlog/config.go", ".", "TestNew_MaxAge", swap('if c.MaxAge < 0 {', 'if false {'))
# The validation function being right proves nothing; New has to really call it
mutate("New really validates the file output configuration", "xlog/xlog.go", ".", "TestNew",
       swap('\tif err := cfg.File.validate(); err != nil {\n\t\treturn nil, nil, xerror.New("xlog", "config", err)\n\t}\n', ''))
# A misconfiguration reports config and an environment problem reports new: an alert goes by op to
# tell "change the configuration" from "look at the machine"
mutate("a misspelled log level reports config", "xlog/xlog.go", ".", "TestNew_FailsFastOnLevelTypo",
       swap('xerror.Newf("xlog", "config",\n\t\t\t"unknown log level', 'xerror.Newf("xlog", "new",\n\t\t\t"unknown log level'))
mutate("a misspelled log format reports config", "xlog/xlog.go", ".", "TestNew_FailsFastOnFormatTypo",
       swap('xerror.Newf("xlog", "config",\n\t\t\t"unknown log format', 'xerror.Newf("xlog", "new",\n\t\t\t"unknown log format'))
mutate("Perm accepts the 0o644 spelling", "xlog/xlog.go", ".", "TestParsePerm", swap('strings.TrimPrefix(s, "0o")', 's'))
# With slog.Default() still pointing at the file after it is closed, the logs of the stop hooks left
# behind and of the requests in flight are all lost
mutate("closing the log file switches back to stderr", "xlog/xlog.go", ".", "TestCloseXLog",
       swap('\tif swapOnClose {\n\t\tslog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))\n\t}\n', ''))
# The other side of it. Swapping when nothing that was being written to was closed takes the
# trace_id, the AddKV fields and the configured level and format off those last lines -- and with no
# output configured at all it turns "I want no logs" into logs on stderr the moment the service stops
mutate("the logger is left alone when nothing it writes to was closed", "xlog/xlog.go", ".", "TestCloseXLog",
       swap('\tif swapOnClose {\n', '\tif true {\n'))
mutate("what decides the swap is the console, not the file alone", "xlog/xlog.go", ".", "TestCloseXLog",
       swap('swapOnClose = !c.Console && c.File.Enable', 'swapOnClose = c.File.Enable'))
# With an ordinary file already at {Name}, Rename atomically replaces it with the symlink and the
# old logs are gone without a word
mutate("an ordinary file of the same name is not overwritten by the symlink", "xlog/rotate.go", ".", "TestRotateWriter",
       cut('\tif occupied(w.linkName) {\n\t\treturn nil, xerror.Newf(', 'change File.Name", w.linkName)\n\t}\n'))
mutate("a link location taken while running is not overwritten either", "xlog/rotate.go", ".", "TestRotateWriter",
       cut('\tif occupied(w.linkName) {\n\t\twarnf(', 'not updating the link", w.linkName)\n\t\treturn\n\t}\n'))
# app.log.bak and app.log.1.gz both match base.*, and going by mtime would delete them as expired
# logs
mutate("the cleanup deletes only the files it named itself", "xlog/rotate.go", ".", "TestRotateWriter",
       swap('\t\treturn !t.Add(period).After(cutoff)\n\t}\n\treturn false\n}', '\t\treturn !t.Add(period).After(cutoff)\n\t}\n\treturn true\n}'))
mutate("the old files are still cleaned up after the granularity changed", "xlog/rotate.go", ".", "TestRotateWriter",
       swap('\t\tt, err := time.ParseInLocation(l.layout, suffix, w.clock().Location())',
     '\t\tif l.layout != w.layout {\n\t\t\tcontinue\n\t\t}\n\t\tt, err := time.ParseInLocation(l.layout, suffix, w.clock().Location())'))
# Computing an old-granularity file's period from the current RotateTime would, after switching
# from daily to hourly, delete today's daily file -- still being written -- as expired the moment
# midnight passes
mutate("an old-granularity file expires by the period of its own granularity", "xlog/rotate.go", ".", "TestRotateWriter",
       swap('\t\tperiod := l.period\n\t\tif l.layout == w.layout {\n\t\t\tperiod = w.rotate\n\t\t}\n', '\t\tperiod := w.rotate\n'))
# Cleaning up only on rotation means a service that rotates daily and restarts a few times a day
# never reaches that rotation
mutate("opening cleans the expired files up once", "xlog/rotate.go", ".", "TestRotateWriter",
       swap('\tw.purge(w.currentName)\n\treturn w, nil', '\treturn w, nil'))
# CtxWithKV has to derive a copy: writing into the parent scope directly bleeds between siblings
# and pollutes the access log as well
mutate("CtxWithKV does not write back into the parent ctx", "internal/logext/scope.go", ".", "TestCtxWithKV",
       swap('return context.WithValue(ctx, ctxScopeKey{}, parent.copyWith(kvs))', 'parent.AddAll(kvs)\n\treturn context.WithValue(ctx, ctxScopeKey{}, parent)'))
mutate("CtxWithKV carries the fields the parent ctx already has", "internal/logext/scope.go", ".", "TestCtxWithKV",
       swap('\tmaps.Copy(c.kv, s.kv)\n', ''))
mutate("an imported fragment has profile variants too", "internal/config/source.go", ".", "TestLoad",
       swap('nested, err := fileSet(target, d, false, profiles, seen, depth+1)',
     'nested, err := withImports(target, d, profiles, seen, depth+1)'))
mutate("the wait between retries doubles each time", "xutil/retry.go", ".", "TestRetry", swap('\t\t\tbackoff = nextBackoff(backoff)\n', ''))
mutate("the budget is computed from the backoff ceiling", "xutil/retry.go", ".", "TestRetryBudget",
       swap('''\tbudget := timeout * time.Duration(attempts)
\tbackoff := min(interval, maxBackoff) // same as Retry: the first one is capped too
\tfor i := 1; i < attempts; i++ {
\t\tbudget += backoff
\t\tbackoff = nextBackoff(backoff)
\t}
\treturn budget''', '\treturn timeout*time.Duration(attempts) + interval*time.Duration(attempts-1)'))
# The documentation says each wait is at most maxBackoff, while the first wait used to use
# interval as it was
mutate("the first wait is capped too", "xutil/retry.go", ".", "TestRetry_FirstWaitAlsoCapped",
       swap('\tvar last error\n\tbackoff := min(interval, maxBackoff)', '\tvar last error\n\tbackoff := interval'))
mutate("the first backoff in the budget is capped too", "xutil/retry.go", ".", "TestRetryBudget_FirstBackoffAlsoCapped",
       swap('\tbackoff := min(interval, maxBackoff) // same as Retry', '\tbackoff := interval // same as Retry'))
mutate("the wait between retries carries jitter", "xutil/retry.go", ".", "TestJitter",
       swap('\treturn time.Duration(rand.Int64N(int64(d) + 1))', '\treturn d - time.Duration(rand.Int64N(2))'))
# A wrong password or a missing database would be retried for the whole round, dragging startup
# out by tens of seconds for nothing
mutate("a permanent error is not retried", "xutil/retry.go", ".", "TestRetry_NoRetryOnPermanentError|TestRetry_RecognizesWrappedPermanentError",
       swap('errors.As(last, &p) {', 'errors.As(last, &p) && false {'))
mutate("a permanent error returns the original error with the marker removed", "xutil/retry.go", ".", "TestRetry_NoRetryOnPermanentError|TestRetry_RecognizesWrappedPermanentError",
       swap('\t\t\treturn p.err\n\t\t}\n', '\t\t\treturn last\n\t\t}\n'))
# Reporting the previous "cannot connect" alone when cancelled during a backoff makes the startup
# path treat shutting down as asked as a failure
mutate("a cancellation during a backoff is reported faithfully", "xutil/retry.go", ".", "TestRetry",
       swap('return fmt.Errorf("%w, last attempt failed: %w", err, last)', 'return fmt.Errorf("%v, last attempt failed: %w", err, last)'))
# yaml.v3 recognizes an untagged field only in all lowercase: writing it as the field is named
# reports "field Endpoint not found" although the field is called exactly that. Without the hint,
# the caller is left staring at an error that contradicts itself
mutate("the error for an untagged field says how to fix it", "internal/config/strict.go", ".", "TestDecodeStrict",
       swap('\tif s.untagged[key] {', '\tif false {'))
# Once a field has moved from the top level into a block (XGin's CertFile into TLS:), whoever
# writes it the old way should see where it moved to
mutate("a field written one level too high is told which block it belongs in", "internal/config/strict.go", ".", "TestDecodeStrict_KeyOneLevelTooHigh",
       swap('} else if block := s.blockOf(key); block != "" {', '} else if block := ""; block != "" {'))
mutate("no guessing when several blocks hold that field", "internal/config/strict.go", ".", "TestDecodeStrict_KeyOneLevelTooHigh",
       swap('\t\t\tif found != "" {\n\t\t\t\treturn ""\n\t\t\t}\n', ''))
# A yaml type error carries the first few characters of the value, and ${VAR} is the recommended
# spelling for a credential: a password written into the wrong field puts part of itself into the
# startup log. One mutation at the call site, one at "remember which values were expanded"
mutate("a value expanded from a placeholder does not reach the error", "internal/config/strict.go", ".", "TestUnmarshal_ExpandedPlaceholderValueNotInError",
       swap('e = c.at(n) + ": " + c.redact(n, msg)', 'e = c.at(n) + ": " + msg'))
mutate("loading remembers the values expanded from placeholders", "internal/config/config.go", ".", "TestUnmarshal_ExpandedPlaceholderValueNotInError",
       swap('\texpand(root, &missing, values)', '\texpand(root, &missing, nil)'))
# When a variable's value happens to be null / ~, re-deciding the type takes it as "unset" and the
# field quietly stays at its default
mutate("a null spelling that came out of an expansion is still a string", "internal/config/config.go", ".", "TestLoad_PlaceholderNullLiteralIsNotTreatedAsUnset",
       swap('\t\tif n.ShortTag() == "!!null" {\n\t\t\tn.Tag = "!!str"\n\t\t}\n', ''))
# Strict decoding used to serialize the node back to text and decode that: it reported the line
# number inside that text, with no file name. Now every error is recorded against a node -- a
# misspelling against the key and a type error against the value -- with one mutation each for
# "remember which file a node came from" and "a merged copy goes by its first key"
mutate("a misspelled field reports that key's line number", "internal/config/strict.go", ".", "TestLoad_BadFieldReportsFileAndLine",
       swap('c.errs = append(c.errs, c.at(k)+": "+s.notFound(k.Value, t))', 'c.errs = append(c.errs, c.at(n)+": "+s.notFound(k.Value, t))'))
mutate("a type error reports the configuration file and the line too", "internal/config/strict.go", ".", "TestLoad_BadFieldReportsFileAndLine",
       swap('if msg, ok := strings.CutPrefix(e, fmt.Sprintf("line %d: ", n.Line)); ok {', 'if msg, ok := e, false; ok {'))
mutate("a malformed field reports which file it is in", "internal/config/config.go", ".", "TestLoad_BadFieldReportsFileAndLine",
       swap('remember(f.node, f.path, from)', 'remember(f.node, "", from)'))
mutate("a merged block takes its file from its first key", "internal/config/strict.go", ".", "TestLoad_BadFieldReportsFileAndLine",
       swap('for ; n != nil; n = first(n) {', 'for ; n != nil; n = nil {'))
mutate("a misspelled field fails startup", "internal/config/strict.go", ".", "TestLoad",
       swap('\t\tif ft == nil {\n\t\t\tc.errs = append(c.errs, c.at(k)+": "+s.notFound(k.Value, t))\n',
            '\t\tif ft == nil {\n'))
# "A key written out overrides one merged in with <<" used to be applied by the decoder only after
# the files had been merged, so a value a profile file wrote with << lost to a key of the same name
# written out in base. One mutation at the call site and one for each of the two rules
mutate("<< is flattened inside each file", "internal/config/source.go", ".", "MergeKey|TestLoad_Anchor|TestLoad_TypoInMerged",
       cut('\tif err := flattenMerges(out); err != nil {', 'return nil, xerror.Newf("xconfig", "config", "config %s: %w", path, err)\n\t}\n'))
mutate("a key written out overrides one merged in with <<", "internal/config/source.go", ".", "TestLoad_MergeKeyRulesWithinOneFile",
       swap('\t\t\town[n.Content[i].Value] = true\n', '\t\t\t_ = own\n'))
mutate("earlier entries of a << list override later ones", "internal/config/source.go", ".", "TestLoad_MergeKeyRulesWithinOneFile",
       swap('\t\t\t\t\town[key] = true // an earlier source overrides a later one\n', ''))
# A second document separated by --- inside one file used to be dropped quietly by yaml.Unmarshal,
# invisible even to Unclaimed. One mutation each for the call site, the error, and "an empty
# document does not count as one"
mutate("every file is checked for several documents", "internal/config/source.go", ".", "MultipleDocuments",
       swap('\tdoc, err := onlyDocument(raw)\n', '\tvar doc yaml.Node\n\terr := yaml.Unmarshal(raw, &doc)\n'))
mutate("a second document fails startup", "internal/config/source.go", ".", "MultipleDocuments",
       swap('\t\tif found {\n\t\t\treturn yaml.Node{}, fmt.Errorf("multiple', '\t\tif false {\n\t\t\treturn yaml.Node{}, fmt.Errorf("multiple'))
mutate("a trailing empty --- still loads as usual", "internal/config/source.go", ".", "TestLoad_LeadingOrTrailingDocumentMarkerStillLoads",
       swap('if len(d.Content) == 0 || isNull(d.Content[0]) {', 'if len(d.Content) == 0 {'))
# Unknown fields are looked up by type here, and one rule differing from yaml.v3's would either let
# a misspelling through or keep a valid configuration from starting
mutate("an ,inline struct is flattened for the lookup", "internal/config/strict.go", ".", "TestDecodeStrict_FieldRulesMatchYAML",
       swap('\t\t\t\ts.add(ft) //', '\t\t\t\t_ = ft //'))
mutate("an ,inline map takes the keys that are not recognized", "internal/config/strict.go", ".", "TestDecodeStrict_FieldRulesMatchYAML",
       swap('\t\t\t\ts.inline = ft.Elem()', '\t\t\t\t_ = ft'))
# ,inline on anything else is a mistake in the struct, and this check reaches the field before yaml
# does: without it what comes out is reflect's NumField of non-struct type, which names neither the
# field nor the tag
mutate(",inline on a non-struct says what is wrong", "internal/config/strict.go", ".", "TestDecodeStrict_InlineOnANonStruct",
       swap('\t\t\tcase ft.Kind() != reflect.Struct:', '\t\t\tcase false:'))
# An element that decodes itself has to be tried during the checking pass: decoded only at the end,
# one mistake outside hides the problems inside the elements until it is fixed and the process
# restarted
mutate("problems inside collection elements are reported along with the rest", "internal/config/strict.go", ".", "TestLoad_CollectionElementErrorsReportedWithOthers",
       swap('reflect.PointerTo(t).Implements(obsoleteType):\n\t\treturn c.try(n, t)',
            'reflect.PointerTo(t).Implements(obsoleteType):\n\t\treturn nil'))
mutate("a placeholder's type is decided from what replaced it", "internal/config/config.go", ".", "TestLoad",
       swap('\t\tif n.Value != before {\n\t\t\tretag(n)\n', '\t\tif n.Value != before {\n'))
# Merging aligns keys by name and a duplicate is swallowed at that step, so it can only be checked
# while each file is parsed.
# Aimed at the call site: only the first file's duplicates used to be caught, and everything from
# the profiles and the imports was missed
mutate("a duplicate key in any file is an error", "internal/config/source.go", ".", "TestLoad",
       cut('\tif err := checkDuplicates(&doc); err != nil {', 'return nil, xerror.Newf("xconfig", "config", "config %s: %w", path, err)\n\t}\n'))
# A profile file holding only comments used to replace every file before it outright, putting the
# whole configuration back to its defaults
mutate("an empty file laid on top changes nothing", "internal/config/merge.go", ".", "TestLoad",
       swap('return n == nil || n.Kind == 0 || isNull(n)', 'return n == nil || isNull(n)'))
mutate("a null in an overlay file does not override a lower-priority value", "internal/config/merge.go", ".", "TestLoad",
       swap('return n == nil || n.Kind == 0 || isNull(n)', 'return n == nil || n.Kind == 0'))
mutate("a placeholder expanding to empty keeps the default", "internal/config/config.go", ".", "TestLoad",
       swap('\tcase n.Value == "":\n\t\tn.Tag = "!!null"\n', '\tcase n.Value == "":\n'))
mutate("an anchor can be referenced across top-level blocks", "internal/config/source.go", ".", "TestLoad",
       swap('\tbudget := maxResolvedNodes\n\tout, err := resolveAliases(&doc, &budget)', '\tout, err := &doc, error(nil)'))
mutate("XApp.Profiles takes a comma-separated string", "internal/config/config.go", ".", "TestLoad",
       swap('out = append(out, splitProfiles(a)...)', 'out = append(out, a)'))
mutate("a placeholder inside XApp.Profiles is expanded", "internal/config/config.go", ".", "TestLoad",
       swap('''\tvar missing []string
\texpand(node, &missing, nil)
\tif len(missing) > 0 {
\t\treturn nil, xerror.Newf("xconfig", "config",
\t\t\t"environment variables not set in %s.%s of %s: %s", AppKey, ProfilesKey, path, missingNames(missing))
\t}
''', ''))
mutate("XApp.Profiles written in an imported file is an error", "internal/config/config.go", ".", "TestLoad",
       swap('if takeFromApp(f.node, ProfilesKey) != nil {',
     'if p, _ := profilesOf(f.node, f.path); len(p) > 0 {'))
# config_schema.json used to flag no misspelled field at all in an editor while marking both the
# single-instance and the multi-instance spellings red -- the four below each guard one place where
# it disagreed with the runtime
# A map's value is decoded starting from the zero value: without spreading the defaults over each
# one, every field left unset in a multi-instance block becomes the zero value
mutate("a field left unset in the multi-instance spelling keeps its default", "internal/config/clients.go", ".", "TestUnmarshalClients",
       swap('\tc := defaults()\n', '\tvar c C\n'))
# node.Decode brings no strict checking, and "a misspelled field fails startup" quietly stops
# working in the multi-instance spelling
mutate("a misspelling in the multi-instance spelling is an error too", "internal/config/clients.go", ".", "TestUnmarshalClients",
       swap('if err := DecodeStrict(n, &c); err != nil {', 'if err := n.Decode(&c); err != nil {'))
# An instance's error names which instance it is, and keeps yaml's type error with %w
mutate("a misspelling in the multi-instance spelling names the instance and keeps the type error", "internal/config/clients.go", ".", "TestUnmarshalClients_TypoInInstanceReportsFileAndLine",
       swap('fmt.Errorf("%s.%s: %w", clientsKey, name, err)', 'fmt.Errorf("%s.%s: %v", clientsKey, name, err)'))
mutate("Validate is called once for each instance", "internal/config/clients.go", ".", "TestUnmarshalClients_CallsValidateForEachInstance",
       swap('any(&c).(interface{ Validate() error })', 'any(c).(interface{ Validate() error })'))

section("Startup and shutdown")
# xone.Func / UntilSignal: the two Runnables that can be handed to Run without writing a type
mutate("Func's error is Run's error", "xone.go", ".", "TestFunc_", swap('func (f funcRunnable) Start(ctx context.Context) error { return f(ctx) }', 'func (f funcRunnable) Start(ctx context.Context) error { _ = f(ctx); return nil }'))
mutate("Func(nil) panics on the spot", "xone.go", ".", "TestFunc_", swap('\tif fn == nil {\n\t\tpanic("xone: Func needs', '\tif false {\n\t\tpanic("xone: Func needs'))
mutate("UntilSignal blocks until the shutdown signal", "xone.go", ".", "TestUntilSignal", swap('\t\t<-ctx.Done()\n\t\treturn nil\n', '\t\treturn nil\n'))
# Stop is optional and is called only when it is implemented. Aimed at the call site -- going
# around the type assertion leaves a server that wrote a Stop unable to stop
mutate("Stop is called only where it was written", "xone.go", ".", "TestRun_ClosesInReverseOrder|TestRun_StartOnlyRunnableWorks",
       swap('\tif s, ok := r.(stopper); ok {\n', '\tif s, ok := any(nil).(stopper); ok {\n'))
# The compiler does not catch a wrong Stop signature, and that Stop is never called
mutate("a wrong Stop signature is an error outright", "xone.go", ".", "TestRun_StopWithWrongSignatureFails",
       swap('if m, ok := t.MethodByName("Stop"); ok {', 'if m, ok := t.MethodByName("NoSuchMethod"); ok {'))
mutate("a Stop on the pointer with a value passed in is an error outright", "xone.go", ".", "TestRun_StopOnPointerButValuePassedFails",
       swap('reflect.PointerTo(t).MethodByName("Stop")', 'reflect.PointerTo(t).MethodByName("NoSuchMethod")'))
# With a Stop returning at the deadline and Start returning right behind it, both are ready at
# once and select picks at random: picking the Done branch loses Start's error and prints a
# pointless "the server did not exit" warning
mutate("at the deadline it first looks whether Start has returned", "xone.go", ".", "TestRun_StartErrorKeptWhenStopReturnsAtDeadline",
       swap('\t\t\tselect {\n\t\t\tcase e := <-runErr:\n\t\t\t\tfirst = errors.Join(first, e)\n\t\t\tdefault:\n',
            '\t\t\tselect {\n\t\t\tdefault:\n'))
mutate("a list is replaced whole rather than merged element by element", "internal/config/merge.go", ".", "TestLoad",
       swap('''\tif base.Kind != yaml.MappingNode || override.Kind != yaml.MappingNode {
\t\treturn override
\t}''', '''\tif base.Kind == yaml.SequenceNode && override.Kind == yaml.SequenceNode {
\t\tout := *base
\t\tout.Content = append(append([]*yaml.Node{}, override.Content...), base.Content[len(override.Content):]...)
\t\treturn &out
\t}
\tif base.Kind != yaml.MappingNode || override.Kind != yaml.MappingNode {
\t\treturn override
\t}'''))
# Moving this document behind the ones it imports turns the precedence around.
# The comment line is carried along to anchor the one place it belongs to: fileSet holds an
# identical `out = append(out, nested...)` + return
mutate("an imported file overrides the one importing it", "internal/config/source.go", ".", "TestLoad",
       swap('\tout := []loaded{{path: path, node: doc}}\n', '\tvar out []loaded\n'),
       swap('''\t\t// An imported file has profile variants too, but a missing variant is not an error
\t\tnested, err := fileSet(target, d, false, profiles, seen, depth+1)
\t\tif err != nil {
\t\t\treturn nil, err
\t\t}
\t\tout = append(out, nested...)
\t}
\treturn out, nil''', '''\t\tnested, err := fileSet(target, d, false, profiles, seen, depth+1)
\t\tif err != nil {
\t\t\treturn nil, err
\t\t}
\t\tout = append(out, nested...)
\t}
\tout = append(out, loaded{path: path, node: doc})
\treturn out, nil'''))
# The third argument is variantRequired: the main file's profile variant has to exist
mutate("a missing profile file fails outright", "internal/config/source.go", ".", "TestLoad",
       swap('files, err := fileSet(base, doc, true, active, seen, 0)', 'files, err := fileSet(base, doc, false, active, seen, 0)'))
mutate("a second signal can terminate a stuck process", "xone.go", ".", "TestRun",
       swap('\t\t\tsignal.Stop(ch)\n\t\t\to.log().Info(','\t\t\to.log().Info(',1))
# Stop has to be able to do its work. Reusing the cancelled ctx would have every shutdown step
# refused the moment it begins -- the requests in flight unfinished and the record in the registry
# not deregistered, which is no graceful shutdown at all
mutate("the ctx Stop receives does not inherit that cancellation", "xone.go", ".", "TestRun_StopCtxDoesNotInheritCancellation",
       swap('context.WithTimeout(context.WithoutCancel(ctx), o.stopTimeout)','context.WithTimeout(ctx, o.stopTimeout)'))
# Without the grace period past the deadline, the error reported by a Stop that returns at the
# deadline is lost and the process exits with 0
mutate("the error of a Stop returning at the deadline is not lost", "xone.go", ".", "TestRun_DeadlineAwareStopErrorIsKept",
       swap('case <-time.After(stopGrace):', 'default:'))
# A Stop that receives a ctx and ignores it: called synchronously, Run never returns and not one
# stop hook gets its turn
mutate("a Stop ignoring its ctx cannot hang the shutdown", "xone.go", ".", "TestRun_StopIgnoringCtxDoesNotHangExit",
       swap('errors.Join(first, stopServer(serverCtx, o, s))', 'errors.Join(first, safe("stop", func() error { return s.Stop(serverCtx) }))'))
# The server may use only the first 2/3 of the budget. Given the whole of it, a server refusing to
# exit eats the time up, every stop hook is out of time the moment it begins, Close() is thrown
# into a goroutine nobody waits for, and the resources stay where they are
mutate("the server cannot eat the share kept for the stop hooks", "xone.go", ".", "TestShutdown|TestRun_ClosesOthersWhenServiceWontExit",
       swap('context.WithTimeout(stopCtx, o.stopTimeout-o.stopTimeout/3)', 'context.WithTimeout(stopCtx, o.stopTimeout)'))
# K8s sends SIGTERM and Ctrl-C sends SIGINT. Miss either one and that path has no graceful
# shutdown at all
mutate("both shutdown signals are listened for", "xone.go", ".", "TestRun_ListensForBothExitSignals",
       swap('[]os.Signal{syscall.SIGINT, syscall.SIGTERM}','[]os.Signal{syscall.SIGTERM}'))
# xlog swaps the global logger inside a start hook. Capturing it once at the top of Run would
# leave every framework log afterwards writing to the old one -- the server comes up and not one
# line of the initialization logs is visible
mutate("the framework logs take the global logger afresh every time", "xone.go", ".", "TestRun_FrameworkLogsFollowGlobalLoggerSwappedByHook",
       swap('\tctx, stopSignals := notifyShutdown(o)','\to.logger = o.log()\n\tctx, stopSignals := notifyShutdown(o)'))
mutate("the stop hooks are bounded by the stop budget", "xone.go", ".", "TestShutdown",
       swap('err := runWithin(hookCtx, e)', 'err := safeHook(hookCtx, e)'))
# If one stuck hook could use the whole deadline, the hooks behind it would enter with an expired
# ctx and time out on the spot, and the logging closed last would not even get to close its file
mutate("each hook keeps a share back for the ones behind it", "xone.go", ".", "TestShutdown",
       swap('end.Add(-time.Duration(len(todo)-1-i)*reserve)', 'end.Add(-time.Duration(len(todo)-1-i)*reserve*0)'))
mutate("a signal during startup means the server is not started", "xone.go", ".", "TestRun",
       swap('started, err := runStart(ctx, o)','started, err := runStart(context.Background(), o)'))
mutate("on an error it can be asked who reported it", "xone.go", ".", "TestRun",
       swap('xerror.Newf("xone", "start", "%s: %w", e.Name, err)', 'xerror.Newf("xone", "start", "%s: %v", e.Name, err)'))
# Run returns an errors.Join. Walking a single chain inwards alone never looks at the siblings
# further along the Join: Is(Join(xgin's error, xgorm's error), "xgorm") would be false
mutate("xerror.Is walks every branch of a Join", "xerror/xerror.go", ".", "TestIs",
       cut('\tcase interface{ Unwrap() []error }:\n', '\t\t\t\treturn true\n\t\t\t}\n\t\t}\n'))
# One error has one frame: every layer used to carry the whole "xone {module} {op} failed,
# err=[...]", burying the root cause in the innermost brackets. The three below each guard one
# place
mutate("an inner layer of the same module keeps only the op and the cause", "xerror/xerror.go", ".", "TestNewf|TestNew_",
       swap('\tif !n.same {\n\t\treturn n.e.render(false)\n\t}', '\tif true {\n\t\treturn n.e.render(false)\n\t}'))
mutate("an inner layer does not repeat the xone prefix", "xerror/xerror.go", ".", "TestNewf|TestNew_",
       swap('return n.e.render(false)', 'return n.e.render(true)'))
mutate("Newf folds an xerror among its arguments", "xerror/xerror.go", ".", "TestNewf",
       swap('\t\t\town[i] = nest(module, xe)\n', '\t\t\town[i] = xe\n'))
# A var e *Error wrapped without a nil check: a panic on the error path covers the real failure up
mutate("New given a typed nil does not panic", "xerror/xerror.go", ".", "TestNew_TypedNilTreatedAsNoCauseNotPanic",
       swap('\t\tif xe == nil {\n\t\t\treturn &Error{Module: module, Op: op}\n\t\t}\n', ''))
mutate("a typed nil among Newf's arguments does not panic", "xerror/xerror.go", ".", "TestNewf_TypedNilArgRendersAsNilNotPanic",
       swap('if xe, ok := a.(*Error); ok && xe != nil {', 'if xe, ok := a.(*Error); ok {'))
mutate("Is given a typed nil does not panic", "xerror/xerror.go", ".", "TestNewf_TypedNilArgRendersAsNilNotPanic",
       swap('\t\tif xe == nil {\n\t\t\treturn false // a typed nil', '\t\tif false {\n\t\t\treturn false // a typed nil'))
mutate("another layer of the same module returns it unchanged", "xerror/xerror.go", ".", "TestNew_",
       swap('\t\tif xe.Module == module {\n\t\t\treturn xe\n\t\t}\n', ''))
mutate("retrying a connect can be cancelled", "xutil/retry.go", ".", "TestRetry",
       swap('\tif err := parent.Err(); err != nil {\n\t\treturn err\n\t}\n\n',''))
# When what was panicked with is an error, %w has to catch it, or errors.Is cannot be asked for
# the root cause. Aimed at both call sites
mutate("an error panicked while building an instance stays in the chain", "internal/xclient/xclient.go", ".", "TestBuild_new_panic",
       swap('"instance %q %w", name, panicked(r))', '"instance %q panicked: %v", name, r)'))
mutate("an error panicked while closing an instance stays in the chain", "internal/xclient/xclient.go", ".", "TestClose_panic",
       swap('"close %w", panicked(r))', '"close panicked: %v", r)'))
mutate("an error of the same module keeps its original op", "internal/xclient/xclient.go", ".", "TestBuild",
       swap('ok && xe.Module == module {', 'false && ok && xe.Module == module {'))
# The integrations' connect logs write the instance name through it: the call sites inside the
# modules have mutations of their own, and this one watches whether Build hands the name over
mutate("Build hands the instance name to new", "internal/xclient/xclient.go", ".", "TestBuild_BuildsInNameOrder",
       swap('new(ctx, name, cfg)', 'new(ctx, "", cfg)'))
# A snapshot is immutable once published: handing the internal map out directly lets a caller
# change the registry by changing it
mutate("All hands out a copy", "internal/xclient/xclient.go", ".", "TestAll",
       swap('return maps.Clone(r.state.Load().items)', 'return r.state.Load().items'))

section("Flow orchestration")
# A step only has to write Process and Rollback: Name defaults to the type name and Dependency to
# a strong dependency
mutate("an unset Name uses the type name", "xflow/xflow.go", ".", "TestNew_",
       swap('name: typeName(p), dep: Strong}', 'name: "", dep: Strong}'))
mutate("an unset Dependency is a strong dependency", "xflow/xflow.go", ".", "TestNew_",
       swap('name: typeName(p), dep: Strong}', 'name: typeName(p), dep: Weak}'))
mutate("a Dependency that was written is used", "xflow/xflow.go", ".", "TestNew_", swap('\t\ts.dep = d.Dependency()\n', '\t\t_ = d\n'))
# Using xflow on its own, nobody reads the configuration file and WithRollbackTimeout is the only
# way to change the rollback budget
mutate("a flow's own rollback budget overrides the configuration", "xflow/xflow.go", ".", "TestWithRollbackTimeout",
       swap('cmp.Or(f.rollbackTimeout, cfg.RollbackTimeout)', 'cmp.Or(cfg.RollbackTimeout, f.rollbackTimeout)'))
# Changing it in place changes the same flow another goroutine is Executing concurrently
mutate("WithRollbackTimeout returns a new flow", "xflow/xflow.go", ".", "TestWithRollbackTimeout",
       swap('\tg := *f\n\tg.rollbackTimeout = d\n\treturn &g\n', '\tf.rollbackTimeout = d\n\treturn f\n'))
mutate("a rollback budget that is not positive panics", "xflow/xflow.go", ".", "TestWithRollbackTimeout",
       swap('\tif d <= 0 {\n\t\tpanic(fmt.Sprintf("xflow: rollback timeout', '\tif d < 0 {\n\t\tpanic(fmt.Sprintf("xflow: rollback timeout'))
# What is rolled back is the prefix that ran: a failed weak dependency is included (it may have
# left side effects behind) and a failed strong dependency is not (it did not succeed)
mutate("a failed weak dependency is rolled back too", "xflow/xflow.go", ".", "TestExecute", swap('\t\t\tn++ // a failed weak dependency', '\t\t\t// a failed weak dependency'))
mutate("the step of a failed strong dependency is not rolled back", "xflow/xflow.go", ".", "TestExecute",
       swap('''\t\tf.rollback(ctx, data, f.steps[:n], res, m)
\t\treturn res
\t}
\treturn res''', '''\t\tf.rollback(ctx, data, f.steps[:min(n+1, len(f.steps))], res, m)
\t\treturn res
\t}
\treturn res'''))
mutate("a cancelled flow cannot be reported as a success", "xflow/xflow.go", ".", "TestExecute",
       swap('\t\t\tif ctx.Err() == nil {\n\t\t\t\tcontinue\n\t\t\t}','\t\t\tcontinue'))
# Two places: one in notifyStep and one in notifyFlow, and only removing both turns the
# containment off
mutate("a successful step is logged at INFO", "xflow/monitor.go", ".", "TestSlogMonitor_StepLogsAtInfoLevel",
       swap('slog.InfoContext(ctx, "xflow step "+action+" done"', 'slog.DebugContext(ctx, "xflow step "+action+" done"'))
mutate("a panic in a monitor implementation is contained", "xflow/monitor.go", ".", "TestMonitor", swap('\tdefer recoverNotify()\n', '', count=2))
# One xerror per module boundary: another layer inside safeProcess would nest the text into two
# layers of xflow
mutate("a panic in a step is wrapped in one layer of xflow only", "xflow/xflow.go", ".", "TestExecute",
       swap('''err = &PanicError{Value: r, Stack: debug.Stack()}
\t\t}
\t}()
\treturn p.Process(ctx, data)''', '''err = xerror.New("xflow", "execute", &PanicError{Value: r, Stack: debug.Stack()})
\t\t}
\t}()
\treturn p.Process(ctx, data)'''))
mutate("an error panicked in a step stays in the chain", "xflow/xflow.go", ".", "TestExecute",
       swap('\terr, _ := e.Value.(error)\n\treturn err', '\treturn nil'))
# With the stack kept out of the error message it has to be visible somewhere: the default monitor
# records it as a field of its own
mutate("the default monitor records a panic's stack", "xflow/monitor.go", ".", "TestMonitor",
       swap('attrs = append(attrs, "stack", string(pe.Stack))', '_ = pe'))
# A rollback reusing the caller's ctx fails entirely the moment the request times out -- which is
# exactly when the compensation is needed most. Stripping the cancellation off and giving it a
# RollbackTimeout of its own is xflow's most central promise
mutate("a rollback after a flow timed out still has a budget of its own", "xflow/xflow.go", ".", "TestRollback|TestExecute",
       swap('context.WithTimeout(context.WithoutCancel(ctx), cmp.Or(', 'context.WithTimeout(ctx, cmp.Or('))
# What is stripped is the cancellation, not the values: swapped for Background, the compensating
# requests lose the trace, the load-test marker and the lane marker, so load-test compensation
# reaches production and lane compensation reaches the trunk
mutate("the rollback ctx carries the caller's values", "xflow/xflow.go", ".", "TestRollback_CtxValuesPreserved",
       swap('context.WithTimeout(context.WithoutCancel(ctx), cmp.Or(', 'context.WithTimeout(context.Background(), cmp.Or('))
# With the budget checked only between steps, one Rollback ignoring its ctx can hang Execute: a
# 50ms budget waited 2s, and the step that hung is not even in RollbackErrors
mutate("a compensation ignoring its ctx is bounded by the rollback budget too", "xflow/xflow.go", ".", "TestRollback",
       swap('err := rollbackWithin(rbCtx, s.Processor, data)', 'err := safeRollback(rbCtx, s.Processor, data)'))
# A flow cancelled before its first step used to report rolled back as well, sending somebody to
# look into a compensation that never happened
mutate("Rolled is not reported when not one step was rolled back", "xflow/xflow.go", ".", "TestExecute",
       swap('\trbCtx, cancel := context.WithTimeout(', '\tres.Rolled = true\n\trbCtx, cancel := context.WithTimeout('))
# Decoding into cfg directly carries the previous Run's values into the next one in the same
# process, and leaves a value that failed validation on cfg as well
mutate("the configuration is decoded from the defaults every time", "xflow/config.go", ".", "TestLoadConfig",
       swap('\tc := DefaultConfig()\n\tif err := xconfig.Unmarshal(ConfigKey, &c)', '\tc := cfg\n\tif err := xconfig.Unmarshal(ConfigKey, &c)'))
mutate("a configuration that failed validation does not take effect", "xflow/config.go", ".", "TestLoadConfig",
       swap('\t\treturn err\n\t}\n\tcfg = c', '\t\tcfg = c\n\t\treturn err\n\t}\n\tcfg = c'))

section("The registration board")
# Decoding into the global directly carries the previous Run's service name into the next one, and
# a half-written value lands on it when decoding fails
mutate("xapp decodes into fresh defaults and swaps them in", "xapp/xapp.go", ".", "TestLoadConfig_",
       swap('\tc := DefaultConfig()\n\tif err := xconfig.Unmarshal(ConfigKey, &c); err != nil {\n\t\treturn err\n\t}\n\tcfg = c\n\treturn nil',
            '\treturn xconfig.Unmarshal(ConfigKey, &cfg)'))
# What SetDefault supplies has to outlive the configuration being loaded: loadConfig starts each Run
# from DefaultConfig(), so dropping this leaves the file as the only way to have an identity at all
mutate("SetDefault survives the configuration being loaded", "xapp/xapp.go", ".", "TestSetDefault_",
       swap('\tdefaults = c\n\tcfg = c\n', '\tcfg = c\n'))
mutate("SetDefault is readable before the configuration is loaded", "xapp/xapp.go", ".", "TestSetDefault_",
       swap('\tdefaults = c\n\tcfg = c\n', '\tdefaults = c\n'))
# The stages are everything the caller has to understand the lifecycle by: logging comes up first,
# tracing before the clients and the servers last. One stage out of order shows up as "the instance
# becomes ready after whatever uses it", and nothing else catches it
mutate("startup follows ascending stages", "internal/hook/hook.go", ".", "TestStartOrder|TestStopOrder|TestAddStart",
       swap('return cmp.Compare(a.Stage, b.Stage)', 'return cmp.Compare(a.Name, b.Name)'))
# Within one stage, whatever registered first comes up first. With an unstable sort the order
# changes with the implementation, while the caller understands it by the order of the imports
mutate("the registration order is kept within one stage", "internal/hook/hook.go", ".", "TestStartOrder", swap('slices.SortStableFunc(out,', 'slices.SortFunc(out,'))
# The promise that "declaring one stage gives both starting earlier and closing later" rests
# entirely on the stop order being the whole thing reversed
mutate("the stop order is the mirror image of the start order", "internal/hook/hook.go", ".", "TestStopOrder",
       swap('''\tout := startOrder(in)
\tslices.Reverse(out)
\treturn out''', '''\tout := slices.Clone(in)
\tslices.SortStableFunc(out, func(a, b Entry) int { return cmp.Compare(b.Stage, a.Stage) })
\treturn out'''))
# Taking the last path element as the pairing key would treat two packages ending in the same name
# as one: a caller wrapping things in a package of their own called xlog is common, and once they
# collide, a failure of its start hook also skips the framework xlog's stop hook and the log writer
# never flushes again
mutate("the pairing key is the whole import path", "xhook/xhook.go", ".", "TestPkgOf|TestBeforeStart",
       swap('\t\treturn full[:slash+1+i]', '\t\treturn full[slash+1 : slash+1+i]'))
# With the default stage falling back to StageClient, a business hook shares a stage with xgorm and
# which runs first is up to the packages' initialization order: with the business package's path
# sorting first, the xgorm.C() inside the hook panics on the spot
mutate("an unset stage lands in the business stage", "xhook/xhook.go", ".", "TestBeforeStart|TestAt",
       swap('o := options{stage: StageBusiness}', 'o := options{stage: StageClient}'))
# A stop hook runs only once the start hook it is paired with has succeeded: otherwise, when
# startup fails halfway, the resources behind it were never built and calling their stop hooks would
# only fail on a pile of zero values
mutate("the stop hook of a failed pair does not run", "internal/hook/hook.go", ".", "TestStopAfter|TestRun_InitFailureRollsBackInitialized",
       swap('e.Pair != 0 && !started[e.Pair] {', 'e.Pair != 0 && !started[e.Pair] && false {'))
# The filtering lives in hook.StopAfter, with one mutation per call site: going around it makes
# the filter itself useless however right it is
mutate("Run filters the stop hooks by what started", "xone.go", ".", "TestRun_InitFailureRollsBackInitialized",
       swap('hook.StopAfter(started)', 'hook.Stop()'))
# A stop hook with no start hook before it always runs. A rejected Runnable and an unreadable
# configuration used to return outright, so a hook like the log flush never ran once. One mutation
# per early return
mutate("a badly written Runnable still runs the stop hooks that do not depend on startup", "xone.go", ".", "TestRun_RunnableInvalid_UnpairedStopHooksStillRun",
       swap('\tif err := checkRunnable(r); err != nil {\n\t\treturn errors.Join(err, stopWithin(o, nil))',
            '\tif err := checkRunnable(r); err != nil {\n\t\treturn err'))
mutate("an unreadable configuration still runs the stop hooks that do not depend on startup", "xone.go", ".", "TestRun_ConfigLoadFails_UnpairedStopHooksStillRun",
       swap('\tif err := config.Ensure(o.configPath, o.log()); err != nil {\n\t\treturn errors.Join(err, stopWithin(o, nil))',
            '\tif err := config.Ensure(o.configPath, o.log()); err != nil {\n\t\treturn err'))
# Registered together makes a pair: paired with the earliest start hook of the same package
# instead, the second pair's stop hook would be called even when it failed to build and would have
# to handle "not built yet"
mutate("a stop hook is paired with the start hook registered most recently before it", "internal/hook/hook.go", ".", "TestRun_StopHookPairsOnlyWithItsStartHook|TestRun_StopHookPairsWithLaterOfTwoStartHooks|TestAddStop",
       swap('for i := len(start) - 1; i >= 0; i-- {', 'for i := 0; i < len(start); i++ {'))
# With At written on the start hook alone, the stop hook used to fall back to StageBusiness: the
# clients were closed before the business
mutate("a stop hook with no stage follows the start hook it is paired with", "xhook/xhook.go", "./xhook", "TestBeforeStop_WithoutStageFollowsPairedStartHook",
       swap('Run: hook.Func(f), Inherit: !o.set}', 'Run: hook.Func(f), Inherit: false}'))
mutate("a stage written explicitly on a stop hook decides", "xhook/xhook.go", "./xhook", "TestAt_OverridesDefaultStage",
       swap('Run: hook.Func(f), Inherit: !o.set}', 'Run: hook.Func(f), Inherit: true}'))
mutate("the stage is inherited only where a pair was found", "internal/hook/hook.go", ".", "TestAddStop_WithoutExplicitStageFollowsPairedStartHook",
       swap('\t\t\tif e.Inherit {\n\t\t\t\te.Stage = start[i].Stage\n\t\t\t}\n', ''))
# The test helper goes by the same rule as Run: a pair whose start failed runs no stop hook, and
# whatever came up is closed as usual
mutate("xonetest closes only what its paired start hook succeeded for", "xonetest/xonetest.go", ".", "TestStartHooks",
       swap('hook.StopAfter(started)', 'hook.Stop()'))
mutate("xonetest records the start hooks that succeeded", "xonetest/xonetest.go", ".", "TestStartHooks",
       swap('\t\tstarted[e.Seq] = true\n', ''))
mutate("xonetest clears the configuration when it ends", "xonetest/xonetest.go", ".", "TestUseConfig",
       swap('\tt.Cleanup(config.Reset)\n', ''))
# A Pair of 0 means "unpaired, always runs". Numbering from 0 would make the stop hook of the
# first start hook registered count as one that does not depend on startup, and it would be called
# even when that start hook failed
mutate("start hooks are numbered from 1", "internal/hook/hook.go", ".", "TestAddStop",
       swap('e.Seq = len(start) + 1', 'e.Seq = len(start)'))
# Pairing goes by the same package alone: with the wrong package, a stop hook would be skipped when
# another integration failed to come up, or called even when its own never did
mutate("a stop hook pairs only with a start hook of the same package", "internal/hook/hook.go", ".", "TestAddStop|TestRun_StopHookPairsOnlyWithItsStartHook",
       swap('\t\tif start[i].Pkg == e.Pkg {', '\t\tif true {'))
# Going by the hook function's name would credit every hook registered through a helper package to
# that helper, pairing them all into one tangle
mutate("the registering package is taken from the init on the call stack", "xhook/xhook.go", ".", "TestBeforeStart",
       swap('Pkg: registrant(full)', 'Pkg: pkgOf(full)'))
# When what was panicked with is an error, %w has to catch it, or errors.Is cannot be asked for
# the root cause
mutate("an error panicked in a hook stays in the chain", "xone.go", ".", "TestRun_HookPanicWrappedOnceWithStandardOp",
       swap('return fmt.Errorf("panicked: %w", err)', 'return fmt.Errorf("panicked: %v", err)'))

section("Clients")
# When an instance cannot be found, the reason has to be exact: called too early, called too late,
# never configured and a misspelled name all send somebody to a different place. Called too early
# used to read the same as "never configured", sending whoever took an instance before Run to the
# configuration file
mutate("called too early is not reported as never configured", "internal/xclient/xclient.go", ".", "TestGet_", swap('\tcase p == notStarted:\n', '\tcase false:\n'))
mutate("called too late is not reported as never configured", "internal/xclient/xclient.go", ".", "TestGet_", swap('\tcase p == closed:\n', '\tcase false:\n'))
# A wrong password stays wrong on a retry and costs two more backoffs; reported as cannot reach it
# sends somebody to look at the network first.
# The same mechanism (xclient.Probe) is hit in two places: Probe stops trying once it recognizes it,
# and xgorm hands the dialect's decision to it
mutate("an authentication failure is not retried", "internal/xclient/probe.go", ".", "TestProbe_AuthFailureReturnsOriginalErrorAfterOneTry",
       swap('\t\t\treturn xutil.Permanent(err)\n', '\t\t\treturn err\n'))
# A multi-instance validation error has to point at the file and the line: with the reason alone,
# which of a dozen instances is wrong cannot be found
mutate("an instance's Validate error carries the file and the line", "internal/config/clients.go", ".", "TestUnmarshalClients_CallsValidateForEachInstance",
       swap('\t\t\treturn c, fmt.Errorf("%s: %w", newChecker().at(n), err)\n', '\t\t\treturn c, err\n'))
# The TLS block (xtls.Config) is defined once and shared by the modules: the rules are hit inside
# xtls, and "this module really uses it" at each module's call site -- the function being right does
# not help when the call site goes around it.
# A CAFile written with Enable forgotten, connecting in plaintext, is worse than an error
mutate("TLS fields written with TLS off must fail", "xtls/xtls.go", ".", "TestValidate_NonsensicalCombosFailAtConfigRead|TestBuild_ErrorsOnFieldsSetWhileDisabled",
       swap('\t\tif c != (Config{}) {', '\t\tif false {'))
mutate("the TLS client certificate and key come in pairs", "xtls/xtls.go", ".", "TestValidate_NonsensicalCombosFailAtConfigRead",
       swap('if (c.CertFile == "") != (c.KeyFile == "") {', 'if false {'))
mutate("a Build called directly validates first too", "xtls/xtls.go", ".", "TestBuild_ErrorsOnFieldsSetWhileDisabled",
       swap('\tif err := c.Validate(); err != nil {\n\t\treturn nil, err\n\t}\n\tif !c.Enable', '\tif !c.Enable'))
mutate("TLS uses the CAFile", "xtls/xtls.go", ".", "TestBuild_CAFile",
       swap('\t\tcfg.RootCAs = pool\n', '\t\t_ = pool\n'))
mutate("TLS carries the client certificate", "xtls/xtls.go", ".", "TestBuild_ClientCert",
       swap('\t\tcfg.Certificates = []tls.Certificate{cert}\n', '\t\t_ = cert\n'))
mutate("the TLS ServerName is passed down", "xtls/xtls.go", ".", "TestBuild_CAFile|TestBuild_MinTLS12AndVerifies",
       swap('ServerName: c.ServerName}', '}'))
mutate("TLS is at least 1.2", "xtls/xtls.go", ".", "TestBuild_MinTLS12AndVerifies",
       swap('MinVersion: tls.VersionTLS12, ', ''))
# A rejected certificate is like a wrong password: another attempt means the same certificate. The
# PG / MySQL / Redis connection probes all go through here
mutate("a rejected certificate is not retried", "internal/xclient/probe.go", ".", "TestProbe",
       swap('(tlsRejected(err) || p.AuthFailed != nil && p.AuthFailed(err))', '(p.AuthFailed != nil && p.AuthFailed(err))'))
mutate("a certificate alert from the peer counts as a rejected certificate too", "internal/xclient/probe.go", ".", "TestProbe_CertRejectedReturnsAfterOneTry",
       swap('if errors.As(err, &op) && op.Op == "remote error" && op.Err != nil {', 'if false {'))
# Publishing half a set of instances when building fails halfway is worse than publishing none:
# C() finds a and not b, while startup has in fact failed.
# When building fails halfway and what was built is closed again, an error from a failed close used
# to be dropped and the leaked connection pool left not one word in the logs
mutate("a failed close is joined into Build's error", "internal/xclient/xclient.go", ".", "TestBuild_CloseErrorsOfBuiltOnesJoinedIntoBuildError",
       swap('\t\tif cerr := closeAll(r.module, closers); cerr != nil {\n\t\t\treturn errors.Join(err, cerr)\n\t\t}\n',
            '\t\tcloseAll(r.module, closers)\n'))
mutate("a failed build leaves the registry as it was", "internal/xclient/xclient.go", ".", "TestBuild",
       swap('''\t\tbuilt[name] = v
\t\tclosers = append(closers, closer)''', '''\t\tbuilt[name] = v
\t\tclosers = append(closers, closer)
\t\tr.state.Store(&snapshot[T]{items: built, phase: running})'''))
# The other way round, C() would still find an instance that is being closed halfway through
mutate("closing an instance takes it out first", "internal/xclient/xclient.go", ".", "TestClose",
       swap('''\told := r.state.Swap(&snapshot[T]{items: map[string]T{}, phase: closed})
\treturn closeAll(r.module, old.closers)''', '''\terr := closeAll(r.module, r.state.Load().closers)
\tr.state.Store(&snapshot[T]{items: map[string]T{}, phase: closed})
\treturn err'''))
# An empty block such as "XRedis:" used to fail startup outright: Has returned false, the package
# skipped it and never Unmarshalled, so the key was claimed by nobody -- and neither reason
# Unclaimed reports (a misspelling, a forgotten import) applied, so following them found nothing
mutate("asking about a key counts as claiming it", "internal/config/config.go", ".", "TestHas|TestUnclaimed",
       swap('''\t\treturn nil, err
\t}
\tclaimed[key] = true
\tif node := sections[key];''', '''\t\treturn nil, err
\t}
\tif node := sections[key];'''))
# The claim is recorded inside sectionLocked, and this one is aimed at the call site: a Has that
# goes around it and looks for itself is back in the shape of the original bug
mutate("Has records a claim too", "internal/config/config.go", ".", "TestHas|TestUnclaimed",
       swap('\tnode, err := sectionLocked(key)\n\treturn err == nil && node != nil',
            '\terr := ensureLocked()\n\treturn err == nil && !isEmptyNode(sections[key])'))
# What this guards against: reading early quietly yields zero values and the service comes up with
# a set of defaults. The first read now loads first, and this mutation puts it back to "not loaded
# means not configured"
mutate("reading early still gets the values from the file", "internal/config/config.go", ".", "TestUnmarshal_LoadsFirstIfNotLoaded|TestRun_ReadingConfigBeforeRunGetsFileValues",
       swap('''\tif err := ensureLocked(); err != nil {
\t\treturn nil, err
\t}
\tclaimed[key] = true''', '''\tif !ready {
\t\treturn nil, nil
\t}
\tclaimed[key] = true'''))
# The call site: an Unmarshal that goes around sectionLocked straight to sections quietly yields
# zero values when nothing was loaded
mutate("Unmarshal loads before it reads", "internal/config/config.go", ".", "TestUnmarshal_LoadsFirstIfNotLoaded|TestRun_ReadingConfigBeforeRunGetsFileValues",
       swap('\tnode, err := sectionLocked(key)\n\tif err != nil {\n\t\treturn err\n\t}',
            '\tnode, err := sections[key], error(nil)\n\tif err != nil {\n\t\treturn err\n\t}'))
# With no configuration file the configuration used to stay at "not loaded yet", every integration
# failed to read its configuration, and a service needing no configuration at all could not start
mutate("with no configuration file everything uses the defaults", "internal/config/config.go", ".", "TestEnsure_UsesDefaultsWhenNoConfigFile|TestRun_StartsWithDefaultsWhenNoConfigFile",
       swap('''\t\tsections, claimed, ready = nil, map[string]bool{}, true
\t\treturn nil''', '''\t\tfailed = errors.New("no config file")
\t\treturn failed'''))
mutate("a configuration loaded earlier is not quietly swapped out", "internal/config/config.go", ".", "TestEnsure_FailsWhenNamedFileDiffersFromEarlierLoad|TestRun_FailsWhenOtherFileNamedAfterEarlyRead",
       swap('\tif path != "" && !sameFile(path, source) {', '\tif false {'))
# Compared as strings, dir/./b.yml, a relative path and a symlink would each count as another file
# and Run would fail to start
mutate("the same file spelled differently is recognized", "internal/config/config.go", ".", "TestEnsure_SameFileSpelledDifferentlyIsFine",
       swap('\tif path != "" && !sameFile(path, source) {', '\tif path != "" && path != source {'))
mutate("a file named through a symlink is the same file", "internal/config/config.go", ".", "TestEnsure_SameFileSpelledDifferentlyIsFine",
       swap('errB == nil && os.SameFile(infoA, infoB)', 'errB == nil && false && os.SameFile(infoA, infoB)'))
# One variable used in several places used to be reported as A, B, A, A: sorting and deduplicating
# are aimed at the call site
mutate("unset variables are sorted and each reported once", "internal/config/config.go", ".", "TestLoad_MissingVariablesListedOnceSorted",
       swap('"environment variables not set: %s", missingNames(missing))', '"environment variables not set: %s", strings.Join(missing, ", "))'))
# ./app --config with the value forgotten used to be ignored quietly and another configuration
# came up
mutate("a command-line flag with no value is an error", "internal/config/config.go", ".", "TestEnsure_FlagWithoutValueIsError",
       swap('\tif failed = danglingArg(); failed != nil {', '\tif failed = nil; failed != nil {'))
# What follows a flag and starts with a - is the next flag: taken as the value, --config
# --profile=dev read --profile=dev as the path and reported a file nobody named
mutate("the next flag is not taken as a value", "internal/config/locate.go", ".", "TestEnsure_FlagFollowedByAnotherFlagIsError",
       swap('return i+1 < len(args) && !strings.HasPrefix(args[i+1], "-")', 'return i+1 < len(args)'))
# Looking at the last argument alone leaves --config --profile=dev reported by nobody: fromArgs
# finds no value, and the conventional paths quietly bring up another configuration
mutate("every flag is checked for its value, not only the last", "internal/config/locate.go", ".", "TestEnsure_FlagFollowedByAnotherFlagIsError",
       swap('\tfor i, a := range args {\n\t\tif !strings.HasPrefix(a, "-") {\n\t\t\tcontinue\n\t\t}\n\t\tname := strings.TrimLeft(a, "-")\n\t\tfor _, key := range []string{ArgKey, ProfileArgKey} {',
            '\tfor i, a := range args[max(len(args)-1, 0):] {\n\t\tif !strings.HasPrefix(a, "-") {\n\t\t\tcontinue\n\t\t}\n\t\tname := strings.TrimLeft(a, "-")\n\t\tfor _, key := range []string{ArgKey, ProfileArgKey} {'))
# Passing a value or nil used to panic on the spot; Unmarshal checks before looking for the block,
# so it still reports when the block is not configured
mutate("an Unmarshal target that is not a pointer is an error", "internal/config/config.go", ".", "TestUnmarshalAndDecodeStrict_BadTargetIsErrorNotPanic",
       swap('func Unmarshal(key string, into any) error {\n\tif err := checkTarget(into); err != nil {',
            'func Unmarshal(key string, into any) error {\n\tif err := error(nil); err != nil {'))
mutate("a DecodeStrict target that is not a pointer is an error", "internal/config/strict.go", ".", "TestUnmarshalAndDecodeStrict_BadTargetIsErrorNotPanic",
       swap('func DecodeStrict(node *yaml.Node, target any) error {\n\tif err := checkTarget(target); err != nil {',
            'func DecodeStrict(node *yaml.Node, target any) error {\n\tif err := error(nil); err != nil {'))
mutate("a malformed configuration reports the same error on every read", "internal/config/config.go", ".", "TestUnmarshal_SameErrorOnEveryReadAfterLoadFailure",
       swap('\t\tfailed = err\n\t\treturn err', '\t\treturn err'))
mutate("Run hands the configuration back when it ends", "xone.go", ".", "TestRun_CanRunAgainAfterReturn", swap('\tdefer config.Reset()\n', ''))
mutate("a Run whose load failed hands the configuration back too", "xone.go", ".", "TestRun_NextRunReadsOwnConfigAfterLoadFailure",
       swap('\tdefer config.Reset()\n', ''),
       swap('''\tif err := config.Ensure(o.configPath, o.log()); err != nil {
\t\treturn errors.Join(err, stopWithin(o, nil))
\t}
''', '''\tif err := config.Ensure(o.configPath, o.log()); err != nil {
\t\treturn errors.Join(err, stopWithin(o, nil))
\t}
\tdefer config.Reset()
'''))

section("Startup output")
# The banner is printed in a terminal alone: in a container, redirected or behind a log collector,
# multi-line ASCII art is unparsable garbage in the logging platform
mutate("the banner is printed in a terminal alone", "banner.go", ".", "TestPrintBanner",
       swap('\tif !terminal {\n\t\treturn\n\t}\n', ''))
mutate("Run decides by stderr whether this is a terminal", "xone.go", ".", "TestRun_StderrNotTerminal_NoBanner",
       swap('printBanner(stderr, isTerminal(stderr))', 'printBanner(stderr, true)'))
mutate("a pipe and a file are not terminals", "banner.go", ".", "TestIsTerminal",
       swap('st.Mode()&os.ModeCharDevice != 0', 'st.Mode() == st.Mode()'))
mutate("a local replace shows the version as (devel)", "banner.go", ".", "TestModuleVersion",
       swap('\t\t\tif d.Replace != nil {\n\t\t\t\treturn "(devel)"\n\t\t\t}\n', ''))
# XONE_DEBUG: written only when it is explicitly on, holding how the loading went and the final
# configuration with the credentials masked
mutate("nothing is written with XONE_DEBUG off", "internal/config/debug.go", ".", "TestDebug_|TestEnsure_PrintsNothingWithoutXONE_DEBUG|TestRun_NoDebugOutputWithoutXONE_DEBUG",
       swap('\t}\n\treturn false\n}\n\n// DebugOut', '\t}\n\treturn true\n}\n\n// DebugOut'))
mutate("how the loading went is printed once it is done", "internal/config/config.go", ".", "TestEnsure_XONE_DEBUG",
       swap('\tdebugReport(path, from, r)\n', '\t_, _ = from, r\n'))
mutate("Run prints the order of the start hooks", "xone.go", ".", "TestRun_XONE_DEBUG",
       swap('\tdebugHooks(hook.Start())\n', ''))
mutate("a value under a credential key is masked whole", "internal/config/debug.go", ".", "TestRedacted",
       swap('if strings.Contains(k, s) {', 'if s == "" {'))
mutate("a password inside a URL is masked", "internal/config/debug.go", ".", "TestRedacted",
       swap('s = urlUserinfo.ReplaceAllString(s, "$1:"+redactedValue+"@")', 's = s'))
mutate("a password inside a MySQL DSN is masked", "internal/config/debug.go", ".", "TestRedacted",
       swap('s = mysqlDSN.ReplaceAllString(s, "$1:"+redactedValue+"@")', 's = s'))
mutate("a password written as password= is masked", "internal/config/debug.go", ".", "TestRedacted",
       swap('\treturn kvPair.ReplaceAllStringFunc(s, func(m string) string {', '\treturn s\n\treturn kvPair.ReplaceAllStringFunc(s, func(m string) string {'))
# token= and api_key= in a query string used to be printed as they were: only the three names
# password / passwd / pwd were recognized
mutate("a query string is masked by the credential word list", "internal/config/debug.go", ".", "TestRedacted_SensitiveQueryParamsMasked",
       swap('if sensitiveKey(name) || strings.EqualFold(name, "pwd") {', 'if strings.EqualFold(name, "password") || strings.EqualFold(name, "pwd") {'))
# Authorization and Signature carry a credential as surely as Password does
mutate("an Authorization or Signature key is masked too", "internal/config/debug.go", ".", "TestRedacted_AuthorizationAndSignature",
       swap('"apikey", "accesskey", "privatekey", "authorization", "signature",', '"apikey", "accesskey", "privatekey",'))
# The other side of the same line. The word list is matched against a configuration key here, not a
# request field, so cookie and session cannot come along: they would hide CookieDomain,
# SessionTimeout and beego's SessionOn, which is what somebody turned XONE_DEBUG on to read
mutate("the word list does not hide ordinary configuration", "internal/config/debug.go", ".", "TestRedacted_AuthorizationAndSignature",
       swap('"apikey", "accesskey", "privatekey", "authorization", "signature",', '"apikey", "accesskey", "privatekey", "authorization", "signature", "cookie", "session",'))
# A password holding @ or / used to be masked up to the first @ only, printing the rest along with
# the host as it was
mutate("a URL password is masked up to the last @", "internal/config/debug.go", ".", "TestRedacted_DSNPasswordMaskedUpToLastAt",
       swap('`(://[^:/?#@\\s]*):\\S*@`', '`(://[^:/?#@\\s]*):[^@/\\s]+@`'))
mutate("a MySQL DSN password is masked up to the last @", "internal/config/debug.go", ".", "TestRedacted_DSNPasswordMaskedUpToLastAt",
       swap('`^([^:@/\\s]+):\\S*@`', '`^([^:@/\\s]+):[^@\\s]+@`'))
# A query string value used to be masked all the way to the whitespace, taking the closing } of a
# ${VAR:default} and the path behind it along with it
mutate("the debug output stops masking a query value before a placeholder's closing brace", "internal/config/debug.go", ".", "TestEnsure_XONE_DEBUGPlaceholderDefaultMaskedKeepsClosingBrace",
       swap("('[^']*'|[^\\s&#;}]+)", "('[^']*'|[^\\s&#;]+)"))
# A credential written as a list or a map used to be printed as it was: only scalars were masked
mutate("a value under a credential key is masked whole whatever its shape", "internal/config/debug.go", ".", "TestRedacted_MasksWholeValueOfAnyShapeUnderSensitiveKey",
       swap('\t\t\t\tc.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: redactedValue}\n',
            '\t\t\t\tif v.Kind == yaml.ScalarNode {\n\t\t\t\t\tc.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: redactedValue}\n\t\t\t\t}\n'))
# A value expanded from ${VAR} used to be printed as it was, so under a key not called password (a
# DSN, a webhook) it leaked. Aimed at both call sites
mutate("the debug output shows a placeholder as it was written", "internal/config/debug.go", ".", "TestEnsure_XONE_DEBUGShowsPlaceholderTextNotExpandedValue",
       swap('redacted(r.root, r.raw)', 'redacted(r.root, nil)'))
mutate("loading hands the placeholder text to the debug output", "internal/config/config.go", ".", "TestEnsure_XONE_DEBUGShowsPlaceholderTextNotExpandedValue",
       swap('\tr.root, r.raw = root, values\n', '\tr.root = root\n'))
mutate("what is masked is a copy and the real configuration is left alone", "internal/config/debug.go", ".", "TestRedacted",
       swap('\t\tc.Content[i] = redacted(ch, raw)\n', '\t\tc.Content[i] = ch\n\t\tredacted(ch, raw)\n'))
mutate("an empty credential is not masked, so it is visible as unset", "internal/config/debug.go", ".", "TestRedacted",
       swap('; isEmptyNode(v) || v.Kind == yaml.ScalarNode && v.Value == "" {', '; false {'))
mutate("the final configuration carries no comments", "internal/config/debug.go", ".", "TestRedacted",
       swap('\tc.HeadComment, c.LineComment, c.FootComment = "", "", ""\n', ''))
# UseHandler: with the backend swapped, the xlog layer (AddKV, trace_id, the observers) still
# wraps around it
mutate("a handler of your own is still wrapped in xlog", "xlog/xlog.go", ".", "TestUseHandler_LogsGoToOwn",
       swap('withStatic(slog.New(newCtxHandler(*h)), identity(), c.Fields)', 'withStatic(slog.New(*h), identity(), c.Fields)'))
# Writing XLog.Level: debug while somebody else's handler decides the level looks like it took
# effect when it did not
mutate("UseHandler together with an XLog block fails startup", "xlog/xlog.go", ".", "TestUseHandler_XLogBlock",
       swap('if !reflect.DeepEqual(output, DefaultConfig()) {', 'if !reflect.DeepEqual(output, output) {'))
mutate("UseHandler has no effect once logging is installed", "xlog/xlog.go", ".", "TestUseHandler_LateCall",
       swap('\tif installed.Load() {\n', '\tif false {\n'))
mutate("UseHandler works again after a close", "xlog/xlog.go", ".", "TestUseHandler_UsableAgain",
       swap('\tinstalled.Store(false) // so UseHandler still works', '\t// so UseHandler still works'))
# The identity fields on every log: added while the framework wires things up, overridden by a
# Fields entry of the same name, and left out when empty
mutate("logs carry the identity fields by default", "xlog/xlog.go", ".", "TestIdentity_DefaultFields",
       swap('l, cl, err := build(c, identity())', 'l, cl, err := build(c, nil)'))
mutate("Fields overrides a default field of the same name", "xlog/xlog.go", ".", "TestFields_ExtraOverrideAndDrop",
       swap('if v, ok := fields[a.Key]; ok {', 'if v, ok := fields[a.Key]; ok && false {'))
mutate("a field with an empty string is not written", "xlog/xlog.go", ".", "TestIdentity_EmptyAppFields",
       swap('if a.Value.Kind() != slog.KindString || a.Value.String() != "" {', 'if true {'))
mutate("UseHandler carries the identity fields too", "xlog/xlog.go", ".", "TestUseHandler_CarriesIdentity",
       swap('slog.SetDefault(withStatic(slog.New(newCtxHandler(*h)), identity(), c.Fields))', 'slog.SetDefault(slog.New(newCtxHandler(*h)))'))
mutate("Fields written together with UseHandler is not a conflict", "xlog/xlog.go", ".", "TestUseHandler_CarriesIdentity",
       swap('\t\toutput.Fields = nil\n', ''))
# XApp comes along with the framework and xlog does not: a program importing only the root package
# can write an XApp and start, with slog.Default left alone.
# internal/coreonly imports the root package alone; were its tests to import xlog / xapp
# themselves, these two mutations would catch nothing
mutate("the root package brings xapp along", "xone.go", ".", "TestRun_XAppWorksWithCoreOnly",
       swap('\t_ "github.com/jupiter-eng/xone/xapp"\n', ''))
mutate("the root package does not bring xlog along", "xone.go", ".", "TestRun_CoreOnlyLeavesSlogDefaultAlone",
       swap('\t_ "github.com/jupiter-eng/xone/xapp"\n', '\t_ "github.com/jupiter-eng/xone/xlog"\n'))
mutate("an unread XLog names the import in the error", "xone.go", ".", "TestRun_XLogWithoutImportNamesTheImport",
       swap('slices.Contains(orphan, "XLog")', 'slices.Contains(orphan, "")'))
mutate("an XApp left with Profiles / Import alone is taken away with the block", "internal/config/source.go", ".", "TestLoad_XAppWithOnlyProfilesAndImportIsNotUnclaimed",
       swap('if val != nil && len(app.Content) == 0 {', 'if false {'))
mutate("the Import inside XApp is taken by the loader", "internal/config/source.go", ".", "TestLoad_XAppNameLeftToXapp_ProfilesAndImportTakenByLoader",
       swap('\tnode := takeFromApp(doc, ImportKey)\n', '\tnode := takeFromApp(doc.Content[0], ImportKey)\n'))

section("Shared by the web integrations: redaction and reading the body ahead")
# The promises of internal/web watched by its own tests. The promises about starting and stopping
# the server, the proxy ranges and the access log fields are watched by xgin's tests, and those
# mutations live in xgin.py (run under ./xgin, with the files here swapped in through the overlay)
mutate("a credential in a request header is masked", "internal/web/redact.go", ".", "TestRedact",
       swap('set[name] || ', ''), swap('\tset := headers()\n', '\tset := headers()\n\t_ = set\n'))
# Even with LogQuery on it has to be masked field by field: an ?access_token= reaching the log as
# it is means a credential on disk
mutate("a masked form value is written as the marker, not an escaped string", "internal/web/redact.go", ".", "TestRedactBody_Form",
       swap('return strings.ReplaceAll(values.Encode(), url.QueryEscape(Redacted), Redacted)', 'return values.Encode()'))
# A form or query value and a plain-text body may carry a URL / DSN credential: the JSON branch
# masks it, and another encoding should not send it to the log as it is
mutate("a URL / DSN credential in the remaining form values is masked", "internal/web/redact.go", ".", "TestRedactBody_CredentialsInFormValue",
       swap('\t\t\tvs[i] = redactCredentials(v)\n', '\t\t\tvs[i] = v\n'))
mutate("a URL / DSN credential in a plain-text body is masked", "internal/web/redact.go", ".", "TestRedactBody_CredentialsInFormValue",
       swap('return newlines.Replace(redactCredentials(s))', 'return newlines.Replace(s)'))
mutate("only a prefix of the request body is buffered", "internal/web/accesslog.go", ".", "TestSnapshotBody",
       swap('io.ReadAll(io.LimitReader(req.Body, maxRequestBody))','io.ReadAll(req.Body)',1))
mutate("an error while reading ahead is handed back downstream", "internal/web/accesslog.go", ".", "TestSnapshotBody",
       swap('\tif b.preErr != nil {\n\t\treturn 0, b.preErr\n\t}\n',''))
# Content-Type is case-insensitive: compared literally, the file contents of a
# Multipart/Form-Data reach the log whole
mutate("uploads and binary streams are not read, whatever the case", "internal/web/accesslog.go", ".", "TestSnapshotBody",
       swap('ct := strings.ToLower(req.Header.Get("Content-Type"))', 'ct := req.Header.Get("Content-Type")'))
# encoding/json matches field names by Unicode folding: {"ſecret":…} binds to Secret.
# With lowercasing alone, neither the field name comparison nor the pre-check recognizes it
mutate("field names are compared by Unicode folding", "internal/web/redact.go", ".", "TestRedactBody_Unicode",
       swap('b.WriteRune(foldRune(r))', 'b.WriteRune(unicode.ToLower(r))'))
# The pre-check and the field name comparison share sensitive, and the folding itself is watched
# by the one above; this one is aimed at the call site: with the pre-check written naively as a
# lowercasing, {"ſecret":…} takes the fast path and reaches the log as it is
mutate("the sensitive-word pre-check uses Unicode folding", "internal/web/redact.go", ".", "TestRedactBody_Unicode",
       swap('sensitive(s, words()) {\n\t\treturn Redacted\n\t}\n\t// No sensitive field name, while it may still carry a URL / DSN credential\n\t// (postgres://u:p@h), as RedactText does\n\treturn newlines', 'func(ws []string) bool { return slices.ContainsFunc(ws, func(w string) bool { return strings.Contains(strings.ToLower(s), w) }) }(words()) {\n\t\treturn Redacted\n\t}\n\t// No sensitive field name, while it may still carry a URL / DSN credential\n\t// (postgres://u:p@h), as RedactText does\n\treturn newlines'),
       swap('|| sensitive(body, ws)', '|| slices.ContainsFunc(ws, func(w string) bool { return strings.Contains(strings.ToLower(body), w) })'))
# path deliberately carries no query string, while Referer carries the previous page's whole URL
mutate("URL-like request headers have their query string removed", "internal/web/redact.go", ".", "TestRedactHeaders_URL",
       swap('\t\t\tv = stripURL(v)\n', ''))
# It used to be an exact match: new_password, client_secret and sessionToken reached the log as
# they were
mutate("a JSON field name containing a sensitive word is masked too", "internal/web/redact.go", ".", "TestRedactBody",
       swap('\t\t\tif sensitive(k, ws) {\n\t\t\t\tt[k] = Redacted', '\t\t\tif slices.Contains(ws, normalize(k)) {\n\t\t\t\tt[k] = Redacted'))
mutate("a form field name containing a sensitive word is masked too", "internal/web/redact.go", ".", "TestRedactBody",
       swap('\t\tif sensitive(k, ws) {\n\t\t\tvalues[k]', '\t\tif slices.Contains(ws, normalize(k)) {\n\t\t\tvalues[k]'))
# A list is never complete (Proxy-Authorization was once left off it), and the word list is the
# fallback layer
mutate("a request header name containing a sensitive word is masked too", "internal/web/redact.go", ".", "TestRedactHeaders",
       swap(' || sensitive(k, ws)', ''), swap('\tset := headers()\n\tws := words()\n', '\tset := headers()\n\tws := words()\n\t_ = ws\n'))
# If the pre-check does not recognize api-key, such a body takes the fast path and reaches the log
# as it is, never getting to the field-by-field redaction
mutate("the sensitive-word pre-check ignores separators", "internal/web/redact.go", ".", "TestRedactBody",
       swap('sensitive(s, words()) {\n\t\treturn Redacted\n\t}\n\t// No sensitive field name, while it may still carry a URL / DSN credential\n\t// (postgres://u:p@h), as RedactText does\n\treturn newlines', 'func(ws []string) bool { return slices.ContainsFunc(ws, func(w string) bool { return strings.Contains(strings.Map(foldRune, s), w) }) }(words()) {\n\t\treturn Redacted\n\t}\n\t// No sensitive field name, while it may still carry a URL / DSN credential\n\t// (postgres://u:p@h), as RedactText does\n\treturn newlines'),
       swap('|| sensitive(body, ws)', '|| slices.ContainsFunc(ws, func(w string) bool { return strings.Contains(strings.Map(foldRune, body), w) })'))
# decodeJSON opens a decoder twice (the whole body, then member by member), so both are turned off
mutate("redaction loses no precision on a large integer", "internal/web/redact.go", ".", "TestRedactBody",
       swap('\tdec.UseNumber()\n', '', 2))
mutate("redaction does not escape HTML characters", "internal/web/redact.go", ".", "TestRedactBody",
       swap('\tenc.SetEscapeHTML(false)\n', ''))
# A complete value followed by a raw dump: taking the value and saying nothing would treat the
# trailer as though it were not there, while it may be the part holding the credential
# io.EOF stays in the replacement: it is redact.go's only use of io, and dropping it would leave
# the mutant failing to compile rather than failing a test
mutate("JSON followed by anything else says it was not read to the end", "internal/web/redact.go", ".", "TestRedactBody_JSONWithTrailingDataDropsTheTrailer",
       swap('return v, dec.Decode(new(any)) == io.EOF', 'return v, dec.Decode(new(any)) != io.EOF || true'))

# A body the access log records is usually one the framework itself cut (MaxResponseBody,
# maxRequestBody), and mayContainField hands anything holding a backslash to the parser -- which the
# \" and \n of ordinary JSON always do. Masking all of it on a failed parse made every large JSON
# body read as "a credential was found here", with nothing of the response visible
mutate("a body that was cut is recorded as far as it was read", "internal/web/redact.go", ".", "TestRedactBody_ResponseCutAtTheCapIsStillReadable|TestRedactBody_JSONCutInsideAValueKeepsWhatWasRead",
       swap('\treturn partialValue(dec)\n', '\treturn nil, false\n'))
# Stopping at the top level leaves {"page":1,"items":[…]} -- every paged response there is --
# unreadable as soon as the cut falls inside the list
mutate("a cut inside a nested value keeps what was read around it", "internal/web/redact.go", ".", "TestRedactBody_JSONCutInsideANestedListKeepsTheElementsRead",
       swap('\t\t\t\treturn nonEmpty(out), false\n\t\t\t}\n\t\t\tv, whole := partialValue(dec)\n',
            '\t\t\t\treturn nonEmpty(out), false\n\t\t\t}\n\t\t\tvar v any\n\t\t\twhole := dec.Decode(&v) == nil\n'))
# A null value and a container that was cut before its first member both come back as a nil any,
# and going by the nil alone drops the field out of the log -- which reads as a response that never
# held it
mutate("a null value survives a cut", "internal/web/redact.go", ".", "TestRedactBody_JSONCutKeepsANullValue",
       swap('if whole || v != nil {\n\t\t\t\tout[name] = v', 'if v != nil {\n\t\t\t\tout[name] = v'))
# Nothing after the first decoding error was ever resolved into a token, so printing it would be
# printing raw bytes -- which is exactly what the backslash rule in mayContainField guards against
mutate("nothing after the cut is recorded", "internal/web/redact.go", ".", "TestRedactBody_JSONCutDropsEverythingAfterTheCut",
       swap('\t\t\tif !whole {\n\t\t\t\treturn nonEmpty(out), false\n\t\t\t}\n\t\t}\n\t\tif _, err := dec.Token(); err != nil { // the closing brace',
            '\t\t\tif !whole {\n\t\t\t\tout["rest"] = string(remaining(dec))\n\t\t\t\treturn nonEmpty(out), false\n\t\t\t}\n\t\t}\n\t\tif _, err := dec.Token(); err != nil { // the closing brace'),
       swap('// nonEmpty is v,', 'func remaining(dec *json.Decoder) []byte { b, _ := io.ReadAll(dec.Buffered()); return b }\n\n// nonEmpty is v,'))
mutate("a body that was not read to the end says so", "internal/web/redact.go", ".", "TestRedactBody_JSONCutInsideAValueKeepsWhatWasRead",
       swap('\treturn redacted + truncatedSuffix\n', '\treturn redacted\n'))
# Redacted means "a sensitive field was found and masked"; reading it for a body nothing could be
# read out of sends somebody looking for a credential that was never located
mutate("a body nothing could be read out of is marked as unparseable, not as masked", "internal/web/redact.go", ".", "TestRedactBody_JSONCutWithNothingReadableOmitsIt",
       swap('\t\treturn unparseableOmitted\n', '\t\treturn Redacted\n'))
# An empty {} in the log reads as a response that really was empty
mutate("a container cut before its first member is not recorded as empty", "internal/web/redact.go", ".", "TestRedactBody_JSONCutWithNothingReadableOmitsIt",
       swap('func nonEmpty[T map[string]any | []any](v T) any {\n\tif len(v) == 0 {\n\t\treturn nil\n\t}\n', 'func nonEmpty[T map[string]any | []any](v T) any {\n'))

section("Shared by the web integrations: client_ip and bounding the labels")
# xecho computes client_ip by the rules here (xgin uses gin's own, and xgin's
# TestClientIP_SharedRuleMatchesGin compares the two).
# Every part of the rule is gin v1.12.0's behavior, and changing one means client_ip changes when
# the same service switches frameworks
mutate("client_ip goes right to left and stops at the first untrusted address", "internal/web/proxy.go", ".", "TestProxies_ClientIP",
       swap('if i == 0 || !ps.contains(ip) {', 'if i == 0 {'))
mutate("several forwarded header lines are joined into one list", "internal/web/proxy.go", ".", "TestProxies_ClientIP",
       swap('ps.fromHeader(strings.Join(r.Header.Values(name), ","))', 'ps.fromHeader(r.Header.Get(name))'))
mutate("an unparsable entry in a forwarded header stops the walk leftwards", "internal/web/proxy.go", ".", "TestProxies_ClientIP",
       swap('\t\tif ip == nil {\n\t\t\tbreak\n\t\t}\n', '\t\tif ip == nil {\n\t\t\tcontinue\n\t\t}\n'))
mutate("X-Real-IP is used when X-Forwarded-For holds nothing usable", "internal/web/proxy.go", ".", "TestProxies_ClientIP",
       swap('var forwardedHeaders = []string{"X-Forwarded-For", "X-Real-IP"}', 'var forwardedHeaders = []string{"X-Forwarded-For"}'))
mutate("the method is bounded to a fixed set", "internal/web/request.go", ".", "TestNormalizeMethod",
       swap('\treturn MethodOther\n', '\treturn m\n'))
mutate("connection reset is recognized", "internal/web/request.go", ".", "TestIsBrokenPipe",
       swap(' || strings.Contains(msg, "connection reset by peer")', ''))
# The client is still waiting when a deadline the business set runs out: only net/http's
# cancellation counts as the client having gone
mutate("only Canceled counts as the client having gone", "internal/web/request.go", ".", "TestClientGone",
       swap('errors.Is(ctx.Err(), context.Canceled)', 'ctx.Err() != nil'))
# identity means no compression and is logged as usual
mutate("identity does not count as compressed", "internal/web/accesslog.go", ".", "TestAccessLog_EncodedResponseBodyOmitted",
       swap('if enc == "" || enc == "identity" {', 'if enc == "" {'))
# The errors net/http reports itself (a failed TLS handshake and so on) go through the standard
# library's log without an ErrorLog: they reach slog as INFO, with a different message every line
mutate("http.Server's ErrorLog is connected to slog", "internal/web/server.go", ".", "TestServer_ErrorLogRoutedToSlog",
       swap('\t\tErrorLog:          ErrorLog(module),\n', ''))

section("Redacting error texts")
# An error text is where a credential most easily hides (a whole DSN reported by a driver,
# "password=..."), so it goes through the word list before reaching the access log
mutate("the access log's errors field is redacted", "internal/web/accesslog.go", ".", "TestAccessLog_ErrorsFieldIsRedacted",
       swap('slog.String("errors", RedactText(a.Errors))', 'slog.String("errors", a.Errors)'))

# A driver's error often carries a whole DSN with not one sensitive word in it: going by the word
# list alone sends the password into the errors field and the Span as it is
mutate("a URL password inside an error text is masked", "internal/web/redact.go", ".", "TestRedactText_MasksPasswordInURLUserinfo",
       swap('\ts = urlUserinfo.ReplaceAllString(s, "$1:"+Redacted+"@")\n', ''))
mutate("a MySQL DSN password inside an error text is masked", "internal/web/redact.go", ".", "TestRedactText_MasksPasswordInMySQLDSN",
       swap('\treturn mysqlUserinfo.ReplaceAllString(s, "$1:"+Redacted+"@$2")\n', '\treturn s\n'))
# An unescaped @ inside a password: cutting at the first @ leaves the half behind it in the log
mutate("a password inside a URL goes by the last @", "internal/web/redact.go", ".", "TestRedactText_MasksPasswordInURLUserinfo",
       swap('`(://[^:/?#@\\s]*):[^/\\s]+@`', '`(://[^:/?#@\\s]*):[^@/\\s]+@`'))
# A sensitive name followed by = or : is a credential that can be located, so the value goes
mutate("the value under a sensitive name in an error text is masked", "internal/web/redact.go", ".", "TestRedactText_MasksTheValueUnderASensitiveName",
       swap('\tif ws := words(); sensitive(s, ws) {\n\t\ts = redactAssignments(s, ws)\n\t}\n', ''))
# The other half: masking the whole text on a sensitive word alone takes "token expired",
# "session not found" and "invalid signature" out of the logs, which are the messages wanted most
# while tracking an authentication failure down and carry no credential at all
mutate("a message that only names a credential is kept", "internal/web/redact.go", ".", "TestRedactText_KeepsAMessageThatOnlyNamesACredential",
       swap('\tif ws := words(); sensitive(s, ws) {\n\t\ts = redactAssignments(s, ws)\n\t}\n', '\tif sensitive(s, words()) {\n\t\treturn Redacted\n\t}\n'))
# Stopping at the first name that is not sensitive leaves every pair behind it in the clear:
# "retry: user=app password=..." begins with a name nobody masks
mutate("the scan carries on past a name that is not sensitive", "internal/web/redact.go", ".", "TestRedactText_MasksEveryPairOnTheLine",
       swap('\t\tif m[0] < last || !sensitive(s[m[2]:m[3]], ws) {\n\t\t\tcontinue\n\t\t}\n',
            '\t\tif m[0] < last {\n\t\t\tcontinue\n\t\t}\n\t\tif !sensitive(s[m[2]:m[3]], ws) {\n\t\t\tbreak\n\t\t}\n'))
# A value running past the line break swallows every error after this one, since gin's
# c.Errors.String() puts one error per line
mutate("a masked value stops at the end of its line", "internal/web/redact.go", ".", "TestRedactText_MasksOnlyToTheEndOfTheLine",
       swap('\t\tend := strings.IndexAny(s[m[1]:], "\\r\\n")\n\t\tif end < 0 {\n\t\t\tend = len(s)\n\t\t} else {\n\t\t\tend += m[1]\n\t\t}\n',
            '\t\tend := len(s)\n'))
# A body is data somebody sent, whose shape nobody here knows: the relaxation above is for the
# sentences this codebase writes, and must not reach a body
mutate("a body holding a sensitive word is still masked whole", "internal/web/redact.go", ".", "TestRedactOpaque_StillMasksAWholeBody",
       swap('func redactOpaque(body []byte) string {\n\ts := string(body)\n\tif sensitive(s, words()) {\n\t\treturn Redacted\n\t}\n',
            'func redactOpaque(body []byte) string {\n\ts := string(body)\n\tif ws := words(); sensitive(s, ws) {\n\t\ts = redactAssignments(s, ws)\n\t}\n'))
# gin's c.Errors.String() puts one error per line: removing the newlines as a body does would glue
# two errors into one sentence
mutate("the lines of an error text are separated by semicolons", "internal/web/redact.go", ".", "TestRedactText_KeepsLinesSeparated",
       swap('return textNewlines.Replace(strings.TrimRight(redactCredentials(s), "\\r\\n"))', 'return newlines.Replace(redactCredentials(s))'))

section("Shared by the web integrations: the proxy ranges")
# gin v1.12.0 reads a single ::ffff:10.0.0.1 as trusting ::1 and ::5, and ParseProxies does not
# Unmap a range: one line of configuration is understood differently in different places
mutate("a proxy range written in IPv4-mapped form is caught", "internal/web/proxy.go", ".", "TestValidateProxies_RejectsIPv4MappedEntries",
       swap('\t\tif plain, ok := unmapped(p); ok {', '\t\tif plain, ok := unmapped(p); ok && false {'))
mutate("the suggested spelling of a mapped range subtracts 96 bits", "internal/web/proxy.go", ".", "TestValidateProxies_RejectsIPv4MappedEntries",
       swap('pfx.Bits()-96', 'pfx.Bits()'))

section("Shared by the web integrations: redacting response headers and JSON values")
# A response header is a URL too: an OAuth callback's 302 Location carries ?code=, and the
# implicit grant carries #access_token=
mutate("the Location response header has its query string removed", "internal/web/redact.go", ".", "TestRedactHeaders_ResponseURL",
       swap('\t"location":         true,\n', ''))
# In a userinfo with only a user name (https://<token>@host) the name itself is the credential,
# while RedactText masks the password of a user:pass alone
mutate("a URL header has its userinfo removed", "internal/web/redact.go", ".", "TestRedactHeaders_ResponseURL",
       swap("\t\tif at := strings.LastIndexByte(host, '@'); at >= 0 {\n\t\t\tu = u[:i+3] + u[i+3+at+1:]\n\t\t}\n", ''))
# The call site: a header with no sensitive word in its name can still hold a whole DSN in its
# value
mutate("a request header value has the DSN password masked", "internal/web/redact.go", ".", "TestRedactHeaders_DSNInValue|TestRedactHeaders_ValuesNaming",
       swap('attrs = append(attrs, slog.String(k, redactCredentials(v)))', 'attrs = append(attrs, slog.String(k, v))'))
# A Cookie or Authorization inside a value is a header name, not a credential: a word-list rule over
# the value would mask the Vary and CORS headers whole. The mutation writes that rule out rather
# than reaching for RedactText, which locates a value by the name in front of it and would leave
# these three untouched anyway -- it is the whole-value rule that has to stay away from here
mutate("a request header value is not masked whole by a sensitive word", "internal/web/redact.go", ".", "TestRedactHeaders_ValuesNaming",
       swap('attrs = append(attrs, slog.String(k, redactCredentials(v)))',
            'if sensitive(v, ws) {\n\t\t\tattrs = append(attrs, slog.String(k, Redacted))\n\t\t\tcontinue\n\t\t}\n\t\tattrs = append(attrs, slog.String(k, redactCredentials(v)))'))
# gin's ShouldBindJSON ignores Content-Type: JSON sent as text/plain takes the plain-text branch
# and an escaped key name is not recognized
mutate("a JSON body without a JSON content type is masked as JSON", "internal/web/redact.go", ".", "TestRedactBody_JSONWithoutJSONContentType",
       swap('\tcase isJSON(body):\n\t\treturn redactJSON(body, cut)\n', ''))
mutate("a JSON string value goes through RedactText", "internal/web/redact.go", ".", "TestRedactBody_JSONStringValueDSN",
       swap('\tcase string:\n\t\treturn RedactText(t)\n', ''))
# With the fast path going by sensitive words alone, {"dsn":"postgres://u:p@h"} is never even
# parsed
mutate("JSON with an @ in a value does not take the fast path", "internal/web/redact.go", ".", "TestRedactBody_JSONStringValueDSN",
       swap("if !cut && !mayContainField(s, ws) && strings.IndexByte(s, '@') < 0 {", "if !cut && !mayContainField(s, ws) {"))
# The cut can take the very character the fast path goes by: a DSN cut before its @ holds no
# sensitive word and no @ either, and used to be written into the log exactly as it arrived
mutate("a body that may have been cut does not take the fast path", "internal/web/redact.go", ".", "TestRedactBody_CutDSNDoesNotTakeTheFastPath|TestRedactBody_NoSecretLeaksAtAnyCutPoint",
       swap("if !cut && !mayContainField(s, ws)", "if !mayContainField(s, ws)"))
# Aimed at the call sites: the flag is only ever true where the access log knows it had to stop at
# the cap, and computing it wrong is the same as not having it
mutate("the access log says which bodies it had to cut", "internal/web/accesslog.go", ".", "TestAccessLog_ABodyAsLongAsTheCapIsNotFastPathed",
       swap('len(a.ReqBody) >= maxRequestBody', 'false'), swap('len(a.RespBody) >= MaxResponseBody', 'false'))
mutate("the DSN regexp is skipped only when there is no @", "internal/web/redact.go", ".", "TestRedactText_MasksPassword",
       swap("\tif strings.IndexByte(s, '@') < 0 {\n\t\treturn s\n", "\tif strings.IndexByte(s, '@') >= 0 {\n\t\treturn s\n"))
# Leaving it empty has to mean 1.2 explicitly; falling through to tls.Config's zero value means
# whatever default the Go version brings
mutate("an empty MinVersion means 1.2", "internal/web/config.go", ".", "TestServerTLS_EmptyMinVersionMeansTLS12",
       swap('"": tls.VersionTLS12, ', ''))
