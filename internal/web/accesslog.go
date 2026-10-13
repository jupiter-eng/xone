package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
)

const (
	// maxRequestBody 请求体记录上限
	maxRequestBody = 256 * 1024
	// MaxResponseBody 响应体记录上限，比请求体小得多：
	// 响应通常大得多，而排查时看开头几 KB 基本够了。截响应的 writer 在集成里，按它截
	MaxResponseBody = 4 * 1024
)

// AccessLog 访问日志记什么：跳过哪些路径、记不记 body / 查询串 / 请求头和响应头。
//
// 零值就能用，什么都不额外记。各集成的日志中间件持有一份，
// 由它们自己的选项（xgin/middleware 的 WithSkipPaths、WithBody……）填。
type AccessLog struct {
	skipExact  map[string]bool
	skipPrefix []string

	ReqBody    bool
	RespBody   bool
	Query      bool
	ReqHeader  bool
	RespHeader bool
}

// SkipPaths 追加不记访问日志的路径：以 / 结尾的按前缀匹配（/health/ 命中 /health/live），其余精确匹配。
func (l *AccessLog) SkipPaths(paths ...string) {
	for _, p := range paths {
		if strings.HasSuffix(p, "/") {
			l.skipPrefix = append(l.skipPrefix, p)
		} else {
			if l.skipExact == nil {
				l.skipExact = map[string]bool{}
			}
			l.skipExact[p] = true
		}
	}
}

// Skip 这个请求要不要跳过：路径在跳过的名单里，或者 Info 级别的日志关着。
//
// 级别关掉时直接放行。中间件里那一整套——缓存请求体、包装 ResponseWriter
// 截响应、结束后的脱敏和序列化——存在的唯一目的就是拼出这一行日志。
// 等 slog 自己去判级别时，代价已经付完了，只是结果被丢弃。
//
// 每请求判一次而不是构造时判一次：级别可能在运行时变。
func (l *AccessLog) Skip(r *http.Request) bool {
	return l.skipPath(r.URL.Path) || !slog.Default().Enabled(r.Context(), slog.LevelInfo)
}

func (l *AccessLog) skipPath(path string) bool {
	if l.skipExact[path] {
		return true
	}
	return slices.ContainsFunc(l.skipPrefix, func(p string) bool { return strings.HasPrefix(path, p) })
}

// Access 一次请求结束时，访问日志里要记的、得从框架里取的那些值。
//
// 路由、状态码、写出的字节数、client IP、错误由集成量好了交进来，这里不去猜：
// 它们在每个框架里的取法都不一样（gin 没匹配上时路由是空串、没写响应体时 Size 是 -1；
// echo 的错误在中间件返回之后才渲染成响应）。其余的从 Request 上取。
type Access struct {
	Request  *http.Request
	Ctx      context.Context // 记这条日志用的 ctx（trace_id、xlog.AddKV 的字段从这里取）；nil 时用 Request 的
	Route    string          // 路由模板，没匹配上时是 unmatched
	Status   int
	Elapsed  time.Duration
	ClientIP string
	BytesOut int // 写出的响应体字节数，不含响应头；一个字节都没写是 0

	RespHeader http.Header // 记响应头、判响应体的 Content-Type 用
	ReqBody    []byte      // ReqBody 开着时 SnapshotBody 取的那一份
	RespBody   []byte      // RespBody 开着时截下的响应体，最多 MaxResponseBody 字节
	Errors     string      // 框架登记的错误，空串就不记；脱敏规则见 RedactText
}

// Log 把 a 记成一条访问日志（Info 级别，message 是 request completed）。
//
// 字段的名字和顺序是日志平台上的索引和看板认的，改一个就对不上一片：
// method route path status elapsed_ms client_ip host proto user_agent bytes_in bytes_out，
// 之后按开关依次是 request_headers query response_headers request_body response_body，最后是 errors。
func (l *AccessLog) Log(a *Access) {
	r := a.Request
	// 直接给 slog.Attr，不给交替的 key、value：后者每个值都要先装进 any
	// 再由 slog 拆出来，实测每条访问日志多 6 次分配（记 body 时 8 次）。
	// 切片在这里拼、在这里交给 slog，不从函数里返回：返回的话它就得分配在堆上
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("route", a.Route), // 没匹配上时记 unmatched，真实路径在 path 里
		// 只记 Path，不含查询串：GET /login?token=... 这种请求里
		// 凭证就在 URL 上。换成 RequestURI() 或 URL.String() 看着
		// 都像是「把日志记全一点」，实际是把凭证明文写进日志
		slog.String("path", r.URL.Path),
		slog.Int("status", a.Status),
		// 字段名带单位：slog 的 JSON 把 Duration 写成纳秒整数，51130 看不出是 51µs。
		// 浮点在 slog 的 JSON 里走 json.Marshal，实测每条多 2 次分配、约 0.5µs——
		// 换成整数微秒能省掉，但日志是给人读的，毫秒更顺手
		slog.Float64("elapsed_ms", millis(a.Elapsed)),
		slog.String("client_ip", a.ClientIP),
		slog.String("host", r.Host),
		slog.String("proto", r.Proto),
		slog.String("user_agent", r.UserAgent()),
		// 请求头里的 Content-Length；分块上传时没有，记 -1，和 net/http 的约定一致
		slog.Int64("bytes_in", r.ContentLength),
		slog.Int("bytes_out", a.BytesOut),
	}
	if l.ReqHeader {
		// 已是 slog.Value，slog.Any 会再装一次箱
		attrs = append(attrs, slog.Attr{Key: "request_headers", Value: RedactHeaders(r.Header)})
	}
	if l.Query && r.URL.RawQuery != "" {
		attrs = append(attrs, slog.String("query", redactForm(r.URL.RawQuery)))
	}
	if l.RespHeader {
		attrs = append(attrs, slog.Attr{Key: "response_headers", Value: RedactHeaders(a.RespHeader)})
	}
	if l.ReqBody {
		attrs = append(attrs, slog.String("request_body", RedactBody(a.ReqBody, r.Header.Get("Content-Type"))))
	}
	if l.RespBody {
		ct := a.RespHeader.Get("Content-Type")
		if isText(ct) && len(a.RespBody) > 0 {
			body := encodedOmitted(a.RespHeader)
			if body == "" {
				body = RedactBody(a.RespBody, ct)
			}
			attrs = append(attrs, slog.String("response_body", body))
		}
	}
	if a.Errors != "" {
		attrs = append(attrs, slog.String("errors", RedactText(a.Errors)))
	}

	ctx := a.Ctx
	if ctx == nil {
		ctx = r.Context()
	}
	slog.LogAttrs(ctx, slog.LevelInfo, "request completed", attrs...)
}

// encodedOmitted 响应体按 Content-Encoding 压缩过（gzip、br……）时记进日志的那一句，没压缩过返回空串。
//
// 压缩中间件（echo 的 middleware.Gzip、gin-contrib/gzip）排在访问日志里面，截下来的是压缩过的字节：
// 照记的话是一串二进制乱码，JSON 还会解不出来。解压一遍只为了记日志不值得，和上传的文件一样只记一句 omitted
func encodedOmitted(h http.Header) string {
	enc := strings.ToLower(strings.TrimSpace(h.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		return ""
	}
	return "[" + enc + "-encoded content omitted]"
}

// isText 判断是不是适合直接记进日志的文本类型
//
// json 用子串匹配，与 RedactBody 保持一致：application/vnd.api+json、
// application/problem+json 都是 JSON，精确匹配会让这些响应体一声不响地不记。
func isText(contentType string) bool {
	ct := strings.ToLower(contentType)
	return strings.Contains(ct, "json") ||
		strings.Contains(ct, "text/") ||
		strings.Contains(ct, "xml")
}

// SnapshotBody 取一份请求体副本（最多前 256KB），不影响后续读取。
//
// 优先用 GetBody：它返回的是副本，原始 Body 一点没动。
// 拿不到才退回「读出来再塞回去」。
func SnapshotBody(req *http.Request) []byte {
	if req == nil || req.Body == nil || req.Body == http.NoBody {
		return nil
	}

	// 先转小写：媒体类型按 RFC 9110 大小写不敏感，照字面比的话
	// 一个 Multipart/Form-Data 的上传会绕过下面的判断，文件内容整个进日志
	ct := strings.ToLower(req.Header.Get("Content-Type"))
	// 文件上传和二进制流不读：内容对排查没用，读一遍却要付全部的内存和时间
	if strings.Contains(ct, "multipart/form-data") {
		return []byte("[multipart/form-data omitted]")
	}
	if strings.Contains(ct, "application/octet-stream") {
		return []byte("[binary content omitted]")
	}

	if req.GetBody != nil {
		if rc, err := req.GetBody(); err == nil {
			defer rc.Close()
			b, _ := io.ReadAll(io.LimitReader(rc, maxRequestBody))
			return b
		}
	}

	// 退路：只读前 maxRequestBody 字节，剩下的原样留在流里。
	//
	// 不能整个读进来。maxRequestBody 限的是「记多少日志」，不该顺手变成
	// 「缓冲多少请求体」：一个 500MB 的 JSON 上传会整个躺进内存，而且
	// handler 要等它全部落地才能开始处理。记一行日志不配有这种代价。
	//
	// 读出错也要把已经读到的接回去：读一半就 return 的话，那半截请求体
	// 已经消失了，下游 handler 拿到的是个缺头的 body——
	// 记日志这件事不该有能力改变请求本身。
	head, err := io.ReadAll(io.LimitReader(req.Body, maxRequestBody))
	req.Body = &prefixedBody{prefix: head, rest: req.Body, preErr: err}
	if err != nil {
		return nil // 读不全就不记，但下游拿到的仍是完整的请求体和那个错误
	}
	return head
}

// prefixedBody 把已经读走的前缀接回请求体前面，连同预读时撞上的错误。
//
// 错误必须接回去。一个合法的 Reader 可以先返回「部分数据 + 错误」，
// 下一次调用再返回 EOF——只把字节接回去的话，下游读到的是
// 「前缀 + EOF」，一个被截断的请求看上去和一个正常的请求一模一样，
// 业务层据此判断「收全了」。记日志这件事不该有能力改变这个判断。
//
// Close 仍然落到原始 body 上：它才是真正持有连接的那个，
// 换成 io.NopCloser 就等于把 http.Request 的关闭语义吃掉了。
type prefixedBody struct {
	prefix []byte
	off    int
	rest   io.ReadCloser
	preErr error // 预读时撞上的错误，前缀读完之后交给下游
}

func (b *prefixedBody) Read(p []byte) (int, error) {
	if b.off < len(b.prefix) {
		n := copy(p, b.prefix[b.off:])
		b.off += n
		return n, nil
	}
	if b.preErr != nil {
		return 0, b.preErr
	}
	return b.rest.Read(p)
}

func (b *prefixedBody) Close() error { return b.rest.Close() }

// millis 耗时换成毫秒，保留到微秒
func millis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
