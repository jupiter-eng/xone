package xclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

var errDenied = errors.New("password authentication failed")

func policy(auth func(error) bool) ProbePolicy {
	return ProbePolicy{Attempts: 3, Timeout: 50 * time.Millisecond, Interval: time.Millisecond, AuthFailed: auth}
}

func isDenied(err error) bool { return errors.Is(err, errDenied) }

func TestProbe_StopsAfterSuccess(t *testing.T) {
	calls := 0
	err := Probe(context.Background(), policy(isDenied), func(context.Context) error {
		calls++
		if calls < 2 {
			return errors.New("connection refused")
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Errorf("succeeding on the second attempt must stop it, err=%v calls=%d", err, calls)
	}
}

func TestProbe_AuthFailureReturnsOriginalErrorAfterOneTry(t *testing.T) {
	// A wrong password stays wrong on a retry and only costs a few more backoffs; what is returned
	// has to be fn's very error, which is what the caller says "authentication failed" by and takes
	// the driver's error type out of with errors.As
	calls := 0
	wrapped := &wrapErr{errDenied}
	err := Probe(context.Background(), policy(isDenied), func(context.Context) error {
		calls++
		return wrapped
	})
	if calls != 1 {
		t.Errorf("an authentication failure must not be retried, it made %d attempts", calls)
	}
	if err != error(wrapped) {
		t.Errorf("what is returned must be fn's very error, got=%#v", err)
	}
}

func TestProbe_RetriesWhenAuthFailureNotRecognized(t *testing.T) {
	for name, auth := range map[string]func(error) bool{
		"no AuthFailed given":         nil,
		"AuthFailed does not know it": func(error) bool { return false },
	} {
		calls := 0
		err := Probe(context.Background(), policy(auth), func(context.Context) error {
			calls++
			return errDenied
		})
		if calls != 3 || !errors.Is(err, errDenied) {
			t.Errorf("%s: all 3 attempts must be made and the last error returned, calls=%d err=%v", name, calls, err)
		}
	}
}

func TestProbe_EachAttemptHasItsOwnDeadline(t *testing.T) {
	var deadlines []time.Duration
	_ = Probe(context.Background(), policy(nil), func(ctx context.Context) error {
		d, ok := ctx.Deadline()
		if !ok {
			t.Fatal("the ctx fn receives must carry this attempt's deadline")
		}
		deadlines = append(deadlines, time.Until(d))
		return errors.New("timeout")
	})
	for _, d := range deadlines {
		if d > 50*time.Millisecond {
			t.Errorf("one attempt's deadline must not exceed Timeout, got=%v", d)
		}
	}
}

func TestProbe_CtxCanceledStopsRetrying(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Probe(ctx, policy(nil), func(context.Context) error {
		calls++
		cancel()
		return errors.New("connection refused")
	})
	if calls != 1 || !errors.Is(err, context.Canceled) {
		t.Errorf("no further attempt may be made after a cancellation, which must be reported faithfully, calls=%d err=%v", calls, err)
	}
}

type wrapErr struct{ err error }

func (w *wrapErr) Error() string { return "connect: " + w.err.Error() }
func (w *wrapErr) Unwrap() error { return w.err }

func TestProbe_CertRejectedReturnsAfterOneTry(t *testing.T) {
	// What the peer's alert looks like over TCP is measured by shaking hands here and now: the
	// server wants a client certificate and the client sends none
	remote := remoteAlert(t)
	if !strings.Contains(remote.Error(), "remote error: tls: certificate required") {
		t.Fatalf("the alert measured does not match what the comment says: %v", remote)
	}
	for name, rejected := range map[string]error{
		"we do not accept the peer's certificate":  fmt.Errorf("failed to connect: %w", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}),
		"no client certificate sent under TLS 1.3": fmt.Errorf("dial: %w", remote),
		"the peer does not accept our CA":          &net.OpError{Op: "remote error", Err: errors.New("tls: unknown certificate authority")},
		"the certificate is broken":                &net.OpError{Op: "remote error", Err: errors.New("tls: bad certificate")},
	} {
		calls := 0
		err := Probe(context.Background(), policy(nil), func(context.Context) error {
			calls++
			return rejected
		})
		if calls != 1 || err != rejected {
			t.Errorf("%s: a rejected certificate must not be retried, calls=%d err=%v", name, calls, err)
		}
	}

	// Another alert (the peer failing internally, say) is retried as usual
	calls := 0
	_ = Probe(context.Background(), policy(nil), func(context.Context) error {
		calls++
		return &net.OpError{Op: "remote error", Err: errors.New("tls: internal error")}
	})
	if calls != 3 {
		t.Errorf("an alert unrelated to certificates must be retried as usual, calls=%d", calls)
	}
}

// remoteAlert shakes hands once, with the server wanting a client certificate and the client
// sending none, and returns the error the client read
func remoteAlert(t *testing.T) error {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		ClientAuth:   tls.RequireAnyClientCert,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			_ = c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true}) // only to get at the alert, nothing is verified
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Read(make([]byte, 1)) // under TLS 1.3 the server's refusal arrives on the first read after the handshake
	return err
}
