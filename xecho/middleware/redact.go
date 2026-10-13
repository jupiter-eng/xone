package middleware

// 请求/响应里的敏感信息脱敏。实现和敏感词表在 internal/web，这里只是转过去。
// 词表进程里只有一张：这里加的词 xgin 也认，xgin/middleware 的同名函数加的这里也认。

import (
	"log/slog"
	"net/http"

	"github.com/jupiter-eng/xone/internal/web"
)

// Redacted 被遮掉的值统一写成这个
const Redacted = "***REDACTED***"

// AddSensitiveFields 追加敏感词，大小写与分隔符不敏感。
//
// 与默认词表同一条规则：字段名里含这个词就遮，body 和请求头都算。
// 比如加了 id_card，idCard、user_id_card、X-Id-Card 都会被遮掉。
func AddSensitiveFields(fields ...string) { web.AddSensitiveFields(fields...) }

// AddSensitiveHeaders 追加按名字精确遮掉的请求头，大小写不敏感。
//
// 用于名字里没有敏感词、内容却是凭证的头，比如 X-Tenant-Id。
// 名字里带敏感词的头不需要加，默认就遮。
func AddSensitiveHeaders(headers ...string) { web.AddSensitiveHeaders(headers...) }

// RedactBody 按 Content-Type 脱敏请求体
//
// Content-Type 先转小写：这个头按 RFC 9110 是大小写不敏感的，
// 照字面比的话 "Application/JSON" 就走不进 JSON 那一支。
//
// JSON 的判断是「含 json」而不是精确匹配 application/json：
// application/vnd.api+json、application/problem+json、text/json
// 都是 JSON，精确匹配会把它们整个漏过去。认错了也不会更糟——
// 解不出 JSON 的那一支本来就是整个遮掉。
func RedactBody(body []byte, contentType string) string { return web.RedactBody(body, contentType) }

// RedactHeaders 脱敏请求头，返回一个可以直接交给 slog 的值。
//
// 返回 slog.Value 而不是序列化好的字符串：交出字符串的话，slog 还要把它
// 当成一个普通字符串字段再转义一遍，日志里就成了
//
//	"请求头":"{\"Authorization\":[\"***REDACTED***\"]}"
//
// 检索时得先解一层字符串才能解 JSON。交出 slog.Value，序列化只发生一次，
// 这个字段在日志里就是一个正常的嵌套对象，顺带省掉了那次 json.Marshal
// （实测整条日志从 2777ns/19 allocs 降到 1630ns/9 allocs）。
//
// 多值的头拼成逗号分隔的一个字符串，而不是数组：同一个字段名在不同日志行里
// 忽而是字符串忽而是数组，日志系统建索引时会直接拒收。
//
// 两条规则，满足一条就遮：名字在名单里（默认名单 + AddSensitiveHeaders），
// 或者名字里带敏感词（与 body 字段同一张词表）。名单永远列不全——
// Proxy-Authorization 就曾经漏在外面——所以词表是兜底的那一层。
func RedactHeaders(h http.Header) slog.Value { return web.RedactHeaders(h) }
