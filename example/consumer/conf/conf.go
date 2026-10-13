// Package conf 演示业务自己的配置块怎么接进框架。
//
// 框架对没人读的顶层 key 是直接启动失败的（多半是拼错了，或者忘了
// import 对应的包），所以配置文件里写了 MyApp，就得有人读它。
// 读只要一行 xconfig.Unmarshal，在 Start 之前任何时候调都行——这个例子在 main 里调。
//
// 单独一个包而不是写在 main 里：别的包也要取配置，放在这里谁都能 import。
package conf

import (
	"fmt"
	"time"

	"github.com/jupiter-eng/xone/xconfig"
)

// Config 业务自己的配置
type Config struct {
	// Topic 订阅哪个主题
	Topic string `yaml:"Topic"`

	// Workers 并发处理消息的协程数。默认 4
	Workers int `yaml:"Workers"`

	// MessageTimeout 单条消息的处理上限。默认 5s。
	// 必须小于服务那一段停止预算（xone.WithStopTimeout 的 2/3，默认 10s），见 Settings.Timeout
	MessageTimeout time.Duration `yaml:"MessageTimeout"`
}

// Validate xconfig.Unmarshal 读完之后会调它，配错的值在启动时就失败。
//
// 这两项配错都不会报错，只会让服务「正常地什么都不做」：Workers 为 0 时
// 一个 worker 都不起，Start 立刻返回；MessageTimeout 不大于 0 时每条消息
// 一进去就超时、全被 Nack。
func (c Config) Validate() error {
	if c.Workers <= 0 {
		return fmt.Errorf("Workers must be > 0, got=%d", c.Workers)
	}
	if c.MessageTimeout <= 0 {
		return fmt.Errorf("MessageTimeout must be > 0, got=%v", c.MessageTimeout)
	}
	return nil
}

// DefaultConfig 默认值预填在结构体里，文件里没写的字段保持不变
func DefaultConfig() Config {
	return Config{
		Topic:          "orders",
		Workers:        4,
		MessageTimeout: 5 * time.Second,
	}
}

// conf 生效中的配置
var conf = DefaultConfig()

// Load 读出 MyApp 这一块。main 里调一次，之后 C() 处处可用
func Load() error { return xconfig.Unmarshal("MyApp", &conf) }

// C 返回最终配置
func C() Config { return conf }
