// Package xkafka sets up franz-go from the configuration: producing uses the native *kgo.Client, and consuming registers a handler with Consume.
//
//	err := xkafka.C().ProduceSync(ctx, &kgo.Record{Topic: "orders", Value: b}).FirstErr()
//
//	xkafka.Consume("orders", "billing", func(ctx context.Context, r *kgo.Record) error { … })
//
// There is no CWithCtx: every franz-go producing and consuming method takes a ctx anyway.
package xkafka

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xtls"
)

// ConfigKey is this module's top-level key in the config file.
const ConfigKey = "XKafka"

// DefaultName is the name of the cluster C() returns without an argument, and also the one Consume uses by default.
const DefaultName = xconfig.DefaultClientName

// Config is this module's configuration. There are two forms:
//
//	XKafka:                 # a single cluster
//	  Brokers: ["127.0.0.1:9092"]
//
//	XKafka:                 # several clusters
//	  Clients:
//	    default: {Brokers: ["10.0.0.1:9092", "10.0.0.2:9092"]}
//	    audit:   {Brokers: ["10.0.1.1:9092"]}
type Config struct {
	// Clients holds the clusters by name. The single-cluster form is normalized to one cluster named default.
	Clients map[string]ClientConfig
}

// ClientConfig is the configuration of one Kafka cluster.
type ClientConfig struct {
	// Brokers are the seed broker addresses, host:port. It is required and has no default.
	//
	// They are only seeds: once connected to any one of them, franz-go learns the whole cluster's brokers from the metadata.
	Brokers []string `yaml:"Brokers"`

	// ClientID is the client.id sent to the brokers, which the broker-side logs, quotas and monitoring use to tell clients apart.
	// It defaults to empty: one built through the framework uses XApp.Name, and without XApp.Name configured it is franz-go's default kgo
	// (measured on v1.21.7); a direct call to New does not read XApp, so empty means kgo.
	ClientID string `yaml:"ClientID"`

	// DialTimeout is the timeout for making one connection (TCP, plus the TLS handshake). It defaults to 1s; franz-go's own default is 10s.
	//
	// Each attempt of the connectivity probe at startup has a budget of 2 x DialTimeout: a connection plus one round trip, see the README's "Behavior (measured)".
	DialTimeout time.Duration `yaml:"DialTimeout"`

	// SASL authentication. There is none by default.
	SASL SASLConfig `yaml:"SASL"`

	// TLS says whether to use TLS when connecting to the brokers. It defaults to off. The fields and rules are shared across modules, see xtls.Config.
	TLS xtls.Config `yaml:"TLS"`

	// Producer holds the settings of the producing side.
	Producer ProducerConfig `yaml:"Producer"`

	// Consumer holds the settings of the consuming side, applying to every Consume on this cluster.
	Consumer ConsumerConfig `yaml:"Consumer"`

	// Trace says whether a Span is opened for producing and for consuming. It defaults to on.
	//
	// Turning it off only turns off the Spans: the trace context (traceparent and the rest) is still written into the message headers and still picked up from them,
	// so a downstream service with tracing on still sees one trace.
	Trace bool `yaml:"Trace"`

	// Metric says whether the consume duration metric (kafka_consume_duration_seconds) is exported. It defaults to on.
	Metric bool `yaml:"Metric"`

	// Log says whether per-message logs are recorded: a failed produce (WARN), each failed processing attempt (WARN) and a successful one (DEBUG). It defaults to on.
	//
	// With it off, a message reaching the dead letter, a skipped message, a panicking handler and a failed dead-letter write are still recorded --
	// they mean a message did not travel its normal path, and without a record there would be no trace of it at all.
	// Only the message key is recorded (printable UTF-8, truncated beyond 256 bytes), never the value, see the README's "Observability".
	Log bool `yaml:"Log"`
}

// SASLConfig is the SASL authentication.
type SASLConfig struct {
	// Mechanism is the authentication mechanism: PLAIN, SCRAM-SHA-256, SCRAM-SHA-512. It defaults to empty, meaning no authentication.
	//
	// PLAIN sends the password to the broker as it is, which is plaintext on the wire without TLS configured.
	Mechanism string `yaml:"Mechanism"`

	// Username is the user name. It is required when Mechanism is non-empty.
	Username string `yaml:"Username"`

	// Password is the password. It is required when Mechanism is non-empty.
	//
	// Writing it as "${KAFKA_PASSWORD}" is the natural choice: a credential does not belong in version control, and a missing one fails startup.
	// This module never writes it into any log or error.
	Password string `yaml:"Password"`
}

// ProducerConfig is the producing side. Anything not listed keeps franz-go's default; the numbers are in the README's "Behavior (measured)".
type ProducerConfig struct {
	// Acks is how many replicas have to acknowledge a write: all, leader, none. It defaults to all (franz-go's default).
	//
	// With all, idempotent writes are on (franz-go's default): a retry writes no duplicate and the order within a partition is kept.
	// With leader or none, franz-go requires idempotent writes to be off, which this module does on the caller's behalf: a retry may then write a duplicate.
	Acks string `yaml:"Acks"`

	// Linger is how long a batch waits at most before being sent, for batching. It defaults to 10ms (franz-go's default); 0 means no waiting, send what there is.
	Linger time.Duration `yaml:"Linger"`

	// Compression is the batch compression algorithm: none, gzip, snappy, lz4, zstd. It defaults to snappy (franz-go's default).
	Compression string `yaml:"Compression"`
}

// ConsumerConfig is the consuming side.
type ConsumerConfig struct {
	// ResetOffset is where to start when a consumer group consumes a partition for the first time (with no committed offset),
	// or when the committed offset has already been cleaned up: latest (only what arrives afterwards) or earliest (from the oldest message still retained). It defaults to latest.
	//
	// franz-go's default is earliest (measured on v1.21.7): a newly deployed consumer group would process every historical message
	// still retained in the topic. This module defaults to latest, at the cost of a new group, or a group starting just after the topic was created,
	// never processing what was written before -- configure earliest when those are wanted.
	ResetOffset string `yaml:"ResetOffset"`
}

// DefaultClientConfig holds every default of a single cluster in one place.
func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		DialTimeout: time.Second,
		Producer: ProducerConfig{
			Acks:        "all",
			Linger:      10 * time.Millisecond,
			Compression: "snappy",
		},
		Consumer: ConsumerConfig{ResetOffset: "latest"},
		Trace:    true,
		Metric:   true,
		Log:      true,
	}
}

// DefaultConfig has no clusters at all -- without XKafka configured, no Kafka should be connected to.
func DefaultConfig() Config { return Config{} }

// loadConfig reads this block of the config file. Both the single-cluster and the multi-cluster form are accepted,
// each cluster is pre-filled with DefaultClientConfig before decoding, and the rules are in xconfig.UnmarshalClients.
func loadConfig() (Config, error) {
	clients, err := xconfig.UnmarshalClients(ConfigKey, DefaultClientConfig)
	return Config{Clients: clients}, err
}

var (
	saslMechanisms = []string{"PLAIN", "SCRAM-SHA-256", "SCRAM-SHA-512"}
	acksValues     = []string{"all", "leader", "none"}
	compressions   = []string{"none", "gzip", "snappy", "lz4", "zstd"}
	resetOffsets   = []string{"latest", "earliest"}
)

// Validate checks what does not make sense in the configuration itself and fails before anything is connected to.
//
// xconfig.UnmarshalClients calls it once per decoded cluster; a direct call to New calls it once as well.
// It returns a plain error, which the layer calling it wraps in an xerror once. The error carries no password.
func (c ClientConfig) Validate() error {
	var errs []error
	if len(c.Brokers) == 0 {
		errs = append(errs, errors.New("Brokers must not be empty"))
	}
	for _, b := range c.Brokers {
		if strings.TrimSpace(b) == "" {
			errs = append(errs, errors.New("Brokers must not contain an empty address"))
			break
		}
	}
	// A negative duration is not "no limit" in franz-go, it is a dial that fails immediately.
	if c.DialTimeout < 0 {
		errs = append(errs, fmt.Errorf("DialTimeout must not be negative, got=%v", c.DialTimeout))
	}
	if c.Producer.Linger < 0 {
		errs = append(errs, fmt.Errorf("Producer.Linger must not be negative, got=%v", c.Producer.Linger))
	}
	errs = append(errs,
		oneOf("Producer.Acks", c.Producer.Acks, acksValues),
		oneOf("Producer.Compression", c.Producer.Compression, compressions),
		oneOf("Consumer.ResetOffset", c.Consumer.ResetOffset, resetOffsets),
		c.SASL.validate(),
		c.TLS.Validate(),
	)
	return errors.Join(errs...)
}

func (s SASLConfig) validate() error {
	if s.Mechanism == "" {
		if s.Username != "" || s.Password != "" {
			return errors.New("SASL.Username / SASL.Password are set but SASL.Mechanism is empty; set it to one of " + strings.Join(saslMechanisms, ", "))
		}
		return nil
	}
	if err := oneOf("SASL.Mechanism", s.Mechanism, saslMechanisms); err != nil {
		return err
	}
	if s.Username == "" || s.Password == "" {
		return errors.New("SASL.Username and SASL.Password are required when SASL.Mechanism is set")
	}
	return nil
}

// oneOf requires the value to be one of the list (case-sensitively: for a value written with the wrong case, an error beats a guess).
func oneOf(field, v string, allowed []string) error {
	if slices.Contains(allowed, v) {
		return nil
	}
	return fmt.Errorf("%s must be one of %s, got=%q", field, strings.Join(allowed, ", "), v)
}
