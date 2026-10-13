// Package xerror 提供统一的错误类型：带模块名和操作名，并正确支持 errors.Is / As
package xerror

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Error 统一错误类型，包含模块名、操作名和原始错误
type Error struct {
	Module string // 模块名，如 "xconfig", "xgorm"
	Op     string // 操作名，如 "config", "close"
	Err    error  // 原始错误
}

// Error 渲染为 "xone {module} {op} failed, err=[...]"
func (e *Error) Error() string { return e.render(true) }

// render prefix 表示要不要带开头那个 "xone "。
//
// 框架自己报的错模块名就是 xone，再加一次前缀就成了「xone xone init failed」。
// 前缀的作用是让这条错误落进别人的日志时看得出是谁报的：最外层说一次就够了，
// 被包在里面的那几层再说一遍只是噪声，见 nested
func (e *Error) render(prefix bool) string {
	var b strings.Builder
	b.Grow(32 + len(e.Module) + len(e.Op))
	if prefix && e.Module != "xone" {
		b.WriteString("xone ")
	}
	b.WriteString(e.Module)
	b.WriteByte(' ')
	b.WriteString(e.Op)
	b.WriteString(" failed")
	if e.Err != nil {
		b.WriteString(", err=[")
		b.WriteString(e.Err.Error())
		b.WriteByte(']')
	}
	return b.String()
}

// Unwrap 支持 errors.Is / errors.As 链式判断
func (e *Error) Unwrap() error {
	return e.Err
}

// New 创建一个 Error。
//
// err 已经是同一个模块的 Error 时原样返回：一个模块边界只该有一层框，
// 再包一层的话文本就成了 xone xgorm init failed, err=[xone xgorm connect failed, ...]，
// 信息没多，噪声翻倍，更具体的那个 op 还被压进了里层。
//
// 带类型的 nil（var e *Error; New(m, op, e)）当成没有原因：它什么都没报，
// 而把它原样存进 Err 的话，渲染和判断时都会在它身上解引用。
func New(module, op string, err error) *Error {
	if xe, ok := err.(*Error); ok {
		if xe == nil {
			return &Error{Module: module, Op: op}
		}
		if xe.Module == module {
			return xe
		}
	}
	return &Error{Module: module, Op: op, Err: nest(module, err)}
}

// Newf 创建一个带格式化消息的 Error。
//
// 参数里的 Error 渲染时去掉重复的部分，见 nested；errors.Is / As 照常穿透。
// 带类型的 nil 不折叠，交给 fmt 渲染成 <nil>。
func Newf(module, op, format string, args ...any) *Error {
	var own []any // 不改调用方的切片：args... 展开传进来的话它就是调用方那一个
	for i, a := range args {
		if xe, ok := a.(*Error); ok && xe != nil {
			if own == nil {
				own = slices.Clone(args)
			}
			own[i] = nest(module, xe)
		}
	}
	if own != nil {
		args = own
	}
	return &Error{Module: module, Op: op, Err: fmt.Errorf(format, args...)}
}

// nest 把要包进 module 那一层的 Error 换成 nested，别的错误原样返回
func nest(module string, err error) error {
	if xe, ok := err.(*Error); ok {
		return nested{e: xe, same: xe.Module == module}
	}
	return err
}

// nested 被包在另一个 Error 里的 Error，渲染时去掉重复的部分：
// 同一个模块的只留「op: 原因」，别的模块的去掉开头那个 "xone "。
//
// 一条错误落进使用者的日志时，只该有一处告诉他「这是 xone 报的」。每层都带
// 完整的框，就是 xone start failed, err=[... xone xconfig config failed, err=[...]]
// ——使用者要找的根因被埋在最里面的括号里。
type nested struct {
	e    *Error
	same bool
}

func (n nested) Error() string {
	if !n.same {
		return n.e.render(false)
	}
	if n.e.Err == nil {
		return n.e.Op + " failed"
	}
	return n.e.Op + ": " + n.e.Err.Error()
}

func (n nested) Unwrap() error { return n.e }

// Is 判断 err 的错误树里是否有指定模块产生的错误
//
// 遍历整棵树而不是只看最外层：模块之间会互相包装错误
// （xgorm 初始化失败里裹着 xconfig 的错误），只比对第一个 Error
// 会让 Is(err, "xconfig") 在这种链上返回 false，与本函数的语义不符。
//
// 「树」是因为 errors.Join：xone.Run 把启动错误和关闭错误 Join 在一起返回。
// 用 errors.As 找第一个 *Error 再顺着它的 .Err 往里走的话，Join 里排在
// 后面的兄弟从来不会被看到。所以这里和 errors.Is 一样，两种 Unwrap 都认
func Is(err error, module string) bool {
	if xe, ok := err.(*Error); ok {
		if xe == nil {
			return false // 带类型的 nil：什么都没报，也没有里层可走
		}
		if xe.Module == module {
			return true
		}
	}
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		return Is(u.Unwrap(), module)
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if Is(e, module) {
				return true
			}
		}
	}
	return false
}

// Module 提取最外层错误的模块名，若不是本包的错误则返回空字符串
//
// 取最外层而非遍历整条链：错误一路向上包装，最外层代表「谁最终报出了这个错误」，
// 这正是调用方要分流处理的依据。要判断链中是否涉及某个模块，用 Is。
//
// 遇到 errors.Join 时取深度优先遇到的第一个本包错误（与 errors.As 的顺序一致），
// 排在它前面的非本包错误跳过。xone.Run 总是把主因放在 Join 的最前面，
// 所以取到的就是主因那一个
func Module(err error) string {
	var xe *Error
	if errors.As(err, &xe) && xe != nil {
		return xe.Module
	}
	return ""
}
