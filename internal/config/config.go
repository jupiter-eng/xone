// Package config 读配置文件，并把结果留在这里等人来取。
//
// 三条规矩：默认值预填在结构体里；未知字段是错误；${VAR} 未设置是错误。
//
// 什么时候取都行：第一次有人取的时候才去找文件、加载，之后都是同一份。
// 于是使用者不必知道配置是什么时候加载的——在 main 里、在 Run 之前、在钩子里，
// 读到的都是配置文件里的最终值。xone.Run 只是确保它加载过，结束时再交还。
//
// 「谁读了哪一块」是运行时才知道的，没人读过的顶层 key 由 Unclaimed
// 在钩子全跑完之后报出来。
package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"go.yaml.in/yaml/v3"

	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xutil"
)

var placeholder = regexp.MustCompile(`\$\{([^}:]+)(?::([^}]*))?\}`)

var (
	mu       sync.Mutex
	sections map[string]*yaml.Node // 顶层 key → 那一段的节点
	claimed  map[string]bool       // 被 Unmarshal / Has 问过的 key
	ready    bool                  // 已加载：读到了文件，或者没找到文件、全用默认值
	source   string                // 加载的是哪个文件；没找到文件时为空
	failed   error                 // 加载失败的原因，之后每次读都原样返回

	// expanded 占位符展开过的节点 → 配置里写的那段原文（如 ${DB_PASSWORD}），见 checker.redact。
	// 不归 mu 管：DecodeStrict 经 xconfig 导出，调用方不一定持有 mu
	expanded atomic.Pointer[map[*yaml.Node]string]

	// origins 每个解析出来的节点 → 它来自哪个文件，见 checker.at。不归 mu 管，理由同 expanded
	origins atomic.Pointer[map[*yaml.Node]string]
)

// Ensure 确保配置已经加载，由 xone.Run 在启动时调用。
//
// path 是 WithConfigPath 点名要的文件，留空按 locate 的顺序找。已经有人提前
// 读过配置（于是已经加载过）时沿用那一份：提前读到的值和之后生效的必须是
// 同一份。所以点名的文件和它不是同一个时报错，而不是悄悄换一份。
func Ensure(path string, log *slog.Logger) error {
	mu.Lock()
	defer mu.Unlock()
	if !ready && failed == nil {
		return loadLocked(path, log)
	}
	if path != "" && !sameFile(path, source) {
		earlier := "when no config file was found"
		if source != "" {
			earlier = "from " + source
		}
		return xerror.Newf("xconfig", "config",
			"config was already loaded by an earlier read (%s), so %s cannot take effect: "+
				"pass the path with --%s or %s instead", earlier, path, ArgKey, EnvKey)
	}
	return failed
}

// sameFile 两个路径是不是同一个文件。
//
// 不能按字符串比：同一个文件写成 dir/./b.yml、相对路径或者经符号链接，
// 从前都被当成另一个文件，Run 报「已经加载过另一份」而启动失败。
// 先比清理过的绝对路径；两边都存在时再按 os.SameFile 认（管得到符号链接和硬链接）。
func sameFile(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA == nil && errB == nil && absA == absB {
		return true
	}
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}

// ensureLocked 第一次读的时候按 locate 的顺序找文件、加载。调用方持有 mu
func ensureLocked() error {
	if ready || failed != nil {
		return failed
	}
	return loadLocked("", slog.Default())
}

// loadLocked 找文件并加载，调用方持有 mu。
//
// 点名要的文件（WithConfigPath、--config、XONE_CONFIG）找不到是错误；约定路径
// 一个都没命中则只告警、全用默认值——这对一个没有任何外部依赖的服务是合理的。
// 两者的区别与「你要的」和「约定俗成的」一致。
//
// 失败也记下来：一份写坏了的配置，之后每一次读都该报同一个错，
// 而不是每次重读一遍、或者第二次读时悄悄当成没配。
func loadLocked(path string, log *slog.Logger) error {
	if failed = danglingArg(); failed != nil {
		return failed
	}
	from := "xone.WithConfigPath"
	if path == "" {
		path, from = locate()
	}
	source = path

	switch {
	case path == "":
		log.Warn("no config file found, using defaults for everything",
			"searched", SearchPaths, "or_use", "--"+ArgKey+"=<path> or "+EnvKey)
		Debugf("no config file: searched %s, everything uses its defaults", strings.Join(SearchPaths, ", "))
		sections, claimed, ready = nil, map[string]bool{}, true
		return nil
	case !xutil.FileExist(path):
		// locate 只在文件确实存在时才返回约定路径，所以走到这里的一定是点名要的
		failed = xerror.Newf("xconfig", "config", "config file does not exist: %s", path)
		return failed
	}

	log.Info("loading config", "file", path)
	s, r, err := load(path)
	if err != nil {
		failed = err
		return err
	}
	debugReport(path, from, r)
	sections, claimed, ready = s, map[string]bool{}, true
	return nil
}

// Load 读配置文件，把结果留在包里，替换掉原来的那一份。只给测试用——
// 生产路径走 Ensure 和第一次读时的自动加载。
func Load(path string) error {
	s, _, err := load(path)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	sections, claimed, ready, source, failed = s, map[string]bool{}, true, path, nil
	return nil
}

// load 读配置文件，解析、合并、展开占位符，返回各顶层 key 对应的节点。
//
// 读的不一定只有一个文件：base 文件可以 Import 别的文件，
// 激活的 profile 还会带上 application-{profile}.yml。
// 合并规则和优先级见 loadAll 与 merge。
//
// 另外返回这次加载的经过（读了哪些文件、激活了哪些 profile、最终的配置），XONE_DEBUG 打出来。
func load(path string) (map[string]*yaml.Node, *report, error) {
	files, active, activeFrom, err := loadAll(path)
	if err != nil {
		return nil, nil, err
	}
	r := &report{profiles: active, profilesFrom: activeFrom}
	for _, f := range files {
		r.files = append(r.files, f.path)
	}
	// 合并之后一个块里的字段可能来自好几个文件，报错时得说清是哪一个
	from := map[*yaml.Node]string{}
	for _, f := range files {
		remember(f.node, f.path, from)
	}
	origins.Store(&from)

	var root *yaml.Node
	for _, f := range files {
		// Profiles 只认 base 文件里那一份，loadAll 已经把它取走了，这里还剩的都是
		// 被引进来的文件写的。它们再去激活 profile 的话，
		// 「谁决定加载哪些文件」就成了一个和加载顺序互相依赖的问题。
		// 写了就算，哪怕是个空列表——那多半是抄 base 时带过来的，留着只会让人
		// 以为它在起作用
		if takeFromApp(f.node, ProfilesKey) != nil {
			return nil, nil, xerror.Newf("xconfig", "config",
				"%s.%s may only be set in the base config file, found it in %s", AppKey, ProfilesKey, f.path)
		}
		root = merge(root, f.node)
	}
	if absent(root) {
		return nil, r, nil
	}

	// 占位符在全部合并完之后统一展开一次：base 里一个必填的 ${VAR}
	// 如果已经被 profile 文件覆盖掉了，就不该再要求它必须设置
	var missing []string
	values := map[*yaml.Node]string{}
	expand(root, &missing, values)
	if len(missing) > 0 {
		return nil, nil, xerror.Newf("xconfig", "config", "environment variables not set: %s", missingNames(missing))
	}
	expanded.Store(&values)
	r.root, r.raw = root, values

	s, err := topLevel(root)
	return s, r, err
}

// Unmarshal 把 key 那一段解进 into，并记下这一块有人读过。
//
// into 里已经是默认值，文件里没写的字段保持不变——所以不需要指针字段来区分
// 「没配」和「配成零值」。整块没配时 into 原样不动，返回 nil。
//
// 在 Start 之前任何时候调都行：还没加载的话先加载，读到的永远是配置文件里的最终值，
// 不会因为读得早就静默拿到一份默认值。
func Unmarshal(key string, into any) error {
	if err := checkTarget(into); err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()

	node, err := sectionLocked(key)
	if err != nil {
		return err
	}
	if node == nil {
		return nil // 没配这一块，或者写了个空块，都保持默认值
	}
	if err := DecodeStrict(node, into); err != nil {
		return xerror.Newf("xconfig", "config", "invalid config %s: %w", key, err)
	}

	// 配置结构体自己说得清什么算合法，就让它说——在建任何东西之前拦住配错的
	// 配置。有些配错不会让初始化失败，只是让某个行为永远走不到（XFlow 的
	// RollbackTimeout 配成 0 会让每次回滚一进去就判超时、补偿全被跳过，
	// 而流程本身看起来一切正常），那种只能靠这里拦。
	if v, ok := into.(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return xerror.Newf("xconfig", "config", "invalid config %s: %w", key, err)
		}
	}
	return nil
}

// Has 报告配置文件里有没有写这一块。用于「配了才初始化」。
//
// 和 Unmarshal 一样记一笔认领：问过就算有人要。少了这一笔，
// 「XRedis:」这样的空块会让启动直接失败——Has 返回 false，那个包就此跳过、
// 不再 Unmarshal，于是这个 key 没人认领，而 Unclaimed 报出来的原因
// （拼错了、或者忘了 import）两条都不成立，使用者照着查什么都查不出来。
//
// 和 Unmarshal 一样，还没加载的话先加载。加载失败时报告没有：那个错误由
// xone.Run 启动时原样报出来，这里再报一遍只会让可选组件各说各的。
func Has(key string) bool {
	mu.Lock()
	defer mu.Unlock()
	node, err := sectionLocked(key)
	return err == nil && node != nil
}

// sectionLocked 取出 key 那一段，调用方持有 mu。Unmarshal、Has、UnmarshalClients 共用。
//
// 三步的顺序是有意的：还没加载就先加载（读得早也拿到文件里的值）；
// 加载成功就记一笔认领——问过就算有人要，哪怕这一块没配或者是空的；
// 最后才看有没有写。没配这一块、或者写了个空块时返回 nil。
// 加载失败时原样返回那个错误，不记认领。
func sectionLocked(key string) (*yaml.Node, error) {
	if err := ensureLocked(); err != nil {
		return nil, err
	}
	claimed[key] = true
	if node := sections[key]; !isEmptyNode(node) {
		return node, nil
	}
	return nil, nil
}

// Unclaimed 返回配置文件里没有任何人读过的顶层 key，按名字排序。
//
// 由框架在全部 BeforeStart 钩子跑完之后检查：一个没人读的 key 多半是拼错了，
// 或者忘了 import 对应的集成包——两种都会让人配了半天发现不生效。
func Unclaimed() []string {
	mu.Lock()
	defer mu.Unlock()

	var out []string
	for key := range sections {
		if !claimed[key] {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// LoadInto 加载一个配置文件并把其中 key 那一块取出来。只给测试用。
func LoadInto(path, key string, into any) error {
	Reset()
	if err := Load(path); err != nil {
		return err
	}
	return Unmarshal(key, into)
}

// Reset 回到「还没加载」：下一次读会重新找文件、重新加载。
//
// xone.Run 结束时调它——配置跟着一次 Run 走，同一个进程里的下一次 Run
// （测试里很常见）要读它自己的那一份。测试也直接用它。
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	sections, claimed, ready, source, failed = nil, nil, false, "", nil
	expanded.Store(nil)
	origins.Store(nil)
}

// remember 记下 n 和它下面每个节点来自 path
func remember(n *yaml.Node, path string, into map[*yaml.Node]string) {
	into[n] = path
	for _, c := range n.Content {
		remember(c, path, into)
	}
}

func topLevel(root *yaml.Node) (map[string]*yaml.Node, error) {
	out := map[string]*yaml.Node{}
	if len(root.Content) == 0 || isNull(root.Content[0]) {
		return out, nil // 只有一个 --- 的文件
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil, xerror.New("xconfig", "config", errors.New("top level of the config file must be a mapping"))
	}
	// 重复的 key 在这里已经不可能出现：每个文件解析时就查过了，见 parse
	for i := 0; i+1 < len(doc.Content); i += 2 {
		out[doc.Content[i].Value] = doc.Content[i+1]
	}
	return out, nil
}

// isEmptyNode 判断一个配置块是否没有任何内容
//
// `Demo:` 后面什么都不写，解析出来是一个 null 标量；`Demo: {}` 是一个空 mapping。
// 两种都该等同于「没配这一块」——直接交给解码器的话，前者会得到一个
// 意义不明的 EOF 错误，后者虽然能过但没必要走一趟。
func isEmptyNode(n *yaml.Node) bool {
	if n == nil {
		return true
	}
	if isNull(n) {
		return true
	}
	return (n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode) && len(n.Content) == 0
}

// expand 在解析后的节点上展开占位符，不在原始字节上做文本替换：
// 环境变量的值里若含冒号或换行，文本替换会改变 YAML 结构。
//
// values 不为 nil 时记下每个展开过的标量和它的原文，报错时按节点遮掉展开出来的值。
func expand(n *yaml.Node, missing *[]string, values map[*yaml.Node]string) {
	if n.Kind == yaml.ScalarNode && strings.Contains(n.Value, "${") {
		before := n.Value
		n.Value = placeholder.ReplaceAllStringFunc(n.Value, func(m string) string {
			idx := placeholder.FindStringSubmatchIndex(m)
			name := m[idx[2]:idx[3]]
			if v, ok := os.LookupEnv(name); ok {
				return v
			}
			if idx[4] >= 0 {
				return m[idx[4]:idx[5]]
			}
			*missing = append(*missing, name)
			return m
		})
		if n.Value != before {
			retag(n)
			if values != nil {
				values[n] = before
			}
		}
	}
	for _, c := range n.Content {
		expand(c, missing, values)
	}
}

// missingNames 没设的变量，按名字排好、每个只列一次：同一个变量用在好几处时
// 不必在报错里读到好几遍
func missingNames(missing []string) string {
	slices.Sort(missing)
	return strings.Join(slices.Compact(missing), ", ")
}

// retag 让替换过的标量按新内容重新判定类型。
//
// 解析的时候整个 ${PORT:8080} 是一段文本，所以这个标量被打上了 !!str。
// 替换之后它的内容是 8080，标签却还留在 !!str 上，于是
//
//	Port: ${PORT:8080}
//
// 会以「cannot unmarshal !!str into int」失败——占位符因此只能用在字符串字段上，
// 而文档里它是一条通用规则。清掉标签，让 yaml 按替换后的内容重新判定即可。
//
// 这不会让环境变量的值改变 YAML 结构：重新判定的对象仍是这一个标量，
// 值里的冒号、换行、星号都留在标量内部。
//
// 分三种情况：
//
// 使用者显式加了引号或写了标签（Style 非 0）时保持原样：那是明确的
// 「按字符串处理」，数字形态的密码、版本号都指望它。
//
// 替换结果为空（`${PORT:}`，或者变量设成了空串）时标成 null，也就是
// 「这一项没写」：解码时 null 让字段保持结构体里预填的默认值，不论字段
// 是什么类型。留着 !!str 的话，字符串字段被清成空串，非字符串字段以
// cannot unmarshal !!str "" into int 失败——同一个占位符在不同字段上
// 两种下场。真要空串就加引号：`"${PW:}"`。
//
// 合并发生在展开之前，所以这里的「没写」落回的是结构体默认值，
// 不是低优先级文件里写的那个值。
//
// 其余的清掉标签重新判定，但展开出来的值永远不是 null：变量的值恰好是 null、~、Null 这类
// YAML 的 null 写法时，重新判定会把它当成「没写」，字段悄悄留在默认值上。
// 那种情况固定成 !!str——字符串字段拿到字面量，别的字段报一个看得懂的类型错误。
func retag(n *yaml.Node) {
	switch {
	case n.Style != 0: // 引号或标签：保持原样
	case n.Value == "":
		n.Tag = "!!null"
	default:
		n.Tag = ""
		if n.ShortTag() == "!!null" {
			n.Tag = "!!str"
		}
	}
}

// profilesOf 取出并移除文档里的 XApp.Profiles，返回它声明的 profile 列表。
//
// 占位符在这里就展开，理由和 Import 的路径一样：要先知道激活哪些 profile，
// 才知道读哪些文件，等不到全部合并完。于是 `Profiles: ${APP_ENV:dev}` 可用。
func profilesOf(doc *yaml.Node, path string) ([]string, error) {
	node := takeFromApp(doc, ProfilesKey)
	if node == nil {
		return nil, nil
	}

	var missing []string
	expand(node, &missing, nil)
	if len(missing) > 0 {
		return nil, xerror.Newf("xconfig", "config",
			"environment variables not set in %s.%s of %s: %s", AppKey, ProfilesKey, path, missingNames(missing))
	}

	active, ok := scalarList(node)
	if !ok {
		return nil, xerror.Newf("xconfig", "config",
			"%s.%s in %s must be a profile name, a comma-separated string or a list", AppKey, ProfilesKey, path)
	}

	// 列表和逗号分隔的字符串两种写法都收，跟 Spring 一样：
	// Profiles: "dev,prod" 解出来是一个元素，这里再拆开
	out := make([]string, 0, len(active))
	for _, a := range active {
		out = append(out, splitProfiles(a)...)
	}
	return out, nil
}
