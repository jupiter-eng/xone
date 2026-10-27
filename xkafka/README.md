# xkafka

Kafka: producing gives a native `*kgo.Client` ([franz-go](https://github.com/twmb/franz-go)), consuming registers a handler with `xkafka.Consume`, and the framework starts and stops it.

- Producing: `xkafka.C().ProduceSync(ctx, r)`, with the trace context written into the message headers automatically; on exit it `Flush`es before closing, so buffered messages are not lost
- Consuming: one goroutine per partition, in order within a partition and in parallel across them; **committed only once processed** (at least once)
- Failures are retried (exponential backoff with jitter) and go to the dead-letter topic once those are used up (`<topic>.dlq` by default); `xutil.Permanent` is not retried; a panic is caught
- A rebalance and a shutdown both wait for the message in flight to be processed and committed before handing the partition over; the handler's ctx is not cancelled by the stop signal
- A new consumer group starts at **latest** by default (franz-go defaults to earliest)
- One `<topic> process` Span per message, continuing the producer's trace; the logs carry `topic` / `partition` / `offset` / `group`; the value is never recorded and the Span carries no key
- One connectivity probe at startup: if it cannot connect, startup fails

## Quick start

```yaml
# conf/application.yml
XKafka:
  Brokers: ["kafka-1:9092", "kafka-2:9092"]
```

**Producing**:

```go
import (
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jupiter-eng/xone/xkafka"
)

err := xkafka.C().ProduceSync(ctx, &kgo.Record{Topic: "orders", Key: []byte(orderID), Value: b}).FirstErr()

// Asynchronously: do not wait for the result, take the error in the callback
xkafka.C().Produce(ctx, &kgo.Record{Topic: "events", Value: b}, func(r *kgo.Record, err error) { … })
```

franz-go's methods take a ctx anyway, which is why there is no `CWithCtx`.

**Consuming inside a web service** -- the consumer runs in the background and the service is unaffected:

```go
func main() {
	err := xkafka.Consume("orders", "billing", func(ctx context.Context, r *kgo.Record) error {
		var o Order
		if err := json.Unmarshal(r.Value, &o); err != nil {
			return xutil.Permanent(err) // malformed, so retrying will not help: straight to the dead letter
		}
		return charge(ctx, o) // returning an error retries, and once those are used up it goes to orders.dlq
	}, xkafka.WithTimeout(10*time.Second))
	if err != nil {
		log.Fatal(err)
	}
	xone.MustRun(xgin.New().WithRoutes(routes))
}
```

**A process that only consumes**:

```go
import _ "github.com/jupiter-eng/xone/xlog" // xkafka does not bring xlog: for JSON logs and the topic / offset fields, blank-import it

func main() {
	xkafka.Consume("orders", "billing", handle)
	xone.MustRun(xone.UntilSignal())
}
```

The consumers come up on StageServer -- the clients (xgorm, xredis, ...) and the caller's business hooks are all ready,
so the handler can call `xgorm.C()` directly; on shutdown they stop first, and the clients are only closed once the
messages in flight are processed and committed. There is no need to write a `Start`, a `WaitGroup` or a
`WithoutCancel`: it already does all three rules from
[docs/guide.md "Non-web services"](../docs/guide.md#non-web-services-consumer--job).

**`C()` produces and `Consume` consumes.** Both read the same `XKafka` configuration: `C()` is this cluster's shared
client with no consumer group; a consumer group can only be set when the client is built, so every `Consume` builds its
own from the same configuration (`WithClient` picks the cluster, and without it the cluster is `default`).
There is no `C().Consume`: what `C()` hands out is a native `*kgo.Client` with no room for a method of ours, and it is
only built on StageClient while `Consume` may be registered at any time.

| What a consumer uses from the configuration | Effect |
|---|---|
| `Brokers` / `SASL` / `TLS` / `DialTimeout` / `ClientID` | How it connects, exactly as for producing |
| `Consumer.ResetOffset` | Whether a new consumer group starts at latest or earliest |
| `Consumer.SessionTimeout` | How long the broker waits for a heartbeat before it hands this consumer's partitions to another member |
| `Trace` / `Metric` / `Log` | The per-message Span, the processing duration metric, the per-message logs |
| `Producer.*` | Used when writing a dead letter (which this consumer's own client writes) |

The topic, the group, the retries and the dead letter are written in code: they decide whether messages can be lost and
belong in code review. For a topic or group that varies by environment, read it from a config block of your own and pass
it in, with `${VAR}` and the misspelled-field error working as usual:

```go
xhook.BeforeStart(func(ctx context.Context) error {
	c := struct{ Topic, Group string }{}
	if err := xconfig.Unmarshal("Orders", &c); err != nil {
		return err
	}
	return xkafka.Consume(c.Topic, c.Group, handle)
})
```

## Configuration

```yaml
XKafka:
  Brokers: ["127.0.0.1:9092"]   # required, the seed brokers
  ClientID: ""                  # empty = XApp.Name; with neither, franz-go's kgo and one WARN at startup
  DialTimeout: 1s               # TCP + the TLS handshake; 0 = franz-go's default of 10s
  SASL:
    Mechanism: ""               # PLAIN / SCRAM-SHA-256 / SCRAM-SHA-512; empty = no authentication
    Username: ""
    Password: "${KAFKA_PASSWORD}"
  TLS:                          # the rules are in xtls; an empty ServerName = the host part of each broker address
    Enable: false
    CAFile: ""
    CertFile: ""
    KeyFile: ""
    ServerName: ""
  Producer:
    Acks: all                   # all / leader / none; with all, idempotent writes are on, and the other two turn them off for you
    Linger: 10ms                # how long a batch waits at most; 0 = no waiting
    Compression: snappy         # none / gzip / snappy / lz4 / zstd
  Consumer:
    ResetOffset: latest         # where a new consumer group starts: latest / earliest
    SessionTimeout: 0s          # how long a killed consumer's partitions wait before they move; 0 keeps franz-go's 45s, 3s at the least
  Trace: true                   # one Span each for producing and consuming; with it off the trace context is still written into and read from the message headers
  Metric: true                  # kafka_consume_duration_seconds
  Log: true                     # per-message logs: a failed produce and each failed processing attempt (WARN), a successful one (DEBUG)
```

**Several clusters** go under `Clients`, each with the same fields as above; anything left out uses the defaults above:

```yaml
XKafka:
  Clients:
    default: {Brokers: ["kafka:9092"]}
    audit:   {Brokers: ["kafka-audit:9092"], Consumer: {ResetOffset: earliest}}
```

`xkafka.C()` fetches `default` and `xkafka.C("audit")` the other one; to consume from another cluster write
`xkafka.WithClient("audit")`. The two forms cannot be mixed.

Any franz-go setting not listed keeps its default; the numbers are in ["Behavior (measured)"](#behavior-measured). For
finer control (transactions, a custom partitioner), call `kgo.NewClient` yourself; the framework does not stand in the
way.

## API

| Function | Description |
|---|---|
| `C(name ...string) *kgo.Client` | Fetches the producing client; without an argument, `default`. It has no consumer group. If there is none it panics, and the message says plainly whether it was called too early, never configured, or the name is misspelled |
| `Has(name ...string) bool` | Whether a cluster is configured, for checking an optional dependency first |
| `Names() []string` | Which clusters are configured |
| `New(ctx, cfg) (*kgo.Client, io.Closer, error)` | The pure constructor: touches no globals and reads no config file. It probes connectivity once; the Closer it returns only does `Close`, so `Flush(ctx)` first |
| `Consume(topic, group, fn, opts...) error` | Registers a consumer, below |

`Consume` validates on the spot, and the error is an `*xerror.Error` (module `xkafka`): an empty `topic` or `group`, a
nil `fn`, an Option that does not hold, and the same `topic` + `group` pair registered twice on the same cluster are
`config`; a `Consume` after the shutdown sequence has begun is `register`. `group` is required and has no default:
several processes in the same group share the partitions while different groups each receive every message -- something
to think through before writing it.

It may be called at any time: one registered before `xone.Run` starts together with StageServer, and one registered
later (inside a hook, inside `Start`) starts on the spot, where a wrong cluster name also returns an error on the spot.

| Option | Default | Description |
|---|---|---|
| `WithClient(name)` | `default` | Which cluster to consume from. Every `Consume` builds its own client from that cluster's configuration (carrying the consumer group) and does not share `C()`'s |
| `WithTimeout(d)` | 30s | The timeout of one call to the handler, cancelling its ctx when reached; it must be greater than 0. Each retry is counted separately |
| `WithRetry(n)` | 3 | How many **more** times to try after a failure (an error or a panic), not counting the first: at most 4 calls by default. 0 means no retry |
| `WithDeadLetter(topic)` | `<topic>.dlq` | Which topic to write to once the retries are used up; `""` turns the dead letter off and records an ERROR instead. It must not be the topic being consumed |

The extra headers a dead-letter message carries are exported constants: `HeaderDLQTopic`, `HeaderDLQPartition`,
`HeaderDLQOffset`, `HeaderDLQGroup` and `HeaderDLQError`.

## Semantics

### Committing: at least once

A message is marked only once it is **done** (it succeeded, reached the dead letter, or was skipped with the dead letter
off), franz-go commits what is marked every 5s, and a rebalance or a shutdown commits on the spot. On a crash or a
`kill -9`, uncommitted messages are redelivered later -- **the handler has to tolerate duplicates** (an idempotent
write, deduplication by business key). Messages processed within the 5s before the kill may arrive again as well
(measured in e2e: one processed before the kill arrived again after the restart).

franz-go's default automatic commit does not work that way: it commits "whatever the last poll returned", processed or
not. With one goroutine per partition, polling and processing are not together, so a fetched message may still be queued
when it is committed -- and a crash loses it. Hence `AutoCommitMarks` here.

### Concurrency and ordering

One goroutine per assigned partition: within a partition the next message starts only once the previous one is done
(messages with the same key are in the same partition, so their order is kept); different partitions run in parallel.
The parallelism is exactly the number of partitions this process is assigned -- for more, add partitions or add
processes.

When one partition is slow and the batches queued in front of it are full (at most 4 batches per partition, at most 500
messages each), dispatching stops and waits for it, and the dispatching of the other partitions waits too: the slow
partition does not keep accumulating in memory. This is what franz-go's example does
(`goroutine_per_partition_consuming/autocommit_marks`).

### Retries and dead letters

The handler returns an error or panics → it waits and retries, starting at 1s and doubling with jitter (the actual wait
is a random value in `[0, the upper bound]`, as in `xutil.Retry`), with upper bounds of 1s, 2s, 4s, capped at 30s.
A `xutil.Permanent(err)` is not retried.

The retries are used up → the original message (key, value, every header) is written into the dead-letter topic with a
few extra headers:

| Header | Value |
|---|---|
| `xkafka-dlq-topic` / `xkafka-dlq-partition` / `xkafka-dlq-offset` | The original message's position, in decimal |
| `xkafka-dlq-group` | The consumer group that failed to process it |
| `xkafka-dlq-error` | The last error text, cut to 256 bytes |

`traceparent` is replaced by this processing attempt's Span, so the dead letter continues that trace. **The error text
lands in the dead-letter topic with the message**: cutting it short only keeps it from growing the headers and keeps
nothing out. The dead-letter topic holds the original message's value anyway, so its read permissions have to be as
tight as the original topic's; do not put credentials into an error.

**A failed dead-letter write** (the topic does not exist, no write permission, the cluster is down) → a WARN is recorded
and it backs off and retries (as above, capped at 30s per attempt) until it gets through: nothing is marked and nothing
after it is processed -- dropping it would be losing a message silently, and skipping it to process the next one would
commit an offset past it. That partition stalls meanwhile while the others carry on. On shutdown or revocation it gives
up, the message is not committed and is redelivered later (which may leave a duplicate in the dead letter).
**The dead-letter topic has to exist beforehand**: franz-go does not create it for you (see
["Behavior (measured)"](#behavior-measured)).

`WithDeadLetter("")` → once the retries are used up, one ERROR `kafka message skipped, …` is recorded and it commits and
moves on.

### Rebalancing

When partitions are revoked (another member joins or leaves), it first waits for the message in flight on those
partitions to be processed, then commits what was marked, and only then lets the rebalance continue: the new member
starts from the next message, does not reprocess what was already processed, and never processes the same message at the
same time. Whatever is queued and not started is left alone and the new member processes it from the beginning.
What is waited for is **one** handler call: a backoff wait, a retry not yet started and a dead-letter retry are all
abandoned on the spot when the partition is revoked (and that message is not committed).
So do not configure `WithTimeout` longer than franz-go's rebalance timeout (60s), or this member is kicked out of the
group.

When partitions are "lost" (a session timeout and the like) it likewise waits for the message in flight but commits
nothing: they are no longer its.

The assignment strategy is franz-go's default cooperative-sticky: a rebalance only moves the partitions that have to
move and leaves the others running.

### Shutdown

After a SIGTERM / SIGINT, the consumers' stop hook (StageServer, which runs first):

1. fetches no new message;
2. leaves the consumer group, with every partition revoked by the "Rebalancing" rules above: waiting for the message in
   flight to be processed, then committing;
3. closes this consumer's client.

**The handler's ctx is not cancelled by the stop signal** -- inheriting an already cancelled ctx would have every
database write and downstream call for that message rejected the moment it starts. It is bounded only by
`WithTimeout`. Whatever has not finished when the stop budget runs out has its ctx cancelled, and the stop hook returns
a `stop` error naming the partitions and the offset each was at, plus a WARN; those messages are not committed and are
redelivered later.

Only then come the StageClient producing clients: `Flush` first (sending out what `Produce` left in the buffer), then
close.

**One message's processing time has to be shorter than the stop budget** (`xone.WithStopTimeout`, 15s by default; in a
web service the server's share goes first and the stop hooks share what is left), or it is cancelled on exit and
redelivered.

### Where a new consumer group starts

`Consumer.ResetOffset` defaults to `latest`: the first time a consumer group consumes a partition, it only processes
what arrives afterwards. franz-go's default is earliest -- a newly deployed group would process every historical message
still retained in the topic, often several days' worth. The cost: a new group, or a group starting just after the topic
was created, never processes what was written before; configure `earliest` when those are wanted. A group that has
already committed an offset is unaffected and continues from the committed position.

### When the topic does not exist

A missing topic at startup records one WARN
`kafka topic does not exist yet, the consumer waits for it to be created` and **does not fail startup** -- a topic is
often created a step later by the producer or by operations, and the consumer going live first is a normal deployment
order. It picks up within seconds of the topic being created (with `latest`, what was written before that moment is not
processed).

Producing to a topic that does not exist: `ProduceSync` returns `UNKNOWN_TOPIC_OR_PARTITION` after about 1s and does not
create it for you.

## Several instances

One producing client per cluster, fetched with `xkafka.C("name")`; one client of its own per `Consume`. At startup they
are built one by one in name order, and if one cannot connect every client already built is closed, with the error
naming which one. The rules are the same as in
[docs/guide.md "Several instances"](../docs/guide.md#several-instances).

## Observability

### Logs

| Message | Level | Fields |
|---|---|---|
| `xkafka connected` | INFO | `name`, `brokers`, `tls`, `sasl` (the mechanism name) |
| `xkafka ready` | INFO | `instances` |
| `xkafka consumers started` | INFO | `consumers` (`topic/group`) |
| `kafka produce failed` | WARN | `name`, `topic`, `partition`, `key`, `error` (requires `Log: true`). For one that failed before being assigned a partition (the topic does not exist) `partition` is 0, so do not read anything into it |
| `kafka message processed` | DEBUG | The message fields, `elapsed_ms` (requires `Log: true`) |
| `kafka message failed` | WARN | The message fields, `attempt` (which attempt), `elapsed_ms`, `error` (requires `Log: true`) |
| `kafka message dead-lettered` | WARN | The message fields, `dlq_topic`, `attempts`, `error` |
| `kafka message skipped, retries exhausted and dead lettering is off` | ERROR | The message fields, `attempts`, `error` |
| `kafka handler panicked` | ERROR | The message fields, `error`, `stack` |
| `kafka partition worker panicked, this partition stops until the next rebalance` | ERROR | `topic`, `partition`, `group`, `error`, `stack`. This module's own code blew up rather than the handler's (that one is the line above and counts as one failed attempt). The partition is given up instead of taking the process down; nothing was marked, so its records come back after the next rebalance. It means a bug in the framework — report it |
| `kafka records dropped, this partition has no goroutine of its own until the next rebalance` | WARN | `topic`, `partition`, `group`, `records`, `from_offset`. What follows the line above, once per partition per minute for as long as it lasts: the records of that partition are still fetched and thrown away. Nothing is marked, so they come back after the next rebalance, and until then that partition makes no progress |
| `kafka dead letter produce failed, retrying` | WARN | The message fields, `dlq_topic`, `error` |
| `kafka fetch failed` | WARN | `topic`, `partition`, `group`, `error` |
| `kafka commit on partition revoke failed, processed messages may be redelivered` | WARN | `topic`, `group`, `error` |
| `kafka topic does not exist yet, the consumer waits for it to be created` | WARN | `topic`, `group` |
| `xkafka consumers still stopping when the stop budget ran out` | WARN | `busy`: the `topic/partition@offset (group …)` still processing, or the `topic/group (committing offsets / leaving the group)` stuck committing or leaving |
| `xkafka franz-go log` | WARN / ERROR | `name`, `detail` (franz-go's own words), `fields` (its key-values, as one object) |

The message fields are `topic`, `partition`, `offset` and `group`, plus `key`:

- **key** is recorded whole when it is printable UTF-8 (truncated beyond 256 bytes, with `key_truncated: true`), the same
  rule as in xredis; a binary key (Avro, protobuf, an integer) records only `key_len`; a message with no key records
  nothing. Do not put a token or a phone number into a key.
- **The value is never recorded**, in any log line.
- `error` is the text the handler returned, as in xcron: it often wraps SQL or a downstream response.

**Logs written inside the handler**: the ctx carries a scope with the four fields `topic` / `partition` / `offset` /
`group`, so with xlog installed (`slog.InfoContext(ctx, …)`) every line carries them, as with xcron's `job`. This module
does not import xlog: a program that only produces with xkafka does not get its `slog.Default()` replaced; a process
that only consumes and wants JSON logs with these fields blank-imports
`github.com/jupiter-eng/xone/xlog`. `Log: false` only turns off the lines marked "requires `Log: true`" above: reaching
the dead letter, being skipped, a panic and a failed dead-letter write all mean a message did not travel its normal
path, and they are recorded regardless.

The global logging conventions are in [`docs/observability.md`](../docs/observability.md#logs).

### Metrics

| Metric | Type | Labels | Source |
|---|---|---|---|
| `kafka_consume_duration_seconds` | histogram | `topic`, `group`, `status` | xkafka consuming, `XKafka.Metric`, per cluster |

One message from the start of processing to a result, retries and the dead-letter write included. `status`: `ok`,
`dead_letter`, `skipped`, `aborted` (unfinished when shutting down or when the partition was revoked, redelivered
later). The buckets are prometheus's defaults (5ms to 10s). It carries no partition, so the series count does not grow
with the partitions. Producing has no metric of its own; use franz-go's hooks for that.

### Tracing

| Source | Span name | Kind | Attributes |
|---|---|---|---|
| Producing | `<topic> publish` | Producer | `messaging.system=kafka`, `messaging.operation.name=publish`, `messaging.destination.name`; on success also `messaging.destination.partition.id` and `messaging.kafka.offset` |
| Consuming | `<topic> process` | Consumer | The same first three (`process`), plus `messaging.destination.partition.id`, `messaging.kafka.offset` and `messaging.consumer.group.name` |

- **The trace joins up**: producing writes the trace context (W3C `traceparent`, `baggage`, xtrace's forwarded headers)
  into the message headers through the global Propagator, carrying the publish Span's context; consuming takes it back
  out as the process Span's parent. An HTTP request → producing → consuming is one `trace_id`.
- **No key, no value**: franz-go's own kotel plugin writes the key into `messaging.kafka.message.key`, which is why it
  is not used here.
- **A failure carries the status only**: a failed produce is `kafka produce failed` and a message that did not succeed
  (reached the dead letter, was skipped) is `kafka message failed`, with no error text, as in xcron.
- **Message headers count as a trusted source**: xtrace accepts forwarded headers and baggage only from a trusted peer
  (on the HTTP side, a directly connected address in `TrustedProxies`). Whoever can write into the topic holds the
  cluster's credentials, and what this module injects when producing is only what this process already accepted, so the
  message headers are accepted as trusted. For a topic open to outside writes, where a third party writes into it
  directly, do not rely on forwarded headers or baggage in its consumer. `traceparent` is not governed by this.
- `Trace: false` only stops the Spans from being opened: the trace context is still written into and read from the
  message headers.

The whole tracing picture, propagation and the trust boundary are in
[`docs/observability.md` "Tracing"](../docs/observability.md#tracing).

## Behavior (measured)

The measurement environment and the cross-module summary table are in [`docs/behavior.md`](../docs/behavior.md).

franz-go v1.21.7, kfake (the same commit), Apache Kafka 3.9.1 (a single KRaft node,
`mirror.gcr.io/apache/kafka:3.9.1`) and Go 1.25.
franz-go's defaults are pinned down by the `TestFranzDefault_*` group in `franz_test.go`: whichever goes red after an
upgrade is a number here that has to be measured again.

**franz-go's defaults** (measured with `cl.OptValue`):

| Setting | franz-go's default | Here |
|---|---|---|
| `ClientID` | `kgo` | `XApp.Name`; configured nowhere it stays `kgo` and startup writes one WARN. `New` reads no `XApp` |
| `RequiredAcks` / idempotent writes | all / on | Unchanged (`Producer.Acks` can change it, and anything other than all turns idempotent writes off for you -- without that, `NewClient` reports an error) |
| `ProducerLinger` | 10ms | Unchanged (`Producer.Linger`) |
| `ProducerBatchCompression` | snappy, falling back to none | Unchanged (`Producer.Compression`) |
| `RecordRetries` / `RecordDeliveryTimeout` | unlimited / unlimited | Unchanged: a message is retried until it succeeds or its ctx ends -- **give `ProduceSync` a ctx with a deadline** (measured: with the broker unreachable, a 200ms ctx returned in 200ms) |
| `UnknownTopicRetries` | 4 | Unchanged: writing to a topic that does not exist reports an error after about 1s |
| `AllowAutoTopicCreation` | off | Unchanged: nothing is created automatically even with `auto.create.topics.enable` on at the broker (Kafka's default) |
| `DialTimeout` | 10s | 1s (`DialTimeout`) |
| `RequestTimeoutOverhead` | 10s | Unchanged |
| Automatic commit | every 5s, committing "whatever the last poll returned" | `AutoCommitMarks`: only what is done is committed, still every 5s |
| `ConsumeResetOffset` | earliest (AtStart) | `latest` (`Consumer.ResetOffset`) |
| `SessionTimeout` / `RebalanceTimeout` / `HeartbeatInterval` | 45s / 60s / 3s | 45s unless `Consumer.SessionTimeout` says otherwise; the other two unchanged |
| Assignment strategy | cooperative-sticky | Unchanged |
| Logging | none (nopLogger) | Routed into slog, WARN and above |

**The automatic commit commits unprocessed messages**: 3 messages polled and none processed, then polled again, and the
default `OnPartitionsRevoked` committed 3 on `Close`
(`TestFranzDefault_AutocommitCommitsPolledButUnprocessed`). Closing after a single poll commits nothing -- it commits
"the last poll"'s, which is why a synchronous loop (poll, process, poll again) is safe while separating polling from
processing is not.

**`Close` does not `Flush`**: every buffered message fails with `kgo.ErrClientClosed` (writing 10 messages with `Linger`
at one minute, all 10 failed after `Close`). The stop hook `Flush(ctx)`es before it `Close`s; a direct call to `New`
should `Flush` first.

**`Ping` does not honor the ctx against a broker that never answers**: when the peer accepts the TCP connection and
never answers, `Ping` waits for the read timeout (`RequestTimeoutOverhead`, 10s): a `Ping` given a 200ms deadline
returned after 10.0s, and without a deadline after 20s, with a cancellation in between waiting the same. Hence the
startup probe runs in a goroutine while this side watches the ctx. A refused connection reports an error in under 1ms.

**The startup probe**: once per cluster (asking a broker for metadata once), 3 attempts, each with a budget of
2 x `DialTimeout` (2s by default) and backoff bounds of 1s and 2s, so 9s in the worst case. A rejected SASL
authentication (the broker replying `SASL_AUTHENTICATION_FAILED`, `UNSUPPORTED_SASL_MECHANISM` or
`ILLEGAL_SASL_STATE`) is not retried -- that one is recognized by error code and has not been measured against a real
Kafka (e2e's Kafka has SASL off; kfake closes the connection outright on an auth failure, reporting `EOF`, which is
retried). Measured in e2e: a refused connection fails startup in 2.5s, and a SYN with no answer (a black hole) in 7.9s.
The error carries the address, not the user name or the password.

**A new consumer group's first assignment**: Kafka 3.9.1 defaults to `group.initial.rebalance.delay.ms=3000`, so a new
group's first member is assigned partitions after 3.0s; a second member joining and cooperative-sticky moving one
partition to it takes about 1.0s. (e2e's Kafka sets that to 0.)

**A member leaving**: an orderly exit (`Close` leaving the group) takes about 10ms, and the other members learn of it
and take over at the next heartbeat (every 3s); a member killed with `kill -9` never leaves and its partitions wait for
the session timeout: measured in e2e, the restarted process received the redelivered message after 45.0s.

**Consuming a topic that does not exist**: franz-go records it only at DEBUG / INFO, `poll` returns nothing and it just
waits. It picks up by itself once the topic is created: on Kafka 3.9.1 the partitions were assigned 7.7s after creation
(3.8s on kfake; the metadata refresh has a 5s minimum).

**Producing to a topic that does not exist**: both Kafka 3.9.1 and kfake return `UNKNOWN_TOPIC_OR_PARTITION` after about
1.0s, and the topic is not created. The same client writing to the same missing topic a second time takes about 10s to
report an error (kfake), which is why a dead-letter write is cancelled on the spot on shutdown or revocation rather than
waited out.

**The TLS handshake is bounded by `DialTimeout`**: franz-go wraps a `net.Dialer{Timeout}` in a `tls.Dialer`, so dialing
and the handshake share one timeout; with `ServerName` empty, each broker uses the host part of its own address.

## Troubleshooting

### `xone xkafka connect failed, err=[instance "default": cannot reach 10.0.0.1:9092: …]`

Not a single broker could be reached at startup. `unable to dial … connection refused` means a wrong address or port, or
a broker that is not up; `context deadline exceeded` means the SYN went unanswered or the broker accepted the connection
and never answered (a firewall, plaintext against a TLS port, or the other way round).

### `xone xkafka connect failed, err=[instance "default": authentication to … failed: SASL_AUTHENTICATION_FAILED: …]`

A wrong user name or password, or a `SASL.Mechanism` that does not match the broker's. It is not retried.

### `xone xkafka config failed, err=[consume "orders": group is empty, every consumer needs an explicit consumer group]`

`Consume`'s second argument is the consumer group and is required.

### `xone xkafka config failed, err=[consume "orders": no Kafka client named "audit", configure it under XKafka]`

The cluster `WithClient("audit")` names is not configured.

### `xone xkafka config failed, err=[consumer for topic "orders" group "billing" on client "default" is already registered]`

The same topic + group pair was registered twice. One is enough: every partition already has a goroutine of its own.

### `xone xkafka register failed, err=[cannot consume "orders": xkafka is shutting down]`

`Consume` was called after the shutdown sequence had begun, most likely after `Start` returned or inside a stop hook.

### `xone xkafka stop failed, err=[consumers still stopping when the stop budget ran out: orders/3@1042 (group billing): context deadline exceeded]`

These partitions were still processing when the stop budget ran out (`topic/partition@offset`): either the handler does
not watch the ctx, or one message takes longer than the budget. Their ctx has been cancelled, nothing was committed and
they are redelivered later. Have the handler pass the ctx to every blocking call, configure a smaller `WithTimeout`, or
raise `xone.WithStopTimeout`.
One named as `orders/billing (committing offsets / leaving the group)` has no message in flight and is stuck committing
or leaving the group: the coordinator is unreachable.

### The consumer never receives anything

- The startup logs hold `kafka topic does not exist yet`: the topic was not created, or the name is misspelled.
- A new consumer group with `ResetOffset: latest`: what was written before the group went live is not processed. To
  process it, configure `earliest` (which only affects a group that has not committed yet).
- One partition is stuck on `kafka dead letter produce failed, retrying`: the dead-letter topic does not exist or there
  is no write permission, and that partition is stalled on that message. See
  ["Retries and dead letters"](#retries-and-dead-letters).
- One partition logged `kafka partition worker panicked`: a bug in this module gave that partition up. It is picked up
  again at the next rebalance (a member joining or leaving, or a restart) and nothing was committed, so no message is
  lost. Report it with the `stack` from that line.
- `kafka records dropped` once a minute for one partition: the same thing, seen later. That partition's records are
  being fetched and thrown away and its committed offset is standing still, while the process stays healthy and the
  other partitions carry on — from outside, the only other sign is that partition's lag growing. Nothing is lost, and
  a rebalance ends it: restarting this replica is enough, and the `stack` from the `panicked` line is what to report.
