package harness

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Proxy 一个可控的 TCP 代理：服务经它连 PG / Redis / 下游，故障测试拨弄它，
// 而不是去停真实的 PG / Redis（那会连累别的用例）。
//
//	pg := harness.NewProxy(t, harness.PGAddr())
//	p := harness.Start(t, harness.Options{PGAddr: pg.Addr()})
//	pg.Cut()        // 断开全部连接，新连接被拒（connection refused）：进程挂了
//	pg.Blackhole()  // 连接留着但不再回话，新连接的 SYN 石沉大海：主机宕机
//	pg.Restore()    // 恢复，同一个地址
//	pg.SetDelay(200 * time.Millisecond)
type Proxy struct {
	t      testing.TB
	target string
	addr   string
	delay  atomic.Int64 // time.Duration

	mu     sync.Mutex
	ln     net.Listener // Cut / Blackhole 之后是 nil
	hole   io.Closer    // Blackhole 期间占着原端口、永不 accept 的监听；不在黑洞里时是 nil
	conns  map[*pipe]struct{}
	closed bool

	holed atomic.Bool // Blackhole 期间为 true：已有连接两个方向都不再转发

	accepted atomic.Int64
}

// pipe 一对连接：客户端 ↔ 目标
type pipe struct {
	client, upstream net.Conn
	done             chan struct{}
	once             sync.Once
}

func (p *pipe) close() {
	p.once.Do(func() {
		close(p.done)
		p.client.Close()
		p.upstream.Close()
	})
}

// NewProxy 在 127.0.0.1 的随机端口上起一个转发到 target 的代理，测试结束时关掉
func NewProxy(t testing.TB, target string) *Proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &Proxy{t: t, target: target, addr: ln.Addr().String(), conns: map[*pipe]struct{}{}}
	p.serve(ln)
	t.Cleanup(p.close)
	return p
}

// Addr 代理的 host:port，交给服务当成 PG / Redis / 下游的地址
func (p *Proxy) Addr() string { return p.addr }

// Target 转发到哪
func (p *Proxy) Target() string { return p.target }

// Accepted 一共接受过多少个连接（含已经断开的）
func (p *Proxy) Accepted() int64 { return p.accepted.Load() }

// Active 当前有多少个连接在转发
func (p *Proxy) Active() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

// SetDelay 之后从目标回来的每一段数据都晚 d 才交给客户端，已经建好的连接也算。
// 按到达时刻算、不累加：一次请求-响应多出来的大约就是 d。0 取消延迟。
// 设得足够大（比如 time.Hour）就是「连得上、但对端不回话」
func (p *Proxy) SetDelay(d time.Duration) { p.delay.Store(int64(d)) }

// Cut 断开现有的全部连接，并且关掉监听：新连接被拒绝（connection refused），
// 像目标进程挂了一样。已经断开时什么都不做；黑洞里调它就从「宕机」变成「拒绝连接」
func (p *Proxy) Cut() {
	p.mu.Lock()
	ln, hole := p.ln, p.hole
	p.ln, p.hole = nil, nil
	p.holed.Store(false)
	conns := p.conns
	p.conns = map[*pipe]struct{}{}
	p.mu.Unlock()

	if ln != nil {
		ln.Close()
	}
	if hole != nil {
		hole.Close()
	}
	for c := range conns {
		c.close()
	}
}

// Blackhole 模拟目标主机宕机（断电、网络分区）。和 Cut 的「进程挂了、端口拒绝连接」不同，
// 宕机的主机什么都不回：
//
//   - 已经建好的连接留着，但两个方向都不再转发：发出去的请求等不到回复，也收不到 RST，
//     只能等客户端自己的读超时；
//   - 新连接的 SYN 没有回音，拨号方等满自己的建连超时（i/o timeout），而不是 connection refused。
//
// Restore 恢复，黑洞期间留着的连接一并断开：主机重启之后，旧连接在对端已经不存在了。
// SetDelay 管不到「新连接」这一半——代理照样 accept，TCP 秒连。
//
// 只能在测试主协程里调：系统不支持时（非 Linux，或内核不丢 SYN）t.Skip
func (p *Proxy) Blackhole() {
	p.t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hole != nil || p.closed {
		return
	}
	p.holed.Store(true)
	if p.ln != nil {
		p.ln.Close()
		p.ln = nil
	}
	// 刚关掉的端口偶尔要等一下才能再绑上，同 Restore
	var err error
	for range 50 {
		if p.hole, err = listenHole(p.addr); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.t.Skipf("proxy blackhole on %s: %v", p.addr, err)
}

// Restore 在原来的地址上重新监听。没断开时什么都不做。
// 从 Blackhole 恢复时先断开黑洞期间留着的连接。
// 绑不回原地址时记一个测试错误（Errorf，哪个协程里调都安全）
func (p *Proxy) Restore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	if p.hole != nil {
		p.hole.Close()
		p.hole = nil
		p.holed.Store(false)
		for c := range p.conns {
			c.close()
		}
		p.conns = map[*pipe]struct{}{}
	}
	if p.ln != nil {
		return
	}
	// 刚关掉的端口偶尔要等一下才能再绑上
	var err error
	for range 50 {
		var ln net.Listener
		if ln, err = net.Listen("tcp", p.addr); err == nil {
			p.serveLocked(ln)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.t.Errorf("proxy restore on %s: %v", p.addr, err)
}

func (p *Proxy) close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.Cut()
}

func (p *Proxy) serve(ln net.Listener) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.serveLocked(ln)
}

func (p *Proxy) serveLocked(ln net.Listener) {
	p.ln = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // Cut 关掉了监听
			}
			p.accepted.Add(1)
			go p.handle(c)
		}
	}()
}

func (p *Proxy) handle(client net.Conn) {
	upstream, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		client.Close()
		return
	}
	c := &pipe{client: client, upstream: upstream, done: make(chan struct{})}

	p.mu.Lock()
	if p.ln == nil {
		// 拨号期间被 Cut 了
		p.mu.Unlock()
		c.close()
		return
	}
	p.conns[c] = struct{}{}
	p.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.copyPlain(c) }()
	go func() { defer wg.Done(); p.copyDelayed(c) }()
	wg.Wait()

	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
}

// copyPlain 客户端 → 目标，不加延迟；黑洞期间收到的直接丢掉。任何一边断了就把整对关掉
func (p *Proxy) copyPlain(c *pipe) {
	defer c.close()
	buf := make([]byte, 32*1024)
	for {
		n, err := c.client.Read(buf)
		if n > 0 && !p.holed.Load() {
			if _, werr := c.upstream.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// chunk 从目标读到的一段数据和它到达的时刻
type chunk struct {
	data []byte
	at   time.Time
}

// copyDelayed 目标 → 客户端，每段数据到达之后等满当时的延迟才转出去。
// 读和写分成两个协程，延迟按到达时刻算，不会一段接一段地累加
func (p *Proxy) copyDelayed(c *pipe) {
	defer c.close()
	ch := make(chan chunk, 64)
	go func() {
		defer close(ch)
		buf := make([]byte, 32*1024)
		for {
			n, err := c.upstream.Read(buf)
			if n > 0 {
				select {
				case ch <- chunk{append([]byte(nil), buf[:n]...), time.Now()}:
				case <-c.done:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	for ck := range ch {
		if wait := time.Until(ck.at.Add(time.Duration(p.delay.Load()))); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-c.done:
				timer.Stop()
				return
			}
		}
		if p.holed.Load() {
			<-c.done // 黑洞：扣下不发，直到这条连接被 Restore / Cut 断开
			return
		}
		if _, err := c.client.Write(ck.data); err != nil {
			return
		}
	}
}
