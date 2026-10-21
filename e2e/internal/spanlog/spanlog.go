// Package spanlog writes spans to a file as JSON lines and reads them back.
//
// The writing end hangs on the e2e service (xtrace.AddSpanProcessor) and the reading end lives in
// the harness. Both share this one Record, so the format is defined here once.
//
// Not stdouttrace: it exports the SDK's internal structures (the attributes being an array of
// {Key, Value{Type, Value}}), so a test picking one attribute would walk that array every time.
// Here they are flattened into a map, one span per line.
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

// Record is one span that has ended
type Record struct {
	Name         string `json:"name"`
	TraceID      string `json:"trace_id"`
	SpanID       string `json:"span_id"`
	ParentSpanID string `json:"parent_span_id,omitempty"`
	// ParentRemote says the parent span comes from upstream (handed over through traceparent)
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

// Event is one event on a span, an exception for instance
type Event struct {
	Name       string         `json:"name"`
	Time       time.Time      `json:"time"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// Attr takes one attribute. A number read back is always a float64 (encoding/json's rule)
func (r Record) Attr(key string) (any, bool) {
	v, ok := r.Attributes[key]
	return v, ok
}

// Str takes one attribute as a string, and returns an empty string when there is none
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

// Duration is how long the span took
func (r Record) Duration() time.Duration { return r.End.Sub(r.Start) }

// exporter implements sdktrace.SpanExporter
type exporter struct {
	mu sync.Mutex
	f  *os.File
}

// NewExporter opens (or creates) path and appends one JSON line per span.
//
// For use with sdktrace.NewSimpleSpanProcessor: a span lands on disk the moment it ends, so the
// reading end has nothing to guess about how long to wait. On exit xtrace calls Shutdown, which
// closes the file.
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
		return nil // Shutdown has happened already
	}
	// One Write per batch: under O_APPEND the reading end never sees half a line of two spans
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

// ReadFile reads every span in the file, and returns nothing when the file does not exist yet.
//
// A last line without a newline is dropped: that is half a line the writing end has not finished
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
