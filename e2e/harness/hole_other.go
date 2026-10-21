//go:build !linux

package harness

import (
	"errors"
	"io"
)

// listenHole is Linux-only: other systems behave differently once the accept queue is full and do
// not necessarily drop the SYN. Proxy.Blackhole skips the test when it meets this error
func listenHole(string) (io.Closer, error) {
	return nil, errors.New("blackhole is only supported on linux")
}
