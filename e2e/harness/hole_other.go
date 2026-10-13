//go:build !linux

package harness

import (
	"errors"
	"io"
)

// listenHole 只在 Linux 上有：别的系统上 accept 队列满了之后的行为不一样，
// 不一定是丢掉 SYN。Proxy.Blackhole 遇到这个错误时跳过测试
func listenHole(string) (io.Closer, error) {
	return nil, errors.New("blackhole is only supported on linux")
}
