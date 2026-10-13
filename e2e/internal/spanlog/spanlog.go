// Package spanlog 把 Span 以 JSON 行写进文件，再读回来。
//
// 写的一端挂在 e2e 服务上（xtrace.AddSpanProcessor），读的一端在 harness 里。
// 两边共用这一个 Record，格式只在这里定义一次。
//
// 不用 stdouttrace：它导出的是 SDK 的内部结构（属性是 {Key, Value{Type, Value}}
// 的数组），测试里每取一个属性都要先翻一遍数组。这里摊平成 map，一行一个 Span。
package spanlog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Record 一个结束了的 Span
type Record struct {
	Name         string `json:"name"`
	TraceID      string `json:"trace_id"`
	SpanID       string `json:"span_id"`
	ParentSpanID string `json:"parent_span_id,omitempty"`
	// ParentRemote 父 Span 来自上游（经 traceparent 传进来）
	ParentRemote bool `json:"parent_remote,omitempty"`
	// Kind server / client / internal / producer / consumer
	Kind  string    `json:"kind"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// StatusCode Unset / Error / Ok
	StatusCode        string         `json:"status_code"`
	StatusDescription string         `json:"status_description,omitempty"`
	Attributes        map[string]any `json:"attributes,omitempty"`
	Events            []Event        `json:"events,omitempty"`
	Resource          map[string]any `json:"resource,omitempty"`
	Scope             string         `json:"scope"`
}

// Event Span 上的一个事件，比如 exception
type Event struct {
	Name       string         `json:"name"`
	Time       time.Time      `json:"time"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// Attr 取一个属性。读回来的数字一律是 float64（encoding/json 的规矩）
func (r Record) Attr(key string) (any, bool) {
	v, ok := r.Attributes[key]
	return v, ok
}

// Str 取一个属性并转成字符串，没有这个属性时返回空串
func (r Record) Str(key string) string {
	v, ok := r.Attributes[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// Duration Span 的耗时
func (r Record) Duration() time.Duration { return r.End.Sub(r.Start) }

// exporter 实现 sdktrace.SpanExporter
type exporter struct {
	mu sync.Mutex
	f  *os.File
}

// NewExporter 打开（或新建）path，每个 Span 追加一行 JSON。
//
// 配 sdktrace.NewSimpleSpanProcessor 用：Span 结束那一刻就落盘，
// 读的一端不用猜要等多久。退出时 xtrace 会调 Shutdown 关掉文件。
func NewExporter(path string) (sdktrace.SpanExporter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open span file: %w", err)
	}
	return &exporter{f: f}, nil
}

func (e *exporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, s := range spans {
		if err := enc.Encode(recordOf(s)); err != nil {
			return fmt.Errorf("encode span %q: %w", s.Name(), err)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.f == nil {
		return nil // 已经 Shutdown 了
	}
	// 一次 Write 写完整批：O_APPEND 下读的一端看不到两个 Span 交错的半行
	_, err := e.f.Write(buf.Bytes())
	return err
}

func (e *exporter) Shutdown(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.f == nil {
		return nil
	}
	err := e.f.Close()
	e.f = nil
	return err
}

func recordOf(s sdktrace.ReadOnlySpan) Record {
	r := Record{
		Name:              s.Name(),
		TraceID:           s.SpanContext().TraceID().String(),
		SpanID:            s.SpanContext().SpanID().String(),
		Kind:              s.SpanKind().String(),
		Start:             s.StartTime(),
		End:               s.EndTime(),
		StatusCode:        s.Status().Code.String(),
		StatusDescription: s.Status().Description,
		Attributes:        attrs(s.Attributes()),
		Scope:             s.InstrumentationScope().Name,
	}
	if p := s.Parent(); p.IsValid() {
		r.ParentSpanID, r.ParentRemote = p.SpanID().String(), p.IsRemote()
	}
	if res := s.Resource(); res != nil {
		r.Resource = attrs(res.Attributes())
	}
	for _, ev := range s.Events() {
		r.Events = append(r.Events, Event{Name: ev.Name, Time: ev.Time, Attributes: attrs(ev.Attributes)})
	}
	return r
}

func attrs(kvs []attribute.KeyValue) map[string]any {
	if len(kvs) == 0 {
		return nil
	}
	m := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		m[string(kv.Key)] = kv.Value.AsInterface()
	}
	return m
}

// ReadFile 读出文件里的全部 Span。文件还不存在时返回空。
//
// 最后一行没有换行符的话丢掉它：那是写的一端还没写完的半行。
func ReadFile(path string) ([]Record, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
		data = data[:i+1]
	} else {
		return nil, nil
	}

	var out []Record
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return out, fmt.Errorf("decode span line: %w", err)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}
