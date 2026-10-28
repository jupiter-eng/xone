package web

// Redacting the sensitive information in a request or a response.
//
// Every decision here has to be "when in doubt, treat it as sensitive". Redaction fails
// asymmetrically: one field masked too many is a little less to go on while tracking something
// down, and one missed is a password in the logs that nobody will ever notice.

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Redacted is what every masked value is written as
const Redacted = "***REDACTED***"

// The default sensitive words.
//
// They match by "contains", not by "equals": both sides have their case and their separators
// (_ - . space) removed before the comparison, so password recognizes new_password, old-password
// and user.password, and token recognizes sessionToken and X-Csrf-Token. It used to be an exact
// match and all of those reached the logs as they were.
//
// The words have to be specific enough: auth would hit author, key would hit primary_key and
// Idempotency-Key, and a short word such as pwd would collide inside a random string and mask a
// whole plain-text body -- which is why whole words such as apikey and authorization are written
// here. Within that bound, a word in doubt is better included: masking too much is a little less to
// go on, and missing one is a credential on disk.
var defaultWords = []string{
	"password", "passwd", "secret", "token", "authorization",
	"apikey", "accesskey", "privatekey", "credential", "cookie", "session", "signature",
}

// The request headers masked by an exact name match by default.
//
// Their names carry a sensitive word to begin with and the word list recognizes them already; they
// are listed separately so that their safety does not hang on the word list -- should somebody
// narrow it one day, these stay masked.
var defaultHeaders = []string{
	"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie", "X-Api-Key", "X-Auth-Token",
}

// There is one word list and one header list per process: the AddSensitiveFields /
// AddSensitiveHeaders each web integration exposes (xgin/middleware) all forward to here. With one
// copy per integration, a word a caller added through xgin would mean nothing to the same service
// on another framework.
//
// Both are copy-on-write: appending replaces the whole of it with a new one, what was already
// handed out is never modified again, and a reader holding one needs no lock.
var (
	mu         sync.RWMutex
	fieldWords = normalizeAll(defaultWords) // the sensitive words once normalized: the defaults + AddSensitiveFields
	headerSet  = lowerSet(defaultHeaders)   // the lowercased header list: the defaults + AddSensitiveHeaders
)

// AddSensitiveFields adds sensitive words, insensitive to case and separators.
//
// The rule is the default list's: a field name containing the word is masked, in a body and in a
// request header alike.
// Adding id_card, for one, masks idCard, user_id_card and X-Id-Card.
func AddSensitiveFields(fields ...string) {
	mu.Lock()
	defer mu.Unlock()
	// After Clip, append always starts a new backing array and never writes into the one somebody
	// else holds
	fieldWords = append(slices.Clip(fieldWords), normalizeAll(fields)...)
}

// AddSensitiveHeaders adds request headers masked by an exact name match, insensitive to case.
//
// It is for a header whose name holds no sensitive word while its content is a credential, such as
// X-Tenant-Id.
// A header whose name does hold one needs no adding and is masked by default.
func AddSensitiveHeaders(headers ...string) {
	mu.Lock()
	defer mu.Unlock()
	set := maps.Clone(headerSet)
	for _, h := range headers {
		set[strings.ToLower(h)] = true
	}
	headerSet = set
}

// words takes the sensitive words once normalized. What comes back never changes again and is
// read-only
func words() []string {
	mu.RLock()
	defer mu.RUnlock()
	return fieldWords
}

// headers takes the lowercased header list. What comes back never changes again and is read-only
func headers() map[string]bool {
	mu.RLock()
	defer mu.RUnlock()
	return headerSet
}

// normalizeAll normalizes them one by one and drops whatever normalizes to empty (a word of
// nothing but separators would match everything)
func normalizeAll(fields []string) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if n := normalize(f); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// lowerSet turns them into a lowercased set
func lowerSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, h := range names {
		set[strings.ToLower(h)] = true
	}
	return set
}

// isSeparator are the characters ignored when field names are compared
func isSeparator(c byte) bool { return c == '_' || c == '-' || c == '.' || c == ' ' }

// normalize folds the case and removes the separators: New_Password, new-password and newPassword
// all normalize to newpassword
func normalize(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if r >= utf8.RuneSelf || !isSeparator(byte(r)) {
			b.WriteRune(foldRune(r))
		}
	}
	return b.String()
}

// foldRune takes the representative of r's case-folding equivalence class, so that two characters
// fold to the same value exactly when encoding/json considers them the same character.
//
// ASCII alone will not do: encoding/json matches field names by Unicode folding, and
// {"ſecret":...} (a long s, U+017F) was measured binding to Secret and {"toKen":...} (the Kelvin
// sign, U+212A) binding to Token. Lowercased as ASCII alone, neither key is recognized in the word
// list, the body takes the fast path and reaches the log as it is -- while the field the server
// read is exactly that one.
//
// The representative is the smallest character in the equivalence class (which is how
// encoding/json takes it internally), lowercased afterwards when it is an uppercase ASCII letter,
// so that the normalized word list still reads as something a person can follow
func foldRune(r rune) rune {
	if r >= utf8.RuneSelf {
		// SimpleFold cycles through the equivalence class in ascending order, and the first one that
		// goes down rather than up is the smallest
		next := unicode.SimpleFold(r)
		for next > r {
			r, next = next, unicode.SimpleFold(next)
		}
		r = next
	}
	if 'A' <= r && r <= 'Z' {
		r += 'a' - 'A'
	}
	return r
}

// sensitive reports whether a field name is sensitive: it contains one of the sensitive words once
// normalized.
// The body pre-check uses it as well, treating the whole body as one "field name", so the two have
// one rule between them.
//
// The price was measured: RedactHeaders over five headers went from 780ns/7 allocs with an exact
// match to 1460ns/11 allocs (one normalization per header). A hand-written byte-by-byte scan was
// tried to save that allocation and turned out far slower -- strings.Contains beats a hand-written
// loop by a wide margin. The body pre-check used to be written that way: every byte position
// compared against every word, with every non-ASCII position decoded and folded each time. For a
// 230-byte JSON holding a few multi-byte characters, RedactBody took 23µs, and 256KB near the upper
// bound took 13ms (pure ASCII) / 46ms (with multi-byte text); normalizing once and then calling
// Contains brings that to 2.4µs and 2.5ms / 2.9ms, at the price of one allocation the size of the
// body.
func sensitive(name string, ws []string) bool {
	n := normalize(name)
	return slices.ContainsFunc(ws, func(w string) bool { return strings.Contains(n, w) })
}

// newlines removes the line breaks, so that one log does not stretch over several lines.
//
// Only the single bytes \r and \n are written: deleting \r\n byte by byte gives the same result.
// Written as single bytes, Replacer takes its byte-by-byte implementation, which returns the input
// unchanged and allocates nothing when there is no line break; adding a \r\n would fall back to the
// general implementation, which copies twice every time
var newlines = strings.NewReplacer("\r", "", "\n", "")

// RedactBody redacts a request body as its Content-Type says.
//
// The Content-Type is lowercased first: the header is case-insensitive under RFC 9110, and compared
// literally "Application/JSON" would never reach the JSON branch.
//
// JSON is decided by "contains json" rather than an exact application/json:
// application/vnd.api+json, application/problem+json and text/json are all JSON, and an exact match
// would let them all through. Deciding wrong makes nothing worse -- the branch that cannot parse
// JSON masks the whole thing anyway.
//
// Another type (text/plain, or none at all) that is valid JSON itself is redacted as JSON too:
// gin's ShouldBindJSON ignores the Content-Type and the server reads such a body as JSON all the
// same. On the plain-text branch only a literal scan happens, a key name written as a \u escape is
// not recognized as password, and the password reaches the log as it is
func RedactBody(body []byte, contentType string) string {
	return redactBody(body, contentType, false)
}

// redactBody is RedactBody, with cut saying the access log had to cut this body short at
// maxRequestBody / MaxResponseBody. Such a body never takes redactJSON's fast path, and on the form branch the
// one key/value pair the cut landed in is dropped (see redactForm).
//
// Every test that path makes assumes a complete body, and the cut can take away the very character
// one of them goes by. Measured: {"dsn":"postgres://app:pw@db:5432/prod"} cut before the @ holds no
// sensitive word and no @ any more, so it took the fast path and the password was written into the
// log in the clear -- and the parsing branch would not have masked it either, since the regexps
// that find a DSN need that @ as well. Sent to the parser instead, the cut leaves the string value
// unterminated, so it is dropped rather than recorded (see decodeJSON).
//
// What this costs a complete body is nothing: cut is false for every one of them, and the fast path
// is reached exactly as before.
func redactBody(body []byte, contentType string, cut bool) string {
	if len(body) == 0 {
		return ""
	}
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "json"):
		return redactJSON(body, cut)
	case strings.Contains(ct, "x-www-form-urlencoded"):
		return redactForm(string(body), cut)
	case isJSON(body):
		return redactJSON(body, cut)
	default:
		return redactOpaque(body)
	}
}

// isJSON reports whether a body is a JSON object, array or string, see RedactBody.
//
// The first non-whitespace character is looked at before json.Valid: a plain-text body that does
// not parse has json.Valid build a SyntaxError, measured taking a 56-byte piece of plain text from
// 2 allocations to 6 and about 300ns slower. A scalar such as a number or true holds nothing to
// mask either
func isJSON(body []byte) bool {
	b := bytes.TrimLeft(body, " \t\r\n")
	return len(b) > 0 && (b[0] == '{' || b[0] == '[' || b[0] == '"') && json.Valid(b)
}

// RedactText redacts a piece of text with no structure, such as an error a handler returned or a
// panicked value. There are three rules:
//
//   - a sensitive name followed by = or : has its value masked and the rest of the text is kept:
//     "login failed for user=x password=y" keeps everything up to password= and masks what comes
//     after it. Masking the whole text on a sensitive word alone would take "token expired",
//     "session not found" and "invalid signature" with it -- the messages wanted most while
//     tracking an authentication failure down, and all of them carrying no credential at all. The
//     price is that a credential written into a sentence with no name in front of it ("bad token
//     abc123") is kept: a name is what makes a value locatable, and there is none there. An
//     unstructured body is a different matter and is still masked whole, see redactOpaque;
//   - the password in a URL's or a MySQL DSN's userinfo is masked and the rest is kept: a driver's
//     error loves carrying a whole DSN -- dial postgres://app:pw@db:5432/prod,
//     app:pw@tcp(db:3306)/prod -- which holds not one sensitive word and would reach the log as it
//     is by the word list alone;
//   - a line break becomes "; " and a trailing one is removed: gin's c.Errors.String() puts one
//     error per line, and removing the line breaks as a body does would glue two errors into one
//     sentence.
//
// The password is written as XONE_DEBUG writes it when printing the configuration
// (internal/config's redactString): the user name and the host are kept and only the password is
// replaced, so which database and which account are visible at a glance.
// What it is replaced with is this package's Redacted, so that everything masked in one line of
// access log carries the same marker
func RedactText(s string) string {
	if s == "" {
		return ""
	}
	// The word list over the whole text is the gate: a name inside it is a substring of it, so with
	// no sensitive word anywhere there is no sensitive name to find either and the scan below is
	// skipped. One normalization of the text pays for every name in it
	if ws := words(); sensitive(s, ws) {
		s = redactAssignments(s, ws)
	}
	return textNewlines.Replace(strings.TrimRight(redactCredentials(s), "\r\n"))
}

// nameSeparator finds a "<name>=" or "<name>:" inside free text, with the quotes JSON puts around a
// key allowed. Only the name and the separator are matched; how far the value reaches is
// redactAssignments' decision.
//
// The separator may be surrounded by spaces and tabs but not by line breaks: a name at the end of a
// line has its value on the next one, which this cannot see, and reading across the break would
// mask a whole second message instead.
var nameSeparator = regexp.MustCompile(`["']?([A-Za-z0-9_.\-]+)["']?[ \t]*[:=][ \t]*`)

// redactAssignments masks the value of every "<name>=<value>" whose name is a sensitive one and
// leaves the rest of the text alone. ws is the sensitive word list, already normalized.
//
// The value runs to the end of its line rather than to the next space: a request header dumped into
// an error (Authorization: Bearer eyJ...) carries its credential in the second word, and stopping
// at the first space would write that word out in full. What comes before the name is kept, which
// is usually the sentence saying what went wrong.
//
// A name with nothing after it on that line is passed over: there is no value to mask, and masking
// would only eat the line break that keeps two errors apart.
func redactAssignments(s string, ws []string) string {
	ms := nameSeparator.FindAllStringSubmatchIndex(s, -1)
	if ms == nil {
		return s
	}

	var b strings.Builder
	last := 0 // how much of s has been written out; 0 means nothing has been masked yet
	for _, m := range ms {
		// The matches do not overlap, but one can still start inside a value already masked
		if m[0] < last || !sensitive(s[m[2]:m[3]], ws) {
			continue
		}
		end := strings.IndexAny(s[m[1]:], "\r\n")
		if end < 0 {
			end = len(s)
		} else {
			end += m[1]
		}
		if end == m[1] {
			continue
		}
		if last == 0 {
			b.Grow(len(s) + len(Redacted))
		}
		b.WriteString(s[last:m[1]])
		b.WriteString(Redacted)
		last = end
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// redactCredentials masks the password in a URL's or a MySQL DSN's userinfo and nothing else:
// RedactText's second rule.
// A header value goes through this one alone and through no name-based rule -- the words in
// Vary: Cookie and Access-Control-Allow-Headers: Authorization are header names and not
// credentials, and a sensitive header name is masked by name through the list and the word list.
//
// Neither DSN spelling exists without an @, so with no @ the regexps are not run. Every header and
// every JSON string value passes through here and the vast majority carry no @, which saves two
// regexp scans and one copy
func redactCredentials(s string) string {
	if strings.IndexByte(s, '@') < 0 {
		return s
	}
	s = urlUserinfo.ReplaceAllString(s, "$1:"+Redacted+"@")
	return mysqlUserinfo.ReplaceAllString(s, "$1:"+Redacted+"@$2")
}

// Credentials hiding inside a piece of text. The rules are internal/config's urlUserinfo / mysqlDSN
// with the anchor at the start removed: there a configuration value is a whole DSN, here it is one
// piece of a sentence.
//
// The password goes by the last @ (a greedy match backing off), as net/url and
// go-sql-driver/mysql parse a DSN: an unescaped @ inside a password leaves no half of it in the log.
// The price is masking a little too much now and then -- when in doubt, treat it as sensitive
var (
	urlUserinfo   = regexp.MustCompile(`(://[^:/?#@\s]*):[^/\s]+@`)        // postgres://u:p@h, redis://:p@h
	mysqlUserinfo = regexp.MustCompile(`([^\s:@/]+):\S+@((?:tcp|unix)\()`) // u:p@tcp(h:3306)/db, u:p@unix(/sock)/db
)

// textNewlines is how RedactText handles line breaks: \n becomes "; " and \r is removed (so \r\n is
// one "; " as well).
// As in newlines, only single-byte keys are written, which takes the byte-by-byte implementation
var textNewlines = strings.NewReplacer("\r", "", "\n", "; ")

// redactOpaque handles a body whose structure is not recognized (text/plain, xml, one with no
// Content-Type and so on).
//
// No single field can be located, while "does it hold a sensitive field name" can still be seen. If
// it does, the whole of it is masked.
// This is the same rule as the JSON branch applies when parsing fails: what cannot be located
// cannot be let through. It is deliberately stricter than RedactText, which keeps the sentence
// around a located value: a body is data somebody sent, whose shape nobody here knows, while an
// error or a panic is a sentence this codebase wrote and the part worth reading is the sentence.
//
// Only a literal scan happens here, and unlike JSON a backslash is not treated as suspicious on
// sight: a backslash is JSON's escape syntax, while in plain text it is an ordinary character, and
// a log holding a Windows path should not be masked whole because of it.
func redactOpaque(body []byte) string {
	s := string(body)
	if sensitive(s, words()) {
		return Redacted
	}
	// No sensitive field name, while it may still carry a URL / DSN credential
	// (postgres://u:p@h), as RedactText does
	return newlines.Replace(redactCredentials(s))
}

// redactJSON parses the JSON and masks the sensitive fields, putting every string value through
// RedactText as well (a whole DSN hiding inside a value).
//
// A structural limit: it goes by key names alone. In an array of "name/value pairs" such as
// [{"name":"password","value":"x"}], what is sensitive is name's value with the password in value,
// and neither key name holds a sensitive word -- so both reach the log as they are. A body of that
// shape is masked by adding a key such as value to the word list with AddSensitiveFields, or by not
// recording the body at all.
//
// Decoding uses UseNumber and encoding turns HTML escaping off: the default any decodes a number as
// a float64 and writes 12345678901234567890 back out as 12345678901234567000, while the default
// encoder writes < > & as backslash-u escape sequences. Either one puts the value in the log out of
// step with the one in the request.
//
// A body that does not parse to the end is recorded as far as it was read, with truncatedSuffix
// after it; the reasoning is in decodeJSON.
func redactJSON(body []byte, cut bool) string {
	ws := words()
	s := string(body)
	// An @ means it has to be parsed too: a value may hold a DSN (postgres://u:p@h, u:p@tcp(h)),
	// see RedactText. A body that may have been cut short skips all of this, see redactBody
	if !cut && !mayContainField(s, ws) && strings.IndexByte(s, '@') < 0 {
		return newlines.Replace(s)
	}

	data, whole := decodeJSON(body)
	if data == nil {
		// Not one value came out, so there is nothing to mask field by field and nothing worth
		// printing either. It is not Redacted: that marker means "a sensitive field was found and
		// masked", and reading it here would send somebody looking for a credential that was never
		// located
		return unparseableOmitted
	}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(redactValue(data, ws)); err != nil {
		return Redacted
	}
	redacted := strings.TrimSuffix(out.String(), "\n") // Encode always adds a line break at the end
	if whole {
		return redacted
	}
	return redacted + truncatedSuffix
}

// What a body that could not be read to the end is marked with. The vocabulary is the one the
// access log already uses for "this was deliberately not recorded" ([binary content omitted] and
// the rest), rather than Redacted, which everywhere else means a sensitive field was masked.
const (
	truncatedSuffix    = " [truncated, the rest was not recorded]"
	unparseableOmitted = "[unparseable content omitted]"
)

// decodeJSON decodes body and says whether that was the whole of it. A nil value means not one
// value could be read.
//
// A body the access log records is usually not the whole body: the response is captured up to
// MaxResponseBody and the request up to maxRequestBody, so a JSON object larger than that arrives
// here cut in the middle of whatever field happened to be there. Handing the whole of it to Decode
// and masking everything on failure made that case indistinguishable from a credential being found
// -- and since the cut is the framework's own doing, every large JSON body read as one. So as much
// as the decoder can read is read, member by member.
//
// What this must never do is let an unread byte through. Everything returned came out of the
// decoder as a resolved token, so a key whose letters were written as \uXXXX escapes is the plain
// string by the time redactValue compares it against the word list, exactly as in a body that
// parsed whole; everything from the first error onwards is dropped rather than printed. That is
// what keeps the backslash rule in mayContainField meaningful (see there).
func decodeJSON(body []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err == nil {
		// Trailing content counts as "not the whole of it" as well: a complete value followed by
		// something else (a raw dump appended to a response) keeps the value and drops the rest
		return v, dec.Decode(new(any)) == io.EOF
	}

	dec = json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	return partialValue(dec)
}

// partialValue reads one value and says whether it was read whole.
//
// It goes down rather than stopping at the top level: the common shape of a response is an envelope
// around a list ({"page":1,"items":[...]}), where the cut falls inside the list and taking that
// member as unreadable would leave the whole body unreadable with it.
//
// How deep it may recurse is encoding/json's business, whose decoder gives up past its own nesting
// limit; measured, 20000 levels of [ come back as "nothing could be read" rather than running out of
// stack.
func partialValue(dec *json.Decoder) (any, bool) {
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	switch tok {
	case json.Delim('{'):
		out := map[string]any{}
		for dec.More() {
			key, err := dec.Token()
			// An object key is always a string token, and the assertion is checked all the same:
			// this runs in the access log's defer, which sits outside the Recover middleware, so a
			// panic here would drop the connection and write no log at all
			name, ok := key.(string)
			if err != nil || !ok {
				return nonEmpty(out), false
			}
			v, whole := partialValue(dec)
			// whole rather than v != nil alone: a value that really is null comes back as a nil
			// any, and going by nil would drop the field out of the log altogether -- while a nil
			// with whole false is a container cut before a single member came out, which there is
			// nothing to record for
			if whole || v != nil {
				out[name] = v
			}
			if !whole {
				return nonEmpty(out), false
			}
		}
		if _, err := dec.Token(); err != nil { // the closing brace
			return nonEmpty(out), false
		}
		return out, true
	case json.Delim('['):
		out := []any{}
		for dec.More() {
			v, whole := partialValue(dec)
			if whole || v != nil { // a null element, see the object branch above
				out = append(out, v)
			}
			if !whole {
				return nonEmpty(out), false
			}
		}
		if _, err := dec.Token(); err != nil { // the closing bracket
			return nonEmpty(out), false
		}
		return out, true
	}
	return tok, true // a scalar, which Token hands over only once it is complete
}

// nonEmpty is v, or nil when nothing came out of it: a container cut before a single member was
// read holds nothing worth printing, and an empty {} in the log would read as a response that
// really was empty.
func nonEmpty[T map[string]any | []any](v T) any {
	if len(v) == 0 {
		return nil
	}
	return v
}

// mayContainField reports whether a sensitive field name may appear inside a body.
//
// It returns false only when it is certain none is there -- its job is to skip the heavy path of
// "parse, walk and re-serialize", not to make a security decision, so anything in doubt returns
// true.
//
// A backslash is the hard dividing line: JSON allows \uXXXX inside a string, so the raw bytes of
//
//	{"password": "hunter2"}
//
// need not hold the substring password at all. The previous version did a literal scan alone and
// such a body took the fast path and reached the log as it was -- the password on disk in plaintext
// with not one sign of it. So a backslash alone hands it to the parser, which turns the escapes
// back into the real keys.
//
// The literal scan uses the very sensitive that compares field names: the same folding and the same
// separators ignored, so api_key matches apikey and {"ſecret":...} matches secret.
func mayContainField(body string, ws []string) bool {
	return strings.IndexByte(body, '\\') >= 0 || sensitive(body, ws)
}

// redactValue masks the whole of a value under a sensitive key name, puts a string value through
// RedactText, and returns the redacted v (maps and slices are changed in place)
func redactValue(v any, ws []string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if sensitive(k, ws) {
				t[k] = Redacted
				continue
			}
			t[k] = redactValue(val, ws)
		}
	case []any:
		for i, item := range t {
			t[i] = redactValue(item, ws)
		}
	case string:
		return RedactText(t)
	}
	return v
}

// redactForm redacts a form-urlencoded request body.
//
// url.ParseQuery rather than cutting on & and =: a form's keys are percent-encoded, so
//
//	p%61ssword=hunter2
//
// cut literally gives the key p%61ssword, which does not match password and reaches the log all the
// same. Parsing it first and comparing afterwards is what makes it the same key the server really
// read.
func redactForm(body string, cut bool) string {
	ws := words()

	// A body the access log cut short ends in whatever key/value pair happened to be there, and that pair is
	// the one the rules below cannot be trusted on -- redactJSON's reason in the encoding a form uses, see
	// redactBody. Measured: dsn=postgres%3A%2F%2Fapp%3Apw%40db%3A5432%2Fprod cut before the %40 decodes to
	// postgres://app:pw, which holds no sensitive word and no @ any more, so redactCredentials finds nothing to
	// mask and the password is written into the log in the clear. Only the last pair can be incomplete, so it is
	// dropped rather than recorded, which is what decodeJSON does with an unterminated string value, and what is
	// left says so with truncatedSuffix.
	//
	// What this costs a complete body is nothing: cut is false for every one of them.
	suffix := ""
	if cut {
		i := strings.LastIndexByte(body, '&')
		if i < 0 {
			// The one and only pair is the cut one: there is nothing left that may be let through
			return Redacted
		}
		body, suffix = body[:i], truncatedSuffix
	}

	values, err := url.ParseQuery(body)
	if err != nil {
		// What cannot be parsed is masked whole, for JSON's reason: what cannot be located cannot be
		// let through
		return Redacted
	}
	for k, vs := range values {
		if sensitive(k, ws) {
			values[k] = []string{Redacted}
			continue
		}
		// A URL / DSN credential inside a value is masked as well, as the RedactText the JSON branch
		// calls on a string value does: ?next=https://u:p@host, or a DSN inside a form, must not reach
		// the log as it is just because the encoding changed
		for i, v := range vs {
			vs[i] = redactCredentials(v)
		}
	}
	// Encode escapes the masked value into %2A%2A%2AREDACTED%2A%2A%2A as well, which is not
	// recognizable at a glance in a log; the marker holds no & or =, so turning it back does not
	// make the boundaries of the key/value pairs ambiguous
	return strings.ReplaceAll(values.Encode(), url.QueryEscape(Redacted), Redacted) + suffix
}

// RedactHeaders redacts the request headers and returns a value that can be handed to slog
// directly.
//
// It returns a slog.Value rather than a serialized string: handing over a string would have slog
// treat it as an ordinary string field and escape it once more, turning the log into
//
//	"request_headers":"{\"Authorization\":[\"***REDACTED***\"]}"
//
// where a search has to peel one layer of string off before the JSON can be parsed. Handing over a
// slog.Value serializes once, the field is an ordinary nested object in the log, and that
// json.Marshal is saved along the way (measured taking the whole line from 2777ns/19 allocs down to
// 1630ns/9 allocs).
//
// A multi-valued header is joined into one comma-separated string rather than an array: a field
// name that is a string in one log line and an array in the next is refused outright when the
// logging system builds its indexes.
//
// There are two rules and either one masks: the name is on the list (the defaults +
// AddSensitiveHeaders), or the name holds a sensitive word (the same word list as the body fields).
// A list is never complete -- Proxy-Authorization was once left off it -- which is why the word list
// is the fallback layer.
func RedactHeaders(h http.Header) slog.Value {
	set := headers()
	ws := words()

	// Sorted by key: a map ranges in a random order, and without sorting the same set of headers
	// would have a different field order in every line of log, which neither matches up nor
	// diffs
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	attrs := make([]slog.Attr, 0, len(keys))
	for _, k := range keys {
		name := strings.ToLower(k) // both lists are looked up in lowercase, so once is enough
		if set[name] || sensitive(k, ws) {
			attrs = append(attrs, slog.String(k, Redacted))
			continue
		}
		v := strings.Join(h[k], ", ") // a single value returns unchanged and allocates nothing, and the vast majority are single-valued
		if urlHeaders[name] {
			v = stripURL(v)
		}
		// A header whose name holds no sensitive word may still carry a whole DSN in its value: only
		// the password in it is masked (redactCredentials).
		// RedactText's name rule is not applied: the words in Vary: Cookie and
		// Access-Control-Allow-Headers: Authorization are header names and not credentials, and
		// masking what follows them would make the header log unreadable.
		// A value with no @ returns unchanged and allocates nothing: RedactHeaders over five headers
		// was measured still at 11 allocs and about 1.9µs
		attrs = append(attrs, slog.String(k, redactCredentials(v)))
	}
	return slog.GroupValue(attrs...)
}

// urlHeaders are the request and response headers whose value is a URL, which have their query
// string, fragment and userinfo removed before being logged.
//
// The access log's path deliberately carries no query string (credentials are often there: ?token=,
// an OAuth callback's ?code=), while Referer carries exactly the previous page's whole URL, and
// copying it would record the same thing again through a side door.
// The x- prefixed ones are what a reverse proxy forwards the original request URI in, whose content
// is this request's path plus its query string.
// The last three are response headers: an OAuth callback's 302 Location carries ?code= and the
// implicit grant carries #access_token=; Refresh's value is "<seconds>; url=…", to which the
// truncation rule applies just as well.
// Origin holds the scheme, the host and the port alone by the specification and is not among
// them
var urlHeaders = map[string]bool{
	"referer":          true,
	"x-original-url":   true,
	"x-original-uri":   true,
	"x-rewrite-url":    true,
	"x-forwarded-uri":  true,
	"location":         true,
	"content-location": true,
	"refresh":          true,
}

// stripURL removes everything after the first ? or #, and then the userinfo between :// and the
// host (https://tok@host).
//
// Truncated by character rather than url.Parse: a failed parse would need a decision about what to
// do, while truncation has no such thing as a failure.
// With several values joined together it removes the later values along with it -- masking a little
// too much, never missing any.
// The userinfo is removed whole rather than having its password masked: in the kind holding only a
// user name (https://<token>@host) the name itself is the credential
func stripURL(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	if i := strings.Index(u, "://"); i >= 0 {
		host := u[i+3:]
		if j := strings.IndexByte(host, '/'); j >= 0 {
			host = host[:j]
		}
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			u = u[:i+3] + u[i+3+at+1:]
		}
	}
	return u
}
