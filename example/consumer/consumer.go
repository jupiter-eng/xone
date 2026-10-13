package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Consumer 一个消息队列消费者服务。
//
// 它满足 xone.Runnable，所以跟 xgin 一样直接交给 xone.Run：
//
//	xone.MustRun(&Consumer{q: client, handle: handle, settings: settings})
//
// 框架负责的事一件都没变：读配置、按 Stage 建好数据库和缓存、
// 收到信号时取消 ctx、等这里返回之后再逆序关掉那些资源。
// 这个类型只负责「怎么消费」。
type Consumer struct {
	q      Queue
	handle func(context.Context, Message) error

	// settings 从哪来本类型不管：这个例子在 main 里从配置文件读，
	// 测试直接传几个数进来
	settings Settings

	wg sync.WaitGroup
}

// Settings Consumer 需要的几个数。从哪来由调用方决定——
// 这个例子是配置文件，你也可以写死或者从别处拿
type Settings struct {
	// Workers 并发处理消息的协程数
	Workers int

	// Timeout 单条消息的处理上限。
	//
	// 它同时决定了退出时最多等多久：收到信号后不再取新消息，
	// 但在途的那几条各自还有最多 Timeout 的时间做完。所以它必须小于服务那一段
	// 停止预算（xone.WithStopTimeout 的 2/3，默认 10s），否则框架等不到就往下关资源了。
	Timeout time.Duration
}

// Start 拉起 worker 并阻塞到它们全部收工。由 xone.Run 调用。
//
// ctx 在退出信号到达时被取消。
func (c *Consumer) Start(ctx context.Context) error {
	s := c.settings
	slog.InfoContext(ctx, "consumer starting", "workers", s.Workers, "message_timeout", s.Timeout.String())

	for range s.Workers {
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.loop(ctx, s.Timeout)
		}()
	}

	// 等所有在途消息处理完再返回。
	//
	// 这一行是整个类型里最要紧的：框架在 Start 返回之后才关数据库、
	// 缓存和 HTTP 连接池。提前返回的话，还在处理的那几条消息会摸到
	// 已经关掉的连接池——表现是退出时零星几条「connection is closed」，
	// 而那几条消息既没处理完、也已经不在队列里了。
	c.wg.Wait()

	// Close 放在在途消息做完之后，不要更早。多数客户端 Close 时会顺带
	// 提交 offset，提前提交等于把还没处理完的消息标记成已消费——
	// 退出时静默丢消息，而且只在「正好有在途消息」的那一刻发生，很难复现。
	//
	// 这个类型没有 Stop：ctx 被取消就足够让 Next 醒过来。你的客户端要是
	// 得调 Pause / Unsubscribe 才停得下来，再写一个 Stop(ctx) error 放它们，
	// 同样别在里面 Close——框架是先调 Stop、再等 Start 返回的
	slog.InfoContext(ctx, "consumer drained, closing the queue client")
	return c.q.Close()
}

// loop 一个 worker：取一条、处理一条，直到 ctx 被取消或队列关闭
func (c *Consumer) loop(ctx context.Context, timeout time.Duration) {
	for {
		m, ok := c.q.Next(ctx)
		if !ok {
			return
		}
		c.process(ctx, m, timeout)
	}
}

// process 处理一条消息。
//
// 第一行是这里的关键：剥掉取消，另给一份处理预算。
//
// 沿用已取消的 parent 的话，这条消息里每一次写库、每一次调下游、
// 连最后那次 Ack 都会一进去就被拒绝——消息没做完，队列那边也没收到确认。
// 补偿逻辑最需要跑完的时机，恰恰就是退出的这一刻，和 xflow 的回滚同理。
//
// 剥掉之后由 Timeout 单独限时，免得一条卡住的消息拖住整个退出流程。
func (c *Consumer) process(parent context.Context, m Message, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	defer cancel()

	// panic 隔离：一条毒消息不该让整个 worker 挂掉，剩下的消息没人消费
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "message handler panicked", "message_id", m.ID, "panic", r)
			m.Nack()
		}
	}()

	if err := c.handle(ctx, m); err != nil {
		slog.ErrorContext(ctx, "message handling failed", "message_id", m.ID, "error", err)
		m.Nack()
		return
	}
	m.Ack()
}
