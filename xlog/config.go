// Package xlog provides configuration-driven logging on top of the standard library log/slog: level, format, console and file rotation.
//
// What the caller gets is a native *slog.Logger, so business code uses the standard library's slog.Info /
// slog.InfoContext directly and does not have to know this package.
package xlog

import (
	"fmt"
	"time"
)

// ConfigKey is this module's top-level key in the config file.
const ConfigKey = "XLog"

// The log formats.
const (
	FormatJSON = "json"
	FormatText = "text"
)

// Config is the logging configuration.
type Config struct {
	// Level is the log level: debug / info / warn / error.
	Level string `yaml:"Level"`

	// Format is the output format: json / text.
	Format string `yaml:"Format"`

	// Timezone is the time zone the log timestamps are rendered in, an IANA name such as Europe/Berlin.
	// Empty means the process's local time zone (usually UTC inside a container).
	//
	// Configuring one that cannot be loaded fails startup outright rather than quietly falling back to local --
	// that would mean believing the timestamps are Berlin time while they are UTC, an hour or two off with no hint.
	// An image such as scratch or distroless has no /usr/share/zoneinfo, so one line in the caller's own main package
	// is needed to compile the time zone database into the binary (about 400KB):
	//
	//	import _ "time/tzdata"
	//
	// The framework does not compile it in: anyone not using this setting should not carry those 400KB.
	Timezone string `yaml:"Timezone"`

	// AddSource says whether the source location of the log call is recorded. It costs something and is off by default.
	AddSource bool `yaml:"AddSource"`

	// Console says whether to write to standard output, for the deployment environment's log collector. It defaults to on.
	Console bool `yaml:"Console"`

	// File is the file output.
	File FileConfig `yaml:"File"`

	// Fields are static fields carried on every log line. A value may take ${VAR} from the environment, such as the
	// POD_NAME and NODE_NAME K8s injects through the Downward API; ${VAR:} is an empty string when unset, and a field with an empty string is not written.
	//
	// The framework already carries service (XApp.Name), version (XApp.Version), hostname (the host name, which is the
	// Pod name in K8s) and pid. A name written here wins, and writing an empty string drops that default field.
	// These values are computed and serialized once at startup: measured, the default 4 (with a hostname as long as a
	// Pod name) add 88 bytes per line, and neither time nor allocations are measurably different (BenchmarkHandle_WithIdentityFields against BenchmarkHandle_Bare).
	Fields map[string]string `yaml:"Fields"`
}

// FileConfig is the file output configuration.
type FileConfig struct {
	// Enable says whether to write to a file. It defaults to off.
	Enable bool `yaml:"Enable"`

	// Path is the log directory.
	Path string `yaml:"Path"`

	// Name is the log file name, and must not be empty. The actual file is {Name}.{time suffix},
	// with a symbolic link of the same name pointing at the current file, so tail can follow it.
	//
	// Empty is the one value that does not fail on its own: the writer takes the directory itself for the
	// symbolic link, and with Path empty as well it writes a hidden file named after nothing but the time
	// suffix into the working directory. So it fails startup instead.
	Name string `yaml:"Name"`

	// RotateTime is the rotation period. It defaults to one day, aligned to the local time zone. The minimum is 1 minute.
	//
	// The time suffix in the file name goes down to the minute: writing 0s would give one file per minute, and writing
	// 30s would put two periods in the same file name while still rotating once a minute -- both are "configure A and
	// get B", so anything shorter than a minute fails startup outright.
	RotateTime time.Duration `yaml:"RotateTime"`

	// MaxAge is how long historical files are kept; 0 means no cleanup. It defaults to 7 days and must not be negative.
	//
	// Cleanup runs once at startup and once per rotation after that. It only removes files named {Name}.{time suffix}
	// the way this package names them, and never touches an app.log.bak or app.log.1.gz in the same directory.
	MaxAge time.Duration `yaml:"MaxAge"`

	// Perm is the log file permission, a string parsed as octal. It defaults to "0644" and also accepts "644" and "0o644".
	//
	// It is a string rather than a number: measured, yaml.v3 parses the number 0644 as octal 420 (exactly 0o644),
	// while 644 with the leading 0 dropped is decimal 644 = 0o1204 (--w----r-T), with no error and no hint.
	// A string is always parsed as octal, with or without the leading 0; quotes are optional too,
	// since a 0644 going into a string field is still "0644".
	Perm string `yaml:"Perm"`
}

// validate checks the fields that do not depend on the file system.
//
// It ignores Enable: the defaults are valid anyway, so the only way to get an error here is an explicitly wrong value,
// and wrong is wrong -- it should not be let through just because file output happens to be off today.
func (c FileConfig) validate() error {
	// An empty Name is the one value that does not fail on its own: the writer would take the directory
	// itself for the log file, and with Path empty too it would write a hidden file named after nothing but
	// the time suffix into whatever the working directory happens to be, with no symlink and no word said.
	if c.Name == "" {
		return fmt.Errorf("File.Name must not be empty: it is the log file's name, with the time suffix appended to it (app.log gives app.log.20260918)")
	}
	if c.RotateTime < time.Minute {
		return fmt.Errorf("File.RotateTime=[%v] must be >= 1m: log file names are only precise to the minute", c.RotateTime)
	}
	if c.MaxAge < 0 {
		return fmt.Errorf("File.MaxAge=[%v] must not be negative, use 0 to keep every file", c.MaxAge)
	}
	return nil
}

// DefaultConfig holds every default in one place.
//
// A field the file does not set keeps these values, and one it sets overrides them -- including one explicitly set to a zero value (false / 0 / "").
func DefaultConfig() Config {
	return Config{
		Level:   "info",
		Format:  FormatJSON,
		Console: true,
		File: FileConfig{
			Name:       "app.log",
			RotateTime: 24 * time.Hour,
			MaxAge:     7 * 24 * time.Hour,
			Perm:       "0644",
		},
	}
}
