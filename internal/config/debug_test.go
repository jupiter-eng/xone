package config

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestDebug_OnlyAcceptsKnownEnableValues(t *testing.T) {
	for v, want := range map[string]bool{
		"1": true, "true": true, "TRUE": true, " yes ": true, "on": true,
		"": false, "0": false, "false": false, "off": false, "debug": false,
	} {
		t.Setenv(DebugEnvKey, v)
		if got := Debug(); got != want {
			t.Errorf("XONE_DEBUG=%q: Debug()=%v, want %v", v, got, want)
		}
	}
}

func TestRedacted_MasksCredentials_KeepsRest_LeavesOriginalNode(t *testing.T) {
	src := `
DB:
  Password: hunter2
  db_password: hunter3
  ApiKey: k-123
  AccessToken: t-123
  ClientSecret: s-123
  Empty: ""
  EmptyPassword: ""
  KeyPrefix: "order:"
  KeyFile: /etc/ssl/client-key.pem
  DSN: "postgres://app:pg-pw@db:5432/app?sslmode=disable"
  MySQL: "report:my-pw@tcp(report-db:3306)/report"
  Keyword: "host=db user=app password=kw-pw dbname=app"
  Query: "https://api.example.com/x?user=a&password=q-pw"
  Redis: "redis://:redis-pw@cache:6379/0"
  Plain: "no secrets here" # the comment is not carried out
`
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(redacted(&doc, nil))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, secret := range []string{"hunter2", "hunter3", "k-123", "t-123", "s-123", "pg-pw", "my-pw", "kw-pw", "q-pw", "redis-pw"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q must be masked, got=\n%s", secret, got)
		}
	}
	for _, keep := range []string{"order:", "/etc/ssl/client-key.pem", "postgres://app:***@db:5432/app?sslmode=disable",
		"report:***@tcp(report-db:3306)/report", "password=***", "no secrets here", `Empty: ""`,
		`EmptyPassword: ""`} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q must be kept, got=\n%s", keep, got)
		}
	}
	if strings.Contains(got, "the comment is not carried out") {
		t.Errorf("the merged configuration must carry no comment, got=\n%s", got)
	}
	// What is masked is a copy: the real configuration still holds the password
	if orig, _ := yaml.Marshal(&doc); !strings.Contains(string(orig), "hunter2") || !strings.Contains(string(orig), "pg-pw") {
		t.Errorf("the original node must not be modified, got=\n%s", orig)
	}
}

// debugLoad writes files into dir, loads base with XONE_DEBUG on, and returns the debug output
func debugLoad(t *testing.T, dir string, files map[string]string, base string) string {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var b bytes.Buffer
	old := DebugOut
	DebugOut = &b
	t.Cleanup(func() { DebugOut = old })
	Reset()
	t.Cleanup(Reset)
	if err := Ensure(filepath.Join(dir, base), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestEnsure_XONE_DEBUGPrintsFileOrderProfilesAndFinalConfig(t *testing.T) {
	t.Setenv(DebugEnvKey, "1")
	t.Setenv(ProfileEnvKey, "prod")
	t.Setenv("ORDER_TOKEN", "tok-s3cret")
	dir := t.TempDir()
	out := debugLoad(t, dir, map[string]string{
		"application.yml":      "XApp:\n  Import: [common/log.yml]\nOrder:\n  Channels: [a, b]\n",
		"common/log.yml":       "XLog:\n  Level: info\n",
		"application-prod.yml": "Order:\n  Channels: [a]\n  Token: \"${ORDER_TOKEN}\"\n",
	}, "application.yml")

	for _, want := range []string{
		"config file: " + filepath.Join(dir, "application.yml") + " (xone.WithConfigPath)",
		"profiles: prod (from XONE_PROFILE)",
		"1. " + filepath.Join(dir, "application.yml"),
		"2. " + filepath.Join(dir, "common/log.yml"),
		"3. " + filepath.Join(dir, "application-prod.yml"),
		"effective config",
		"Level: info",
		"Token: '***'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the debug output must hold %q, got=\n%s", want, out)
		}
	}
	// The final configuration is the merged one: prod's list replaced base's whole
	if !strings.Contains(out, "Channels: [a]") {
		t.Errorf("the merged final value must be printed (Channels holding a alone), got=\n%s", out)
	}
	if strings.Contains(out, "tok-s3cret") {
		t.Errorf("no credential may appear in the debug output, got=\n%s", out)
	}
}

func TestEnsure_PrintsNothingWithoutXONE_DEBUG(t *testing.T) {
	t.Setenv(DebugEnvKey, "")
	out := debugLoad(t, t.TempDir(), map[string]string{"application.yml": "XLog:\n  Level: info\n"}, "application.yml")
	if out != "" {
		t.Errorf("no debug output may appear without XONE_DEBUG, got=\n%s", out)
	}
}

// redactedYAML parses src, masks the credentials, and returns the rendered text
func redactedYAML(t *testing.T, src string) string {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(redacted(&doc, nil))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestRedacted_AuthorizationAndSignatureAreMasked(t *testing.T) {
	// They carry a credential as surely as Password does, and neither has an everyday spelling that
	// does not. cookie and session are the two words the access log masks and this does not: they
	// would take CookieDomain, CookieSameSite, SessionTimeout and beego's SessionOn with them, and
	// what is hidden here is what somebody turned XONE_DEBUG on to read.
	got := redactedYAML(t, `
Demo:
  Authorization: "Bearer a-token"
  Webhook_Signature: sig-123
  CookieDomain: example.com
  SessionTimeout: 30m
  SessionOn: true
`)
	for _, secret := range []string{"Bearer a-token", "sig-123"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q must be masked, got=\n%s", secret, got)
		}
	}
	for _, keep := range []string{"example.com", "30m", "true"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q is not a credential and must stay readable, got=\n%s", keep, got)
		}
	}
}

func TestRedacted_MasksWholeValueOfAnyShapeUnderSensitiveKey(t *testing.T) {
	// Only scalars used to be masked: a credential written as a list or a map was printed as it
	// was
	got := redactedYAML(t, `
Tokens: [tok-a, tok-b]
Secrets:
  Stripe: sk-live-1
  Nested: {Deep: sk-live-2}
Password: 12345
Credentials: {}
TokenList: []
`)
	for _, secret := range []string{"tok-a", "tok-b", "sk-live-1", "sk-live-2", "Stripe", "12345"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q must be masked, got=\n%s", secret, got)
		}
	}
	for _, keep := range []string{"Tokens: '***'", "Secrets: '***'", "Password: '***'", "Credentials: {}", "TokenList: []"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q must be there (an empty one is not masked), got=\n%s", keep, got)
		}
	}
}

func TestRedacted_DSNPasswordMaskedUpToLastAt(t *testing.T) {
	// A password holding @ or / used to be masked up to the first @ alone, printing the rest along
	// with the host as it was
	got := redactedYAML(t, `
A: "postgres://u:p@ss@db:5432/app"
B: "postgres://u:p/ss@db:5432/app"
C: "u:p@ss@tcp(db:3306)/app"
D: "redis://:r@dis/pw@cache:6379/0"
`)
	for _, secret := range []string{"p@ss", "ss@", "p/ss", "r@dis"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q must be masked, got=\n%s", secret, got)
		}
	}
	for _, keep := range []string{"postgres://u:***@db:5432/app", "u:***@tcp(db:3306)/app", "redis://:***@cache:6379/0"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q must be there, got=\n%s", keep, got)
		}
	}
}

func TestRedacted_SensitiveQueryParamsMasked(t *testing.T) {
	// token= and api_key= in a query string used to be printed as they were, and password= was the
	// only spelling masked
	got := redactedYAML(t, `
URL: "https://api.example.com/x?token=q-tok&api_key=q-key&access_token=q-at&sslpassword=q-ssl&pwd=q-pwd&page=2"
Keyword: "host=db user=app client_secret=kw-sec dbname=app"
`)
	for _, secret := range []string{"q-tok", "q-key", "q-at", "q-ssl", "q-pwd", "kw-sec"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q must be masked, got=\n%s", secret, got)
		}
	}
	for _, keep := range []string{"token=***&api_key=***", "page=2", "user=app", "dbname=app"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q must be there, got=\n%s", keep, got)
		}
	}
}

func TestEnsure_XONE_DEBUGPlaceholderDefaultMaskedKeepsClosingBrace(t *testing.T) {
	// A query string value is masked up to the whitespace or the &, and the closing } of a ${...}
	// used to be masked along with it: what was printed read ${OPT:https://h/?api_key=***, which
	// looks like an unfinished placeholder
	t.Setenv(DebugEnvKey, "1")
	out := debugLoad(t, t.TempDir(), map[string]string{
		"application.yml": "Demo:\n  URL: ${XONE_T_UNSET_URL:https://h/?api_key=def-key}\n" +
			"  Hook: ${XONE_T_UNSET_HOOK:https://h/x?page=2&token=def-tok}/tail\n",
	}, "application.yml")
	for _, secret := range []string{"def-key", "def-tok"} {
		if strings.Contains(out, secret) {
			t.Errorf("%q must not appear in the debug output, got=\n%s", secret, out)
		}
	}
	for _, want := range []string{"URL: ${XONE_T_UNSET_URL:https://h/?api_key=***}", "Hook: ${XONE_T_UNSET_HOOK:https://h/x?page=2&token=***}/tail"} {
		if !strings.Contains(out, want) {
			t.Errorf("the debug output must hold %q, got=\n%s", want, out)
		}
	}
}

func TestEnsure_XONE_DEBUGShowsPlaceholderTextNotExpandedValue(t *testing.T) {
	// An expanded value used to be printed verbatim, and ${VAR} is exactly the recommended way to
	// write a credential: under a key not named password (DSN, Webhook) it went into the debug output.
	t.Setenv(DebugEnvKey, "1")
	t.Setenv("XONE_T_WEBHOOK", "https://hooks.example.com/services/T0/B0/s3cr3t")
	t.Setenv("XONE_T_PORT", "8081")
	// The debug output names every file it read, so the config path is part of what is searched
	// below. t.TempDir() appends a random number that can contain a value under test: on
	// 2026-10-20 it produced .../Value80819342/001, main went red on "8081", and nothing had
	// leaked. Put all three values in the directory name so the collision always happens, then
	// strip the path before asserting — the test now proves the stripping works instead of
	// relying on a random number to miss.
	dir := filepath.Join(t.TempDir(), "port-8081-hook-s3cr3t-dsn-def-pw")
	raw := debugLoad(t, dir, map[string]string{
		"application.yml": "Demo:\n  Webhook: ${XONE_T_WEBHOOK}\n  Port: ${XONE_T_PORT}\n" +
			"  DSN: ${XONE_T_UNSET_DSN:postgres://u:def-pw@db/app}\n  Password: ${XONE_T_UNSET_PW:}\n  Token: ${XONE_T_PORT}\n",
	}, "application.yml")
	out := strings.ReplaceAll(raw, dir, "<tmpdir>")
	if !strings.Contains(out, "<tmpdir>") {
		t.Fatalf("debug output never names the directory we created, so stripping it proves nothing; got=\n%s", raw)
	}
	if strings.Contains(out, "Password: '***'") || !strings.Contains(out, "Token: '***'") {
		t.Errorf("under a credential key an empty expansion stays plain and a non-empty one is masked, got=\n%s", out)
	}
	for _, secret := range []string{"s3cr3t", "8081", "def-pw"} {
		if strings.Contains(out, secret) {
			t.Errorf("%q must not appear in the debug output, got=\n%s", secret, out)
		}
	}
	for _, want := range []string{"Webhook: ${XONE_T_WEBHOOK}", "Port: ${XONE_T_PORT}", "DSN: ${XONE_T_UNSET_DSN:postgres://u:***@db/app}"} {
		if !strings.Contains(out, want) {
			t.Errorf("the debug output should contain %q, got=\n%s", want, out)
		}
	}
}
