package xflow

import (
	"context"
	"errors"
	"testing"
)

// 下面几个步骤只写了必须写的：Process 和 Rollback

type 扣券 struct{ err error }

func (s *扣券) Process(context.Context, *struct{}) error  { return s.err }
func (s *扣券) Rollback(context.Context, *struct{}) error { return nil }

type 扣库存 struct{ rolled bool }

func (s *扣库存) Process(context.Context, *struct{}) error  { return nil }
func (s *扣库存) Rollback(context.Context, *struct{}) error { s.rolled = true; return nil }

// 发通知 两个可选的都写了
type 发通知 struct{}

func (*发通知) Name() string                              { return "notify" }
func (*发通知) Dependency() Dependency                    { return Weak }
func (*发通知) Process(context.Context, *struct{}) error  { return errors.New("sms gateway timeout") }
func (*发通知) Rollback(context.Context, *struct{}) error { return nil }

func TestNew_UsesTypeNameWhenNameUnset(t *testing.T) {
	// 名字就写在类型上，再让使用者写一个返回同一个字符串的方法是纯粹的重复
	res := New[*struct{}]("下单", &扣券{err: errors.New("coupon used up")}).Execute(context.Background(), &struct{}{})

	var se *StepError
	if !errors.As(res.Err, &se) {
		t.Fatalf("失败的步骤应出现在错误里，got=%v", res.Err)
	}
	if se.Processor != "扣券" {
		t.Errorf("没写 Name 时应取类型名，got=%q", se.Processor)
	}
}

func TestNew_StrongDependencyWhenDependencyUnset(t *testing.T) {
	// 默认要偏安全的那一侧：失败就中断并回滚，而不是悄悄跳过继续往下扣钱
	stock := &扣库存{}
	res := New[*struct{}]("下单", stock, &扣券{err: errors.New("coupon used up")}).
		Execute(context.Background(), &struct{}{})

	if res.Success() {
		t.Fatal("没写 Dependency 的步骤失败应当中断流程")
	}
	if !stock.rolled {
		t.Error("强依赖失败应回滚前面已经成功的步骤")
	}
}

func TestNew_UsesNameAndDependencyWhenSet(t *testing.T) {
	res := New[*struct{}]("下单", &发通知{}).Execute(context.Background(), &struct{}{})

	if !res.Success() {
		t.Fatalf("弱依赖失败不该让流程失败，got=%v", res.Err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Processor != "notify" {
		t.Errorf("应按写的名字记进 Skipped，got=%+v", res.Skipped)
	}
}
