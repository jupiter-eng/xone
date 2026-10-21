# xhttp

The outbound HTTP client: what you get is a native `*resty.Client` (resty v2), and a usable instance exists at any time, configured or not.

- Tracing is already set up: `traceparent` travels downstream automatically and the Span name is the method alone (on the instance built by `Run`; the fallback instance before and after it has neither)
- The outbound duration metric `http_client_request_duration_seconds`, recorded once per logical request
- Only transport-layer errors are retried, and by default only for idempotent methods
- An optional request log (`Log: true`): one line per logical request with the method, host, path, status code, duration and attempt count, and no query string
- URLs in the logs and on the Spans have their query string removed; there is no cookie jar
- For a second set of parameters (calling the public internet, metrics off) build another one with `xhttp.New`

## Quick start

```yaml
# conf/application.yml (optional; without it everything uses the defaults)
XHttp:
  Timeout: 1s              # the timeout of one attempt
  RetryCount: 2            # only transport-layer errors, and only idempotent methods
```

```go
// Now is the struct the downstream's JSON maps to
func Current(ctx context.Context, city string) (*Now, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second) // caps the whole request: retries and backoff are inside it
	defer cancel()

	var out Now
	resp, err := xhttp.R(ctx).
		SetQueryParam("city", city).
		SetResult(&out). // on a 2xx the JSON is decoded into out
		Get("https://weather.internal/api/v1/now")
	if err != nil {
		return nil, err
	}
	if resp.IsError() { // a 4xx / 5xx is not an err and has to be checked
		return nil, fmt.Errorf("weather api returned %s", resp.Status())
	}
	return &out, nil
}
```

## Configuration

It works unconfigured: `xhttp.R(ctx)` gives a usable client at any time. Before and after `Run` what comes back is the
fallback instance: a 30s timeout and nothing else below, with no retries, no tracing and no metrics. There is only one
instance.

```yaml
XHttp:
  Timeout: 60s               # the timeout of one attempt, not of the whole logical request; 0 = never time out
  DialTimeout: 30s
  DialKeepAlive: 30s         # a negative value = send no keep-alive probes
  MaxIdleConns: 100
  MaxIdleConnsPerHost: 10    # the standard library's default is only 2
  MaxConnsPerHost: 0         # the per-host connection limit, 0 = unlimited
  IdleConnTimeout: 90s       # has to be shorter than the downstream's keep-alive timeout
  RetryCount: 0              # no retry by default
  RetryWaitTime: 100ms       # the initial retry wait
  RetryMaxWaitTime: 2s       # the upper bound on the retry wait
  RetryOnlyIdempotent: true  # retry idempotent methods only
  Trace: true                # outbound Spans; with it off, traceparent, baggage and the forwarded headers still travel downstream
  Metric: true               # the outbound duration metric
  Log: false                 # one log line per logical request (once, after the retries); no query string, headers or body
  SlowThreshold: 1s          # a WARN when the whole request takes longer than this; requires Log, 0 = not recorded
  TLS:                       # the rules are in xtls; it governs every https request this client sends, and http:// is unaffected
    Enable: false
    CAFile: ""               # once set, only this CA is accepted: an https downstream on the public internet no longer verifies
    CertFile: ""
    KeyFile: ""
    ServerName: ""           # once set, every downstream's certificate is verified against it
```

The TLS block suits a client that calls one kind of internal downstream; to call the public internet as well, build
another one with `xhttp.New`.

## API

| Function | Description |
|---|---|
| `R(ctx) *resty.Request` | Opens a request bound to a ctx, which is what carries the trace and the timeout downstream. This is the usual one |
| `C() *resty.Client` | Fetches the resty client. It never panics: before initialization and after shutdown it gives the fallback instance with its 30s timeout |
| `RawClient() *http.Client` | The underlying native `*http.Client`, for a streaming request such as SSE that reads the response body itself |
| `New(cfg) (*resty.Client, io.Closer, error)` | The pure constructor: it does not touch this package's global instance; with `Metric` on, the histogram is registered on xmetric's global Registry |

## Notes

- **`Timeout` governs one attempt, not the whole request**: `Timeout: 300ms` with `RetryCount: 3` was measured at a full
  1.24s. To cap it, use the ctx as shown above. See ["Behavior (measured)"](#behavior-measured).
- **Only transport-layer errors are retried** (a failed connection, a timeout, a reset connection); with a response in
  hand there is no retry, a 5xx included. `RetryOnlyIdempotent` is on by default, so a POST is not resent and a
  condition you attach with `AddRetryCondition` cannot put it back -- except a condition retrying on status codes, see
  ["Behavior (measured)"](#behavior-measured).
- **A request whose body is an `io.Reader` is not retried**: the first attempt read it to the end and a resend would
  have an empty body. To be retried, pass a `[]byte` or a `string`.
- **With `HTTP_PROXY` / `HTTPS_PROXY` set, every outbound request goes through the proxy**; across a host redirect a
  custom credential header (such as `X-Api-Key`) is still carried to the new host.
  See the table in ["Behavior (measured)"](#behavior-measured).
- **The TLS block governs every https request this client sends**: with `CAFile` set, an https downstream on the public
  internet no longer verifies.
- **The metric's `host` label is the URL's `host[:port]` as it is**: with targets coming from user input, or with a
  batch of IPs called directly, the cardinality runs away; build a separate client with `Metric: false` for those calls.
  See ["Metrics"](#metrics).
- **`SetDebug(true)` writes the whole request into the log** (the query string and `Authorization` included); use it
  only when investigating locally. See ["Logs"](#logs).

## Observability

### Logs

| Message | Level | Fields |
|---|---|---|
| `xhttp ready` | INFO | `timeout`, `max_idle_conns_per_host`, `retries` |
| `http request` / `slow http request` / `http request failed` | INFO / WARN / WARN | the table below (requires `XHttp.Log: true`) |
| `xhttp resty log` | resty's own level | `detail`: resty's own log text, with the URL's query string, fragment and userinfo removed (a request dump from `SetDebug(true)` is the exception, below) |
| `xhttp skipped retrying a non-idempotent method` | DEBUG | `method`, `to_allow_set` |
| `xhttp cannot tune the connection pool` | WARN | `default_transport_type`: `http.DefaultTransport` was replaced by another type and the pool parameters had no effect |

The request log's fields:

| Field | Description |
|---|---|
| `method` | The request method |
| `host` | `host[:port]` |
| `path` | In its escaped form (such as `/a%2Fb`), without the query string |
| `status` | The status code, or `0` when no response arrived |
| `elapsed_ms` | The whole logical request, retries and backoff included; `0` when nothing went out |
| `attempts` | resty's attempt count |
| `threshold_ms` | On a slow request only |
| `error` | When there is one; the URL has its query string, fragment and userinfo removed |

- **A failure** is an error returned by resty, or a 5xx from the downstream; a 4xx is the downstream's business answer
  and is recorded at INFO. An error is not only "no response arrived" (a transport error, an expired ctx): a 200 whose
  `SetResult` cannot be parsed (`status: 200`, `error: invalid character …`), a 3xx under `NoRedirectPolicy` and a
  `FlexibleRedirectPolicy` used up (`status: 302`, `error: Get "/b": auto redirect is disabled`) are recorded as
  `http request failed` too. One that is both slow and failed is recorded as a failure, not additionally as slow.
- **One line per logical request**, recorded only after every retry has finished. With `Log: true`, resty's own
  `xhttp resty log` lines on the retry path (one WARN per failed attempt, one ERROR once used up) are no longer printed
  and their content is exactly this line's `error`; resty's other warnings (Basic Auth over plaintext HTTP, say) and the
  ERROR for an ignored configuration call are unchanged. With your own `client.SetLogger(…)` or `R().SetLogger(…)`,
  resty's logs follow resty's rules and all go to your logger (the two retry-path kinds included).
- **What has no log line**: a request resty rejects before sending it (a GET with multipart, reported as
  `multipart content is not allowed in HTTP verb [GET]`), for which resty calls only `OnInvalid` and neither
  `OnSuccess` nor `OnError`.
- **With `SetDoNotParseResponse(true)`, `elapsed_ms` is the time to the response headers**: the body is yours to read
  and resty is done once it has the headers. Measured against a downstream answering the headers first and finishing
  the body 150ms later: recorded as `0.25` this way, and as `151` when the body is read as usual.
- The query string, the fragment, the userinfo, the headers and the body are never recorded. The log uses the caller's
  ctx, so `trace_id` / `span_id` are the caller's (the outbound Span has ended by then).
- **Warning: `SetDebug(true)` writes the whole request into the log.** resty assembles the request line (with its query
  string), every header (`Authorization` included), the body and the response into one piece of text and writes it
  through `xhttp resty log` (DEBUG); the path in the request line carries no `http://` and is not quoted, so xhttp does
  not recognize it, and measured, `GET  /ok?token=…` and `Authorization: Bearer …` appeared as they are. Use it only
  when investigating locally, never in production.

The global logging conventions (`trace_id` injection, `xlog.AddKV`, the framework's start and stop logs) are in
[`docs/observability.md`](../docs/observability.md#logs).

### Metrics

| Metric | Type | Labels | Source |
|---|---|---|---|
| `http_client_request_duration_seconds` | histogram | `method`, `host`, `status` | xhttp, `XHttp.Metric`; recorded once per logical request (retries and backoff included) |

- **`method`** is collapsed to RFC 9110's methods plus `PATCH`, with everything else recorded as `OTHER`.
  **`status`** is `0` when no response arrived.
- **`host`** is the outbound request URL's `host[:port]`, copied as it is, so the cardinality equals the number of
  targets called. Each new value multiplies by `method` x `status`, and each combination is 15 time series (12 default
  buckets plus `+Inf`, `_sum` and `_count`). For calls whose target comes from user input, or that go to a batch of IPs
  directly, build a separate client with `Metric: false`.

The metric name prefix, the constant labels and a few general rules are in
[`docs/observability.md` "Metrics"](../docs/observability.md#metrics).

### Tracing

| Source | Span name | Key attributes |
|---|---|---|
| xhttp (outbound) | The method alone, such as `GET` | otelhttp's standard attributes; `url.full` **has its query string, fragment and userinfo removed** |

The whole tracing picture, propagation and the trust boundary are in
[`docs/observability.md` "Tracing"](../docs/observability.md#tracing).

## Behavior (measured)

The measurement environment and the cross-module summary table are in [`docs/behavior.md`](../docs/behavior.md).

resty v2.17.2, otelhttp v0.71.0, Go 1.25.

**`Timeout` governs one attempt, not one logical request.** With `RetryCount` on, the worst case is
`(RetryCount+1) x Timeout` plus the backoff waits: `Timeout: 300ms` with `RetryCount: 3` was measured at 1.24s. To cap
the whole logical request, use the caller's ctx (`xhttp.R(ctx)`), which every attempt and every backoff in between
honors.

**What is retried**: the same as resty's own default, only transport-layer errors (a failed connection, a timeout, a
reset connection, an incomplete response body); with a response in hand there is no retry -- a 5xx included.
Attaching a retry condition voids resty's own judgment entirely, which is why xhttp guards this itself: without that,
a 200 with broken JSON and `RetryCount: 3` was measured sending the same GET 4 times. `RetryOnlyIdempotent` is on by
default: a transport-layer timeout cannot tell "the request never reached the server" from "it was processed and the
response was lost", so resending a POST may mean a duplicate order.

**A retry condition you attach yourself.** resty v2.17.2's conditions are an "or": request-level ones come first and
client-level ones in the order they were attached, and one returning true means the rest are not consulted
(`retry.go` `Backoff`), so xhttp's "do not retry a POST" is just one of the falses. Hence a second gate on
`RetryAfter` (called after the decision to retry and before the wait, where returning an error stops the retry): a POST
or PATCH with no response is not retried, and the caller still gets the first transport error (resty only hands over
`RetryAfter`'s error when that attempt had none). Measured with `RetryCount: 2`, a condition retrying on any
`err != nil` and a cut connection: without this gate the POST went out twice, with it once.

- **What it cannot block**: a condition retrying on a status code after a response arrived (`StatusCode() >= 500`, say).
  There is no original error to hand over then, and vetoing would mean replacing that 503 with an error we invented, so
  it does not: the POST is retried by your condition (measured at 3 attempts, ending with a 503 and a nil error) and
  the method is yours to check in the condition.
- Your own `SetRetryAfter` replaces this gate. The `OnRetry` hook is called before it, including for the attempt that is
  then vetoed.

**A request whose body is an `io.Reader` is not retried.** resty v2.17.2 rebuilds the request from `Request.Body` on
every attempt (`middleware.go` `createHTTPRequest`); a `[]byte`, a `string` and a struct are re-serialized each time,
while an `io.Reader` was read to the end on the first attempt and is not rewound for you (`RetryResetReaders` only
covers multipart). Measured with a `PUT` of a `strings.NewReader(…)` whose first connection was cut: the second attempt
sent 0 bytes, the server answered 200 and the caller saw success. So such a request is not retried (with
`RetryOnlyIdempotent` off alike) and the caller gets the first error; when a status-code condition wants to resend it,
the caller gets `not retrying: the request body is an io.Reader that the first attempt already consumed` together with
that response. The exception is `SetContentLength(true)`, where resty reads the `io.Reader` into a buffer and sends that
buffer every time (measured complete both times); a PUT / DELETE with no body is retried as usual.

**Whatever the connection pool does not configure is the standard library's default** (cloned from
`http.DefaultTransport`):

| Item | Default | Measured behavior |
|---|---|---|
| Proxy | `ProxyFromEnvironment` | With `HTTP_PROXY` / `HTTPS_PROXY` set, every outbound request goes through the proxy, and an `http://` request is handed to it with its query string as it is; loopback does not, and `NO_PROXY` can exclude more. The environment variables are read once on first use and cached, so changing them later has no effect |
| TLS handshake timeout | 10s | With the peer accepting the TCP connection and never handshaking, `TLS handshake timeout` after 10.0s |
| Waiting for response headers | unlimited | Bounded by `Timeout` over the whole attempt; with `Timeout` at 0 as well it hangs forever |
| `MaxConnsPerHost` | 0, unlimited | Against a downstream answering after 300ms with 200 concurrent requests it received 200 new connections; another 200 right after reused only the 10 of `MaxIdleConnsPerHost` |
| Redirects | the standard library: at most 10 | The 11th reports `stopped after 10 redirects`. Across hosts only `Authorization` and `Cookie` are dropped, and **a custom credential header (such as `X-Api-Key`) is still carried to the new host**; a 302 turns a POST into a GET with no body, and a 307 keeps the method and the body |
| HTTP/2 | `ForceAttemptHTTP2: true` | `HTTP/2.0` is still negotiated after replacing `TLSClientConfig` (the TLS block) |

To stop following redirects: `xhttp.C().SetRedirectPolicy(resty.NoRedirectPolicy())`.

**`IdleConnTimeout` has to be shorter than the downstream's keep-alive timeout**: when the peer closes an idle
connection first, a request reusing it at exactly that moment fails. Measured with a 200ms server-side idle timeout and
request intervals around 200ms: 27 of 300 POSTs failed (`connection reset by peer`), while GETs are resent
automatically by the standard library on a new connection and none of 200 failed.

**The request log (`Log: true`)**. Measured on resty v2.17.2: `OnSuccess` / `OnError` are called once after every retry
has finished, and `Request.Attempt` is the number of attempts used; with a response in hand it goes to `OnSuccess`,
**a 5xx included** (resty only calls `OnError` when there is an error), so whether a 5xx counts as a failure is decided
here. Measured with `RetryCount: 2` and `RetryWaitTime` / `RetryMaxWaitTime: 5ms`:

| Downstream | What it received | Callback | Log |
|---|---|---|---|
| 200 | once | `OnSuccess` once | `http request`, `status: 200`, `attempts: 1` |
| 500 | once (no retry with a response in hand) | `OnSuccess` once | `http request failed`, `status: 500`, `attempts: 1` |
| cutting the connection every time | 3 times; 4 with an idle connection to that downstream in the pool | `OnError` once, `Attempt` 3 | `http request failed`, `status: 0`, `attempts: 3`, `elapsed_ms` about 12 (the two backoffs included), `error: Get "http://127.0.0.1:…/cut": EOF` |
| nothing listening on the port | — | `OnError` once, `Attempt` 3 | `http request failed`, `attempts: 3`, `error: Get "http://127.0.0.1:1/x/y": dial tcp …: connection refused` |

The 4-times case on the cut-connection row: the first attempt used an idle connection from the pool, and the standard
library resent that GET once on a new connection when the peer closed it (the same as the `IdleConnTimeout` row above);
resty does not see that one, so `attempts` is still 3.

**URLs inside errors.** The error text is a `*url.Error`: `Get "http://someone:***@host/x?token=…": …` -- the whole URL
including the query string, with the standard library replacing only the password with `***` and keeping the user name.
That URL is not necessarily absolute either: when a redirect policy refuses, what goes in is the `Location` as it is,
and measured with `Location: /b?token=…` the text was `Get "/b?token=…": auto redirect is disabled`; a query string may
also hold unescaped spaces (`?q=hello world&token=…`). So the `error` in the log is handled by structure:

- Every `*url.Error` on the error chain (wrapped with `%w` or joined with `errors.Join` alike) has its URL parsed and
  its query string, fragment and userinfo removed before rendering; one that cannot be parsed is cut at the first `?`
  or `#`.
- The free text outside the chain gets a text pass as a fallback (a quoted one up to the closing quote). An invalid
  escape in `Location` puts it in the error text: `failed to parse Location header "/b%zz": …`.

resty's own logs and the outbound Span's `url.full` use the same treatment. The error returned to the caller is
unchanged.

**Where this differs from resty's / otelhttp's defaults:**

| | The library's own default | Here |
|---|---|---|
| resty's logs | Written to `os.Stderr`; with retries on, one `WARN RESTY Get "http://…?token=…": …, Attempt 1` per failure and one ERROR once used up | Routed into slog (`xhttp resty log`, with the content in `detail`), keeping the level and removing the query string from the URL; with `Log: true` the retry-path lines are not printed and one `http request failed` line takes their place |
| cookie jar | `resty.New()` brings one, and every request of that client shares the session cookie | None (`NewWithClient` is used). The fallback instance before initialization and after shutdown has none either |
| outbound Span name | The common form is `GET /users/42`, whose cardinality grows with the id | The method alone, `GET` (OTel's semantic conventions without a route template) |
| `url.full` | otelhttp removes only `user:password` and writes the query string in as it is | The query string, the fragment and the userinfo are all removed |
| `CloseIdleConnections` | otelhttp's Transport does not implement it, so `http.Client.CloseIdleConnections()` stops at that layer and the whole call becomes a no-op | On close, xhttp closes the connection pool it holds directly |
