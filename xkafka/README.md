# xkafka

Kafka：生产拿到的是原生 `*kgo.Client`（[franz-go](https://github.com/twmb/franz-go)），消费用 `xkafka.Consume` 登记一个处理函数，框架起停。

- 生产：`xkafka.C().ProduceSync(ctx, r)`，链路上下文自动写进消息头；退出时先 `Flush` 再关，缓冲里的消息不丢
- 消费：每个分区一个协程，分区内按顺序、分区之间并行；**处理完才提交**（至少一次）
- 失败重试（指数退避、带抖动），用完写进死信 topic（默认 `<topic>.dlq`）；`xutil.Permanent` 不重试；panic 被接住
- 再均衡、退出时等在途的那条处理完、提交之后才交出分区；处理函数的 ctx 不随退出信号取消
- 新消费组默认从 **latest** 开始（franz-go 默认 earliest）
- 每条消息一个 `<topic> process` Span，接着生产方的链路；日志带 `topic` / `partition` / `offset` / `group`；不记 value、Span 里不带 key
- 启动时探一次连通性，连不上直接启动失败

## 快速上手

```yaml
# conf/application.yml
XKafka:
  Brokers: ["kafka-1:9092", "kafka-2:9092"]
```

**生产**：

```go
import (
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jupiter-eng/xone/xkafka"
)

err := xkafka.C().ProduceSync(ctx, &kgo.Record{Topic: "orders", Key: []byte(orderID), Value: b}).FirstErr()

// 异步：不等结果，回调里拿错误
xkafka.C().Produce(ctx, &kgo.Record{Topic: "events", Value: b}, func(r *kgo.Record, err error) { … })
```

franz-go 的方法本来就收 ctx，所以没有 `CWithCtx`。

**Web 服务里消费**——消费者在后台跑，服务照常：

```go
func main() {
	err := xkafka.Consume("orders", "billing", func(ctx context.Context, r *kgo.Record) error {
		var o Order
		if err := json.Unmarshal(r.Value, &o); err != nil {
			return xutil.Permanent(err) // 格式不对，重试也没用：直接进死信
		}
		return charge(ctx, o) // 返回错误就重试，用完进 orders.dlq
	}, xkafka.WithTimeout(10*time.Second))
	if err != nil {
		log.Fatal(err)
	}
	xone.MustRun(xgin.New().WithRoutes(routes))
}
```

**只消费的进程**：

```go
import _ "github.com/jupiter-eng/xone/xlog" // xkafka 不带 xlog：要 JSON 日志和 topic / offset 这些字段，匿名 import 它

func main() {
	xkafka.Consume("orders", "billing", handle)
	xone.MustRun(xone.UntilSignal())
}
```

消费者在 StageServer 那一档起来——客户端（xgorm、xredis……）和你的业务钩子都已就绪，处理函数里直接 `xgorm.C()`；
停的时候它最先停，在途的消息处理完、提交之后才轮到关客户端。不用自己写 `Start`、`WaitGroup`、`WithoutCancel`，
[docs/guide.md「非 Web 服务」](../docs/guide.md#non-web-services-consumer--job)那三条规矩它都替你做了。

**生产用 `C()`，消费用 `Consume`。** 两者读同一份 `XKafka` 配置：`C()` 是这个集群上不带消费组的那个共享客户端；
消费组只能在建客户端时定，所以每个 `Consume` 用同一份配置另建一个自己的（`WithClient` 选集群，不写是 `default`）。
没有 `C().Consume`：`C()` 交出的是原生 `*kgo.Client`，挂不上方法，而且它要到 StageClient 才建好，`Consume` 却什么时候都能登记。

| 消费者用到的配置 | 作用 |
|---|---|
| `Brokers` / `SASL` / `TLS` / `DialTimeout` / `ClientID` | 怎么连，和生产完全一样 |
| `Consumer.ResetOffset` | 新消费组从 latest 还是 earliest 开始 |
| `Trace` / `Metric` / `Log` | 每条消息的 Span、处理耗时指标、逐条日志 |
| `Producer.*` | 写死信时用（死信由这个消费者自己的客户端写） |

topic、group、重试、死信写在代码里：它们决定消息会不会丢，跟着代码评审。topic、group 随环境变的，
自己的配置块里读出来再传进来，`${VAR}` 和拼错报错照样有效：

```go
xhook.BeforeStart(func(ctx context.Context) error {
	c := struct{ Topic, Group string }{}
	if err := xconfig.Unmarshal("Orders", &c); err != nil {
		return err
	}
	return xkafka.Consume(c.Topic, c.Group, handle)
})
```

## 配置

```yaml
XKafka:
  Brokers: ["127.0.0.1:9092"]   # 必填，种子 broker
  ClientID: ""                  # 空 = XApp.Name，XApp.Name 也空就是 franz-go 的 kgo
  DialTimeout: 1s               # TCP + TLS 握手；0 = franz-go 默认的 10s
  SASL:
    Mechanism: ""               # PLAIN / SCRAM-SHA-256 / SCRAM-SHA-512；空 = 不认证
    Username: ""
    Password: "${KAFKA_PASSWORD}"
  TLS:                          # 规则见 xtls；ServerName 空着 = 每个 broker 地址的主机部分
    Enable: false
    CAFile: ""
    CertFile: ""
    KeyFile: ""
    ServerName: ""
  Producer:
    Acks: all                   # all / leader / none；all 时开着幂等写，另两个替你关掉
    Linger: 10ms                # 攒批最多等多久；0 = 不等
    Compression: snappy         # none / gzip / snappy / lz4 / zstd
  Consumer:
    ResetOffset: latest         # 新消费组从哪开始：latest / earliest
  Trace: true                   # 生产、消费各一个 Span；关掉之后链路上下文照样写进、读出消息头
  Metric: true                  # kafka_consume_duration_seconds
  Log: true                     # 逐条的日志：生产失败、每次处理失败（WARN），处理成功（DEBUG）
```

**多集群**写在 `Clients` 下，每个集群的字段同上，没写的用上面的默认值：

```yaml
XKafka:
  Clients:
    default: {Brokers: ["kafka:9092"]}
    audit:   {Brokers: ["kafka-audit:9092"], Consumer: {ResetOffset: earliest}}
```

`xkafka.C()` 取 `default`，`xkafka.C("audit")` 取另一个；消费另一个集群写 `xkafka.WithClient("audit")`。两种写法不能混用。

没列出来的 franz-go 设置一律是它的默认值，数字见[「行为与实测」](#行为与实测)。要更细的控制（事务、自定义分区器），
自己 `kgo.NewClient`，框架不挡路。

## API

| 函数 | 说明 |
|---|---|
| `C(name ...string) *kgo.Client` | 取生产用的客户端，不带参数取 `default`。没有消费组。取不到直接 panic，消息里说清是调早了、没配还是名字写错 |
| `Has(name ...string) bool` | 集群配了没有，可选依赖先判断 |
| `Names() []string` | 配了哪些集群 |
| `New(ctx, cfg) (*kgo.Client, io.Closer, error)` | 纯构造器：不碰全局、不读配置文件。探一次连通性；返回的 Closer 只是 `Close`——关之前自己 `Flush(ctx)` |
| `Consume(topic, group, fn, opts...) error` | 登记一个消费者，见下 |

`Consume` 当场校验，错误是 `*xerror.Error`（模块 `xkafka`）：`topic`、`group` 为空、`fn` 是 nil、Option 不成立、同一个集群上
同一对 `topic` + `group` 登记两次是 `config`；退出流程开始之后再 `Consume` 是 `register`。`group` 必填，没有默认值：
同一个 group 的几个进程分摊分区，不同的 group 各自收到全部消息——这是要想清楚再写的事。

什么时候调都行：`xone.Run` 之前登记的在 StageServer 一起开始；之后（钩子里、`Start` 里）登记的当场开始，
这时集群名不对也当场返回错误。

| Option | 默认 | 说明 |
|---|---|---|
| `WithClient(name)` | `default` | 消费哪个集群。每个 `Consume` 用那个集群的配置另建一个自己的客户端（带消费组），不和 `C()` 共用 |
| `WithTimeout(d)` | 30s | 每一次调用处理函数的超时，到点取消它的 ctx；必须大于 0。每次重试各算各的 |
| `WithRetry(n)` | 3 | 失败（返回错误或 panic）之后**再试**几次，不算第一次：默认最多调 4 次。0 是不重试 |
| `WithDeadLetter(topic)` | `<topic>.dlq` | 重试用完写到哪个 topic；`""` 关掉死信：记 ERROR 跳过。不能是消费的 topic 本身 |

死信消息多带的头是导出的常量：`HeaderDLQTopic`、`HeaderDLQPartition`、`HeaderDLQOffset`、`HeaderDLQGroup`、`HeaderDLQError`。

## 语义

### 提交：至少一次

一条消息**处理完**（成功、进了死信、或者死信关着时被跳过）才标记，franz-go 每 5s 把标记过的提交一次，再均衡和退出时
当场提交。进程崩溃、被 `kill -9` 时，没提交的消息之后重新投递——**处理函数要能承受重复**（幂等写、按业务主键去重）。
被 kill 前 5s 内处理完的那些也可能再来一遍（e2e 实测：kill 前处理完的那一条，重启之后又收到了一次）。

franz-go 默认的自动提交不是这样：它提交的是「上一次 poll 取到的」，不管处理完没有。每个分区一个协程的写法里，
poll 和处理不在一起，取到的消息可能还排在队列里就提交了——进程一崩就丢。所以这里换成 `AutoCommitMarks`。

### 并发与顺序

每个分到的分区一个协程：同一个分区里一条处理完才处理下一条（同一个 key 的消息在同一个分区里，顺序不乱）；
不同分区之间并行。并行度就是这个进程分到的分区数——要更多并行，加分区或者加进程。

一个分区处理得慢、它前面排着的批次满了（每个分区最多排 4 批，一批最多 500 条）时，派发停下等它，其余分区的派发也跟着等：
慢的那个分区不会一直往内存里攒。这是 franz-go 示例（`goroutine_per_partition_consuming/autocommit_marks`）的做法。

### 重试与死信

处理函数返回错误或 panic → 等一会儿重试，等待从 1s 起逐次翻倍、带抖动（实际等 `[0, 上界]` 里的随机值，同 `xutil.Retry`），
上界依次 1s、2s、4s，最多 30s。返回 `xutil.Permanent(err)` 的不重试。

重试用完 → 把原消息（key、value、全部头）写进死信 topic，另加几个头：

| 头 | 值 |
|---|---|
| `xkafka-dlq-topic` / `xkafka-dlq-partition` / `xkafka-dlq-offset` | 原消息的位置，十进制 |
| `xkafka-dlq-group` | 处理失败的消费组 |
| `xkafka-dlq-error` | 最后一次的错误文本，截到 256 字节 |

`traceparent` 换成这次处理的 Span：死信接着这条链路。**错误文本会和消息一起落进死信 topic**：截短只是不让它撑大消息头，
挡不住内容。死信 topic 本来就装着原消息的 value，它的读权限要和原 topic 一样收紧；别把凭证拼进错误里。

**死信写不进去**（topic 不存在、没有写权限、集群挂了）→ 记 WARN，退避重试（同上，最多 30s 一次），一直到写进去为止：
不标记、不往下处理——丢掉它是静默丢消息，跳过它去处理下一条的话，提交的 offset 就越过了它。这期间这个分区停着，
别的分区照常。退出、分区被收回时放弃，这条没提交，之后重新投递（死信里可能因此多一份）。
**死信 topic 要事先建好**：franz-go 不替你自动建（见[「行为与实测」](#行为与实测)）。

`WithDeadLetter("")` → 重试用完记一条 ERROR `kafka message skipped, …` 就提交、往下处理。

### 再均衡

分区被收回时（有别的组员加入、离开），先等这些分区在途的那一条处理完，再提交标记过的 offset，然后才让再均衡继续：
新的组员从下一条开始，不会把处理过的再处理一遍，也不会和这里同时处理同一条。排着还没开始的不管，新组员会从头处理它们。
等的是**一次**处理函数调用：退避等待、还没开始的重试、死信的重试在分区被收回时当场放弃（这条不提交）。
所以 `WithTimeout` 别配得比 franz-go 的再均衡超时（60s）还长，否则这个组员会被踢出消费组。

分区「丢了」（会话超时之类）时同样等在途的那条，但不提交：它已经不归这里了。

分配策略是 franz-go 默认的 cooperative-sticky：再均衡只动要挪的那几个分区，别的分区不停。

### 退出

SIGTERM / SIGINT 之后，消费者的停止钩子（StageServer，最先跑）：

1. 不再取新消息；
2. 离开消费组，全部分区按「再均衡」那一套收回：等在途的那条处理完、提交；
3. 关掉这个消费者的客户端。

**处理函数的 ctx 不随退出信号取消**——沿用一个已取消的 ctx，这条消息里每一次写库、调下游都会一进去就被拒绝。
它只受 `WithTimeout` 管。停止预算用完还没处理完的，取消它们的 ctx，停止钩子返回 `stop` 错误并点名是哪几个分区、处理到哪个 offset，
另记一条 WARN；这些消息没提交，之后重新投递。

之后才轮到 StageClient 的生产客户端：先 `Flush`（把 `Produce` 异步发出、还在缓冲里的消息发完），再关。

**单条消息的处理时间要小于停止预算**（`xone.WithStopTimeout`，默认 15s；Web 服务里先用掉服务那一段，剩下的各停止钩子分着用），
否则退出时会被取消、重新投递。

### 新消费组从哪开始

`Consumer.ResetOffset` 默认 `latest`：一个消费组第一次消费某个分区时，只处理之后新来的。franz-go 的默认是 earliest——
新上线的组会把 topic 里还留着的全部历史处理一遍，往往是几天的量。代价：新组上线、topic 刚建好的那一刻之前写进去的消息，
这个组不会处理；要它们就配 `earliest`。已经提交过 offset 的组不受影响，从提交的位置接着。

### topic 不存在

启动时 topic 不存在：记一条 WARN `kafka topic does not exist yet, the consumer waits for it to be created`，**不让启动失败**——
topic 常常由生产方或者运维晚一步建，消费方先上线是正常的部署顺序。建好之后几秒内自动接上（配合 `latest` 的话，
建好那一刻之前写进去的不处理）。

生产到不存在的 topic：`ProduceSync` 约 1s 后返回 `UNKNOWN_TOPIC_OR_PARTITION`，不会替你建。

## 多实例

每个集群一个生产客户端，`xkafka.C("name")` 取；每个 `Consume` 一个自己的客户端。启动时按名字排序挨个建，有一个连不上就把已建好的全关掉，
错误里点名是哪一个。规则同 [docs/guide.md「多实例」](../docs/guide.md#several-instances)。

## 可观测

### 日志

| 消息 | 级别 | 字段 |
|---|---|---|
| `xkafka connected` | INFO | `name`、`brokers`、`tls`、`sasl`（机制名） |
| `xkafka ready` | INFO | `instances` |
| `xkafka consumers started` | INFO | `consumers`（`topic/group`） |
| `kafka produce failed` | WARN | `name`、`topic`、`partition`、`key`、`error`（需 `Log: true`）。没分到分区就失败的（topic 不存在）`partition` 是 0，别据此判断 |
| `kafka message processed` | DEBUG | 消息字段、`elapsed_ms`（需 `Log: true`） |
| `kafka message failed` | WARN | 消息字段、`attempt`（第几次）、`elapsed_ms`、`error`（需 `Log: true`） |
| `kafka message dead-lettered` | WARN | 消息字段、`dlq_topic`、`attempts`、`error` |
| `kafka message skipped, retries exhausted and dead lettering is off` | ERROR | 消息字段、`attempts`、`error` |
| `kafka handler panicked` | ERROR | 消息字段、`error`、`stack` |
| `kafka dead letter produce failed, retrying` | WARN | 消息字段、`dlq_topic`、`error` |
| `kafka fetch failed` | WARN | `topic`、`partition`、`group`、`error` |
| `kafka commit on partition revoke failed, processed messages may be redelivered` | WARN | `topic`、`group`、`error` |
| `kafka topic does not exist yet, the consumer waits for it to be created` | WARN | `topic`、`group` |
| `xkafka consumers still stopping when the stop budget ran out` | WARN | `busy`：还在处理的 `topic/分区@offset (group …)`，或者卡在提交、离开消费组上的 `topic/group (committing offsets / leaving the group)` |
| `xkafka franz-go log` | WARN / ERROR | `name`、`detail`（franz-go 的原话）、`fields`（它的键值，组成一个对象） |

消息字段是 `topic`、`partition`、`offset`、`group`，再加 `key`：

- **key** 是可打印的 UTF-8 时整条记（超过 256 字节截断，带 `key_truncated: true`），规则同 xredis；二进制的 key（Avro、protobuf、整数）
  只记 `key_len`；没有 key 的不记。别把令牌、手机号拼进 key。
- **value 从不记**，哪一条日志都没有。
- `error` 是处理函数返回的原文，同 xcron：它常常包着 SQL、下游的响应。

**处理函数里写的日志**：ctx 里带着 `topic` / `partition` / `offset` / `group` 这四个字段的作用域——装了 xlog 时
（`slog.InfoContext(ctx, …)`）每一行都带上，同 xcron 的 `job`。本模块不 import xlog：只用 xkafka 生产的程序不会被换掉 `slog.Default()`；
只消费的进程要 JSON 日志和这几个字段，匿名 import `github.com/jupiter-eng/xone/xlog`。`Log: false` 只关上面标了「需 `Log: true`」的几条：
进死信、被跳过、panic、死信写不进去这几种意味着一条消息没走完正常的路，照样记。

日志的全局约定见 [`docs/observability.md`](../docs/observability.md#logs)。

### 指标

| 指标 | 类型 | 标签 | 来源 |
|---|---|---|---|
| `kafka_consume_duration_seconds` | histogram | `topic`、`group`、`status` | xkafka 消费，`XKafka.Metric`，按集群 |

一条消息从开始处理到有结果的耗时，含重试和写死信。`status`：`ok`、`dead_letter`、`skipped`、`aborted`（退出、分区被收回时没处理完，之后重新投递）。
桶是 prometheus 的默认（5ms 到 10s）。不带分区：序列数不随分区数涨。生产没有单独的指标，用 franz-go 的钩子自己接。

### 链路

| 来源 | Span 名 | 种类 | 属性 |
|---|---|---|---|
| 生产 | `<topic> publish` | Producer | `messaging.system=kafka`、`messaging.operation.name=publish`、`messaging.destination.name`；成功时另有 `messaging.destination.partition.id`、`messaging.kafka.offset` |
| 消费 | `<topic> process` | Consumer | 同上的前三个（`process`），加 `messaging.destination.partition.id`、`messaging.kafka.offset`、`messaging.consumer.group.name` |

- **链路接得上**：生产时用全局 Propagator 把链路上下文（W3C `traceparent`、`baggage`、xtrace 的透传 Header）写进消息头，
  写的是 publish Span 的上下文；消费时从消息头取出来当 process 的父亲。HTTP 请求 → 生产 → 消费是同一个 `trace_id`。
- **不带 key、不带 value**：franz-go 自带的 kotel 插件把 key 写进 `messaging.kafka.message.key`，这里不用它。
- **出错只标状态**：生产失败 `kafka produce failed`、消息没成功处理（进了死信、被跳过）`kafka message failed`，不记错误原文，同 xcron。
- **消息头算可信来源**：xtrace 只收可信对端的透传 Header 和 baggage（HTTP 那一侧是 `TrustedProxies` 里的直连地址）。
  能往 topic 里写的是持有集群凭证的生产者，本模块生产时注入的也只是本进程已经认下的值，所以消息头当成可信的照收。
  对外开放写入、第三方直接往里写的 topic，别在它的消费者上依赖透传 Header 和 baggage。`traceparent` 不受这一条管。
- `Trace: false` 只是不开 Span：链路上下文照样写进、读出消息头。

链路的全貌、传播与信任边界见 [`docs/observability.md`「链路」](../docs/observability.md#tracing)。

## 行为与实测

实测环境和跨模块的总表见 [`docs/behavior.md`](../docs/behavior.md)。

franz-go v1.21.7、kfake（同一个提交）、Apache Kafka 3.9.1（KRaft 单节点，`mirror.gcr.io/apache/kafka:3.9.1`）、Go 1.25。
franz-go 默认值由 `franz_test.go` 里 `TestFranzDefault_*` 那一组钉着：升级之后哪一条红了，这里对应的数字就要重量。

**franz-go 的默认值**（`cl.OptValue` 实测）：

| 设置 | franz-go 默认 | 这里 |
|---|---|---|
| `ClientID` | `kgo` | `XApp.Name`（没配时仍是 `kgo`） |
| `RequiredAcks` / 幂等写 | all / 开 | 不改（`Producer.Acks` 可改，非 all 时替你关幂等写——franz-go 不关就 `NewClient` 报错） |
| `ProducerLinger` | 10ms | 不改（`Producer.Linger`） |
| `ProducerBatchCompression` | snappy，不行退回 none | 不改（`Producer.Compression`） |
| `RecordRetries` / `RecordDeliveryTimeout` | 不限 / 不限 | 不改：一条消息会一直重试到成功或者它的 ctx 结束——**给 `ProduceSync` 的 ctx 带上截止时间**（实测 broker 不可达时 200ms 的 ctx 200ms 返回） |
| `UnknownTopicRetries` | 4 | 不改：写不存在的 topic 约 1s 报错 |
| `AllowAutoTopicCreation` | 关 | 不改：broker 开着 `auto.create.topics.enable`（Kafka 默认开）也不会被自动建 |
| `DialTimeout` | 10s | 1s（`DialTimeout`） |
| `RequestTimeoutOverhead` | 10s | 不改 |
| 自动提交 | 每 5s，提交「上一次 poll 取到的」 | `AutoCommitMarks`：只提交处理完的，间隔仍是 5s |
| `ConsumeResetOffset` | 最早（AtStart） | `latest`（`Consumer.ResetOffset`） |
| `SessionTimeout` / `RebalanceTimeout` / `HeartbeatInterval` | 45s / 60s / 3s | 不改 |
| 分配策略 | cooperative-sticky | 不改 |
| 日志 | 不记（nopLogger） | 接到 slog，WARN 及以上 |

**自动提交提交没处理的消息**：poll 到 3 条、一条都没处理，再 poll 一次，`Close` 时默认的 `OnPartitionsRevoked` 提交了 3
（`TestFranzDefault_AutocommitCommitsPolledButUnprocessed`）。只 poll 过一次就 `Close` 的不提交——它提交的是「上一次 poll」的，
同步处理的循环（poll、处理完、再 poll）因此是安全的；poll 和处理分开的不安全。

**`Close` 不 `Flush`**：缓冲里的消息全部以 `kgo.ErrClientClosed` 失败（`Linger` 1 分钟时写 10 条，`Close` 之后 10 条全失败）。
停止钩子先 `Flush(ctx)` 再 `Close`；直接调 `New` 的自己先 `Flush`。

**`Ping` 对不回话的 broker 不听 ctx**：对端收下 TCP 连接却不回话时，`Ping` 等的是读超时（`RequestTimeoutOverhead` 10s）：
给了 200ms 截止时间的 `Ping` 10.0s 才返回，不给截止时间 20s，期间取消 ctx 也一样等。启动探测因此放在协程里、这边看着 ctx。
拒绝连接时不到 1ms 就报错。

**启动探测**：每个集群探一次（向 broker 要一次元数据），3 次、每次预算 2 × `DialTimeout`（默认 2s）、两次退避上界 1s、2s，
最坏 9s。SASL 认证被拒（broker 回 `SASL_AUTHENTICATION_FAILED`、`UNSUPPORTED_SASL_MECHANISM`、`ILLEGAL_SASL_STATE`）不重试——
这一条按错误码认，没有在真 Kafka 上量过（e2e 的 Kafka 没开 SASL；kfake 认证失败时直接断开连接，报的是 `EOF`，会重试）。e2e 实测：拒绝连接 2.5s 启动失败，SYN 没有回音（黑洞）7.9s。
错误里是地址，没有用户名、密码。

**新消费组的首次分配**：Kafka 3.9.1 默认 `group.initial.rebalance.delay.ms=3000`，一个新组第一个组员分到分区要 3.0s；
第二个组员加入、cooperative-sticky 挪一个分区给它约 1.0s。（e2e 的 Kafka 把这一项配成 0。）

**组员离开**：正常退出（`Close` 离开消费组）约 10ms，别的组员在下一次心跳（3s 一次）知道、接手；
`kill -9` 的组员不会离开，它的分区要等会话超时：e2e 实测重启的进程 45.0s 之后才收到重新投递的消息。

**topic 不存在时消费**：franz-go 只在 DEBUG / INFO 级别记一笔，`poll` 什么都不返回，一直等。建好之后自己接上：
Kafka 3.9.1 上建好 7.7s 之后分到分区（kfake 3.8s，元数据最短刷新间隔 5s）。

**生产到不存在的 topic**：Kafka 3.9.1、kfake 都是约 1.0s 返回 `UNKNOWN_TOPIC_OR_PARTITION`，topic 没有被建出来。
同一个客户端第二次写同一个不存在的 topic 要约 10s 才报错（kfake），所以写死信的那一次随退出、收回分区当场取消，不干等。

**TLS 握手受 `DialTimeout` 管**：franz-go 用 `tls.Dialer` 套着 `net.Dialer{Timeout}`，拨号和握手共用一个超时；
`ServerName` 空着时每个 broker 用它自己地址的主机部分。

## 排错

### `xone xkafka connect failed, err=[instance "default": cannot reach 10.0.0.1:9092: …]`

启动时一个 broker 都连不上。`unable to dial … connection refused` 是地址或端口写错、broker 没起；`context deadline exceeded`
是 SYN 没有回音或者 broker 收下连接却不回话（防火墙、对着 TLS 端口说明文、反过来）。

### `xone xkafka connect failed, err=[instance "default": authentication to … failed: SASL_AUTHENTICATION_FAILED: …]`

用户名或密码不对，或者 `SASL.Mechanism` 和 broker 上配的不一致。不重试。

### `xone xkafka config failed, err=[consume "orders": group is empty, every consumer needs an explicit consumer group]`

`Consume` 的第二个参数是消费组，必填。

### `xone xkafka config failed, err=[consume "orders": no Kafka client named "audit", configure it under XKafka]`

`WithClient("audit")` 指的集群没配。

### `xone xkafka config failed, err=[consumer for topic "orders" group "billing" on client "default" is already registered]`

同一对 topic + group 登记了两次。一个就够：每个分区已经各有一个协程。

### `xone xkafka register failed, err=[cannot consume "orders": xkafka is shutting down]`

退出流程已经开始之后才 `Consume`。多半是在 `Start` 返回之后、或者停止钩子里登记的。

### `xone xkafka stop failed, err=[consumers still stopping when the stop budget ran out: orders/3@1042 (group billing): context deadline exceeded]`

这些分区在停止预算用完时还在处理（`topic/分区@offset`）：处理函数没看 ctx，或者一条消息比预算还长。它们的 ctx 已被取消、
没提交，之后重新投递。让处理函数把 ctx 传给每一个阻塞操作、配小 `WithTimeout`，或者调大 `xone.WithStopTimeout`。
点的是 `orders/billing (committing offsets / leaving the group)` 的，没有消息在处理，卡在提交或者离开消费组上：协调者连不上。

### 消费者一直收不到消息

- 启动日志里有 `kafka topic does not exist yet`：topic 没建，或者名字写错。
- 新消费组、`ResetOffset: latest`：组上线之前写进去的不处理。要处理就配 `earliest`（只对还没提交过的组有效）。
- 某个分区卡在 `kafka dead letter produce failed, retrying`：死信 topic 没建或没有写权限，这个分区停在那一条上，见[「重试与死信」](#重试与死信)。
