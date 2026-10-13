package xtrace

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"

	"go.opentelemetry.io/otel/propagation"

	"github.com/jupiter-eng/xone/xerror"
)

// context key
type (
	forwardHeadersKey struct{}
	targetHostKey     struct{}
)

// headerRule 规范化之后的域名规则
type headerRule struct {
	domains []string // 小写域名模式，支持 *.example.com
	headers []string // 规范化的 header 名
}

// HeaderPropagator 透传指定的自定义 HTTP Header（如 X-Request-Id、X-Tenant-Id）。
//
// 它实现 propagation.TextMapPropagator，所以一旦装进全局 Propagator，
// otelhttp 之类的组件会自动带上这些 Header，无需业务代码参与。
//
// 两种模式：全局透传发给所有下游；域名规则只发给匹配的下游。
// 两种都只收可信对端发来的值，见 Extract。
type HeaderPropagator struct {
	globalHeaders []string
	rules         []headerRule
	allHeaders    []string // 全局 + 规则去重，Extract 和 Fields 用

	// warned 不可信的对端发来透传 Header 时，只告警一次
	warned atomic.Bool
}

// NewHeaderPropagator 创建 HeaderPropagator。
//
// header 名按 http.CanonicalHeaderKey 规范化，域名转小写。
// 域名或 header 为空的规则整条丢弃。
//
// 域名只认两种写法：精确的 api.example.com，和通配的 *.example.com。
// 其余带 * 的写法（*example.com、a.*.com、*）直接报错，理由见 checkDomain。
//
// 同一个 header 同时出现在 globalHeaders 和某条域名规则里会直接报错：
// 那是一份自相矛盾的配置——一边说发给所有人，一边说只发给这些人。
// 猜哪边为准都可能把内部标识发给第三方，所以让它在启动时就停下。
func NewHeaderPropagator(globalHeaders []string, rules []ForwardHeaderRule) (*HeaderPropagator, error) {
	p, err := newHeaderPropagator(globalHeaders, rules)
	if err != nil {
		return nil, xerror.New("xtrace", "config", err)
	}
	return p, nil
}

// newHeaderPropagator 同 NewHeaderPropagator，返回普通 error：
// Config.Validate 也用它查透传配置，那一层的错误由调用方包
func newHeaderPropagator(globalHeaders []string, rules []ForwardHeaderRule) (*HeaderPropagator, error) {
	normalizedRules, err := normalizeRules(rules)
	if err != nil {
		return nil, fmt.Errorf("invalid ForwardHeaderRules: %w", err)
	}

	restricted := make(map[string]struct{})
	for _, r := range normalizedRules {
		for _, h := range r.headers {
			restricted[h] = struct{}{}
		}
	}

	global := canonicalHeaders(globalHeaders)
	var conflicts []string
	for _, h := range global {
		if _, limited := restricted[h]; limited {
			conflicts = append(conflicts, h)
		}
	}
	if len(conflicts) > 0 {
		slices.Sort(conflicts)
		return nil, fmt.Errorf("header %s appears in both ForwardHeaders and ForwardHeaderRules; "+
			"the former sends it to every domain, the latter only to the listed ones — remove one of them",
			strings.Join(conflicts, ", "))
	}

	return &HeaderPropagator{
		globalHeaders: global,
		rules:         normalizedRules,
		allHeaders:    mergeHeaders(global, normalizedRules),
	}, nil
}

// normalizeRules 规范化域名规则，域名或 header 为空的规则整条丢弃
func normalizeRules(rules []ForwardHeaderRule) ([]headerRule, error) {
	out := make([]headerRule, 0, len(rules))
	for _, r := range rules {
		domains, err := normalizeDomains(r.Domains)
		if err != nil {
			return nil, err
		}
		headers := canonicalHeaders(r.Headers)
		if len(domains) == 0 || len(headers) == 0 {
			continue
		}
		out = append(out, headerRule{domains: domains, headers: headers})
	}
	return out, nil
}

// mergeHeaders 汇总去重，保持稳定顺序：全局在前，规则在后
func mergeHeaders(global []string, rules []headerRule) []string {
	all := make([]string, 0, len(global))
	seen := make(map[string]struct{}, len(global))
	add := func(h string) {
		if _, ok := seen[h]; ok {
			return
		}
		seen[h] = struct{}{}
		all = append(all, h)
	}
	for _, h := range global {
		add(h)
	}
	for _, r := range rules {
		for _, h := range r.headers {
			add(h)
		}
	}
	return all
}

// canonicalHeaders 规范化 header 名并丢弃空项
//
// 不做 TrimSpace：header 名本就不允许含空格，CanonicalHeaderKey 遇到非法字符
// 会原样返回，trim 掉反而把一个明显的配置错误悄悄改对了。
func canonicalHeaders(headers []string) []string {
	out := make([]string, 0, len(headers))
	for _, h := range headers {
		if h != "" {
			out = append(out, http.CanonicalHeaderKey(h))
		}
	}
	return out
}

func normalizeDomains(domains []string) ([]string, error) {
	out := make([]string, 0, len(domains))
	for _, d := range domains {
		if d = strings.TrimSpace(d); d == "" {
			continue
		}
		if err := checkDomain(d); err != nil {
			return nil, err
		}
		out = append(out, strings.ToLower(d))
	}
	return out, nil
}

// checkDomain 通配只允许出现在最前面、并且紧跟一个点：*.example.com。
//
// 原先的匹配是「切掉开头的 * 再比后缀」，于是 *trusted.com 也能匹配
// eviltrusted.com——一个谁都能注册的域名拿到了内部令牌。*、a.*.com 这类写法
// 同样说不清想匹配什么。这是一份决定内部凭证发给谁的配置，看不懂的就不猜，
// 启动时直接停下。
func checkDomain(d string) error {
	if rest, _ := strings.CutPrefix(d, "*."); rest == "" || strings.Contains(rest, "*") {
		return fmt.Errorf("domain pattern %q is not supported, use an exact domain (api.example.com) or a leading *. wildcard (*.example.com)", d)
	}
	return nil
}

// Extract 从上游请求里读出所有配置的 Header，存进 context。
//
// 不区分域名：进来的东西先收下，发给谁是 Inject 的事。
//
// 但区分来源：只收可信对端发来的值。透传是「把上游给的值原样带给下游」，
// 上游是谁就是信任边界——照单全收的话，公网客户端发一个 X-Tenant-Id /
// X-Internal-Token，就被当成自己人给的，带进内网的每一次调用。
//
// 本包不知道对端是谁，知道的是接请求的那一层，所以由 carrier 来说：
// 它实现
//
//	TrustedPeer() bool
//
// 并返回 true，才算可信。xgin / xecho 的链路中间件按各自的 TrustedProxies 判断直连的
// 对端，可信时交来的 carrier 带着这个方法。没有它的 carrier（otelhttp 的 handler、
// 业务自己拼的 MapCarrier）一律当作不可信：拿不准就不收。
// 从消息队列之类确实可信的来源取值时，给 carrier 加上这个方法即可。
func (p *HeaderPropagator) Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	if len(p.allHeaders) == 0 {
		return ctx
	}
	if !fromTrustedPeer(carrier) {
		p.warnUntrusted(carrier)
		return ctx
	}

	existing := forwardHeadersRaw(ctx)
	var merged map[string]string
	for _, h := range p.allHeaders {
		v := carrier.Get(h)
		if v == "" {
			continue
		}
		if merged == nil {
			merged = make(map[string]string, len(p.allHeaders)+len(existing))
			maps.Copy(merged, existing)
		}
		merged[h] = v
	}

	if merged == nil {
		return ctx
	}
	return context.WithValue(ctx, forwardHeadersKey{}, merged)
}

// fromTrustedPeer carrier 是否声明了「发来这些值的对端可信」，见 Extract
func fromTrustedPeer(c propagation.TextMapCarrier) bool {
	t, ok := c.(interface{ TrustedPeer() bool })
	return ok && t.TrustedPeer()
}

// warnUntrusted 不可信的对端带来了透传 Header 时告警一次。
//
// 这是「配了透传却什么都没传」时唯一的线索：升级之前这些值是照单全收的。
// 只告警一次——公网上谁都能发这些头，每次都打就成了一个刷日志的入口。
func (p *HeaderPropagator) warnUntrusted(c propagation.TextMapCarrier) {
	if p.warned.Load() {
		return
	}
	for _, h := range p.allHeaders {
		if c.Get(h) != "" {
			if p.warned.CompareAndSwap(false, true) {
				slog.Warn("xtrace ignored forward headers from an untrusted peer, only peers in the web server's TrustedProxies (XGin / XEcho) are trusted",
					"header", h)
			}
			return
		}
	}
}

// trustedBaggage W3C baggage，只收可信对端发来的。
//
// baggage 和透传 Header 是同一种东西：上游给的键值原样带给下游、进每一次调用。
// OTel 自带的 propagation.Baggage 谁发来的都收，于是 ForwardHeaders 挡在门外的
// X-Tenant-Id，公网客户端改写成 baggage: tenant=… 照样被当成自己人给的、带进内网。
// 所以它和 HeaderPropagator.Extract 认同一个记号：carrier 实现 TrustedPeer() 并返回 true。
//
// 注入不设限：ctx 里的 baggage 要么来自可信的上游，要么是本进程自己写的。
type trustedBaggage struct {
	propagation.Baggage

	// warned 不可信的对端发来 baggage 时，只告警一次
	warned atomic.Bool
}

// Extract 可信对端发来的才收，见 trustedBaggage
func (b *trustedBaggage) Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	if !fromTrustedPeer(carrier) {
		// 只告警一次，理由同 warnUntrusted
		if carrier.Get(baggageHeader) != "" && b.warned.CompareAndSwap(false, true) {
			slog.Warn("xtrace ignored baggage from an untrusted peer, only peers in the web server's TrustedProxies (XGin / XEcho) are trusted")
		}
		return ctx
	}
	return b.Baggage.Extract(ctx, carrier)
}

// baggageHeader W3C baggage 的请求头
const baggageHeader = "baggage"

// Inject 把 context 里的透传值写进下游请求。
//
// 全局 header 无条件注入；规则 header 只在目标域名匹配时注入。
// 目标域名由 Transport 写进 context——没有它，域名规则一条都不会命中。
func (p *HeaderPropagator) Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	if len(p.allHeaders) == 0 {
		return
	}
	vals := forwardHeadersRaw(ctx)
	if len(vals) == 0 {
		return
	}

	for _, h := range p.globalHeaders {
		if v := vals[h]; v != "" {
			carrier.Set(h, v)
		}
	}

	if len(p.rules) == 0 {
		return
	}
	host := TargetHostFromContext(ctx)
	if host == "" {
		return
	}
	for _, rule := range p.rules {
		if !matchDomains(host, rule.domains) {
			continue
		}
		for _, h := range rule.headers {
			if v := vals[h]; v != "" {
				carrier.Set(h, v)
			}
		}
	}
}

// Fields 返回本 Propagator 管理的全部 Header（拷贝）
func (p *HeaderPropagator) Fields() []string { return slices.Clone(p.allHeaders) }

// matchDomains 判断 host 是否匹配任一域名模式，支持精确匹配和 *.example.com。
//
// *.example.com 匹配任意层级的子域（a.example.com、a.b.example.com），
// 但不匹配 example.com 自身：想连裸域一起，就把 example.com 也写上。
// 模式在构造时已经过 checkDomain，带 * 的一定是 *. 开头。
func matchDomains(host string, patterns []string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host // 没带端口
	}
	h = strings.ToLower(h)

	for _, pattern := range patterns {
		if parent, ok := strings.CutPrefix(pattern, "*."); ok {
			// 比的是「. + 父域」，所以 eviltrusted.com 对不上 *.trusted.com
			if strings.HasSuffix(h, "."+parent) {
				return true
			}
			continue
		}
		if h == pattern {
			return true
		}
	}
	return false
}

// WithTargetHost 把目标请求的 Host 写进 context，供域名规则过滤使用
func WithTargetHost(ctx context.Context, host string) context.Context {
	return context.WithValue(ctx, targetHostKey{}, host)
}

// TargetHostFromContext 取出目标请求的 Host，没有则为空字符串
func TargetHostFromContext(ctx context.Context) string {
	host, _ := ctx.Value(targetHostKey{}).(string)
	return host
}

func forwardHeadersRaw(ctx context.Context) map[string]string {
	m, _ := ctx.Value(forwardHeadersKey{}).(map[string]string)
	return m
}

// ForwardHeadersFromContext 取出全部透传的 Header 键值对（拷贝）
func ForwardHeadersFromContext(ctx context.Context) map[string]string {
	return maps.Clone(forwardHeadersRaw(ctx)) // Extract 只存非空的 map，没有时是 nil
}

// ForwardHeaderFromContext 取出指定 Header 的值，大小写不敏感
func ForwardHeaderFromContext(ctx context.Context, key string) string {
	return forwardHeadersRaw(ctx)[http.CanonicalHeaderKey(key)]
}
