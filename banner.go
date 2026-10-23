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
// Somebody who does not want it in their terminal either turns it off with XONE_BANNER, see
// bannerEnabled.
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

// printBanner writes the banner to w when it is wanted (bannerWanted decides): the ASCII art,
// and below it the name with the version right-aligned
func printBanner(w io.Writer, want bool) {
	if !want {
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

// bannerWanted reports whether the banner should be written to f: it is wanted, and f is a place
// for it.
//
// The two reasons are joined here rather than at the call site so that each can be asked about on
// its own -- a call site spelling out `a && b` is an expression no test can reach, and the half of
// it that nobody exercises is the half that quietly stops working.
func bannerWanted(f *os.File) bool { return bannerEnabled() && isTerminal(f) }

// bannerEnvKey turns the startup banner off: 0 / false / no / off, in any case.
//
// An environment variable rather than an Option: the one person who sees the banner is the one
// sitting at the terminal, and asking them to edit main.go and rebuild to quieten their own shell
// is the wrong shape. XONE_CONFIG, XONE_PROFILE and XONE_DEBUG are read the same way, so this is a
// knob in a place somebody already looks.
const bannerEnvKey = "XONE_BANNER"

// bannerEnabled reports whether the banner is wanted at all. It is on unless the environment says
// otherwise, which is the other way round from config.Debug (off unless the environment says so),
// because the two have opposite defaults.
//
// Only the spellings above turn it off, and anything else -- including a misspelling -- leaves it
// on. Failing startup over it would be out of proportion for something whose whole effect is six
// lines of ASCII art, and a misspelling announces itself immediately: the banner is still there.
//
// What it does not do is force the banner on. Whether this is a place for ASCII art at all stays
// the framework's judgement (see printBanner): among the JSON of a log collector it is six lines
// nothing can parse, and no environment variable should be able to put it there.
func bannerEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(bannerEnvKey))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
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
