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

// bannerText is the startup banner (the ANSI Shadow font).
//
// It is printed only while stderr is a terminal: in a container, redirected into a file or piped
// into a log collector, a coloured multi-line piece of ASCII art is a few lines of unparsable
// garbage in the logging platform -- every line the framework writes should be JSON.
var bannerText = []string{
	`██╗  ██╗ ██████╗ ███╗   ██╗███████╗`,
	`╚██╗██╔╝██╔═══██╗████╗  ██║██╔════╝`,
	` ╚███╔╝ ██║   ██║██╔██╗ ██║█████╗  `,
	` ██╔██╗ ██║   ██║██║╚██╗██║██╔══╝  `,
	`██╔╝ ██╗╚██████╔╝██║ ╚████║███████╗`,
	`╚═╝  ╚═╝ ╚═════╝ ╚═╝  ╚═══╝╚══════╝`,
}

// The gradient from left to right (24-bit ANSI): cyan-blue to light purple. The name on the line
// below takes the colour halfway between the two ends
var (
	bannerFrom = [3]int{80, 190, 230}
	bannerTo   = [3]int{176, 132, 236}
)

// stderr is where the banner is written. The tests swap in a pipe to see whether Run really prints
// it only in a terminal
var stderr = os.Stderr

// printBanner writes the banner to w while stderr is a terminal: the ASCII art, and below it the
// name with the version right-aligned
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
			if r != '█' { // the outline strokes are darkened so that the letters stand out
				c = [3]int{c[0] * 11 / 20, c[1] * 11 / 20, c[2] * 11 / 20}
			}
			b.WriteString(fg(c) + string(r))
		}
		b.WriteString("\x1b[0m\n")
	}
	ver := version()
	if !strings.HasPrefix(ver, "(") { // (devel) brings its own brackets
		ver = "(" + ver + ")"
	}
	pad := strings.Repeat(" ", max(1, width-len(" :: xone ::")-len(ver)))
	fmt.Fprintf(&b, "\x1b[2m :: \x1b[0m\x1b[1m%sxone\x1b[0m\x1b[2m ::%s%s\x1b[0m\n\n", fg(blend(0.5)), pad, ver)
	fmt.Fprint(w, b.String())
}

// blend is the colour at t (0 to 1) along the gradient
func blend(t float64) [3]int {
	var c [3]int
	for i := range c {
		c[i] = bannerFrom[i] + int(float64(bannerTo[i]-bannerFrom[i])*t)
	}
	return c
}

func fg(c [3]int) string { return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c[0], c[1], c[2]) }

// isTerminal reports whether f is a terminal (a character device). No third-party library is
// pulled in for it: the core depends on yaml alone
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// modulePath is the core module's path, used to find its version in the binary's build info
const modulePath = "github.com/jupiter-eng/xone"

// version is the xone version compiled into this binary: taken from the build info, so a release
// changes no constant. With a replace pointing at a local directory the build info holds no real
// version and (devel) is shown
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

// stageNames are the stage names in the debug output
var stageNames = map[hook.Stage]string{
	hook.StageLog:       "Log",
	hook.StageTelemetry: "Telemetry",
	hook.StageClient:    "Client",
	hook.StageBusiness:  "Business",
	hook.StageServer:    "Server",
}

// debugHooks lists the start hooks in the order they run while XONE_DEBUG is on; the stop hooks
// run in reverse
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
