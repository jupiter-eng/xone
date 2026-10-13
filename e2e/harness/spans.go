package harness

import (
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/internal/spanlog"
)

// Span 服务导出的一个 Span，格式见 e2e/internal/spanlog
type Span = spanlog.Record

// ReadSpans 读一个 Span 文件，文件还不存在时返回空
func ReadSpans(path string) ([]Span, error) { return spanlog.ReadFile(path) }

// Spans 到目前为止导出的全部 Span，要 Options.Spans
func (p *Process) Spans(t testing.TB) []Span {
	t.Helper()
	if p.SpanFile == "" {
		t.Fatal("Spans needs Options.Spans")
	}
	spans, err := ReadSpans(p.SpanFile)
	if err != nil {
		t.Fatalf("read spans: %v", err)
	}
	return spans
}

// WaitSpan 等到出现一个满足 match 的 Span，timeout 内没等到就 t.Fatal。
//
// 响应回到客户端时服务端 Span 不一定已经结束（中间件在写完响应之后才 End），
// 所以读 Span 要等，不能发完请求立刻读
func (p *Process) WaitSpan(t testing.TB, timeout time.Duration, match func(Span) bool) Span {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, s := range p.Spans(t) {
			if match(s) {
				return s
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no matching span from %s within %v (%d spans so far)", p.name, timeout, len(p.Spans(t)))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
