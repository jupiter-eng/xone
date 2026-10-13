// Package xapp 提供应用自身的身份：名字和版本。
//
// 这些是多个组件共用的事实——链路要它做 service.name，接口文档要它做默认标题和版本。
// 放在任何一个组件的配置里，另外几个就得重复配一遍，然后总有一天它们会不一致。
// 所以单独一块，谁都能读。
//
//	XApp:
//	  Name: xone.demo.app
//	  Version: v1.2.0
//
// 本包不初始化任何东西，只是在配置加载时认领 XApp 这一块（其中的 Profiles、Import 由配置加载本身读，到不了这里）。
package xapp

import (
	"context"

	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xhook"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XApp"

// Config 应用身份
type Config struct {
	// Name 应用名，建议全局唯一，如 team.system.app
	Name string `yaml:"Name"`

	// Version 应用版本，如 v1.2.0
	Version string `yaml:"Version"`
}

// DefaultConfig 默认值。名字留空——猜一个名字比没有名字更糟：
// 链路上会出现一堆同名的 unknown，反而看不出是谁。
func DefaultConfig() Config { return Config{} }

var cfg = DefaultConfig()

// init 本包没有要初始化的资源，只把 XApp 这一块读进来。
//
// 挂在 StageLog：链路要拿 XApp.Name 当服务名，它在 StageTelemetry，
// 所以这一块必须更早读好。
func init() {
	xhook.BeforeStart(loadConfig, xhook.At(xhook.StageLog))
}

// loadConfig 解进一份新的默认值，成功了才换上。
//
// 直接解进 cfg 的话，同一进程里前一次 Run 的值会带进下一次（没写的字段不回到默认值），
// 解码失败时写了一半的值也已经落到了 cfg 上。
func loadConfig(context.Context) error {
	c := DefaultConfig()
	if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
		return err
	}
	cfg = c
	return nil
}

// Name 返回应用名，未配置时为空字符串。
//
// 配置在其余组件启动之前就读好了，所以任何启动钩子里读到的都是最终值。
func Name() string { return cfg.Name }

// Version 返回应用版本，未配置时为空字符串。
func Version() string { return cfg.Version }
