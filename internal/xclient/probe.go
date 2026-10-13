package xclient

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"time"

	"github.com/jupiter-eng/xone/xutil"
)

// ProbePolicy 启动期建连探测怎么试：试几次、每次多久、第一次退避多久、哪些错误不必再试
type ProbePolicy struct {
	// Attempts 最多试几次，<1 按 1 次算
	Attempts int

	// Timeout 单次尝试的预算
	Timeout time.Duration

	// Interval 第一次退避的上界，之后逐次翻倍、带抖动，规则见 xutil.Retry
	Interval time.Duration

	// AuthFailed 服务端是否已经明确拒绝了这组凭证。可以留空，留空就是一律重试。
	//
	// 认得出来的不再重试：密码错了再试几次也是错，只是把同一个错误多等几轮退避
	// （默认最多 3s）才报出来，还会在服务端多留几条认证失败的记录。
	AuthFailed func(error) bool
}

// Probe 按 p 反复调用 fn，直到成功、认证被拒、试满次数或 ctx 被取消。
//
// fn 收到的 ctx 带着这一次尝试的截止时间，fn 要尊重它——Probe 不会替 fn 把
// 一个卡住的调用丢下不管：sql.DB 的 Close 会等在途的查询，丢下的协程并不会
// 因为连接池关了就返回。驱动本身不理 ctx 的（go-redis 只认截止时间、不认取消）
// 由调用方自己在 fn 里处理。
//
// 返回值：
//   - 成功时 nil；
//   - 认证被拒、或者 TLS 握手因为证书被拒时（见 tlsRejected），就是 fn 返回的那个错误，一次都不多试；
//   - 其余情况同 xutil.Retry：最后一次的错误，ctx 被取消时还同时满足 errors.Is(err, ctx.Err())。
//
// 调用方拿 p.AuthFailed 再问一次，就能决定报「认证失败」还是「连不上」。
func Probe(ctx context.Context, p ProbePolicy, fn func(context.Context) error) error {
	return xutil.Retry(ctx, p.Attempts, p.Timeout, p.Interval, func(ctx context.Context) error {
		err := fn(ctx)
		if err != nil && (tlsRejected(err) || p.AuthFailed != nil && p.AuthFailed(err)) {
			return xutil.Permanent(err)
		}
		return err
	})
}

// tlsRejected 握手因为证书被明确拒绝了：我们不认对端的证书（CA 不对、名字对不上、过期），
// 或者对端不认我们的（没带客户端证书、不是它认的 CA 签的）。
//
// 和密码错一样，再试几次还是同一张证书、同一个结论，所以不重试。
//
// 对端的拒绝是它发来的 TLS 告警。TCP 上 crypto/tls 把它包成 Op 为 "remote error" 的
// *net.OpError，里面的告警类型没有导出（tls.AlertError 只给 QUIC 用），所以按告警的文字认：
// bad_certificate、unknown_ca、certificate_required（TLS 1.3 下没带客户端证书时服务端发的）。
// 实测 go-redis v9.22.0 连 tls-auth-clients yes 的 Redis 7.0.15，不带证书是
// remote error: tls: certificate required，带着别的 CA 签的是 remote error: tls: unknown certificate authority。
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
