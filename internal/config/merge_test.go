package config

import (
	"strings"
	"testing"
	"time"
)

// ---- An empty file: laid on top it changes nothing ----

func TestLoad_EmptyProfileFileChangesNothing(t *testing.T) {
	// An empty file and one holding only comments parse into a node whose Kind is 0, not a document
	// node. It used to miss the "document node" branch, count as a scalar and replace every file
	// before it whole -- one # TODO line in application-prod.yml quietly put the whole configuration
	// back to its defaults
	for name, body := range map[string]string{
		"an empty file":           "",
		"comments alone":          "# TODO\n",
		"blank lines alone":       "\n\n",
		"a document marker alone": "---\n",
	} {
		t.Run(name, func(t *testing.T) {
			withProfileEnv(t, "prod")
			base := files(t, "application.yml", map[string]string{
				"application.yml":      "Demo:\n  Addr: base:1\n  Timeout: 2s\n",
				"application-prod.yml": body,
			})
			c := listComps(t)
			if err := LoadInto(base, "Demo", c); err != nil {
				t.Fatal(err)
			}
			if c.Addr != "base:1" || c.Timeout != 2*time.Second {
				t.Errorf("an empty profile file must not touch base's configuration, got Addr=%q Timeout=%v", c.Addr, c.Timeout)
			}
		})
	}
}

func TestLoad_ImportOfEmptyFileChangesNothing(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import: empty.yml\nDemo:\n  Addr: base:1\n",
		"empty.yml":       "# to be filled in later\n",
	})
	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "base:1" {
		t.Errorf("importing an empty file must not touch the one importing it, Addr=%q", c.Addr)
	}
}

func TestLoad_BaseEmpty_ProfileStillApplies(t *testing.T) {
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "# everything is in the profile\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})
	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "prod:1" {
		t.Errorf("Addr=%q", c.Addr)
	}
}

// ---- A duplicate key: every file is checked, not the first alone ----

func TestLoad_DuplicateKeyInLaterFileIsError(t *testing.T) {
	// Only the first file's duplicates used to be found: while merging, a key of the same name in
	// override is deduplicated by name, and neither the strict decoding nor the top-level check
	// ever sees it again
	cases := map[string]string{
		"at the top level": "Demo:\n  Addr: prod:1\nDemo:\n  Addr: prod:2\n",
		"nested":           "Demo:\n  Addr: prod:1\n  Addr: prod:2\n",
		"deeper":           "Demo:\n  Nested:\n    A: x\n    A: y\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			withProfileEnv(t, "prod")
			base := files(t, "application.yml", map[string]string{
				"application.yml":      "Demo:\n  Addr: base:1\n",
				"application-prod.yml": body,
			})
			err := LoadInto(base, "Demo", listComps(t))
			if err == nil {
				t.Fatal("a duplicate key in a profile file must be an error")
			}
			for _, want := range []string{"duplicate", "application-prod.yml", "line"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error must hold %q (the file and the line have to be locatable), got=%v", want, err)
				}
			}
		})
	}
}

func TestLoad_ImportDuplicateKeyInImportedFileIsError(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import: shared.yml\nDemo:\n  Addr: base:1\n",
		"shared.yml":      "Demo:\n  Addr: a:1\n  Timeout: 1s\n  Addr: a:2\n",
	})
	err := LoadInto(base, "Demo", listComps(t))
	if err == nil {
		t.Fatal("a duplicate key in an imported file must be an error")
	}
	for _, want := range []string{"duplicate", "shared.yml", "line 4", "line 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must hold %q, got=%v", want, err)
		}
	}
}

func TestLoad_DuplicateKeyInUnreadBlockIsError(t *testing.T) {
	// A nested duplicate used to be found by the strict decoding, which only happens when somebody
	// Unmarshals that block
	c := comps(t)
	err := LoadInto(write(t, "Demo:\n  Retries: 1\nOther:\n  X: 1\n  X: 2\n"), "Demo", c)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("a duplicate key must be reported while loading, got=%v", err)
	}
}

// ---- null: written in an overlay file it means nothing was written ----

func TestLoad_NullInOverlayDoesNotOverrideLowerPriority(t *testing.T) {
	// Inside one file a null keeps the struct's default, and laid on top it should change nothing
	// either. The empty block `Demo:` used to replace base's mapping whole while `Demo: {}` kept
	// base -- one empty block with two spellings and two outcomes
	cases := map[string]string{
		"an empty block":   "Demo:\n",
		"an explicit null": "Demo: null\n",
		"a tilde":          "Demo: ~\n",
		"an empty mapping": "Demo: {}\n",
		"a null field":     "Demo:\n  Addr:\n",
		"a null list":      "Demo:\n  Headers: ~\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			withProfileEnv(t, "prod")
			base := files(t, "application.yml", map[string]string{
				"application.yml":      "Demo:\n  Addr: base:1\n  Headers: [a]\n",
				"application-prod.yml": body,
			})
			c := listComps(t)
			if err := LoadInto(base, "Demo", c); err != nil {
				t.Fatal(err)
			}
			if c.Addr != "base:1" || len(c.Headers) != 1 {
				t.Errorf("a null must not override base, got Addr=%q Headers=%v", c.Addr, c.Headers)
			}
		})
	}
}

func TestLoad_OverlayClearsWithEmptyValueNotNull(t *testing.T) {
	// A null is "not written"; clearing something means writing the empty value out
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Addr: base:1\n  Headers: [a]\n",
		"application-prod.yml": "Demo:\n  Addr: \"\"\n  Headers: []\n",
	})
	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "" || len(c.Headers) != 0 {
		t.Errorf("an empty string and an empty list must override base, got Addr=%q Headers=%v", c.Addr, c.Headers)
	}
}
