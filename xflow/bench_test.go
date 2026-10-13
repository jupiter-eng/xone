package xflow

import (
	"context"
	"testing"

	"github.com/jupiter-eng/xone/internal/testkit"
)

type noop struct{ n string }

func (p noop) Name() string                         { return p.n }
func (p noop) Dependency() Dependency               { return Strong }
func (p noop) Process(context.Context, *int) error  { return nil }
func (p noop) Rollback(context.Context, *int) error { return nil }

func BenchmarkExecute_FiveStepsAllSucceed_MonitorOn(b *testing.B) {
	testkit.QuietSlog(b)
	f := New("下单", noop{"1"}, noop{"2"}, noop{"3"}, noop{"4"}, noop{"5"})
	d := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.Execute(context.Background(), &d)
	}
}

func BenchmarkExecute_FiveStepsAllSucceed_MonitorOff(b *testing.B) {
	testkit.QuietSlog(b)
	old := cfg
	cfg.Monitor = false
	b.Cleanup(func() { cfg = old })
	f := New("下单", noop{"1"}, noop{"2"}, noop{"3"}, noop{"4"}, noop{"5"})
	d := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.Execute(context.Background(), &d)
	}
}
