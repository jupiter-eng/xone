//go:build go1.24

package web

// The core module's floor is Go 1.23, while h2c needs http.Protocols, which exists from 1.24 (the
// reasoning is in protocols). The web integrations' floors are all above 1.24 and they are the only
// users of Server; building the core with 1.23 leaves this file out and the rest of this package
// works as usual.

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

// Server starts and stops one HTTP server: started once, stoppable before Start has even arrived,
// and with every handler returned by the time Stop returns nil.
//
// Its zero value works and it must not be copied. What Start / Stop mean is written on the
// integration's public methods (xgin.XGin.Start / Stop), which is what a caller reads.
//
// The module name (the "xgin" in the errors and the logs) is handed in on every call rather than
// kept in the struct: kept as a field, only New would fill it, and a caller writing &xgin.XGin{}
// would have the errors recorded under an empty module name, where xerror.Is(err, "xgin") does not
// hold and the log messages lose the module name in front
type Server struct {
	mu       sync.Mutex
	srv      *http.Server
	stopping bool

	// cancel cancels the root of every request ctx (http.Server.BaseContext), and Stop calls it once
	// Shutdown / Close has returned. The handlers that have not returned by then are either on a
	// hijacked connection (a WebSocket) or have timed out: Shutdown does not wait for the former and
	// Close cannot break it either, so without cancelling, their ctx would never end
	cancel context.CancelFunc

	// running is how many handlers have not returned yet, see track. It is how Stop knows whether
	// the handlers have really stopped after the connections were broken: a broken connection is not
	// a returned handler
	running atomic.Int64
}

// Start listens as c says, handles the requests with h, and blocks until the server stops. module
// is the module name in the errors and the logs.
//
// A TLS setup that does not work (TLS.ClientCAFile cannot be read) returns a config error and
// listens for nothing; a Stop that arrived first listens for nothing and returns nil outright.
// "<module> listening" is printed only once the certificate has been read and the port is bound: if
// either fails, the listen failed error is all there is
func (s *Server) Start(module string, c ServerConfig, h http.Handler) error {
	tlsCfg, err := c.serverTLS()
	if err != nil {
		return xerror.New(module, "config", err)
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	srv := s.newServer(module, c, addr, h)
	srv.TLSConfig = tlsCfg // ListenAndServeTLS adds the certificate and h2 to a copy of it
	base, cancel := context.WithCancel(context.Background())
	srv.BaseContext = func(net.Listener) context.Context { return base }

	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		// The shutdown signal arrived before startup. Listening as usual would bring the server up
		// after "the stop signal has already arrived" and keep it running until the framework times
		// out waiting
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
		// Failing to listen does not count as running: leaving s.srv behind would have a later Start,
		// once the port is free, say nothing but already running
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

	// A Stop arriving after listen and before Serve is fine: having seen a Shutdown, Serve closes ln
	// and returns ErrServerClosed (measured on Go 1.25.0: Shutdown followed by Serve has Serve return
	// http: Server closed on the spot, and the port refuses connections right away)
	if c.tlsEnabled() {
		err = srv.ServeTLS(ln, "", "") // the certificate is in TLSConfig already; ServeTLS still adds the h2 negotiation
	} else {
		err = srv.Serve(ln)
	}
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return xerror.Newf(module, "start", "listen on %s failed: %w", addr, err)
}

// listen reads the certificate and binds the port, and only both succeeding counts as listening.
//
// ListenAndServe(TLS) is not used: they tie those two steps to Serve and leave the caller unable to
// tell the moment "it is listening", so listening could only be printed before them -- and with an
// unreadable certificate or a port already taken, the logs would hold a listening followed
// immediately by a listen failed
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

// Stop shuts down gracefully: it waits for the requests in flight, until the ctx deadline at the
// latest; with requests still unfinished at the deadline it force-closes every connection, waits
// for the handlers to return, and returns an error. module is as in Start
func (s *Server) Stop(ctx context.Context, module string) error {
	s.mu.Lock()
	s.stopping = true // set first: a Start that has not begun listening returns outright when it arrives
	srv, cancelBase := s.srv, s.cancel
	s.mu.Unlock()

	if srv == nil {
		return nil // the signal arrived before the server came up
	}

	shutCtx, cancel := shutdownCtx(ctx)
	defer cancel()
	err := srv.Shutdown(shutCtx)
	if err != nil {
		// The deadline passed with requests still unfinished. Shutdown only returns an error and
		// leaves those connections alone, so Close() follows: every connection is broken and the ctx
		// of the requests in flight is cancelled with them.
		// Those requests fail, which is what a timeout means to begin with
		if cerr := srv.Close(); cerr != nil {
			slog.Warn(module+" force close failed", "error", cerr)
		}
	}
	// The handlers that have not returned by here are either on a hijacked connection or have timed
	// out, and their ctx is cancelled: a WebSocket's read loop and a long poll waiting on the ctx exit
	// because of it. Without cancelling, even a successful Shutdown would wait out the whole budget
	// and report still running
	cancelBase()

	// A broken connection is not a returned handler: Close only closes the connections and cancels
	// the requests' ctx, while the goroutines the handlers run in keep going. Without waiting for
	// them, the framework goes straight on to close the databases and the caches, and a handler that
	// has not returned would reach a connection pool that is already closed.
	// It waits after a successful Shutdown too: Shutdown does not wait for a hijacked connection (a
	// WebSocket) and Close cannot break it either, so the only way out is the ctx cancelled above
	// letting them exit by themselves
	if n := s.waitHandlers(ctx); n > 0 {
		return xerror.Newf(module, "stop", "%d handler(s) still running when the shutdown deadline passed: %w", n, ctx.Err())
	}
	if err != nil {
		return xerror.Newf(module, "stop", "graceful shutdown timed out, connections were force closed: %w", err)
	}
	return nil
}

// shutdownCtx gives Shutdown a deadline somewhat earlier than the ctx's: 20% of the time left, 1s
// at most.
//
// What is held back goes to waiting for the handlers to return after the connections were broken.
// Were Shutdown to use the whole of it, the budget would already be spent when Close lands, nobody
// would wait for the handlers that saw the cancellation and are finishing up, and the framework
// would go closing the databases before they returned all the same
func shutdownCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx) // with no deadline it waits forever, so nothing has to be held back
	}
	tail := min(time.Second, time.Until(deadline)/5)
	return context.WithDeadline(ctx, deadline.Add(-tail))
}

// track counts every request, which is how Stop waits for the handlers to really return.
// It wraps the outermost handler and a caller never sees it
func (s *Server) track(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.running.Add(1)
		defer s.running.Add(-1) // deferred: a handler ending in a panic (http.ErrAbortHandler, say) has to count down too
		h.ServeHTTP(w, r)
	})
}

// waitHandlers waits for every handler to return, until the ctx ends. It returns how many had not
// returned by then.
//
// It polls rather than waiting to be notified: it runs once, on the way out, and net/http's own
// Shutdown polls as well
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

// newServer builds the http.Server from the configuration; module is the module name in its own
// error log (see ErrorLog)
func (s *Server) newServer(module string, c ServerConfig, addr string, h http.Handler) *http.Server {
	// The timeouts have to be set explicitly: the zero value is "never times out", a slow client can
	// hold a connection forever, and once the connection count is full the whole service is
	// unavailable
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

// protocols is which protocols this server speaks.
//
// h2c uses the standard library's own UnencryptedHTTP2 (from Go 1.24) rather than x/net's
// h2c.NewHandler: the latter hijacks the connections and http.Server no longer knows them -- with a
// 2s request in flight it was measured returning nil from Shutdown after about 60µs, with Close()
// returning immediately as well, while that request ran its full 2s and the framework went straight
// on to close the databases. Left to the standard library, these connections are Shutdown's and
// Close's business just as the HTTP/1.1 ones are.
//
// The price is that the HTTP/1.1 Upgrade: h2c handshake is no longer supported, and only "prior
// knowledge" is -- a client sending the HTTP/2 preface right away (which is what gRPC and
// curl --http2-prior-knowledge do). A client sending Upgrade does not fail; it gets an ordinary
// HTTP/1.1 response.
//
// The HTTP/2 settings are the standard library's defaults (measured at up to 250 concurrent streams
// per connection).
func protocols(c ServerConfig) *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetHTTP2(true) // applies to TLS alone and does nothing on a plaintext connection
	p.SetUnencryptedHTTP2(c.UseH2C && !c.tlsEnabled())
	return p
}

// ErrorLog is the logger for http.Server.ErrorLog: the errors net/http reports itself (a failed TLS
// handshake, an Accept error, a duplicate WriteHeader and so on) are connected to slog at WARN, with
// the fixed message "<module> http server error" and the original text in the error field.
//
// Unset, net/http writes to the standard library's log: that reaches slog as INFO with the whole
// line as the message, different every time, which cannot be searched for or alerted on. WARN
// rather than ERROR: most of them are failed TLS handshakes from scanners and health checks, not
// failures of the service
func ErrorLog(module string) *log.Logger { return log.New(serverLog(module), "", 0) }

// serverLog passes a line net/http wrote to it on to slog, see ErrorLog
type serverLog string

func (m serverLog) Write(p []byte) (int, error) {
	slog.Warn(string(m)+" http server error", "error", strings.TrimSpace(string(p)))
	return len(p), nil
}
