package xkafka

import (
	"time"
	"unicode"
	"unicode/utf8"
)

// maxLoggedKey key 最多记这么多字节，超出的截掉并带上 key_truncated。规则同 xredis
const maxLoggedKey = 256

// keyAttrs 日志里的消息 key。
//
// 规则同 xredis 的 key：整条记，超过 maxLoggedKey 字节截断（不切断 UTF-8 字符），带上 key_truncated。
// Kafka 的 key 是任意字节，常常是二进制（Avro、protobuf、大端整数）：不是可打印的 UTF-8 的不记原文，
// 只记 key_len——写进 JSON 的话是一串转义乱码，还可能把控制字符带进日志平台。
// 没有 key（nil）的一个字段都不加。从不记 value。
func keyAttrs(key []byte) []any {
	if key == nil {
		return nil
	}
	if !printable(key) {
		return []any{"key_len", len(key)}
	}
	if len(key) <= maxLoggedKey {
		return []any{"key", string(key)}
	}
	cut := maxLoggedKey
	for back := 0; back < utf8.UTFMax-1 && !utf8.RuneStart(key[cut]); back++ {
		cut--
	}
	return []any{"key", string(key[:cut]), "key_truncated", true}
}

// printable 合法的 UTF-8，且每个字符都可打印（空格算，换行、制表符这类控制字符不算）
func printable(b []byte) bool {
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size <= 1 {
			return false
		}
		if !unicode.IsPrint(r) {
			return false
		}
		b = b[size:]
	}
	return true
}

// truncate 截到最多 n 字节，不切断 UTF-8 字符。只用在已经是合法 UTF-8 的文本上
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// ms 耗时换成毫秒，保留到微秒。字段名带单位：slog 的 JSON 把 Duration 写成纳秒整数
func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
