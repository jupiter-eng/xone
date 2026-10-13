package hook

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func noop(context.Context) error { return nil }

// board 把一组登记项拼成 "名字 名字 ..." 便于断言顺序
func board(es []Entry) string {
	names := make([]string, 0, len(es))
	for _, e := range es {
		names = append(names, e.Name)
	}
	return strings.Join(names, " ")
}

func entry(name string, s Stage) Entry {
	return Entry{Name: name, Pkg: name, Stage: s, Run: noop}
}

func TestStartOrder_AscendingByStage(t *testing.T) {
	got := board(startOrder([]Entry{
		entry("server", StageServer),
		entry("db", StageClient),
		entry("log", StageLog),
		entry("trace", StageTelemetry),
	}))
	if want := "log trace db server"; got != want {
		t.Errorf("启动要从最底层往上走\nwant %s\ngot  %s", want, got)
	}
}

func TestStartOrder_KeepsRegistrationOrderWithinStage(t *testing.T) {
	// 稳定排序是有意的：同一档内谁先登记谁先起，换成不稳定排序之后
	// 顺序会随实现变化，而使用者是照着 import 的先后去理解它的。
	//
	// 输入要够长、而且本来就是乱的：标准库的排序在 12 项以内走插入排序，
	// 输入已经有序时又会直接放过——两种情况下它碰巧都是稳的，
	// 用三五项顺序输入根本测不出 slices.SortFunc 和 slices.SortStableFunc 的差别
	stages := []Stage{StageTelemetry, StageClient, StageServer}
	const per = 8

	var in []Entry
	for i := 0; i < per; i++ { // 按档位轮转着登记，保证输入不是有序的
		for _, s := range stages {
			in = append(in, entry(fmt.Sprintf("s%d-%d", s, i), s))
		}
	}

	var want []string
	for _, s := range stages {
		for i := 0; i < per; i++ {
			want = append(want, fmt.Sprintf("s%d-%d", s, i))
		}
	}

	if got := board(startOrder(in)); got != strings.Join(want, " ") {
		t.Errorf("同档内不该重排\nwant %s\ngot  %s", strings.Join(want, " "), got)
	}
}

func TestStopOrder_MirrorsStartOrder(t *testing.T) {
	// 「声明一个档位就同时做到先启动、后关闭」这条承诺，全靠这里是整体逆序。
	// 只按档位降序而不逆转同档内顺序的话，同档里先起的会先关
	in := []Entry{
		entry("log", StageLog),
		entry("trace", StageTelemetry),
		entry("db", StageClient),
		entry("cache", StageClient),
		entry("server", StageServer),
	}
	if got, want := board(stopOrder(in)), "server cache db trace log"; got != want {
		t.Errorf("关闭要从最上层往下走，同档内后起的先关\nwant %s\ngot  %s", want, got)
	}
}

func TestStopOrder_SymmetricWithStartOrderPerItem(t *testing.T) {
	in := []Entry{
		entry("a", StageClient),
		entry("b", StageLog),
		entry("c", StageServer),
		entry("d", StageClient),
		entry("e", StageTelemetry),
	}
	start, stop := startOrder(in), stopOrder(in)
	if len(start) != len(stop) {
		t.Fatalf("两个顺序的长度必须一致，got %d vs %d", len(start), len(stop))
	}
	for i := range start {
		if j := len(stop) - 1 - i; start[i].Name != stop[j].Name {
			t.Errorf("第 %d 项不对称：启动 %s，对应位置的停止是 %s", i, start[i].Name, stop[j].Name)
		}
	}
}

func TestStartOrder_DoesNotModifyInput(t *testing.T) {
	// 排序如果落在调用方的底层数组上，全局登记板会被每次读取悄悄重排——
	// 之后每一次 Start() 拿到的顺序都不一样了
	in := []Entry{entry("server", StageServer), entry("log", StageLog)}
	startOrder(in)
	stopOrder(in)
	if got, want := board(in), "server log"; got != want {
		t.Errorf("入参被改动了\nwant %s\ngot  %s", want, got)
	}
}

func TestStart_ReturnsCopy(t *testing.T) {
	t.Cleanup(Reset)
	Reset()
	AddStart(entry("a", StageClient))
	AddStart(entry("b", StageClient))

	got := Start()
	got[0], got[1] = got[1], got[0] // 调用方随手重排自己拿到的那份

	if want := "a b"; board(Start()) != want {
		t.Errorf("登记板被调用方改掉了，want %s，got %s", want, board(Start()))
	}
}

func TestAddStart_RetrievedByStageAfterRegister(t *testing.T) {
	t.Cleanup(Reset)
	Reset()
	AddStart(entry("db", StageClient))
	AddStart(entry("log", StageLog))
	AddStop(entry("db", StageClient))
	AddStop(entry("log", StageLog))

	if got, want := board(Start()), "log db"; got != want {
		t.Errorf("Start want %s, got %s", want, got)
	}
	if got, want := board(Stop()), "db log"; got != want {
		t.Errorf("Stop want %s, got %s", want, got)
	}
}

func TestReset_ClearsBothBoards(t *testing.T) {
	t.Cleanup(Reset)
	AddStart(entry("a", StageClient))
	AddStop(entry("a", StageClient))
	Reset()
	if len(Start()) != 0 || len(Stop()) != 0 {
		t.Errorf("Reset 之后两块板都应为空，got start=%d stop=%d", len(Start()), len(Stop()))
	}
}

func TestAddStart_ConcurrentRegisterLosesNothing(t *testing.T) {
	// 集成包的 init() 之间是串行的，但框架不该指望这一点：
	// 使用者完全可以在自己的协程里登记
	t.Cleanup(Reset)
	Reset()

	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			AddStart(entry("x", StageClient))
			AddStop(entry("x", StageClient))
		}()
	}
	wg.Wait()

	if len(Start()) != n || len(Stop()) != n {
		t.Errorf("登记丢项了，want %d，got start=%d stop=%d", n, len(Start()), len(Stop()))
	}
}

func TestAddStop_PairsWithLatestPriorStartHookInSamePackage(t *testing.T) {
	// 一起登记的就是一对：open 配 close，第二组各配各的。
	// 别的包的、登记在它之后的，都不算
	Reset()
	t.Cleanup(Reset)
	AddStart(Entry{Name: "a.openA", Pkg: "a", Run: noop})
	AddStart(Entry{Name: "b.open", Pkg: "b", Run: noop})
	AddStop(Entry{Name: "a.closeA", Pkg: "a", Run: noop})
	AddStart(Entry{Name: "a.openB", Pkg: "a", Run: noop})
	AddStop(Entry{Name: "a.closeB", Pkg: "a", Run: noop})
	AddStop(Entry{Name: "c.flush", Pkg: "c", Run: noop})

	seqOf := map[string]int{}
	for _, s := range Start() {
		if s.Seq == 0 { // 0 留给「没有配对」：编成 0 的那个启动钩子失败了，它的停止钩子照样会被调
			t.Errorf("%s 的登记序号是 0，序号要从 1 编起", s.Name)
		}
		seqOf[s.Name] = s.Seq
	}
	want := map[string]int{"a.closeA": seqOf["a.openA"], "a.closeB": seqOf["a.openB"], "c.flush": 0}
	for _, e := range Stop() {
		if e.Pair != want[e.Name] {
			t.Errorf("%s 该配序号 %d 的启动钩子（0 表示不配），got=%d", e.Name, want[e.Name], e.Pair)
		}
	}
}

func TestStopAfter_SkipsStopHooksWhoseStartHookFailed(t *testing.T) {
	// 配对的启动钩子没成功的跳过，没有配对的总会留下；留下的保持停止顺序
	Reset()
	t.Cleanup(Reset)
	AddStart(Entry{Name: "a.open", Pkg: "a", Stage: StageClient, Run: noop})
	AddStop(Entry{Name: "a.close", Pkg: "a", Stage: StageClient, Run: noop})
	AddStart(Entry{Name: "b.open", Pkg: "b", Stage: StageClient, Run: noop})
	AddStop(Entry{Name: "b.close", Pkg: "b", Stage: StageClient, Run: noop})
	AddStop(Entry{Name: "c.flush", Pkg: "c", Stage: StageLog, Run: noop})
	AddStart(Entry{Name: "d.open", Pkg: "d", Stage: StageServer, Run: noop})
	AddStop(Entry{Name: "d.close", Pkg: "d", Stage: StageServer, Run: noop})

	seqOf := map[string]int{}
	for _, s := range Start() {
		seqOf[s.Name] = s.Seq
	}
	started := map[int]bool{seqOf["a.open"]: true, seqOf["d.open"]: true} // b.open 失败了

	if got, want := board(StopAfter(started)), "d.close a.close c.flush"; got != want {
		t.Errorf("StopAfter want %s, got %s", want, got)
	}
	if got, want := board(StopAfter(nil)), "c.flush"; got != want {
		t.Errorf("一个启动钩子都没成功时只剩不配对的，want %s, got %s", want, got)
	}
}

func TestAddStop_WithoutExplicitStageFollowsPairedStartHook(t *testing.T) {
	// 一对钩子管的是同一个资源：只在启动钩子上声明档位，关闭也该在那一档
	Reset()
	t.Cleanup(Reset)
	AddStart(Entry{Name: "a.open", Pkg: "a", Stage: StageClient, Run: noop})
	AddStop(Entry{Name: "a.close", Pkg: "a", Stage: StageBusiness, Inherit: true, Run: noop})
	AddStop(Entry{Name: "a.flush", Pkg: "a", Stage: StageServer, Run: noop})
	AddStop(Entry{Name: "c.flush", Pkg: "c", Stage: StageBusiness, Inherit: true, Run: noop})

	want := map[string]Stage{
		"a.close": StageClient,   // 继承
		"a.flush": StageServer,   // 显式指定的不动
		"c.flush": StageBusiness, // 没配上对，保持原样
	}
	for _, e := range Stop() {
		if e.Stage != want[e.Name] {
			t.Errorf("%s 的档位该是 %v，got=%v", e.Name, want[e.Name], e.Stage)
		}
	}
}

func TestStage_BusinessIsAfterClientBeforeServer(t *testing.T) {
	// 五档的相对次序是对外承诺的一部分，靠 iota 的书写顺序保证。
	// 业务钩子里直接用 xgorm.C()，靠的就是 StageClient < StageBusiness
	if !(StageLog < StageTelemetry && StageTelemetry < StageClient &&
		StageClient < StageBusiness && StageBusiness < StageServer) {
		t.Errorf("档位次序不对：log=%d telemetry=%d client=%d business=%d server=%d",
			StageLog, StageTelemetry, StageClient, StageBusiness, StageServer)
	}
}
