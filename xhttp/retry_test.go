package xhttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
)

// flaky 第 1 次请求读完 body 之后掐断连接（传输层错误），之后回 status。
// 记下每次收到的 body 长度
type flaky struct {
	mu     sync.Mutex
	bodies []int
	status int
}

func newFlaky(t *testing.T, status int) (*flaky, string) {
	f := &flaky{status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, len(b))
		first := len(f.bodies) == 1
		f.mu.Unlock()
		if first && f.status == 0 {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		w.WriteHeader(cmpStatus(f.status))
	}))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func cmpStatus(s int) int {
	if s == 0 {
		return http.StatusOK
	}
	return s
}

func (f *flaky) got() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.bodies...)
}

func retrying(t *testing.T, onlyIdempotent bool) *resty.Client {
	c := DefaultConfig()
	c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 2, time.Millisecond, 5*time.Millisecond
	c.RetryOnlyIdempotent = onlyIdempotent
	client, _ := newQuiet(t, c)
	return client
}

const payload = "payload-that-must-not-be-lost"

// resty v2.17.2 每次尝试都拿 Request.Body 重新建 http.Request（middleware.go createHTTPRequest），
// io.Reader 在第一次就读到了头：第二次发出去的是空 body，服务端回 200，调用方看到的是成功
func TestRetry_ConsumedReaderBodyNotResent(t *testing.T) {
	for _, only := range []bool{true, false} {
		for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPost} {
			if only && method == http.MethodPost {
				continue // POST 在这一档本来就不重试，见 TestRetry_ActuallyResends
			}
			f, url := newFlaky(t, 0)
			client := retrying(t, only)
			resp, err := client.R().SetContext(context.Background()).SetBody(strings.NewReader(payload)).Execute(method, url)
			if got := f.got(); len(got) != 1 || got[0] != len(payload) {
				t.Errorf("RetryOnlyIdempotent=%v %s：io.Reader 的 body 只能发一次，每次收到的长度 got=%v", only, method, got)
			}
			if err == nil {
				t.Errorf("RetryOnlyIdempotent=%v %s：第一次的传输层错误该原样交给调用方，got status=%v", only, method, resp.StatusCode())
			}
		}
	}
}

// 对照：[]byte、string、结构体的 body 每次重建都是完整的，照旧重试
func TestRetry_RewindableBodyStillRetried(t *testing.T) {
	for name, body := range map[string]any{
		"[]byte": []byte(payload), "string": payload, "map": map[string]string{"k": payload},
		"没有 body": nil,
	} {
		f, url := newFlaky(t, 0)
		client := retrying(t, true)
		r := client.R().SetContext(context.Background())
		if body != nil {
			r.SetBody(body)
		}
		if _, err := r.Put(url); err != nil {
			t.Errorf("%s：重试之后该成功，got=%v", name, err)
		}
		got := f.got()
		if len(got) != 2 || got[0] != got[1] {
			t.Errorf("%s：该重试一次、两次收到的 body 一样长，got=%v", name, got)
		}
	}
}

// SetContentLength(true) 时 resty 把 io.Reader 读进缓冲、Body 置 nil，之后每次发那份缓冲（middleware.go handleRequestBody）
func TestRetry_ReaderBufferedBySetContentLengthStillRetried(t *testing.T) {
	f, url := newFlaky(t, 0)
	client := retrying(t, true)
	if _, err := client.R().SetContext(context.Background()).SetContentLength(true).SetBody(strings.NewReader(payload)).Put(url); err != nil {
		t.Errorf("重试之后该成功，got=%v", err)
	}
	if got := f.got(); len(got) != 2 || got[0] != len(payload) || got[1] != len(payload) {
		t.Errorf("该重试一次、两次都是完整的 body，got=%v", got)
	}
}

// resty 的 AddRetryCondition 是「或」：挂了任何一个返回 true 的，前面的 false 就不作数
// （v2.17.2 retry.go Backoff）。使用者为了别的请求挂的条件，不能把 POST 的重试放回来
func TestRetry_UserConditionCannotReenableNonIdempotentRetry(t *testing.T) {
	f, url := newFlaky(t, 0)
	client := retrying(t, true)
	client.AddRetryCondition(func(_ *resty.Response, err error) bool { return err != nil })
	_, err := client.R().SetContext(context.Background()).SetBody(payload).Post(url)
	if got := f.got(); len(got) != 1 {
		t.Errorf("POST 不该重试，got %d 次", len(got))
	}
	if err == nil || strings.Contains(err.Error(), "xhttp") {
		t.Errorf("调用方拿到的该是第一次的传输层错误，不是我们的否决，got=%v", err)
	}

	// 请求级的条件排在最前面，一样挡住
	f, url = newFlaky(t, 0)
	_, _ = client.R().SetContext(context.Background()).
		AddRetryCondition(func(*resty.Response, error) bool { return true }).Post(url)
	if got := f.got(); len(got) != 1 {
		t.Errorf("请求级条件也不该让 POST 重试，got %d 次", len(got))
	}
}

// 按响应重试（使用者自己挂的 5xx 条件）挡不住：resty 只给 RetryAfter 一个出口，
// 拿到了响应时在那里否决，调用方拿到的就是我们编出来的错误，而不是那个 503。
// 这一档如实写在 Config.RetryOnlyIdempotent 和 README 里
func TestRetry_UserStatusConditionNotVetoedAndNoBogusError(t *testing.T) {
	f, url := newFlaky(t, http.StatusServiceUnavailable)
	client := retrying(t, true)
	client.AddRetryCondition(func(r *resty.Response, _ error) bool { return r.StatusCode() >= 500 })
	resp, err := client.R().SetContext(context.Background()).SetBody(payload).Post(url)
	if err != nil {
		t.Errorf("拿到了响应就不该冒出错误，got=%v", err)
	}
	if resp.StatusCode() != http.StatusServiceUnavailable {
		t.Errorf("调用方该拿到最后那个 503，got=%d", resp.StatusCode())
	}
	if got := f.got(); len(got) != 3 {
		t.Errorf("使用者按状态码重试的条件照旧生效，got %d 次", len(got))
	}
}

// 例外：body 是读过的 io.Reader 时，拿到了响应也不重试——再发就是一个空 body，比一个错误糟得多
func TestRetry_UserStatusConditionWithConsumedReaderFailsLoudly(t *testing.T) {
	f, url := newFlaky(t, http.StatusServiceUnavailable)
	client := retrying(t, true)
	client.AddRetryCondition(func(r *resty.Response, _ error) bool { return r.StatusCode() >= 500 })
	resp, err := client.R().SetContext(context.Background()).SetBody(strings.NewReader(payload)).Put(url)
	if got := f.got(); len(got) != 1 {
		t.Errorf("io.Reader 的 body 不该再发，got=%v", got)
	}
	if err == nil || !strings.Contains(err.Error(), "io.Reader") {
		t.Errorf("该说清为什么没重试，got=%v", err)
	}
	if resp == nil || resp.StatusCode() != http.StatusServiceUnavailable {
		t.Errorf("响应照样交给调用方")
	}
}
