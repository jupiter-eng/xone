package xclient

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"time"

	"github.com/jupiter-eng/xone/xutil"
)

// ProbePolicy is how the connection probe at startup goes about it: how many attempts, how long
// each one gets, how long the first backoff is, and which errors need no further attempt
type ProbePolicy struct {
	// Attempts is how many attempts at most; anything below 1 counts as 1
	Attempts int

	// Timeout is one attempt's budget
	Timeout time.Duration

	// Interval is the ceiling of the first backoff, doubling each time afterwards with jitter; the
	// rules are in xutil.Retry
	Interval time.Duration

	// AuthFailed reports whether the server has refused these credentials outright. It may be left
	// empty, which means everything is retried.
	//
	// What it recognizes is not retried: a wrong password stays wrong however many attempts are
	// made, and only delays the same error by a few more backoffs (3s at most by default) while
	// leaving a few more authentication failures in the server's own records.
	AuthFailed func(error) bool
}

// Probe calls fn again and again as p says, until it succeeds, authentication is refused, the
// attempts run out, or the ctx is cancelled.
//
// The ctx fn receives carries this attempt's deadline and fn has to honour it -- Probe does not
// abandon a stuck call on fn's behalf: sql.DB's Close waits for the queries in flight, and an
// abandoned goroutine does not return just because the pool was closed. A driver that ignores the
// ctx itself (go-redis goes by the deadline and not by cancellation) is the caller's to handle
// inside fn.
//
// What it returns:
//   - nil on success;
//   - the very error fn returned, without one further attempt, when authentication was refused or
//     the TLS handshake failed because the certificate was rejected (see tlsRejected);
//   - otherwise what xutil.Retry returns: the last attempt's error, which also satisfies
//     errors.Is(err, ctx.Err()) when the ctx was cancelled.
//
// Asking p.AuthFailed once more lets the caller decide between reporting "authentication failed"
// and "cannot connect".
func Probe(ctx context.Context, p ProbePolicy, fn func(context.Context) error) error {
	return xutil.Retry(ctx, p.Attempts, p.Timeout, p.Interval, func(ctx context.Context) error {
		err := fn(ctx)
		if err != nil && (tlsRejected(err) || p.AuthFailed != nil && p.AuthFailed(err)) {
			return xutil.Permanent(err)
		}
		return err
	})
}

// tlsRejected: the handshake failed because a certificate was refused outright -- either we do not
// accept the peer's certificate (the wrong CA, a name that does not match, an expired one) or the
// peer does not accept ours (no client certificate sent, or one not signed by a CA it accepts).
//
// As with a wrong password, another attempt means the same certificate and the same verdict, so it
// is not retried.
//
// The peer's refusal is a TLS alert it sends. Over TCP, crypto/tls wraps it into a *net.OpError
// whose Op is "remote error", and the alert type inside is not exported (tls.AlertError is for QUIC
// alone), so it is recognized by the alert's text: bad_certificate, unknown_ca and
// certificate_required (what a server sends under TLS 1.3 when no client certificate was sent).
// Measured with go-redis v9.22.0 against a Redis 7.0.15 with tls-auth-clients yes: without a
// certificate it is remote error: tls: certificate required, and with one signed by another CA it
// is remote error: tls: unknown certificate authority.
func tlsRejected(err error) bool {
	var verr *tls.CertificateVerificationError
	if errors.As(err, &verr) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "remote error" && op.Err != nil {
		switch op.Err.Error() {
		case "tls: bad certificate", "tls: unknown certificate authority", "tls: certificate required":
			return true
		}
	}
	return false
}
