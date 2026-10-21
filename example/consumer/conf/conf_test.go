package conf

import (
	"strings"
	"testing"

	"github.com/jupiter-eng/xone/xonetest"
)

func TestLoad_BadValuesFailAtStartup(t *testing.T) {
	// Neither of these two produces an error on its own; they only make the service "do nothing,
	// normally"
	cases := []struct{ yml, field string }{
		{"MyApp:\n  Workers: 0\n", "Workers"},
		{"MyApp:\n  MessageTimeout: 0s\n", "MessageTimeout"},
	}
	for _, c := range cases {
		useConf(t, c.yml)
		err := Load()
		if err == nil || !strings.Contains(err.Error(), c.field) {
			t.Errorf("a misconfigured %s must fail startup and name the field, got=%v", c.field, err)
		}
	}
}

func TestLoad_DefaultsPassValidation(t *testing.T) {
	useConf(t, "MyApp:\n  Topic: orders\n")
	if err := Load(); err != nil {
		t.Fatal(err)
	}
}

// useConf installs a configuration into the global one and restores conf afterwards
func useConf(t *testing.T, yml string) {
	t.Helper()
	old := conf
	t.Cleanup(func() { conf = old })
	conf = DefaultConfig()
	xonetest.UseConfigYAML(t, yml)
}
