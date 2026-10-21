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

// Kafka: producing goes through xkafka.C() and consuming through xkafka.Consume. The XKafka exists
// only when the harness activates the kafka profile (service/application-kafka.yml), and without
// it every /kafka/... answers 503.
//
//	POST /kafka/produce   {"topic": "...", "key": "...", "value": "..."}, a ProduceSync on the request's ctx, answering partition / offset
//
// With Service.KafkaTopic non-empty it is consumed. A message's value is a piece of JSON saying
// what the handler should do:
//
//	{"id": "m1", "fail": 2}          returns an error the first 2 times and succeeds on the 3rd; -1 fails forever
//	{"id": "m2", "permanent": true}  returns xutil.Permanent and is not retried
//	{"id": "m3", "hold_ms": 2000}    sleeps that long without watching ctx and then succeeds (for waiting out the messages in flight on exit)
//
// Every call records an e2e kafka handle line (id, attempt), and a successful one an e2e kafka done
// line as well (id, ctx_err: empty when ctx was not cancelled during the exit).
// Both go on the ctx the handler received, which is how xlog carries trace_id and topic /
// partition / offset / group
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

// kafkaMsg is the instruction inside a message value
type kafkaMsg struct {
	ID        string `json:"id"`
	Fail      int    `json:"fail"`
	Permanent bool   `json:"permanent"`
	HoldMS    int    `json:"hold_ms"`
}

// attempts is how many times each id was called. It is per process: a redelivery to another process
// starts counting from 1
var attempts = struct {
	sync.Mutex
	n map[string]int
}{n: map[string]int{}}

// addConsumer registers the consumer for Service.KafkaTopic
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
	time.Sleep(time.Duration(m.HoldMS) * time.Millisecond) // deliberately not watching ctx: for a message in flight finishing during the exit
	ctxErr := ""
	if err := ctx.Err(); err != nil {
		ctxErr = err.Error()
	}
	slog.InfoContext(ctx, "e2e kafka done", "id", m.ID, "ctx_err", ctxErr)
	return nil
}
