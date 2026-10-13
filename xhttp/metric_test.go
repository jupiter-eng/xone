package xhttp

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/jupiter-eng/xone/internal/testkit"
	"github.com/jupiter-eng/xone/xmetric"
)

// withMetrics 装一套独立的指标设施并让本包用上它
func withMetrics(t *testing.T) *xmetric.Metrics {
	t.Helper()
	m, closer, err := xmetric.New(xmetric.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closer.Close() })
	m.Install()
	return m
}

func TestMetric_RecordsStatusAndDuration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()

	client, m := newQuiet(t, DefaultConfig())
	if _, err := client.R().SetContext(context.Background()).Get(srv.URL); err != nil {
		t.Fatal(err)
	}

	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `status="503"`) || !strings.Contains(out, `method="GET"`) {
		t.Errorf("应按方法和状态码分标签\n实际=\n%s", out)
	}
	if !strings.Contains(out, "http_client_request_duration_seconds_count") {
		t.Errorf("应记录耗时\n实际=\n%s", out)
	}
}

func TestMetric_NetworkErrorRecordsStatus0(t *testing.T) {
	// 把它和真实状态码混在一起，会让「5xx 比例」这类告警在网络故障时反而看不出问题
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	client, m := newQuiet(t, DefaultConfig())
	client.R().SetContext(context.Background()).Get(srv.URL)

	if out := testkit.Scrape(m.Handler); !strings.Contains(out, `status="0"`) {
		t.Errorf("网络错误应记状态码 0\n实际=\n%s", out)
	}
}

func TestMetric_RetriesRecordedOnce(t *testing.T) {
	// 挂在中间件上会把每次重试都记一遍，请求量和耗时分布都虚高
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	c := DefaultConfig()
	c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 2, time.Millisecond, 5*time.Millisecond
	client, m := newQuiet(t, c)
	client.R().SetContext(context.Background()).Get(srv.URL)

	if got := hits.Load(); got != 3 {
		t.Fatalf("应当真的重试了，got=%d 次请求", got)
	}
	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `status="0"} 1`) {
		t.Errorf("重试 3 次也只该记 1 个样本\n实际=\n%s", out)
	}
}

func TestMetric_UsesXMetricBucketsAndLabels(t *testing.T) {
	// 出站和入站的耗时要在同一把刻度上，看板才对得起来
	m, closer, err := xmetric.New(func() xmetric.Config {
		c := xmetric.DefaultConfig()
		c.GoMetrics, c.ProcessMetrics, c.LogErrorMetric = false, false, false
		c.Namespace = "demo"
		c.ConstLabels = map[string]string{"env": "prod"}
		c.HTTPDurationBuckets = []float64{0.5}
		return c
	}())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	m.Install()

	srv, _ := echo(t, nil)
	client, closer, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	client.SetLogger(discardLogger{})
	client.R().SetContext(context.Background()).Get(srv.URL)

	out := testkit.Scrape(m.Handler)
	if !strings.Contains(out, `demo_http_client_request_duration_seconds_bucket{env="prod"`) {
		t.Errorf("应带上前缀和常量标签\n实际=\n%s", out)
	}
	if strings.Contains(out, `le="0.25"`) {
		t.Errorf("应当用配置里的桶\n实际=\n%s", out)
	}
}

func TestMetric_DuplicateRegisterReusesExisting(t *testing.T) {
	// 建两个 client 时第二个的指标必须落在已注册的那个上，否则记的值导不出去
	m := withMetrics(t)
	srv, _ := echo(t, nil)

	for i := 0; i < 2; i++ {
		client, closer, err := New(DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer closer.Close()
		client.SetLogger(discardLogger{})
		if _, err := client.R().SetContext(context.Background()).Get(srv.URL); err != nil {
			t.Fatal(err)
		}
	}

	if out := testkit.Scrape(m.Handler); !strings.Contains(out, `status="204"} 2`) {
		t.Errorf("两个 client 的请求应记在同一条序列上\n实际=\n%s", out)
	}
}

func TestNew_MetricRegisterFailureOnlyLogs_ClientStillUsable(t *testing.T) {
	// 指标导不出去是可观测性问题，与 xgin / xgorm / xredis 一致：
	// 不该让所有出站调用跟着起不来
	m := withMetrics(t)
	m.Registry.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{
		Name: "http_client_request_duration_seconds", Help: "占位",
	}))
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf}, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	client, closer, err := New(DefaultConfig())
	if err != nil {
		t.Fatalf("指标名被占成别的类型时 New 不该失败：%v", err)
	}
	defer closer.Close()
	client.SetLogger(discardLogger{})
	srv, _ := echo(t, nil)
	if _, err := client.R().SetContext(context.Background()).Get(srv.URL); err != nil {
		t.Fatalf("注册失败之后客户端照样要能用：%v", err)
	}
	if got := buf.String(); !strings.Contains(got, "xhttp failed to register the request duration metric") || !strings.Contains(got, `"level":"ERROR"`) {
		t.Errorf("注册失败要打一条错误日志，got=%s", got)
	}
}

func TestMetric_MethodLabelCollapsedToFixedSet(t *testing.T) {
	// 方法是自由 token：照抄进标签的话 CUSTOM1、CUSTOM2 各是一组时间序列
	srv, _ := echo(t, nil)
	client, m := newQuiet(t, DefaultConfig())
	for _, method := range []string{"CUSTOM1", "CUSTOM2", "get", http.MethodPatch} {
		if _, err := client.R().SetContext(context.Background()).Execute(method, srv.URL); err != nil {
			t.Fatal(err)
		}
	}

	out := testkit.Scrape(m.Handler)
	for _, bad := range []string{`method="CUSTOM1"`, `method="CUSTOM2"`, `method="get"`} {
		if strings.Contains(out, bad) {
			t.Errorf("不认识的方法不该原样进标签：%s\n实际=\n%s", bad, out)
		}
	}
	if !strings.Contains(out, `method="OTHER"`) || !strings.Contains(out, `method="PATCH"`) {
		t.Errorf("不认识的记成 OTHER、认识的原样保留\n实际=\n%s", out)
	}
}

func TestNormalizeMethod(t *testing.T) {
	for in, want := range map[string]string{
		"GET": "GET", "POST": "POST", "PATCH": "PATCH", "CONNECT": "CONNECT",
		"get": methodOther, "CUSTOM": methodOther, "": methodOther,
	} {
		if got := normalizeMethod(in); got != want {
			t.Errorf("normalizeMethod(%q)=%q，want %q", in, got, want)
		}
	}
}

func TestElapsed_FallsBackToAttemptDurationWithoutStart(t *testing.T) {
	// 起点是在第一次尝试之前写进 context 的：拿不到就说明这条路径上
	// 没经过那个中间件，此时报这一次尝试的耗时，好过报 0
	want := 3 * time.Second
	if got := elapsed(nil, want); got != want {
		t.Errorf("req 为 nil 时应退回 fallback，got=%v", got)
	}

	req := resty.New().R()
	if got := elapsed(req, want); got != want {
		t.Errorf("context 里没有起点时应退回 fallback，got=%v", got)
	}
}

func TestElapsed_WithStartCoversWholeLogicalRequest(t *testing.T) {
	// 重试三次的话，每次尝试各自的耗时加起来才是调用方等的时间；
	// 只报最后一次会让 P99 看上去比实际好得多
	start := time.Now().Add(-2 * time.Second)
	req := resty.New().R().SetContext(context.WithValue(context.Background(), startKey{}, start))

	got := elapsed(req, time.Millisecond)
	if got < 2*time.Second {
		t.Errorf("应当从起点算起，got=%v", got)
	}
}
