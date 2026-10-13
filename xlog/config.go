// Package xlog 基于标准库 log/slog 提供配置驱动的日志：级别、格式、控制台与文件轮转。
//
// 使用者拿到的是原生的 *slog.Logger，业务代码直接用标准库的
// slog.Info / slog.InfoContext 即可，不需要认识本包。
package xlog

import (
	"fmt"
	"time"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XLog"

// 日志格式
const (
	FormatJSON = "json"
	FormatText = "text"
)

// Config 日志配置
type Config struct {
	// Level 日志级别：debug / info / warn / error
	Level string `yaml:"Level"`

	// Format 输出格式：json / text
	Format string `yaml:"Format"`

	// Timezone 日志时间戳按哪个时区渲染，IANA 时区名，如 Europe/Berlin。
	// 留空表示跟随进程的本地时区（容器里通常就是 UTC）。
	//
	// 配了却加载不到会直接启动失败，不会悄悄退回本地时区——
	// 那意味着你以为在看柏林时间，实际是 UTC，差一两个小时而毫无提示。
	// scratch / distroless 这类镜像里没有 /usr/share/zoneinfo，
	// 需要在自己的 main 包里加一行把时区库编进二进制（约 400KB）：
	//
	//	import _ "time/tzdata"
	//
	// 框架不替你编进去：没用到这项的人不该背这 400KB。
	Timezone string `yaml:"Timezone"`

	// AddSource 是否记录打日志的代码位置。有开销，默认关闭。
	AddSource bool `yaml:"AddSource"`

	// Console 是否打到标准输出，由部署环境的日志采集组件收集。默认开启。
	Console bool `yaml:"Console"`

	// File 文件输出
	File FileConfig `yaml:"File"`

	// Fields 每条日志都带的静态字段。值可以用 ${VAR} 从环境变量取，比如 K8s 用 Downward API
	// 注入的 POD_NAME、NODE_NAME；${VAR:} 没设时是空串，空串的字段不写。
	//
	// 框架默认已带 service（XApp.Name）、version（XApp.Version）、hostname（主机名，
	// K8s 里就是 Pod 名）、pid。同名的以这里为准，写成空串就是不要这个默认字段。
	// 这些值启动时算一次、序列化一次：实测默认的 4 个（Pod 名那么长的 hostname）每行多 88 字节，
	// 时间和分配都量不出差别（BenchmarkHandle_WithIdentityFields 对照 BenchmarkHandle_Bare）。
	Fields map[string]string `yaml:"Fields"`
}

// FileConfig 文件输出配置
type FileConfig struct {
	// Enable 是否写文件。默认关闭。
	Enable bool `yaml:"Enable"`

	// Path 日志目录
	Path string `yaml:"Path"`

	// Name 日志文件名。实际文件是 {Name}.{时间后缀}，
	// 另有一个指向当前文件的同名符号链接，便于 tail 跟随。
	Name string `yaml:"Name"`

	// RotateTime 轮转周期。默认一天，按本地时区对齐。至少 1 分钟。
	//
	// 文件名的时间后缀最细到分钟：写成 0s 时每分钟一个文件，写成 30s 时
	// 两个周期落在同一个文件名上、实际还是每分钟轮转——都是配了 A 跑的是 B，
	// 所以短于 1 分钟直接启动失败。
	RotateTime time.Duration `yaml:"RotateTime"`

	// MaxAge 历史文件保留时长，0 表示不清理。默认 7 天。不能为负。
	//
	// 启动时清一次，之后每次轮转清一次。只清 {Name}.{时间后缀} 这种
	// 我们自己命名的文件，同目录下的 app.log.bak、app.log.1.gz 一律不碰。
	MaxAge time.Duration `yaml:"MaxAge"`

	// Perm 日志文件权限，按八进制解析的字符串。默认 "0644"，也认 "644" 和 "0o644"。
	//
	// 用字符串而不是数字：实测 yaml.v3 把数字 0644 按八进制解析成 420（正是 0o644），
	// 可漏掉前导 0 写成 644 就是十进制 644 = 0o1204（--w----r-T），不报错也不提示。
	// 字符串一律按八进制解析，写不写前导 0 结果都一样；不加引号也行，
	// 进字符串字段的 0644 还是 "0644"。
	Perm string `yaml:"Perm"`
}

// validate 校验不依赖文件系统的那些字段。
//
// 不看 Enable：默认值本来就合法，能走到这里报错的只有显式写错的值，
// 而写错了就是写错了，不该因为今天恰好没开文件输出就放过去。
func (c FileConfig) validate() error {
	if c.RotateTime < time.Minute {
		return fmt.Errorf("File.RotateTime=[%v] must be >= 1m: log file names are only precise to the minute", c.RotateTime)
	}
	if c.MaxAge < 0 {
		return fmt.Errorf("File.MaxAge=[%v] must not be negative, use 0 to keep every file", c.MaxAge)
	}
	return nil
}

// DefaultConfig 全部默认值集中在这里。
//
// 文件里没写的字段保持这些值，写了的覆盖——包括显式写成零值（false / 0 / ""）。
func DefaultConfig() Config {
	return Config{
		Level:   "info",
		Format:  FormatJSON,
		Console: true,
		File: FileConfig{
			Name:       "app.log",
			RotateTime: 24 * time.Hour,
			MaxAge:     7 * 24 * time.Hour,
			Perm:       "0644",
		},
	}
}
