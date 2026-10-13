// Package xkv 是一个「自己写的集成」的完整样例：一个落盘的键值存储。
//
// 它不是框架的一部分，就是你在自己项目里会写的那种包。整个接入只有三样东西，
// 都在本文件末尾的「接入框架」一节：
//
//	BeforeStart  读配置文件里的 XKV 那一块，建好实例存起来
//	BeforeStop   退出时关掉它
//	C()          运行期取实例
//
// 除此之外这个包只 import xhook 和 xconfig —— 集成不认识框架本体，
// 所以你把它单独发成一个 module 也完全成立。
package xkv

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xhook"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XKV"

// Config 本模块的配置。字段上的 yaml tag 决定配置文件里怎么写
type Config struct {
	// Path 数据文件路径
	Path string `yaml:"Path"`

	// FlushInterval 多久把内存里的改动刷一次盘。默认 5s，必须大于 0
	FlushInterval time.Duration `yaml:"FlushInterval"`
}

// Validate xconfig.Unmarshal 读完之后会调它，New 也会调一次。
//
// FlushInterval 不大于 0 时 time.NewTicker 会 panic，而且是在后台协程里——
// 进程直接死掉，连一条说明是哪项配错了的错误都没有。
func (c Config) Validate() error {
	if c.FlushInterval <= 0 {
		return fmt.Errorf("FlushInterval must be > 0, got=%v", c.FlushInterval)
	}
	return nil
}

// DefaultConfig 全部默认值集中在这里。
//
// 配置文件里没写的字段保持这里的值——所以不需要指针字段来区分
// 「没配」和「配成零值」。
func DefaultConfig() Config {
	return Config{Path: "xkv.json", FlushInterval: 5 * time.Second}
}

// Store 一个落盘的键值存储
type Store struct {
	path string

	mu    sync.RWMutex
	items map[string]string
	dirty bool

	stop chan struct{}
	done chan struct{}
}

// Get 读一个键
func (s *Store) Get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.items[key]
	return v, ok
}

// Set 写一个键。落盘由后台协程按 FlushInterval 批量做
func (s *Store) Set(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[key] = value
	s.dirty = true
}

// Len 返回当前键的数量
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.items)
}

// New 纯构造器：不碰任何全局变量，不读配置文件，也不依赖框架。
//
// 这条是框架对集成的硬性要求（check.sh 会查），为的是「零装配」永远只是
// 默认路径而不是唯一路径：测试直接调它拿一个干净实例，不需要 mock；
// 需要两套配置时也有出路。
//
// 它不收 ctx——开个文件不会把人卡住。会建连、会重试、会探测的构造器才收 ctx，
// 签名如实说明这件事。
func New(c Config) (*Store, io.Closer, error) {
	// 不经过配置文件、直接调 New 的也要拦住：Unmarshal 那次校验管不到这里
	if err := c.Validate(); err != nil {
		return nil, nil, err
	}
	items := map[string]string{}
	switch b, err := os.ReadFile(c.Path); {
	case err == nil:
		if err := json.Unmarshal(b, &items); err != nil {
			return nil, nil, err
		}
	case !os.IsNotExist(err):
		return nil, nil, err
	}

	s := &Store{
		path:  c.Path,
		items: items,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go s.flushLoop(c.FlushInterval)
	return s, s, nil
}

// Close 停掉后台协程并把最后一次改动刷下去。
//
// 退出时由本包的停止钩子 closeXKV 调它；停止钩子按启动的逆序执行，
// 所以这里不需要考虑「数据库是不是已经关了」之类的问题，本模块只管自己。
func (s *Store) Close() error {
	close(s.stop)
	<-s.done
	return s.flush()
}

func (s *Store) flushLoop(every time.Duration) {
	defer close(s.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			if err := s.flush(); err != nil {
				slog.Error("xkv flush failed", "error", err)
			}
		}
	}
}

func (s *Store) flush() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	b, err := json.Marshal(s.items)
	s.dirty = false
	s.mu.Unlock()
	if err == nil {
		err = s.write(b)
	}
	if err != nil {
		// 没写成就把脏标记还回去：否则这批改动再也没人刷，
		// 连 Close 时最后那一次也会当成「没有改动」跳过，数据就此丢掉
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
	}
	return err
}

// write 先写临时文件再改名：直接覆写的话，进程在写一半时被杀掉就剩一个坏文件
func (s *Store) write(b []byte) error {
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Clean(s.path))
}

// ---- 接入框架：全部代码就是下面这些 ----

// conf 生效中的配置。它和 store 都是普通的包级变量，怎么存、怎么取由本包自己决定
var (
	conf  = DefaultConfig()
	store *Store
)

// init 只登记，不初始化。真正的初始化由框架在 StageClient 执行——
// 这一档是「被业务依赖的东西」。不写档位的话落在默认的 StageBusiness，
// 和用它的业务钩子同档，谁先谁后就看包的初始化顺序了。
func init() {
	xhook.BeforeStart(initXKV, xhook.At(xhook.StageClient))
	xhook.BeforeStop(closeXKV) // 档位跟着上面那个启动钩子
}

// initXKV 读配置、建实例、存起来。里面是普通的 Go 代码。
func initXKV(context.Context) error {
	conf = DefaultConfig()
	if err := xconfig.Unmarshal(ConfigKey, &conf); err != nil {
		return err
	}

	s, _, err := New(conf)
	if err != nil {
		return err
	}
	store = s
	return nil
}

// closeXKV 收工：停后台协程、把最后一次改动刷下去。
//
// 启动钩子失败时它不会被调到——停止钩子只在和它配对的启动钩子成功之后才执行，
// 所以这里不必处理「store 还是 nil」。
func closeXKV(context.Context) error { return store.Close() }

// C 取实例
func C() *Store { return store }

// Conf 返回生效中的配置。运行期要读配置才需要这个，不需要就别导出
func Conf() Config { return conf }
