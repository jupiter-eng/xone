package web

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsText(t *testing.T) {
	for ct, want := range map[string]bool{
		"application/json":         true,
		"text/plain; charset=utf8": true,
		"application/xml":          true,
		"image/png":                false,
		"application/octet-stream": false,
		"":                         false,
	} {
		if got := isText(ct); got != want {
			t.Errorf("isText(%q)=%v want %v", ct, got, want)
		}
	}
}

// countingBody 记下被读走了多少字节，用来看清究竟缓冲了多少
type countingBody struct {
	r      io.Reader
	read   int
	closed bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read += n
	return n, err
}
func (b *countingBody) Close() error { b.closed = true; return nil }

func TestSnapshotBody_BuffersOnlyPrefix(t *testing.T) {
	// maxRequestBody 限的是「记多少日志」，不该顺手变成「缓冲多少请求体」。
	// 整个读进来的话，一个大上传会躺进内存，而且 handler 要等它全部落地
	// 才能开始处理
	const total = 3 * maxRequestBody
	body := &countingBody{r: bytes.NewReader(bytes.Repeat([]byte("x"), total))}
	req := httptest.NewRequest("POST", "/", nil)
	req.Body, req.GetBody = body, nil
	req.Header.Set("Content-Type", "application/json")

	got := SnapshotBody(req)

	if len(got) != maxRequestBody {
		t.Errorf("记日志只该留前 %d 字节，got=%d", maxRequestBody, len(got))
	}
	if body.read > maxRequestBody+4096 {
		t.Errorf("只该读走前缀，实际已读 %d 字节（共 %d）", body.read, total)
	}

	// 下游仍要读得到完整的请求体
	rest, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != total {
		t.Errorf("下游该拿到完整请求体 %d 字节，got=%d", total, len(rest))
	}
}

func TestSnapshotBody_CloseReachesOriginalBody(t *testing.T) {
	// 换成 io.NopCloser 就等于把 http.Request 的关闭语义吃掉了
	body := &countingBody{r: bytes.NewReader([]byte(`{"a":1}`))}
	req := httptest.NewRequest("POST", "/", nil)
	req.Body, req.GetBody = body, nil
	req.Header.Set("Content-Type", "application/json")

	SnapshotBody(req)
	if err := req.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !body.closed {
		t.Error("Close 该落到原始 body 上")
	}
}

// partialThenEOF 先返回「部分数据 + 错误」，下一次调用返回 EOF。
// 这是合法的 io.Reader 行为，也是一个被截断的请求在网络层的样子。
type partialThenEOF struct {
	data []byte
	done bool
	err  error
}

func (r *partialThenEOF) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, r.data), r.err
}
func (r *partialThenEOF) Close() error { return nil }

func TestSnapshotBody_PrereadErrorPassedDownstream(t *testing.T) {
	// 只把字节接回去的话，下游读到的是「前缀 + EOF」——一个被截断的请求
	// 看上去和一个正常的请求一模一样，业务层据此判断「收全了」
	boom := errors.New("connection reset by peer")
	req := httptest.NewRequest("POST", "/", nil)
	req.Body, req.GetBody = &partialThenEOF{data: []byte("partial"), err: boom}, nil
	req.Header.Set("Content-Type", "application/json")

	SnapshotBody(req)

	got, err := io.ReadAll(req.Body)
	if string(got) != "partial" {
		t.Errorf("已经读到的字节要还给下游，got=%q", got)
	}
	if !errors.Is(err, boom) {
		t.Errorf("预读时撞上的错误也要还给下游，got=%v", err)
	}
}

func TestSnapshotBody_EmptyWithoutBody(t *testing.T) {
	if got := SnapshotBody(nil); got != nil {
		t.Errorf("nil 请求应当返回 nil，got=%v", got)
	}
	req := httptest.NewRequest("GET", "/hello", nil)
	req.Body = http.NoBody
	if got := SnapshotBody(req); got != nil {
		t.Errorf("NoBody 应当返回 nil，got=%v", got)
	}
	req2 := httptest.NewRequest("GET", "/hello", nil)
	req2.Body = nil
	if got := SnapshotBody(req2); got != nil {
		t.Errorf("没有 body 应当返回 nil，got=%v", got)
	}
}

func TestSnapshotBody_MediaTypeCaseInsensitive(t *testing.T) {
	// 媒体类型按 RFC 9110 大小写不敏感。照字面比的话，
	// Multipart/Form-Data 的上传绕过判断，文件内容整个进日志
	for _, ct := range []string{"Multipart/Form-Data; boundary=x", "Application/Octet-Stream"} {
		req := httptest.NewRequest("POST", "/hello", strings.NewReader("文件内容"))
		req.Header.Set("Content-Type", ct)
		if got := string(SnapshotBody(req)); !strings.Contains(got, "omitted") {
			t.Errorf("Content-Type=%q 的 body 不该被读进日志，got=%q", ct, got)
		}
	}
}

func TestAccessLog_ErrorsFieldIsRedacted(t *testing.T) {
	// 两个 Web 集成都经这里写 errors：驱动、下游报的错里常夹着凭证
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	var l AccessLog
	for msg, want := range map[string]string{"db down": `"errors":"db down"`, "login failed password=hunter2": `"errors":"` + Redacted + `"`} {
		buf.Reset()
		l.Log(&Access{Request: httptest.NewRequest("GET", "/x", nil), Route: "/x", Status: 500, Errors: msg})
		if !strings.Contains(buf.String(), want) || strings.Contains(buf.String(), "hunter2") {
			t.Errorf("errors 该记成 %s，got=%s", want, buf.String())
		}
	}
}

func TestAccessLog_EncodedResponseBodyOmitted(t *testing.T) {
	// 压缩过的字节进日志是一串乱码；identity 就是没压缩
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	l := AccessLog{RespBody: true}
	for enc, want := range map[string]string{
		"gzip":     `"response_body":"[gzip-encoded content omitted]"`,
		" BR ":     `"response_body":"[br-encoded content omitted]"`,
		"identity": `"response_body":"hello"`,
		"":         `"response_body":"hello"`,
	} {
		buf.Reset()
		h := http.Header{"Content-Type": {"text/plain"}}
		if enc != "" {
			h.Set("Content-Encoding", enc)
		}
		l.Log(&Access{Request: httptest.NewRequest("GET", "/x", nil), RespHeader: h, RespBody: []byte("hello")})
		if !strings.Contains(buf.String(), want) {
			t.Errorf("Content-Encoding=%q 该记 %s，got=%s", enc, want, buf.String())
		}
	}
}
