package xkafka

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/internal/config"
)

func load(t *testing.T, yml string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	config.Reset()
	t.Cleanup(config.Reset)
	if err := config.Load(path); err != nil {
		return Config{}, err
	}
	return loadConfig()
}

func TestConfig_SingleClusterFormKeepsDefaults(t *testing.T) {
	c, err := load(t, "XKafka:\n  Brokers: [\"10.0.0.1:9092\"]\n  Producer:\n    Linger: 0s\n")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.Clients[DefaultName]
	if !ok {
		t.Fatalf("single form should become %q: %v", DefaultName, c.Clients)
	}
	if got.Brokers[0] != "10.0.0.1:9092" || got.Producer.Linger != 0 {
		t.Errorf("fields not decoded: %+v", got)
	}
	if got.DialTimeout != time.Second || got.Producer.Acks != "all" || got.Producer.Compression != "snappy" ||
		got.Consumer.ResetOffset != "latest" || !got.Trace || !got.Metric || !got.Log {
		t.Errorf("unset fields should keep their defaults: %+v", got)
	}
}

func TestConfig_MultiClusterForm(t *testing.T) {
	c, err := load(t, "XKafka:\n  Clients:\n    default: {Brokers: [\"a:9092\"]}\n    audit: {Brokers: [\"b:9092\"], Consumer: {ResetOffset: earliest}}\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Clients) != 2 || c.Clients["audit"].Consumer.ResetOffset != "earliest" || c.Clients["audit"].Producer.Acks != "all" {
		t.Fatalf("multi: %+v", c.Clients)
	}
}

func TestConfig_UnknownFieldAndUnsetVarFail(t *testing.T) {
	if _, err := load(t, "XKafka:\n  Brokers: [\"a:9092\"]\n  Broker: x\n"); err == nil || !strings.Contains(err.Error(), "Broker") {
		t.Errorf("unknown field: %v", err)
	}
	if _, err := load(t, "XKafka:\n  Brokers: [\"a:9092\"]\n  SASL: {Mechanism: PLAIN, Username: u, Password: \"${XKAFKA_TEST_UNSET_PW}\"}\n"); err == nil {
		t.Error("unset ${VAR} should fail")
	}
}

func TestConfig_Validate(t *testing.T) {
	ok := DefaultClientConfig()
	ok.Brokers = []string{"a:9092"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for _, c := range []struct {
		name string
		mut  func(*ClientConfig)
		want string
	}{
		{"no brokers", func(c *ClientConfig) { c.Brokers = nil }, "Brokers must not be empty"},
		{"blank broker", func(c *ClientConfig) { c.Brokers = []string{" "} }, "empty address"},
		{"negative dial", func(c *ClientConfig) { c.DialTimeout = -1 }, "DialTimeout"},
		{"negative linger", func(c *ClientConfig) { c.Producer.Linger = -time.Millisecond }, "Producer.Linger"},
		{"acks", func(c *ClientConfig) { c.Producer.Acks = "1" }, "Producer.Acks must be one of all, leader, none"},
		{"compression", func(c *ClientConfig) { c.Producer.Compression = "brotli" }, "Producer.Compression"},
		{"reset", func(c *ClientConfig) { c.Consumer.ResetOffset = "smallest" }, "Consumer.ResetOffset"},
		{"mechanism", func(c *ClientConfig) { c.SASL = SASLConfig{Mechanism: "GSSAPI", Username: "u", Password: "p"} }, "SASL.Mechanism"},
		{"sasl no password", func(c *ClientConfig) { c.SASL = SASLConfig{Mechanism: "PLAIN", Username: "u"} }, "required"},
		{"sasl without mechanism", func(c *ClientConfig) { c.SASL = SASLConfig{Username: "u", Password: "p"} }, "SASL.Mechanism is empty"},
		{"tls fields without enable", func(c *ClientConfig) { c.TLS.CAFile = "/x" }, "TLS.Enable"},
		// franz-go refuses a session timeout under the interval it heartbeats at, with a message
		// naming neither this field nor where the interval comes from
		{"session timeout under the heartbeat interval",
			func(c *ClientConfig) { c.Consumer.SessionTimeout = time.Second }, "Consumer.SessionTimeout"},
		{"negative session timeout",
			func(c *ClientConfig) { c.Consumer.SessionTimeout = -time.Second }, "Consumer.SessionTimeout"},
	} {
		cfg := ok
		c.mut(&cfg)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestConfig_SessionTimeoutZeroAndTheFloorAreAccepted(t *testing.T) {
	// Zero is "keep franz-go's own default" and has to stay valid, and the floor itself is a value
	// somebody may well write
	ok := DefaultClientConfig()
	ok.Brokers = []string{"a:9092"}
	for _, d := range []time.Duration{0, minSessionTimeout, time.Minute} {
		cfg := ok
		cfg.Consumer.SessionTimeout = d
		if err := cfg.Validate(); err != nil {
			t.Errorf("Consumer.SessionTimeout=%v should be accepted, got %v", d, err)
		}
	}
}

func TestConfig_ValidateNeverEchoesPassword(t *testing.T) {
	cfg := DefaultClientConfig()
	cfg.SASL = SASLConfig{Mechanism: "SCRAM-SHA-1", Username: "u", Password: "s3cret-pw"}
	if err := cfg.Validate(); err == nil || strings.Contains(err.Error(), "s3cret-pw") {
		t.Fatalf("validate: %v", err)
	}
}

func TestSASLMechanisms(t *testing.T) {
	for m, want := range map[string]string{"PLAIN": "PLAIN", "SCRAM-SHA-256": "SCRAM-SHA-256", "SCRAM-SHA-512": "SCRAM-SHA-512"} {
		if got := saslMechanism(SASLConfig{Mechanism: m, Username: "u", Password: "p"}); got == nil || got.Name() != want {
			t.Errorf("%s: %v", m, got)
		}
	}
	if saslMechanism(SASLConfig{}) != nil {
		t.Error("no mechanism should mean no SASL")
	}
}
