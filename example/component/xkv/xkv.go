// Package xkv is a complete example of "an integration of your own": a key-value store that lands
// on disk.
//
// It is not part of the framework; it is the kind of package anybody writes in their own project.
// The whole of the integration is three things, all in the "Joining the framework" section at the
// end of this file:
//
//	BeforeStart  reads the XKV block of the configuration file, builds the instance and keeps it
//	BeforeStop   closes it on exit
//	C()          takes the instance at run time
//
// Beyond that this package imports xhook and xconfig alone -- an integration does not know the
// framework itself, so releasing it as a module of its own works just as well.
package xkv

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jupiter-eng/xone/xconfig"
	"github.com/jupiter-eng/xone/xhook"
)

// ConfigKey is this module's top-level key in the configuration file
const ConfigKey = "XKV"

// Config is this module's configuration. The yaml tag on a field decides how it is written in the
// configuration file
type Config struct {
	// Path is the path of the data file
	Path string `yaml:"Path"`

	// FlushInterval is how often the changes in memory are flushed to disk. It defaults to 5s and
	// must be > 0
	FlushInterval time.Duration `yaml:"FlushInterval"`
}

// Validate is called by xconfig.Unmarshal once it has read, and once more by New.
//
// With a FlushInterval that is not greater than 0, time.NewTicker panics -- inside a background
// goroutine at that, so the process simply dies without even an error saying which field was
// misconfigured.
func (c Config) Validate() error {
	if c.FlushInterval <= 0 {
		return fmt.Errorf("FlushInterval must be > 0, got=%v", c.FlushInterval)
	}
	return nil
}

// DefaultConfig gathers every default in one place.
//
// A field the configuration file does not set keeps the value here -- which is why no pointer field
// is needed to tell "not configured" from "configured as the zero value".
func DefaultConfig() Config {
	return Config{Path: "xkv.json", FlushInterval: 5 * time.Second}
}

// Store is a key-value store that lands on disk
type Store struct {
	path string

	mu    sync.RWMutex
	items map[string]string
	dirty bool

	stop chan struct{}
	done chan struct{}
}

// Get reads one key
func (s *Store) Get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.items[key]
	return v, ok
}

// Set writes one key. Landing on disk is done in batches by the background goroutine, every
// FlushInterval
func (s *Store) Set(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[key] = value
	s.dirty = true
}

// Len returns how many keys there are right now
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.items)
}

// New is the pure constructor: it touches no global, reads no configuration file, and depends on no
// framework.
//
// This is a hard requirement the framework places on an integration (check.sh verifies it), so that
// "zero wiring" always stays the default path rather than the only one: a test calls it directly for
// a clean instance and needs no mock, and two sets of configuration have a way out too.
//
// It takes no ctx -- opening a file holds nobody up. Only a constructor that connects, retries or
// probes takes a ctx, and the signature says so faithfully.
func New(c Config) (*Store, io.Closer, error) {
	// A New called directly, without going through the configuration file, has to be caught too:
	// Unmarshal's validation does not reach here
	if err := c.Validate(); err != nil {
		return nil, nil, err
	}
	items := map[string]string{}
	switch b, err := os.ReadFile(c.Path); {
	case err == nil:
		if err := json.Unmarshal(b, &items); err != nil {
			return nil, nil, err
		}
	case !os.IsNotExist(err):
		return nil, nil, err
	}

	s := &Store{
		path:  c.Path,
		items: items,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go s.flushLoop(c.FlushInterval)
	return s, s, nil
}

// Close stops the background goroutine and flushes the last changes.
//
// On exit it is called by this package's stop hook, closeXKV; stop hooks run in the reverse order of
// startup, so there is no need to wonder here whether the database has been closed already -- this
// module minds only itself.
func (s *Store) Close() error {
	close(s.stop)
	<-s.done
	return s.flush()
}

func (s *Store) flushLoop(every time.Duration) {
	defer close(s.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			if err := s.flush(); err != nil {
				slog.Error("xkv flush failed", "error", err)
			}
		}
	}
}

func (s *Store) flush() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	b, err := json.Marshal(s.items)
	s.dirty = false
	s.mu.Unlock()
	if err == nil {
		err = s.write(b)
	}
	if err != nil {
		// The dirty flag is handed back when the write failed: otherwise nobody would ever flush this
		// batch of changes again, and even the last flush in Close would skip it as "no changes",
		// losing the data for good
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
	}
	return err
}

// write writes a temporary file and renames it: overwriting directly would leave a broken file
// behind if the process were killed halfway through the write
func (s *Store) write(b []byte) error {
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Clean(s.path))
}

// ---- Joining the framework: the code below is all of it ----

// conf is the configuration in effect. It and store are ordinary package-level variables, and how
// they are kept and taken is this package's own decision
var (
	conf  = DefaultConfig()
	store *Store
)

// init only registers and initializes nothing. The real initialization is run by the framework at
// StageClient -- the stage for "what the business depends on". Without a stage it would land in the
// default StageBusiness, in the same stage as the business hooks using it, and which came first
// would be up to the packages' initialization order.
func init() {
	xhook.BeforeStart(initXKV, xhook.At(xhook.StageClient))
	xhook.BeforeStop(closeXKV) // the stage is inherited from the start hook above
}

// initXKV reads the configuration, builds the instance and keeps it. Inside is ordinary Go code.
func initXKV(context.Context) error {
	conf = DefaultConfig()
	if err := xconfig.Unmarshal(ConfigKey, &conf); err != nil {
		return err
	}

	s, _, err := New(conf)
	if err != nil {
		return err
	}
	store = s
	return nil
}

// closeXKV finishes up: stops the background goroutine and flushes the last changes.
//
// It is never called when the start hook failed -- a stop hook runs only once the start hook it is
// paired with has succeeded, so there is no need to handle "store is still nil" here.
func closeXKV(context.Context) error { return store.Close() }

// C takes the instance
func C() *Store { return store }

// Conf returns the configuration in effect. This is only needed when the configuration is read at
// run time; export nothing like it otherwise
func Conf() Config { return conf }
