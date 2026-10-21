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

// listenHole opens a listener on addr that never accepts and fills its accept queue: the kernel
// then drops the SYNs that arrive (what it does by default, with tcp_abort_on_overflow=0), the
// dialing side hears nothing back and can only sit out its own connect timeout -- the same thing
// a host that has gone down looks like.
//
// backlog is 0, so one placeholder connection fills the queue. Once open it checks itself with one
// dial: it returns an error when the kernel does not drop the SYN (with tcp_abort_on_overflow on,
// say, where an RST comes back), so that a test does not take "a host that cannot be reached" for
// what is really "connection refused".
//
// SOCK_CLOEXEC matters: the test process starts child processes, and an fd leaking into one would
// keep the port taken after it has been closed
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
	// The self-check: with the queue full, a dial has to time out rather than connect or be refused
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
