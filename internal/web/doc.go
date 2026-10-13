// Package web 是 Web 集成（xgin、xecho）共用的、与框架无关的那一半。
//
// 一个 Web 集成要做的事里，有一大半跟用的是哪个框架没有关系：
//
//   - 起停 HTTP 服务：超时、h2c、TLS / 双向认证，优雅关闭时等在途的 handler 真正返回（Server）；
//   - 信任哪些代理：TrustedProxies 的解析、private 关键字、直连的对端可不可信（Proxies）；
//   - 脱敏：请求体、查询串、请求头里的凭证（RedactBody / RedactHeaders），敏感词表进程里只有一张；
//   - 访问日志：字段的名字、顺序和取值（AccessLog）。
//
// 这些语义本该处处一致。分散在每个集成里，就变成了几份会各自漂移的实现——
// 同一个服务换一个框架，client_ip 信不信、日志里有没有凭证、退出时等不等 handler，
// 答案都可能不一样。所以写在这里一次，各集成只做「从自己的框架里取值」那一层：
// 路由模板、状态码、写出的字节数怎么取，每个框架都不一样，由集成量好了当作普通的值交进来。
//
// 它是 internal 的：使用者看到的仍是各集成自己的 API（xgin.Config、xgin/middleware 的函数），
// 这里的名字不是使用者要学的东西。只依赖标准库和核心的包，核心模块因此仍然零三方依赖。
//
// 报错的模块名由集成给（Server.Module）：错误出自谁的 Start / Stop，就算谁报的，
// 调用方 xerror.Is(err, "xgin") 照样成立。不返回 xerror 的函数返回普通 error，
// 由集成在自己的边界上包一次。
package web
