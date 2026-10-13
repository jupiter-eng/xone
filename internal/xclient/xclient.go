// Package xclient 提供「一组按名字组织的实例」这件事本身。
//
// xgorm、xredis、xcache 都是同一个形状：配置里可以写一个实例，也可以按名字写
// 好几个；运行时用 C() 按名字取；启动时挨个建、有一个建不起来就把已建好的全关掉。
// 这些语义本该处处一致——「名字找不到时说什么」「两种写法混用怎么办」
// 都是一次决定，分散在三个模块里就变成了三份会各自漂移的实现。
//
// 集成包这样用：
//
//	var reg = xclient.NewRegistry[*redis.Client]("xredis", ConfigKey)
//
//	func C(name ...string) *redis.Client { return reg.Get(name...) }
//	func Has(name ...string) bool        { return reg.Has(name...) }
//	func Names() []string                { return reg.Names() }
//
//	func initXRedis(ctx context.Context) error {
//		if !xconfig.Has(ConfigKey) {
//			return xclient.Build(ctx, reg, nil, build) // 没配也走一遍：之后取不到时报「没配」而不是「调早了」
//		}
//		c := DefaultConfig()
//		if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
//			return err
//		}
//		return xclient.Build(ctx, reg, c.Clients, build)
//	}
//
//	// build 建一个实例；name 是它在配置里的名字，写进日志好分清是哪一个
//	func build(ctx context.Context, name string, c ClientConfig) (*redis.Client, io.Closer, error) {
//		return New(ctx, c)
//	}
//
//	func closeXRedis(context.Context) error { return reg.Close() }
//
// 启动期的建连探测也是同一件事：试几次、每次多久、认证被拒就别再试。
// Probe 把这三条收成一处（见 probe.go），各模块只提供「怎么探一次」和「怎么认出认证失败」。
//
// 它是 internal 的：这是本仓库三个多实例模块的共用实现，不是使用者要学的东西。
// 你自己写的集成想要多实例，照着抄一份就好——那只是一个按名字存实例的 map，
// 不值得让所有人为它多认一个包。
//
// 本包零第三方依赖。
package xclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/jupiter-eng/xone/xerror"
)

// DefaultName 不带参数取实例时用的名字，也是单实例写法被规整成的名字。
// 只写在这一处：internal/config、xconfig 和各集成模块的同名常量都引用它
const DefaultName = "default"

// Registry 一组按名字组织的实例
type Registry[T any] struct {
	module string // 出现在 panic 文案里，如 "xgorm"
	key    string // 配置块的顶层 key，如 "XGorm"

	// 整份换掉而不是逐个字段改：读的一方拿到的永远是完整的一份，不用加锁。
	// C() 每次数据访问都要走一遍，读锁的计数在多核之间来回争抢：
	// 实测 4 核并发 Get 约 100ns，比单协程的 25ns 还慢；换成原子指针后约 30ns
	state atomic.Pointer[snapshot[T]]
}

// snapshot 注册表某一刻的全部内容，发布之后不再修改
type snapshot[T any] struct {
	items   map[string]T
	closers []io.Closer
	phase   phase
}

// phase 注册表处在生命周期的哪一段。只用来把「取不到」的原因说准：
// 调早了、调晚了、没配、名字写错，是四个不同的问题，要查的地方各不相同
type phase int

const (
	notStarted phase = iota // 启动钩子还没跑
	running                 // 建好了；没配这一块的话就是一个都没建
	closed                  // 停止钩子跑过了
)

// NewRegistry 创建注册表。module 与 key 只用于拼错误信息。
func NewRegistry[T any](module, key string) *Registry[T] {
	r := &Registry[T]{module: module, key: key}
	r.state.Store(&snapshot[T]{items: map[string]T{}})
	return r
}

// Get 取一个实例，不带参数时取名为 default 的那个。取不到直接 panic。
//
// 不返回零值：返回 nil 不会让程序走得更远——客户端上的任何方法在 nil 上
// 都是空指针解引用，只是把同一个 panic 推迟到调用方第一次用它的时候，
// 而那里的栈里只剩 "invalid memory address"，看不出根因是配置没配。
// 这是启动期的配置问题，不是运行期要处理的错误。
//
// 可选依赖（配了就用、没配就跳过）用 Has 先判断。
func (r *Registry[T]) Get(name ...string) T {
	v, ok := r.Lookup(name...)
	if !ok {
		panic(r.missing(nameOf(name)))
	}
	return v
}

// Lookup 取一个实例，取不到时返回零值和 false
func (r *Registry[T]) Lookup(name ...string) (T, bool) {
	v, ok := r.state.Load().items[nameOf(name)]
	return v, ok
}

// Has 报告指定实例是否已配置，供可选依赖判断
func (r *Registry[T]) Has(name ...string) bool {
	_, ok := r.Lookup(name...)
	return ok
}

// Names 返回已配置的实例名，按名字排序
func (r *Registry[T]) Names() []string {
	return slices.Sorted(maps.Keys(r.state.Load().items))
}

// All 返回当前全部实例，名字到实例。
//
// 返回的是一份拷贝：快照发布之后不许改，调用方拿去随便改也碰不到它。
// 同一刻的一份——逐个 Names 再 Lookup 的话，中间赶上 Close 就会取到一半。
func (r *Registry[T]) All() map[string]T {
	return maps.Clone(r.state.Load().items)
}

// missing 只报「没找到」帮助有限：调早了、调晚了、整块没配、名字写错，
// 要查的地方各不相同，文案要让人一眼分清是哪一种。
//
// 调早了最容易被说错：从前它和「整块没配」是同一句话，于是在 main 里、
// 在 Run 之前取实例的人会去翻一份明明写对了的配置文件。
func (r *Registry[T]) missing(want string) string {
	s := r.state.Load()
	p, got := s.phase, slices.Sorted(maps.Keys(s.items))

	switch {
	case p == notStarted:
		return fmt.Sprintf("%s: instance %q was requested before xone.Run started %s — "+
			"use it from your Runnable's Start, a request handler, or a hook at the default stage (StageBusiness) or later", r.module, want, r.module)
	case p == closed:
		return fmt.Sprintf("%s: instance %q was requested after %s was closed — "+
			"nothing should use it once xone.Run has stopped the server", r.module, want, r.module)
	case len(got) == 0:
		return fmt.Sprintf("%s: no instance named %q, and none is configured at all — check the %s block in the config",
			r.module, want, r.key)
	}
	return fmt.Sprintf("%s: no instance named %q, configured ones are [%s]",
		r.module, want, strings.Join(got, " "))
}

func nameOf(name []string) string {
	if len(name) > 0 {
		return name[0]
	}
	return DefaultName
}

// Build 按名字顺序建出全部实例，并把它们发布到注册表。
//
// cfgs 为空（这一块没配）也要调：什么都不建，但注册表由此知道启动钩子跑过了，
// 取不到实例时报「没配」而不是「调早了」。
//
// 任何一个建不起来就把已经建好的全关掉再返回错误：启动钩子返回错误时，
// 这一包的停止钩子不会被执行，不自己收拾就会漏掉那几个连接池。
// 关不掉的那几个的错误并进返回的错误里，漏了哪个连接池看得见。
//
// new 收到实例名：建好时的日志要写是哪一个——配了好几个时只写地址分不出来。
//
// ctx 一路传给 new，并在每个实例之前检查一次：配了五个库、第一个就要
// 重试到超时的话，收到退出信号应当就此打住，而不是把剩下四个也挨个试一遍。
func Build[C, T any](ctx context.Context, r *Registry[T], cfgs map[string]C,
	new func(ctx context.Context, name string, cfg C) (T, io.Closer, error)) error {
	built := make(map[string]T, len(cfgs))
	var closers []io.Closer
	// abort 关掉已经建好的，关不掉的并在 err 后面；都关掉了就原样返回 err，
	// 最外层照旧是本模块的 *xerror.Error
	abort := func(err error) error {
		if cerr := closeAll(r.module, closers); cerr != nil {
			return errors.Join(err, cerr)
		}
		return err
	}

	// 名字排序后再建，让失败顺序可复现，日志顺序也稳定
	for _, name := range slices.Sorted(maps.Keys(cfgs)) {
		if err := ctx.Err(); err != nil {
			return abort(xerror.Newf(r.module, "new", "shutdown signal received before building instance %q: %w", name, err))
		}
		v, closer, err := safeNew(ctx, r.module, name, cfgs[name], new)
		if err != nil {
			return abort(err)
		}
		built[name] = v
		closers = append(closers, closer)
	}

	r.state.Store(&snapshot[T]{items: built, closers: closers, phase: running})
	return nil
}

// Close 摘掉全部实例并逆序关闭它们，供停止钩子直接使用。
//
// 先摘再关：反过来的话，关到一半时 C() 还能取到正在被关闭的实例。
func (r *Registry[T]) Close() error {
	old := r.state.Swap(&snapshot[T]{items: map[string]T{}, phase: closed})
	return closeAll(r.module, old.closers)
}

// safeNew 建一个实例并隔离 panic。
//
// 不隔离的话，panic 会穿过 Build 往上抛，而已经建好的那几个实例的 Closer
// 还只存在于 Build 这一帧的局部变量里——栈一展开就找不回来了，
// 那是几个再也关不掉的连接池。
func safeNew[C, T any](ctx context.Context, module, name string, cfg C,
	new func(context.Context, string, C) (T, io.Closer, error)) (v T, closer io.Closer, err error) {
	defer func() {
		if r := recover(); r != nil {
			var zero T
			v, closer, err = zero, nil, xerror.Newf(module, "new", "instance %q %w", name, panicked(r))
		}
	}()

	v, closer, err = new(ctx, name, cfg)
	if err != nil {
		err = named(module, name, err)
	}
	return v, closer, err
}

// named 给错误点名是哪个实例，并保证一个模块边界只有一层 xerror。
//
// new 返回的通常已经是本模块的 xerror，带着 config / connect 这样的 op。
// 再按 new 包一层，文本就成了
// xone xgorm new failed, err=[instance "a": xone xgorm connect failed, err=[...]]：
// 模块名重复一遍，真正有用的 op 被压进里层，errors.As 取出来的永远是 new。
// 所以本模块的错误沿用它自己的 op、只在消息前补上实例名；其余的才按 new 包。
//
// 只看最外层而不用 errors.As：后者会穿过别人包的那一层，改写时把那层的文字丢掉。
func named(module, name string, err error) error {
	if xe, ok := err.(*xerror.Error); ok && xe.Module == module {
		return xerror.Newf(module, xe.Op, "instance %q: %w", name, xe.Err)
	}
	return xerror.Newf(module, "new", "instance %q: %w", name, err)
}

// safeClose 关一个实例并隔离 panic。
//
// 理由与 safeNew 对称：一个实例的 Close 炸了，不该让同一组里
// 剩下的实例跟着关不掉。
func safeClose(module string, c io.Closer) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = xerror.Newf(module, "close", "close %w", panicked(r))
		}
	}()
	return c.Close()
}

// panicked 把 recover 到的值变成 error，与 xone.go 里的同名函数一致。
//
// panic 出来的本身是 error 时用 %w 接住：用 %v 的话它就只剩一段文本，
// errors.Is / errors.As 到此为止。
func panicked(r any) error {
	if err, ok := r.(error); ok {
		return fmt.Errorf("panicked: %w", err)
	}
	return fmt.Errorf("panicked: %v", r)
}

// closeAll 逆序关闭，一个失败不影响其余
func closeAll(module string, closers []io.Closer) error {
	var errs []error
	for i := len(closers) - 1; i >= 0; i-- {
		if closers[i] == nil {
			continue
		}
		if err := safeClose(module, closers[i]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
