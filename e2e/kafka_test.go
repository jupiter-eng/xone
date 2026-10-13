package e2e

// Kafka 端到端测试：TestKafka_* 系列，e2e 服务经 xkafka 连 Docker 里的 Kafka 3.9.1（KRaft 单节点）。
//
//	scripts/e2e.sh -run Kafka
//
// XKafka 是可选的：harness.Options.Kafka 激活 service/application-kafka.yml 那份 profile 才有；
// Kafka 不可用时这一组各自跳过（harness.RequireKafka），其余 e2e 照跑。每个用例自己建 topic、用自己的消费组。
//
// 断言对照 xkafka/README.md「语义」「可观测」「行为与实测」；失败信息写成「文档说 X，实际 Y」。
// 消息怎么处理由 value 里的 JSON 决定，见 service/kafka.go。

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

// kafkaEarliest 新建的 topic、新的消费组：从头消费，写进去的消息不用等「分到分区」之后才写。
// 默认的 latest 由 xkafka 的单元测试钉着
const kafkaEarliest = "XKafka:\n  Consumer:\n    ResetOffset: earliest\n"

// kafkaSessionTimeout franz-go v1.21.7 的 SessionTimeout 默认值：被 kill -9 的组员的分区要等这么久才交给别人
const kafkaSessionTimeout = 45 * time.Second

// kafkaMsg 一条消息的 value，字段见 service/kafka.go
func kafkaMsg(t *testing.T, fields map[string]any) string {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// handled 进程 p 里处理过 id 的 e2e kafka handle 日志
func handled(p *harness.Process, id string) []harness.Log {
	return p.FindLogs(func(l harness.Log) bool { return l.Msg() == "e2e kafka handle" && l.Str("id") == id })
}

// done 等 p 处理完 id
func done(t *testing.T, p *harness.Process, id string, timeout time.Duration) harness.Log {
	t.Helper()
	return p.WaitLog(t, timeout, func(l harness.Log) bool { return l.Msg() == "e2e kafka done" && l.Str("id") == id })
}

// TestKafka_ProduceConsume_TraceContinues
//
// 文档（xkafka/README.md「链路」「日志」）：生产时把链路上下文写进消息头，消费时从消息头接上——
// 处理函数里的日志和 HTTP 请求是同一个 trace_id；<topic> process 挂在 <topic> publish 下面；
// 处理函数的日志带 topic / partition / offset / group；Span 和日志里都没有 value
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
		t.Errorf("文档说消费那一侧接着生产方的链路：请求 trace_id=%s，处理函数里 trace_id=%s", access.Str("trace_id"), d.Str("trace_id"))
	}
	if d.Str("topic") != topic || d.Str("group") != p.KafkaGroup || d.Str("offset") != "0" || d.Str("partition") != "0" {
		t.Errorf("文档说处理函数的日志带 topic / partition / offset / group：%s", d.Line)
	}

	proc := p.WaitSpan(t, 10*time.Second, func(s harness.Span) bool { return s.Name == topic+" process" })
	pub := p.WaitSpan(t, 10*time.Second, func(s harness.Span) bool { return s.Name == topic+" publish" })
	if proc.TraceID != access.Str("trace_id") || proc.ParentSpanID != pub.SpanID || pub.TraceID != proc.TraceID {
		t.Errorf("文档说 process 挂在 publish 下面、同一个 trace：publish=%+v process=%+v", pub, proc)
	}
	exit := p.Terminate(t, 20*time.Second)
	if exit.Code != 0 {
		t.Fatalf("SIGTERM 之后应以 0 退出：%v", exit)
	}
	if strings.Contains(p.Output(), "s3cret-value") {
		t.Error("文档说从不记 value：输出里有消息的 value")
	}
	for _, s := range p.Spans(t) {
		if strings.Contains(fmt.Sprint(s), "s3cret-value") || strings.Contains(fmt.Sprint(s), "user-42") {
			t.Errorf("文档说 Span 里没有 key 和 value：%+v", s)
		}
	}
}

// TestKafka_FailureRetriesThenDeadLetter_PermanentSkipsRetries
//
// 文档（xkafka/README.md「重试」「死信」）：处理函数失败按 WithRetry 重试，用完了写进 <topic>.dlq，
// 带着原消息的 key / value / 头和 xkafka-dlq-* 几个头；xutil.Permanent 不重试；之后提交 offset
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
		t.Fatalf("文档说重试用完写进死信：%s.dlq 里只有 %d 条\n%s", topic, len(dlq), lastLines(p.Output(), 30))
	}
	first := dlq[0]
	for k, want := range map[string]string{
		"xkafka-dlq-topic": topic, "xkafka-dlq-partition": "0", "xkafka-dlq-offset": "0",
		"xkafka-dlq-group": p.KafkaGroup, "xkafka-dlq-error": "e2e failure 3",
	} {
		if got := harness.KafkaHeader(first, k); got != want {
			t.Errorf("死信头 %s：文档说 %q，实际 %q", k, want, got)
		}
	}
	if string(first.Key) != "k-fail" || !strings.Contains(string(first.Value), `"always"`) || harness.KafkaHeader(first, "traceparent") == "" {
		t.Errorf("死信带着原消息的 key、value，链路头接着消费的那个 Span：key=%s value=%s headers=%v", first.Key, first.Value, first.Headers)
	}
	if n := len(handled(p, "always")); n != 3 {
		t.Errorf("WithRetry(2) 是最多调 3 次，实际 %d 次", n)
	}
	if n := len(handled(p, "perm")); n != 1 {
		t.Errorf("xutil.Permanent 不重试，实际调了 %d 次", n)
	}
	if got := harness.KafkaHeader(dlq[1], "xkafka-dlq-error"); got != "e2e permanent failure" {
		t.Errorf("permanent 那条的错误头：%q", got)
	}
	// 日志在死信写成功之后才记：读到死信的那一刻，第二行可能还没写出来
	p.WaitLog(t, 5*time.Second, func(l harness.Log) bool {
		return l.Msg() == "kafka message dead-lettered" && l.Str("offset") == "1"
	})
	w := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "kafka message dead-lettered" })
	if len(w) != 2 || w[0].Level() != "WARN" || w[0].Str("dlq_topic") != topic+".dlq" {
		t.Errorf("文档说进死信记一条 WARN kafka message dead-lettered：%v", w)
	}

	if exit := p.Terminate(t, 20*time.Second); exit.Code != 0 {
		t.Fatalf("SIGTERM 之后应以 0 退出：%v", exit)
	}
	if got := harness.KafkaCommitted(t, p.KafkaGroup, topic, 0); got != 2 {
		t.Errorf("进了死信的消息也提交：committed=%d，应为 2", got)
	}
}

// TestKafka_SIGTERMWhileInFlight_FinishesCommitsNotRedelivered
//
// 文档（xkafka/README.md「退出」）：退出时不再取新消息，在途的那一条处理完（ctx 不被取消）、提交之后才离开消费组；
// 重启之后不会再收到它
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
		t.Fatalf("SIGTERM 之后应以 0 退出：%v\n%s", exit, lastLines(p.Output(), 30))
	}
	d := p.FindLogs(func(l harness.Log) bool { return l.Msg() == "e2e kafka done" && l.Str("id") == "inflight" })
	if len(d) != 1 || d[0].Str("ctx_err") != "" {
		t.Fatalf("文档说在途的消息处理完、它的 ctx 不随退出取消：%v", d)
	}
	if exit.SinceSignal < 2*time.Second {
		t.Errorf("在途的消息要睡 3s，SIGTERM 之后 %v 就退出了：没等它", exit.SinceSignal)
	}
	if n := len(handled(p, "queued")); n != 0 {
		t.Errorf("文档说退出时不再取新消息：排在后面的那条被处理了")
	}
	if got := harness.KafkaCommitted(t, group, topic, 0); got != 1 {
		t.Fatalf("在途的那条处理完之后提交：committed=%d，应为 1", got)
	}

	p2 := harness.Start(t, o)
	done(t, p2, "queued", 30*time.Second)
	if n := len(handled(p2, "inflight")); n != 0 {
		t.Errorf("退出前处理完、提交了的消息，重启之后又收到了 %d 次", n)
	}
	p2.Terminate(t, 20*time.Second)
}

// TestKafka_SIGKILLMidProcessing_Redelivered
//
// 文档（xkafka/README.md「提交」）：至少一次——处理完才标记、提交；kill -9 时在处理的那条没提交，重启之后重新投递。
// 被 kill 的组员不会主动离开消费组：新的进程要等它的会话超时（franz-go 默认 45s）才分到分区
func TestKafka_SIGKILLMidProcessing_Redelivered(t *testing.T) {
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
	// 重新投递时它照样要睡 60s：看的是「又开始处理它了」
	d := p2.WaitLog(t, kafkaSessionTimeout+30*time.Second, func(l harness.Log) bool { return l.Msg() == "e2e kafka handle" && l.Str("id") == "killed" })
	t.Logf("数字：kill -9 之后重启的进程 %v 收到了重新投递的消息（会话超时 %v）", time.Since(start), kafkaSessionTimeout)
	if d.Str("offset") != "1" {
		t.Errorf("重新投递的是被 kill 时在处理的那条（offset 1）：%s", d.Line)
	}
	// before 在 kill 之前处理完：有没有重新投递取决于自动提交（5s 一次）赶没赶上，不卡
	t.Logf("数字：kill 之前处理完的那条，重启之后重新投递了 %d 次", len(handled(p2, "before")))
	p2.Kill() // 在途的那条要睡 60s，不等它
}

// TestKafka_TwoProcessesSplitPartitions_HandOverWithoutLoss
//
// 文档（xkafka/README.md「再均衡」）：同一个消费组的两个进程分摊分区；一个退出时它的分区交给另一个，
// 交出去之前提交——不丢，也不重复
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
	// 等两个进程都分到分区：各自处理到消息为止
	deadline := time.Now().Add(30 * time.Second)
	for round := 0; ; round++ {
		send(fmt.Sprintf("warm%d", round))
		time.Sleep(time.Second)
		if len(p1.FindLogs(isDone)) > 0 && len(p2.FindLogs(isDone)) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("文档说同组的两个进程分摊分区：30s 内 p1 处理了 %d 条、p2 处理了 %d 条", len(p1.FindLogs(isDone)), len(p2.FindLogs(isDone)))
		}
	}

	if exit := p1.Terminate(t, 30*time.Second); exit.Code != 0 {
		t.Fatalf("p1 SIGTERM 之后应以 0 退出：%v", exit)
	}
	send("after")
	// p2 要在下一次心跳（3s 一次）才知道 p1 走了，再走一遍两阶段的再均衡，才接手 p1 的分区
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
	time.Sleep(time.Second) // 有重复的话给它时间冒出来
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
		t.Errorf("交接时丢了消息：%v", missing)
	}
	if len(dup) > 0 {
		t.Errorf("文档说交出分区之前提交，不重复：%v 处理了不止一次", dup)
	}
	p2.Terminate(t, 20*time.Second)
}

func isDone(l harness.Log) bool { return l.Msg() == "e2e kafka done" }

// TestKafka_BrokerUnreachableAtStartup_FailsFastNamingAddressNoSecret
//
// 文档（xkafka/README.md「行为与实测」）：启动时探测连通性，3 次、每次 2 × DialTimeout（默认 2s）、
// 退避上界 1s、2s；连不上启动失败，错误里有地址、没有密码。黑洞（SYN 没有回音）和拒绝连接各一遍
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
				t.Fatalf("文档说连不上启动失败：%v 之后还没退出", budget+10*time.Second)
			}
			t.Logf("数字：%s 启动失败用了 %v（预算 %v）", name, exit.Uptime, budget)
			if exit.Code != 1 {
				t.Errorf("启动失败应以 1 退出：%v", exit)
			}
			if !strings.Contains(p.Stderr(), "xkafka connect failed") || !strings.Contains(p.Stderr(), "cannot reach "+addr) {
				t.Errorf("文档说错误里点名地址：\n%s", lastLines(p.Stderr(), 10))
			}
			if strings.Contains(p.Output(), secret) {
				t.Error("密码出现在了输出里")
			}
		})
	}
}

// TestKafka_ConsumerOnlyProcess
//
// 文档（xkafka/README.md「快速上手」）：只消费的进程 xone.MustRun(xone.UntilSignal())，消费者在 StageServer 起来，
// SIGTERM 时停下、提交，以 0 退出
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
		t.Fatalf("SIGTERM 之后应以 0 退出：%v\n%s", exit, lastLines(p.Output(), 30))
	}
	var stops []string
	for _, l := range p.Logs() {
		if l.Msg() == "stopping" {
			stops = append(stops, l.Str("hook"))
		}
	}
	// 消费者先停（StageServer），生产客户端后关（StageClient）
	i, j := slices.Index(stops, "xkafka.stopConsumers"), slices.Index(stops, "xkafka.closeXKafka")
	if i < 0 || j < 0 || i > j {
		t.Errorf("文档说消费者先停、客户端后关：%v", stops)
	}
	for part := range int32(2) {
		if got := harness.KafkaCommitted(t, p.KafkaGroup, topic, part); got != 1 {
			t.Errorf("分区 %d 退出前提交：committed=%d", part, got)
		}
	}
}
