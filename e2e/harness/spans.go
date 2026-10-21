package harness

import (
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/internal/spanlog"
)

// Span is one span the service exported, in the format of e2e/internal/spanlog
type Span = spanlog.Record

// ReadSpans reads a span file, and returns nothing when the file does not exist yet
func ReadSpans(path string) ([]Span, error) { return spanlog.ReadFile(path) }

// Spans are every span exported so far, and wants Options.Spans
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

// WaitSpan waits for a span matching match to appear, and t.Fatals when none has within timeout.
//
// The server span has not necessarily ended by the time the response reaches the client (the
// middleware Ends it after the response has been written), so reading spans has to wait and cannot
// happen right after the request
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
