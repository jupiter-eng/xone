// Package conf is the e2e service's own configuration block, Service.
//
// It is written the way example/component/conf is: the defaults are pre-filled in the struct, main
// reads it once, and C() has it everywhere afterwards. The table name, the key prefix and the
// downstream address are injected by the test through ${VAR}, and every test has its own, so they
// do not disturb each other.
package conf

import (
	"fmt"
	"regexp"
	"time"

	"github.com/jupiter-eng/xone/xconfig"
)

// Config is the business configuration
type Config struct {
	// Table is the user table's name; the order table is this plus _orders. It goes into SQL, so it
	// takes a lower-case identifier alone
	Table string `yaml:"Table"`

	// KeyPrefix is the key prefix in Redis and in the local cache
	KeyPrefix string `yaml:"KeyPrefix"`

	// Downstream is the base URL GET /proxy calls, http://127.0.0.1:9000 for instance.
	// Empty, /proxy answers 503
	Downstream string `yaml:"Downstream"`

	// SpanFile writes every span into this file as one JSON line, and writes none when empty
	SpanFile string `yaml:"SpanFile"`

	// SpanDiscard hangs a BatchSpanProcessor whose exporter counts alone and reports nothing.
	// It is for the load tests: it measures the overhead of the export pipeline the documentation
	// recommends rather than of SpanFile writing to a file synchronously
	SpanDiscard bool `yaml:"SpanDiscard"`

	// UserTTL is how long a user is cached in Redis
	UserTTL time.Duration `yaml:"UserTTL"`

	// StopTimeout is the budget for the whole shutdown, handed to xone.WithStopTimeout
	StopTimeout time.Duration `yaml:"StopTimeout"`

	// StartStall, above 0, has the warmup start hook sleep that long without looking at ctx before
	// returning successfully, to test "the framework checks ctx once more before every hook". 0 does
	// not sleep
	StartStall time.Duration `yaml:"StartStall"`

	// Drain, above 0, has the service's Start wind down for that long after xgin has returned
	// (without looking at ctx) and then touch PG and Redis once, to test "the framework waits for
	// Start to really return before it closes the rest". 0 does not wind down
	Drain time.Duration `yaml:"Drain"`

	// CronEvery, above 0, registers an @every CronEvery scheduled job named e2e-tick (see cron.go). 0 registers none
	CronEvery time.Duration `yaml:"CronEvery"`

	// CronHold is how long each e2e-tick run waits while watching ctx; once ctx is cancelled it
	// winds down for 300ms without looking at ctx again
	CronHold time.Duration `yaml:"CronHold"`

	// KafkaTopic, non-empty, is consumed with xkafka.Consume in the consumer group KafkaGroup (see kafka.go). Empty consumes nothing
	KafkaTopic string `yaml:"KafkaTopic"`

	// KafkaGroup is the consumer group, required when KafkaTopic is non-empty
	KafkaGroup string `yaml:"KafkaGroup"`

	// KafkaRetry is handed to xkafka.WithRetry; -1 writes no such option (xkafka's default applies)
	KafkaRetry int `yaml:"KafkaRetry"`

	// KafkaTimeout is handed to xkafka.WithTimeout; 0 writes no such option
	KafkaTimeout time.Duration `yaml:"KafkaTimeout"`

	// NoHTTP starts no xgin, xone.MustRun(xone.UntilSignal()): a process that only consumes
	NoHTTP bool `yaml:"NoHTTP"`
}

// DefaultConfig are the defaults
func DefaultConfig() Config {
	return Config{
		Table:       "e2e_users",
		KeyPrefix:   "e2e:",
		UserTTL:     time.Minute,
		StopTimeout: 15 * time.Second,
		KafkaRetry:  -1,
	}
}

// ident is how a table name has to look. A PG identifier stops at 63 bytes and the order table adds
// 7 more
var ident = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,55}$`)

// Validate is called once by xconfig.Unmarshal after the configuration is decoded
func (c Config) Validate() error {
	if !ident.MatchString(c.Table) {
		return fmt.Errorf("Table %q must match %s", c.Table, ident)
	}
	if c.UserTTL <= 0 {
		return fmt.Errorf("UserTTL must be > 0, got=%v", c.UserTTL)
	}
	if c.StopTimeout <= 0 {
		return fmt.Errorf("StopTimeout must be > 0, got=%v", c.StopTimeout)
	}
	if c.KafkaTopic != "" && c.KafkaGroup == "" {
		return fmt.Errorf("KafkaGroup is required when KafkaTopic is set")
	}
	if c.StartStall < 0 || c.Drain < 0 {
		return fmt.Errorf("StartStall and Drain must be >= 0, got StartStall=%v Drain=%v", c.StartStall, c.Drain)
	}
	return nil
}

// conf is the configuration in force
var conf = DefaultConfig()

// Load reads the Service block. main calls it once, and C() has it everywhere afterwards
func Load() error { return xconfig.Unmarshal("Service", &conf) }

// C returns the final configuration
func C() Config { return conf }
