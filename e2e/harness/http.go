package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
)

// Response 读完了 body 的响应
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// JSON 把 body 按 JSON 解进 v，失败时 t.Fatal
func (r Response) JSON(t testing.TB, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("decode response body as JSON: %v\nstatus=%d body=%s", err, r.Status, r.Body)
	}
}

// Map 把 body 解成 map，失败时 t.Fatal
func (r Response) Map(t testing.TB) map[string]any {
	t.Helper()
	var m map[string]any
	r.JSON(t, &m)
	return m
}

func (r Response) String() string { return fmt.Sprintf("status=%d body=%s", r.Status, r.Body) }

func do(ctx context.Context, c *http.Client, method, url string, body any, header ...string) (Response, error) {
	if len(header)%2 != 0 {
		return Response{}, fmt.Errorf("header must be key, value pairs, got %d strings", len(header))
	}
	var rd io.Reader
	isJSON := false
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = bytes.NewReader([]byte(b))
	default:
		data, err := json.Marshal(b)
		if err != nil {
			return Response{}, fmt.Errorf("encode request body: %w", err)
		}
		rd, isJSON = bytes.NewReader(data), true
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return Response{}, err
	}
	if isJSON {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{Status: resp.StatusCode, Header: resp.Header}, fmt.Errorf("read response body: %w", err)
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: data}, nil
}
