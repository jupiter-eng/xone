package xkafka

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"

	"github.com/jupiter-eng/xone/xerror"
)

// Consume 登记一个消费者：从 topic 里以消费组 group 的身份消费，每条消息调一次 fn。
//
//	err := xkafka.Consume("orders", "billing", func(ctx context.Context, r *kgo.Record) error {
//		return charge(ctx, r.Value)
//	}, xkafka.WithTimeout(10*time.Second))
//
// 语义（详见 README「语义」）：
//   - 每个分配到的分区一个协程：同一分区里按顺序一条一条处理，不同分区之间并行；
//   - 一条消息处理完（成功、进了死信、或者被跳过）才提交它的 offset——至少一次：
//     进程崩溃、被 kill -9 的话，没处理完的消息之后会重新投递，fn 要能承受重复；
//   - fn 返回错误或 panic 时按 WithRetry 重试，用完了写进死信 topic（WithDeadLetter）；
//   - 退出时不再取新消息，等在途的那一条处理完、提交之后才离开消费组。fn 收到的 ctx 不随退出信号取消，
//     只受 WithTimeout 管；停止预算用完时才取消。
//
// group 必填：同一个 group 的多个进程分摊分区，不同的 group 各自收到全部消息。
// 新的 group 从哪里开始见 ConsumerConfig.ResetOffset（默认 latest）。
//
// 当场校验，返回 *xerror.Error（模块 xkafka）：topic、group 为空、fn 是 nil、Option 不成立、
// 同一个集群上同一对 topic + group 登记了两次是 config；退出流程已经开始之后再 Consume 是 register。
//
// 什么时候调都行：在 xone.Run 之前登记的，在 StageServer 那一档一起开始消费；之后（比如在某个钩子里、
// 在 Start 里）登记的，当场开始，这时集群名不对、建不了客户端也当场返回错误。
func Consume(topic, group string, fn func(ctx context.Context, r *kgo.Record) error, opts ...Option) error {
	switch {
	case strings.TrimSpace(topic) == "":
		return xerror.Newf("xkafka", "config", "consume: topic is empty")
	case strings.TrimSpace(group) == "":
		return xerror.Newf("xkafka", "config", "consume %q: group is empty, every consumer needs an explicit consumer group", topic)
	case fn == nil:
		return xerror.Newf("xkafka", "config", "consume %q: handler is nil", topic)
	}
	o := buildOptions(topic, opts)
	if err := o.validate(topic); err != nil {
		return xerror.Newf("xkafka", "config", "consume %q group %q: %w", topic, group, err)
	}
	return std.add(&consumer{topic: topic, group: group, fn: fn, o: o})
}

// ---- 一个进程里的全部消费者 ----

// state 消费者们走到哪一步了
type state int

const (
	idle    state = iota // 还没起来：Consume 只登记
	running              // 起来了：Consume 当场开始
	stopped              // 开始停了（或停完了）：Consume 报错
)

// manager 一个进程里的全部消费者，由 xkafka StageServer 那一对钩子起停。
// lookup 按集群名取集群：正常是注册表，测试换成自己的
type manager struct {
	lookup func(name string) (instance, bool)

	mu        sync.Mutex
	state     state
	consumers []*consumer
}

var std = &manager{lookup: func(name string) (instance, bool) { return reg.Lookup(name) }}

func startConsumers(ctx context.Context) error { return std.start(ctx) }

func stopConsumers(ctx context.Context) error { return std.stop(ctx) }

// add 登记一个消费者，已经起来的话当场开始
func (m *manager) add(c *consumer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == stopped {
		return xerror.Newf("xkafka", "register", "cannot consume %q: xkafka is shutting down", c.topic)
	}
	if slices.ContainsFunc(m.consumers, c.same) {
		return xerror.Newf("xkafka", "config", "consumer for topic %q group %q on client %q is already registered", c.topic, c.group, c.o.client)
	}
	if m.state == running {
		if err := c.prepare(context.Background(), m.lookup); err != nil {
			return err
		}
		c.launch()
	}
	m.consumers = append(m.consumers, c)
	return nil
}

// start 先给每个消费者建好客户端，全部成功了才开始消费：
// 一个起不来时还一条消息都没取过，关掉已建的客户端就收拾干净了（这一档的停止钩子不会被调到）
func (m *manager) start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range m.consumers {
		if err := c.prepare(ctx, m.lookup); err != nil {
			for _, p := range m.consumers[:i] {
				p.cl.CloseAllowingRebalance()
			}
			m.state = stopped
			return err
		}
	}
	for _, c := range m.consumers {
		c.launch()
	}
	m.state = running
	if len(m.consumers) > 0 {
		slog.InfoContext(ctx, "xkafka consumers started", "consumers", m.names())
	}
	return nil
}

// stop 并行停下全部消费者，等它们都停完或者 ctx 到点
func (m *manager) stop(ctx context.Context) error {
	m.mu.Lock()
	wasRunning := m.state == running
	m.state = stopped
	cs := slices.Clone(m.consumers)
	m.mu.Unlock()
	if !wasRunning {
		return nil
	}

	busy := make([][]string, len(cs))
	var wg sync.WaitGroup
	for i, c := range cs {
		wg.Go(func() { busy[i] = c.stop(ctx) })
	}
	wg.Wait()
	return joinBusy(ctx, busy)
}

func (m *manager) names() []string {
	out := make([]string, len(m.consumers))
	for i, c := range m.consumers {
		out[i] = c.String()
	}
	return out
}

// joinBusy 停止预算用完时还没停完的，点名报出来：在处理消息的分区（topic/分区@offset），或者卡在提交、离开消费组上的消费者。
// 框架也在看同一个截止时间，可能先一步返回、把这里的错误丢掉，所以点名的这一条日志无论如何都要写出去
func joinBusy(ctx context.Context, busy [][]string) error {
	all := slices.Concat(busy...)
	if len(all) == 0 {
		return nil
	}
	slog.WarnContext(ctx, "xkafka consumers still stopping when the stop budget ran out", "busy", all)
	return xerror.Newf("xkafka", "stop", "consumers still stopping when the stop budget ran out: %s: %w",
		strings.Join(all, ", "), ctx.Err())
}

// ---- 一个消费者 ----

// consumer 一次 Consume 登记的消费者，有它自己的 *kgo.Client（带着消费组）
type consumer struct {
	topic, group string
	fn           func(context.Context, *kgo.Record) error
	o            options

	// 下面这些在 prepare 里填好
	cl     *kgo.Client
	log    bool
	trace  bool
	metric bool

	// base 每条消息的 ctx 的父亲：不随退出信号取消（在途的消息要处理完）。
	// hard 在停止预算用完时才取消，叫停还没返回的处理函数
	base       context.Context
	hard       context.Context
	hardCancel context.CancelFunc

	pollCancel context.CancelFunc
	pollDone   chan struct{}

	mu       sync.Mutex
	assigned map[int32]*worker  // 当前分到的分区
	live     map[*worker]string // 还没退出的分区协程（包括刚被收回、正在等在途消息的），值是 topic/partition
}

func (c *consumer) String() string { return c.topic + "/" + c.group }

// same 同一个集群上同一对 topic + group：登记两次只是让同一个进程里多一个组员，分区一样分
func (c *consumer) same(o *consumer) bool {
	return c.topic == o.topic && c.group == o.group && c.o.client == o.o.client
}

// maxPollRecords 一次最多取多少条来分给各分区。取多了，再均衡要多等这一批派发完
const maxPollRecords = 500

// workerQueue 每个分区的协程前面最多排几批没处理的消息。排满了派发就停下等它——
// 一个分区慢下来，其余分区的派发也跟着等，这是 franz-go 示例（goroutine_per_partition_consuming）的做法：
// 不这样的话，慢的那个分区会一直往内存里攒
const workerQueue = 4

// prepare 建好这个消费者自己的客户端，还不开始消费
func (c *consumer) prepare(ctx context.Context, lookup func(string) (instance, bool)) error {
	inst, ok := lookup(c.o.client)
	if !ok {
		return xerror.Newf("xkafka", "config", "consume %q: no Kafka client named %q, configure it under %s", c.topic, c.o.client, ConfigKey)
	}
	opts, err := clientOpts(inst.name, inst.cfg)
	if err != nil {
		return err
	}
	opts = append(opts,
		kgo.ConsumerGroup(c.group),
		kgo.ConsumeTopics(c.topic),
		kgo.ConsumeResetOffset(resetOffset(inst.cfg.Consumer.ResetOffset)),
		// 只提交标记过的：一条消息处理完才标记。franz-go 默认的自动提交提交的是「上一次 poll 取到的」，
		// 不管处理完没有——这里 poll 和处理在不同的协程里，取到的消息可能还排在分区的队列里，
		// 进程一崩就丢了（v1.21.7 实测，见 TestFranzDefault_AutocommitCommitsPolledButUnprocessed）
		kgo.AutoCommitMarks(),
		// poll 到 AllowRebalance 之间不再均衡：派发和收回分区不会同时发生，分区协程的表不用和 poll 抢。
		// franz-go 示例的做法（autocommit_marks）
		kgo.BlockRebalanceOnPoll(),
		kgo.OnPartitionsAssigned(c.assignedFn),
		kgo.OnPartitionsRevoked(c.revokedFn),
		kgo.OnPartitionsLost(c.lostFn),
	)
	// 这些要在 NewClient 之前填好：它一建好就开始入组，分区分配的回调（assignedFn）可能在它返回之前就来了
	c.log, c.trace, c.metric = inst.cfg.Log, inst.cfg.Trace, inst.cfg.Metric
	c.base = context.WithoutCancel(ctx)
	c.hard, c.hardCancel = context.WithCancel(c.base)
	c.assigned, c.live = map[int32]*worker{}, map[*worker]string{}
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return xerror.Newf("xkafka", "new", "consume %q: create client: %w", c.topic, err)
	}
	// 消息只在 launch 起了 poll 之后才派给分区协程，它们读 c.cl 时这里早写完了
	c.cl = cl
	if c.metric {
		registerMetrics()
	}
	c.warnIfMissing(ctx)
	return nil
}

func resetOffset(s string) kgo.Offset {
	if s == "earliest" {
		return kgo.NewOffset().AtStart()
	}
	return kgo.NewOffset().AtEnd()
}

// warnIfMissing topic 不存在时记一条 WARN，不让启动失败。
//
// franz-go v1.21.7 对不存在的 topic 一声不吭地等：它的日志里只有 DEBUG / INFO，poll 什么都不返回，
// topic 建好之后几秒内（元数据最短刷新间隔 5s）自己接上（实测 kfake 3.8s、Kafka 3.9.1 见 README）。
// 不让启动失败：topic 常常由生产方或者运维晚一步建，消费方先上线是正常的部署顺序。
// 但「一直没消息」时要有个线索，所以记一条。查不了（broker 一时连不上）就不说，那由连接的错误去说
func (c *consumer) warnIfMissing(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req := kmsg.NewPtrMetadataRequest()
	t := kmsg.NewMetadataRequestTopic()
	t.Topic = kmsg.StringPtr(c.topic)
	req.Topics = append(req.Topics, t)
	resp, err := req.RequestWith(ctx, c.cl)
	if err != nil || len(resp.Topics) == 0 {
		return
	}
	if errors.Is(kerr.ErrorForCode(resp.Topics[0].ErrorCode), kerr.UnknownTopicOrPartition) {
		slog.WarnContext(ctx, "kafka topic does not exist yet, the consumer waits for it to be created",
			"topic", c.topic, "group", c.group)
	}
}

// launch 开始消费：一个协程 poll，按分区派发
func (c *consumer) launch() {
	var ctx context.Context
	ctx, c.pollCancel = context.WithCancel(c.base)
	c.pollDone = make(chan struct{})
	go c.poll(ctx)
}

// poll 取消息、按分区派给各自的协程，直到 stop 取消 ctx
func (c *consumer) poll(ctx context.Context) {
	defer close(c.pollDone)
	for {
		fs := c.cl.PollRecords(ctx, maxPollRecords)
		if ctx.Err() != nil || fs.IsClientClosed() {
			// 这一批不派了：没处理就不会被标记，下一个拿到这些分区的组员（或者重启之后的自己）从头处理
			return
		}
		fs.EachError(func(topic string, partition int32, err error) {
			slog.WarnContext(ctx, "kafka fetch failed", "topic", topic, "partition", partition, "group", c.group, "error", err)
		})
		fs.EachPartition(func(p kgo.FetchTopicPartition) {
			if len(p.Records) == 0 {
				return
			}
			c.mu.Lock()
			w := c.assigned[p.Partition]
			c.mu.Unlock()
			if w == nil {
				return // BlockRebalanceOnPoll 下不会发生：收回分区要等这里 AllowRebalance
			}
			select {
			case w.recs <- p.Records:
			case <-ctx.Done():
			}
		})
		c.cl.AllowRebalance()
	}
}

// stop 停下这个消费者：不再取新消息；离开消费组时 franz-go 收回全部分区，
// revokedFn 等在途的消息处理完、提交标记过的 offset；然后关掉客户端。
// ctx 到点还没停完的，取消在途处理函数的 ctx，返回还在处理的分区
func (c *consumer) stop(ctx context.Context) []string {
	c.pollCancel()
	<-c.pollDone

	done := make(chan struct{})
	go func() {
		// 不是 Close：poll 停在 PollRecords 和 AllowRebalance 之间的话，Close 会一直等那次被挡住的再均衡
		c.cl.CloseAllowingRebalance()
		close(done)
	}()
	defer c.hardCancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
	}
	// 先点名再叫停：叫停之后它们陆续返回，再点就点不全了。
	// 没有在途消息却还没停完的，卡在提交或者离开消费组上（协调者连不上），点这个消费者的名
	busy := c.busy()
	if len(busy) == 0 {
		busy = []string{c.String() + " (committing offsets / leaving the group)"}
	}
	c.hardCancel()
	return busy
}

// busy 还在处理消息的分区，topic/partition@offset
func (c *consumer) busy() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for w, name := range c.live {
		if off := w.inflight.Load(); off > 0 {
			out = append(out, fmt.Sprintf("%s@%d (group %s)", name, off-1, c.group))
		}
	}
	slices.Sort(out)
	return out
}

// ---- 再均衡 ----

func (c *consumer) assignedFn(_ context.Context, _ *kgo.Client, assigned map[string][]int32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range assigned[c.topic] {
		w := newWorker(c, p)
		c.assigned[p] = w
		c.live[w] = partitionStr(c.topic, p)
		go w.run()
	}
}

// revokedFn 分区被收回（再均衡、离开消费组）：等这些分区的在途消息处理完，提交标记过的 offset，
// 这之后 franz-go 才让再均衡继续。新的组员因此从下一条开始，不会把处理过的再处理一遍
func (c *consumer) revokedFn(ctx context.Context, cl *kgo.Client, revoked map[string][]int32) {
	c.kill(revoked[c.topic])
	if err := cl.CommitMarkedOffsets(ctx); err != nil {
		slog.WarnContext(ctx, "kafka commit on partition revoke failed, processed messages may be redelivered",
			"topic", c.topic, "group", c.group, "error", err)
	}
}

// lostFn 分区丢了（会话超时之类）：已经不是这个组员的了，提交会被 broker 拒绝。只等在途的消息处理完
func (c *consumer) lostFn(_ context.Context, _ *kgo.Client, lost map[string][]int32) {
	c.kill(lost[c.topic])
}

// kill 让这些分区的协程处理完手上那一条就退出，等它们退出
func (c *consumer) kill(partitions []int32) {
	c.mu.Lock()
	var ws []*worker
	for _, p := range partitions {
		if w := c.assigned[p]; w != nil {
			delete(c.assigned, p)
			ws = append(ws, w)
		}
	}
	c.mu.Unlock()
	for _, w := range ws {
		w.quit()
	}
	for _, w := range ws {
		<-w.done
	}
}
