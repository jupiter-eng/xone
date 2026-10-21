package harness

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Proxy is a TCP proxy under the test's control: the service reaches PG / Redis / a downstream
// through it, and a fault test plays with the proxy rather than stopping the real PG / Redis
// (which would drag the other cases down with it).
//
//	pg := harness.NewProxy(t, harness.PGAddr())
//	p := harness.Start(t, harness.Options{PGAddr: pg.Addr()})
//	pg.Cut()        // break every connection, refuse the new ones (connection refused): the process died
//	pg.Blackhole()  // keep the connections but answer nothing, and let a new connection's SYN vanish: the host went down
//	pg.Restore()    // back up, at the same address
//	pg.SetDelay(200 * time.Millisecond)
type Proxy struct {
	t      testing.TB
	target string
	addr   string
	delay  atomic.Int64 // time.Duration

	mu     sync.Mutex
	ln     net.Listener // nil after Cut / Blackhole
	hole   io.Closer    // the listener holding the original port during Blackhole and never accepting; nil outside the blackhole
	conns  map[*pipe]struct{}
	closed bool

	holed atomic.Bool // true during Blackhole: the connections already open forward in neither direction

	accepted atomic.Int64
}

// pipe is one pair of connections: client <-> target
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

// NewProxy starts a proxy on a random port of 127.0.0.1 forwarding to target, and closes it when
// the test ends
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

// Addr is the proxy's host:port, to hand to the service as the address of PG / Redis / a downstream
func (p *Proxy) Addr() string { return p.addr }

// Target is what it forwards to
func (p *Proxy) Target() string { return p.target }

// Accepted is how many connections it has accepted in all (those already closed included)
func (p *Proxy) Accepted() int64 { return p.accepted.Load() }

// Active is how many connections it is forwarding right now
func (p *Proxy) Active() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

// SetDelay has every chunk coming back from the target reach the client d later, the connections
// already open included. It goes by the moment of arrival and does not add up: one
// request-response pair takes about d longer. 0 takes the delay off.
// Set high enough (time.Hour, say) it means "it connects, but the far end says nothing"
func (p *Proxy) SetDelay(d time.Duration) { p.delay.Store(int64(d)) }

// Cut breaks every connection there is and closes the listener: new connections are refused
// (connection refused), as if the target process had died. On an already broken one it does
// nothing; called inside a blackhole it turns "down" into "connection refused"
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

// Blackhole imitates the target host going down (loss of power, a network partition). Unlike Cut's
// "the process died and the port refuses connections", a host that is down answers nothing at all:
//
//   - the connections already open stay, but forward in neither direction: a request that went out
//     gets no answer and no RST either, and can only sit out the client's own read timeout;
//   - a new connection's SYN is never answered, so the dialing side waits out its own connect
//     timeout (i/o timeout) rather than hearing connection refused.
//
// Restore brings it back and breaks the connections kept through the blackhole along the way:
// after a host has rebooted, the old connections no longer exist on the far end.
// SetDelay does not reach the "new connection" half of this -- the proxy accepts as usual and TCP
// connects at once.
//
// It can only be called on the test's main goroutine: where the system does not support it (not
// Linux, or a kernel that does not drop the SYN) it t.Skips
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
	// A port just closed sometimes takes a moment before it can be bound again, as in Restore
	var err error
	for range 50 {
		if p.hole, err = listenHole(p.addr); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.t.Skipf("proxy blackhole on %s: %v", p.addr, err)
}

// Restore listens again at the original address. On one that is not broken it does nothing.
// Coming back from Blackhole it first breaks the connections kept through it.
// Failing to bind the original address records a test error (Errorf, which is safe from any
// goroutine)
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
	// A port just closed sometimes takes a moment before it can be bound again
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
				return // Cut closed the listener
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
		// Cut happened while dialing
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

// copyPlain goes client -> target with no delay, and drops what arrives during a blackhole. Either
// side closing closes the whole pair
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

// chunk is one piece of data read from the target and the moment it arrived
type chunk struct {
	data []byte
	at   time.Time
}

// copyDelayed goes target -> client, holding each chunk for the delay in force when it arrived.
// Reading and writing are separate goroutines, so the delay goes by the moment of arrival and does
// not add up chunk after chunk
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
			<-c.done // the blackhole: hold it back until Restore / Cut breaks this connection
			return
		}
		if _, err := c.client.Write(ck.data); err != nil {
			return
		}
	}
}
