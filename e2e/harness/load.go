package harness

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// LoadSpec 一次压测。Duration 和 Requests 至少给一个，都给时哪个先到算哪个
type LoadSpec struct {
	// Concurrency 固定并发数，默认 1
	Concurrency int
	// Duration 跑多久。到点时在途的请求照样等它做完、照样计数
	Duration time.Duration
	// Requests 一共发多少个
	Requests int

	// Method / URL / Body / Header 每个请求都一样时用这几个。Method 默认 GET
	Method string
	URL    string
	Body   []byte
	Header http.Header

	// NewRequest 每个请求不一样时用它（比如轮着读不同的 id），i 从 0 开始。给了就不看上面四个
	NewRequest func(ctx context.Context, i int) (*http.Request, error)

	// Timeout 单个请求的超时，默认 10s
	Timeout time.Duration
}

// LoadResult 压测结果。延迟统计的是每个请求从发出到读完 body，出错的请求也算在内
type LoadResult struct {
	// Requests 发出去的请求数，Errors 其中传输层失败（没拿到响应）的个数
	Requests int
	Errors   int
	// Status 拿到响应的请求按状态码分桶
	Status  map[int]int
	Elapsed time.Duration
	QPS     float64

	P50, P90, P99, Max time.Duration

	// ErrorSamples 前几个不同的传输层错误，排查用
	ErrorSamples []string
}

// OK 状态码在 [200, 300) 的请求数
func (r LoadResult) OK() int {
	n := 0
	for code, c := range r.Status {
		if code >= 200 && code < 300 {
			n += c
		}
	}
	return n
}

func (r LoadResult) String() string {
	codes := make([]int, 0, len(r.Status))
	for c := range r.Status {
		codes = append(codes, c)
	}
	slices.Sort(codes)
	parts := make([]string, len(codes))
	for i, c := range codes {
		parts[i] = fmt.Sprintf("%d:%d", c, r.Status[c])
	}
	return fmt.Sprintf("requests=%d errors=%d qps=%.0f p50=%v p90=%v p99=%v max=%v elapsed=%v status=[%s]",
		r.Requests, r.Errors, r.QPS, r.P50, r.P90, r.P99, r.Max, r.Elapsed.Round(time.Millisecond), strings.Join(parts, " "))
}

// Load 按 spec 压一轮。连接池按并发数配（每个 worker 一条长连接），跑完就关
func Load(t testing.TB, spec LoadSpec) LoadResult {
	t.Helper()
	if spec.Duration <= 0 && spec.Requests <= 0 {
		t.Fatal("LoadSpec needs Duration or Requests")
	}
	if spec.NewRequest == nil && spec.URL == "" {
		t.Fatal("LoadSpec needs URL or NewRequest")
	}
	conc := max(spec.Concurrency, 1)
	tr := &http.Transport{
		MaxIdleConns:        conc,
		MaxIdleConnsPerHost: conc,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: or(spec.Timeout, 10*time.Second)}

	newReq := spec.NewRequest
	if newReq == nil {
		newReq = func(ctx context.Context, _ int) (*http.Request, error) {
			var body io.Reader
			if spec.Body != nil {
				body = bytes.NewReader(spec.Body)
			}
			req, err := http.NewRequestWithContext(ctx, or(spec.Method, http.MethodGet), spec.URL, body)
			if err == nil && spec.Header != nil {
				req.Header = spec.Header.Clone()
			}
			return req, err
		}
	}

	var (
		next   atomic.Int64
		mu     sync.Mutex
		lat    []time.Duration
		status = map[int]int{}
		errs   int
		sample []string
		wg     sync.WaitGroup
	)
	start := time.Now()
	end := start.Add(spec.Duration)
	for range conc {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var myLat []time.Duration
			myStatus := map[int]int{}
			var myErrs int
			var mySample []string
			for {
				i := int(next.Add(1) - 1)
				if spec.Requests > 0 && i >= spec.Requests {
					break
				}
				if spec.Duration > 0 && time.Now().After(end) {
					break
				}
				t0 := time.Now()
				code, err := once(client, newReq, i)
				myLat = append(myLat, time.Since(t0))
				if err != nil {
					myErrs++
					if len(mySample) < 5 && !slices.Contains(mySample, err.Error()) {
						mySample = append(mySample, err.Error())
					}
				} else {
					myStatus[code]++
				}
			}
			mu.Lock()
			defer mu.Unlock()
			lat = append(lat, myLat...)
			for c, n := range myStatus {
				status[c] += n
			}
			errs += myErrs
			for _, e := range mySample {
				if len(sample) < 5 && !slices.Contains(sample, e) {
					sample = append(sample, e)
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	slices.Sort(lat)
	r := LoadResult{Requests: len(lat), Errors: errs, Status: status, Elapsed: elapsed, ErrorSamples: sample}
	if len(lat) > 0 {
		r.QPS = float64(len(lat)) / elapsed.Seconds()
		r.P50, r.P90, r.P99 = percentile(lat, 0.50), percentile(lat, 0.90), percentile(lat, 0.99)
		r.Max = lat[len(lat)-1]
	}
	return r
}

func once(c *http.Client, newReq func(context.Context, int) (*http.Request, error), i int) (int, error) {
	req, err := newReq(context.Background(), i)
	if err != nil {
		return 0, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// 读完 body 连接才回得了池子，否则每个请求都新建连接
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return 0, err
	}
	return resp.StatusCode, nil
}

// percentile 最近秩法：排好序的样本里第 ceil(p*n) 个
func percentile(sorted []time.Duration, p float64) time.Duration {
	k := int(math.Ceil(float64(len(sorted))*p)) - 1
	return sorted[min(max(k, 0), len(sorted)-1)]
}
