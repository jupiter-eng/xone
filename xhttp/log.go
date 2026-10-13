package xhttp

import (
	"errors"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
)

// installLog 每个逻辑请求记一条日志（Config.Log）。
//
// 挂在 OnSuccess / OnError 上，理由同指标：它们在所有重试结束之后只调一次
// （实测 resty v2.17.2，RetryCount=2、连接被掐断：发了 3 次，OnError 调 1 次，Attempt=3）。
// 记的是方法、host、路径、状态码、耗时、尝试次数；查询串、片段、userinfo、Header、body 都不记——
// 查询串里常有令牌和签名，与出站 Span 的 url.full 去掉查询串是同一条原则。
//
// resty 自己在重试路径上也打日志：每次失败的尝试一行 WARN（…, Attempt N），用完再一行 ERROR，
// 内容就是这里那一行的 error。Log 开着时这两种不再打（restyLogger 的 quiet，New 里设在 client 上），
// 一个逻辑请求只有这里的一行；每次尝试的细节在出站 Span 里（一次尝试一个 Span）。
// 使用者自己 SetLogger 换掉它的话，resty 的日志照 resty 的规矩全交给他，这里不插手。
//
// 不记的（实测 resty v2.17.2）：发出去之前就被 resty 拒掉的请求（比如 GET 带 multipart）
// 走 OnInvalid，不调这里的两个回调。
func installLog(client *resty.Client, slow time.Duration) {
	markStart(client)
	client.OnSuccess(func(_ *resty.Client, resp *resty.Response) {
		if resp != nil && resp.Request != nil {
			logRequest(resp.Request, resp, nil, slow)
		}
	})
	client.OnError(func(req *resty.Request, err error) {
		if req == nil {
			return
		}
		// 拿到了响应之后才出的错（比如 SetResult 解析失败），resty 包成 ResponseError
		var resp *resty.Response
		var re *resty.ResponseError
		if errors.As(err, &re) {
			resp, err = re.Response, re.Err
		}
		logRequest(req, resp, err, slow)
	})
}

// logRequest 记一个逻辑请求。
//
// 失败 = 没拿到能用的响应（err 不为 nil），或者下游回了 5xx；4xx 是业务上的回答，照常记 INFO。
// err 不为 nil 也包括拿到了响应的：200 但 SetResult 解不开、NoRedirectPolicy 下的 3xx。
// 又慢又失败的记失败（带 error）。
// ctx 是调用方的：出站 Span 开在 transport 里，这时已经结束，所以 span_id 是调用方的那个。
func logRequest(req *resty.Request, resp *resty.Response, err error, slow time.Duration) {
	ctx := req.Context()
	status, fallback := 0, sinceAttempt(req)
	if resp != nil {
		status, fallback = resp.StatusCode(), resp.Time()
	}
	took := elapsed(req, fallback)
	failed := err != nil || status >= 500
	isSlow := slow > 0 && took > slow
	if !failed && !isSlow && !slog.Default().Enabled(ctx, slog.LevelInfo) {
		return
	}

	host, path := target(req)
	attrs := []any{"method", req.Method, "host", host, "path", path, "status", status,
		"elapsed_ms", ms(took), "attempts", req.Attempt}
	if err != nil {
		attrs = append(attrs, "error", scrubError(err))
	}

	switch {
	case failed:
		slog.WarnContext(ctx, "http request failed", attrs...)
	case isSlow:
		slog.WarnContext(ctx, "slow http request", append(attrs, "threshold_ms", ms(slow))...)
	default:
		slog.InfoContext(ctx, "http request", attrs...)
	}
}

// sinceAttempt 这一次尝试到现在的耗时。一次都没发出去就失败的（SRV 查不到）Request.Time 是零值，
// time.Since 会是 292 年、日志里是 elapsed_ms 9223372036854.775，这时记 0
func sinceAttempt(req *resty.Request) time.Duration {
	if req.Time.IsZero() {
		return 0
	}
	return time.Since(req.Time)
}

// ---- 日志里的 URL ----
//
// 凡是 xhttp 写出去的 URL（请求日志的 error、resty 自己的日志、出站 Span 的 url.full）
// 都只留 scheme、host、路径：查询串、片段、userinfo 一律去掉。查询串里常有令牌和签名。

// scrubError 错误原文，里面的 URL 去掉查询串、片段和 userinfo。
//
// 先按结构来：错误链上（%w 包的、errors.Join 的都算）每个 *url.Error 的 URL 换成干净的再渲染。
// 标准库的原文是 Get "http://user:***@host/x?token=…": …（实测 Go 1.25：只把密码换成 ***，用户名照留），
// 而且那个 URL 不一定是绝对的：重定向策略拒绝时（NoRedirectPolicy、FlexibleRedirectPolicy 用完）
// 放进去的是 Location 原样，/b?token=… 这种相对的也有；查询串里还可能有没转义的空格。
// 这两种按文本找 URL 都认不全，所以文本只是兜底（scrubText），管错误链之外的自由文本。
func scrubError(err error) string {
	s := err.Error()
	for _, ue := range urlErrors(err) {
		// *url.Error 用 %q 渲染 URL，与 strconv.Quote 一致
		if clean := cleanURL(ue.URL); clean != ue.URL {
			s = strings.ReplaceAll(s, strconv.Quote(ue.URL), strconv.Quote(clean))
		}
	}
	return scrubText(s)
}

// urlErrors 错误链上所有的 *url.Error，包括 errors.Join 的每一支
func urlErrors(err error) []*url.Error {
	var out []*url.Error
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if ue, ok := e.(*url.Error); ok {
			out = append(out, ue)
		}
		switch w := e.(type) {
		case interface{ Unwrap() error }:
			walk(w.Unwrap())
		case interface{ Unwrap() []error }:
			for _, e := range w.Unwrap() {
				walk(e)
			}
		}
	}
	walk(err)
	return out
}

// cleanURL 去掉 URL 串的查询串、片段和 userinfo，相对的也一样。
// 解析不了的（比如路径里有不合法的 % 转义）切在第一个 ? 或 # 上，再按 scheme://…@ 的样子去掉 userinfo
func cleanURL(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return bareURL(u)
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	start := 0
	if i := strings.Index(raw, "://"); i >= 0 {
		start = i + len("://")
	}
	end := len(raw)
	if i := strings.IndexByte(raw[start:], '/'); i >= 0 {
		end = start + i
	}
	if at := strings.LastIndexByte(raw[start:end], '@'); at >= 0 {
		raw = raw[:start] + raw[start+at+1:]
	}
	return raw
}

// bareURL u 去掉 userinfo、查询串、片段之后的样子，不改 u
func bareURL(u *url.URL) string {
	c := *u
	c.User, c.RawQuery, c.ForceQuery, c.Fragment, c.RawFragment = nil, "", false, "", ""
	return c.String()
}

var (
	// quotedQuery 引号里的 URL（绝对的、相对的都算）的查询串和片段：到右引号为止，中间有空格也不停。
	// 问号前面不许有空白，免得把 "what is this?" 这种话也切掉
	quotedQuery = regexp.MustCompile(`"([^"\s?#]*)[?#][^"]*"`)
	// urlQuery 没在引号里的 http(s) URL 的查询串和片段，到空白为止
	urlQuery = regexp.MustCompile(`(https?://[^\s"?#]*)[?#][^\s"]*`)
	// urlUserinfo http(s) URL 的 userinfo（user@ 或 user:***@）
	urlUserinfo = regexp.MustCompile(`(https?://)[^\s"/?#@]*@`)
)

// scrubText 按文本去掉每个 URL 的查询串、片段和 userinfo。兜底用：认得出结构的先按结构来（scrubError）。
// 认不出来的：没有引号、也不以 http(s):// 开头的 URL（比如 resty 调试模式里的请求行 GET /x?token=…）
func scrubText(s string) string {
	s = quotedQuery.ReplaceAllString(s, `"$1"`)
	s = urlQuery.ReplaceAllString(s, "$1")
	return urlUserinfo.ReplaceAllString(s, "$1")
}

// target 最后一次尝试发出去的 host 和路径。还没拼出原生请求就失败的（请求前的中间件出错），
// 退回 resty 请求上的 URL 串。只取 Host 和路径：userinfo、查询串、片段都不要。
// 路径用转义过的形式（EscapedPath）：/a%2Fb 不记成 /a/b，路径里的换行、引号这类也不原样落进日志
func target(req *resty.Request) (host, path string) {
	u := (*url.URL)(nil)
	if req.RawRequest != nil {
		u = req.RawRequest.URL
	} else if parsed, err := url.Parse(req.URL); err == nil {
		u = parsed
	}
	if u == nil {
		return "", ""
	}
	return u.Host, u.EscapedPath()
}

// ms 耗时换成毫秒，保留到微秒。字段名带单位，与 xgorm、xredis 的 elapsed_ms 一致
func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
