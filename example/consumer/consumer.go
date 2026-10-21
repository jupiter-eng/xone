package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Consumer is a message queue consumer service.
//
// It satisfies xone.Runnable, so it is handed to xone.Run just like xgin:
//
//	xone.MustRun(&Consumer{q: client, handle: handle, settings: settings})
//
// Nothing the framework takes care of changes: reading the configuration, building the database and
// the cache by Stage, cancelling the ctx when the signal arrives, and closing those resources in
// reverse order only after this has returned. This type is responsible for "how to consume" alone.
type Consumer struct {
	q      Queue
	handle func(context.Context, Message) error

	// Where settings comes from is none of this type's business: this example reads it from the
	// configuration file in main, and the tests simply pass a few numbers in
	settings Settings

	wg sync.WaitGroup
}

// Settings is the handful of numbers a Consumer needs. Where they come from is the caller's
// decision -- this example uses the configuration file, while writing them down or taking them from
// somewhere else works as well
type Settings struct {
	// Workers is how many goroutines handle messages concurrently
	Workers int

	// Timeout is the upper bound on handling one message.
	//
	// It also decides how long the exit waits at most: once the signal has arrived no new message is
	// taken, while the few in flight each still have up to Timeout to finish. So it has to be smaller
	// than the server's share of the stop budget (2/3 of xone.WithStopTimeout, 10s by default), or
	// the framework stops waiting and closes the resources anyway.
	Timeout time.Duration
}

// Start brings the workers up and blocks until all of them have finished. It is called by
// xone.Run.
//
// The ctx is cancelled when a shutdown signal arrives.
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

	// Returning only once every message in flight has been handled.
	//
	// This line matters most in the whole type: the framework closes the database, the cache and the
	// HTTP connection pool only after Start has returned. Returning early, the messages still being
	// handled would reach connection pools that are already closed -- showing up as a scattering of
	// "connection is closed" on exit, with those messages neither finished nor still in the queue.
	c.wg.Wait()

	// Close goes after the messages in flight are done, not earlier. Most clients commit their
	// offsets along with Close, and committing early marks messages that have not been handled as
	// consumed -- messages lost quietly on exit, and only at the moment "there happens to be
	// something in flight", which is hard to reproduce.
	//
	// This type has no Stop: a cancelled ctx is enough to wake Next. A client that can only be
	// stopped by calling Pause / Unsubscribe gets a Stop(ctx) error holding those, and likewise does
	// not Close inside it -- the framework calls Stop first and waits for Start to return afterwards
	slog.InfoContext(ctx, "consumer drained, closing the queue client")
	return c.q.Close()
}

// loop is one worker: take one, handle one, until the ctx is cancelled or the queue is closed
func (c *Consumer) loop(ctx context.Context, timeout time.Duration) {
	for {
		m, ok := c.q.Next(ctx)
		if !ok {
			return
		}
		c.process(ctx, m, timeout)
	}
}

// process handles one message.
//
// The first line is what matters here: the cancellation is stripped off and a handling budget of its
// own is given.
//
// Reusing an already cancelled parent, every write to the database, every call downstream and even
// the final Ack in this message would be refused the moment it begins -- the message unfinished and
// the queue never acknowledged. The moment compensating logic most needs to run to completion is
// exactly this one, on the way out, for the same reason xflow's rollback does.
//
// Once stripped, Timeout bounds it on its own, so that one stuck message cannot hold up the whole
// shutdown.
func (c *Consumer) process(parent context.Context, m Message, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	defer cancel()

	// Containing a panic: one poison message should not take the whole worker down and leave the
	// remaining messages unconsumed
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
