package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// xhttp/README.md XHttp 的默认值，以及这组用例配的 Timeout
const (
	faultHTTPTimeout          = 300 * time.Millisecond // XHttp.Timeout，和文档里「实测 1.24s」那一例同一个数
	faultHTTPRetryWaitTime    = 100 * time.Millisecond // RetryWaitTime 默认
	faultHTTPRetryMaxWaitTime = 2 * time.Second        // RetryMaxWaitTime 默认
)

// faultHTTPWorstCase xhttp/README.md：「开了 RetryCount 之后最坏情况是 (RetryCount+1) × Timeout
// 再加上几次退避等待」。每次退避不超过 RetryMaxWaitTime
func faultHTTPWorstCase(retries int) time.Duration {
	return time.Duration(retries+1)*faultHTTPTimeout + time.Duration(retries)*faultHTTPRetryMaxWaitTime
}

// faultDownstreamCall /proxy 调一次下游的结果
type faultDownstreamCall struct {
	resp harness.Response
	took time.Duration
}

// faultStartDownstream 起一个 /proxy 连 downstream 的服务：Timeout 300ms，XHttp 再叠 overlay
func faultStartDownstream(t *testing.T, downstream, overlay string) *harness.Process {
	t.Helper()
	return harness.Start(t, harness.Options{Downstream: downstream, Overlay: "XHttp:\n  Timeout: " + faultHTTPTimeout.String() + "\n" + overlay})
}

// faultCallDownstream GET /proxy?token=...[&method=...]
func faultCallDownstream(t *testing.T, p *harness.Process, token, method string) faultDownstreamCall {
	t.Helper()
	path := "/proxy?token=" + token
	if method != "" {
		path += "&method=" + method
	}
	r, took := faultTimed(t, p, http.MethodGet, path, nil)
	return faultDownstreamCall{r, took}
}

// faultRestyLogs xhttp 接到 slog 上的 resty 日志，按级别数一下。
// resty 只在 RetryCount > 0 时记：每次失败的尝试一行 WARN（…, Attempt N），用完再一行 ERROR
// （resty v2.17.2 request.go 的 Execute；xhttp/README.md「行为与实测」那张表的第一行）
func faultRestyLogs(p *harness.Process) (warn, errs int, lines []harness.Log) {
	for _, l := range p.FindLogs(func(l harness.Log) bool { return l.Msg() == "xhttp resty log" }) {
		switch l.Level() {
		case "WARN":
			warn++
		case "ERROR":
			errs++
		}
		lines = append(lines, l)
	}
	return warn, errs, lines
}

// faultWaitRestyLogs 等到 resty 的 ERROR 那一行出现（它在 handler 返回之前写，但 harness 读输出有先后），
// 再核对这次请求查询串里的 token 没有落进日志：
//
//	接到 slog 的每一行里都不该有它（xhttp/README.md XHttp：「URL 去掉查询串」）
//	stderr 里也不该有它：resty 的日志没接到 slog 时，resty 自己的 logger 往 stderr 写
//	「WARN RESTY Get "…/echo?token=…"」，这几行不经 slog，只查 slog 的那几行查不到它们。
//	xlog 运行期间写 stdout，stderr 上只剩绕开 slog 的输出，所以这里整段查
func faultWaitRestyLogs(t *testing.T, p *harness.Process, token string) (warn, errs int, lines []harness.Log) {
	t.Helper()
	if _, ok := p.LookForLog(waitFor, func(l harness.Log) bool { return l.Msg() == "xhttp resty log" && l.Level() == "ERROR" }); !ok {
		t.Errorf("xhttp/README.md 说 resty 的日志接到 slog（消息 xhttp resty log）：%v 内没等到重试用完的那行 ERROR；stderr 里 resty 自己写的纯文本有 %d 行",
			waitFor, strings.Count(p.Stderr(), " RESTY "))
	}
	warn, errs, lines = faultRestyLogs(p)
	for _, l := range lines {
		mustNotContain(t, "resty 日志", l.Line, token)
	}
	mustNotContain(t, "进程的 stderr（不经 slog 的输出）", p.Stderr(), token)
	return warn, errs, lines
}

// 下游慢过 XHttp.Timeout（桩每个请求等 2s）：
//
//	xhttp/README.md XHttp.Timeout：「一次尝试的超时」「管的是一次尝试，不是一次逻辑请求：开了 RetryCount 之后，
//	  最坏情况是 (RetryCount+1) × Timeout 再加上几次退避等待」
//	RetryCount：「默认 0，即不重试」
//	RetryOnlyIdempotent：「默认只重试幂等方法」「确认接口幂等之后再关掉它」
//	重试什么：「只重试传输层的错……拿到了响应就不重试——5xx 也不重试」
//	「要给整次逻辑请求封顶，用调用方的 ctx，每次尝试和中间的退避都听它的」
//
// 次数用下游桩数，每次尝试的时长用桩记下的到达时刻之差量
func TestFault_DownstreamSlowerThanTimeout_PerAttemptFail_IdempotentRetryOnly(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	t.Run("默认不重试_一次尝试在Timeout失败", func(t *testing.T) {
		t.Parallel()
		stub := harness.NewStub(t)
		stub.SetDelay(2 * time.Second)
		p := faultStartDownstream(t, stub.URL, "")
		c := faultCallDownstream(t, p, "t-"+harness.NewID(), "")
		if c.resp.Status != http.StatusBadGateway {
			t.Fatalf("下游超时应 502，实际 %v", c.resp)
		}
		if n := stub.Count(); n != 1 {
			t.Errorf("文档说 RetryCount 默认 0、不重试，下游应只收到 1 次，实际 %d 次", n)
		}
		if c.took < faultHTTPTimeout || c.took > faultHTTPTimeout+faultSlack {
			t.Errorf("文档说 Timeout 管一次尝试：配 %v，应在那时失败，实际 %v", faultHTTPTimeout, c.took)
		}
		t.Logf("数字：Timeout=%v、不重试：%s 后 502（%s）", faultHTTPTimeout, faultMS(c.took), c.resp.Body)
	})

	t.Run("GET按RetryCount重试_每次尝试各自Timeout_总耗时在文档的最坏情况内", func(t *testing.T) {
		t.Parallel()
		const retries = 2
		stub := harness.NewStub(t)
		stub.SetDelay(2 * time.Second)
		p := faultStartDownstream(t, stub.URL, fmt.Sprintf("  RetryCount: %d\n", retries))
		token := "t-" + harness.NewID()
		start := time.Now()
		c := faultCallDownstream(t, p, token, "")
		if c.resp.Status != http.StatusBadGateway {
			t.Fatalf("每次尝试都超时，应 502，实际 %v", c.resp)
		}
		reqs := stub.Requests()
		if len(reqs) != retries+1 {
			t.Fatalf("文档说 RetryCount=%d：GET 超时是传输层的错，应共发 %d 次，下游实际收到 %d 次", retries, retries+1, len(reqs))
		}
		if lo, hi := time.Duration(retries+1)*faultHTTPTimeout, faultHTTPWorstCase(retries); c.took < lo || c.took > hi+faultSlack {
			t.Errorf("文档说最坏是 (RetryCount+1)×Timeout 加退避 = %v 以内，而每次都等满 Timeout 至少 %v，实际 %v", hi, lo, c.took)
		}
		// 相邻两次到达之差 = 上一次尝试（等满 Timeout 被掐断）+ 一次退避（RetryWaitTime 起，不超过 RetryMaxWaitTime）
		var gaps []time.Duration
		for i := 1; i < len(reqs); i++ {
			gap := reqs[i].At.Sub(reqs[i-1].At)
			gaps = append(gaps, gap)
			if gap < faultHTTPTimeout+faultHTTPRetryWaitTime-20*time.Millisecond || gap > faultHTTPTimeout+faultHTTPRetryMaxWaitTime+faultSlack {
				t.Errorf("第 %d、%d 次之间隔了 %v：应是一次 Timeout（%v）加一次退避（%v–%v）", i, i+1, gap, faultHTTPTimeout, faultHTTPRetryWaitTime, faultHTTPRetryMaxWaitTime)
			}
		}
		last := start.Add(c.took).Sub(reqs[len(reqs)-1].At)
		if last < faultHTTPTimeout-20*time.Millisecond || last > faultHTTPTimeout+faultSlack {
			t.Errorf("最后一次尝试应在 Timeout（%v）被掐断，实际从到达到 502 用了 %v", faultHTTPTimeout, last)
		}
		for _, r := range reqs {
			if r.Query.Get("token") != token || r.Method != http.MethodGet {
				t.Errorf("每次重试都应是同一个 GET、带同样的查询串，实际 %s %s?%s", r.Method, r.Path, r.RawQuery)
			}
		}
		t.Logf("数字：Timeout=%v、RetryCount=%d：下游收到 %d 次，间隔 %v，最后一次 %s 后被掐断，总共 %s（文档的最坏情况 %v）",
			faultHTTPTimeout, retries, len(reqs), gaps, faultMS(last), faultMS(c.took), faultHTTPWorstCase(retries))

		// xhttp/README.md「行为与实测」那张表：resty 的日志「接到 slog（消息 xhttp resty log，内容在 detail 字段），
		// 级别照搬，URL 去掉查询串」；「开了重试后每次失败打一行 WARN……用完再打一行 ERROR」
		warn, errs, lines := faultWaitRestyLogs(t, p, token)
		if warn != retries+1 || errs != 1 {
			t.Errorf("文档说每次失败一行 WARN、用完一行 ERROR：%d 次失败应是 %d WARN + 1 ERROR，实际 %d WARN + %d ERROR", retries+1, retries+1, warn, errs)
		}
		for _, l := range lines {
			if !strings.Contains(l.Str("detail"), "/echo") {
				t.Errorf("resty 日志的 detail 里应有出错的 URL，实际 %s", l.Line)
			}
		}
	})

	t.Run("POST不重试", func(t *testing.T) {
		t.Parallel()
		stub := harness.NewStub(t)
		stub.SetDelay(2 * time.Second)
		p := faultStartDownstream(t, stub.URL, "  RetryCount: 2\n")
		c := faultCallDownstream(t, p, "t-"+harness.NewID(), http.MethodPost)
		if c.resp.Status != http.StatusBadGateway {
			t.Fatalf("下游超时应 502，实际 %v", c.resp)
		}
		if n := stub.Count(); n != 1 {
			t.Errorf("文档说 RetryOnlyIdempotent 默认开着、只重试幂等方法：POST 超时不该重发，下游实际收到 %d 次", n)
		}
		if last := stub.Last(t); last.Method != http.MethodPost {
			t.Errorf("下游收到的应是 POST，实际 %s", last.Method)
		}
		if c.took > faultHTTPTimeout+faultSlack {
			t.Errorf("不重试就只有一次尝试，应在 Timeout（%v）左右失败，实际 %v", faultHTTPTimeout, c.took)
		}
		t.Logf("数字：RetryCount=2、POST：下游收到 %d 次，%s 后 502", stub.Count(), faultMS(c.took))
	})

	t.Run("关掉RetryOnlyIdempotent后POST也重试", func(t *testing.T) {
		t.Parallel()
		stub := harness.NewStub(t)
		stub.SetDelay(2 * time.Second)
		p := faultStartDownstream(t, stub.URL, "  RetryCount: 2\n  RetryOnlyIdempotent: false\n")
		c := faultCallDownstream(t, p, "t-"+harness.NewID(), http.MethodPost)
		if c.resp.Status != http.StatusBadGateway {
			t.Fatalf("下游超时应 502，实际 %v", c.resp)
		}
		if n := stub.Count(); n != 3 {
			t.Errorf("RetryOnlyIdempotent: false 之后 POST 也按 RetryCount=2 重试，应共 3 次，下游实际收到 %d 次", n)
		}
		t.Logf("数字：RetryCount=2、RetryOnlyIdempotent=false、POST：下游收到 %d 次，%s 后 502", stub.Count(), faultMS(c.took))
	})

	t.Run("下游回5xx不重试", func(t *testing.T) {
		t.Parallel()
		stub := harness.NewStub(t)
		stub.SetStatus(http.StatusServiceUnavailable)
		stub.SetBody("application/json", `{"down":true}`)
		p := faultStartDownstream(t, stub.URL, "  RetryCount: 2\n")
		c := faultCallDownstream(t, p, "t-"+harness.NewID(), "")
		if c.resp.Status != http.StatusServiceUnavailable || string(c.resp.Body) != `{"down":true}` {
			t.Errorf("拿到了下游的响应就原样转回，应是 503 {\"down\":true}，实际 %v", c.resp)
		}
		if n := stub.Count(); n != 1 {
			t.Errorf("文档说「拿到了响应就不重试——5xx 也不重试」，下游实际收到 %d 次", n)
		}
		t.Logf("数字：RetryCount=2、下游回 503：下游收到 %d 次，%s 返回", stub.Count(), faultMS(c.took))
	})

	t.Run("调用方放弃之后不再重试", func(t *testing.T) {
		t.Parallel()
		const retries, giveUp = 5, 450 * time.Millisecond
		stub := harness.NewStub(t)
		stub.SetDelay(2 * time.Second)
		p := faultStartDownstream(t, stub.URL, fmt.Sprintf("  RetryCount: %d\n", retries))
		// 调用方在第二次尝试的中途断开：/proxy 用的是请求的 ctx，断开连接它就被取消
		ctx, cancel := context.WithTimeout(context.Background(), giveUp)
		defer cancel()
		_, err := p.Request(ctx, http.MethodGet, "/proxy?token=t", nil)
		if err == nil {
			t.Fatalf("调用方 %v 就放弃了，请求不该有响应", giveUp)
		}
		time.Sleep(100 * time.Millisecond) // 放弃那一刻可能正好有一次在路上
		atGiveUp := stub.Count()
		time.Sleep(faultHTTPWorstCase(retries) / 4) // 不听 ctx 的话，这段时间里至少还会再发两三次
		after := stub.Count()
		if after != atGiveUp {
			t.Errorf("文档说重试的退避和每次尝试都听调用方的 ctx：调用方放弃时下游收到 %d 次，之后又收到 %d 次", atGiveUp, after-atGiveUp)
		}
		if atGiveUp > 2 {
			t.Errorf("%v 之内最多两次尝试（每次 %v + 退避 %v 起），下游实际收到 %d 次", giveUp, faultHTTPTimeout, faultHTTPRetryWaitTime, atGiveUp)
		}
		t.Logf("数字：RetryCount=%d、调用方 %v 后放弃：下游一共收到 %d 次（不放弃的话是 %d 次）", retries, giveUp, after, retries+1)
	})
}

// 下游进程挂了（拒绝连接）和主机宕机（SYN 没回音）：都是传输层的错，GET 照样按 RetryCount 重试。
// 宕机时拨号会一直挂着，管住它的是 Timeout：xhttp/README.md 说它是「一次尝试的超时」，
// 拨号在一次尝试之内（DialTimeout 默认 30s，远大于 Timeout，所以先到的是 Timeout）。
// 拒绝连接时每次尝试立刻失败，总耗时只剩退避。
// 下游桩数不到次数（连接根本没到它），改数 resty 每次失败记的那行 WARN
func TestFault_DownstreamRefusedOrDown_GETRetriesRetryCount_EachWithinTimeout(t *testing.T) {
	harness.Require(t)
	t.Parallel()
	const retries = 2
	for _, c := range []struct {
		name   string
		inject func(*harness.Proxy)
		lo, hi time.Duration // 总耗时的区间
		errMsg []string      // 错误里至少有其中一个
	}{
		// 拒绝连接：三次尝试都是立刻失败，只剩两次退避（resty 的退避不低于 RetryWaitTime）
		{"拒绝连接", func(p *harness.Proxy) { p.Cut() }, retries * faultHTTPRetryWaitTime, faultHTTPWorstCase(retries), []string{"connection refused"}},
		// 主机宕机：每次都等满 Timeout。卡在拨号上时 http.Client 的超时报成 context deadline exceeded，
		// 卡在等响应头上时报成 Client.Timeout exceeded，两种都是 Timeout 到了
		{"主机宕机", func(p *harness.Proxy) { p.Blackhole() }, (retries + 1) * faultHTTPTimeout, faultHTTPWorstCase(retries),
			[]string{"context deadline exceeded", "Client.Timeout exceeded"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			stub := harness.NewStub(t)
			px := harness.NewProxy(t, stub.Addr())
			p := faultStartDownstream(t, "http://"+px.Addr(), fmt.Sprintf("  RetryCount: %d\n", retries))
			if ok := faultCallDownstream(t, p, "warmup", ""); ok.resp.Status != http.StatusOK {
				t.Fatalf("故障前 /proxy 应 200，实际 %v", ok.resp)
			}
			c.inject(px)

			token := "t-" + harness.NewID()
			call := faultCallDownstream(t, p, token, "")
			found := false
			for _, m := range c.errMsg {
				found = found || strings.Contains(string(call.resp.Body), m)
			}
			if call.resp.Status != http.StatusBadGateway || !found {
				t.Errorf("下游%s时应 502，错误里有 %q 之一，实际 %v", c.name, c.errMsg, call.resp)
			}
			if call.took < c.lo-20*time.Millisecond || call.took > c.hi+faultSlack {
				t.Errorf("下游%s、RetryCount=%d：总耗时应在 %v–%v（文档的最坏情况），实际 %v", c.name, retries, c.lo, c.hi, call.took)
			}
			warn, errs, _ := faultWaitRestyLogs(t, p, token)
			if warn != retries+1 || errs != 1 {
				t.Errorf("%d 次尝试都失败，应是 %d WARN + 1 ERROR，实际 %d WARN + %d ERROR", retries+1, retries+1, warn, errs)
			}
			if stub.Count() != 1 {
				t.Errorf("故障期间连接到不了桩，桩应只收到故障前那 1 次，实际 %d 次", stub.Count())
			}
			t.Logf("数字：下游%s、Timeout=%v、RetryCount=%d：%s 后 502，resty 记了 %d WARN + %d ERROR", c.name, faultHTTPTimeout, retries, faultMS(call.took), warn, errs)
		})
	}
}
