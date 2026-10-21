package xredis

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRedis is a Redis stub that does just enough: it reads RESP command frames, answers PONG to PING and OK to everything else.
//
// Written here rather than pulling in miniredis: a test dependency is recorded as a direct dependency by go mod tidy
// and travels into every caller's module graph (measured on xtrace: 23 grew to 27).
// Saving that for a few dozen lines of handshake is a good trade.
type fakeRedis struct {
	ln net.Listener

	mu       sync.Mutex
	commands []string // the command names received, lowercased
	failPing bool     // make PING return an error, for the cannot-connect branch
	failAuth bool     // make AUTH answer WRONGPASS, for the failed-authentication branch
	stall    string   // accept this command and never answer, for testing deadlines
	resp3    bool     // accept HELLO 3 and answer in RESP3 afterwards: go-redis only sends the maintenance-notification handshake on a RESP3 connection
	live     int      // the number of connections currently open
	conns    map[net.Conn]struct{}
	replies  map[string]string        // answer these commands with this raw RESP, for testing nil and server errors
	delays   map[string]time.Duration // delay these commands before answering, for testing slow commands
}

func newFakeRedis(t *testing.T) *fakeRedis { return newFakeRedisTLS(t, nil) }

// newFakeRedisTLS is newFakeRedis listening over TLS when cfg is non-nil.
func newFakeRedisTLS(t *testing.T, cfg *tls.Config) *fakeRedis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		ln = tls.NewListener(ln, cfg)
	}
	f := &fakeRedis{ln: ln, conns: map[net.Conn]struct{}{}}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeRedis) addr() string { return f.ln.Addr().String() }

func (f *fakeRedis) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func (f *fakeRedis) setFailPing(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failPing = v
}

// setReply makes the command cmd (lowercase) answer raw, which is raw RESP such as "$-1\r\n" or "-ERR x\r\n".
func (f *fakeRedis) setReply(cmd, raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.replies == nil {
		f.replies = map[string]string{}
	}
	f.replies[cmd] = raw
}

// setDelay makes the command cmd (lowercase) wait d before answering.
func (f *fakeRedis) setDelay(cmd string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delays == nil {
		f.delays = map[string]time.Duration{}
	}
	f.delays[cmd] = d
}

func (f *fakeRedis) setResp3(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resp3 = v
}

func (f *fakeRedis) setFailAuth(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAuth = v
}

// stop closes the listener and every connection, so connecting afterwards is a connection refused: simulating the Redis process dying at runtime.
func (f *fakeRedis) stop() {
	f.ln.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.conns {
		c.Close()
	}
}

// count is how many times this command was received.
func (f *fakeRedis) count(cmd string) int {
	n := 0
	for _, c := range f.seen() {
		if c == cmd {
			n++
		}
	}
	return n
}

// setStall makes a command be accepted and never answered, simulating a stuck Redis.
func (f *fakeRedis) setStall(cmd string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stall = cmd
}

// liveConns is the number of connections currently open.
//
// It is used instead of counting goroutines to verify "the instance really was closed": the goroutine count is
// muddied by the fake server's own goroutines, while the connection count is a directly observable fact.
func (f *fakeRedis) liveConns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live
}

// waitConns waits for the connection count to fall to want and returns the actual value on timeout.
func (f *fakeRedis) waitConns(want int) int {
	for i := 0; i < 100; i++ {
		if n := f.liveConns(); n == want {
			return n
		}
		time.Sleep(20 * time.Millisecond)
	}
	return f.liveConns()
}

func (f *fakeRedis) serve(conn net.Conn) {
	f.mu.Lock()
	f.live++
	f.conns[conn] = struct{}{}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.live--
		delete(f.conns, conn)
		f.mu.Unlock()
		conn.Close()
	}()
	r := bufio.NewReader(conn)
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		if len(args) == 0 {
			continue
		}
		name := strings.ToLower(args[0])

		f.mu.Lock()
		f.commands = append(f.commands, name)
		fail := f.failPing
		failAuth := f.failAuth
		stall := f.stall == name
		resp3 := f.resp3
		custom, hasCustom := f.replies[name]
		delay := f.delays[name]
		f.mu.Unlock()
		time.Sleep(delay)

		if stall {
			// Accepted and simply not answered. The connection stays open and the caller decides how long to wait.
			continue
		}

		var reply string
		switch {
		case hasCustom:
			reply = custom
		case name == "hello" && resp3:
			// HELLO answers a map, from which go-redis concludes that RESP3 was negotiated
			reply = "%1\r\n+proto\r\n:3\r\n"
		case name == "hello":
			// Answer an error and go-redis falls back to RESP2 and carries on -- which saves implementing the RESP3 handshake
			reply = "-ERR unknown command 'HELLO'\r\n"
		case name == "auth" && failAuth:
			reply = "-WRONGPASS invalid username-password pair or user is disabled.\r\n"
		case name == "ping" && fail:
			reply = "-ERR pretending to be down\r\n"
		case name == "ping":
			reply = "+PONG\r\n"
		default:
			reply = "+OK\r\n"
		}
		if _, err := io.WriteString(conn, reply); err != nil {
			return
		}
	}
}

// readCommand reads one RESP command frame: *N\r\n followed by N times $len\r\n<data>\r\n.
func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("not a command frame: %q", line)
	}
	n, err := strconv.Atoi(line[1:])
	if err != nil || n < 0 {
		return nil, fmt.Errorf("wrong argument count: %q", line)
	}

	args := make([]string, 0, n)
	for i := 0; i < n; i++ {
		head, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimRight(head, "\r\n")[1:])
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2) // read the trailing \r\n with it
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}
