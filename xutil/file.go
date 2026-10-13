// Package xutil 放通用的小工具。
//
// 收录标准只有一条：它必须是**与框架无关**的纯函数或纯数据结构——
// 换个项目照样能用。任何「为了绕开某个设计问题」而存在的辅助函数
// 都不属于这里，那说明该修的是那个设计。
package xutil

import "os"

// FileExist 路径存在且是一个文件
func FileExist(path string) bool {
	stat, err := os.Stat(path)
	return err == nil && !stat.IsDir()
}

// DirExist 路径存在且是一个目录
func DirExist(path string) bool {
	stat, err := os.Stat(path)
	return err == nil && stat.IsDir()
}
