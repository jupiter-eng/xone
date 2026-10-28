package web

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const secret = "hunter2"

// mustNotLeak asserts that the redacted result holds no password
func mustNotLeak(t *testing.T, got string) {
	t.Helper()
	if strings.Contains(got, secret) {
		t.Fatalf("the password leaked into the log: %s", got)
	}
}

func TestRedactBody_JSON(t *testing.T) {
	got := RedactBody([]byte(`{"user":"alice","password":"`+secret+`"}`), "application/json")
	mustNotLeak(t, got)
	if !strings.Contains(got, `"user":"alice"`) {
		t.Errorf("a field that is not sensitive must be kept, got=%s", got)
	}
}

func TestRedactBody_JSONEscapedFieldNames(t *testing.T) {
	// A regression case. JSON allows \uXXXX inside a string, so password may be written as
	// \u0070assword and the raw bytes need not hold that substring at all.
	// The previous version did a literal scan alone and such a body took the fast path and reached
	// the log as it was -- the password on disk in plaintext.
	cases := map[string]string{
		"the first letter escaped":       `{"` + esc('p') + `assword":"` + secret + `"}`,
		"one in the middle escaped":      `{"pass` + esc('w') + `ord":"` + secret + `"}`,
		"several characters, mixed case": `{"` + esc('P') + esc('A') + `SSWORD":"` + secret + `"}`,
		"nested":                         `{"a":{"` + esc('t') + `oken":"` + secret + `"}}`,
		"inside an array":                `[{"` + esc('s') + `ecret":"` + secret + `"}]`,
	}
	for name, body := range cases {
		// Confirm first that this case really goes around the literal scan, or it tests nothing
		if strings.Contains(strings.ToLower(body), "password") ||
			strings.Contains(strings.ToLower(body), "token") ||
			strings.Contains(strings.ToLower(body), "secret") {
			t.Fatalf("%s: the case still holds the field name literally, so the escapes were turned back along the way: %s", name, body)
		}
		got := RedactBody([]byte(body), "application/json")
		mustNotLeak(t, got)
		if !strings.Contains(got, Redacted) {
			t.Errorf("%s must be masked, body=%s got=%s", name, body, got)
		}
	}
}

func TestRedactBody_JSONCutInsideAValueKeepsWhatWasRead(t *testing.T) {
	// A body the access log records is usually cut by the framework itself (MaxResponseBody,
	// maxRequestBody), so what arrives here ends in the middle of whatever field happened to be
	// there. What was read is recorded, what follows the cut is dropped, and the line says so
	got := RedactBody([]byte(`{"user":"alice","password":"`+secret), "application/json")
	mustNotLeak(t, got)
	if !strings.Contains(got, `"user":"alice"`) {
		t.Errorf("the members read before the cut must be kept, got=%s", got)
	}
	if !strings.HasSuffix(got, truncatedSuffix) {
		t.Errorf("a body that was not read to the end has to say so, got=%s", got)
	}
}

func TestRedactBody_JSONCutWithNothingReadableOmitsIt(t *testing.T) {
	// Not one member came out, so there is nothing to mask field by field. It must not read as
	// Redacted: that marker means a sensitive field was found, and nothing was located here
	got := RedactBody([]byte(`{"password": "`+secret), "application/json")
	mustNotLeak(t, got)
	if got != unparseableOmitted {
		t.Errorf("got=%s, want %s", got, unparseableOmitted)
	}
}

func TestRedactBody_JSONCutAfterAnEscapedFieldNameStillMasksIt(t *testing.T) {
	// The reason the cut body goes through the decoder at all rather than being printed as text:
	// a key written as password is the string password by the time the word list sees it,
	// exactly as in a body that parsed whole. A literal scan over the raw bytes would miss it
	body := `{"` + esc('p') + `assword":"` + secret + `","next":"cut here`
	if strings.Contains(strings.ToLower(body), "password") {
		t.Fatalf("the case no longer hides the field name, so it tests nothing: %s", body)
	}
	got := RedactBody([]byte(body), "application/json")
	mustNotLeak(t, got)
	if !strings.Contains(got, Redacted) {
		t.Errorf("the escaped field name must still be masked, got=%s", got)
	}
}

func TestRedactBody_JSONCutDropsEverythingAfterTheCut(t *testing.T) {
	// Whatever follows the first decoding error never reaches the log, whether or not the word list
	// would have recognized it: it was never resolved into a token. Here b's string is never closed,
	// so everything written after it is inside a value the decoder could not read
	got := RedactBody([]byte(`{"a":1,"b":"never closed, so this password=`+secret), "application/json")
	mustNotLeak(t, got)
	if got != `{"a":1}`+truncatedSuffix {
		t.Errorf("got=%s, want only the members read before the cut", got)
	}
}

func TestRedactBody_JSONCutInsideANestedListKeepsTheElementsRead(t *testing.T) {
	// The shape this is all for: an envelope around a list, cut inside the list. Stopping at the
	// top level would take items as unreadable and leave the whole body unreadable with it, which
	// is every paged response there is
	got := RedactBody([]byte(`{"page":1,"items":[{"id":0,"token":"`+secret+`"},{"id":1},{"id`), "application/json")
	mustNotLeak(t, got)
	for _, want := range []string{`"page":1`, `"id":0`, `"id":1`, `"token":"` + Redacted + `"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s, got=%s", want, got)
		}
	}
	if !strings.HasSuffix(got, truncatedSuffix) {
		t.Errorf("missing the truncation marker, got=%s", got)
	}
}

// The cut lands wherever the body happened to reach the cap, which is to say anywhere. This walks
// every single cut point of each body and asserts the credential never comes out -- a key read but
// not yet masked, a value read in halves, an escaped name cut mid-escape, all of them.
func TestRedactBody_NoSecretLeaksAtAnyCutPoint(t *testing.T) {
	bodies := []string{
		`{"user":"alice","password":"` + secret + `","next":"x"}`,
		`{"` + esc('p') + `assword":"` + secret + `","next":"x"}`, // the name only exists after decoding
		`{"a":{"b":{"token":"` + secret + `"}},"c":1}`,            // nested
		`[{"id":1},{"secret":"` + secret + `"},{"id":2}]`,         // inside an array
		`{"page":1,"items":[{"apikey":"` + secret + `"}],"n":2}`,  // envelope around a list
		`{"ſecret":"` + secret + `"}`,                             // Unicode folding, not a literal match
		`{"dsn":"postgres://app:` + secret + `@db:5432/prod"}`,    // no sensitive key, a DSN in the value
		`{"note":"login failed password=` + secret + `","id":7}`,  // a name inside a string value
		`{"a":[[[{"session":"` + secret + `"}]]],"b":"tail"}`,     // deep inside arrays
		`{"x":1,"CREDENTIAL":"` + secret + `","y":{"api_key":"` + secret + `"},"w":2}`,
	}
	for _, body := range bodies {
		for cut := 0; cut <= len(body); cut++ {
			// cut is true for every one of these: a body the access log shortened is what this is
			// about, and the fast path's tests all assume a complete body (see redactBody)
			got := redactBody([]byte(body[:cut]), "application/json", true)
			if strings.Contains(got, secret) {
				t.Fatalf("cut at %d of %d leaked the password\n body=%s\n  got=%s", cut, len(body), body[:cut], got)
			}
		}
	}
}

func TestRedactBody_CutDSNDoesNotTakeTheFastPath(t *testing.T) {
	// The cut took the @ the DSN regexps go by, and with it the last reason the fast path had to
	// hand this body to the parser: no sensitive word, no backslash, no @. It used to be written
	// into the log exactly as it arrived
	body := []byte(`{"dsn":"postgres://app:` + secret)
	if got := redactBody(body, "application/json", true); strings.Contains(got, secret) {
		t.Fatalf("the password reached the log: %s", got)
	}
	// Whereas a complete body still takes it, unchanged: that is every ordinary request
	whole := []byte(`{"id":1,"name":"a"}`)
	if got, want := redactBody(whole, "application/json", false), `{"id":1,"name":"a"}`; got != want {
		t.Fatalf("the fast path changed for a complete body: got=%s want=%s", got, want)
	}
}

// The last member read before the cut is where a key has been seen and its value has not: a bug
// there would be invisible in the cases above, which only assert that nothing leaked.
func TestRedactBody_CutRightAfterASensitiveKeyRecordsNothingForIt(t *testing.T) {
	body := `{"user":"alice","password":"` + secret + `"}`
	cut := strings.Index(body, `:"`+secret) + 1 // just past the colon after "password"
	got := RedactBody([]byte(body[:cut]), "application/json")
	mustNotLeak(t, got)
	if !strings.Contains(got, `"user":"alice"`) {
		t.Errorf("the member before it was lost, got=%s", got)
	}
	if strings.Contains(got, "password") {
		t.Errorf("a key whose value was never read must not be recorded at all, got=%s", got)
	}
}

func TestRedactBody_JSONCutKeepsANullValue(t *testing.T) {
	// A null value is a value: the decoder hands it over as a nil any, which is also what a
	// container cut before its first member comes back as. Telling the two apart by the nil alone
	// would make the field disappear from the log, which reads as a response that never held it
	got := RedactBody([]byte(`{"deleted_at":null,"token":"`+secret), "application/json")
	mustNotLeak(t, got)
	if !strings.Contains(got, `"deleted_at":null`) {
		t.Errorf("a null value was dropped, got=%s", got)
	}
}

func TestRedactBody_ResponseCutAtTheCapIsStillReadable(t *testing.T) {
	// The case this all exists for. The capture writer stops at MaxResponseBody, and mayContainField
	// hands anything holding a backslash to the parser -- which \" and \n in ordinary JSON always do.
	// Such a body used to be masked whole: the switch was on, the log said REDACTED, and nothing of
	// the response was visible, with no way to tell that from a credential having been found
	var b strings.Builder
	b.WriteString(`{"items":[`)
	for i := 0; b.Len() < MaxResponseBody; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"note":"a \"quoted\" line"}`, i)
	}
	got := RedactBody([]byte(b.String()[:MaxResponseBody]), "application/json")
	if !strings.Contains(got, `"note":"a \"quoted\" line"`) {
		t.Errorf("a response cut at the cap records nothing readable: %.100s", got)
	}
	if !strings.HasSuffix(got, truncatedSuffix) {
		t.Errorf("missing the truncation marker: %.100s", got)
	}
}

func TestRedactBody_KeptAsIsWithoutSensitiveFields(t *testing.T) {
	// This is what the fast path is for: the vast majority of request bodies hold no sensitive
	// field and should not pay for parsing
	body := `{"user":"alice","age":30}`
	if got := RedactBody([]byte(body), "application/json"); got != body {
		t.Errorf("one holding no sensitive field must be returned as it is, got=%s", got)
	}
}

func TestRedactBody_CaseInsensitive(t *testing.T) {
	for _, body := range []string{
		`{"PASSWORD":"` + secret + `"}`,
		`{"PassWord":"` + secret + `"}`,
		`{"Api_Key":"` + secret + `"}`,
	} {
		mustNotLeak(t, RedactBody([]byte(body), "application/json"))
	}
}

func TestRedactBody_NestedAndArrays(t *testing.T) {
	body := `{"a":{"b":[{"token":"` + secret + `"}]},"c":"keep"}`
	got := RedactBody([]byte(body), "application/json")
	mustNotLeak(t, got)
	if !strings.Contains(got, "keep") {
		t.Errorf("content that is not sensitive must be kept, got=%s", got)
	}
}

func TestRedactBody_MasksFieldsContainingSensitiveWord(t *testing.T) {
	// A regression case. It used to be an exact match and new_password, client_secret and
	// sessionToken all reached the logs as they were. Redaction fails asymmetrically: one field
	// masked too many is a little less to go on, and one missed is a credential on disk in
	// plaintext. So it goes by "contains" once the case and the separators are removed
	for _, key := range []string{
		"new_password", "old-password", "user.password", "client_secret",
		"sessionToken", "X-API-KEY", "api-key", "AccessToken", "user_passwd",
	} {
		body := `{"a":{"b":[{"` + key + `":"` + secret + `"}]},"keep":"yes"}`
		got := RedactBody([]byte(body), "application/json")
		mustNotLeak(t, got)
		if !strings.Contains(got, `"keep":"yes"`) {
			t.Errorf("%s: a field that is not sensitive must be kept, got=%s", key, got)
		}

		got = RedactBody([]byte(url.Values{key: {secret}, "keep": {"yes"}}.Encode()), "application/x-www-form-urlencoded")
		mustNotLeak(t, got)
		if !strings.Contains(got, "keep=yes") {
			t.Errorf("%s: a form field that is not sensitive must be kept, got=%s", key, got)
		}
	}
}

func TestRedactBody_ReserializeKeepsOtherValues(t *testing.T) {
	// A regression case. It used to decode into any and json.Marshal: a large integer went through
	// float64 and 12345678901234567890 became 12345678901234567000, while < > & were written as
	// backslash-u escape sequences -- the order id in the log did not match and a search did not
	// find the original either
	body := `{"password":"` + secret + `","id":12345678901234567890,"n":0.1,"q":"a<b>&c"}`
	got := RedactBody([]byte(body), "application/json")
	mustNotLeak(t, got)
	for _, want := range []string{`"id":12345678901234567890`, `"n":0.1`, `"q":"a<b>&c"`} {
		if !strings.Contains(got, want) {
			t.Errorf("%s must be kept as it is, got=%s", want, got)
		}
	}
	if strings.ContainsAny(got, "\n") {
		t.Errorf("no line break may be carried out, got=%q", got)
	}
}

func TestRedactBody_JSONWithTrailingDataDropsTheTrailer(t *testing.T) {
	// A complete value followed by something else counts as "not read to the end": the value is
	// kept and the trailer is dropped rather than quietly treated as though it were not there --
	// it may be exactly the part holding the credential
	got := RedactBody([]byte(`{"a":1} {"password":"`+secret+`"}`), "application/json")
	mustNotLeak(t, got)
	if got != `{"a":1}`+truncatedSuffix {
		t.Errorf("got=%s", got)
	}
}

func TestRedactBody_Form(t *testing.T) {
	got := RedactBody([]byte("user=alice&password="+secret), "application/x-www-form-urlencoded")
	mustNotLeak(t, got)
	if !strings.Contains(got, "user=alice") {
		t.Errorf("a field that is not sensitive must be kept, got=%s", got)
	}
	// A masked value is written as the marker itself, not the escaped %2A%2A%2AREDACTED%2A%2A%2A
	if !strings.Contains(got, "password="+Redacted) {
		t.Errorf("a masked value must be written as %s, got=%s", Redacted, got)
	}
}

func TestRedactBody_FormPercentEncodedKeys(t *testing.T) {
	// A regression case. A form's keys are percent-encoded, and p%61ssword cut literally does not
	// match password and reaches the log all the same. Comparing against the key the server really
	// read means decoding it first
	for _, body := range []string{
		"p%61ssword=" + secret,
		"%50assword=" + secret,
		"%74oken=" + secret,
	} {
		got := RedactBody([]byte(body), "application/x-www-form-urlencoded")
		mustNotLeak(t, got)
		// url.Values.Encode percent-encodes the * inside the marker, so it is decoded before the
		// comparison
		decoded, err := url.QueryUnescape(got)
		if err != nil {
			t.Fatalf("the result is not a valid query: %v", err)
		}
		if !strings.Contains(decoded, Redacted) {
			t.Errorf("this must be masked, body=%s got=%s", body, got)
		}
	}
}

func TestRedactBody_FormParseFailureMasksAll(t *testing.T) {
	got := RedactBody([]byte("password=%zz"), "application/x-www-form-urlencoded")
	if got != Redacted {
		t.Errorf("a failed parse must mask the whole of it, got=%s", got)
	}
}

// The form's half of TestRedactBody_CutDSNDoesNotTakeTheFastPath. A DSN inside a form value is
// percent-encoded, so a cut takes the %40 the DSN regexps go by exactly as it takes a JSON body's @:
// dsn=postgres%3A%2F%2Fapp%3Ahunter2 decodes to postgres://app:hunter2, which holds no sensitive word
// and no @ any more. Only the last pair can be incomplete, and it is the one dropped; see redactForm.
func TestRedactBody_CutFormDropsThePairTheCutLandedIn(t *testing.T) {
	body := "user=alice&dsn=" + url.QueryEscape("postgres://app:"+secret+"@db:5432/prod")
	for cut := 0; cut <= len(body); cut++ {
		got := redactBody([]byte(body[:cut]), "application/x-www-form-urlencoded", true)
		if strings.Contains(got, secret) {
			t.Fatalf("cut at %d of %d leaked the password\n body=%s\n  got=%s", cut, len(body), body[:cut], got)
		}
	}
	// Everything complete before the cut pair is still recorded, with the marker saying the rest is
	// not: a body masked whole would read as a credential having been found
	at := strings.Index(body, "%40")
	got := redactBody([]byte(body[:at]), "application/x-www-form-urlencoded", true)
	if !strings.Contains(got, "user=alice") {
		t.Errorf("the pairs completed before the cut one were lost, got=%s", got)
	}
	if !strings.HasSuffix(got, truncatedSuffix) {
		t.Errorf("missing the truncation marker, got=%s", got)
	}
	// With one pair and no & there is nothing complete left to let through
	if got := redactBody([]byte("dsn=postgres%3A%2F%2Fapp%3A"+secret), "application/x-www-form-urlencoded", true); got != Redacted {
		t.Errorf("a cut body holding one pair must be masked whole, got=%s", got)
	}
	// A complete body is untouched by any of this: cut is false for every one of them
	if got, want := redactBody([]byte("user=alice"), "application/x-www-form-urlencoded", false), "user=alice"; got != want {
		t.Errorf("a complete form body changed: got=%s want=%s", got, want)
	}
}

func TestRedactBody_OtherTypesStripNewlines(t *testing.T) {
	// One log stretched over several lines puts the collection and the search downstream out of
	// step
	got := RedactBody([]byte("line1\nline2\r\nline3"), "text/plain")
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("the line breaks must be removed, got=%q", got)
	}
}

func TestRedactBody_JSONVariantsAllRedacted(t *testing.T) {
	// It used to be an exact match on application/json, so the +json subtypes, text/json and the
	// uppercase spellings all went around the redaction -- the password reached the log as it was.
	// A Content-Type is case-insensitive under RFC 9110 to begin with.
	body := []byte(`{"password":"hunter2"}`)
	for _, ct := range []string{
		"application/json",
		"application/json; charset=utf-8",
		"application/vnd.api+json",
		"application/problem+json",
		"application/ld+json",
		"text/json",
		"Application/JSON",
		"APPLICATION/JSON; CHARSET=UTF-8",
	} {
		got := RedactBody(body, ct)
		if strings.Contains(got, "hunter2") {
			t.Errorf("Content-Type=%q missed the password, got=%s", ct, got)
		}
	}
}

func TestRedactBody_UppercaseFormTypeRedacted(t *testing.T) {
	got := RedactBody([]byte("password=hunter2"), "Application/X-WWW-Form-Urlencoded")
	if strings.Contains(got, "hunter2") {
		t.Errorf("an uppercase Content-Type must be redacted just as well, got=%s", got)
	}
}

func TestRedactBody_UnknownFormatWithSensitiveNameMasksAll(t *testing.T) {
	// In a text/plain, xml or Content-Type-less body no single field can be located, while "does it
	// hold a sensitive field name" can still be seen. If it does, the whole of it is masked -- the
	// same rule the JSON branch applies when parsing fails: what cannot be located cannot be let
	// through.
	for _, ct := range []string{"text/plain", "application/xml", "text/xml", ""} {
		got := RedactBody([]byte("<user><password>hunter2</password></user>"), ct)
		if got != Redacted {
			t.Errorf("Content-Type=%q must be masked whole, got=%s", ct, got)
		}
	}
}

func TestRedactBody_UnknownFormatWithoutSensitiveNameKept(t *testing.T) {
	// Masking too much wipes out what there is to go on, so it only acts when a sensitive field
	// name really appears
	const body = "just a plain note about the weather"
	if got := RedactBody([]byte(body), "text/plain"); got != body {
		t.Errorf("one with no sensitive field name must be kept as it is, got=%s", got)
	}
}

func TestRedactBody_BackslashInPlainTextDoesNotMaskAll(t *testing.T) {
	// A backslash is JSON's escape syntax while in plain text it is an ordinary character.
	// Copying JSON's "a backslash is suspicious on sight" rule would mask a log holding a Windows
	// path whole
	const body = `open failed: C:\Users\app\data.txt`
	if got := RedactBody([]byte(body), "text/plain"); got != body {
		t.Errorf("a backslash in plain text must not trigger masking the whole of it, got=%s", got)
	}
}

func TestRedactBody_EmptyBody(t *testing.T) {
	if got := RedactBody(nil, "application/json"); got != "" {
		t.Errorf("an empty body must return an empty string, got=%q", got)
	}
}

func TestAddSensitiveFields(t *testing.T) {
	reset := func() {
		mu.Lock()
		fieldWords = normalizeAll(defaultWords)
		mu.Unlock()
	}
	reset()
	t.Cleanup(reset)

	body := `{"my_custom_key":"` + secret + `"}`
	if got := RedactBody([]byte(body), "application/json"); strings.Contains(got, Redacted) {
		t.Fatal("nothing may be masked before it has been added")
	}

	AddSensitiveFields("My_Custom_Key") // insensitive to case
	mustNotLeak(t, RedactBody([]byte(body), "application/json"))
}

// headerLog renders the redacted headers the way they appear in a log
func headerLog(h http.Header) string {
	var buf strings.Builder
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("req", "request_headers", RedactHeaders(h))
	return buf.String()
}

func TestRedactHeaders(t *testing.T) {
	h := http.Header{
		"Authorization": {"Bearer " + secret},
		"Cookie":        {"session=" + secret},
		"Content-Type":  {"application/json"},
	}
	got := headerLog(h)
	mustNotLeak(t, got)
	if !strings.Contains(got, "application/json") {
		t.Errorf("a header that is not sensitive must be kept, got=%s", got)
	}
	// The original header must not be modified
	if h.Get("Authorization") != "Bearer "+secret {
		t.Error("the header passed in must not be modified")
	}
}

func TestRedactHeaders_LoggedAsNestedObjectNotEscapedString(t *testing.T) {
	// Handing over a serialized string would have slog treat it as an ordinary string field and
	// escape it once more, making the log "request_headers":"{\"X-A\":\"1\"}" -- a search would have
	// to peel one layer of string off first
	got := headerLog(http.Header{"X-A": {"1"}})
	if strings.Contains(got, `\"`) {
		t.Errorf("no double escaping may appear, got=%s", got)
	}
	if !strings.Contains(got, `"request_headers":{"X-A":"1"}`) {
		t.Errorf("this must be a nested object, got=%s", got)
	}
}

func TestRedactHeaders_JoinsMultiValueHeader(t *testing.T) {
	// A field name that is a string in one line and an array in the next is refused outright when
	// the logging system builds its indexes.
	// A single value stays as it is and one with no values is an empty string
	got := headerLog(http.Header{"X-Multi": {"a", "b"}, "X-One": {"c"}, "X-None": {}})
	if !strings.Contains(got, `{"X-Multi":"a, b","X-None":"","X-One":"c"}`) {
		t.Errorf("several values must be joined into one string, got=%s", got)
	}
}

func TestRedactHeaders_FieldsSortedByKey(t *testing.T) {
	// A map ranges in a random order, and without sorting the field order differs in every line of
	// log
	h := http.Header{"X-C": {"3"}, "X-A": {"1"}, "X-B": {"2"}}
	for i := 0; i < 20; i++ {
		got := headerLog(h)
		if !strings.Contains(got, `{"X-A":"1","X-B":"2","X-C":"3"}`) {
			t.Fatalf("the field order must be stable, got=%s", got)
		}
	}
}

func TestRedactHeaders_CookieMaskedByDefault(t *testing.T) {
	// A Cookie almost always holds a session identifier, which is a credential
	mustNotLeak(t, headerLog(http.Header{"Cookie": {"sid=" + secret}}))
	mustNotLeak(t, headerLog(http.Header{"Set-Cookie": {"sid=" + secret}}))
}

func TestRedactHeaders_MasksAddedSensitiveHeaders(t *testing.T) {
	// A header whose name holds no sensitive word rests on the list it is added to: this case is
	// also the only guard over the "masked by the list" code -- every header on the default list
	// carries a sensitive word
	reset := func() {
		mu.Lock()
		headerSet = lowerSet(defaultHeaders)
		mu.Unlock()
	}
	reset()
	t.Cleanup(reset)

	h := http.Header{"X-Tenant-Id": {secret}}
	if !strings.Contains(headerLog(h), secret) {
		t.Fatal("nothing may be masked before it has been added")
	}
	AddSensitiveHeaders("x-tenant-id")
	mustNotLeak(t, headerLog(h))
}

func TestRedactHeaders_MasksNamesContainingSensitiveWord(t *testing.T) {
	// A regression case. The default list missed Proxy-Authorization, which carries a credential
	// just as Authorization does. A list is never complete, so there is a second rule: a name
	// holding a sensitive word is masked
	for _, name := range []string{
		"Proxy-Authorization", "X-Csrf-Token", "X-Session-Id", "X-Goog-Api-Key",
		"X-Amz-Security-Token", "X-Client-Secret", "X-Hub-Signature-256",
	} {
		mustNotLeak(t, headerLog(http.Header{name: {secret}}))
	}

	// The other way round it must not mask too much either: these are the headers most often looked
	// at while tracking something down, and their names merely brush against the words
	benign := http.Header{
		"User-Agent": {"ua"}, "Accept": {"a"}, "Content-Type": {"ct"}, "X-Request-Id": {"rid"},
		"Keep-Alive": {"ka"}, "Idempotency-Key": {"ik"}, "Sec-Websocket-Key": {"wk"},
		"Www-Authenticate": {"wa"}, "Traceparent": {"tp"}, "X-Forwarded-For": {"xff"},
	}
	got := headerLog(benign)
	for _, v := range []string{"ua", `"a"`, "ct", "rid", "ka", "ik", "wk", "wa", "tp", "xff"} {
		if !strings.Contains(got, v) {
			t.Errorf("%s must not be masked, got=%s", v, got)
		}
	}
}

func TestRedactHeaders_NoLeakBetweenCalls(t *testing.T) {
	// A regression case: an earlier implementation took a map out of a pool and reused it, and
	// forgetting to clear it leaked the previous request's headers into the next log
	first := headerLog(http.Header{"X-One": {"1"}})
	second := headerLog(http.Header{"X-Two": {"2"}})
	if strings.Contains(second, "X-One") {
		t.Errorf("the previous request's headers leaked into this one: first=%s second=%s", first, second)
	}
}

func TestMayContainField_IgnoresCaseAndSeparators(t *testing.T) {
	ws := words()
	for _, c := range []struct {
		body string
		want bool
	}{
		{"PASSWORD=1", true},
		{"PaSsWoRd", true},
		{`{"Api-Key":1}`, true}, // a separator inside the word
		{"passwor", false},      // shorter than the word
		{"passwerd", false},
		{"", false},
	} {
		if got := mayContainField(c.body, ws); got != c.want {
			t.Errorf("mayContainField(%q)=%v want %v", c.body, got, c.want)
		}
	}
}

func TestMayContainField_BackslashAlwaysTakesSlowPath(t *testing.T) {
	ws := words()
	if !mayContainField(`{"a":"b\\c"}`, ws) {
		t.Error("a backslash must take the slow path -- a literal scan is unreliable against escapes")
	}
	if mayContainField(`{"a":"b"}`, ws) {
		t.Error("with neither a backslash nor a field name it must take the fast path")
	}
}

// Proving that this set of cases really tests what the old implementation missed
func TestRedact_CasesOldImplementationMissed(t *testing.T) {
	escaped := `{"` + esc('p') + `assword":"` + secret + `"}`
	encoded := "p%61ssword=" + secret

	// The first old implementation: a literal scan over the raw bytes alone, with no backslash
	// check
	legacyJSON := func(body []byte) bool { return sensitive(string(body), words()) }
	if legacyJSON([]byte(escaped)) {
		t.Fatal("this case does not hold against the old implementation, so it tests nothing it should")
	}

	// The second old implementation: cutting the form on & and = without decoding the keys
	legacyForm := func(body string) bool {
		for _, pair := range strings.Split(body, "&") {
			k, _, _ := strings.Cut(pair, "=")
			if strings.Contains(strings.ToLower(k), "password") {
				return true
			}
		}
		return false
	}
	if legacyForm(encoded) {
		t.Fatal("this case does not hold against the old implementation, so it tests nothing it should")
	}

	// The same inputs, all masked by the new implementation
	mustNotLeak(t, RedactBody([]byte(escaped), "application/json"))
	mustNotLeak(t, RedactBody([]byte(encoded), "application/x-www-form-urlencoded"))
}

// esc writes one ASCII character as JSON's \uXXXX escape.
//
// Not written literally: this file travels through several layers of text transport, and a literal
// "backslash u" is easily interpreted away by one of them along the way. The first version failed
// exactly that way -- the \u0070assword in the cases was turned back into password, so the whole set
// of "escape bypass" regression cases tested ordinary field names, passed, and proved nothing.
func esc(c byte) string { return fmt.Sprintf("%cu%04x", 92, c) }

func TestRedactBody_UnicodeFoldedSensitiveWordMasked(t *testing.T) {
	// encoding/json matches field names by Unicode case folding: a long s (U+017F) folds to s and
	// the Kelvin sign (U+212A) folds to k. Compared as ASCII alone, such a key is not recognized by
	// the pre-check and the body reaches the log as it is -- while that is exactly the field the
	// server read
	var bound struct{ Secret, Token string }
	body := `{"ſecret":"` + secret + `","to` + "K" + `en":"` + secret + `","note":"hello"}`
	if err := json.Unmarshal([]byte(body), &bound); err != nil || bound.Secret != secret || bound.Token != secret {
		t.Fatalf("the premise does not hold: encoding/json must bind these two keys to Secret and Token, got=%+v err=%v", bound, err)
	}

	got := RedactBody([]byte(body), "application/json")
	mustNotLeak(t, got)
	if !strings.Contains(got, `"note":"hello"`) {
		t.Errorf("a field that is not sensitive must be kept, got=%s", got)
	}
	mustNotLeak(t, RedactBody([]byte("ſecret="+secret), "application/x-www-form-urlencoded"))
	if got := RedactBody([]byte("ſecret: "+secret), "text/plain"); got != Redacted {
		t.Errorf("an unrecognized structure holding a folded sensitive word must be masked whole, got=%s", got)
	}
}

func TestRedactBody_ChineseContentTakesFastPath(t *testing.T) {
	// A multi-byte character is decoded before it is folded, which must not make an ordinary
	// multi-byte body suspicious
	body := `{"note":"hello, world"}`
	if got := RedactBody([]byte(body), "text/plain"); got != body {
		t.Errorf("a multi-byte body with no sensitive word must be kept as it is, got=%s", got)
	}
}

func TestRedactHeaders_URLHeadersStripQueryString(t *testing.T) {
	// The access log's path deliberately carries no query string while Referer carries the previous
	// page's whole URL: an OAuth callback's ?code= and a ?token= on a link reach the log through
	// here again
	got := headerLog(http.Header{
		"Referer":        {"https://app.example.com/cb?code=" + secret + "#frag"},
		"X-Original-Url": {"/login?token=" + secret},
	})
	mustNotLeak(t, got)
	if !strings.Contains(got, `"Referer":"https://app.example.com/cb"`) || !strings.Contains(got, `"X-Original-Url":"/login"`) {
		t.Errorf("only the part before the query string may be kept, got=%s", got)
	}
}

func TestRedactText_MasksTheValueUnderASensitiveName(t *testing.T) {
	for in, want := range map[string]string{
		"":        "",
		"db down": "db down",
		// The sentence in front of the value is what says what went wrong, so it is kept
		"login failed password=hunter2": "login failed password=" + Redacted,
		// The name is folded and its separators removed before the comparison, as a body field is
		"login failed for user=x New-Password=y": "login failed for user=x New-Password=" + Redacted,
		"access_token: abc":                      "access_token: " + Redacted,
		`{"session_id":"abc"} rejected`:          `{"session_id":` + Redacted,
		"cfg={Secret: abc, Host: db}":            "cfg={Secret: " + Redacted,
		"upstream said: x-api-key = sent":        "upstream said: x-api-key = " + Redacted,
		// A header dumped into an error carries its credential in the second word: stopping at the
		// first space would write the token out in full
		"unexpected reply, Authorization: Bearer eyJhbGciOi": "unexpected reply, Authorization: " + Redacted,
		// A name with nothing after it on that line has no value to mask, and masking would eat the
		// line break that keeps two errors apart
		"token:\nsecond error": "token:; second error",
	} {
		if got := RedactText(in); got != want {
			t.Errorf("RedactText(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestRedactText_KeepsAMessageThatOnlyNamesACredential(t *testing.T) {
	// These are the messages wanted most while tracking an authentication failure down, and not one
	// of them carries a credential. Masking the whole text on a sensitive word alone took every one
	// of them out of the logs.
	for _, in := range []string{
		"token expired",
		"jwt token is malformed",
		"session not found for user 42",
		"invalid signature",
		"failed to parse cookie header",
		"missing credential for upstream",
		"Authorization required",
		"Key: 'LoginReq.Password' Error:Field validation for 'Password' failed on the 'required' tag",
		"pq: password authentication failed for user app",
	} {
		if got := RedactText(in); got != in {
			t.Errorf("RedactText(%q)=%q, a message naming a credential is not one", in, got)
		}
	}
}

func TestRedactText_MasksEveryPairOnTheLine(t *testing.T) {
	// The first name on the line is not sensitive, so the scan has to carry on past its value
	got := RedactText("retry: user=app password=hunter2")
	if strings.Contains(got, "hunter2") {
		t.Errorf("RedactText leaked the password: %q", got)
	}
	if want := "retry: user=app password=" + Redacted; got != want {
		t.Errorf("RedactText=%q, want %q", got, want)
	}
}

func TestRedactText_MasksOnlyToTheEndOfTheLine(t *testing.T) {
	// gin's c.Errors.String() puts one error per line: a value running to the end of the text would
	// swallow every error after this one
	got := RedactText("Error #01: password=hunter2\nError #02: db down\n")
	if want := "Error #01: password=" + Redacted + "; Error #02: db down"; got != want {
		t.Errorf("RedactText=%q, want %q", got, want)
	}
}

func TestRedactOpaque_StillMasksAWholeBody(t *testing.T) {
	// A body is data somebody sent, whose shape nobody here knows, so a sensitive word in it still
	// masks the whole of it -- the rule RedactText relaxed is not relaxed here
	for _, in := range []string{"hunter2 is my password", "invalid Access_Token abc"} {
		if got := RedactBody([]byte(in), "text/plain"); got != Redacted {
			t.Errorf("RedactBody(%q)=%q, want %q", in, got, Redacted)
		}
	}
}

func TestRedactText_MasksPasswordInURLUserinfo(t *testing.T) {
	// No sensitive word and a credential all the same: a driver and echo's HTTPError both write a
	// whole DSN into the error text
	for in, want := range map[string]string{
		"dial postgres://app:hunter2@db:5432/prod failed":                                      "dial postgres://app:" + Redacted + "@db:5432/prod failed",
		"code=502, message=upstream, internal=dial postgres://app:hunter2@db:5432/prod failed": "code=502, message=upstream, internal=dial postgres://app:" + Redacted + "@db:5432/prod failed",
		"redis://:hunter2@cache:6379/0 timeout":                                                "redis://:" + Redacted + "@cache:6379/0 timeout",
		"a amqp://u:hunter2@mq b amqp://v:hunter2@mq2":                                         "a amqp://u:" + Redacted + "@mq b amqp://v:" + Redacted + "@mq2",
		// An unescaped @ inside the password: it goes by the last @, as net/url does, so no half of
		// the password is left
		"dial postgres://app:hun@ter2@db:5432/prod failed": "dial postgres://app:" + Redacted + "@db:5432/prod failed",
	} {
		got := RedactText(in)
		if strings.Contains(got, "hunter2") || strings.Contains(got, "ter2") {
			t.Errorf("RedactText(%q) leaked the password: %q", in, got)
		}
		if got != want {
			t.Errorf("RedactText(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestRedactText_MasksPasswordInMySQLDSN(t *testing.T) {
	// go-sql-driver/mysql's spelling has no ://: user:pass@tcp(host)/db, user:pass@unix(/path)/db
	for in, want := range map[string]string{
		"dial app:hunter2@tcp(db:3306)/prod failed":           "dial app:" + Redacted + "@tcp(db:3306)/prod failed",
		"open app:hunter2@unix(/tmp/mysql.sock)/prod: denied": "open app:" + Redacted + "@unix(/tmp/mysql.sock)/prod: denied",
		"internal=app:hunter2@tcp(db:3306)/prod":              "internal=app:" + Redacted + "@tcp(db:3306)/prod",
	} {
		if got := RedactText(in); got != want {
			t.Errorf("RedactText(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestRedactText_KeepsTextWithoutCredentials(t *testing.T) {
	// Only the password in a userinfo is masked: a URL with no password, an email address, a time
	// and a port are all kept as they are
	for _, in := range []string{
		"dial postgres://app@db:5432/prod failed",
		"GET https://api.example.com:8443/v1/users?id=1 failed",
		"notify ops@example.com at 12:30:00",
		"redis://cache:6379/0 i/o timeout",
		`C:\data\app.db locked`,
	} {
		if got := RedactText(in); got != in {
			t.Errorf("RedactText(%q)=%q, text without credentials should stay intact", in, got)
		}
	}
}

func TestRedactText_KeepsLinesSeparated(t *testing.T) {
	// gin's c.Errors.String() puts one error per line with a trailing line break; removing the line
	// breaks alone would glue two errors into one sentence
	for in, want := range map[string]string{
		"Error #01: a\nError #02: b\n": "Error #01: a; Error #02: b",
		"first\r\nsecond":              "first; second",
		"single line\n":                "single line",
	} {
		if got := RedactText(in); got != want {
			t.Errorf("RedactText(%q)=%q, want %q", in, got, want)
		}
	}
	// A request body is unaffected: its line breaks are still removed outright
	if got := RedactBody([]byte("a\nb"), "text/plain"); got != "ab" {
		t.Errorf("RedactBody should keep dropping newlines, got=%q", got)
	}
}

func TestRedactHeaders_ResponseURLHeadersStripQueryFragmentAndUserinfo(t *testing.T) {
	// A regression case. The Location / Content-Location / Refresh response headers are URLs as
	// well: an OAuth callback's 302 Location carries ?code= and the implicit grant carries
	// #access_token=, and they used to reach response_headers as they were
	got := headerLog(http.Header{
		"Location":         {"https://app.example.com/cb?code=" + secret + "#access_token=" + secret},
		"Content-Location": {"/orders/1?token=" + secret},
		"Refresh":          {"0; url=https://app:" + secret + "@example.com/next?sig=" + secret},
		"Referer":          {"https://tok" + secret + "@example.com/prev"},
	})
	mustNotLeak(t, got)
	for _, want := range []string{
		`"Location":"https://app.example.com/cb"`,
		`"Content-Location":"/orders/1"`,
		`"Refresh":"0; url=https://example.com/next"`,
		`"Referer":"https://example.com/prev"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%s must be kept, got=%s", want, got)
		}
	}
}

func TestRedactHeaders_ValuesNamingSensitiveHeadersKept(t *testing.T) {
	// What appears in the value is a header name, not a credential: masking it whole by the
	// sensitive-word rule would make the header log unreadable
	got := headerLog(http.Header{
		"Vary":                          {"Cookie"},
		"Access-Control-Allow-Headers":  {"Authorization, X-Api-Key"},
		"Access-Control-Expose-Headers": {"Set-Cookie"},
		"X-Upstream-Dsn":                {"postgres://u:" + secret + "@h/db"},
	})
	mustNotLeak(t, got)
	for _, want := range []string{
		`"Vary":"Cookie"`,
		`"Access-Control-Allow-Headers":"Authorization, X-Api-Key"`,
		`"Access-Control-Expose-Headers":"Set-Cookie"`,
		`"X-Upstream-Dsn":"postgres://u:` + Redacted + `@h/db"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("this must be recorded as %s, got=%s", want, got)
		}
	}
}

func TestRedactHeaders_DSNInValueMasked(t *testing.T) {
	// A regression case. A header whose name holds no sensitive word while its value hides a whole
	// DSN (an X-Upstream for debugging, an error header a proxy writes back) used to reach the log
	// as it was; the password in such a DSN is masked as it is in an error text
	got := headerLog(http.Header{
		"X-Upstream": {"dial postgres://app:" + secret + "@db:5432/prod failed"},
		"X-Db":       {"app:" + secret + "@tcp(db:3306)/prod"},
		"Accept":     {"application/json"},
	})
	mustNotLeak(t, got)
	if !strings.Contains(got, `"X-Upstream":"dial postgres://app:`+Redacted+`@db:5432/prod failed"`) || !strings.Contains(got, `"Accept":"application/json"`) {
		t.Errorf("only the password is masked and the rest is kept as it is, got=%s", got)
	}
}

func TestRedactBody_JSONWithoutJSONContentType(t *testing.T) {
	// A regression case. gin's ShouldBindJSON ignores the Content-Type and JSON sent as text/plain
	// or with none at all parses just as well; those used to take the plain-text branch with its
	// literal scan alone, where a key name written as a JSON Unicode escape (see esc) is not
	// recognized and the password reaches the log as it is
	body := []byte(`{"user":"alice","p` + esc('a') + `ssword":"` + secret + `"}`)
	for _, ct := range []string{"", "text/plain", "text/plain; charset=utf-8"} {
		t.Run(ct, func(t *testing.T) {
			got := RedactBody(body, ct)
			mustNotLeak(t, got)
			if !strings.Contains(got, `"user":"alice"`) {
				t.Errorf("recognized as JSON it masks by field and keeps the other fields, got=%s", got)
			}
		})
	}
	// Plain text that is not JSON is unchanged
	if got := RedactBody([]byte("hello world"), "text/plain"); got != "hello world" {
		t.Errorf("plain text stays as it is, got=%s", got)
	}
}

func TestRedactBody_JSONStringValueDSNMasked(t *testing.T) {
	// A regression case. A key name with no sensitive word whose value is a whole DSN: judged by
	// the key it used to be kept as it was and never even parsed (the fast path)
	for name, body := range map[string]string{
		"inside an object": `{"dsn":"postgres://app:` + secret + `@db:5432/prod"}`,
		"inside an array":  `{"targets":["app:` + secret + `@tcp(db:3306)/prod"]}`,
		"at the top level": `"redis://:` + secret + `@cache:6379/0"`,
	} {
		t.Run(name, func(t *testing.T) {
			got := RedactBody([]byte(body), "application/json")
			mustNotLeak(t, got)
			if !strings.Contains(got, Redacted) {
				t.Errorf("the password must be replaced with %s, got=%s", Redacted, got)
			}
		})
	}
}

func TestRedactBody_CredentialsInFormValueAndPlainTextRedacted(t *testing.T) {
	// A regression case. The same URL with a password is masked inside a JSON string while reaching
	// the log as it is in a form value, a query or plain text -- the encoding is all that changed
	cases := []struct{ body, ct, want string }{
		// The form output is re-encoded, so : and @ are %3A and %40
		{"next=https%3A%2F%2Fbob%3A" + secret + "%40example.com%2Fx", "application/x-www-form-urlencoded", "bob%3A" + Redacted + "%40"},
		{"dsn postgres://bob:" + secret + "@db:5432/app", "text/plain", "bob:" + Redacted + "@"},
		{"bob:" + secret + "@tcp(db:3306)/app", "", "bob:" + Redacted + "@"},
	}
	for _, c := range cases {
		got := RedactBody([]byte(c.body), c.ct)
		mustNotLeak(t, got)
		if !strings.Contains(got, c.want) {
			t.Errorf("Content-Type=%q must keep the user name and replace the password with the marker, got=%s", c.ct, got)
		}
	}
}
