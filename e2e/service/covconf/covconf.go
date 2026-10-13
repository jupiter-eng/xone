// Package covconf 是 e2e 服务里给配置测试用的业务配置块 Cov。
//
// 它和 Service 块互为对照：Service 在 main 里、xone.Run 之前读（docs/config.md「通用规则·什么时候读」），
// Cov 在启动钩子里读（docs/guide.md「读自己的配置」的另一种写法）。两处读到的
// 都应该是合并完 profile / Import、展开完 ${VAR} 之后的最终值，GET /probe/config 把两者一起回给测试。
//
// 不配这一块就是默认值，也不会被当成「没人认领」：启动钩子里读过它就算认领了
package covconf

import (
	"context"
	"sync"

	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xhook"
)

// Config 配置测试用的几项：一个标量、一个列表（测列表整体替换）、一个 map（测递归合并）
type Config struct {
	Label  string            `yaml:"Label"`
	Items  []string          `yaml:"Items"`
	Labels map[string]string `yaml:"Labels"`
}

// DefaultConfig 默认值
func DefaultConfig() Config { return Config{Label: "default", Items: []string{"default"}} }

var (
	mu   sync.RWMutex
	conf = DefaultConfig()
)

func init() { xhook.BeforeStart(load) }

func load(context.Context) error {
	c := DefaultConfig()
	if err := xconfig.Unmarshal("Cov", &c); err != nil {
		return err
	}
	mu.Lock()
	conf = c
	mu.Unlock()
	return nil
}

// C 启动钩子读到的 Cov 块
func C() Config {
	mu.RLock()
	defer mu.RUnlock()
	return conf
}
