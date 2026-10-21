package xkafka

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Option is an optional setting for Consume; the uses are on each With*.
type Option func(*options)

// dlqSuffix is what the default dead-letter topic adds to the original topic name.
const dlqSuffix = ".dlq"

const (
	// defaultTimeout is the default timeout of one call to the handler
	defaultTimeout = 30 * time.Second

	// defaultRetries is how many more times a failed handler is called by default (not counting the first)
	defaultRetries = 3
)

type options struct {
	client  string
	timeout time.Duration
	retries int
	dlq     string
}

// WithClient is which cluster to consume from, by the name under XKafka.Clients. It defaults to default.
//
// Every Consume builds its own client from that cluster's configuration (carrying the consumer group) and does not share the one C() returns.
func WithClient(name string) Option {
	return func(o *options) { o.client = name }
}

// WithTimeout is the timeout of one call to the handler: the ctx fn receives is cancelled when it is reached. It defaults to 30s and must be greater than 0.
//
// It only cancels the ctx: an fn that does not look at the ctx runs to completion anyway, since Go has no way to stop a goroutine from the outside.
// Each retry is counted separately. A rebalance waits for the call in flight to finish (see the README's "Rebalancing"),
// so do not configure it longer than franz-go's rebalance timeout (60s, the v1.21.7 default).
func WithTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// WithRetry is how many more times the handler is called after it returns an error (or panics), not counting the first. It defaults to 3, that is at most 4 calls; 0 means no retry.
//
// The wait between two calls starts at 1s and doubles each time with jitter (as in xutil.Retry: the actual wait is a random value in [0, the upper bound]),
// with upper bounds of 1s, 2s, 4s and so on, capped at 30s. A xutil.Permanent(err) is not retried and goes straight to the dead letter.
// On shutdown or when the partition is revoked no further call is started, the message is not committed, and it is redelivered later.
func WithRetry(n int) Option {
	return func(o *options) { o.retries = n }
}

// WithDeadLetter is which topic a message goes to once its retries are used up (the dead letter). It defaults to the original topic plus .dlq (orders -> orders.dlq).
//
// Passing "" turns the dead letter off: a message whose retries are used up is recorded once at ERROR (kafka message skipped), skipped, and its offset committed, with no further processing.
// When the dead-letter write fails it is retried forever and nothing is committed, see the README's "Dead letters".
func WithDeadLetter(topic string) Option {
	return func(o *options) { o.dlq = topic }
}

// buildOptions applies the defaults and every Option.
func buildOptions(topic string, opts []Option) options {
	o := options{client: DefaultName, timeout: defaultTimeout, retries: defaultRetries, dlq: topic + dlqSuffix}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// validate reports whether the settings Consume received hold. It returns a plain error, which Consume wraps in an xerror at the boundary.
func (o options) validate(topic string) error {
	var errs []error
	if strings.TrimSpace(o.client) == "" {
		errs = append(errs, errors.New("client name is empty"))
	}
	if o.timeout <= 0 {
		errs = append(errs, fmt.Errorf("timeout must be > 0, got %v", o.timeout))
	}
	if o.retries < 0 {
		errs = append(errs, fmt.Errorf("retry count must be >= 0, got %d", o.retries))
	}
	if o.dlq != "" && strings.TrimSpace(o.dlq) == "" {
		errs = append(errs, errors.New("dead letter topic is blank, pass \"\" to disable dead lettering"))
	}
	// A dead letter written back to the topic being consumed: a message that cannot be processed comes round again and is never done with.
	if o.dlq == topic {
		errs = append(errs, fmt.Errorf("dead letter topic must differ from the consumed topic %q", topic))
	}
	return errors.Join(errs...)
}
