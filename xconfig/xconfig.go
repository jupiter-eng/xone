// Package xconfig 提供集成包写配置解码时需要的那一点东西。
//
// 框架读配置的规矩是「默认值预填在结构体里、未知字段是错误」。
// 结构体字段上这两条都自动成立，但**集合装不下默认值**：
// map 的 value 和切片元素都是从零值开始解的，框架不知道该拿什么去填。
//
// 多实例的 Clients 块不用操这个心：UnmarshalClients 拿你给的默认值逐个实例铺好再解。
// 别的集合要默认值，就给元素类型写一个 UnmarshalYAML，先铺默认值再解：
//
//	func (c *ClientConfig) UnmarshalYAML(n *yaml.Node) error {
//		*c = DefaultClientConfig()
//		type raw ClientConfig // 换个类型，否则这里会递归调用自己
//		return xconfig.DecodeStrict(n, (*raw)(c))
//	}
//
// 这里必须用 DecodeStrict 而不是 n.Decode：后者不带严格检查，
// 于是「字段拼错就启动失败」这条保证会在集合里悄悄失效。
package xconfig

import (
	"go.yaml.in/yaml/v3"

	"github.com/jupiter-eng/xone/internal/config"
)

// Unmarshal 把配置文件里 key 那一块解进 into。
//
//	func initXRedis(ctx context.Context) error {
//		c := DefaultConfig()
//		if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
//			return err
//		}
//		...
//	}
//
// into 里已经是默认值，文件里没写的字段保持不变——所以不需要指针字段来区分
// 「没配」和「配成零值」。整块没配时 into 原样不动，返回 nil；要区分
// 「没配」和「配了」用 Has。
//
// 认不出的字段是错误。into 若实现了 Validate() error，解完会调一次。
//
// **在 Start 之前任何时候调都行**：在 main 里、在 xone.Run 之前、在启动钩子里都一样。
// 第一次调用时框架才去找配置文件、加载它，读到的永远是最终值——不会因为
// 读得早就静默拿到一份默认值。只在 Start 里才第一次读的块不算有人认领：
// 启动钩子跑完时框架把没人读过的顶层 key 报成启动失败。
func Unmarshal(key string, into any) error { return config.Unmarshal(key, into) }

// Has 报告配置文件里有没有写这一块，用于「配了才初始化」。
//
//	func initXRedis(ctx context.Context) error {
//		if !xconfig.Has(ConfigKey) {
//			return nil // 没配就不建，本模块是可选依赖
//		}
//		...
//	}
//
// 和 Unmarshal 一样，问过这一块就算你认领了它：框架在全部启动钩子跑完之后
// 会把没人认领的顶层 key 报出来，跳过的那些不该落在那张名单里。
func Has(key string) bool { return config.Has(key) }

// UnmarshalClients 解一个「既支持单实例也支持多实例」的配置块，按名字给出每个实例。
//
//	XGorm:                  # 单实例，直接写字段，名字就是 default
//	  DSN: "${DB_DSN}"
//
//	XGorm:                  # 多实例，按名字写
//	  Clients:
//	    default: {DSN: "${DB_DSN}"}
//	    report:  {DSN: "${REPORT_DSN}"}
//
//	clients, err := xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)
//
// 看有没有 Clients 决定按哪种解。两种混着写直接报错：那时候
// 「default 到底是哪个」没有一个不让人意外的答案；写了 Clients 却是空的也报错。
//
// defaults 提供单个实例的默认值，两种写法里的每个实例都先铺上它再解，
// 所以文件里没写的字段保持默认——实例类型不必自己写 UnmarshalYAML。
// 整块没配时返回 nil；其余规矩与 Unmarshal 相同（认领、先加载、Validate）。
func UnmarshalClients[C any](key string, defaults func() C) (map[string]C, error) {
	return config.UnmarshalClients(key, defaults)
}

// DecodeStrict 把一个 YAML 节点解进 v，认不出的字段是错误。
//
// 只在集合元素自己的 UnmarshalYAML 里用（见包文档）：节点是 yaml 交给
// UnmarshalYAML 的那一个，报错里的文件和行号照样是配置文件里的。
func DecodeStrict(node *yaml.Node, v any) error { return config.DecodeStrict(node, v) }

// DefaultClientName 单实例写法被规整成的名字
const DefaultClientName = config.DefaultClientName
