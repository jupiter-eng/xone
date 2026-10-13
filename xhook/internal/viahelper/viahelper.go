// Package viahelper 只给 xhook 的测试用：一个替别的包登记钩子的辅助包。
//
// 登记的是这里造的闭包，函数名属于本包；登记它的却是调用 Register 的那个包。
// 框架该把钩子算在后者头上，否则所有经辅助包登记的包都会被当成同一个。
package viahelper

import (
	"context"

	"github.com/jupiter-eng/xone/xhook"
)

// Register 替调用方登记一个什么都不做的启动钩子。
//
// 不许内联：内联之后这个闭包会在调用方那里实例化，名字随之变成调用方的，
// 测试就分辨不出「按名字认包」和「按调用栈认包」了。真实的辅助包没有这个保证——
// 稍大一点的函数就不会被内联，方法值更是从来不会
//
//go:noinline
func Register() {
	xhook.BeforeStart(func(context.Context) error { return nil })
}
