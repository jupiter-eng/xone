package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// 这个文件是 shutdown_test.go 用的辅助：从输出里取钩子的起停顺序、带时间线的持续流量、
// 信号之后不断开新连接的探测器。

// stopHookPkgs 登记了停止钩子的包（量自 e2e 服务一次正常退出的 stopping 日志）。
// 启动期间被打断的用例据此推算「该关哪几个」；框架给别的包加了停止钩子时，
// 关闭顺序那个用例会先报出来，到时候把它补进这里
var stopHookPkgs = []string{"xlog", "xtrace", "xcache", "xgorm", "xredis", "xhttp", "xkafka", "xcron"}

// stopPairs 一个包登记了不止一对钩子时，停止钩子配的是哪个启动钩子（docs/guide.md「停止钩子：配对与继承」：
// 同一个包里、在它之前最近登记的那个）。xkafka 一对管生产客户端（StageClient），一对管消费者（StageServer）。
// 只有一对的包按包名配
var stopPairs = map[string]string{
	"xkafka.closeXKafka":   "xkafka.initXKafka",
	"xkafka.stopConsumers": "xkafka.startConsumers",
}

// startIndex 停止钩子 stop 配对的启动钩子在 starts 里的位置，没启动过时 ok 为 false
func startIndex(starts []string, stop string) (int, bool) {
	for i := len(starts) - 1; i >= 0; i-- {
		if want, ok := stopPairs[stop]; ok && starts[i] == want || !ok && pkgOf(starts[i]) == pkgOf(stop) {
			return i, true
		}
	}
	return 0, false
}

// textHook xlog 装好之前，框架日志是 slog 默认格式写 stderr 的文本：
//
//	2026/09/24 01:59:17 INFO starting hook=xapp.loadConfig
var textHook = regexp.MustCompile(`\bINFO (starting|stopping) hook=(\S+)`)

// hookSeq 按发生顺序取出 msg（starting / stopping）日志里的钩子名。
//
// 不按两个管道的到达顺序排：stdout 和 stderr 各由一个协程读，谁先到不保证。
// 按逻辑顺序拼：stderr 里的文本行一定最早（xlog 就是在它们之后装上的），
// 然后是 stdout 里的 JSON（xlog 生效期间），最后是 stderr 里的 JSON（xlog 关掉之后）
func hookSeq(p *harness.Process, msg string) []string {
	var out []string
	for _, line := range strings.Split(p.Stderr(), "\n") {
		if m := textHook.FindStringSubmatch(line); m != nil && m[1] == msg {
			out = append(out, m[2])
		}
	}
	logs := p.Logs()
	for _, stream := range []string{"stdout", "stderr"} {
		for _, l := range logs {
			if l.Stream == stream && l.Msg() == msg {
				out = append(out, l.Str("hook"))
			}
		}
	}
	return out
}

// pkgOf 钩子名的包：xgorm.initXGorm → xgorm
func pkgOf(hook string) string {
	pkg, _, _ := strings.Cut(hook, ".")
	return pkg
}

// expectedStops 按文档推出来的停止顺序：启动成功了的钩子里有停止钩子的那些，倒过来
func expectedStops(started []string) []string {
	var out []string
	for i := len(started) - 1; i >= 0; i-- {
		if slices.Contains(stopHookPkgs, pkgOf(started[i])) {
			out = append(out, pkgOf(started[i]))
		}
	}
	return out
}

func pkgsOf(hooks []string) []string {
	out := make([]string, len(hooks))
	for i, h := range hooks {
		out[i] = pkgOf(h)
	}
	return out
}

// waitGauge 轮询 /metrics，直到 name 的值达到 want。用来确认请求已经进了 handler 再发信号
func waitGauge(t *testing.T, p *harness.Process, name string, want float64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got float64
	for time.Now().Before(deadline) {
		got = p.Metrics(t).Sum(name)
		if got >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s 在 %v 内没到 %v（最后一次是 %v）：请求没进 handler，后面的断言没有意义", name, timeout, want, got)
}

// reqShot 一个请求的时间线
type reqShot struct {
	stream     string
	start, end time.Time
	status     int // 拿到响应时的状态码
	err        error
}

func (s reqShot) latency() time.Duration { return s.end.Sub(s.start) }

func (s reqShot) String() string {
	if s.err != nil {
		return fmt.Sprintf("%s err=%v (%v)", s.stream, s.err, s.latency().Round(time.Microsecond))
	}
	return fmt.Sprintf("%s status=%d (%v)", s.stream, s.status, s.latency().Round(time.Microsecond))
}

// lane 一路固定并发的流量：conc 个 worker，每个一条长连接，一个接一个地发
type lane struct {
	name string
	conc int
	req  func(i int) (*http.Request, error)
}

// getReq 每个请求都一样的 GET
func getReq(url string) func(int) (*http.Request, error) {
	return func(int) (*http.Request, error) { return http.NewRequest(http.MethodGet, url, nil) }
}

// timedTraffic 持续发请求直到 stop，记下每个请求的起止时刻。
//
// 和 harness.Load 的区别在于要时间线：优雅退出的断言是按「信号之前 / 之后发出」
// 分组的，只有总数和分位数判断不了
type timedTraffic struct {
	stopped atomic.Bool
	wg      sync.WaitGroup
	mu      sync.Mutex
	shots   []reqShot
	trs     []*http.Transport
}

func startTraffic(lanes ...lane) *timedTraffic {
	tr := &timedTraffic{}
	for _, s := range lanes {
		transport := &http.Transport{MaxIdleConns: s.conc, MaxIdleConnsPerHost: s.conc, DisableCompression: true}
		tr.trs = append(tr.trs, transport)
		// 10s 的客户端超时只是兜底：挂住的请求会以超时收场，延迟就是它挂了多久
		client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
		var next atomic.Int64
		for range s.conc {
			tr.wg.Add(1)
			go func() {
				defer tr.wg.Done()
				var mine []reqShot
				for !tr.stopped.Load() {
					sh := reqShot{stream: s.name, start: time.Now()}
					req, err := s.req(int(next.Add(1)))
					if err == nil {
						var resp *http.Response
						if resp, err = client.Do(req); err == nil {
							_, err = io.Copy(io.Discard, resp.Body)
							resp.Body.Close()
							sh.status = resp.StatusCode
						}
					}
					sh.end, sh.err = time.Now(), err
					mine = append(mine, sh)
					if err != nil {
						time.Sleep(2 * time.Millisecond) // 端口关了之后别空转
					}
				}
				tr.mu.Lock()
				tr.shots = append(tr.shots, mine...)
				tr.mu.Unlock()
			}()
		}
	}
	return tr
}

// stop 停止发新请求，等在途的做完，返回按发出时刻排好序的全部请求
func (tr *timedTraffic) stop() []reqShot {
	tr.stopped.Store(true)
	tr.wg.Wait()
	for _, t := range tr.trs {
		t.CloseIdleConnections()
	}
	sort.Slice(tr.shots, func(i, j int) bool { return tr.shots[i].start.Before(tr.shots[j].start) })
	return tr.shots
}

// connProbe 一次新连接探测
type connProbe struct {
	at      time.Time
	latency time.Duration
	status  int
	err     error
}

func (p connProbe) refused() bool { return errors.Is(p.err, syscall.ECONNREFUSED) }

// probeNewConns 每隔 every 用一条新连接（不复用）GET url，直到 ctx 结束。
// 看的是「信号之后新来的客户端」遇到什么：被拒、出错，还是挂住
func probeNewConns(ctx context.Context, url string, every time.Duration) <-chan []connProbe {
	out := make(chan []connProbe, 1)
	go func() {
		transport := &http.Transport{DisableKeepAlives: true}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		var probes []connProbe
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			pr := connProbe{at: time.Now()}
			resp, err := client.Get(url)
			if err == nil {
				_, err = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				pr.status = resp.StatusCode
			}
			pr.latency, pr.err = time.Since(pr.at), err
			probes = append(probes, pr)
			select {
			case <-ctx.Done():
				out <- probes
				return
			case <-tick.C:
			}
		}
	}()
	return out
}

// afterCloseErrors 输出里「资源关掉之后还有人在用」的痕迹：
// database/sql、go-redis 的「已关闭」错误，xclient 的「调晚了」panic 文案
var afterCloseErrors = []string{
	"sql: database is closed",
	"redis: client is closed",
	"was requested after",
}

// fmtMS 把时长渲染成毫秒，写进日志和汇报用
func fmtMS(d time.Duration) string {
	return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
}

// later 两个时刻里晚的那个
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// firstN 前 n 个，打印用
func firstN[T any](s []T, n int) []T { return s[:min(n, len(s))] }

// grepLines 含 pat 的前 n 行
func grepLines(text, pat string, n int) string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, pat) {
			out = append(out, line)
			if len(out) == n {
				break
			}
		}
	}
	return strings.Join(out, "\n")
}

// lastLines 最后 n 行
func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

// errKind 传输层错误去掉请求 URL 之后的那一截（端口每次都不一样），归类打印用：
// Get "http://127.0.0.1:41234/slow?ms=300": dial tcp ...: connection refused → dial tcp ...: connection refused
func errKind(err error) string {
	s := err.Error()
	if i := strings.Index(s, "\": "); i >= 0 {
		s = s[i+3:]
	}
	return s
}
