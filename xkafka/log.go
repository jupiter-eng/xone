package xkafka

import (
	"time"
	"unicode"
	"unicode/utf8"
)

// maxLoggedKey is how many bytes of a key are recorded at most; anything beyond is cut and key_truncated is added. Same rule as in xredis.
const maxLoggedKey = 256

// keyAttrs is the message key in the logs.
//
// Same rule as xredis's key: recorded whole, truncated beyond maxLoggedKey bytes (without cutting a UTF-8 character) with key_truncated added.
// A Kafka key is arbitrary bytes and often binary (Avro, protobuf, a big-endian integer): one that is not printable UTF-8 is not recorded verbatim,
// only its key_len -- in JSON it would be a string of escaped noise and might carry control characters into the logging platform.
// A message with no key (nil) adds no field at all. The value is never recorded.
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

// printable reports whether the text is valid UTF-8 and every character is printable (a space counts; a newline, a tab and other control characters do not).
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

// truncate cuts to at most n bytes without cutting a UTF-8 character. It is only used on text already known to be valid UTF-8.
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

// ms converts an elapsed time to milliseconds, keeping microseconds. The field name carries the unit: slog's JSON writes a Duration as an integer number of nanoseconds.
func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
