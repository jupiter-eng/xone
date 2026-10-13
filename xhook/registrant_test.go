package xhook_test

import (
	"strings"
	"testing"

	"github.com/jupiter-eng/xone/internal/hook"
	"github.com/jupiter-eng/xone/xhook/internal/viahelper"
)

// viaHelper 在本测试包初始化时经辅助包登记一次，当场取下登记板的快照。
//
// 包级变量的初始化同样跑在 init 里，这正是集成包登记钩子的时机。
// 要当场取：等测试函数开始时，别的用例早把登记板清空过了
var viaHelper = func() []hook.Entry {
	viahelper.Register()
	return hook.Start()
}()

func TestBeforeStart_HookViaHelperPkgAttributedToRegisteringPkg(t *testing.T) {
	// 钩子函数是辅助包里的闭包，名字属于辅助包。按名字认包的话，所有经它登记的
	// 包都成了同一个：一个的启动钩子失败，别人的停止钩子跟着被跳过
	const want = "github.com/jupiter-eng/xone/xhook_test"
	var found bool
	for _, e := range viaHelper {
		if !strings.Contains(e.Name, "viahelper") {
			continue
		}
		found = true
		if e.Pkg != want {
			t.Errorf("该算在登记它的包 %s 头上，got=%s", want, e.Pkg)
		}
	}
	if !found {
		t.Fatalf("没找到经辅助包登记的那个钩子，登记板=%v", viaHelper)
	}
}
