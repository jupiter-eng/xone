package e2e

// 覆盖面测试：TestCoverage_* 系列，补上 functional_* / fault_* / shutdown_* 没碰到的模块和配置项。
//
//	scripts/e2e.sh -run Coverage
//
// 和 functional_* 一样，每条断言对照 README.md / docs/config.md / 各模块 README 里写下的一句话，
// 失败信息写成「文档说 X，实际 Y」；量出来的数字用 t.Logf 打出来，行首带「数字：」。
// 被测服务里给它们用的接口都挂在 /probe 下面，见 service/probe.go。
//
// 文件划分：
//
//	options_helpers_test.go   本文件：共用的小工具
//	swagger_ui_test.go        xginswagger
//	gin_options_test.go       xgin：TrustedProxies、ReadHeaderTimeout、MaxMultipartMemory、TLS、h2c、
//	                          LogSkipPaths、MetricPath、ZHTranslations、404 / 405
//	trace_options_test.go     xtrace：ForwardHeaderRules、SampleRatio、Console、service.name
//	log_options_test.go       xlog：文件轮转与清理、Perm、Level、Format、Timezone、AddSource
//	metric_options_test.go    xmetric：Namespace、ConstLabels；xapp
//	client_options_test.go    xcache、xredis 多实例与重试、xhttp 不串 cookie
//	flow_options_test.go      xflow：挂住的回滚、Monitor 开关
//	config_loading_test.go    profile、Import、占位符、--config / XONE_CONFIG、WithConfigPath

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// covConfig 把 service/application.yml 里的顶层块换掉（值为空串就是删掉），没有的块追加到末尾，
// 写成一份临时配置，返回路径。
//
// 给 Overlay 做不到的那几种：Overlay 是 map 递归合并，单实例的 XRedis 块上叠一个 Clients
// 就成了「两种写法混着写」、启动失败；要换成多实例写法只能把整块换掉。
// 要换的块在 application.yml 里必须存在（追加的除外），找不到时当场报错，
// 而不是悄悄测回原来的配置
func covConfig(t *testing.T, blocks map[string]string) string {
	t.Helper()
	base, err := os.ReadFile(filepath.Join(harness.ModuleDir(), "service", "application.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	replaced := map[string]bool{}
	skipping := false
	for _, l := range strings.Split(string(base), "\n") {
		// 顶层 key：行首不是空白、不是注释
		if l != "" && l[0] != ' ' && l[0] != '#' {
			key, _, _ := strings.Cut(l, ":")
			if body, ok := blocks[key]; ok {
				replaced[key], skipping = true, true
				if body != "" {
					out = append(out, strings.TrimRight(body, "\n"))
				}
				continue
			}
			skipping = false
		}
		if !skipping {
			out = append(out, l)
		}
	}
	keys := make([]string, 0, len(blocks))
	for k := range blocks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !replaced[k] {
			if blocks[k] == "" {
				t.Fatalf("service/application.yml 里没有顶层块 %s，删不掉", k)
			}
			out = append(out, strings.TrimRight(blocks[k], "\n"))
		}
	}
	cfg := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(cfg, []byte(strings.Join(out, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// covWrite 在 dir 下写一个文件，返回路径
func covWrite(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// covAccessLogs 到目前为止的访问日志里 path 等于它的那些
func covAccessLogs(p *harness.Process, path string) []harness.Log {
	return p.FindLogs(func(l harness.Log) bool { return l.Msg() == "request completed" && l.Str("path") == path })
}

// covHTTPSClient 只信 pool 里那张证书的 HTTPS 客户端，允许协商 HTTP/2
func covHTTPSClient(pool *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: pool},
			ForceAttemptHTTP2: true,
		},
	}
}

// covH2CClient 只说 h2c「先验知识」的客户端：一上来就发 HTTP/2 前言（curl --http2-prior-knowledge 那样）
func covH2CClient() *http.Client {
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Protocols: &p}}
}

// covStartupError 起一个注定起不来的服务，返回 stderr（MustRun 把错误写在那里）
func covStartupError(t *testing.T, o harness.Options) string {
	t.Helper()
	_, p := faultStartFails(t, o)
	return p.Stderr()
}
