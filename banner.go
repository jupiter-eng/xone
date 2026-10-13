package xone

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"unicode/utf8"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/internal/hook"
)

// bannerText 启动 banner（ANSI Shadow 字体）。
//
// 只在 stderr 是终端时打：在容器里、被重定向到文件或者接在日志采集器后面时，
// 一段带颜色的多行字符画就是日志平台里几行解析失败的垃圾——框架写出的每一行都该是 JSON。
var bannerText = []string{
	`██╗  ██╗ ██████╗ ███╗   ██╗███████╗`,
	`╚██╗██╔╝██╔═══██╗████╗  ██║██╔════╝`,
	` ╚███╔╝ ██║   ██║██╔██╗ ██║█████╗  `,
	` ██╔██╗ ██║   ██║██║╚██╗██║██╔══╝  `,
	`██╔╝ ██╗╚██████╔╝██║ ╚████║███████╗`,
	`╚═╝  ╚═╝ ╚═════╝ ╚═╝  ╚═══╝╚══════╝`,
}

// 从左到右的渐变（24 位 ANSI）：青蓝到淡紫。底下那行的名字用两端的中间色
var (
	bannerFrom = [3]int{80, 190, 230}
	bannerTo   = [3]int{176, 132, 236}
)

// stderr banner 写到哪里。测试换成管道，看 Run 是不是只在终端里才打
var stderr = os.Stderr

// printBanner stderr 是终端时往 w 写 banner：字符画，下面一行名字和右对齐的版本号
func printBanner(w io.Writer, terminal bool) {
	if !terminal {
		return
	}
	width := utf8.RuneCountInString(bannerText[0])
	var b strings.Builder
	b.WriteString("\n")
	for _, l := range bannerText {
		for i, r := range []rune(l) {
			if r == ' ' {
				b.WriteRune(r)
				continue
			}
			c := blend(float64(i) / float64(width-1))
			if r != '█' { // 描边的线条压暗，字才立得起来
				c = [3]int{c[0] * 11 / 20, c[1] * 11 / 20, c[2] * 11 / 20}
			}
			b.WriteString(fg(c) + string(r))
		}
		b.WriteString("\x1b[0m\n")
	}
	ver := version()
	if !strings.HasPrefix(ver, "(") { // (devel) 自带括号
		ver = "(" + ver + ")"
	}
	pad := strings.Repeat(" ", max(1, width-len(" :: xone ::")-len(ver)))
	fmt.Fprintf(&b, "\x1b[2m :: \x1b[0m\x1b[1m%sxone\x1b[0m\x1b[2m ::%s%s\x1b[0m\n\n", fg(blend(0.5)), pad, ver)
	fmt.Fprint(w, b.String())
}

// blend 渐变上 t（0 到 1）处的颜色
func blend(t float64) [3]int {
	var c [3]int
	for i := range c {
		c[i] = bannerFrom[i] + int(float64(bannerTo[i]-bannerFrom[i])*t)
	}
	return c
}

func fg(c [3]int) string { return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c[0], c[1], c[2]) }

// isTerminal f 是不是终端（字符设备）。不引第三方库：核心只依赖 yaml
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// modulePath 核心模块的路径，从二进制的构建信息里找它的版本
const modulePath = "github.com/jupiter-eng/xone"

// version 编进这个二进制的 xone 版本：取自构建信息，发布时不用改任何常量。
// 用 replace 指向本地目录时构建信息里没有真实版本，显示 (devel)
func version() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "(devel)"
	}
	return moduleVersion(bi)
}

func moduleVersion(bi *debug.BuildInfo) string {
	if bi.Main.Path == modulePath {
		return orDevel(bi.Main.Version)
	}
	for _, d := range bi.Deps {
		if d.Path == modulePath {
			if d.Replace != nil {
				return "(devel)"
			}
			return orDevel(d.Version)
		}
	}
	return "(devel)"
}

func orDevel(v string) string {
	if v == "" {
		return "(devel)"
	}
	return v
}

// stageNames 调试输出里档位的名字
var stageNames = map[hook.Stage]string{
	hook.StageLog:       "Log",
	hook.StageTelemetry: "Telemetry",
	hook.StageClient:    "Client",
	hook.StageBusiness:  "Business",
	hook.StageServer:    "Server",
}

// debugHooks XONE_DEBUG 开着时列出启动钩子的执行顺序；停止钩子按它倒过来
func debugHooks(entries []hook.Entry) {
	if !config.Debug() {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[xone debug] xone %s, %s\n", version(), runtime.Version())
	fmt.Fprintf(&b, "[xone debug] start hooks, in order (stop hooks run in reverse):\n")
	for i, e := range entries {
		fmt.Fprintf(&b, "  %d. %-9s %s\n", i+1, stageNames[e.Stage], e.Name)
	}
	fmt.Fprint(config.DebugOut, b.String())
}
