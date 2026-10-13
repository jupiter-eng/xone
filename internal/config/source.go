package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/jupiter-eng/xone/xerror"
)

// AppKey 应用级的配置块：应用叫什么（Name、Version，xapp 读），跑在哪个环境、配置从哪些文件来
// （Profiles、Import，加载器自己读）。对应 Spring 把 spring.application.name、
// spring.profiles.active、spring.config.import 收在 spring 下面。
const AppKey = "XApp"

// XApp 里由加载器自己消费的两项。读完就从文档里摘掉，交给 xapp 的只剩 Name、Version
const (
	// ProfilesKey 声明激活哪些 profile，对应 Spring 的 spring.profiles.active
	ProfilesKey = "Profiles"
	// ImportKey 引入别的配置文件，对应 Spring 的 spring.config.import
	ImportKey = "Import"
)

// optionalPrefix 带这个前缀的 import 文件不存在时跳过，不报错
const optionalPrefix = "optional:"

// maxImportDepth import 的嵌套上限，防止配置写出一条看不见的深链。
// 成环走不到这里：同一个文件只读一次，环在第二次遇到时就断了
const maxImportDepth = 16

// maxResolvedNodes 一个文件在别名展开之后最多有多少个节点。
// 正常的配置只有几百个；这条线挡的是别名层层相乘的「十亿笑」——
// 十层、每层十个别名，展开后是一百亿个节点
const maxResolvedNodes = 100_000

// loaded 一个加载好的配置文件
type loaded struct {
	path string
	node *yaml.Node // 文档节点
}

// loadAll 按优先级从低到高列出所有要合并的文件。
//
// 顺序与 Spring 一致：
//
//	application.yml  <  它 import 的  <  application-prod.yml  <  prod import 的
//
// 也就是说 import 进来的会压过引它的那个文件（Spring 的说法是
// 「import 相当于插在声明它的那份文档正下方」，而下面的压过上面的），
// profile 文件又压过不带 profile 的那份。
//
// 另外返回激活的 profile 和它们是从哪来的，XONE_DEBUG 打出来。
func loadAll(base string) ([]loaded, []string, string, error) {
	seen := map[string]bool{} // 同一个文件只 import 一次，与 Spring 一致
	doc, err := read(base, true, seen)
	if err != nil {
		return nil, nil, "", err
	}

	// profile 从三个地方来，优先级：启动参数 > 环境变量 > base 文件里的 XApp.Profiles。
	// 文件里那一份要在往下走之前取出来：base 的 import 和变体都按它选文件
	declared, err := profilesOf(doc, base)
	if err != nil {
		return nil, nil, "", err
	}
	active, from := profiles(declared)

	// 主文件的 profile 变体必须存在：点名要了某个 profile 文件却不在，
	// 几乎总是名字写错了，静默跳过的结果是一份谁都没看过的配置以默认值起来。
	// 被 import 进来的文件的变体则是可选的，见 fileSet
	files, err := fileSet(base, doc, true, active, seen, 0)
	return files, active, from, err
}

// fileSet 一个已经读好的文件连同它 import 的和它的 profile 变体，按优先级从低到高：
//
//	F、F 的 import、F-p1、F-p1 的 import、F-p2、……
//
// variantRequired 说的是「F-p1 不存在算不算错」。主文件算，import 进来的不算：
// 拆成十个片段之后，要求每个片段都备齐每个环境的变体是没法用的，
// 而 profile 名写错这件事已经由主文件的变体挡住了。
func fileSet(path string, doc *yaml.Node, variantRequired bool, profiles []string, seen map[string]bool, depth int) ([]loaded, error) {
	out, err := withImports(path, doc, profiles, seen, depth)
	if err != nil {
		return nil, err
	}
	for _, p := range profiles {
		vp := profilePath(path, p)
		v, err := read(vp, variantRequired, seen)
		if err != nil {
			return nil, err
		}
		if v == nil {
			continue
		}
		// 变体自己也能 import，所以照样走 withImports
		nested, err := withImports(vp, v, profiles, seen, depth)
		if err != nil {
			return nil, err
		}
		out = append(out, nested...)
	}
	return out, nil
}

// withImports 一个已经读好的文件，后面跟着它 import 的文件（于是压过它）
func withImports(path string, doc *yaml.Node, profiles []string, seen map[string]bool, depth int) ([]loaded, error) {
	imports, err := importsOf(doc, path)
	if err != nil {
		return nil, err
	}

	out := []loaded{{path: path, node: doc}}
	for _, spec := range imports {
		target, optional := strings.CutPrefix(spec, optionalPrefix)
		// 相对路径按「引它的那个文件所在目录」解析，而不是进程的工作目录：
		// 配置目录整个搬个位置，里面的相对引用不该跟着失效
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		if depth+1 > maxImportDepth {
			return nil, xerror.Newf("xconfig", "config",
				"import nested more than %d levels deep at %s", maxImportDepth, target)
		}
		d, err := read(target, !optional, seen)
		if err != nil {
			return nil, err
		}
		if d == nil {
			continue
		}
		// import 进来的文件同样有 profile 变体，但变体不存在不算错
		nested, err := fileSet(target, d, false, profiles, seen, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, nested...)
	}
	return out, nil
}

// read 读并解析一个文件。已经读过、或者不要求存在而它不在时返回 nil
func read(path string, required bool, seen map[string]bool) (*yaml.Node, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if seen[abs] {
		// 同一个文件只算一次，与 Spring 一致。菱形引用（两个片段都引了
		// 同一份公共配置）和成环都在这里断开，后者因此不会报错
		return nil, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if !required && os.IsNotExist(err) {
			return nil, nil
		}
		return nil, xerror.Newf("xconfig", "config", "read config %s: %w", path, err)
	}
	seen[abs] = true
	return parse(path, raw)
}

// importsOf 取出并移除文档里的 XApp.Import。
//
// 路径上的 ${VAR} 在这里就展开：要先知道读哪个文件，才谈得上合并。
// 其余字段的展开留到全部合并完之后——base 里一个必填的 ${VAR}
// 如果被 profile 文件覆盖掉了，就不该再要求它必须设置。
func importsOf(doc *yaml.Node, path string) ([]string, error) {
	node := takeFromApp(doc, ImportKey)
	if node == nil {
		return nil, nil
	}

	var missing []string
	expand(node, &missing, nil)
	if len(missing) > 0 {
		return nil, xerror.Newf("xconfig", "config",
			"environment variables not set in %s.%s of %s: %s", AppKey, ImportKey, path, missingNames(missing))
	}

	out, ok := scalarList(node)
	if !ok {
		return nil, xerror.Newf("xconfig", "config",
			"%s.%s in %s must be a path or a list of paths", AppKey, ImportKey, path)
	}
	return out, nil
}

// scalarList 取「一个字符串，或一个字符串列表」形状的值。
// null 和空串算一个都没有；形状不对时 ok 为 false
func scalarList(n *yaml.Node) (out []string, ok bool) {
	switch n.Kind {
	case 0: // 解码进 yaml.Node 的字段压根没写
		return nil, true
	case yaml.ScalarNode:
		if isNull(n) || n.Value == "" {
			return nil, true
		}
		return []string{n.Value}, true
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return nil, false
			}
			out = append(out, c.Value)
		}
		return out, true
	}
	return nil, false
}

// parse 解析一个配置文件，把两件只能在合并之前做的事一并做掉。
//
//   - 一个文件只能有一份文档，见 onlyDocument。
//   - 重复的 key 报错，带文件和行号。合并按名字对齐 key，重复在那一步就被
//     吞掉了，后面的严格解码再也看不见它——于是同一个块写两遍能正常加载，
//     静默地以后一份为准，而写的人多半以为两份都生效了。从前只有第一个文件
//     能靠严格解码查出来，profile 和 import 进来的文件全都漏掉。
//   - 别名换成它指向的内容。各顶层块是分开解码的，别名若指向另一个块里的
//     锚点，解码那一块时锚点已经不在了（unknown anchor）；展开之后
//     profile 文件也就能覆盖别名带进来的字段。
//   - 合并键 `<<` 摊平成普通的 key，见 flattenMerges。
func parse(path string, raw []byte) (*yaml.Node, error) {
	doc, err := onlyDocument(raw)
	if err != nil {
		return nil, xerror.Newf("xconfig", "config", "parse config %s: %w", path, err)
	}
	if err := checkDuplicates(&doc); err != nil {
		return nil, xerror.Newf("xconfig", "config", "config %s: %w", path, err)
	}
	budget := maxResolvedNodes
	out, err := resolveAliases(&doc, &budget)
	if err != nil {
		return nil, xerror.Newf("xconfig", "config", "config %s: %w", path, err)
	}
	if err := flattenMerges(out); err != nil {
		return nil, xerror.Newf("xconfig", "config", "config %s: %w", path, err)
	}
	return out, nil
}

// onlyDocument 解析 raw 里唯一的那份文档；空文件（只有注释也算）得到 Kind 为 0 的节点。
//
// 一个文件里用 --- 隔开好几份文档不支持：yaml.Unmarshal 只读第一份，
// 后面的整段静默丢掉，连「没人读的配置块」都报不出来——写的人以为那段生效了。
// 分环境的写法是 application-{profile}.yml，所以直接报错，指过去。
//
// 只有「空」的文档不算第二份（量过，go.yaml.in/yaml/v3 v3.0.4）：开头一个 ---、
// 结尾多写一个 ---（后面什么都没有、只有注释、或者写着 ~）解出来都是一个 null，
// 结尾的 ... 则根本不产生文档。它们照常加载。
func onlyDocument(raw []byte) (yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	found := false
	for {
		var d yaml.Node
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			return doc, nil
		}
		if err != nil {
			return yaml.Node{}, err
		}
		if len(d.Content) == 0 || isNull(d.Content[0]) {
			if !found && doc.Kind == 0 {
				doc = d // 全是空文档时留第一份：「只有一个 --- 的文件」照旧是它
			}
			continue
		}
		if found {
			return yaml.Node{}, fmt.Errorf("multiple YAML documents in one file are not supported "+
				"(another document starts at line %d): put per-environment settings in application-{profile}.yml instead", d.Line)
		}
		doc, found = d, true
	}
}

// checkDuplicates 查出同一个 mapping 里重复的 key
func checkDuplicates(n *yaml.Node) error {
	if n.Kind == yaml.MappingNode {
		lines := map[string]int{} // key → 第一次出现的行号
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i]
			if prev, dup := lines[key.Value]; dup {
				return fmt.Errorf("duplicate key %q at line %d (first seen at line %d)", key.Value, key.Line, prev)
			}
			lines[key.Value] = key.Line
		}
	}
	for _, c := range n.Content {
		if err := checkDuplicates(c); err != nil {
			return err
		}
	}
	return nil
}

// resolveAliases 返回 n 的一份副本，其中的别名都换成了它指向的内容。
//
// 每处引用各得一份副本而不是共用同一个节点：占位符按节点展开，
// 共用的话同一个值会被展开两次，环境变量的值里恰好带着 ${ 时就错了。
func resolveAliases(n *yaml.Node, budget *int) (*yaml.Node, error) {
	if n.Kind == yaml.AliasNode {
		return resolveAliases(n.Alias, budget)
	}
	if *budget--; *budget < 0 {
		return nil, fmt.Errorf("alias expansion exceeds %d nodes", maxResolvedNodes)
	}
	out := *n
	out.Anchor = "" // 别名都已展开，锚点再留着只会在序列化时多出一个没人引用的定义
	out.Content = make([]*yaml.Node, len(n.Content))
	for i, c := range n.Content {
		r, err := resolveAliases(c, budget)
		if err != nil {
			return nil, err
		}
		out.Content[i] = r
	}
	return &out, nil
}

// flattenMerges 把每个 mapping 里的合并键 `<<` 摊平成普通的 key，就地改 n。
//
// yaml 的规矩是「写明的 key 压过 << 并进来的」，这条规矩只该在一个文件内部用。
// 留到几个文件合并完再交给解码器的话，profile 文件里 `<<: *prod` 带进来的
// Addr 会输给 base 里写明的 Addr——优先级高的文件反而没生效。
// 在解析时摊平，合并时看到的就只有普通的 key，谁压过谁全按文件的优先级。
//
// 规则和 yaml.v3 解码时一致（量过，go.yaml.in/yaml/v3 v3.0.4）：
//   - 写明的 key 压过并进来的，不论写在 << 之前还是之后；
//   - << 后面是列表时，靠前的压过靠后的；
//   - 并进来的只有那一层：写明了 Nested 就整个用写明的，不和并进来的 Nested 再合；
//   - 并进来的 mapping 自己也能带 <<，先摊平它。
//
// 调用前别名已经展开成各自的副本（见 resolveAliases），所以就地改不会改到别处。
func flattenMerges(n *yaml.Node) error {
	for _, c := range n.Content {
		if err := flattenMerges(c); err != nil {
			return err
		}
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}

	own := map[string]bool{} // 写明的 key
	merges := false
	for i := 0; i+1 < len(n.Content); i += 2 {
		if isMergeKey(n.Content[i]) {
			merges = true
		} else {
			own[n.Content[i].Value] = true
		}
	}
	if !merges {
		return nil
	}

	// 并进来的 key 放在 << 原来的位置上，节点顺序因此和文件里读到的一致
	out := make([]*yaml.Node, 0, len(n.Content))
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if !isMergeKey(k) {
			out = append(out, k, v)
			continue
		}
		sources := []*yaml.Node{v}
		if v.Kind == yaml.SequenceNode {
			sources = v.Content
		}
		for _, src := range sources {
			if src.Kind != yaml.MappingNode {
				return fmt.Errorf("line %d: merge key << needs a mapping or a list of mappings", k.Line)
			}
			for j := 0; j+1 < len(src.Content); j += 2 {
				if key := src.Content[j].Value; !own[key] {
					own[key] = true // 靠前的来源压过靠后的
					out = append(out, src.Content[j], src.Content[j+1])
				}
			}
		}
	}
	n.Content = out
	return nil
}

// isMergeKey 是不是合并键：`<<` 且没加引号（加了引号的 "<<" 是个普通字符串 key）
func isMergeKey(k *yaml.Node) bool {
	return k.Kind == yaml.ScalarNode && k.Value == "<<" && k.ShortTag() == "!!merge"
}

// takeFromApp 取出 XApp 下面的某个 key 并把它从文档里摘掉，没有则返回 nil。
//
// 摘完 XApp 空了就连它一起摘掉：只写了 Profiles / Import 的 XApp 交给 xapp 时
// 是一个空块，没 import xapp 的程序会把它当成「没人读的配置块」启动失败
func takeFromApp(doc *yaml.Node, key string) *yaml.Node {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	i := keyIndex(root, AppKey)
	if i < 0 || root.Content[i+1].Kind != yaml.MappingNode {
		return nil
	}
	app := root.Content[i+1]
	val := removeKey(app, key)
	if val != nil && len(app.Content) == 0 {
		root.Content = append(root.Content[:i], root.Content[i+2:]...)
	}
	return val
}

// removeKey 取出 mapping m 里的某个 key 并把它摘掉，没有则返回 nil
func removeKey(m *yaml.Node, key string) *yaml.Node {
	i := keyIndex(m, key)
	if i < 0 {
		return nil
	}
	val := m.Content[i+1]
	m.Content = append(m.Content[:i], m.Content[i+2:]...)
	return val
}

// keyIndex mapping m 里 key 所在的下标（值在 i+1），没有则返回 -1。
// 重复的 key 在解析时就拦下了（见 checkDuplicates），所以第一个就是唯一的那个
func keyIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

// profilePath 由 application.yml 推出 application-prod.yml，目录和扩展名都跟着原文件
func profilePath(base, profile string) string {
	ext := filepath.Ext(base)
	return strings.TrimSuffix(base, ext) + "-" + profile + ext
}
