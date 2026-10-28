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

// Consume registers a consumer: it consumes topic as a member of the consumer group group, calling fn once per message.
//
//	err := xkafka.Consume("orders", "billing", func(ctx context.Context, r *kgo.Record) error {
//		return charge(ctx, r.Value)
//	}, xkafka.WithTimeout(10*time.Second))
//
// The semantics (in full in the README's "Semantics"):
//   - one goroutine per assigned partition: messages within a partition are processed one at a time in order, and different partitions run in parallel;
//   - a message's offset is committed only once it is done (succeeded, reached the dead letter, or was skipped) -- at least once:
//     on a crash or a kill -9, unfinished messages are redelivered later and fn has to tolerate duplicates;
//   - when fn returns an error or panics it is retried per WithRetry, and once those are used up it goes to the dead-letter topic (WithDeadLetter);
//   - on shutdown no new message is fetched and the group is only left once the message in flight is processed and committed. The ctx fn receives is not cancelled by the stop signal,
//     only by WithTimeout; it is cancelled only once the stop budget runs out.
//
// group is required: several processes in the same group share the partitions, while different groups each receive every message.
// Where a new group starts is in ConsumerConfig.ResetOffset (latest by default).
//
// It validates on the spot and returns an *xerror.Error (module xkafka): an empty topic or group, a nil fn, an Option that does not hold,
// and the same topic + group pair registered twice on the same cluster are config; a Consume after the shutdown sequence has begun is register.
//
// It may be called at any time: one registered before xone.Run starts consuming together with the StageServer stage, and one registered later (inside some hook,
// or inside Start) starts on the spot, where a wrong cluster name or a client that cannot be built returns an error on the spot too.
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

// ---- every consumer in one process ----

// state is how far the consumers have got.
type state int

const (
	idle    state = iota // not up yet: Consume only registers
	running              // up: Consume starts on the spot
	stopped              // stopping has begun (or has finished): Consume is an error
)

// manager holds every consumer in one process, started and stopped by xkafka's StageServer pair of hooks.
// lookup fetches a cluster by name: the registry normally, replaced by its own in tests.
type manager struct {
	lookup func(name string) (instance, bool)

	mu        sync.Mutex
	state     state
	consumers []*consumer
}

var std = &manager{lookup: func(name string) (instance, bool) { return reg.Lookup(name) }}

func startConsumers(ctx context.Context) error { return std.start(ctx) }

func stopConsumers(ctx context.Context) error { return std.stop(ctx) }

// add registers a consumer, starting on the spot when they are already up.
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

// start builds every consumer's client first and only begins consuming once they have all succeeded:
// when one cannot come up, not a single message has been fetched yet and closing the clients already built is the whole cleanup (this stage's stop hook is never called).
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

// stop stops every consumer in parallel and waits for them all, or for the ctx to be reached.
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

// joinBusy names what has not stopped by the time the stop budget runs out: the partitions still processing a message (topic/partition@offset), or the consumers stuck committing or leaving the group.
// The framework watches the same deadline and may return first and drop this error, so this one log line naming them has to go out no matter what.
func joinBusy(ctx context.Context, busy [][]string) error {
	all := slices.Concat(busy...)
	if len(all) == 0 {
		return nil
	}
	slog.WarnContext(ctx, "xkafka consumers still stopping when the stop budget ran out", "busy", all)
	return xerror.Newf("xkafka", "stop", "consumers still stopping when the stop budget ran out: %s: %w",
		strings.Join(all, ", "), ctx.Err())
}

// ---- one consumer ----

// consumer is one consumer registered by a Consume, with a *kgo.Client of its own (carrying the consumer group).
type consumer struct {
	topic, group string
	fn           func(context.Context, *kgo.Record) error
	o            options

	// The fields below are filled in by prepare
	cl     *kgo.Client
	log    bool
	trace  bool
	metric bool

	// base is the parent of every message's ctx: it is not cancelled by the stop signal (a message in flight has to finish).
	// hard is cancelled only once the stop budget runs out, to stop a handler that has not returned.
	base       context.Context
	hard       context.Context
	hardCancel context.CancelFunc

	pollCancel context.CancelFunc
	pollDone   chan struct{}

	mu       sync.Mutex
	assigned map[int32]*worker  // the partitions currently assigned
	live     map[*worker]string // the partition goroutines that have not exited (including ones just revoked and waiting for a message in flight), with topic/partition as the value

	// lastDropWarn is when a partition with no goroutine of its own was last warned about, see dropped
	lastDropWarn map[int32]time.Time
}

func (c *consumer) String() string { return c.topic + "/" + c.group }

// same means the same topic + group pair on the same cluster: registering twice only adds another group member in the same process, and the partitions are shared the same way.
func (c *consumer) same(o *consumer) bool {
	return c.topic == o.topic && c.group == o.group && c.o.client == o.o.client
}

// maxPollRecords is how many messages are fetched at most to dispatch across the partitions. Fetching more makes a rebalance wait longer for that batch to be dispatched.
const maxPollRecords = 500

// workerQueue is how many undispatched batches may queue in front of each partition's goroutine. Once full, dispatching stops and waits for it --
// one slow partition makes the dispatching of the others wait too, which is what franz-go's example (goroutine_per_partition_consuming) does:
// without it, the slow partition would keep accumulating in memory.
const workerQueue = 4

// prepare builds this consumer's own client without starting to consume yet.
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
		// Only what has been marked is committed: a message is marked once it is done. franz-go's default automatic commit commits "whatever the last poll returned",
		// processed or not -- and here polling and processing are in different goroutines, so a fetched message may still be queued on a partition
		// and would be lost on a crash (measured on v1.21.7, see TestFranzDefault_AutocommitCommitsPolledButUnprocessed).
		kgo.AutoCommitMarks(),
		// No rebalance happens between the poll and AllowRebalance: dispatching and revoking partitions never overlap, so the partition goroutine table does not have to race the poll.
		// This is what franz-go's example does (autocommit_marks).
		kgo.BlockRebalanceOnPoll(),
		kgo.OnPartitionsAssigned(c.assignedFn),
		kgo.OnPartitionsRevoked(c.revokedFn),
		kgo.OnPartitionsLost(c.lostFn),
	)
	// Zero keeps franz-go's own default rather than passing a zero on, which franz-go would refuse
	// (it checks the session timeout against its heartbeat interval). The reasoning is under
	// ConsumerConfig.SessionTimeout
	if d := inst.cfg.Consumer.SessionTimeout; d != 0 {
		opts = append(opts, kgo.SessionTimeout(d))
	}
	// These have to be filled in before NewClient: it starts joining the group as soon as it is built, and the partition assignment callback (assignedFn) may arrive before it returns.
	c.log, c.trace, c.metric = inst.cfg.Log, inst.cfg.Trace, inst.cfg.Metric
	c.base = context.WithoutCancel(ctx)
	c.hard, c.hardCancel = context.WithCancel(c.base)
	c.assigned, c.live = map[int32]*worker{}, map[*worker]string{}
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return xerror.Newf("xkafka", "new", "consume %q: create client: %w", c.topic, err)
	}
	// Messages are only dispatched to the partition goroutines after launch has started the poll, by which time this has long finished writing c.cl.
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

// warnIfMissing records a WARN when the topic does not exist, without failing startup.
//
// franz-go v1.21.7 waits for a missing topic without a word: its logs hold only DEBUG / INFO, poll returns nothing,
// and it picks up within seconds of the topic being created (the metadata refresh has a 5s minimum; measured 3.8s on kfake, and Kafka 3.9.1 in the README).
// Startup is not failed: a topic is often created a step later by the producer or by operations, and the consumer going live first is a normal deployment order.
// But "no messages ever arrive" needs a clue, so one line is recorded. If it cannot be checked (a broker briefly unreachable) nothing is said, and the connection error says it instead.
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

// launch starts consuming: one goroutine polls and dispatches by partition.
func (c *consumer) launch() {
	var ctx context.Context
	ctx, c.pollCancel = context.WithCancel(c.base)
	c.pollDone = make(chan struct{})
	go c.poll(ctx)
}

// poll fetches messages and dispatches them to each partition's goroutine, until stop cancels the ctx.
func (c *consumer) poll(ctx context.Context) {
	defer close(c.pollDone)
	for {
		fs := c.cl.PollRecords(ctx, maxPollRecords)
		if ctx.Err() != nil || fs.IsClientClosed() {
			// This batch is not dispatched: nothing processed is ever marked, so whichever member gets these partitions next (or this process after a restart) starts over
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
				// Not a revoked partition: that waits for the AllowRebalance below under BlockRebalanceOnPoll.
				// What reaches here is a goroutine that exited on its own and took itself out of the table
				// (see retire), so this batch has nowhere to go. Dropping it loses nothing -- none of it was
				// marked, and it comes back after the next rebalance
				c.dropped(ctx, p.Partition, p.Records)
				return
			}
			select {
			case w.recs <- p.Records:
			case <-ctx.Done():
			}
		})
		c.cl.AllowRebalance()
	}
}

// stop stops this consumer: no new message is fetched; leaving the group makes franz-go revoke every partition,
// revokedFn waits for the messages in flight to be processed and commits what was marked; then the client is closed.
// Whatever has not stopped by the time the ctx is reached has the ctx of its handler in flight cancelled, and the partitions still processing are returned.
func (c *consumer) stop(ctx context.Context) []string {
	c.pollCancel()

	done := make(chan struct{})
	go func() {
		// PollRecords may be waiting for a rebalance whose revoke callback is draining a handler.
		// Keep this wait inside the stop budget too, so its deadline can cancel that handler.
		<-c.pollDone
		// Not Close: with the poll stopped between PollRecords and AllowRebalance, Close would wait forever for that blocked rebalance
		c.cl.CloseAllowingRebalance()
		close(done)
	}()
	defer c.hardCancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
	}
	// Named before being stopped: once stopped they return one by one and naming them afterwards would miss some.
	// One with no message in flight that has still not stopped is stuck committing or leaving the group (the coordinator is unreachable), so the consumer itself is named.
	busy := c.busy()
	if len(busy) == 0 {
		busy = []string{c.String() + " (committing offsets / leaving the group)"}
	}
	c.hardCancel()
	return busy
}

// busy lists the partitions still processing a message, as topic/partition@offset.
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

// ---- rebalancing ----

func (c *consumer) assignedFn(_ context.Context, _ *kgo.Client, assigned map[string][]int32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range assigned[c.topic] {
		w := newWorker(c, p)
		delete(c.lastDropWarn, p) // this partition has a goroutine again: the next loss says so at once
		c.assigned[p] = w
		c.live[w] = partitionStr(c.topic, p)
		go w.run()
	}
}

// revokedFn runs when partitions are revoked (a rebalance, leaving the group): it waits for those partitions' messages in flight to be processed and commits what was marked,
// and only then does franz-go let the rebalance continue. The new member therefore starts from the next message and does not reprocess what was already processed.
func (c *consumer) revokedFn(ctx context.Context, cl *kgo.Client, revoked map[string][]int32) {
	c.kill(revoked[c.topic])
	if err := cl.CommitMarkedOffsets(ctx); err != nil {
		slog.WarnContext(ctx, "kafka commit on partition revoke failed, processed messages may be redelivered",
			"topic", c.topic, "group", c.group, "error", err)
	}
}

// lostFn runs when partitions are lost (a session timeout and the like): they are no longer this member's and a commit would be rejected by the broker. It only waits for the messages in flight.
func (c *consumer) lostFn(_ context.Context, _ *kgo.Client, lost map[string][]int32) {
	c.kill(lost[c.topic])
}

// dropWarnInterval is the minimum interval between warnings about one partition whose goroutine has gone.
const dropWarnInterval = time.Minute

// dropped records that this partition's records are being thrown away, at most once per
// dropWarnInterval per partition.
//
// It says so again and again on purpose. The goroutine exiting is one ERROR at the moment it
// happens, and what follows it has no sign of its own: poll goes on fetching this partition and
// dropping every batch, nothing is marked so the committed offset stays where it was, and nothing
// here is counted -- the service keeps its heartbeat, the other partitions carry on, and the only
// trace from outside is a consumer lag nobody may be watching. The state also mends itself only on
// the next rebalance, which may be days away, so the one line at the start is the wrong place to
// say it: it has to be sayable at any later moment too.
//
// Repeating it per batch would be a flood of its own (every poll, for as long as it lasts), hence
// the interval. One per partition: two partitions in this state are two different things to fix.
func (c *consumer) dropped(ctx context.Context, partition int32, recs []*kgo.Record) {
	c.mu.Lock()
	if c.lastDropWarn == nil {
		c.lastDropWarn = map[int32]time.Time{}
	}
	now := time.Now()
	if last, ok := c.lastDropWarn[partition]; ok && now.Sub(last) < dropWarnInterval {
		c.mu.Unlock()
		return
	}
	c.lastDropWarn[partition] = now
	c.mu.Unlock()

	attrs := []any{"topic", c.topic, "partition", partition, "group", c.group, "records", len(recs)}
	if len(recs) > 0 {
		attrs = append(attrs, "from_offset", recs[0].Offset)
	}
	slog.WarnContext(ctx, "kafka records dropped, this partition has no goroutine of its own until the next rebalance",
		attrs...)
}

// retire takes a goroutine that has exited out of both tables, and is the last thing run does.
//
// Clearing the partition table is what keeps one partition's exit from stopping the whole consumer: poll
// dispatches by that table, and a goroutine nobody is reading any more would take workerQueue batches and
// then leave poll blocked on the channel for good -- no AllowRebalance, no further PollRecords, so every
// partition of this consumer stops with it. Out of the table, poll finds nothing for that partition and
// drops the batch instead; nothing was marked, so those records come back after the next rebalance.
//
// kill took a revoked partition out already, and a rebalance may have put a new goroutine there since, so
// only this very one is removed.
func (c *consumer) retire(w *worker) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.live, w)
	if c.assigned[w.partition] == w {
		delete(c.assigned, w.partition)
	}
}

// kill makes these partitions' goroutines finish the message in hand and exit, and waits for them to exit.
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
