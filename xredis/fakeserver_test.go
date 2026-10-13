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

// fakeRedis 一个够用的假 Redis：认 RESP 的命令帧，PING 回 PONG，其余回 OK。
//
// 自己写而不是引 miniredis：测试依赖会被 go mod tidy 记成直接依赖，
// 跟着进每个使用者的模块图（xtrace 上实测过，23 涨到 27）。
// 为了几十行的握手省这一笔是划算的。
type fakeRedis struct {
	ln net.Listener

	mu       sync.Mutex
	commands []string // 收到过的命令名，小写
	failPing bool     // 让 PING 返回错误，用来测连不上的分支
	failAuth bool     // 让 AUTH 回 WRONGPASS，用来测认证失败的分支
	stall    string   // 这个命令收下但永不回复，用来测 deadline
	resp3    bool     // 认 HELLO 3，之后按 RESP3 回复：go-redis 只在 RESP3 连接上发维护通知的握手
	live     int      // 当前还开着的连接数
	conns    map[net.Conn]struct{}
	replies  map[string]string        // 这些命令回这段原始 RESP，用来测 nil、服务端报错
	delays   map[string]time.Duration // 这些命令等一会儿再回，用来测慢命令
}

func newFakeRedis(t *testing.T) *fakeRedis { return newFakeRedisTLS(t, nil) }

// newFakeRedisTLS 同 newFakeRedis，cfg 不为 nil 时监听的是 TLS
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

// setReply 让命令 cmd（小写）回 raw，raw 是原始的 RESP，如 "$-1\r\n"、"-ERR x\r\n"
func (f *fakeRedis) setReply(cmd, raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.replies == nil {
		f.replies = map[string]string{}
	}
	f.replies[cmd] = raw
}

// setDelay 让命令 cmd（小写）等 d 再回
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

// stop 关掉监听和全部连接，之后再连就是 connection refused：模拟运行中 Redis 进程挂了
func (f *fakeRedis) stop() {
	f.ln.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.conns {
		c.Close()
	}
}

// count 收到过几次这个命令
func (f *fakeRedis) count(cmd string) int {
	n := 0
	for _, c := range f.seen() {
		if c == cmd {
			n++
		}
	}
	return n
}

// setStall 让某个命令收下之后永不回复，模拟一个卡住的 Redis
func (f *fakeRedis) setStall(cmd string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stall = cmd
}

// liveConns 当前还开着的连接数。
//
// 用它而不是数协程来验证「实例真的被关掉了」：协程数会被假服务端
// 自己的那些协程搅浑，而连接数是直接可观察的事实。
func (f *fakeRedis) liveConns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live
}

// waitConns 等连接数降到 want，超时返回实际值
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
			// 收下了，就是不回。连接留着，让调用方自己决定等到什么时候
			continue
		}

		var reply string
		switch {
		case hasCustom:
			reply = custom
		case name == "hello" && resp3:
			// HELLO 回一个 map，go-redis 由此认定协商成了 RESP3
			reply = "%1\r\n+proto\r\n:3\r\n"
		case name == "hello":
			// 回错误，go-redis 会退回 RESP2 继续——省掉实现 RESP3 握手
			reply = "-ERR unknown command 'HELLO'\r\n"
		case name == "auth" && failAuth:
			reply = "-WRONGPASS invalid username-password pair or user is disabled.\r\n"
		case name == "ping" && fail:
			reply = "-ERR 假装挂了\r\n"
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

// readCommand 读一个 RESP 命令帧：*N\r\n 之后跟 N 个 $len\r\n<data>\r\n
func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("不是命令帧: %q", line)
	}
	n, err := strconv.Atoi(line[1:])
	if err != nil || n < 0 {
		return nil, fmt.Errorf("参数个数不对: %q", line)
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
		buf := make([]byte, size+2) // 连同结尾的 \r\n 一起读掉
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}
