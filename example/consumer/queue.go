package main

import (
	"context"
	"sync"
	"sync/atomic"
)

// Message 一条消息。对应你的客户端里的 kafka.Message / amqp.Delivery / nsq.Message。
//
// Ack / Nack 之所以是方法而不是 Consumer 的参数：确认要发回取出它的那个
// 连接/会话，这件事只有消息自己知道。
type Message struct {
	ID   string
	Body []byte

	ack  func()
	nack func()
}

// Ack 确认这条消息已处理
func (m Message) Ack() {
	if m.ack != nil {
		m.ack()
	}
}

// Nack 告诉队列这条没处理成功，按它的规则重投或进死信
func (m Message) Nack() {
	if m.nack != nil {
		m.nack()
	}
}

// Queue 消费者需要的最小接口。把它换成你的 kafka / rabbitmq / nsq 客户端，
// 下面 Consumer 的形状一个字都不用改。
type Queue interface {
	// Next 取下一条消息。ctx 取消或队列关闭时返回 false。
	//
	// 必须接受 ctx：退出信号到达时，卡在「等下一条消息」上的那些 worker
	// 要能立刻醒过来，否则进程得等到下一条消息进来才肯退出。
	Next(ctx context.Context) (Message, bool)

	// Close 断开与队列的连接。多数客户端会在这里提交 offset，
	// 所以它必须等在途消息处理完之后才调——见 Consumer.Start。
	Close() error
}

// memQueue 一个内存队列，只为让这个例子能直接跑起来。
// 真实项目里这一整个类型都不存在，换成你的客户端即可。
type memQueue struct {
	ch chan Message

	// done 关闭的信号。用它而不是 close(ch)：生产者可能正好在这一刻
	// 往里发，而往已关闭的 channel 发会 panic——退出时偶发崩溃，
	// 只有「关的那一刻正好有人在投递」才复现
	done     chan struct{}
	closed   atomic.Bool
	closeOne sync.Once

	mu     sync.Mutex
	acked  []string
	nacked []string
}

func newMemQueue(buf int) *memQueue {
	return &memQueue{ch: make(chan Message, buf), done: make(chan struct{})}
}

func (q *memQueue) publish(id string, body []byte) {
	m := Message{ID: id, Body: body}
	m.ack = func() { q.record(&q.acked, id) }
	m.nack = func() { q.record(&q.nacked, id) }
	select {
	case <-q.done: // 已经关了，丢掉这条
	case q.ch <- m:
	}
}

func (q *memQueue) record(into *[]string, id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	*into = append(*into, id)
}

func (q *memQueue) Next(ctx context.Context) (Message, bool) {
	// 先单独查一次 ctx：select 在几路同时就绪时随机挑一路，ctx 取消了、
	// 队列里又还有消息的话，有一半的概率照样取走一条——退出信号到了
	// worker 还在接着消费，这正是 Consumer 说好不做的事
	if ctx.Err() != nil {
		return Message{}, false
	}
	select {
	case <-ctx.Done():
		return Message{}, false
	case <-q.done:
		return Message{}, false
	case m := <-q.ch:
		return m, true
	}
}

func (q *memQueue) Close() error {
	q.closeOne.Do(func() {
		q.closed.Store(true)
		close(q.done)
	})
	return nil
}
