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

// LoadSpec is one load round. At least one of Duration and Requests has to be given, and with both
// the first one reached wins
type LoadSpec struct {
	// Concurrency is the fixed number of concurrent requests, 1 by default
	Concurrency int
	// Duration is how long to run. The requests in flight when the time is up are still waited for
	// and still counted
	Duration time.Duration
	// Requests is how many to send in all
	Requests int

	// Method / URL / Body / Header are for when every request is the same. Method is GET by default
	Method string
	URL    string
	Body   []byte
	Header http.Header

	// NewRequest is for when the requests differ (reading one id after another, say), with i
	// starting at 0. Given, it replaces the four fields above
	NewRequest func(ctx context.Context, i int) (*http.Request, error)

	// Timeout is one request's timeout, 10s by default
	Timeout time.Duration
}

// LoadResult is a load round's result. The latencies are measured from sending a request to having
// read its body, failed requests included
type LoadResult struct {
	// Requests is how many were sent, and Errors how many of them failed in the transport (no
	// response arrived)
	Requests int
	Errors   int
	// Status buckets the requests that got a response by status code
	Status  map[int]int
	Elapsed time.Duration
	QPS     float64

	P50, P90, P99, Max time.Duration

	// ErrorSamples are the first few distinct transport errors, to look into
	ErrorSamples []string
}

// OK is how many requests came back with a status code in [200, 300)
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

// Load runs one round after spec. The connection pool is sized to the concurrency (one long-lived
// connection per worker) and closed once the round is over
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
	// The connection only goes back to the pool once the body has been read; without it every request opens a new one
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return 0, err
	}
	return resp.StatusCode, nil
}

// percentile takes the nearest rank: the ceil(p*n)-th of the sorted samples
func percentile(sorted []time.Duration, p float64) time.Duration {
	k := int(math.Ceil(float64(len(sorted))*p)) - 1
	return sorted[min(max(k, 0), len(sorted)-1)]
}
