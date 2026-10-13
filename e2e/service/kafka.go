package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jupiter-eng/xone/e2e/service/conf"
	"github.com/jupiter-eng/xone/xkafka"
	"github.com/jupiter-eng/xone/xutil"
)

// Kafka：生产经 xkafka.C()，消费经 xkafka.Consume。XKafka 只在 harness 激活 kafka 那份 profile 时才有
// （service/application-kafka.yml），没有时 /kafka/... 一律 503。
//
//	POST /kafka/produce   {"topic": "...", "key": "...", "value": "..."}，用请求的 ctx ProduceSync，回 partition / offset
//
// Service.KafkaTopic 非空时消费它。消息的 value 是一段 JSON，说处理函数该怎么做：
//
//	{"id": "m1", "fail": 2}          前 2 次返回错误，第 3 次成功；-1 是一直失败
//	{"id": "m2", "permanent": true}  返回 xutil.Permanent，不重试
//	{"id": "m3", "hold_ms": 2000}    不看 ctx 地睡这么久再成功（测退出时等在途的消息）
//
// 每次调用记一行 e2e kafka handle（id、attempt），成功时再记一行 e2e kafka done（id、ctx_err：退出时 ctx 没被取消的话是空）。
// 两行都用处理函数收到的 ctx：xlog 由此带上 trace_id 和 topic / partition / offset / group
func kafkaRoutes(e *gin.Engine) {
	g := e.Group("/kafka", requireKafka)
	g.POST("/produce", kafkaProduce)
}

func requireKafka(c *gin.Context) {
	if !xkafka.Has() {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "XKafka is not configured"})
	}
}

type produceReq struct {
	Topic string `json:"topic" binding:"required"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

func kafkaProduce(c *gin.Context) {
	var req produceReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	r := &kgo.Record{Topic: req.Topic, Value: []byte(req.Value)}
	if req.Key != "" {
		r.Key = []byte(req.Key)
	}
	if err := xkafka.C().ProduceSync(c.Request.Context(), r).FirstErr(); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"partition": r.Partition, "offset": r.Offset})
}

// kafkaMsg 消息 value 里的指令
type kafkaMsg struct {
	ID        string `json:"id"`
	Fail      int    `json:"fail"`
	Permanent bool   `json:"permanent"`
	HoldMS    int    `json:"hold_ms"`
}

// attempts 每个 id 被调了几次。进程级的：重新投递到另一个进程时从 1 数起
var attempts = struct {
	sync.Mutex
	n map[string]int
}{n: map[string]int{}}

// addConsumer 按 Service.KafkaTopic 登记消费者
func addConsumer(c conf.Config) {
	var opts []xkafka.Option
	if c.KafkaRetry >= 0 {
		opts = append(opts, xkafka.WithRetry(c.KafkaRetry))
	}
	if c.KafkaTimeout > 0 {
		opts = append(opts, xkafka.WithTimeout(c.KafkaTimeout))
	}
	if err := xkafka.Consume(c.KafkaTopic, c.KafkaGroup, handleKafka, opts...); err != nil {
		log.Fatal(err)
	}
}

func handleKafka(ctx context.Context, r *kgo.Record) error {
	var m kafkaMsg
	if err := json.Unmarshal(r.Value, &m); err != nil {
		return xutil.Permanent(fmt.Errorf("bad message: %w", err))
	}
	attempts.Lock()
	attempts.n[m.ID]++
	n := attempts.n[m.ID]
	attempts.Unlock()
	slog.InfoContext(ctx, "e2e kafka handle", "id", m.ID, "attempt", n)

	switch {
	case m.Permanent:
		return xutil.Permanent(errors.New("e2e permanent failure"))
	case m.Fail < 0 || n <= m.Fail:
		return fmt.Errorf("e2e failure %d", n)
	}
	time.Sleep(time.Duration(m.HoldMS) * time.Millisecond) // 故意不看 ctx：测退出时在途的消息做完
	ctxErr := ""
	if err := ctx.Err(); err != nil {
		ctxErr = err.Error()
	}
	slog.InfoContext(ctx, "e2e kafka done", "id", m.ID, "ctx_err", ctxErr)
	return nil
}
