# core 的变异：根模块（核心） 里的承诺。写法见 scripts/mutations/__init__.py
from . import cut, mutate, section, swap

section("配置")
mutate("日志按配置的时区渲染", "xlog/xlog.go", ".", "TestNew_RendersTimeInConfiguredTimezone|TestNew_FailsWhenTimezoneCannotLoad",
       swap('ReplaceAttr: inLocation(loc)}', 'ReplaceAttr: nil}\n\t_ = loc'))
# 同一个进程里再跑一次 Run：location 只在配了时才写的话，上一次的时区一直留着
mutate("没配时区时清掉上一次的", "xlog/xlog.go", ".", "TestInitXLog_TimezoneClearedWhenLaterRunLeavesItUnset",
       swap('\tloc, _ := parseLocation(c.Timezone)\n\tlocation.Store(loc)\n',
            '\tif loc, _ := parseLocation(c.Timezone); loc != nil {\n\t\tlocation.Store(loc)\n\t}\n'))
mutate("UseHandler 时清掉上一次的时区", "xlog/xlog.go", ".", "TestInitXLog_TimezoneClearedWhenLaterRunLeavesItUnset",
       swap('\t\tlocation.Store(nil) // 时间戳由你的 handler 决定，xlog 没有时区\n', ''))
mutate("时区加载不到直接失败", "xlog/xlog.go", ".", "TestNew_FailsWhenTimezoneCannotLoad",
       swap('''\tloc, err := time.LoadLocation(name)
\tif err != nil {''', '''\tloc, err := time.LoadLocation(name)
\tif false {'''))
# 文件名最细到分钟：0s 实际每分钟一个文件、30s 两个周期撞同一个名字，都是配了 A 跑的是 B
mutate("轮转周期短于一分钟启动失败", "xlog/config.go", ".", "TestNew_FailsFastOnRotationPeriodUnderOneMinute",
       swap('if c.RotateTime < time.Minute {', 'if c.RotateTime < 0 {'))
mutate("MaxAge 为负启动失败", "xlog/config.go", ".", "TestNew_MaxAge", swap('if c.MaxAge < 0 {', 'if false {'))
# 校验函数本身是对的不算数，New 得真的调它
mutate("New 真的校验了文件输出的配置", "xlog/xlog.go", ".", "TestNew",
       swap('\tif err := cfg.File.validate(); err != nil {\n\t\treturn nil, nil, xerror.New("xlog", "config", err)\n\t}\n', ''))
# 配置写错报 config、运行环境出错报 new：告警按 op 分得清该改配置还是该查机器
mutate("日志级别写错报 config", "xlog/xlog.go", ".", "TestNew_FailsFastOnLevelTypo",
       swap('xerror.Newf("xlog", "config",\n\t\t\t"unknown log level', 'xerror.Newf("xlog", "new",\n\t\t\t"unknown log level'))
mutate("日志格式写错报 config", "xlog/xlog.go", ".", "TestNew_FailsFastOnFormatTypo",
       swap('xerror.Newf("xlog", "config",\n\t\t\t"unknown log format', 'xerror.Newf("xlog", "new",\n\t\t\t"unknown log format'))
mutate("Perm 认 0o644 写法", "xlog/xlog.go", ".", "TestParsePerm", swap('strings.TrimPrefix(s, "0o")', 's'))
# 关掉文件之后 slog.Default() 还指着它的话，Run 返回的错误、被丢下的停止钩子打的日志全没了
mutate("关掉日志文件之后改写 stderr", "xlog/xlog.go", ".", "TestCloseXLog",
       swap('\tslog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))\n\treturn closer.Close()', '\treturn closer.Close()'))
# {Name} 上已经有普通文件时，Rename 会把它原子地换成符号链接，旧日志一声不响就没了
mutate("同名普通文件不被符号链接覆盖", "xlog/rotate.go", ".", "TestRotateWriter",
       cut('\tif occupied(w.linkName) {\n\t\treturn nil, xerror.Newf(', 'change File.Name", w.linkName)\n\t}\n'))
mutate("运行中链接位置被占了也不覆盖", "xlog/rotate.go", ".", "TestRotateWriter",
       cut('\tif occupied(w.linkName) {\n\t\twarnf(', 'not updating the link", w.linkName)\n\t\treturn\n\t}\n'))
# app.log.bak、app.log.1.gz 都匹配 base.*，按 mtime 判断就会被当成过期日志删掉
mutate("清理只删自己命名的文件", "xlog/rotate.go", ".", "TestRotateWriter",
       swap('\t\treturn !t.Add(period).After(cutoff)\n\t}\n\treturn false\n}', '\t\treturn !t.Add(period).After(cutoff)\n\t}\n\treturn true\n}'))
mutate("换过粒度之后旧文件照样清", "xlog/rotate.go", ".", "TestRotateWriter",
       swap('\t\tt, err := time.ParseInLocation(l.layout, suffix, w.clock().Location())',
     '\t\tif l.layout != w.layout {\n\t\t\tcontinue\n\t\t}\n\t\tt, err := time.ParseInLocation(l.layout, suffix, w.clock().Location())'))
# 旧粒度的文件按现在的 RotateTime 算周期的话，从按天改成按小时之后，
# 今天那个还在写的按天文件零点一过就被当成过期删掉
mutate("旧粒度的文件按它自己那一档的周期算过期", "xlog/rotate.go", ".", "TestRotateWriter",
       swap('\t\tperiod := l.period\n\t\tif l.layout == w.layout {\n\t\t\tperiod = w.rotate\n\t\t}\n', '\t\tperiod := w.rotate\n'))
# 只在轮转时清的话，按天轮转、一天重启几次的服务永远等不到那次轮转
mutate("打开时就清理一次过期文件", "xlog/rotate.go", ".", "TestRotateWriter",
       swap('\tw.purge(w.currentName)\n\treturn w, nil', '\treturn w, nil'))
# CtxWithKV 要派生一份副本：直接往父作用域里写，兄弟之间就串了、访问日志也被污染
mutate("CtxWithKV 不写回父 ctx", "internal/logext/scope.go", ".", "TestCtxWithKV",
       swap('return context.WithValue(ctx, ctxScopeKey{}, parent.copyWith(kvs))', 'parent.AddAll(kvs)\n\treturn context.WithValue(ctx, ctxScopeKey{}, parent)'))
mutate("CtxWithKV 带着父 ctx 已有的字段", "internal/logext/scope.go", ".", "TestCtxWithKV",
       swap('\tmaps.Copy(c.kv, s.kv)\n', ''))
mutate("片段也有 profile 变体", "internal/config/source.go", ".", "TestLoad",
       swap('nested, err := fileSet(target, d, false, profiles, seen, depth+1)',
     'nested, err := withImports(target, d, profiles, seen, depth+1)'))
mutate("重试的等待逐次翻倍", "xutil/retry.go", ".", "TestRetry", swap('\t\t\tbackoff = nextBackoff(backoff)\n', ''))
mutate("预算按退避上界算", "xutil/retry.go", ".", "TestRetryBudget",
       swap('''\tbudget := timeout * time.Duration(attempts)
\tbackoff := min(interval, maxBackoff) // 与 Retry 一致：第一次也封顶
\tfor i := 1; i < attempts; i++ {
\t\tbudget += backoff
\t\tbackoff = nextBackoff(backoff)
\t}
\treturn budget''', '\treturn timeout*time.Duration(attempts) + interval*time.Duration(attempts-1)'))
# 文档说每次最多等 maxBackoff，第一次等待从前原样用 interval
mutate("第一次等待也封顶", "xutil/retry.go", ".", "TestRetry_FirstWaitAlsoCapped",
       swap('\tvar last error\n\tbackoff := min(interval, maxBackoff)', '\tvar last error\n\tbackoff := interval'))
mutate("预算里的第一次退避也封顶", "xutil/retry.go", ".", "TestRetryBudget_FirstBackoffAlsoCapped",
       swap('\tbackoff := min(interval, maxBackoff) // 与 Retry 一致', '\tbackoff := interval // 与 Retry 一致'))
mutate("重试的等待带抖动", "xutil/retry.go", ".", "TestJitter",
       swap('\treturn time.Duration(rand.Int64N(int64(d) + 1))', '\treturn d - time.Duration(rand.Int64N(2))'))
# 密码错、库不存在也照样重试满整轮，启动白白拖长几十秒
mutate("永久错误不再重试", "xutil/retry.go", ".", "TestRetry_NoRetryOnPermanentError|TestRetry_RecognizesWrappedPermanentError",
       swap('errors.As(last, &p) {', 'errors.As(last, &p) && false {'))
mutate("永久错误返回去掉标记的原错误", "xutil/retry.go", ".", "TestRetry_NoRetryOnPermanentError|TestRetry_RecognizesWrappedPermanentError",
       swap('\t\t\treturn p.err\n\t\t}\n', '\t\t\treturn last\n\t\t}\n'))
# 退避期间被取消时只报上一次的「连不上」，启动路径会把按要求退出当成故障
mutate("退避期间被取消时如实报告取消", "xutil/retry.go", ".", "TestRetry",
       swap('return fmt.Errorf("%w, last attempt failed: %w", err, last)', 'return fmt.Errorf("%v, last attempt failed: %w", err, last)'))
# yaml.v3 对没写 tag 的字段只认全小写：照着字段名写，报的是「field Endpoint not found」，
# 字段明明就叫这个。提示丢了的话，使用者只能对着一个自相矛盾的报错发愣
mutate("字段没写 tag 时报错说怎么改", "internal/config/strict.go", ".", "TestDecodeStrict",
       swap('\tif s.untagged[key] {', '\tif false {'))
# 字段从平铺挪进子块（XGin 的 CertFile 挪进 TLS:）之后，照旧写法写的该看到往哪挪
mutate("字段写在了上一层时报错指出挪进哪个块", "internal/config/strict.go", ".", "TestDecodeStrict_KeyOneLevelTooHigh",
       swap('} else if block := s.blockOf(key); block != "" {', '} else if block := ""; block != "" {'))
mutate("好几个子块都有这个字段时不猜", "internal/config/strict.go", ".", "TestDecodeStrict_KeyOneLevelTooHigh",
       swap('\t\t\tif found != "" {\n\t\t\t\treturn ""\n\t\t\t}\n', ''))
# yaml 的类型错误带着值的前几个字符，${VAR} 又是凭证的推荐写法：密码填错了字段，
# 它的一截就进了启动日志。调用点和「记下展开了哪些值」各打一条
mutate("占位符展开出来的值不进报错", "internal/config/strict.go", ".", "TestUnmarshal_ExpandedPlaceholderValueNotInError",
       swap('e = c.at(n) + ": " + c.redact(n, msg)', 'e = c.at(n) + ": " + msg'))
mutate("加载时记下占位符展开出来的值", "internal/config/config.go", ".", "TestUnmarshal_ExpandedPlaceholderValueNotInError",
       swap('\texpand(root, &missing, values)', '\texpand(root, &missing, nil)'))
# 变量的值恰好是 null / ~ 时，重新判定把它当成「没写」，字段悄悄留在默认值上
mutate("展开出来的 null 写法仍是字符串", "internal/config/config.go", ".", "TestLoad_PlaceholderNullLiteralIsNotTreatedAsUnset",
       swap('\t\tif n.ShortTag() == "!!null" {\n\t\t\tn.Tag = "!!str"\n\t\t}\n', ''))
# 严格解码从前是把节点序列化成文本再解的：报的是那段文本的行号，也没有文件名。
# 现在每条报错都记在节点名下——拼错记在 key 上、类型错误记在值上，
# 「记下节点来自哪个文件」、合并出来的副本跟着第一个 key 走，各打一条
mutate("字段拼错时报的是那个 key 的行号", "internal/config/strict.go", ".", "TestLoad_BadFieldReportsFileAndLine",
       swap('c.errs = append(c.errs, c.at(k)+": "+s.notFound(k.Value, t))', 'c.errs = append(c.errs, c.at(n)+": "+s.notFound(k.Value, t))'))
mutate("类型错误也报出配置文件和那一行", "internal/config/strict.go", ".", "TestLoad_BadFieldReportsFileAndLine",
       swap('if msg, ok := strings.CutPrefix(e, fmt.Sprintf("line %d: ", n.Line)); ok {', 'if msg, ok := e, false; ok {'))
mutate("字段写错时报出是哪个文件", "internal/config/config.go", ".", "TestLoad_BadFieldReportsFileAndLine",
       swap('remember(f.node, f.path, from)', 'remember(f.node, "", from)'))
mutate("合并出来的块跟着第一个 key 认文件", "internal/config/strict.go", ".", "TestLoad_BadFieldReportsFileAndLine",
       swap('for ; n != nil; n = first(n) {', 'for ; n != nil; n = nil {'))
mutate("字段拼错要启动失败", "internal/config/strict.go", ".", "TestLoad",
       swap('\t\tif ft == nil {\n\t\t\tc.errs = append(c.errs, c.at(k)+": "+s.notFound(k.Value, t))\n',
            '\t\tif ft == nil {\n'))
# 「写明的 key 压过 << 并进来的」从前在几个文件合并完之后才由解码器套用，
# profile 文件用 << 写的值于是输给了 base 里写明的同名 key。打在调用点和两条规则上
mutate("<< 在每个文件里就摊平", "internal/config/source.go", ".", "MergeKey|TestLoad_Anchor|TestLoad_TypoInMerged",
       cut('\tif err := flattenMerges(out); err != nil {', 'return nil, xerror.Newf("xconfig", "config", "config %s: %w", path, err)\n\t}\n'))
mutate("写明的 key 压过 << 并进来的", "internal/config/source.go", ".", "TestLoad_MergeKeyRulesWithinOneFile",
       swap('\t\t\town[n.Content[i].Value] = true\n', '\t\t\t_ = own\n'))
mutate("<< 的列表里靠前的压过靠后的", "internal/config/source.go", ".", "TestLoad_MergeKeyRulesWithinOneFile",
       swap('\t\t\t\t\town[key] = true // 靠前的来源压过靠后的\n', ''))
# 一个文件里 --- 隔开的第二份文档从前被 yaml.Unmarshal 静默丢掉，连 Unclaimed 都看不见。
# 调用点、报错、「空文档不算一份」各打一条
mutate("每个文件都查多份文档", "internal/config/source.go", ".", "MultipleDocuments",
       swap('\tdoc, err := onlyDocument(raw)\n', '\tvar doc yaml.Node\n\terr := yaml.Unmarshal(raw, &doc)\n'))
mutate("第二份文档启动失败", "internal/config/source.go", ".", "MultipleDocuments",
       swap('\t\tif found {\n\t\t\treturn yaml.Node{}, fmt.Errorf("multiple', '\t\tif false {\n\t\t\treturn yaml.Node{}, fmt.Errorf("multiple'))
mutate("结尾多一个空的 --- 照常加载", "internal/config/source.go", ".", "TestLoad_LeadingOrTrailingDocumentMarkerStillLoads",
       swap('if len(d.Content) == 0 || isNull(d.Content[0]) {', 'if len(d.Content) == 0 {'))
# 未知字段是自己按类型查的，认字段的规则一处和 yaml.v3 不一样，要么拼错放行、要么合法的配置起不来
mutate(",inline 的结构体摊平来认", "internal/config/strict.go", ".", "TestDecodeStrict_FieldRulesMatchYAML",
       swap('\t\t\t\ts.add(ft) //', '\t\t\t\t_ = ft //'))
mutate(",inline 的 map 收下认不出的 key", "internal/config/strict.go", ".", "TestDecodeStrict_FieldRulesMatchYAML",
       swap('\t\t\t\ts.inline = ft.Elem()', '\t\t\t\t_ = ft'))
# 自己会解的元素要在检查那一遍里就试解：等到最后才解的话，外面有一处写错，
# 元素里的问题要改完、重启一次才看得见
mutate("集合元素里的问题和外面的一起报", "internal/config/strict.go", ".", "TestLoad_CollectionElementErrorsReportedWithOthers",
       swap('reflect.PointerTo(t).Implements(obsoleteType):\n\t\treturn c.try(n, t)',
            'reflect.PointerTo(t).Implements(obsoleteType):\n\t\treturn nil'))
mutate("占位符按替换后的内容判定类型", "internal/config/config.go", ".", "TestLoad",
       swap('\t\tif n.Value != before {\n\t\t\tretag(n)\n', '\t\tif n.Value != before {\n'))
# 合并按名字对齐 key，重复在那一步就被吞掉了，所以只能在每个文件解析时查。
# 打在调用点上：从前只有第一个文件的重复查得出来，profile 和 import 进来的全漏
mutate("每个文件里的重复 key 都要报错", "internal/config/source.go", ".", "TestLoad",
       cut('\tif err := checkDuplicates(&doc); err != nil {', 'return nil, xerror.Newf("xconfig", "config", "config %s: %w", path, err)\n\t}\n'))
# 只有注释的 profile 文件从前把前面所有文件整个替换掉，全部配置退回默认值
mutate("空文件叠上来不改变任何东西", "internal/config/merge.go", ".", "TestLoad",
       swap('return n == nil || n.Kind == 0 || isNull(n)', 'return n == nil || isNull(n)'))
mutate("叠加文件里的 null 不覆盖低优先级的值", "internal/config/merge.go", ".", "TestLoad",
       swap('return n == nil || n.Kind == 0 || isNull(n)', 'return n == nil || n.Kind == 0'))
mutate("占位符展开为空时保持默认值", "internal/config/config.go", ".", "TestLoad",
       swap('\tcase n.Value == "":\n\t\tn.Tag = "!!null"\n', '\tcase n.Value == "":\n'))
mutate("锚点可以跨顶层块引用", "internal/config/source.go", ".", "TestLoad",
       swap('\tbudget := maxResolvedNodes\n\tout, err := resolveAliases(&doc, &budget)', '\tout, err := &doc, error(nil)'))
mutate("XApp.Profiles 收逗号分隔的字符串", "internal/config/config.go", ".", "TestLoad",
       swap('out = append(out, splitProfiles(a)...)', 'out = append(out, a)'))
mutate("XApp.Profiles 里的占位符会展开", "internal/config/config.go", ".", "TestLoad",
       swap('''\tvar missing []string
\texpand(node, &missing, nil)
\tif len(missing) > 0 {
\t\treturn nil, xerror.Newf("xconfig", "config",
\t\t\t"environment variables not set in %s.%s of %s: %s", AppKey, ProfilesKey, path, missingNames(missing))
\t}
''', ''))
mutate("被引进来的文件里写了 XApp.Profiles 就报错", "internal/config/config.go", ".", "TestLoad",
       swap('if takeFromApp(f.node, ProfilesKey) != nil {',
     'if p, _ := profilesOf(f.node, f.path); len(p) > 0 {'))
# config_schema.json 从前在编辑器里一个字段拼错都标不出来，单实例 / 多实例两种写法
# 却全被标红——下面四条各守一处它和运行时对不上的地方
# map 的 value 从零值开始解：不逐个铺默认值，多实例里没写的字段全成了零值
mutate("多实例写法里没写的字段保持默认", "internal/config/clients.go", ".", "TestUnmarshalClients",
       swap('\tc := defaults()\n', '\tvar c C\n'))
# node.Decode 不带严格检查，「字段拼错就启动失败」在多实例里悄悄失效
mutate("多实例写法里拼错也报错", "internal/config/clients.go", ".", "TestUnmarshalClients",
       swap('if err := DecodeStrict(n, &c); err != nil {', 'if err := n.Decode(&c); err != nil {'))
# 实例的报错点名是哪个实例，并且用 %w 保住 yaml 的类型错误
mutate("多实例写法里拼错时点名实例、保住类型错误", "internal/config/clients.go", ".", "TestUnmarshalClients_TypoInInstanceReportsFileAndLine",
       swap('fmt.Errorf("%s.%s: %w", clientsKey, name, err)', 'fmt.Errorf("%s.%s: %v", clientsKey, name, err)'))
mutate("多实例的每个实例都调一次 Validate", "internal/config/clients.go", ".", "TestUnmarshalClients_CallsValidateForEachInstance",
       swap('any(&c).(interface{ Validate() error })', 'any(c).(interface{ Validate() error })'))

section("启动与退出")
# xone.Func / UntilSignal：不写类型也能交给 Run 的两个 Runnable
mutate("Func 的错误就是 Run 的错误", "xone.go", ".", "TestFunc_", swap('func (f funcRunnable) Start(ctx context.Context) error { return f(ctx) }', 'func (f funcRunnable) Start(ctx context.Context) error { _ = f(ctx); return nil }'))
mutate("Func(nil) 当场 panic", "xone.go", ".", "TestFunc_", swap('\tif fn == nil {\n\t\tpanic("xone: Func needs', '\tif false {\n\t\tpanic("xone: Func needs'))
mutate("UntilSignal 阻塞到退出信号", "xone.go", ".", "TestUntilSignal", swap('\t\t<-ctx.Done()\n\t\treturn nil\n', '\t\treturn nil\n'))
# Stop 是可选的：实现了才调。打在调用点上——绕开类型断言，写了 Stop 的服务就停不下来
mutate("写了 Stop 的才调它", "xone.go", ".", "TestRun_ClosesInReverseOrder|TestRun_StartOnlyRunnableWorks",
       swap('\tif s, ok := r.(stopper); ok {\n', '\tif s, ok := any(nil).(stopper); ok {\n'))
# Stop 签名写错编译器不拦，那个 Stop 就永远不会被调到
mutate("Stop 签名写错时直接报错", "xone.go", ".", "TestRun_StopWithWrongSignatureFails",
       swap('if m, ok := t.MethodByName("Stop"); ok {', 'if m, ok := t.MethodByName("NoSuchMethod"); ok {'))
mutate("Stop 写在指针上却传了值时直接报错", "xone.go", ".", "TestRun_StopOnPointerButValuePassedFails",
       swap('reflect.PointerTo(t).MethodByName("Stop")', 'reflect.PointerTo(t).MethodByName("NoSuchMethod")'))
# Stop 看着截止时间返回、Start 紧跟着返回时两边同时就绪，select 随机挑：
# 挑中 Done 那一支就丢了 Start 的错误，还白告警一条「服务没退出」
mutate("到点时先看一眼 Start 返回了没有", "xone.go", ".", "TestRun_StartErrorKeptWhenStopReturnsAtDeadline",
       swap('\t\t\tselect {\n\t\t\tcase e := <-runErr:\n\t\t\t\tfirst = errors.Join(first, e)\n\t\t\tdefault:\n',
            '\t\t\tselect {\n\t\t\tdefault:\n'))
mutate("列表整体替换不逐元素合并", "internal/config/merge.go", ".", "TestLoad",
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
# 把这份文档挪到它 import 的那些后面，优先级就反过来了。
# 带上那行注释是为了锚定唯一一处：fileSet 里有一段一模一样的
# `out = append(out, nested...)` + return
mutate("import 进来的压过引它的", "internal/config/source.go", ".", "TestLoad",
       swap('\tout := []loaded{{path: path, node: doc}}\n', '\tvar out []loaded\n'),
       swap('''\t\t// import 进来的文件同样有 profile 变体，但变体不存在不算错
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
# 第三个参数是 variantRequired：主文件的 profile 变体必须存在
mutate("profile 文件不存在直接失败", "internal/config/source.go", ".", "TestLoad",
       swap('files, err := fileSet(base, doc, true, active, seen, 0)', 'files, err := fileSet(base, doc, false, active, seen, 0)'))
mutate("第二个信号能终止卡住的进程", "xone.go", ".", "TestRun",
       swap('\t\t\tsignal.Stop(ch)\n\t\t\to.log().Info(','\t\t\to.log().Info(',1))
# Stop 要能真的做事。沿用被取消的 ctx 的话，每个关闭动作一进去就被拒绝——
# 在途请求没做完、注册中心那条记录没注销，等于没有优雅退出这回事
mutate("Stop 拿到的 ctx 不继承那次取消", "xone.go", ".", "TestRun_StopCtxDoesNotInheritCancellation",
       swap('context.WithTimeout(context.WithoutCancel(ctx), o.stopTimeout)','context.WithTimeout(ctx, o.stopTimeout)'))
# 到点之后不留那一截余量：看着截止时间返回的 Stop 报的错被丢掉，进程以 0 退出
mutate("看着截止时间返回的 Stop 报的错不丢", "xone.go", ".", "TestRun_DeadlineAwareStopErrorIsKept",
       swap('case <-time.After(stopGrace):', 'default:'))
# Stop 收了 ctx 却不看它：同步调的话 Run 永远返回不了，一个停止钩子都轮不到
mutate("不看 ctx 的 Stop 挂不住退出", "xone.go", ".", "TestRun_StopIgnoringCtxDoesNotHangExit",
       swap('errors.Join(first, stopServer(serverCtx, o, s))', 'errors.Join(first, safe("stop", func() error { return s.Stop(serverCtx) }))'))
# 服务只能用预算的前 2/3。让它用满整份的话，不肯退出的服务把时间吃光，
# 每个停止钩子一进去就判超时，Close() 被扔进没人等的协程，资源全留在原地
mutate("服务吃不掉留给停止钩子的那一段", "xone.go", ".", "TestShutdown|TestRun_ClosesOthersWhenServiceWontExit",
       swap('context.WithTimeout(stopCtx, o.stopTimeout-o.stopTimeout/3)', 'context.WithTimeout(stopCtx, o.stopTimeout)'))
# K8s 发 SIGTERM，Ctrl-C 发 SIGINT。漏掉任何一个，那条路径上就没有优雅退出
mutate("两种退出信号都监听", "xone.go", ".", "TestRun_ListensForBothExitSignals",
       swap('[]os.Signal{syscall.SIGINT, syscall.SIGTERM}','[]os.Signal{syscall.SIGTERM}'))
# xlog 是在启动钩子里换掉全局 logger 的。Run 开头把它捕获一次的话，
# 之后所有框架日志都还写在旧的那个上——服务起来了，初始化日志一行看不到
mutate("框架日志每次重新取全局 logger", "xone.go", ".", "TestRun_FrameworkLogsFollowGlobalLoggerSwappedByHook",
       swap('\tctx, stopSignals := notifyShutdown(o)','\to.logger = o.log()\n\tctx, stopSignals := notifyShutdown(o)'))
mutate("停止钩子受停止预算约束", "xone.go", ".", "TestShutdown",
       swap('err := runWithin(hookCtx, e)', 'err := safeHook(hookCtx, e)'))
# 一个卡住的钩子能用到整个截止时间的话，后面的钩子揣着过期的 ctx 进去当场判超时，
# 最后关的日志连关文件都来不及
mutate("钩子之间给排在后面的各留一份", "xone.go", ".", "TestShutdown",
       swap('end.Add(-time.Duration(len(todo)-1-i)*reserve)', 'end.Add(-time.Duration(len(todo)-1-i)*reserve*0)'))
mutate("启动期间收到信号就不启动服务", "xone.go", ".", "TestRun",
       swap('started, err := runStart(ctx, o)','started, err := runStart(context.Background(), o)'))
mutate("出错时问得出是谁报的", "xone.go", ".", "TestRun",
       swap('xerror.Newf("xone", "start", "%s: %w", e.Name, err)', 'xerror.Newf("xone", "start", "%s: %v", e.Name, err)'))
# Run 返回的是 errors.Join。只顺着单链往里走的话，Join 里排在后面的兄弟
# 从来没被看过：Is(Join(xgin 的错, xgorm 的错), "xgorm") 是 false
mutate("xerror.Is 走遍 Join 的每个分支", "xerror/xerror.go", ".", "TestIs",
       cut('\tcase interface{ Unwrap() []error }:\n', '\t\t\t\treturn true\n\t\t\t}\n\t\t}\n'))
# 一条错误只有一个框：从前每层都带完整的「xone {module} {op} failed, err=[...]」，
# 根因被埋在最里面的括号里。下面三条各守一处
mutate("同模块的里层只留 op 和原因", "xerror/xerror.go", ".", "TestNewf|TestNew_",
       swap('\tif !n.same {\n\t\treturn n.e.render(false)\n\t}', '\tif true {\n\t\treturn n.e.render(false)\n\t}'))
mutate("里层不再重复 xone 前缀", "xerror/xerror.go", ".", "TestNewf|TestNew_",
       swap('return n.e.render(false)', 'return n.e.render(true)'))
mutate("Newf 折叠参数里的 xerror", "xerror/xerror.go", ".", "TestNewf",
       swap('\t\t\town[i] = nest(module, xe)\n', '\t\t\town[i] = xe\n'))
# var e *Error 没判空就被包一层：报错路径上的 panic 会把真正的故障盖掉
mutate("New 收到带类型的 nil 不 panic", "xerror/xerror.go", ".", "TestNew_TypedNilTreatedAsNoCauseNotPanic",
       swap('\t\tif xe == nil {\n\t\t\treturn &Error{Module: module, Op: op}\n\t\t}\n', ''))
mutate("Newf 参数里带类型的 nil 不 panic", "xerror/xerror.go", ".", "TestNewf_TypedNilArgRendersAsNilNotPanic",
       swap('if xe, ok := a.(*Error); ok && xe != nil {', 'if xe, ok := a.(*Error); ok {'))
mutate("Is 遇到带类型的 nil 不 panic", "xerror/xerror.go", ".", "TestNewf_TypedNilArgRendersAsNilNotPanic",
       swap('\t\tif xe == nil {\n\t\t\treturn false // 带类型的 nil', '\t\tif false {\n\t\t\treturn false // 带类型的 nil'))
mutate("同模块再包一层原样返回", "xerror/xerror.go", ".", "TestNew_",
       swap('\t\tif xe.Module == module {\n\t\t\treturn xe\n\t\t}\n', ''))
mutate("建连重试可以被取消", "xutil/retry.go", ".", "TestRetry",
       swap('\tif err := parent.Err(); err != nil {\n\t\treturn err\n\t}\n\n',''))
# panic 出来的是 error 时要用 %w 接住，否则 errors.Is 问不出根因。打在两个调用点上
mutate("建实例 panic 出来的 error 留在链上", "internal/xclient/xclient.go", ".", "TestBuild_new_panic",
       swap('"instance %q %w", name, panicked(r))', '"instance %q panicked: %v", name, r)'))
mutate("关实例 panic 出来的 error 留在链上", "internal/xclient/xclient.go", ".", "TestClose_panic",
       swap('"close %w", panicked(r))', '"close panicked: %v", r)'))
mutate("同模块的错误沿用原来的 op", "internal/xclient/xclient.go", ".", "TestBuild",
       swap('ok && xe.Module == module {', 'false && ok && xe.Module == module {'))
# 各集成的建连日志靠它写实例名：模块里的调用点另有变异，这条盯 Build 自己有没有把名字交出去
mutate("Build 把实例名交给 new", "internal/xclient/xclient.go", ".", "TestBuild_BuildsInNameOrder",
       swap('new(ctx, name, cfg)', 'new(ctx, "", cfg)'))
# 快照发布之后不许改：把内部的 map 直接交出去，调用方一改就改到了注册表
mutate("All 交出去的是拷贝", "internal/xclient/xclient.go", ".", "TestAll",
       swap('return maps.Clone(r.state.Load().items)', 'return r.state.Load().items'))

section("流程编排")
# 步骤只要写 Process 和 Rollback：Name 默认取类型名，Dependency 默认强依赖
mutate("没写 Name 时用类型名", "xflow/xflow.go", ".", "TestNew_",
       swap('name: typeName(p), dep: Strong}', 'name: "", dep: Strong}'))
mutate("没写 Dependency 时是强依赖", "xflow/xflow.go", ".", "TestNew_",
       swap('name: typeName(p), dep: Strong}', 'name: typeName(p), dep: Weak}'))
mutate("写了 Dependency 就用写的", "xflow/xflow.go", ".", "TestNew_", swap('\t\ts.dep = d.Dependency()\n', '\t\t_ = d\n'))
# 单独用 xflow 时配置文件没人读，WithRollbackTimeout 是改回滚预算的唯一办法
mutate("流程自己的回滚预算压过配置", "xflow/xflow.go", ".", "TestWithRollbackTimeout",
       swap('cmp.Or(f.rollbackTimeout, cfg.RollbackTimeout)', 'cmp.Or(cfg.RollbackTimeout, f.rollbackTimeout)'))
# 就地改的话，别处正在并发 Execute 的同一个流程也被改掉
mutate("WithRollbackTimeout 返回新流程", "xflow/xflow.go", ".", "TestWithRollbackTimeout",
       swap('\tg := *f\n\tg.rollbackTimeout = d\n\treturn &g\n', '\tf.rollbackTimeout = d\n\treturn f\n'))
mutate("回滚预算不是正数要 panic", "xflow/xflow.go", ".", "TestWithRollbackTimeout",
       swap('\tif d <= 0 {\n\t\tpanic(fmt.Sprintf("xflow: rollback timeout', '\tif d < 0 {\n\t\tpanic(fmt.Sprintf("xflow: rollback timeout'))
# 回滚的是已执行的那段前缀：失败的弱依赖算在内（它可能留下了副作用），
# 失败的强依赖不算（它没成）
mutate("失败的弱依赖也要回滚", "xflow/xflow.go", ".", "TestExecute", swap('\t\t\tn++ // 失败的弱依赖', '\t\t\t// 失败的弱依赖'))
mutate("失败的强依赖那一步不回滚", "xflow/xflow.go", ".", "TestExecute",
       swap('''\t\tf.rollback(ctx, data, f.steps[:n], res, m)
\t\treturn res
\t}
\treturn res''', '''\t\tf.rollback(ctx, data, f.steps[:min(n+1, len(f.steps))], res, m)
\t\treturn res
\t}
\treturn res'''))
mutate("被取消的流程不能报成功", "xflow/xflow.go", ".", "TestExecute",
       swap('\t\t\tif ctx.Err() == nil {\n\t\t\t\tcontinue\n\t\t\t}','\t\t\tcontinue'))
# 两处：notifyStep 和 notifyFlow 各有一个，都去掉才算关掉隔离
mutate("成功的步骤记 INFO", "xflow/monitor.go", ".", "TestSlogMonitor_StepLogsAtInfoLevel",
       swap('slog.InfoContext(ctx, "xflow step "+action+" done"', 'slog.DebugContext(ctx, "xflow step "+action+" done"'))
mutate("监控实现 panic 被隔离", "xflow/monitor.go", ".", "TestMonitor", swap('\tdefer recoverNotify()\n', '', count=2))
# 一个模块边界一个 xerror：safeProcess 自己再包一层，文本就套成两层 xflow
mutate("步骤 panic 只包一层 xflow", "xflow/xflow.go", ".", "TestExecute",
       swap('''err = &PanicError{Value: r, Stack: debug.Stack()}
\t\t}
\t}()
\treturn p.Process(ctx, data)''', '''err = xerror.New("xflow", "execute", &PanicError{Value: r, Stack: debug.Stack()})
\t\t}
\t}()
\treturn p.Process(ctx, data)'''))
mutate("步骤 panic 出来的 error 留在链上", "xflow/xflow.go", ".", "TestExecute",
       swap('\terr, _ := e.Value.(error)\n\treturn err', '\treturn nil'))
# 栈不进错误消息，就得有地方看得到：默认监控把它记成单独的字段
mutate("默认监控记下 panic 的调用栈", "xflow/monitor.go", ".", "TestMonitor",
       swap('attrs = append(attrs, "stack", string(pe.Stack))', '_ = pe'))
# 回滚沿用调用方的 ctx 的话，请求一超时补偿就全部失败——而补偿最需要执行的
# 恰恰是那时候。剥掉取消之后另给一份 RollbackTimeout，是 xflow 最核心的一条承诺
mutate("流程超时之后回滚仍有自己的预算", "xflow/xflow.go", ".", "TestRollback|TestExecute",
       swap('context.WithTimeout(context.WithoutCancel(ctx), cmp.Or(', 'context.WithTimeout(ctx, cmp.Or('))
# 剥的是取消，不是值：换成 Background 的话，补偿请求丢了链路、压测标、泳道标，
# 压测流量的补偿打到线上、泳道里的补偿打到主干
mutate("回滚的 ctx 带着调用方的值", "xflow/xflow.go", ".", "TestRollback_CtxValuesPreserved",
       swap('context.WithTimeout(context.WithoutCancel(ctx), cmp.Or(', 'context.WithTimeout(context.Background(), cmp.Or('))
# 预算只在步骤之间查的话，一个不看 ctx 的 Rollback 能把 Execute 挂住：
# 50ms 的预算等了 2s，挂住的那一步还不在 RollbackErrors 里
mutate("不看 ctx 的补偿也受回滚预算约束", "xflow/xflow.go", ".", "TestRollback",
       swap('err := rollbackWithin(rbCtx, s.Processor, data)', 'err := safeRollback(rbCtx, s.Processor, data)'))
# 第一步之前就取消的流程从前也报 rolled back，让人去查一次不存在的补偿
mutate("一步都没回滚就不报 Rolled", "xflow/xflow.go", ".", "TestExecute",
       swap('\trbCtx, cancel := context.WithTimeout(', '\tres.Rolled = true\n\trbCtx, cancel := context.WithTimeout('))
# 直接解进 cfg：同一进程里上一次 Run 的值带进下一次，校验失败的值也留在了 cfg 上
mutate("配置每次从默认值解起", "xflow/config.go", ".", "TestLoadConfig",
       swap('\tc := DefaultConfig()\n\tif err := xconfig.Unmarshal(ConfigKey, &c)', '\tc := cfg\n\tif err := xconfig.Unmarshal(ConfigKey, &c)'))
mutate("校验失败的配置不生效", "xflow/config.go", ".", "TestLoadConfig",
       swap('\t\treturn err\n\t}\n\tcfg = c', '\t\tcfg = c\n\t\treturn err\n\t}\n\tcfg = c'))

section("登记板")
# 直接解进全局：前一次 Run 的服务名带进下一次，解码失败时写了一半的值也落了上去
mutate("xapp 解进新的默认值再换上", "xapp/xapp.go", ".", "TestLoadConfig_",
       swap('\tc := DefaultConfig()\n\tif err := xconfig.Unmarshal(ConfigKey, &c); err != nil {\n\t\treturn err\n\t}\n\tcfg = c\n\treturn nil',
            '\treturn xconfig.Unmarshal(ConfigKey, &cfg)'))
# 档位是使用者理解生命周期的全部依据：日志最先起、链路早于客户端、
# 服务最后起。排错一档，表现是「实例比用它的东西晚就绪」，别处都测不出来
mutate("启动按档位升序", "internal/hook/hook.go", ".", "TestStartOrder|TestStopOrder|TestAddStart",
       swap('return cmp.Compare(a.Stage, b.Stage)', 'return cmp.Compare(a.Name, b.Name)'))
# 同档内谁先登记谁先起。换成不稳定排序之后顺序会随实现变化，
# 而使用者是照着 import 的先后去理解它的
mutate("同档内保持登记顺序", "internal/hook/hook.go", ".", "TestStartOrder", swap('slices.SortStableFunc(out,', 'slices.SortFunc(out,'))
# 「声明一个档位就同时做到先启动、后关闭」这条承诺，全靠停止顺序是整体逆序
mutate("停止顺序是启动顺序的整体镜像", "internal/hook/hook.go", ".", "TestStopOrder",
       swap('''\tout := startOrder(in)
\tslices.Reverse(out)
\treturn out''', '''\tout := slices.Clone(in)
\tslices.SortStableFunc(out, func(a, b Entry) int { return cmp.Compare(b.Stage, a.Stage) })
\treturn out'''))
# 配对键取末段包名的话，两个末段同名的包会被当成同一个：使用者自己包一层
# 叫 xlog 的包很常见，撞上之后它的启动钩子一失败，框架 xlog 的停止钩子
# 就跟着被跳过，日志写入器再也不 flush
mutate("配对键取完整 import path", "xhook/xhook.go", ".", "TestPkgOf|TestBeforeStart",
       swap('\t\treturn full[:slash+1+i]', '\t\treturn full[slash+1 : slash+1+i]'))
# 默认档落回 StageClient 的话，业务钩子和 xgorm 同档，谁先跑看包的初始化顺序：
# 业务包路径排在前面时，钩子里的 xgorm.C() 当场 panic
mutate("不写档位就落在业务那一档", "xhook/xhook.go", ".", "TestBeforeStart|TestAt",
       swap('o := options{stage: StageBusiness}', 'o := options{stage: StageClient}'))
# 停止钩子只在和它配对的启动钩子成功之后执行：不然启动到一半失败时，后面那些
# 资源根本没建起来，调它们的停止钩子只会在一堆空值上出错
mutate("启动失败的那一对不执行停止钩子", "internal/hook/hook.go", ".", "TestStopAfter|TestRun_InitFailureRollsBackInitialized",
       swap('e.Pair != 0 && !started[e.Pair] {', 'e.Pair != 0 && !started[e.Pair] && false {'))
# 过滤写在 hook.StopAfter 里，两个调用点各打一条：绕开它的话过滤本身再对也没用
mutate("Run 关闭时按启动结果过滤停止钩子", "xone.go", ".", "TestRun_InitFailureRollsBackInitialized",
       swap('hook.StopAfter(started)', 'hook.Stop()'))
# 之前没有启动钩子的停止钩子总会执行。Runnable 被拦下、配置读不出来时从前直接返回，
# 日志 flush 这类钩子一次都没跑。两条提前返回的路各打一条
mutate("Runnable 写错时不依赖启动的停止钩子照样执行", "xone.go", ".", "TestRun_RunnableInvalid_UnpairedStopHooksStillRun",
       swap('\tif err := checkRunnable(r); err != nil {\n\t\treturn errors.Join(err, stopWithin(o, nil))',
            '\tif err := checkRunnable(r); err != nil {\n\t\treturn err'))
mutate("配置读不出来时不依赖启动的停止钩子照样执行", "xone.go", ".", "TestRun_ConfigLoadFails_UnpairedStopHooksStillRun",
       swap('\tif err := config.Ensure(o.configPath, o.log()); err != nil {\n\t\treturn errors.Join(err, stopWithin(o, nil))',
            '\tif err := config.Ensure(o.configPath, o.log()); err != nil {\n\t\treturn err'))
# 一起登记的就是一对：配成同包里最早的那个启动钩子的话，第二对建不起来时
# 它的停止钩子照样被调到，得处理「还没建起来」
mutate("停止钩子配的是之前最近登记的那个启动钩子", "internal/hook/hook.go", ".", "TestRun_StopHookPairsOnlyWithItsStartHook|TestRun_StopHookPairsWithLaterOfTwoStartHooks|TestAddStop",
       swap('for i := len(start) - 1; i >= 0; i-- {', 'for i := 0; i < len(start); i++ {'))
# 只在启动钩子上写 At 的话，停止钩子从前掉回 StageBusiness：客户端先于业务被关掉
mutate("停止钩子不写档位时跟着配对的启动钩子", "xhook/xhook.go", "./xhook", "TestBeforeStop_WithoutStageFollowsPairedStartHook",
       swap('Run: hook.Func(f), Inherit: !o.set}', 'Run: hook.Func(f), Inherit: false}'))
mutate("停止钩子显式写的档位以它为准", "xhook/xhook.go", "./xhook", "TestAt_OverridesDefaultStage",
       swap('Run: hook.Func(f), Inherit: !o.set}', 'Run: hook.Func(f), Inherit: true}'))
mutate("配上对时才继承档位", "internal/hook/hook.go", ".", "TestAddStop_WithoutExplicitStageFollowsPairedStartHook",
       swap('\t\t\tif e.Inherit {\n\t\t\t\te.Stage = start[i].Stage\n\t\t\t}\n', ''))
# 测试辅助和 Run 是同一条规矩：启动失败的那一对不跑停止钩子，起来了的照样关
mutate("xonetest 只关配对的启动钩子成功了的", "xonetest/xonetest.go", ".", "TestStartHooks",
       swap('hook.StopAfter(started)', 'hook.Stop()'))
mutate("xonetest 记下成功了的启动钩子", "xonetest/xonetest.go", ".", "TestStartHooks",
       swap('\t\tstarted[e.Seq] = true\n', ''))
mutate("xonetest 结束时清掉配置", "xonetest/xonetest.go", ".", "TestUseConfig",
       swap('\tt.Cleanup(config.Reset)\n', ''))
# Pair 为 0 表示「没有配对、总会执行」。序号从 0 编起的话，第一个登记的启动钩子
# 失败时，它的停止钩子会被当成不依赖启动的那种照样调到
mutate("启动钩子的序号从 1 编起", "internal/hook/hook.go", ".", "TestAddStop",
       swap('e.Seq = len(start) + 1', 'e.Seq = len(start)'))
# 配对只认同一个包：认错包的话，别的集成起不来时它的停止钩子被跳过，
# 或者它自己没建起来时停止钩子照样被调到
mutate("停止钩子只配同一个包的启动钩子", "internal/hook/hook.go", ".", "TestAddStop|TestRun_StopHookPairsOnlyWithItsStartHook",
       swap('\t\tif start[i].Pkg == e.Pkg {', '\t\tif true {'))
# 按钩子函数的名字认包的话，经辅助包登记的钩子全算在辅助包头上，彼此配成一团
mutate("登记它的包按调用栈上的 init 认", "xhook/xhook.go", ".", "TestBeforeStart",
       swap('Pkg: registrant(full)', 'Pkg: pkgOf(full)'))
# panic 出来的是 error 时要用 %w 接住，否则 errors.Is 问不出根因
mutate("钩子 panic 出来的 error 留在链上", "xone.go", ".", "TestRun_HookPanicWrappedOnceWithStandardOp",
       swap('return fmt.Errorf("panicked: %w", err)', 'return fmt.Errorf("panicked: %v", err)'))

section("客户端")
# 取不到实例时把原因说准：调早了、调晚了、没配、名字写错，要查的地方各不相同。
# 调早了从前和「没配」是同一句话，于是在 Run 之前取实例的人去翻配置文件
mutate("调早了不说成没配", "internal/xclient/xclient.go", ".", "TestGet_", swap('\tcase p == notStarted:\n', '\tcase false:\n'))
mutate("调晚了不说成没配", "internal/xclient/xclient.go", ".", "TestGet_", swap('\tcase p == closed:\n', '\tcase false:\n'))
# 密码错了重试也是错，还多等两轮退避；报成 cannot reach 会让人先去查网络
# 同一个机制（xclient.Probe）两处都要打：Probe 里认出来就不再试，xgorm 把方言的判断交给它
mutate("认证失败不重试", "internal/xclient/probe.go", ".", "TestProbe_AuthFailureReturnsOriginalErrorAfterOneTry",
       swap('\t\t\treturn xutil.Permanent(err)\n', '\t\t\treturn err\n'))
# 多实例的校验错误要指到是哪个文件哪一行：只报原因的话，十几个实例里找不到是哪一个写错了
mutate("实例的 Validate 错误带着文件和行号", "internal/config/clients.go", ".", "TestUnmarshalClients_CallsValidateForEachInstance",
       swap('\t\t\treturn c, fmt.Errorf("%s: %w", newChecker().at(n), err)\n', '\t\t\treturn c, err\n'))
# TLS 块（xtls.Config）一处定义、各模块共用：规则打在 xtls 里，
# 「这个模块真的用了它」打在每个模块的调用点上——函数本身对，调用点绕开了照样是 bug
# 写了 CAFile 却忘了 Enable，照明文连过去比报错更糟
mutate("没开 TLS 却写了 TLS 字段要失败", "xtls/xtls.go", ".", "TestValidate_NonsensicalCombosFailAtConfigRead|TestBuild_ErrorsOnFieldsSetWhileDisabled",
       swap('\t\tif c != (Config{}) {', '\t\tif false {'))
mutate("TLS 客户端证书和私钥要成对", "xtls/xtls.go", ".", "TestValidate_NonsensicalCombosFailAtConfigRead",
       swap('if (c.CertFile == "") != (c.KeyFile == "") {', 'if false {'))
mutate("直接调 Build 也先校验", "xtls/xtls.go", ".", "TestBuild_ErrorsOnFieldsSetWhileDisabled",
       swap('\tif err := c.Validate(); err != nil {\n\t\treturn nil, err\n\t}\n\tif !c.Enable', '\tif !c.Enable'))
mutate("TLS 用上 CAFile", "xtls/xtls.go", ".", "TestBuild_CAFile",
       swap('\t\tcfg.RootCAs = pool\n', '\t\t_ = pool\n'))
mutate("TLS 带上客户端证书", "xtls/xtls.go", ".", "TestBuild_ClientCert",
       swap('\t\tcfg.Certificates = []tls.Certificate{cert}\n', '\t\t_ = cert\n'))
mutate("TLS 的 ServerName 传下去", "xtls/xtls.go", ".", "TestBuild_CAFile|TestBuild_MinTLS12AndVerifies",
       swap('ServerName: c.ServerName}', '}'))
mutate("TLS 最低 1.2", "xtls/xtls.go", ".", "TestBuild_MinTLS12AndVerifies",
       swap('MinVersion: tls.VersionTLS12, ', ''))
# 证书被拒和密码错一样：再试还是同一张证书。PG / MySQL / Redis 的建连探测都走这里
mutate("证书被拒不重试", "internal/xclient/probe.go", ".", "TestProbe",
       swap('(tlsRejected(err) || p.AuthFailed != nil && p.AuthFailed(err))', '(p.AuthFailed != nil && p.AuthFailed(err))'))
mutate("对端的证书告警也算证书被拒", "internal/xclient/probe.go", ".", "TestProbe_CertRejectedReturnsAfterOneTry",
       swap('if errors.As(err, &op) && op.Op == "remote error" && op.Err != nil {', 'if false {'))
# 建到一半失败时把半套实例发布出去，比一个都没有更糟：C() 取得到 a
# 取不到 b，而启动其实已经失败了
# 建到一半失败、回头关已建好的时，关不掉的错误从前被丢掉，漏着的连接池在日志里一个字都没有
mutate("关不掉的错误并进 Build 的错误", "internal/xclient/xclient.go", ".", "TestBuild_CloseErrorsOfBuiltOnesJoinedIntoBuildError",
       swap('\t\tif cerr := closeAll(r.module, closers); cerr != nil {\n\t\t\treturn errors.Join(err, cerr)\n\t\t}\n',
            '\t\tcloseAll(r.module, closers)\n'))
mutate("建失败时注册表保持原样", "internal/xclient/xclient.go", ".", "TestBuild",
       swap('''\t\tbuilt[name] = v
\t\tclosers = append(closers, closer)''', '''\t\tbuilt[name] = v
\t\tclosers = append(closers, closer)
\t\tr.state.Store(&snapshot[T]{items: built, phase: running})'''))
# 反过来的话，关到一半时 C() 还能取到正在被关闭的实例
mutate("关实例先摘再关", "internal/xclient/xclient.go", ".", "TestClose",
       swap('''\told := r.state.Swap(&snapshot[T]{items: map[string]T{}, phase: closed})
\treturn closeAll(r.module, old.closers)''', '''\terr := closeAll(r.module, r.state.Load().closers)
\tr.state.Store(&snapshot[T]{items: map[string]T{}, phase: closed})
\treturn err'''))
# 「XRedis:」这样的空块曾经让启动直接失败：Has 返回 false，那个包就此跳过、
# 不再 Unmarshal，于是这个 key 没人认领，而 Unclaimed 报出来的两条原因
# （拼错了、忘了 import）都不成立，照着查什么都查不出来
mutate("问过配置就算认领了它", "internal/config/config.go", ".", "TestHas|TestUnclaimed",
       swap('''\t\treturn nil, err
\t}
\tclaimed[key] = true
\tif node := sections[key];''', '''\t\treturn nil, err
\t}
\tif node := sections[key];'''))
# 认领记在 sectionLocked 里，这一条打在调用点上：Has 绕开它自己去查，就回到了当初那个 bug 的形状
mutate("Has 也记一笔认领", "internal/config/config.go", ".", "TestHas|TestUnclaimed",
       swap('\tnode, err := sectionLocked(key)\n\treturn err == nil && node != nil',
            '\terr := ensureLocked()\n\treturn err == nil && !isEmptyNode(sections[key])'))
# 要防的是：读得早就静默拿到空值，服务带着一套默认配置正常起来。
# 现在第一次读就先加载，这条变异把它改回「没加载就当没配」
mutate("读得早也拿到文件里的值", "internal/config/config.go", ".", "TestUnmarshal_LoadsFirstIfNotLoaded|TestRun_ReadingConfigBeforeRunGetsFileValues",
       swap('''\tif err := ensureLocked(); err != nil {
\t\treturn nil, err
\t}
\tclaimed[key] = true''', '''\tif !ready {
\t\treturn nil, nil
\t}
\tclaimed[key] = true'''))
# 调用点：Unmarshal 绕开 sectionLocked 直接查 sections 的话，没加载时同样静默拿到空值
mutate("Unmarshal 读之前先加载", "internal/config/config.go", ".", "TestUnmarshal_LoadsFirstIfNotLoaded|TestRun_ReadingConfigBeforeRunGetsFileValues",
       swap('\tnode, err := sectionLocked(key)\n\tif err != nil {\n\t\treturn err\n\t}',
            '\tnode, err := sections[key], error(nil)\n\tif err != nil {\n\t\treturn err\n\t}'))
# 从前没有配置文件时配置一直停在「还没加载」，每个集成读配置都报错，
# 一个不需要任何配置的服务根本起不来
mutate("没有配置文件时全用默认值", "internal/config/config.go", ".", "TestEnsure_UsesDefaultsWhenNoConfigFile|TestRun_StartsWithDefaultsWhenNoConfigFile",
       swap('''\t\tsections, claimed, ready = nil, map[string]bool{}, true
\t\treturn nil''', '''\t\tfailed = errors.New("no config file")
\t\treturn failed'''))
mutate("提前加载过的配置不会被悄悄换掉", "internal/config/config.go", ".", "TestEnsure_FailsWhenNamedFileDiffersFromEarlierLoad|TestRun_FailsWhenOtherFileNamedAfterEarlyRead",
       swap('\tif path != "" && !sameFile(path, source) {', '\tif false {'))
# 按字符串比的话 dir/./b.yml、相对路径、符号链接都被当成另一个文件，Run 启动失败
mutate("同一个文件换一种写法也认得出", "internal/config/config.go", ".", "TestEnsure_SameFileSpelledDifferentlyIsFine",
       swap('\tif path != "" && !sameFile(path, source) {', '\tif path != "" && path != source {'))
mutate("经符号链接点名的也是同一个文件", "internal/config/config.go", ".", "TestEnsure_SameFileSpelledDifferentlyIsFine",
       swap('errB == nil && os.SameFile(infoA, infoB)', 'errB == nil && false && os.SameFile(infoA, infoB)'))
# 同一个变量用了好几处时从前报成 A, B, A, A：排好、去重打在调用点上
mutate("没设的变量排好、每个只报一次", "internal/config/config.go", ".", "TestLoad_MissingVariablesListedOnceSorted",
       swap('"environment variables not set: %s", missingNames(missing))', '"environment variables not set: %s", strings.Join(missing, ", "))'))
# ./app --config 忘了带值时从前被静默忽略，起来的是另一份配置
mutate("启动参数没带值时报错", "internal/config/config.go", ".", "TestEnsure_FlagWithoutValueIsError",
       swap('\tif failed = danglingArg(); failed != nil {', '\tif failed = nil; failed != nil {'))
# 传了值或 nil 从前当场 panic；Unmarshal 在找块之前就查，块没配时照样报
mutate("Unmarshal 的目标不是指针时报错", "internal/config/config.go", ".", "TestUnmarshalAndDecodeStrict_BadTargetIsErrorNotPanic",
       swap('func Unmarshal(key string, into any) error {\n\tif err := checkTarget(into); err != nil {',
            'func Unmarshal(key string, into any) error {\n\tif err := error(nil); err != nil {'))
mutate("DecodeStrict 的目标不是指针时报错", "internal/config/strict.go", ".", "TestUnmarshalAndDecodeStrict_BadTargetIsErrorNotPanic",
       swap('func DecodeStrict(node *yaml.Node, target any) error {\n\tif err := checkTarget(target); err != nil {',
            'func DecodeStrict(node *yaml.Node, target any) error {\n\tif err := error(nil); err != nil {'))
mutate("写坏的配置每次读都报同一个错", "internal/config/config.go", ".", "TestUnmarshal_SameErrorOnEveryReadAfterLoadFailure",
       swap('\t\tfailed = err\n\t\treturn err', '\t\treturn err'))
mutate("Run 结束时交还配置", "xone.go", ".", "TestRun_CanRunAgainAfterReturn", swap('\tdefer config.Reset()\n', ''))
mutate("加载失败的 Run 也交还配置", "xone.go", ".", "TestRun_NextRunReadsOwnConfigAfterLoadFailure",
       swap('\tdefer config.Reset()\n', ''),
       swap('''\tif err := config.Ensure(o.configPath, o.log()); err != nil {
\t\treturn errors.Join(err, stopWithin(o, nil))
\t}
''', '''\tif err := config.Ensure(o.configPath, o.log()); err != nil {
\t\treturn errors.Join(err, stopWithin(o, nil))
\t}
\tdefer config.Reset()
'''))

section("启动输出")
# banner 只在终端里打：容器、重定向、日志采集器后面，多行字符画是日志平台里解析失败的垃圾
mutate("banner 只在终端里打", "banner.go", ".", "TestPrintBanner",
       swap('\tif !terminal {\n\t\treturn\n\t}\n', ''))
mutate("Run 拿 stderr 判断是不是终端", "xone.go", ".", "TestRun_StderrNotTerminal_NoBanner",
       swap('printBanner(stderr, isTerminal(stderr))', 'printBanner(stderr, true)'))
mutate("管道和文件不是终端", "banner.go", ".", "TestIsTerminal",
       swap('st.Mode()&os.ModeCharDevice != 0', 'st.Mode() == st.Mode()'))
mutate("replace 到本地时版本显示 (devel)", "banner.go", ".", "TestModuleVersion",
       swap('\t\t\tif d.Replace != nil {\n\t\t\t\treturn "(devel)"\n\t\t\t}\n', ''))
# XONE_DEBUG：只在明确打开时写，写的是加载经过和遮掉凭证的最终配置
mutate("XONE_DEBUG 没开就不写", "internal/config/debug.go", ".", "TestDebug_|TestEnsure_PrintsNothingWithoutXONE_DEBUG|TestRun_NoDebugOutputWithoutXONE_DEBUG",
       swap('\t}\n\treturn false\n}\n\n// DebugOut', '\t}\n\treturn true\n}\n\n// DebugOut'))
mutate("加载完打出经过", "internal/config/config.go", ".", "TestEnsure_XONE_DEBUG",
       swap('\tdebugReport(path, from, r)\n', '\t_, _ = from, r\n'))
mutate("Run 打出启动钩子的顺序", "xone.go", ".", "TestRun_XONE_DEBUG",
       swap('\tdebugHooks(hook.Start())\n', ''))
mutate("key 名是凭证的值整个遮掉", "internal/config/debug.go", ".", "TestRedacted",
       swap('if strings.Contains(k, s) {', 'if s == "" {'))
mutate("URL 里的密码遮掉", "internal/config/debug.go", ".", "TestRedacted",
       swap('s = urlUserinfo.ReplaceAllString(s, "$1:"+redactedValue+"@")', 's = s'))
mutate("MySQL DSN 里的密码遮掉", "internal/config/debug.go", ".", "TestRedacted",
       swap('s = mysqlDSN.ReplaceAllString(s, "$1:"+redactedValue+"@")', 's = s'))
mutate("password= 写法的密码遮掉", "internal/config/debug.go", ".", "TestRedacted",
       swap('\treturn kvPair.ReplaceAllStringFunc(s, func(m string) string {', '\treturn s\n\treturn kvPair.ReplaceAllStringFunc(s, func(m string) string {'))
# 查询串里的 token=、api_key= 从前原样打出来：只认 password / passwd / pwd 三个名字
mutate("查询串里按凭证词表遮", "internal/config/debug.go", ".", "TestRedacted_SensitiveQueryParamsMasked",
       swap('if sensitiveKey(name) || strings.EqualFold(name, "pwd") {', 'if strings.EqualFold(name, "password") || strings.EqualFold(name, "pwd") {'))
# 密码里带 @ 或 / 时从前只遮到第一个 @，后半截连同主机原样打出来
mutate("URL 的密码遮到最后一个 @", "internal/config/debug.go", ".", "TestRedacted_DSNPasswordMaskedUpToLastAt",
       swap('`(://[^:/?#@\\s]*):\\S*@`', '`(://[^:/?#@\\s]*):[^@/\\s]+@`'))
mutate("MySQL DSN 的密码遮到最后一个 @", "internal/config/debug.go", ".", "TestRedacted_DSNPasswordMaskedUpToLastAt",
       swap('`^([^:@/\\s]+):\\S*@`', '`^([^:@/\\s]+):[^@\\s]+@`'))
# 查询串的值从前一路遮到空白为止，${VAR:默认值} 收尾的 } 连同后面的路径一起没了
mutate("调试输出遮查询串的值停在占位符收尾之前", "internal/config/debug.go", ".", "TestEnsure_XONE_DEBUGPlaceholderDefaultMaskedKeepsClosingBrace",
       swap("('[^']*'|[^\\s&#;}]+)", "('[^']*'|[^\\s&#;]+)"))
# 凭证写成列表或 map 时从前原样打出来：只遮了标量
mutate("凭证 key 下的值不论形状整个遮掉", "internal/config/debug.go", ".", "TestRedacted_MasksWholeValueOfAnyShapeUnderSensitiveKey",
       swap('\t\t\t\tc.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: redactedValue}\n',
            '\t\t\t\tif v.Kind == yaml.ScalarNode {\n\t\t\t\t\tc.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: redactedValue}\n\t\t\t\t}\n'))
# ${VAR} 展开出来的值从前原样打出来，放在不叫 password 的 key 下面（DSN、Webhook）就漏了。打在两个调用点上
mutate("调试输出里占位符显示原文", "internal/config/debug.go", ".", "TestEnsure_XONE_DEBUGShowsPlaceholderTextNotExpandedValue",
       swap('redacted(r.root, r.raw)', 'redacted(r.root, nil)'))
mutate("加载时把占位符原文交给调试输出", "internal/config/config.go", ".", "TestEnsure_XONE_DEBUGShowsPlaceholderTextNotExpandedValue",
       swap('\tr.root, r.raw = root, values\n', '\tr.root = root\n'))
mutate("遮的是拷贝，真正的配置不动", "internal/config/debug.go", ".", "TestRedacted",
       swap('\t\tc.Content[i] = redacted(ch, raw)\n', '\t\tc.Content[i] = ch\n\t\tredacted(ch, raw)\n'))
mutate("空的凭证不遮：看得出没配", "internal/config/debug.go", ".", "TestRedacted",
       swap('; isEmptyNode(v) || v.Kind == yaml.ScalarNode && v.Value == "" {', '; false {'))
mutate("最终配置里不带注释", "internal/config/debug.go", ".", "TestRedacted",
       swap('\tc.HeadComment, c.LineComment, c.FootComment = "", "", ""\n', ''))
# UseHandler：换了后端，xlog 那一层（AddKV、trace_id、观察者）照样包在外面
mutate("自己的 handler 外面照样包着 xlog", "xlog/xlog.go", ".", "TestUseHandler_LogsGoToOwn",
       swap('withStatic(slog.New(newCtxHandler(*h)), identity(), c.Fields)', 'withStatic(slog.New(*h), identity(), c.Fields)'))
# 写了 XLog.Level: debug 却由别人的 handler 决定级别，以为生效了其实没有
mutate("UseHandler 和 XLog 块同时写就启动失败", "xlog/xlog.go", ".", "TestUseHandler_XLogBlock",
       swap('if !reflect.DeepEqual(output, DefaultConfig()) {', 'if !reflect.DeepEqual(output, output) {'))
mutate("日志装好之后 UseHandler 不生效", "xlog/xlog.go", ".", "TestUseHandler_LateCall",
       swap('\tif installed.Load() {\n', '\tif false {\n'))
mutate("关掉之后 UseHandler 重新可用", "xlog/xlog.go", ".", "TestUseHandler_UsableAgain",
       swap('\tinstalled.Store(false) // 同一进程里再跑一次 Run', '\t// 同一进程里再跑一次 Run'))
# 每条日志的身份字段：框架装配时加上，Fields 覆盖同名的，空串不写
mutate("日志默认带身份字段", "xlog/xlog.go", ".", "TestIdentity_DefaultFields",
       swap('l, cl, err := build(c, identity())', 'l, cl, err := build(c, nil)'))
mutate("Fields 覆盖同名的默认字段", "xlog/xlog.go", ".", "TestFields_ExtraOverrideAndDrop",
       swap('if v, ok := fields[a.Key]; ok {', 'if v, ok := fields[a.Key]; ok && false {'))
mutate("空串的字段不写", "xlog/xlog.go", ".", "TestIdentity_EmptyAppFields",
       swap('if a.Value.Kind() != slog.KindString || a.Value.String() != "" {', 'if true {'))
mutate("UseHandler 也带身份字段", "xlog/xlog.go", ".", "TestUseHandler_CarriesIdentity",
       swap('slog.SetDefault(withStatic(slog.New(newCtxHandler(*h)), identity(), c.Fields))', 'slog.SetDefault(slog.New(newCtxHandler(*h)))'))
mutate("UseHandler 时写 Fields 不算冲突", "xlog/xlog.go", ".", "TestUseHandler_CarriesIdentity",
       swap('\t\toutput.Fields = nil\n', ''))
# XApp 跟着框架一起来，xlog 不跟：只 import 根包的程序写了 XApp 能启动，slog.Default 不被换掉。
# internal/coreonly 只 import 根包；那里的测试要是自己 import 了 xlog / xapp，这两条变异就抓不住了
mutate("根包带着 xapp", "xone.go", ".", "TestRun_XAppWorksWithCoreOnly",
       swap('\t_ "github.com/jupiter-eng/xone/xapp"\n', ''))
mutate("根包不带 xlog", "xone.go", ".", "TestRun_CoreOnlyLeavesSlogDefaultAlone",
       swap('\t_ "github.com/jupiter-eng/xone/xapp"\n', '\t_ "github.com/jupiter-eng/xone/xlog"\n'))
mutate("XLog 没人读时报错给出 import", "xone.go", ".", "TestRun_XLogWithoutImportNamesTheImport",
       swap('slices.Contains(orphan, "XLog")', 'slices.Contains(orphan, "")'))
mutate("只剩 Profiles / Import 的 XApp 连块一起摘掉", "internal/config/source.go", ".", "TestLoad_XAppWithOnlyProfilesAndImportIsNotUnclaimed",
       swap('if val != nil && len(app.Content) == 0 {', 'if false {'))
mutate("XApp 里的 Import 被加载器取走", "internal/config/source.go", ".", "TestLoad_XAppNameLeftToXapp_ProfilesAndImportTakenByLoader",
       swap('\tnode := takeFromApp(doc, ImportKey)\n', '\tnode := takeFromApp(doc.Content[0], ImportKey)\n'))

section("Web 集成共用：脱敏与请求体预读")
# internal/web 里由它自己的测试盯着的承诺。服务启停、代理网段、访问日志字段这几块的承诺
# 由 xgin 的测试盯着，那些变异在 xgin.py 里（在 ./xgin 下跑，经 overlay 换进这里的文件）
mutate("请求头里的凭证被遮掉", "internal/web/redact.go", ".", "TestRedact",
       swap('set[name] || ', ''), swap('\tset := headers()\n', '\tset := headers()\n\t_ = set\n'))
# 开了 LogQuery 也得逐字段遮：?access_token= 原样进日志就是凭证落盘
mutate("遮掉的表单值写成标记而不是转义串", "internal/web/redact.go", ".", "TestRedactBody_Form",
       swap('return strings.ReplaceAll(values.Encode(), url.QueryEscape(Redacted), Redacted)', 'return values.Encode()'))
# 表单 / query 的值、纯文本 body 里带着 URL / DSN 凭证：JSON 那一支会遮，换个编码不该就原样进日志
mutate("表单其余值里的 URL / DSN 凭证被遮掉", "internal/web/redact.go", ".", "TestRedactBody_CredentialsInFormValue",
       swap('\t\t\tvs[i] = redactCredentials(v)\n', '\t\t\tvs[i] = v\n'))
mutate("纯文本 body 里的 URL / DSN 凭证被遮掉", "internal/web/redact.go", ".", "TestRedactBody_CredentialsInFormValue",
       swap('return newlines.Replace(redactCredentials(s))', 'return newlines.Replace(s)'))
mutate("请求体只缓存前缀", "internal/web/accesslog.go", ".", "TestSnapshotBody",
       swap('io.ReadAll(io.LimitReader(req.Body, maxRequestBody))','io.ReadAll(req.Body)',1))
mutate("预读时的错误接回下游", "internal/web/accesslog.go", ".", "TestSnapshotBody",
       swap('\tif b.preErr != nil {\n\t\treturn 0, b.preErr\n\t}\n',''))
# Content-Type 大小写不敏感：照字面比的话 Multipart/Form-Data 的文件内容整个进日志
mutate("上传和二进制流不读，不论大小写", "internal/web/accesslog.go", ".", "TestSnapshotBody",
       swap('ct := strings.ToLower(req.Header.Get("Content-Type"))', 'ct := req.Header.Get("Content-Type")'))
# encoding/json 按 Unicode 折叠匹配字段名：{"ſecret":…} 绑得上 Secret。
# 只转小写的话字段名比对和预检都认不出它
mutate("字段名按 Unicode 折叠比对", "internal/web/redact.go", ".", "TestRedactBody_Unicode",
       swap('b.WriteRune(foldRune(r))', 'b.WriteRune(unicode.ToLower(r))'))
# 预检和字段名比对共用 sensitive，折叠本身由上一条盯着；这一条打在调用点上：
# 预检换成只转小写的朴素写法，{"ſecret":…} 就走快路径原样进日志
mutate("敏感词预检按 Unicode 折叠", "internal/web/redact.go", ".", "TestRedactBody_Unicode",
       swap('sensitive(s, words()) {\n\t\treturn Redacted\n\t}\n\t// 没有敏感字段名，但可能带着 URL / DSN 凭证（postgres://u:p@h），同 RedactText\n\treturn newlines', 'func(ws []string) bool { return slices.ContainsFunc(ws, func(w string) bool { return strings.Contains(strings.ToLower(s), w) }) }(words()) {\n\t\treturn Redacted\n\t}\n\t// 没有敏感字段名，但可能带着 URL / DSN 凭证（postgres://u:p@h），同 RedactText\n\treturn newlines'),
       swap('|| sensitive(body, ws)', '|| slices.ContainsFunc(ws, func(w string) bool { return strings.Contains(strings.ToLower(body), w) })'))
# path 特意不带查询串，Referer 却带着上一个页面的完整 URL
mutate("URL 类请求头去掉查询串", "internal/web/redact.go", ".", "TestRedactHeaders_URL",
       swap('\t\t\tv = stripURL(v)\n', ''))
# 原先是精确匹配：new_password、client_secret、sessionToken 原样进日志
mutate("JSON 字段名里带敏感词也遮", "internal/web/redact.go", ".", "TestRedactBody",
       swap('\t\t\tif sensitive(k, ws) {\n\t\t\t\tt[k] = Redacted', '\t\t\tif slices.Contains(ws, normalize(k)) {\n\t\t\t\tt[k] = Redacted'))
mutate("表单字段名里带敏感词也遮", "internal/web/redact.go", ".", "TestRedactBody",
       swap('\t\tif sensitive(k, ws) {\n\t\t\tvalues[k]', '\t\tif slices.Contains(ws, normalize(k)) {\n\t\t\tvalues[k]'))
# 名单永远列不全（Proxy-Authorization 就曾漏在外面），词表是兜底的那一层
mutate("请求头名字里带敏感词也遮", "internal/web/redact.go", ".", "TestRedactHeaders",
       swap(' || sensitive(k, ws)', ''), swap('\tset := headers()\n\tws := words()\n', '\tset := headers()\n\tws := words()\n\t_ = ws\n'))
# 预检认不出 api-key 的话，这种 body 走快路径原样进日志，根本到不了逐字段脱敏
mutate("敏感词预检忽略分隔符", "internal/web/redact.go", ".", "TestRedactBody",
       swap('sensitive(s, words()) {\n\t\treturn Redacted\n\t}\n\t// 没有敏感字段名，但可能带着 URL / DSN 凭证（postgres://u:p@h），同 RedactText\n\treturn newlines', 'func(ws []string) bool { return slices.ContainsFunc(ws, func(w string) bool { return strings.Contains(strings.Map(foldRune, s), w) }) }(words()) {\n\t\treturn Redacted\n\t}\n\t// 没有敏感字段名，但可能带着 URL / DSN 凭证（postgres://u:p@h），同 RedactText\n\treturn newlines'),
       swap('|| sensitive(body, ws)', '|| slices.ContainsFunc(ws, func(w string) bool { return strings.Contains(strings.Map(foldRune, body), w) })'))
mutate("脱敏后大整数不丢精度", "internal/web/redact.go", ".", "TestRedactBody", swap('\tdec.UseNumber()\n', ''))
mutate("脱敏后不转义 HTML 字符", "internal/web/redact.go", ".", "TestRedactBody",
       swap('\tenc.SetEscapeHTML(false)\n', ''))
mutate("JSON 后面跟着别的东西时整个遮掉", "internal/web/redact.go", ".", "TestRedactBody",
       swap('dec.Decode(new(any)) != io.EOF', '(dec.Decode(new(any)) != io.EOF && false)'))

section("Web 集成共用：client_ip 与标签收敛")
# xecho 的 client_ip 按这里的规则算（xgin 用 gin 自己的，两者一致由 xgin 的 TestClientIP_SharedRuleMatchesGin 比对）。
# 规则的每一处都是 gin v1.12.0 的行为，改一处就是同一个服务换框架之后 client_ip 变了
mutate("client_ip 从右往左停在第一个不可信的地址", "internal/web/proxy.go", ".", "TestProxies_ClientIP",
       swap('if i == 0 || !ps.contains(ip) {', 'if i == 0 {'))
mutate("转发头有好几行时拼成一个列表", "internal/web/proxy.go", ".", "TestProxies_ClientIP",
       swap('ps.fromHeader(strings.Join(r.Header.Values(name), ","))', 'ps.fromHeader(r.Header.Get(name))'))
mutate("转发头里解不出的一项之后不再往左看", "internal/web/proxy.go", ".", "TestProxies_ClientIP",
       swap('\t\tif ip == nil {\n\t\t\tbreak\n\t\t}\n', '\t\tif ip == nil {\n\t\t\tcontinue\n\t\t}\n'))
mutate("X-Forwarded-For 没有可用的值时看 X-Real-IP", "internal/web/proxy.go", ".", "TestProxies_ClientIP",
       swap('var forwardedHeaders = []string{"X-Forwarded-For", "X-Real-IP"}', 'var forwardedHeaders = []string{"X-Forwarded-For"}'))
mutate("方法收敛到固定集合", "internal/web/request.go", ".", "TestNormalizeMethod",
       swap('\treturn MethodOther\n', '\treturn m\n'))
mutate("认出 connection reset", "internal/web/request.go", ".", "TestIsBrokenPipe",
       swap(' || strings.Contains(msg, "connection reset by peer")', ''))
# 业务自己设的截止时间到了客户端还在等：只有 net/http 的取消才算客户端走了
mutate("客户端走了只认 Canceled", "internal/web/request.go", ".", "TestClientGone",
       swap('errors.Is(ctx.Err(), context.Canceled)', 'ctx.Err() != nil'))
# identity 就是没压缩，照常记
mutate("identity 不算压缩过", "internal/web/accesslog.go", ".", "TestAccessLog_EncodedResponseBodyOmitted",
       swap('if enc == "" || enc == "identity" {', 'if enc == "" {'))
# net/http 自己报的错（TLS 握手失败……）不设 ErrorLog 就走标准库的 log：进了 slog 是 INFO、消息每行都不一样
mutate("http.Server 的 ErrorLog 接到 slog", "internal/web/server.go", ".", "TestServer_ErrorLogRoutedToSlog",
       swap('\t\tErrorLog:          ErrorLog(module),\n', ''))

section("错误文本脱敏")
# 错误原文最容易夹带凭证（驱动报的整串 DSN、"password=..."），进访问日志之前过一遍词表
mutate("访问日志的 errors 字段脱过敏", "internal/web/accesslog.go", ".", "TestAccessLog_ErrorsFieldIsRedacted",
       swap('slog.String("errors", RedactText(a.Errors))', 'slog.String("errors", a.Errors)'))

# 驱动报的错里常带整串 DSN，一个敏感词都没有：只看词表的话密码原样进 errors 字段和 Span
mutate("错误文本里 URL 的密码被遮掉", "internal/web/redact.go", ".", "TestRedactText_MasksPasswordInURLUserinfo",
       swap('\ts = urlUserinfo.ReplaceAllString(s, "$1:"+Redacted+"@")\n', ''))
mutate("错误文本里 MySQL DSN 的密码被遮掉", "internal/web/redact.go", ".", "TestRedactText_MasksPasswordInMySQLDSN",
       swap('\treturn mysqlUserinfo.ReplaceAllString(s, "$1:"+Redacted+"@$2")\n', '\treturn s\n'))
# 密码里没转义的 @：按第一个 @ 截的话，@ 后面那半截密码留在日志里
mutate("URL 里的密码按最后一个 @ 算", "internal/web/redact.go", ".", "TestRedactText_MasksPasswordInURLUserinfo",
       swap('`(://[^:/?#@\\s]*):[^/\\s]+@`', '`(://[^:/?#@\\s]*):[^@/\\s]+@`'))
mutate("错误文本里有敏感词仍整段遮掉", "internal/web/redact.go", ".", "TestRedactText_SensitiveWordRedactsWholeText",
       swap('\tif sensitive(s, words()) {\n\t\treturn Redacted\n\t}\n\treturn textNewlines', '\treturn textNewlines'))
# gin 的 c.Errors.String() 一条错误一行：换行像 body 那样直接删掉，两条错误粘成一句
mutate("错误文本的多行用分号隔开", "internal/web/redact.go", ".", "TestRedactText_KeepsLinesSeparated",
       swap('return textNewlines.Replace(strings.TrimRight(redactCredentials(s), "\\r\\n"))', 'return newlines.Replace(redactCredentials(s))'))

section("Web 集成共用：代理网段")
# gin v1.12.0 把单个的 ::ffff:10.0.0.1 解成信任 ::1、::5，ParseProxies 不 Unmap 网段：同一行配置各处理解不一样
mutate("IPv4 映射写法的代理网段要拦住", "internal/web/proxy.go", ".", "TestValidateProxies_RejectsIPv4MappedEntries",
       swap('\t\tif plain, ok := unmapped(p); ok {', '\t\tif plain, ok := unmapped(p); ok && false {'))
mutate("映射网段的建议写法减掉 96 位", "internal/web/proxy.go", ".", "TestValidateProxies_RejectsIPv4MappedEntries",
       swap('pfx.Bits()-96', 'pfx.Bits()'))

section("Web 集成共用：响应头与 JSON 值的脱敏")
# 响应头也是 URL：OAuth 回调的 302 Location 带着 ?code=，隐式授权带着 #access_token=
mutate("Location 响应头去掉查询串", "internal/web/redact.go", ".", "TestRedactHeaders_ResponseURL",
       swap('\t"location":         true,\n', ''))
# 只写用户名的 userinfo（https://<token>@host）用户名本身就是凭证，RedactText 只遮 user:pass 的密码
mutate("URL 头去掉 userinfo", "internal/web/redact.go", ".", "TestRedactHeaders_ResponseURL",
       swap("\t\tif at := strings.LastIndexByte(host, '@'); at >= 0 {\n\t\t\tu = u[:i+3] + u[i+3+at+1:]\n\t\t}\n", ''))
# 调用点：名字里没有敏感词的头，值里照样夹着整串 DSN
mutate("请求头的值遮掉 DSN 的密码", "internal/web/redact.go", ".", "TestRedactHeaders_DSNInValue|TestRedactHeaders_ValuesNaming",
       swap('attrs = append(attrs, slog.String(k, redactCredentials(v)))', 'attrs = append(attrs, slog.String(k, v))'))
# 值里的 Cookie、Authorization 是头名不是凭证：套 RedactText 的敏感词规则的话 Vary、CORS 头整段遮掉
mutate("请求头的值不按敏感词整段遮", "internal/web/redact.go", ".", "TestRedactHeaders_ValuesNaming",
       swap('attrs = append(attrs, slog.String(k, redactCredentials(v)))', 'attrs = append(attrs, slog.String(k, RedactText(v)))'))
# gin 的 ShouldBindJSON 不看 Content-Type：text/plain 的 JSON 走纯文本那一支，转义过的键名认不出来
mutate("不是 JSON 类型的 JSON body 按 JSON 遮", "internal/web/redact.go", ".", "TestRedactBody_JSONWithoutJSONContentType",
       swap('\tcase isJSON(body):\n\t\treturn redactJSON(body)\n', ''))
mutate("JSON 的字符串值过 RedactText", "internal/web/redact.go", ".", "TestRedactBody_JSONStringValueDSN",
       swap('\tcase string:\n\t\treturn RedactText(t)\n', ''))
# 快路径只看敏感词的话，{"dsn":"postgres://u:p@h"} 连解析都不进
mutate("值里有 @ 的 JSON 不走快路径", "internal/web/redact.go", ".", "TestRedactBody_JSONStringValueDSN",
       swap("if !mayContainField(s, ws) && strings.IndexByte(s, '@') < 0 {", "if !mayContainField(s, ws) {"))
mutate("没有 @ 才跳过 DSN 的正则", "internal/web/redact.go", ".", "TestRedactText_MasksPassword",
       swap("\tif strings.IndexByte(s, '@') < 0 {\n\t\treturn s\n", "\tif strings.IndexByte(s, '@') >= 0 {\n\t\treturn s\n"))
# 留空取默认要写明 1.2，落到 tls.Config 的零值就是随 Go 版本变的默认
mutate("MinVersion 留空是 1.2", "internal/web/config.go", ".", "TestServerTLS_EmptyMinVersionMeansTLS12",
       swap('"": tls.VersionTLS12, ', ''))
