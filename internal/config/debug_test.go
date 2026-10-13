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
			t.Errorf("XONE_DEBUG=%q：Debug()=%v，want %v", v, got, want)
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
  Plain: "no secrets here" # 注释不带出来
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
			t.Errorf("%q 该被遮掉，got=\n%s", secret, got)
		}
	}
	for _, keep := range []string{"order:", "/etc/ssl/client-key.pem", "postgres://app:***@db:5432/app?sslmode=disable",
		"report:***@tcp(report-db:3306)/report", "password=***", "no secrets here", `Empty: ""`,
		`EmptyPassword: ""`} {
		if !strings.Contains(got, keep) {
			t.Errorf("该保留 %q，got=\n%s", keep, got)
		}
	}
	if strings.Contains(got, "注释不带出来") {
		t.Errorf("合并之后的配置不该带着注释，got=\n%s", got)
	}
	// 遮的是拷贝：真正的配置照样拿得到密码
	if orig, _ := yaml.Marshal(&doc); !strings.Contains(string(orig), "hunter2") || !strings.Contains(string(orig), "pg-pw") {
		t.Errorf("原节点不该被改，got=\n%s", orig)
	}
}

// debugLoad 在 dir 里写好 files，打开 XONE_DEBUG 加载 base，返回调试输出
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
			t.Errorf("调试输出里该有 %q，got=\n%s", want, out)
		}
	}
	// 最终配置是合并之后的：prod 的列表整体替换了 base 的
	if !strings.Contains(out, "Channels: [a]") {
		t.Errorf("该打合并之后的最终值（Channels 只剩 a），got=\n%s", out)
	}
	if strings.Contains(out, "tok-s3cret") {
		t.Errorf("凭证不该出现在调试输出里，got=\n%s", out)
	}
}

func TestEnsure_PrintsNothingWithoutXONE_DEBUG(t *testing.T) {
	t.Setenv(DebugEnvKey, "")
	out := debugLoad(t, t.TempDir(), map[string]string{"application.yml": "XLog:\n  Level: info\n"}, "application.yml")
	if out != "" {
		t.Errorf("没开 XONE_DEBUG 不该有调试输出，got=\n%s", out)
	}
}

// redactedYAML 解析 src、遮掉凭证，返回渲染出来的文本
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

func TestRedacted_MasksWholeValueOfAnyShapeUnderSensitiveKey(t *testing.T) {
	// 从前只遮标量：凭证写成列表或 map 时原样打了出来
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
			t.Errorf("%q 该被遮掉，got=\n%s", secret, got)
		}
	}
	for _, keep := range []string{"Tokens: '***'", "Secrets: '***'", "Password: '***'", "Credentials: {}", "TokenList: []"} {
		if !strings.Contains(got, keep) {
			t.Errorf("该有 %q（空的不遮），got=\n%s", keep, got)
		}
	}
}

func TestRedacted_DSNPasswordMaskedUpToLastAt(t *testing.T) {
	// 密码里带 @ 或 / 时从前只遮到第一个 @，后半截连同主机原样打出来
	got := redactedYAML(t, `
A: "postgres://u:p@ss@db:5432/app"
B: "postgres://u:p/ss@db:5432/app"
C: "u:p@ss@tcp(db:3306)/app"
D: "redis://:r@dis/pw@cache:6379/0"
`)
	for _, secret := range []string{"p@ss", "ss@", "p/ss", "r@dis"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q 该被遮掉，got=\n%s", secret, got)
		}
	}
	for _, keep := range []string{"postgres://u:***@db:5432/app", "u:***@tcp(db:3306)/app", "redis://:***@cache:6379/0"} {
		if !strings.Contains(got, keep) {
			t.Errorf("该有 %q，got=\n%s", keep, got)
		}
	}
}

func TestRedacted_SensitiveQueryParamsMasked(t *testing.T) {
	// 查询串里的 token=、api_key= 从前原样打出来，只有 password= 一种写法被遮
	got := redactedYAML(t, `
URL: "https://api.example.com/x?token=q-tok&api_key=q-key&access_token=q-at&sslpassword=q-ssl&pwd=q-pwd&page=2"
Keyword: "host=db user=app client_secret=kw-sec dbname=app"
`)
	for _, secret := range []string{"q-tok", "q-key", "q-at", "q-ssl", "q-pwd", "kw-sec"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q 该被遮掉，got=\n%s", secret, got)
		}
	}
	for _, keep := range []string{"token=***&api_key=***", "page=2", "user=app", "dbname=app"} {
		if !strings.Contains(got, keep) {
			t.Errorf("该有 %q，got=\n%s", keep, got)
		}
	}
}

func TestEnsure_XONE_DEBUGPlaceholderDefaultMaskedKeepsClosingBrace(t *testing.T) {
	// 查询串的值一路遮到空白或 & 为止，从前连 ${...} 收尾的 } 也一起遮掉：
	// 打出来是 ${OPT:https://h/?api_key=***，看着像占位符没写完
	t.Setenv(DebugEnvKey, "1")
	out := debugLoad(t, t.TempDir(), map[string]string{
		"application.yml": "Demo:\n  URL: ${XONE_T_UNSET_URL:https://h/?api_key=def-key}\n" +
			"  Hook: ${XONE_T_UNSET_HOOK:https://h/x?page=2&token=def-tok}/tail\n",
	}, "application.yml")
	for _, secret := range []string{"def-key", "def-tok"} {
		if strings.Contains(out, secret) {
			t.Errorf("%q 不该出现在调试输出里，got=\n%s", secret, out)
		}
	}
	for _, want := range []string{"URL: ${XONE_T_UNSET_URL:https://h/?api_key=***}", "Hook: ${XONE_T_UNSET_HOOK:https://h/x?page=2&token=***}/tail"} {
		if !strings.Contains(out, want) {
			t.Errorf("调试输出里该有 %q，got=\n%s", want, out)
		}
	}
}

func TestEnsure_XONE_DEBUGShowsPlaceholderTextNotExpandedValue(t *testing.T) {
	// 展开出来的值从前原样打出来：${VAR} 恰恰是凭证的推荐写法，
	// 放在一个不叫 password 的 key 下面（DSN、Webhook）就进了调试输出
	t.Setenv(DebugEnvKey, "1")
	t.Setenv("XONE_T_WEBHOOK", "https://hooks.example.com/services/T0/B0/s3cr3t")
	t.Setenv("XONE_T_PORT", "8081")
	out := debugLoad(t, t.TempDir(), map[string]string{
		"application.yml": "Demo:\n  Webhook: ${XONE_T_WEBHOOK}\n  Port: ${XONE_T_PORT}\n" +
			"  DSN: ${XONE_T_UNSET_DSN:postgres://u:def-pw@db/app}\n  Password: ${XONE_T_UNSET_PW:}\n  Token: ${XONE_T_PORT}\n",
	}, "application.yml")
	if strings.Contains(out, "Password: '***'") || !strings.Contains(out, "Token: '***'") {
		t.Errorf("凭证 key 下展开为空的不遮、有值的遮，got=\n%s", out)
	}
	for _, secret := range []string{"s3cr3t", "8081", "def-pw"} {
		if strings.Contains(out, secret) {
			t.Errorf("%q 不该出现在调试输出里，got=\n%s", secret, out)
		}
	}
	for _, want := range []string{"Webhook: ${XONE_T_WEBHOOK}", "Port: ${XONE_T_PORT}", "DSN: ${XONE_T_UNSET_DSN:postgres://u:***@db/app}"} {
		if !strings.Contains(out, want) {
			t.Errorf("调试输出里该有 %q，got=\n%s", want, out)
		}
	}
}
