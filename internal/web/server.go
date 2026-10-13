//go:build go1.24

package web

// 核心模块的下限是 Go 1.23，而 h2c 要用 1.24 起才有的 http.Protocols（理由见 protocols）。
// Web 集成的下限都在 1.24 之上，只有它们用得到 Server；1.23 编核心时这个文件不编进去，
// 本包其余的部分照常可用。

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jupiter-eng/xone/xerror"
)

// Server 一个 HTTP 服务的启停：只起一次，可以早于 Start 就 Stop，
// Stop 返回 nil 时所有 handler 都已经返回。
//
// 零值就能用，不可复制。Start / Stop 的语义写在集成公开的方法上
// （xgin.XGin.Start / Stop），使用者读的是那里。
//
// 模块名（报错和日志里的 "xgin"）由集成在每次调用时交进来，不存在结构体里：
// 存成字段的话只有 New 会填它，使用者写 &xgin.XGin{} 时错误就记在空模块名下，
// xerror.Is(err, "xgin") 不成立，日志消息也少了开头的模块名
type Server struct {
	mu       sync.Mutex
	srv      *http.Server
	stopping bool

	// cancel 取消所有请求 ctx 的根（http.Server.BaseContext），Stop 在 Shutdown / Close 返回之后调。
	// 那时还没返回的 handler 要么在劫持走的连接上（WebSocket），要么已经超时：
	// 前者 Shutdown 不等、Close 也断不掉，不取消的话它们的 ctx 永远不结束
	cancel context.CancelFunc

	// running 还没返回的 handler 数，见 track。Stop 靠它知道断连之后
	// handler 是不是真的停下来了：连接断了不等于 handler 返回了
	running atomic.Int64
}

// Start 按 c 监听、用 h 处理请求，阻塞到服务停止。module 是报错和日志里的模块名。
//
// TLS 的设置不对（读不出 TLS.ClientCAFile）时返回 config 错误、不监听；
// Stop 早于它到达时不监听、直接返回 nil。
// "<module> listening" 在证书读好、端口绑上之后才打：两样有一样失败，只有那条 listen failed 的错误
func (s *Server) Start(module string, c ServerConfig, h http.Handler) error {
	tlsCfg, err := c.serverTLS()
	if err != nil {
		return xerror.New(module, "config", err)
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	srv := s.newServer(module, c, addr, h)
	srv.TLSConfig = tlsCfg // ListenAndServeTLS 在它的副本上补证书和 h2
	base, cancel := context.WithCancel(context.Background())
	srv.BaseContext = func(net.Listener) context.Context { return base }

	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		// 退出信号早于启动到达。照常监听的话，服务会在「已经收到停止信号」
		// 之后才起来，然后一直跑到框架等超时为止
		cancel()
		slog.Warn(module + " received the shutdown signal before starting, the server will not start")
		return nil
	}
	if s.srv != nil {
		s.mu.Unlock()
		cancel()
		return xerror.Newf(module, "start", "server is already running on %s", s.srv.Addr)
	}
	s.srv, s.cancel = srv, cancel
	s.mu.Unlock()

	ln, err := listen(c, srv)
	if err != nil {
		// 没监听上就不算在跑：留着 s.srv 的话，端口空出来之后再 Start 也只会说 already running
		s.mu.Lock()
		if s.srv == srv {
			s.srv, s.cancel = nil, nil
		}
		s.mu.Unlock()
		cancel()
		return xerror.Newf(module, "start", "listen on %s failed: %w", addr, err)
	}
	slog.Info(module+" listening", "addr", addr, "tls", c.tlsEnabled(), "mtls", c.tlsEnabled() && c.ClientCAFile != "",
		"h2c", c.UseH2C && !c.tlsEnabled())

	// Stop 在 listen 之后、Serve 之前到达也没关系：Serve 见到 Shutdown 过就关掉 ln、返回 ErrServerClosed
	// （实测 Go 1.25.0：先 Shutdown 再 Serve，Serve 当场返回 http: Server closed，端口随即拒绝连接）
	if c.tlsEnabled() {
		err = srv.ServeTLS(ln, "", "") // 证书已经在 TLSConfig 里；h2 的协商照样由 ServeTLS 补上
	} else {
		err = srv.Serve(ln)
	}
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return xerror.Newf(module, "start", "listen on %s failed: %w", addr, err)
}

// listen 读证书、绑端口，两样都成了才算监听上了。
//
// 不用 ListenAndServe(TLS)：它们把这两步和 Serve 捆在一起，调用方分不出
// 「监听上了」的那一刻，listening 只能在它们之前打——证书读不出来、端口被占时，
// 日志里先是一条 listening、紧跟着 listen failed
func listen(c ServerConfig, srv *http.Server) (net.Listener, error) {
	if c.tlsEnabled() {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, err
		}
		srv.TLSConfig.Certificates = append(srv.TLSConfig.Certificates, cert)
	}
	return net.Listen("tcp", srv.Addr)
}

// Stop 优雅关闭：等在途请求做完，最多等到 ctx 的截止时间；
// 到点还有请求没做完时强制断开所有连接，再等 handler 返回，并返回错误。module 同 Start
func (s *Server) Stop(ctx context.Context, module string) error {
	s.mu.Lock()
	s.stopping = true // 先置位：Start 若还没开始监听，到达时会直接返回
	srv, cancelBase := s.srv, s.cancel
	s.mu.Unlock()

	if srv == nil {
		return nil // 信号在服务起来之前就到了
	}

	shutCtx, cancel := shutdownCtx(ctx)
	defer cancel()
	err := srv.Shutdown(shutCtx)
	if err != nil {
		// 到点了还有请求没做完。Shutdown 只是返回错误，它不动那些连接，
		// 所以补一刀 Close()：断掉所有连接，在途请求的 ctx 随之取消。
		// 在途请求会失败，但那本来就是超时的含义
		if cerr := srv.Close(); cerr != nil {
			slog.Warn(module+" force close failed", "error", cerr)
		}
	}
	// 到这里还没返回的 handler 要么在劫持走的连接上，要么已经超时，取消它们的 ctx：
	// WebSocket 的读循环、等 ctx 的长轮询据此退出。不取消的话 Shutdown 成功也得把预算等满、报 still running
	cancelBase()

	// 连接断了不等于 handler 返回了：Close 只关连接、取消请求的 ctx，
	// handler 所在的协程照跑。不等它们的话，框架紧接着去关数据库和缓存，
	// 还没返回的 handler 会摸到已经关掉的连接池。
	// Shutdown 成功时也要等：被劫持走的连接（WebSocket）Shutdown 不等，Close 也断不掉，
	// 只能靠上面取消的 ctx 让它们自己退出
	if n := s.waitHandlers(ctx); n > 0 {
		return xerror.Newf(module, "stop", "%d handler(s) still running when the shutdown deadline passed: %w", n, ctx.Err())
	}
	if err != nil {
		return xerror.Newf(module, "stop", "graceful shutdown timed out, connections were force closed: %w", err)
	}
	return nil
}

// shutdownCtx 给 Shutdown 的截止时间比 ctx 的早一截：剩余时间的 20%，最多 1s。
//
// 留出来的这一截给断连之后等 handler 返回。Shutdown 用满全部时间的话，
// Close 那一刀落下时预算已经花完，看到 ctx 取消、正在收尾的 handler 没人等，
// 框架照样在它们返回之前就去关数据库了
func shutdownCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx) // 没有截止时间就一直等，也就不用留尾巴
	}
	tail := min(time.Second, time.Until(deadline)/5)
	return context.WithDeadline(ctx, deadline.Add(-tail))
}

// track 给每个请求计数，Stop 据此等 handler 真正返回。
// 包在 handler 最外面，使用者看不到它
func (s *Server) track(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.running.Add(1)
		defer s.running.Add(-1) // defer：handler 以 panic 结束（比如 http.ErrAbortHandler）时也要减回去
		h.ServeHTTP(w, r)
	})
}

// waitHandlers 等所有 handler 返回，直到 ctx 结束。返回那时还没返回的个数。
//
// 轮询而不是等通知：只在退出时跑这一次，net/http 的 Shutdown 自己也是轮询的
func (s *Server) waitHandlers(ctx context.Context) int64 {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		n := s.running.Load()
		if n == 0 {
			return 0
		}
		select {
		case <-ctx.Done():
			return s.running.Load()
		case <-tick.C:
		}
	}
}

// newServer 按配置构建 http.Server，module 是它自己的错误日志里的模块名（见 ErrorLog）
func (s *Server) newServer(module string, c ServerConfig, addr string, h http.Handler) *http.Server {
	// 超时必须显式设置：零值是「永不超时」，慢客户端可以一直占着连接，
	// 连接数打满之后服务整体不可用
	return &http.Server{
		Addr:              addr,
		Handler:           s.track(h),
		Protocols:         protocols(c),
		ReadHeaderTimeout: c.ReadHeaderTimeout,
		ReadTimeout:       c.ReadTimeout,
		WriteTimeout:      c.WriteTimeout,
		IdleTimeout:       c.IdleTimeout,
		ErrorLog:          ErrorLog(module),
	}
}

// protocols 这个服务说哪几种协议。
//
// h2c 用标准库自己的 UnencryptedHTTP2（Go 1.24 起），不用 x/net 的
// h2c.NewHandler：后者把连接劫持走，http.Server 从此不认识它们——
// 实测一个 2s 的请求在途时，Shutdown 约 60µs 就返回 nil，Close() 同样
// 立即返回，那个请求又照跑了整整 2s，而框架紧接着就去关数据库了。
// 交给标准库之后，这些连接和 HTTP/1.1 的一样归 Shutdown / Close 管。
//
// 代价是不再支持 HTTP/1.1 的 Upgrade: h2c 握手，只认「先验知识」——
// 客户端一上来就发 HTTP/2 前言（gRPC、curl --http2-prior-knowledge 都是这样）。
// 发 Upgrade 的客户端不会失败，拿到的是一个普通的 HTTP/1.1 响应。
//
// HTTP/2 的参数用标准库默认（实测单连接最多 250 个并发流）。
func protocols(c ServerConfig) *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetHTTP2(true) // 只对 TLS 生效，明文连接上它不起作用
	p.SetUnencryptedHTTP2(c.UseH2C && !c.tlsEnabled())
	return p
}

// ErrorLog 给 http.Server.ErrorLog 用的 logger：net/http 自己报的错（TLS 握手失败、Accept 出错、
// 重复的 WriteHeader……）接到 slog，级别 WARN，消息固定是 "<module> http server error"，原文在 error 字段里。
//
// 不设的话 net/http 写的是标准库的 log：进了 slog 也是 INFO，消息是每次都不一样的那一整行，
// 没法按消息检索和告警。WARN 而不是 ERROR：其中最多的是扫描器、健康检查打出来的 TLS 握手失败，不是服务的故障
func ErrorLog(module string) *log.Logger { return log.New(serverLog(module), "", 0) }

// serverLog 把 net/http 写进来的一行转给 slog，见 ErrorLog
type serverLog string

func (m serverLog) Write(p []byte) (int, error) {
	slog.Warn(string(m)+" http server error", "error", strings.TrimSpace(string(p)))
	return len(p), nil
}
