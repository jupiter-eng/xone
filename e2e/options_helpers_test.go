package e2e

// The coverage tests: the TestCoverage_* family, filling in the modules and settings
// functional_* / fault_* / shutdown_* do not touch.
//
//	scripts/e2e.sh -run Coverage
//
// As in functional_*, every assertion follows a sentence written down in README.md /
// docs/config.md / a module's README, and a failure message reads "the documentation says X, got
// Y"; the numbers measured are printed with t.Logf behind a "numbers:" prefix.
// The endpoints the service under test provides for them all sit under /probe, see
// service/probe.go.
//
// How the files divide up:
//
//	options_helpers_test.go   this file: the shared helpers
//	swagger_ui_test.go        xginswagger
//	gin_options_test.go       xgin: TrustedProxies, ReadHeaderTimeout, MaxMultipartMemory, TLS, h2c,
//	                          LogSkipPaths, MetricPath, ZHTranslations, 404 / 405
//	trace_options_test.go     xtrace: ForwardHeaderRules, SampleRatio, Console, service.name
//	log_options_test.go       xlog: the file rotation and cleanup, Perm, Level, Format, Timezone, AddSource
//	metric_options_test.go    xmetric: Namespace, ConstLabels; xapp
//	client_options_test.go    xcache, several xredis instances and the retries, xhttp sharing no cookies
//	flow_options_test.go      xflow: a rollback that hangs, the Monitor switch
//	config_loading_test.go    profiles, Import, the placeholders, --config / XONE_CONFIG, WithConfigPath

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

// covConfig replaces the top-level blocks of service/application.yml (an empty value deletes one)
// and appends the blocks that are not there, writes it as a temporary configuration and returns
// the path.
//
// It is for the cases Overlay cannot do: Overlay merges maps recursively, and laying a Clients
// over a single-instance XRedis block makes it "both styles written at once" and fails the start;
// turning it into the several-instances style takes replacing the whole block.
// A block to be replaced has to exist in application.yml (an appended one aside), and not finding
// it fails there and then rather than quietly testing the original configuration
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
		// A top-level key: the line starts with neither whitespace nor a comment
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
				t.Fatalf("service/application.yml holds no top-level block %s, which cannot be deleted", k)
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

// covWrite writes one file under dir and returns the path
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

// covAccessLogs are the access log lines so far whose path equals this one
func covAccessLogs(p *harness.Process, path string) []harness.Log {
	return p.FindLogs(func(l harness.Log) bool { return l.Msg() == "request completed" && l.Str("path") == path })
}

// covHTTPSClient is an HTTPS client trusting the certificate in pool alone, allowing HTTP/2 to be negotiated
func covHTTPSClient(pool *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: pool},
			ForceAttemptHTTP2: true,
		},
	}
}

// covH2CClient is a client that speaks h2c "with prior knowledge" alone: it sends the HTTP/2 preface straight away (the way curl --http2-prior-knowledge does)
func covH2CClient() *http.Client {
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Protocols: &p}}
}

// covStartupError starts a service that is bound to fail and returns stderr (where MustRun writes the error)
func covStartupError(t *testing.T, o harness.Options) string {
	t.Helper()
	_, p := faultStartFails(t, o)
	return p.Stderr()
}
