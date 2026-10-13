package harness

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// listenHole 在 addr 上开一个永不 accept 的监听，并把它的 accept 队列占满：
// 之后到达的 SYN 被内核直接丢掉（tcp_abort_on_overflow=0 时的默认行为），
// 拨号方收不到任何回复，只能等满自己的建连超时——和对端主机宕机时看到的一样。
//
// backlog 取 0，队列里放一个占位连接就满了。开完用一次拨号自检：
// 内核不丢 SYN（比如开了 tcp_abort_on_overflow，回的是 RST）时返回错误，
// 免得测试以为在测「连不上的主机」，实际测的是「拒绝连接」。
//
// SOCK_CLOEXEC 不能少：测试进程会起子进程，fd 漏过去的话端口在关掉之后还被子进程占着
func listenHole(addr string) (io.Closer, error) {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil || !ap.Addr().Is4() {
		return nil, fmt.Errorf("blackhole needs an IPv4 host:port, got %q", addr)
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("blackhole socket: %w", err)
	}
	h := &hole{fd: fd}
	ok := false
	defer func() {
		if !ok {
			h.Close()
		}
	}()
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		return nil, fmt.Errorf("blackhole setsockopt: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Port: int(ap.Port()), Addr: ap.Addr().As4()}); err != nil {
		return nil, fmt.Errorf("blackhole bind %s: %w", addr, err)
	}
	if err := syscall.Listen(fd, 0); err != nil {
		return nil, fmt.Errorf("blackhole listen: %w", err)
	}
	if h.filler, err = net.DialTimeout("tcp", addr, time.Second); err != nil {
		return nil, fmt.Errorf("blackhole fill the accept queue: %w", err)
	}
	// 自检：队列满了之后的拨号应该超时，而不是连上或被拒
	probe, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if probe != nil {
		probe.Close()
	}
	var ne net.Error
	if err == nil || !errors.As(err, &ne) || !ne.Timeout() {
		return nil, fmt.Errorf("this kernel does not drop SYNs to a full accept queue (probe dial: %v)", err)
	}
	ok = true
	return h, nil
}

type hole struct {
	fd     int
	filler net.Conn
}

func (h *hole) Close() error {
	if h.filler != nil {
		h.filler.Close()
	}
	return syscall.Close(h.fd)
}
