package xflow

import (
	"context"
	"fmt"
	"time"

	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xhook"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XFlow"

// Config 流程编排配置
type Config struct {
	// Monitor 是否开启监控回调。默认开启。
	//
	// 关掉之后 Execute 一次监控回调都不走，是真正的零开销。
	Monitor bool `yaml:"Monitor"`

	// RollbackTimeout 回滚全部步骤的总预算。默认 30s。
	//
	// 回滚不沿用调用方的 context（否则请求一超时，补偿必然全部失败），
	// 改由这一项单独限时，免得补偿逻辑无限期挂住退出流程。
	// 不看 ctx 的 Rollback 也挂不住：到点就不再等它，记进 RollbackErrors，
	// 没轮到的步骤同样逐个记下；被放弃的那一步的协程仍在后台跑。
	RollbackTimeout time.Duration `yaml:"RollbackTimeout"`
}

// DefaultConfig 全部默认值集中在这里
func DefaultConfig() Config {
	return Config{Monitor: true, RollbackTimeout: 30 * time.Second}
}

// Validate 框架在配置加载完之后会调一次，在建任何东西之前。
//
// 本模块没有要初始化的资源，只有这一件事要做：RollbackTimeout 配成 0 会让
// 每次回滚一进去就判超时、所有补偿被跳过，而流程本身看起来一切正常。
// 这种配错不会让任何初始化失败，只能在这里拦住。
func (c Config) Validate() error {
	if c.RollbackTimeout <= 0 {
		return fmt.Errorf("RollbackTimeout must be > 0, got=%v", c.RollbackTimeout)
	}
	return nil
}

var cfg = DefaultConfig()

// init 本模块没有要初始化的资源，只把配置读进来——Unmarshal 会顺手调 Validate
func init() {
	xhook.BeforeStart(loadConfig, xhook.At(xhook.StageLog))
}

// loadConfig 解进一份新的默认值，校验通过才换上。
//
// 直接解进 cfg 的话，同一进程里前一次 Run 的值会带进下一次（没写的字段不回到默认值），
// 校验失败时那个非法值（比如 RollbackTimeout: 0）也已经落到了 cfg 上。
func loadConfig(context.Context) error {
	c := DefaultConfig()
	if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
		return err
	}
	cfg = c
	return nil
}
