// Package peer 是 xgin 和它的内置中间件之间传「直连的对端可不可信」用的约定。
//
// 放在 internal 里：这是框架内部两个包之间的事，使用者不需要、也不该碰它。
// 对端可不可信只有一个出处——XGin.TrustedProxies，不另设开关。
package peer

// TrustedKey gin.Context 里的 key，值为 true 表示直连的对端在 XGin.TrustedProxies 里。
//
// 只在可信时才写：对端不可信的请求不必付一次写 gin.Context.Keys 的分配。
// 读的一侧没写过就是 false，正好是安全的那一边——漏写只会少信，不会多信。
const TrustedKey = "xone/xgin.trusted_peer"
