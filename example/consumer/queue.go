package main

import (
	"context"
	"sync"
	"sync/atomic"
)

// Message is one message, standing for the kafka.Message / amqp.Delivery / nsq.Message of whichever
// client is in use.
//
// Ack / Nack are methods rather than arguments of the Consumer because the acknowledgement has to go
// back to the connection or session it was taken from, which only the message itself knows.
type Message struct {
	ID   string
	Body []byte

	ack  func()
	nack func()
}

// Ack acknowledges that this message has been handled
func (m Message) Ack() {
	if m.ack != nil {
		m.ack()
	}
}

// Nack tells the queue this one was not handled successfully, to be redelivered or sent to the dead
// letter queue by its own rules
func (m Message) Nack() {
	if m.nack != nil {
		m.nack()
	}
}

// Queue is the smallest interface a consumer needs. Swap in a kafka / rabbitmq / nsq client and not
// one word of the Consumer's shape below has to change.
type Queue interface {
	// Next takes the next message. It returns false when the ctx is cancelled or the queue is
	// closed.
	//
	// It has to accept a ctx: when a shutdown signal arrives, the workers blocked on "waiting for the
	// next message" have to wake up at once, or the process will not exit until another message
	// arrives.
	Next(ctx context.Context) (Message, bool)

	// Close disconnects from the queue. Most clients commit their offsets here, so it must not be
	// called until the messages in flight have been handled -- see Consumer.Start.
	Close() error
}

// memQueue is an in-memory queue, only so that this example runs as it is.
// A real project has no such type at all; a client of your own takes its place.
type memQueue struct {
	ch chan Message

	// done signals the close. It is used rather than close(ch) because a producer may be publishing
	// at exactly that moment, and publishing to a closed channel panics -- an occasional crash on
	// exit that only reproduces when "somebody happens to be publishing at the moment it closes"
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
	case <-q.done: // already closed, this one is dropped
	case q.ch <- m:
	}
}

func (q *memQueue) record(into *[]string, id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	*into = append(*into, id)
}

func (q *memQueue) Next(ctx context.Context) (Message, bool) {
	// The ctx is checked once on its own first: select picks at random among the cases that are ready
	// at the same time, so with the ctx cancelled and messages still in the queue there is a one in
	// two chance of taking one anyway -- a worker consuming on after the shutdown signal, which is
	// exactly what the Consumer promised not to do
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
