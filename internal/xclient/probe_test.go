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
		t.Errorf("第二次成功就该停，err=%v calls=%d", err, calls)
	}
}

func TestProbe_AuthFailureReturnsOriginalErrorAfterOneTry(t *testing.T) {
	// 密码错了再试也是错，只是多等几轮退避；返回的要是 fn 的那个错误本身，
	// 调用方靠它说「认证失败」，也靠 errors.As 取驱动的错误类型
	calls := 0
	wrapped := &wrapErr{errDenied}
	err := Probe(context.Background(), policy(isDenied), func(context.Context) error {
		calls++
		return wrapped
	})
	if calls != 1 {
		t.Errorf("认证失败不该重试，试了 %d 次", calls)
	}
	if err != error(wrapped) {
		t.Errorf("返回的应是 fn 的错误本身，got=%#v", err)
	}
}

func TestProbe_RetriesWhenAuthFailureNotRecognized(t *testing.T) {
	for name, auth := range map[string]func(error) bool{
		"没给 AuthFailed": nil,
		"AuthFailed 不认": func(error) bool { return false },
	} {
		calls := 0
		err := Probe(context.Background(), policy(auth), func(context.Context) error {
			calls++
			return errDenied
		})
		if calls != 3 || !errors.Is(err, errDenied) {
			t.Errorf("%s：应试满 3 次并返回最后的错误，calls=%d err=%v", name, calls, err)
		}
	}
}

func TestProbe_EachAttemptHasItsOwnDeadline(t *testing.T) {
	var deadlines []time.Duration
	_ = Probe(context.Background(), policy(nil), func(ctx context.Context) error {
		d, ok := ctx.Deadline()
		if !ok {
			t.Fatal("fn 拿到的 ctx 应带着这一次的截止时间")
		}
		deadlines = append(deadlines, time.Until(d))
		return errors.New("timeout")
	})
	for _, d := range deadlines {
		if d > 50*time.Millisecond {
			t.Errorf("单次截止时间不该超过 Timeout，got=%v", d)
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
		t.Errorf("取消之后不该再试，并如实报取消，calls=%d err=%v", calls, err)
	}
}

type wrapErr struct{ err error }

func (w *wrapErr) Error() string { return "connect: " + w.err.Error() }
func (w *wrapErr) Unwrap() error { return w.err }

func TestProbe_CertRejectedReturnsAfterOneTry(t *testing.T) {
	// 对端的告警在 TCP 上长什么样，现场握一次手量出来：服务端要客户端证书，客户端不带
	remote := remoteAlert(t)
	if !strings.Contains(remote.Error(), "remote error: tls: certificate required") {
		t.Fatalf("量出来的告警和注释里写的对不上：%v", remote)
	}
	for name, rejected := range map[string]error{
		"我们不认对端的证书":        fmt.Errorf("failed to connect: %w", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}),
		"TLS 1.3 下没带客户端证书": fmt.Errorf("dial: %w", remote),
		"对端不认我们的 CA":       &net.OpError{Op: "remote error", Err: errors.New("tls: unknown certificate authority")},
		"证书坏了":             &net.OpError{Op: "remote error", Err: errors.New("tls: bad certificate")},
	} {
		calls := 0
		err := Probe(context.Background(), policy(nil), func(context.Context) error {
			calls++
			return rejected
		})
		if calls != 1 || err != rejected {
			t.Errorf("%s：证书被拒不该重试，calls=%d err=%v", name, calls, err)
		}
	}

	// 别的告警（比如对端内部出错）照常重试
	calls := 0
	_ = Probe(context.Background(), policy(nil), func(context.Context) error {
		calls++
		return &net.OpError{Op: "remote error", Err: errors.New("tls: internal error")}
	})
	if calls != 3 {
		t.Errorf("与证书无关的告警该照常重试，calls=%d", calls)
	}
}

// remoteAlert 握一次手：服务端要客户端证书、客户端不带，返回客户端读到的错误
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
	c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true}) // 只为拿到告警，不校验
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Read(make([]byte, 1)) // TLS 1.3 下服务端的拒绝在握手之后的第一次读才到
	return err
}
