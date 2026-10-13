// Package conf 是「业务自己的配置块」的样例。
//
// 它没有任何东西要初始化，所以整个包只做一件事：把 MyApp 那一块读进一个
// 包级变量。读只要一行 xconfig.Unmarshal，在 Start 之前任何时候调都行——这个例子在 main 里调。
//
// 单独一个包而不是写在 main 里：别的包也要取配置，放在这里谁都能 import。
package conf

import "github.com/jupiter-eng/xone/xconfig"

// Config 业务自己的配置
type Config struct {
	// Greeting 每轮打印的问候语
	Greeting string `yaml:"Greeting"`

	// Rounds 跑几轮就收工。默认 3
	Rounds int `yaml:"Rounds"`
}

// DefaultConfig 默认值预填在结构体里，文件里没写的字段保持不变
func DefaultConfig() Config {
	return Config{Greeting: "hello", Rounds: 3}
}

// conf 生效中的配置
var conf = DefaultConfig()

// Load 读出 MyApp 这一块。main 里调一次，之后 C() 处处可用
func Load() error { return xconfig.Unmarshal("MyApp", &conf) }

// C 返回最终配置
func C() Config { return conf }
