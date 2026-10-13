package config

import (
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/jupiter-eng/xone/internal/xclient"
	"github.com/jupiter-eng/xone/xerror"
)

const (
	clientsKey = "Clients"

	// DefaultClientName 单实例写法被规整成的名字，也就是 C() 不带参数时取的那个。
	// 规整和取用必须是同一个名字，所以只在 xclient 写一处；方向是这里依赖 xclient：
	// 反过来的话 xclient 就间接带上了 yaml，不再是零第三方依赖
	DefaultClientName = xclient.DefaultName
)

// UnmarshalClients 解一个「既支持单实例也支持多实例」的配置块，并记下这一块有人读过。
//
// 整块没配（或者是个空块）时返回 nil，不报错。别的规矩和 Unmarshal 一样：
// 还没加载的话先加载；实例的类型若实现了 Validate() error，每个实例解完调一次。
func UnmarshalClients[C any](key string, defaults func() C) (map[string]C, error) {
	mu.Lock()
	defer mu.Unlock()

	node, err := sectionLocked(key)
	if err != nil || node == nil {
		return nil, err
	}
	out, err := decodeClients(node, defaults)
	if err != nil {
		return nil, xerror.Newf("xconfig", "config", "invalid config %s: %w", key, err)
	}
	return out, nil
}

// decodeClients 看有没有 Clients 决定按哪种写法解：
//
//	XGorm:                  # 单实例，直接写字段，名字就是 default
//	  DSN: "${DB_DSN}"
//
//	XGorm:                  # 多实例，按名字写
//	  Clients:
//	    default: {DSN: "${DB_DSN}"}
//	    report:  {DSN: "${REPORT_DSN}"}
//
// 两种混着写直接报错：那时候「default 到底是哪个」没有一个不让人意外的答案。
//
// 两种写法里的每个实例都先铺上 defaults 再解，所以文件里没写的字段保持默认——
// 实例类型不必自己写 UnmarshalYAML。
func decodeClients[C any](n *yaml.Node, defaults func() C) (map[string]C, error) {
	list := valueOf(n, clientsKey)
	if list == nil {
		single, err := decodeClient(n, defaults)
		if err != nil {
			return nil, err
		}
		return map[string]C{DefaultClientName: single}, nil
	}

	// 混用先自己认出来，别指望从解码错误里读。
	// 交给解码器的话，实例里一个字段拼错（Clients.default.DSNN）报的也是
	// 「不能混用」——而那份配置根本没混用，使用者会照着这句话去改一个没问题的地方。
	if stray := keysExcept(n, clientsKey); len(stray) > 0 {
		return nil, fmt.Errorf("cannot mix the single- and multi-instance forms: with %s present, "+
			"%s belong to no instance — move them into one, or drop %s and use the single-instance form",
			clientsKey, strings.Join(stray, ", "), clientsKey)
	}
	if isEmptyNode(list) {
		return nil, fmt.Errorf("%s is empty: either list instances under it, or remove the whole block", clientsKey)
	}
	if list.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: %s must map instance names to their config", newChecker().at(list), clientsKey)
	}

	// 逐个实例铺默认值再解：直接解进 map[string]C 的话，map 的 value 是从零值
	// 开始的，没写的字段全成了零值。每个实例的问题收齐了一起报
	var errs []error
	out := make(map[string]C, len(list.Content)/2)
	for i := 0; i+1 < len(list.Content); i += 2 {
		name := list.Content[i].Value
		c, err := decodeClient(list.Content[i+1], defaults)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s.%s: %w", clientsKey, name, err))
		}
		out[name] = c
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

// decodeClient 铺上默认值再严格解一个实例。
// 只写了名字（report: 后面是空的）是 null，解完就是全用默认值
func decodeClient[C any](n *yaml.Node, defaults func() C) (C, error) {
	c := defaults()
	if err := DecodeStrict(n, &c); err != nil {
		return c, err
	}
	// Validate 的错误前面补上这个实例在哪个文件第几行：它说得出哪个字段不对，
	// 却不知道自己是从哪儿读来的，而多实例、多文件合并之后，光有字段名不好找
	if v, ok := any(&c).(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return c, fmt.Errorf("%s: %w", newChecker().at(n), err)
		}
	}
	return c, nil
}

// valueOf mapping 里 key 对应的值，没有则返回 nil
func valueOf(node *yaml.Node, key string) *yaml.Node {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	if i := keyIndex(node, key); i >= 0 {
		return node.Content[i+1]
	}
	return nil
}

// keysExcept 列出 mapping 里除 except 之外的 key，保持书写顺序
func keysExcept(node *yaml.Node, except string) []string {
	var out []string
	for i := 0; i+1 < len(node.Content); i += 2 {
		if k := node.Content[i].Value; k != except {
			out = append(out, k)
		}
	}
	return out
}
