package config

import (
	"go.yaml.in/yaml/v3"
)

// merge 把 override 叠加到 base 上，返回叠加后的结果。
//
// 规则跟 Spring 的一致（使用者多半是从那边过来的）：
//
//   - map：递归合并，两边都有的 key 用 override 的
//   - 列表：整个替换，不逐元素合并
//   - 标量：override 覆盖 base
//
// 列表整体替换是这里最容易被误解的一条，所以说清楚为什么：
// 逐元素合并的话，base 写了 [A, B]、override 写了 [C]，结果会是 [C, B]——
// 使用者以为自己换掉了整张表，实际只换掉了第一项，而剩下那项来自
// 另一个文件。Spring 在这一点上也是整体替换，理由相同。
//
// 想往列表里追加就把完整的列表写全。
//
// 「没写」叠上来不改变任何东西：空文件（只有注释也算）解析出来是一个
// Kind 为 0 的节点，`Demo:` 和 `Addr: ~` 是 null。前者从前被当成标量，
// 整个替换掉了前面所有文件——profile 文件里只写一行 # TODO，全部配置
// 就悄悄退回默认值；后者让 `Demo:` 清掉 base 的整块，而 `Demo: {}` 却保留它。
// 单个文件里 null 保持默认值，叠加时也就该保持低优先级文件里的值。
// 要清空就写出空的那个值：`""`、`[]`。
func merge(base, override *yaml.Node) *yaml.Node {
	if absent(override) {
		return base
	}
	if absent(base) {
		return override
	}

	// 文档节点：往里走一层再合并
	if base.Kind == yaml.DocumentNode && override.Kind == yaml.DocumentNode {
		if len(base.Content) == 0 {
			return override
		}
		if len(override.Content) == 0 {
			return base
		}
		out := *base
		out.Content = []*yaml.Node{merge(base.Content[0], override.Content[0])}
		return &out
	}

	// 只有两边都是 map 才递归，其余一律整体替换
	if base.Kind != yaml.MappingNode || override.Kind != yaml.MappingNode {
		return override
	}
	return mergeMapping(base, override)
}

// mergeMapping 合并两个 map 节点，保持 base 里 key 的出现顺序，
// override 里新增的 key 追加在后面。
//
// 保序不是为了好看：报错信息里带行号，节点顺序乱了之后「第几行」对不上，
// 而重复 key 检查正是靠行号把两处指出来的。
func mergeMapping(base, override *yaml.Node) *yaml.Node {
	out := *base
	out.Content = nil

	// override 里的 key → value，用完就删，剩下的是新增的
	pending := map[string]*yaml.Node{}
	order := make([]*yaml.Node, 0, len(override.Content)/2) // override 里的 key 节点，保持书写顺序
	for i := 0; i+1 < len(override.Content); i += 2 {
		k := override.Content[i]
		if _, dup := pending[k.Value]; !dup {
			order = append(order, k)
		}
		pending[k.Value] = override.Content[i+1]
	}

	for i := 0; i+1 < len(base.Content); i += 2 {
		key, val := base.Content[i], base.Content[i+1]
		if ov, ok := pending[key.Value]; ok {
			out.Content = append(out.Content, key, merge(val, ov))
			delete(pending, key.Value)
			continue
		}
		out.Content = append(out.Content, key, val)
	}

	// override 独有的 key，按它们在 override 里的顺序追加
	for _, k := range order {
		if v, ok := pending[k.Value]; ok { // 不在的已经被上面消费掉了
			out.Content = append(out.Content, k, v)
		}
	}
	return &out
}

// absent 判断一个节点是不是「这里什么都没写」：空文件，或者 null
func absent(n *yaml.Node) bool {
	return n == nil || n.Kind == 0 || isNull(n)
}

// isNull 判断一个节点是不是 null：`Demo:`、`Demo: ~`、`Demo: null`
func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}
