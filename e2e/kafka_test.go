package e2e

// The Kafka end-to-end tests: the TestKafka_* family, with the e2e service reaching the Kafka
// 3.9.1 in Docker (one KRaft node) through xkafka.
//
//	scripts/e2e.sh -run Kafka
//
// XKafka is optional: it exists only once harness.Options.Kafka activates the
// service/application-kafka.yml profile; when Kafka is unavailable this group skips case by case
// (harness.RequireKafka) while the rest of e2e runs. Every case creates a topic of its own and
// uses a consumer group of its own.
//
// The assertions follow xkafka/README.md, "The semantics", "Observability" and "Behavior
// (measured)"; a failure message reads "the documentation says X, got Y".
// What happens to a message is decided by the JSON in its value, see service/kafka.go.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// kafkaEarliest: a topic just created and a new consumer group consume from the start, so a message
// written does not have to wait for the partitions to be assigned.
// The default, latest, is pinned by xkafka's unit tests
const kafkaEarliest = "XKafka:\n  Consumer:\n    ResetOffset: earliest\n"

// kafkaSessionTimeout is the SessionTimeout default of franz-go v1.21.7: how long the partitions of a member that was kill -9ed wait before they go to someone else
const kafkaSessionTimeout = 45 * time.Second

// kafkaMsg is one message's value, whose fields are in service/kafka.go
func kafkaMsg(t *testing.T, fields map[string]any) string {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// handled are the e2e kafka handle log lines for id in the process p
func handled(p *harness.Process, id string) []harness.Log {
	return p.FindLogs(func(l harness.Log) bool { return l.Msg() == "e2e kafka handle" && l.Str("id") == id })
}

// done waits for p to finish handling id
func done(t *testing.T, p *harness.Process, id string, timeout time.Duration) harness.Log {
	t.Helper()
	return p.WaitLog(t, timeout, func(l harness.Log) bool { return l.Msg() == "e2e kafka done" && l.Str("id") == id })
}

// TestKafka_ProduceConsume_TraceContinues
//
// The documentation (xkafka/README.md, "Tracing" and "The logs"): producing writes the trace
// context into the message headers and consuming picks it up again -- a log line inside the
// handler carries the same trace_id as the HTTP request; <topic> process hangs under <topic>
// publish; the handler's log lines carry topic / partition / offset / group; and neither the spans
// nor the logs hold the value
func TestKafka_ProduceConsume_TraceContinues(t *testing.T) {
	harness.RequireKafka(t)
	t.Parallel()
	topic := harness.KafkaTopic(t, 1)
	p := harness.Start(t, harness.Options{Kafka: true, KafkaTopic: topic, Spans: true, Overlay: kafkaEarliest})

	value := kafkaMsg(t, map[string]any{"id": "trace-1", "secret": "s3cret-value"})
	r := p.PostJSON(t, "/kafka/produce", map[string]any{"topic": topic, "key": "user-42", "value": value})
	if r.Status != http.StatusOK {
		t.Fatalf("POST /kafka/produce: %v", r)
	}
	access := p.WaitLog(t, 5*time.Second, func(l harness.Log) bool { return l.Str("path") == "/kafka/produce" && l.Str("trace_id") != "" })
	d := done(t, p, "trace-1", 20*time.Second)
	if d.Str("trace_id") != access.Str("trace_id") {
		t.Errorf("the documentation says the consuming side continues the producer's trace: the request has trace_id=%s and the handler trace_id=%s", access.Str("trace_id"), d.Str("trace_id"))
	}
	if d.Str("topic") != topic || d.Str("group") != p.KafkaGroup || d.Str("offset") != "0" || d.Str("partition") != "0" {
		t.Errorf("the documentation says the handler's log lines carry topic / partition / offset / group: %s", d.Line)
	}

	proc := p.WaitSpan(t, 10*time.Second, func(s harness.Span) bool { return s.Name == topic+" process" })
	pub := p.WaitSpan(t, 10*time.Second, func(s harness.Span) bool { return s.Name == topic+" publish" })
	if proc.TraceID != access.Str("trace_id") || proc.ParentSpanID != pub.SpanID || pub.TraceID != proc.TraceID {
		t.Errorf("the documentation says process hangs under publish on the same trace: publish=%+v process=%+v", pub, proc)
	}
	exit := p.Terminate(t, 20*time.Second)
	if exit.Code != 0 {
		t.Fatalf("it should exit with 0 after the SIGTERM: %v", exit)
	}
	if strings.Contains(p.Output(), "s3cret-value") {
		t.Error("the documentation says the value is never recorded: the output holds the message's value")
	}
	for _, s := range p.Spans(t) {
		if strings.Contains(fmt.Sprint(s), "s3cret-value") || strings.Contains(fmt.Sprint(s), "user-42") {
			t.Errorf("the documentation says the spans hold neither the key nor the value: %+v", s)
		}
	}
}

// TestKafka_FailureRetriesThenDeadLetter_PermanentSkipsRetries
//
// The documentation (xkafka/README.md, "Retries" and "Dead letters"): a failing handler is retried
// as WithRetry says, and once the retries are used up the message goes into <topic>.dlq carrying
// the original key / value / headers and the xkafka-dlq-* headers; xutil.Permanent is not retried;
// and the offset is committed afterwards
func TestKafka_FailureRetriesThenDeadLetter_PermanentSkipsRetries(t *testing.T) {
	harness.RequireKafka(t)
	t.Parallel()
	topic := harness.KafkaTopic(t, 1)
	harness.CreateKafkaTopic(t, topic+".dlq", 1)
	p := harness.Start(t, harness.Options{Kafka: true, KafkaTopic: topic,
		Overlay: kafkaEarliest + "Service:\n  KafkaRetry: 2\n"})

	cl := harness.KafkaClient(t)
	harness.KafkaProduce(t, cl, topic, 0, "k-fail", kafkaMsg(t, map[string]any{"id": "always", "fail": -1}))
	harness.KafkaProduce(t, cl, topic, 0, "k-perm", kafkaMsg(t, map[string]any{"id": "perm", "permanent": true}))

	dlq := harness.KafkaRead(t, topic+".dlq", 2, 30*time.Second)
	if len(dlq) != 2 {
		t.Fatalf("the documentation says a message goes to the dead letters once the retries are used up: %s.dlq holds only %d\n%s", topic, len(dlq), lastLines(p.Output(), 30))
	}
	first := dlq[0]
	for k, want := range map[string]string{
		"xkafka-dlq-topic": topic, "xkafka-dlq-partition": "0", "xkafka-dlq-offset": "0",
		"xkafka-dlq-group": p.KafkaGroup, "xkafka-dlq-error": "e2e failure 3",
	} {
		if got := harness.KafkaHeader(first, k); got != want {
			t.Errorf("the dead letter header %s: the documentation says %q, got %q", k, want, got)
		}
	}
	if string(first.Key) != "k-fail" || !strings.Contains(string(first.Value), `"always"`) || harness.KafkaHeader(first, "traceparent") == "" {
		t.Errorf("a dead letter carries the original key and value, and its trace headers continue the consuming span: key=%s value=%s headers=%v", first.Key, first.Value, first.Headers)
	}
	if n := len(handled(p, "always")); n != 3 {
		t.Errorf("WithRetry(2) means 3 calls at most, got %d", n)
	}
	if n := len(handled(p, "perm")); n != 1 {
		t.Errorf("xutil.Permanent is not retried, and it was called %d times", n)
	}
	if got := harness.KafkaHeader(dlq[1], "xkafka-dlq-error"); got != "e2e permanent failure" {
		t.Errorf("the error header of the permanent one: %q", got)
	}
	// The log line is recorded after the dead letter has been written: the moment the dead letter is read, the second line may not be out yet
	p.WaitLog(t, 5*time.Second, func(l harness.Log) bool {
		return l.Msg() == "kafka message dead-lettered" && l.Str("offset") == "1"
	})
	w := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "kafka message dead-lettered" })
	if len(w) != 2 || w[0].Level() != "WARN" || w[0].Str("dlq_topic") != topic+".dlq" {
		t.Errorf("the documentation says a dead letter records one WARN kafka message dead-lettered: %v", w)
	}

	if exit := p.Terminate(t, 20*time.Second); exit.Code != 0 {
		t.Fatalf("it should exit with 0 after the SIGTERM: %v", exit)
	}
	if got := harness.KafkaCommitted(t, p.KafkaGroup, topic, 0); got != 2 {
		t.Errorf("a message that went to the dead letters is committed as well: committed=%d, want 2", got)
	}
}

// TestKafka_SIGTERMWhileInFlight_FinishesCommitsNotRedelivered
//
// The documentation (xkafka/README.md, "The exit"): on exit it fetches no new message, finishes
// the one in flight (whose ctx is not cancelled) and leaves the consumer group only after
// committing; after a restart it does not receive it again
func TestKafka_SIGTERMWhileInFlight_FinishesCommitsNotRedelivered(t *testing.T) {
	harness.RequireKafka(t)
	t.Parallel()
	topic := harness.KafkaTopic(t, 1)
	group := "e2e_" + harness.NewID()
	o := harness.Options{Kafka: true, KafkaTopic: topic, KafkaGroup: group, Overlay: kafkaEarliest}
	p := harness.Start(t, o)

	cl := harness.KafkaClient(t)
	harness.KafkaProduce(t, cl, topic, 0, "", kafkaMsg(t, map[string]any{"id": "inflight", "hold_ms": 3000}))
	harness.KafkaProduce(t, cl, topic, 0, "", kafkaMsg(t, map[string]any{"id": "queued"}))
	p.WaitLog(t, 20*time.Second, func(l harness.Log) bool { return l.Msg() == "e2e kafka handle" && l.Str("id") == "inflight" })

	exit := p.Terminate(t, 30*time.Second)
	if exit.Code != 0 {
		t.Fatalf("it should exit with 0 after the SIGTERM: %v\n%s", exit, lastLines(p.Output(), 30))
	}
	d := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "e2e kafka done" && l.Str("id") == "inflight" })
	if len(d) != 1 || d[0].Str("ctx_err") != "" {
		t.Fatalf("the documentation says the message in flight is finished and its ctx is not cancelled by the exit: %v", d)
	}
	if exit.SinceSignal < 2*time.Second {
		t.Errorf("the message in flight sleeps 3s and it exited %v after the SIGTERM: it did not wait", exit.SinceSignal)
	}
	if n := len(handled(p, "queued")); n != 0 {
		t.Errorf("the documentation says no new message is fetched on exit: the one queued behind it was handled")
	}
	if got := harness.KafkaCommitted(t, group, topic, 0); got != 1 {
		t.Fatalf("it commits once the message in flight is finished: committed=%d, want 1", got)
	}

	p2 := harness.Start(t, o)
	done(t, p2, "queued", 30*time.Second)
	if n := len(handled(p2, "inflight")); n != 0 {
		t.Errorf("a message finished and committed before the exit was received %d times after the restart", n)
	}
	p2.Terminate(t, 20*time.Second)
}

// TestKafka_SIGKILLMidProcessing_Redelivered
//
// The documentation (xkafka/README.md, "Committing"): at least once -- it marks and commits only
// once the message is handled; with a kill -9 the one being handled is not committed and is
// redelivered after a restart.
// A member that was killed does not leave the consumer group of its own accord: the new process
// waits out its session timeout (45s by default in franz-go) before it is assigned partitions
func TestKafka_SIGKILLMidProcessing_Redelivered(t *testing.T) {
	requireSlow(t, "franz-go's session timeout, "+kafkaSessionTimeout.String())
	harness.RequireKafka(t)
	t.Parallel()
	topic := harness.KafkaTopic(t, 1)
	group := "e2e_" + harness.NewID()
	o := harness.Options{Kafka: true, KafkaTopic: topic, KafkaGroup: group, Overlay: kafkaEarliest}
	p := harness.Start(t, o)

	cl := harness.KafkaClient(t)
	harness.KafkaProduce(t, cl, topic, 0, "", kafkaMsg(t, map[string]any{"id": "before"}))
	harness.KafkaProduce(t, cl, topic, 0, "", kafkaMsg(t, map[string]any{"id": "killed", "hold_ms": 60000}))
	done(t, p, "before", 20*time.Second)
	p.WaitLog(t, 20*time.Second, func(l harness.Log) bool { return l.Msg() == "e2e kafka handle" && l.Str("id") == "killed" })
	p.Kill()
	p.Wait(10 * time.Second)

	start := time.Now()
	p2 := harness.Start(t, o)
	// Redelivered, it sleeps its 60s again: what is looked for is "it started handling it again"
	d := p2.WaitLog(t, kafkaSessionTimeout+30*time.Second, func(l harness.Log) bool { return l.Msg() == "e2e kafka handle" && l.Str("id") == "killed" })
	t.Logf("numbers: after the kill -9 the restarted process received the redelivered message %v in (the session timeout being %v)", time.Since(start), kafkaSessionTimeout)
	if d.Str("offset") != "1" {
		t.Errorf("what is redelivered is the one being handled at the kill (offset 1): %s", d.Line)
	}
	// before was finished before the kill: whether it is redelivered depends on whether the automatic commit (every 5s) got there, which is not asserted
	t.Logf("numbers: the one finished before the kill was redelivered %d times after the restart", len(handled(p2, "before")))
	p2.Kill() // the one in flight sleeps 60s and is not waited for
}

// TestKafka_TwoProcessesSplitPartitions_HandOverWithoutLoss
//
// The documentation (xkafka/README.md, "Rebalancing"): two processes in one consumer group share
// the partitions; when one exits its partitions go to the other, and it commits before handing
// them over -- nothing is lost and nothing is handled twice
func TestKafka_TwoProcessesSplitPartitions_HandOverWithoutLoss(t *testing.T) {
	harness.RequireKafka(t)
	t.Parallel()
	topic := harness.KafkaTopic(t, 4)
	group := "e2e_" + harness.NewID()
	o := harness.Options{Kafka: true, KafkaTopic: topic, KafkaGroup: group, Overlay: kafkaEarliest}
	p1 := harness.Start(t, o)
	p2 := harness.Start(t, o)

	cl := harness.KafkaClient(t)
	var ids []string
	send := func(round string) {
		for part := range int32(4) {
			for i := range 3 {
				id := fmt.Sprintf("%s-p%d-%d", round, part, i)
				ids = append(ids, id)
				harness.KafkaProduce(t, cl, topic, part, "", kafkaMsg(t, map[string]any{"id": id, "hold_ms": 20}))
			}
		}
	}
	// Wait until both processes have partitions: each of them handles a message
	deadline := time.Now().Add(30 * time.Second)
	for round := 0; ; round++ {
		send(fmt.Sprintf("warm%d", round))
		time.Sleep(time.Second)
		if len(p1.FindLogs(isDone)) > 0 && len(p2.FindLogs(isDone)) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the documentation says two processes in one group share the partitions: in 30s p1 handled %d and p2 handled %d", len(p1.FindLogs(isDone)), len(p2.FindLogs(isDone)))
		}
	}

	if exit := p1.Terminate(t, 30*time.Second); exit.Code != 0 {
		t.Fatalf("p1 should exit with 0 after the SIGTERM: %v", exit)
	}
	send("after")
	// p2 learns that p1 is gone at its next heartbeat (every 3s) and takes p1's partitions over only after another two-phase rebalance
	count := func() map[string]int {
		n := map[string]int{}
		for _, p := range []*harness.Process{p1, p2} {
			for _, l := range p.FindLogs(isDone) {
				n[l.Str("id")]++
			}
		}
		return n
	}
	deadline = time.Now().Add(30 * time.Second)
	for slices.ContainsFunc(ids, func(id string) bool { return count()[id] == 0 }) && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(time.Second) // give a duplicate time to turn up, if there is one
	final := count()
	var missing, dup []string
	for _, id := range ids {
		switch final[id] {
		case 0:
			missing = append(missing, id)
		case 1:
		default:
			dup = append(dup, id)
		}
	}
	if len(missing) > 0 {
		t.Errorf("messages were lost in the hand-over: %v", missing)
	}
	if len(dup) > 0 {
		t.Errorf("the documentation says it commits before handing the partitions over, so nothing repeats: %v was handled more than once", dup)
	}
	p2.Terminate(t, 20*time.Second)
}

func isDone(l harness.Log) bool { return l.Msg() == "e2e kafka done" }

// TestKafka_BrokerUnreachableAtStartup_FailsFastNamingAddressNoSecret
//
// The documentation (xkafka/README.md, "Behavior (measured)"): at the start it probes the
// connection 3 times, each 2 x DialTimeout (2s by default), with the backoff bounds 1s and 2s;
// when it cannot connect the start fails, with the address in the error and no password. Once with
// a blackhole (no answer to the SYN) and once with connections refused
func TestKafka_BrokerUnreachableAtStartup_FailsFastNamingAddressNoSecret(t *testing.T) {
	harness.RequireKafka(t)
	t.Parallel()
	const secret = "e2e-kafka-pw-s3cret"
	overlay := "XKafka:\n  SASL:\n    Mechanism: PLAIN\n    Username: app\n    Password: " + secret + "\n"
	budget := faultPingAttempts*2*time.Second + faultPingBackoffs // 9s

	hole := harness.NewProxy(t, harness.KafkaAddr())
	hole.Blackhole()
	refused := "127.0.0.1:" + fmt.Sprint(harness.FreePort(t))

	for name, addr := range map[string]string{"blackhole": hole.Addr(), "refused": refused} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := harness.Start(t, harness.Options{KafkaAddr: addr, Overlay: overlay, NoWait: true})
			exit, ok := p.Wait(budget + 10*time.Second)
			if !ok {
				t.Fatalf("the documentation says the start fails when it cannot connect: it had not exited after %v", budget+10*time.Second)
			}
			t.Logf("numbers: %s took %v to fail the start (budget %v)", name, exit.Uptime, budget)
			if exit.Code != 1 {
				t.Errorf("a failed start should exit with 1: %v", exit)
			}
			if !strings.Contains(p.Stderr(), "xkafka connect failed") || !strings.Contains(p.Stderr(), "cannot reach "+addr) {
				t.Errorf("the documentation says the error names the address:\n%s", lastLines(p.Stderr(), 10))
			}
			if strings.Contains(p.Output(), secret) {
				t.Error("the password turned up in the output")
			}
		})
	}
}

// TestKafka_ConsumerOnlyProcess
//
// The documentation (xkafka/README.md, "Quick start"): a process that only consumes is
// xone.MustRun(xone.UntilSignal()), its consumer comes up in StageServer, and on SIGTERM it stops,
// commits and exits with 0
func TestKafka_ConsumerOnlyProcess(t *testing.T) {
	harness.RequireKafka(t)
	t.Parallel()
	topic := harness.KafkaTopic(t, 2)
	p := harness.Start(t, harness.Options{Kafka: true, KafkaTopic: topic, NoWait: true,
		Overlay: kafkaEarliest + "Service:\n  NoHTTP: true\n"})
	p.WaitLog(t, 30*time.Second, func(l harness.Log) bool { return l.Msg() == "xkafka consumers started" })

	cl := harness.KafkaClient(t)
	harness.KafkaProduce(t, cl, topic, 0, "", kafkaMsg(t, map[string]any{"id": "c0"}))
	harness.KafkaProduce(t, cl, topic, 1, "", kafkaMsg(t, map[string]any{"id": "c1"}))
	done(t, p, "c0", 30*time.Second)
	done(t, p, "c1", 30*time.Second)

	exit := p.Terminate(t, 20*time.Second)
	if exit.Code != 0 {
		t.Fatalf("it should exit with 0 after the SIGTERM: %v\n%s", exit, lastLines(p.Output(), 30))
	}
	var stops []string
	for _, l := range p.Logs() {
		if l.Msg() == "stopping" {
			stops = append(stops, l.Str("hook"))
		}
	}
	// The consumer stops first (StageServer) and the producing client closes after it (StageClient)
	i, j := slices.Index(stops, "xkafka.stopConsumers"), slices.Index(stops, "xkafka.closeXKafka")
	if i < 0 || j < 0 || i > j {
		t.Errorf("the documentation says the consumer stops first and the client closes after it: %v", stops)
	}
	for part := range int32(2) {
		if got := harness.KafkaCommitted(t, p.KafkaGroup, topic, part); got != 1 {
			t.Errorf("partition %d is committed before the exit: committed=%d", part, got)
		}
	}
}
