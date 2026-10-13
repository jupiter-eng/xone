// Package peer 是 xecho 和它的内置中间件之间传「直连的对端可不可信」用的约定。
//
// 放在 internal 里：这是框架内部两个包之间的事，使用者不需要、也不该碰它。
// 对端可不可信只有一个出处——XEcho.TrustedProxies，不另设开关。
package peer

// TrustedKey echo.Context 里的 key，值为 true 表示直连的对端在 XEcho.TrustedProxies 里。
//
// 只在可信时才写：读的一侧没写过就是 false，正好是安全的那一边——漏写只会少信，不会多信。
const TrustedKey = "xone/xecho.trusted_peer"
