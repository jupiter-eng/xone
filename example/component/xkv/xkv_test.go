package xkv

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/xonetest"
)

// 这个文件也是样例的一部分：纯构造器 New 的回报就是测试里不需要任何 mock，
// 也不需要把框架拉起来——直接造一个干净实例，用完关掉。

func TestStore_ReadsBackWhatWasWritten(t *testing.T) {
	s, closer, err := New(Config{Path: filepath.Join(t.TempDir(), "kv.json"), FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closer.Close() })

	s.Set("k", "v")
	if got, ok := s.Get("k"); !ok || got != "v" {
		t.Fatalf("写进去的该读得回来，got=%q ok=%v", got, ok)
	}
	if s.Len() != 1 {
		t.Errorf("该只有一个键，got=%d", s.Len())
	}
}

func TestStore_FlushesOnCloseAndReadsBackNextTime(t *testing.T) {
	// 后台刷盘间隔调得很长，确保这次落盘只可能来自 Close
	path := filepath.Join(t.TempDir(), "kv.json")
	c := Config{Path: path, FlushInterval: time.Hour}

	s, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	s.Set("k", "v")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}

	again, closer2, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closer2.Close() })
	if got, _ := again.Get("k"); got != "v" {
		t.Fatalf("关闭时该把改动刷下去，重开应当读得到，got=%q", got)
	}
}

func TestStore_FailsToBuildOnCorruptDataFile(t *testing.T) {
	// 建不起来就该报错，让启动当场失败——而不是静默地从一个空 store 开始，
	// 那会让线上看起来一切正常，只是数据没了
	path := filepath.Join(t.TempDir(), "kv.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := New(Config{Path: path, FlushInterval: time.Hour}); err == nil {
		t.Fatal("数据文件坏掉时应当报错")
	}
}

func TestRegister_RegistersStartAndStopHooks(t *testing.T) {
	// 这是本包和框架之间唯一的一根线：钩子漏登记的话，表现是「C() 取不到」
	// 或者「退出时数据没刷下去」，别处都测不出来。所以按框架的方式跑一遍钩子，
	// 而不是直接调 initXKV / closeXKV
	path := filepath.Join(t.TempDir(), "kv.json")
	t.Run("起停一轮", func(t *testing.T) {
		xonetest.UseConfigYAML(t, "XKV:\n  Path: \""+path+"\"\n  FlushInterval: 1h\n")
		xonetest.StartHooks(t) // 子测试结束时跑停止钩子
		if C() == nil {
			t.Fatal("启动钩子没登记，C() 取不到")
		}
		C().Set("k", "v")
	})

	// 刷盘间隔是 1h，读得回来只能是停止钩子关闭时刷下去的
	s, closer, err := New(Config{Path: path, FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	if got, _ := s.Get("k"); got != "v" {
		t.Errorf("停止钩子没登记，退出时数据没刷下去，got=%q", got)
	}
}

func TestInitXKV_WalksTheRealFrameworkPath(t *testing.T) {
	// 这是本例子存在的意义：读配置 → 建实例 → 存起来 → 退出时关掉。
	// 使用者照抄的就是这几行，它们必须真的串得起来
	dir := t.TempDir()
	path := filepath.Join(dir, "kv.json")
	xonetest.UseConfigYAML(t, "XKV:\n  Path: \""+path+"\"\n  FlushInterval: 50ms\n")

	if err := initXKV(context.Background()); err != nil {
		t.Fatalf("初始化失败：%v", err)
	}
	if C() == nil {
		t.Fatal("实例没存起来，C() 取不到")
	}
	if Conf().Path != path {
		t.Errorf("配置没读到，got=%+v", Conf())
	}

	C().Set("k", "v")
	if err := closeXKV(context.Background()); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}

	// 关闭时把最后一次改动刷下去了，所以重新建一个能读回来
	s, closer, err := New(Conf())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	if got, _ := s.Get("k"); got != "v" {
		t.Errorf("退出前没落盘，got=%q", got)
	}
}

func TestInitXKV_BadConfigFailsStartup(t *testing.T) {
	xonetest.UseConfigYAML(t, "XKV:\n  Paht: /tmp/kv.json\n")
	if err := initXKV(context.Background()); err == nil {
		t.Fatal("字段拼错应当让启动失败")
	}
}

func TestNew_FailsWhenFlushIntervalNotPositive(t *testing.T) {
	// 放过去的话 time.NewTicker 在后台协程里 panic，进程直接死掉
	for _, every := range []time.Duration{0, -time.Second} {
		if _, _, err := New(Config{Path: filepath.Join(t.TempDir(), "kv.json"), FlushInterval: every}); err == nil {
			t.Errorf("FlushInterval=%v 应当建不起来", every)
		}
	}
}

func TestInitXKV_ZeroFlushIntervalFailsStartup(t *testing.T) {
	xonetest.UseConfigYAML(t, "XKV:\n  FlushInterval: 0s\n")
	if err := initXKV(context.Background()); err == nil {
		t.Fatal("FlushInterval 为 0 应当让启动失败")
	}
}

func TestStore_FailedFlushLosesNoData(t *testing.T) {
	// 从前刷盘先清掉脏标记再写，写失败了也不还回去：这批改动再也没人刷，
	// 连 Close 时最后那一次也当成「没有改动」跳过
	dir := filepath.Join(t.TempDir(), "还不存在的目录")
	path := filepath.Join(dir, "kv.json")
	s, closer, err := New(Config{Path: path, FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	s.Set("k", "v")
	if err := s.flush(); err == nil {
		t.Fatal("目录不存在，这次刷盘应当失败")
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("目录建好之后 Close 应当刷得下去：%v", err)
	}

	again, closer2, err := New(Config{Path: path, FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer2.Close() })
	if got, _ := again.Get("k"); got != "v" {
		t.Errorf("刷盘失败过一次的改动丢了，got=%q", got)
	}
}
