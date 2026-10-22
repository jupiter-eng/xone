package xkafka

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"

	"github.com/jupiter-eng/xone/internal/xclient"
	"github.com/jupiter-eng/xone/xapp"
	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xhook"

	// Using xkafka brings tracing: xtrace installs the global TracerProvider and Propagator, and the trace context is
	// written into the message headers when producing and picked up from them when consuming, both through otel's global API.
	_ "github.com/jupiter-eng/xone/xtrace"
)

// pingAttempts is how many attempts the connectivity probe makes at startup.
const pingAttempts = 3

// pingInterval is the upper bound on the first backoff, doubling after that (xutil.Retry); between 3 attempts the two backoff bounds are 1s and 2s,
// as in xgorm / xredis. It is a variable rather than a constant only so tests can shorten it.
var pingInterval = time.Second

// New builds a Kafka client from the configuration and touches no globals. What comes back is the native *kgo.Client
// with no consumer group set, for producing (ProduceSync / Produce); consuming goes through Consume.
//
// It probes connectivity first (asking a broker for metadata once): a wrong address or a wrong password should surface at startup
// rather than at the first message in production. The ctx bounds that round of probing and gives up on the spot when a stop signal arrives.
//
// The io.Closer returned only closes the client: franz-go's Close fails every message still in the buffer with
// kgo.ErrClientClosed (measured on v1.21.7: 10 of 10 lost). A direct call to New should Flush(ctx) before closing;
// through the framework, the stop hook Flushes before it Closes.
func New(ctx context.Context, cfg ClientConfig) (*kgo.Client, io.Closer, error) {
	return newClient(ctx, "", cfg)
}

// newClient is New with name being the cluster name from the config file, used only in the logs.
func newClient(ctx context.Context, name string, cfg ClientConfig) (*kgo.Client, io.Closer, error) {
	opts, err := clientOpts(name, cfg)
	if err != nil {
		return nil, nil, err
	}
	client, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, nil, xerror.Newf("xkafka", "new", "create client: %w", err)
	}

	if err := xclient.Probe(ctx, probePolicy(cfg), probe(client)); err != nil {
		client.Close()
		addrs := strings.Join(cfg.Brokers, ",")
		// The original error is carried with %w, which the caller needs to judge the root cause. It holds no credential:
		// a failed connection gives a dial error (only the address), and a failed authentication gives the broker's error code and its description.
		if authFailed(err) {
			return nil, nil, xerror.Newf("xkafka", "connect", "authentication to %s failed: %w", addrs, err)
		}
		return nil, nil, xerror.Newf("xkafka", "connect", "cannot reach %s: %w", addrs, err)
	}
	return client, clientCloser{client}, nil
}

// clientOpts are the settings shared by one client (the producing one, or a Consume's own).
// The configuration is validated here once: a direct call to New never went through xconfig.
func clientOpts(name string, cfg ClientConfig) ([]kgo.Opt, error) {
	if err := cfg.Validate(); err != nil {
		return nil, xerror.Newf("xkafka", "config", "invalid config: %w", err)
	}
	tlsCfg, err := cfg.TLS.Build()
	if err != nil {
		return nil, xerror.Newf("xkafka", "config", "invalid TLS config: %w", err)
	}

	opts := []kgo.Opt{
		kgo.SeedBrokers(cfg.Brokers...),
		// It governs TCP plus the TLS handshake: franz-go v1.21.7 wraps a net.Dialer{Timeout} in a tls.Dialer,
		// so the handshake is inside the same timeout.
		kgo.DialTimeout(cfg.DialTimeout),
		kgo.ProducerLinger(cfg.Producer.Linger),
		kgo.ProducerBatchCompression(compression(cfg.Producer.Compression)),
		// franz-go's own logs are routed into slog, accepting WARN and above only, see slogLogger
		kgo.WithLogger(slogLogger{name: name}),
		kgo.WithHooks(produceHook{name: name, trace: cfg.Trace, log: cfg.Log}),
	}
	if cfg.ClientID != "" {
		opts = append(opts, kgo.ClientID(cfg.ClientID))
	}
	if tlsCfg != nil {
		opts = append(opts, kgo.DialTLSConfig(tlsCfg))
	}
	if m := saslMechanism(cfg.SASL); m != nil {
		opts = append(opts, kgo.SASL(m))
	}
	switch cfg.Producer.Acks {
	case "leader":
		// franz-go's idempotent writes require acks=all; without turning them off, NewClient reports an error outright
		opts = append(opts, kgo.RequiredAcks(kgo.LeaderAck()), kgo.DisableIdempotentWrite())
	case "none":
		opts = append(opts, kgo.RequiredAcks(kgo.NoAck()), kgo.DisableIdempotentWrite())
	}
	return opts, nil
}

func compression(name string) kgo.CompressionCodec {
	switch name {
	case "gzip":
		return kgo.GzipCompression()
	case "snappy":
		return kgo.SnappyCompression()
	case "lz4":
		return kgo.Lz4Compression()
	case "zstd":
		return kgo.ZstdCompression()
	}
	return kgo.NoCompression()
}

func saslMechanism(s SASLConfig) sasl.Mechanism {
	switch s.Mechanism {
	case "PLAIN":
		return plain.Auth{User: s.Username, Pass: s.Password}.AsMechanism()
	case "SCRAM-SHA-256":
		return scram.Auth{User: s.Username, Pass: s.Password}.AsSha256Mechanism()
	case "SCRAM-SHA-512":
		return scram.Auth{User: s.Username, Pass: s.Password}.AsSha512Mechanism()
	}
	return nil
}

// probePolicy is how the probe at startup retries. A rejected authentication is not retried: a wrong password stays wrong.
func probePolicy(cfg ClientConfig) xclient.ProbePolicy {
	return xclient.ProbePolicy{
		Attempts:   pingAttempts,
		Timeout:    pingTimeout(cfg),
		Interval:   pingInterval,
		AuthFailed: authFailed,
	}
}

// probe makes one Ping and returns on the spot when the ctx is reached or cancelled, without waiting for it.
//
// franz-go v1.21.7's Ping does not honor the ctx when the peer accepts the connection and never answers: after the request goes out it waits for the read timeout
// (RequestTimeoutOverhead, 10s by default), and measured, a Ping given a 200ms deadline still took 10s to return,
// with a stop signal in between waiting out the same 10s. So it runs in a goroutine while this side watches the ctx.
// The abandoned goroutine does not leak: on a failed probe newClient closes the client, and closing the connection makes the blocked read return at once.
func probe(client *kgo.Client) func(context.Context) error {
	return func(ctx context.Context) error {
		done := make(chan error, 1)
		go func() { done <- client.Ping(ctx) }()
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// pingTimeout is one probe's budget: a connection plus one round trip, computed as 2 x DialTimeout.
// With DialTimeout configured as 0, franz-go uses its own 10s.
func pingTimeout(cfg ClientConfig) time.Duration {
	d := cfg.DialTimeout
	if d == 0 {
		d = 10 * time.Second
	}
	return 2 * d
}

// authFailed reports whether the broker explicitly rejected these credentials (a failed SASL authentication, an unsupported mechanism).
func authFailed(err error) bool {
	return errors.Is(err, kerr.SaslAuthenticationFailed) || errors.Is(err, kerr.UnsupportedSaslMechanism) ||
		errors.Is(err, kerr.IllegalSaslState)
}

type clientCloser struct{ client *kgo.Client }

// Close closes the client. Messages still in the buffer fail; through the framework they have already been Flushed by then.
func (c clientCloser) Close() error {
	c.client.Close()
	return nil
}

// ---- global instances ----

// C fetches a Kafka client (for producing); without an argument, the one named default.
//
// If there is none it panics, for the reason given in xclient.Registry.Get. An optional dependency checks with Has first.
func C(name ...string) *kgo.Client { return reg.Get(name...).client }

// Has reports whether the given cluster is configured, for checking an optional dependency.
func Has(name ...string) bool { return reg.Has(name...) }

// Names returns the configured cluster names.
func Names() []string { return reg.Names() }

// ---- registration ----

// instance is one cluster's producing client together with its configuration: when Consume fetches a cluster by name, it builds its own client from the same configuration.
type instance struct {
	client *kgo.Client
	name   string
	cfg    ClientConfig
}

var reg = xclient.NewRegistry[instance]("xkafka", ConfigKey)

// Two pairs of hooks:
//   - the StageClient pair covers the producing clients, on the same stage as xgorm / xredis;
//   - the StageServer pair covers the consumers Consume registered, on the same stage as xgin / xcron:
//     consuming only begins once the clients and the business hooks are up, and it stops first -- the messages in
//     flight are still using the database and still have dead letters to write, and only once they are processed and their offsets committed are the clients closed.
func init() {
	xhook.BeforeStart(initXKafka, xhook.At(xhook.StageClient))
	xhook.BeforeStop(closeXKafka) // the stage follows the start hook above
	xhook.BeforeStart(startConsumers, xhook.At(xhook.StageServer))
	xhook.BeforeStop(stopConsumers)
}

// initXKafka reads the configuration and builds the clusters one by one by name.
//
// Without this block configured it builds none, but Build still runs: a C() that finds nothing then reports "not configured" rather than "called too early".
func initXKafka(ctx context.Context) error {
	if !xconfig.Has(ConfigKey) {
		return xclient.Build(ctx, reg, nil, build)
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if err := xclient.Build(ctx, reg, c.Clients, build); err != nil {
		return err
	}
	slog.Info("xkafka ready", "instances", reg.Names())
	return nil
}

// closeXKafka first sends out the messages buffered in each client (Flush), then removes every instance and closes them in reverse.
//
// Closing without Flushing would make franz-go fail every buffered message with kgo.ErrClientClosed (see New):
// a message sent with Produce (asynchronously) at the moment of exit would be lost that way. The Flush is bounded by the stop hook's deadline,
// and whatever is unsent by then is failed by Close anyway, with the unflushed cluster names in the error.
func closeXKafka(ctx context.Context) error {
	var errs []error
	for name, inst := range reg.All() {
		if err := inst.client.Flush(ctx); err != nil {
			errs = append(errs, xerror.Newf("xkafka", "close", "flush %q before close: %w", name, err))
		}
	}
	return errors.Join(append(errs, reg.Close())...)
}

// build builds one cluster's producing client; the resolved ClientConfig it keeps is also what this cluster's
// consumers are built from, so the client id settled here is the one both sides send.
//
// Without ClientID configured it uses XApp.Name: the brokers tell clients apart by client.id in their request logs
// and their quotas, and configuring the identity once is what the XApp block is for.
func build(ctx context.Context, name string, c ClientConfig) (instance, io.Closer, error) {
	if c.ClientID == "" {
		c.ClientID = xapp.Name()
	}
	if c.ClientID == "" {
		// Nothing is invented here, as nothing is invented for the logs, the metrics or the spans. Unlike those
		// three this one cannot simply be left out -- client.id goes into every request header and franz-go sends
		// its own default when none is set -- so the consequence is said out loud once, at startup.
		slog.WarnContext(ctx, "xkafka no client id configured, the brokers will see franz-go's default kgo and cannot tell this service apart in their request logs or quotas, set XKafka.ClientID or XApp.Name", "name", name)
	}
	client, closer, err := newClient(ctx, name, c)
	if err != nil {
		return instance{}, nil, err
	}
	// Only the address and the authentication mechanism go into the log; the user name and password do not.
	slog.Info("xkafka connected", "name", name, "brokers", c.Brokers, "tls", c.TLS.Enable, "sasl", c.SASL.Mechanism)
	return instance{client: client, name: name, cfg: c}, closer, nil
}
