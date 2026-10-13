// Package xhook 是集成接入框架的两个动词：BeforeStart 和 BeforeStop。
// 写一个集成只需要认识本包和 xconfig（读配置）。
//
//	func init() {
//		xhook.BeforeStart(initXMine, xhook.At(xhook.StageClient))
//		xhook.BeforeStop(closeXMine) // 档位跟着上面那个启动钩子
//	}
//
//	func initXMine(ctx context.Context) error {
//		c := DefaultConfig()
//		if err := xconfig.Unmarshal(ConfigKey, &c); err != nil {
//			return err
//		}
//		client, closer, err := New(ctx, c)
//		if err != nil {
//			return err
//		}
//		setDefault(client, closer) // 存到哪、怎么取，是你自己的事
//		return nil
//	}
//
//	func closeXMine(context.Context) error { return currentCloser().Close() }
//
// 里面写普通的 Go 代码就行：读配置、建实例、存起来。框架只负责在对的时候
// 调它们，以及按相反的顺序调停止钩子。
//
// 档位（At）说的是「必须早于或晚于谁」。你自己的业务代码不写——默认的
// StageBusiness 排在全部客户端之后、服务之前，钩子里直接用 xgorm.C() 就行。
// 写的是数据库、缓存这类被业务依赖的客户端时，才像上面这样写 StageClient；
// 停止钩子不写档位，跟着和它配对的那个启动钩子。
//
// 本包零第三方依赖，也不 import 框架本体：集成只认识这里和 xconfig，
// 所以你把一个集成单独发成 module 完全成立。
package xhook

import (
	"context"
	"reflect"
	"runtime"
	"strings"

	"github.com/jupiter-eng/xone/internal/hook"
)

// Stage 资源层级：值越小越底层——启动越早、关闭越晚。
type Stage = hook.Stage

const (
	// StageLog 日志：最先起、最后关，这样其余组件的启停日志都写得出去
	StageLog = hook.StageLog

	// StageTelemetry 链路与指标：要在任何客户端之前就绪，
	// 客户端发出的 Span 才挂得上、打的指标才收得到
	StageTelemetry = hook.StageTelemetry

	// StageClient 数据库、缓存、HTTP 客户端等基础设施：业务要用它们。
	// 框架自带的 xgorm、xredis、xhttp、xcache 都在这一档
	StageClient = hook.StageClient

	// StageBusiness 你自己的业务资源：本地缓存预热、定时任务、消息消费者……
	// 它们用得上客户端（钩子里直接 xgorm.C()），又被服务依赖。不指定时就是它
	StageBusiness = hook.StageBusiness

	// StageServer 对外服务：最后起、最先关
	StageServer = hook.StageServer
)

// HookFunc 一个钩子。
//
// ctx 是进程的启停生命期：启动钩子收到的那个在退出信号到达时被取消，
// 停止钩子收到的那个带着它那一份停止预算。会阻塞的操作（建连、重试、探测）
// 必须把它传下去——否则启动到一半收到 SIGTERM 时，进程只能卡在那里
// 等它自己跑完。
type HookFunc func(ctx context.Context) error

// Option 钩子的可选设置
type Option func(*options)

type options struct {
	stage Stage
	set   bool // 显式写了 At
}

// At 指定档位。启动钩子不写就是 StageBusiness；停止钩子不写就跟着和它
// 配对的那个启动钩子（见 BeforeStop），没有配对的才是 StageBusiness。
//
//	xhook.BeforeStart(initXLog, xhook.At(xhook.StageLog))
//
// 只有「必须早于或晚于别人」的东西才需要它：日志要最先起最后关，链路和指标
// 要早于客户端，客户端要早于业务。业务代码不写，默认那一档就对。
func At(s Stage) Option { return func(o *options) { o.stage, o.set = s, true } }

// BeforeStart 登记一个启动钩子，在配置加载完之后、服务起来之前执行。
//
// 按档位升序执行。任何一个返回错误（或 panic），启动就此失败：后面的启动钩子
// 不再执行，服务不会起来；已经成功的那些，和它们配对的停止钩子逆序执行来收拾现场
// （共用 WithStopTimeout 那一份预算），然后 Run 返回这个错误。
//
// 同档内按登记顺序，也就是 Go 初始化各个包的顺序：同一份代码每次都一样，
// 但它由包之间的 import 关系和 import path 的字典序决定，不是 import 语句的书写顺序。
// 所以同档内的东西不该互相依赖——有先后要求的，放进不同的档位。
func BeforeStart(f HookFunc, opts ...Option) {
	hook.AddStart(entry(f, opts))
}

// BeforeStop 登记一个停止钩子，在服务停下来之后执行。
//
// 执行顺序是 BeforeStart 的整体镜像：档位降序，同档内按登记顺序逆序。
//
// 它和同一个包里在它之前最近登记的那个启动钩子是一对：那个启动钩子成功了才执行，
// 所以停止钩子里不必处理「还没建起来」。之前没有启动钩子的话，它总会执行。
//
// 不写 At 时档位跟着那个启动钩子：只在启动钩子上声明一次档位，就同时做到了
// 「先启动、后关闭」。显式写了 At 的以它为准。
//
// 一个返回错误不会打断其余的——退出阶段要尽量把能关的都关掉。
func BeforeStop(f HookFunc, opts ...Option) {
	hook.AddStop(entry(f, opts))
}

func entry(f HookFunc, opts []Option) hook.Entry {
	o := options{stage: StageBusiness}
	for _, opt := range opts {
		opt(&o)
	}
	full := fullName(f)
	return hook.Entry{Name: shortName(full), Pkg: registrant(full), Stage: o.stage, Run: hook.Func(f), Inherit: !o.set}
}

// registrant 认出是哪个包在登记：调用栈上最近的那个包初始化函数
// （pkg.init、pkg.init.0 ……）所在的包。框架用它把启动和停止配成对（见 BeforeStop）。
//
// 只看钩子函数自己的名字会认错：经一个辅助包登记的钩子（辅助包里造的闭包、
// 辅助包类型的方法值），名字属于辅助包，于是所有经它登记的包被当成了同一个——
// 一个的启动钩子失败，别人的停止钩子跟着被跳过。
// 不在 init 里登记（比如测试里直接调）时栈上没有这一帧，退回钩子函数的包。
func registrant(full string) string {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if pkg, ok := initPkg(f.Function); ok {
			return pkg
		}
		if !more {
			return pkgOf(full)
		}
	}
}

// initPkg 认出包初始化函数：github.com/you/app/xredis.init、….init.0，返回包路径。
//
// init 里的闭包（….init.0.func1）不算：调用栈再往外一层就是它所在的那个 init。
func initPkg(fn string) (string, bool) {
	pkg := pkgOf(fn)
	rest, ok := strings.CutPrefix(fn, pkg+".")
	if !ok {
		return "", false
	}
	if rest == "init" {
		return pkg, true
	}
	n, ok := strings.CutPrefix(rest, "init.")
	return pkg, ok && n != "" && strings.Trim(n, "0123456789") == ""
}

// pkgOf 从 "github.com/you/app/xredis.initXRedis" 取出 "github.com/you/app/xredis"。
//
// 取的是完整 import path 而不是末段包名：末段会撞。使用者自己包一层叫
// xlog 的包是很常见的事，撞上之后两个包被当成同一个——它的启动钩子失败，
// 框架 xlog 的停止钩子就跟着被跳过，日志写入器再也不 flush。
// 这个键从不出现在任何输出里，长一点没有代价。
func pkgOf(full string) string {
	// 包路径里可以有点（github.com），函数名里也可以（方法值的 Comp.Init-fm），
	// 所以分界点是最后一个 "/" 之后的第一个 "."
	slash := strings.LastIndex(full, "/")
	if i := strings.Index(full[slash+1:], "."); i >= 0 {
		return full[:slash+1+i]
	}
	return full
}

// fullName 取钩子函数的完整名字，形如 github.com/jupiter-eng/xone/xredis.initXRedis。
//
// 让使用者再传一个名字字符串是纯粹的重复——那个名字就写在他刚传进来的
// 函数上。取不到（传了 nil）时退回一个占位符，总比空字符串强。
func fullName(f HookFunc) string {
	fn := runtime.FuncForPC(reflect.ValueOf(f).Pointer())
	if fn == nil {
		return "hook"
	}
	return fn.Name()
}

// shortName 去掉路径前缀，留下 "xredis.initXRedis"，用于日志和错误信息。
//
// 泛型实例化后的名字形如 xredis.newFor[...]，类型实参被 runtime 折叠成
// "..."，所以括号里不会再有 "/" 来干扰这次切分。
func shortName(full string) string {
	if i := strings.LastIndex(full, "/"); i >= 0 {
		return full[i+1:]
	}
	return full
}
