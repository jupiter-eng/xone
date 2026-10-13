// Package hook 是生命周期钩子的存储与执行。
//
// 单独拆出来只为一件事：让 xhook 的 godoc 里只剩使用者真正要认识的东西。
// 这里的类型是框架装配时用的，第三方集成 import 不到（internal 规则按路径生效，
// 只有 github.com/jupiter-eng/xone/... 下的包能进来），也不需要。
//
// 本包零第三方依赖。
package hook

import (
	"cmp"
	"context"
	"slices"
	"sync"
)

// Stage 资源层级：值越小越底层——启动越早、关闭越晚。
//
// 用固定几档而不是任意数字：数字需要全局协调（谁该填 30、谁该填 50），
// 档位不需要——同一档内的东西本来就互不依赖，顺序无所谓。
type Stage int

const (
	// StageLog 日志：最先起、最后关，这样其余组件的启停日志都写得出去
	StageLog Stage = iota

	// StageTelemetry 链路与指标：要在任何客户端之前就绪，
	// 客户端发出的 Span 才挂得上、打的指标才收得到
	StageTelemetry

	// StageClient 数据库、缓存、HTTP 客户端等基础设施：业务要用它们
	StageClient

	// StageBusiness 使用者自己的业务资源：本地缓存预热、定时任务、消息消费者……
	// 它们用得上客户端，又被服务依赖。默认档
	StageBusiness

	// StageServer 对外服务：最后起、最先关
	StageServer
)

// Func 一个钩子。ctx 是进程的启停生命期。
type Func func(ctx context.Context) error

// Entry 一个登记项
type Entry struct {
	Name  string // 出现在日志和错误里，取的是钩子函数自己的名字
	Pkg   string // 登记它的包（调用栈上的 init 所在的包），用来把启动和停止配成对
	Stage Stage
	Run   Func
	Seq   int // 启动钩子的登记序号，从 1 编起
	Pair  int // 停止钩子配对的那个启动钩子的 Seq，0 表示没有，见 AddStop

	// Inherit 停止钩子没有显式指定档位：AddStop 配上对之后改用那个启动钩子的档位
	Inherit bool
}

var (
	mu    sync.Mutex
	start []Entry
	stop  []Entry
)

// AddStart 登记一个启动钩子
func AddStart(e Entry) {
	mu.Lock()
	defer mu.Unlock()
	e.Seq = len(start) + 1
	start = append(start, e)
}

// AddStop 登记一个停止钩子，并在这时就配好对：同一个包里、在它之前最近登记的
// 那个启动钩子。
//
// 一起登记的就是一对：
//
//	BeforeStart(openA); BeforeStop(closeA)
//	BeforeStart(openB); BeforeStop(closeB)
//
// closeA 配 openA，closeB 配 openB。openB 失败时 closeA 照常执行、closeB 不执行——
// 各关各的，谁都不用处理「还没建起来」。之前没有同包的启动钩子时 Pair 为 0：
// 这样的停止钩子不依赖任何启动，总会执行。
//
// Inherit 的停止钩子同时继承配对的那个启动钩子的档位：一对钩子管的是同一个资源，
// 档位本来就该一样。没配上对的保持原来的档位。
func AddStop(e Entry) {
	mu.Lock()
	defer mu.Unlock()
	for i := len(start) - 1; i >= 0; i-- {
		if start[i].Pkg == e.Pkg {
			e.Pair = start[i].Seq
			if e.Inherit {
				e.Stage = start[i].Stage
			}
			break
		}
	}
	stop = append(stop, e)
}

// Start 取出启动钩子，按档位升序；同档内保持登记顺序。
func Start() []Entry {
	mu.Lock()
	defer mu.Unlock()
	return startOrder(start)
}

// startOrder 按启动顺序排：档位升序，同档内保持登记顺序。
func startOrder(in []Entry) []Entry {
	out := slices.Clone(in)
	slices.SortStableFunc(out, func(a, b Entry) int { return cmp.Compare(a.Stage, b.Stage) })
	return out
}

// Stop 取出停止钩子，是 Start 顺序的整体镜像：档位降序，同档内按登记顺序逆序。
//
// 这样一个资源只要声明一个档位，就同时做到了「先启动、后关闭」。
func Stop() []Entry {
	mu.Lock()
	defer mu.Unlock()
	return stopOrder(stop)
}

// stopOrder 按停止顺序排：Start 顺序的整体镜像。
func stopOrder(in []Entry) []Entry {
	out := startOrder(in)
	slices.Reverse(out)
	return out
}

// StopAfter 取出该执行的停止钩子，顺序同 Stop：跳过那些配对的启动钩子没成功的——
// 它们管的资源根本没建起来。started 是成功了的启动钩子的 Seq；
// 没有配对（Pair 为 0）的总会留下。
func StopAfter(started map[int]bool) []Entry {
	mu.Lock()
	defer mu.Unlock()
	var out []Entry
	for _, e := range stopOrder(stop) {
		if e.Pair != 0 && !started[e.Pair] {
			continue // 和它配对的启动钩子没跑成功，资源不存在
		}
		out = append(out, e)
	}
	return out
}

// Reset 清空登记板。只给测试用。
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	start, stop = nil, nil
}
