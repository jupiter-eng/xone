package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

type demoConf struct {
	Addr    string        `yaml:"Addr"`
	Timeout time.Duration `yaml:"Timeout"`
	Retries int           `yaml:"Retries"`
	Enable  bool          `yaml:"Enable"`
}

func defaults() demoConf {
	return demoConf{Addr: "127.0.0.1:5432", Timeout: 5 * time.Second, Retries: 3, Enable: true}
}

// comps builds a configuration with the defaults filled in, for LoadInto to decode into
func comps(t *testing.T) *demoConf {
	t.Helper()
	c := defaults()
	return &c
}

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_DefaultsKeptOnUnsetFields(t *testing.T) {
	c := comps(t)
	if err := LoadInto(write(t, "Demo:\n  Addr: \"10.0.0.1:1\"\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "10.0.0.1:1" {
		t.Errorf("a field that was written must be overridden, got=%q", c.Addr)
	}
	if c.Timeout != 5*time.Second || c.Retries != 3 || !c.Enable {
		t.Errorf("a field that was not written must keep its default, got=%+v", *c)
	}
}

func TestLoad_ExplicitZeroOverridesDefault(t *testing.T) {
	// This is exactly the problem the *bool pointer pattern tries to solve, and pre-filled defaults
	// have those semantics to begin with
	c := comps(t)
	if err := LoadInto(write(t, "Demo:\n  Enable: false\n  Retries: 0\n  Addr: \"\"\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	if c.Enable || c.Retries != 0 || c.Addr != "" {
		t.Errorf("a zero value written explicitly must override the default, got=%+v", *c)
	}
}

func TestLoad_DurationParsedNatively(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want time.Duration
	}{
		{"Demo:\n  Timeout: 30s\n", 30 * time.Second},
		{"Demo:\n  Timeout: \"1500ms\"\n", 1500 * time.Millisecond},
		{"Demo:\n  Timeout: 2m\n", 2 * time.Minute},
	} {
		c := comps(t)
		if err := LoadInto(write(t, tc.yaml), "Demo", c); err != nil {
			t.Fatalf("%q: %v", tc.yaml, err)
		}
		if c.Timeout != tc.want {
			t.Errorf("%q -> %v, want %v", tc.yaml, c.Timeout, tc.want)
		}
	}
}

func TestLoad_BadDurationFormatFailsFast(t *testing.T) {
	c := comps(t)
	err := LoadInto(write(t, "Demo:\n  Timeout: that long\n"), "Demo", &c)
	if err == nil {
		t.Fatal("a malformed duration must fail startup rather than quietly becoming 0")
	}
}

func TestLoad_MisspelledFieldIsError(t *testing.T) {
	c := comps(t)
	err := LoadInto(write(t, "Demo:\n  Adrr: \"x\"\n"), "Demo", &c)
	if err == nil {
		t.Fatal("a misspelled field must be an error rather than being ignored")
	}
	if !strings.Contains(err.Error(), "Demo") {
		t.Errorf("the error must name the block the problem is in, got=%v", err)
	}
}

func TestUnclaimed_ReportsBlocksNobodyRead(t *testing.T) {
	// Reading is what each integration does in its own start hook, so "nobody wants it" is only
	// known once every hook has run. The framework reports it then, which covers both "a misspelled
	// key" and "a forgotten import"
	c := comps(t)
	if err := LoadInto(write(t, "Demo:\n  Addr: x\nXRedis:\n  Addr: y\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	got := Unclaimed()
	if len(got) != 1 || got[0] != "XRedis" {
		t.Errorf("the unread XRedis alone must be reported, got=%v", got)
	}
}

func TestLoad_Placeholder(t *testing.T) {
	t.Run("the environment variable is set", func(t *testing.T) {
		t.Setenv("XONE_T_ADDR", "10.1.1.1:9")
		c := comps(t)
		if err := LoadInto(write(t, "Demo:\n  Addr: \"${XONE_T_ADDR}\"\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if c.Addr != "10.1.1.1:9" {
			t.Errorf("got=%q", c.Addr)
		}
	})

	t.Run("unset, the default is used", func(t *testing.T) {
		os.Unsetenv("XONE_T_UNSET")
		c := comps(t)
		if err := LoadInto(write(t, "Demo:\n  Addr: \"${XONE_T_UNSET:1.2.3.4:5}\"\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if c.Addr != "1.2.3.4:5" {
			t.Errorf("a default holding a colon must be kept whole, got=%q", c.Addr)
		}
	})

	t.Run("a required one unset fails startup", func(t *testing.T) {
		os.Unsetenv("XONE_T_REQUIRED")
		c := comps(t)
		err := LoadInto(write(t, "Demo:\n  Addr: \"${XONE_T_REQUIRED}\"\n"), "Demo", &c)
		if err == nil || !strings.Contains(err.Error(), "XONE_T_REQUIRED") {
			t.Fatalf("a missing required placeholder must be an error naming the variable, got=%v", err)
		}
	})

	t.Run("an explicit empty string overrides the default", func(t *testing.T) {
		t.Setenv("XONE_T_EMPTY", "")
		c := comps(t)
		if err := LoadInto(write(t, "Demo:\n  Addr: \"${XONE_T_EMPTY:fallback}\"\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if c.Addr != "" {
			t.Errorf("an empty string set explicitly is a valid value and must override the default, got=%q", c.Addr)
		}
	})
}

func TestLoad_PlaceholderFillsNonStringField(t *testing.T) {
	// The documentation presents ${VAR} as a general rule, so it has to hold for every field type.
	// While parsing, the whole ${PORT:8080} is one piece of text and the scalar is tagged !!str;
	// without deciding the type again after the replacement, every such field would fail with
	// "cannot unmarshal !!str into int" and placeholders would be for string fields alone.
	type demo struct {
		Retries int           `yaml:"Retries"`
		Enable  bool          `yaml:"Enable"`
		Ratio   float64       `yaml:"Ratio"`
		Timeout time.Duration `yaml:"Timeout"`
		Addr    string        `yaml:"Addr"`
	}

	t.Run("using the default", func(t *testing.T) {
		os.Unsetenv("XONE_T_N1")
		c := demo{}
		body := "Demo:\n  Retries: ${XONE_T_N1:7}\n  Enable: ${XONE_T_N1:true}\n" +
			"  Ratio: ${XONE_T_N1:0.25}\n  Timeout: ${XONE_T_N1:90s}\n  Addr: ${XONE_T_N1:h:1}\n"
		if err := LoadInto(write(t, body), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		want := demo{Retries: 7, Enable: true, Ratio: 0.25, Timeout: 90 * time.Second, Addr: "h:1"}
		if c != want {
			t.Errorf("got=%+v want=%+v", c, want)
		}
	})

	t.Run("taking the environment variable", func(t *testing.T) {
		t.Setenv("XONE_T_N2", "42")
		c := demo{}
		if err := LoadInto(write(t, "Demo:\n  Retries: ${XONE_T_N2}\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if c.Retries != 42 {
			t.Errorf("Retries must come from the environment variable, got=%d", c.Retries)
		}
	})

	t.Run("a bare number in a duration field is still an error", func(t *testing.T) {
		// Deciding the type again must not casually drop this promise: ${T:30} is the same thing as
		// writing 30, and whoever wrote it wanted 30 seconds, not 30 nanoseconds
		c := demo{}
		if err := LoadInto(write(t, "Demo:\n  Timeout: ${XONE_T_N3:30}\n"), "Demo", &c); err == nil {
			t.Fatal("a bare number inside a placeholder must be an error just the same")
		}
	})

	t.Run("quotes keep it a string", func(t *testing.T) {
		// A numeric-looking password or version depends on this: explicit quotes are an explicit
		// intent and must not be decided again
		t.Setenv("XONE_T_N4", "0123456")
		c := demo{}
		if err := LoadInto(write(t, "Demo:\n  Addr: \"${XONE_T_N4}\"\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if c.Addr != "0123456" {
			t.Errorf("a quoted value must reach a string field as it is, got=%q", c.Addr)
		}
	})

	t.Run("an unquoted number in a string field keeps its leading zero", func(t *testing.T) {
		t.Setenv("XONE_T_N5", "0123456")
		c := demo{}
		if err := LoadInto(write(t, "Demo:\n  Addr: ${XONE_T_N5}\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if c.Addr != "0123456" {
			t.Errorf("deciding the type again must not change what a string field reads, got=%q", c.Addr)
		}
	})

	t.Run("an empty value is not decided again", func(t *testing.T) {
		// Decided again, "" would become null and clear the default pre-filled in the struct to the
		// zero value
		t.Setenv("XONE_T_N6", "")
		c := demo{Retries: 3}
		if err := LoadInto(write(t, "Demo:\n  Addr: ${XONE_T_N6}\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if c.Addr != "" || c.Retries != 3 {
			t.Errorf("got=%+v", c)
		}
	})
}

func TestLoad_PlaceholderInMapAndSliceTypedByExpandedContent(t *testing.T) {
	// The expansion happens over the whole node tree, so a map's values, a slice's elements and a
	// nested struct all have to come out right.
	// map[string]string above all: deciding the type again must not turn "123" into something a
	// string cannot read
	os.Unsetenv("XONE_T_C1")
	type demo struct {
		Labels  map[string]string `yaml:"Labels"`
		Names   []string          `yaml:"Names"`
		Buckets []float64         `yaml:"Buckets"`
		Nested  struct {
			Max int `yaml:"Max"`
		} `yaml:"Nested"`
	}

	body := "Demo:\n" +
		"  Labels:\n    env: ${XONE_T_C1:dev}\n    ver: ${XONE_T_C1:123}\n    on: ${XONE_T_C1:true}\n" +
		"  Names:\n    - ${XONE_T_C1:a}\n    - ${XONE_T_C1:7}\n" +
		"  Buckets:\n    - ${XONE_T_C1:0.1}\n    - ${XONE_T_C1:1}\n" +
		"  Nested:\n    Max: ${XONE_T_C1:9}\n"

	c := demo{}
	if err := LoadInto(write(t, body), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"env": "dev", "ver": "123", "on": "true"}
	if !reflect.DeepEqual(c.Labels, want) {
		t.Errorf("a map's string value must stay text as it is, got=%v", c.Labels)
	}
	if !reflect.DeepEqual(c.Names, []string{"a", "7"}) {
		t.Errorf("a slice of strings, got=%v", c.Names)
	}
	if !reflect.DeepEqual(c.Buckets, []float64{0.1, 1}) {
		t.Errorf("a slice of numbers, got=%v", c.Buckets)
	}
	if c.Nested.Max != 9 {
		t.Errorf("a nested struct, got=%d", c.Nested.Max)
	}
}

func TestLoad_PlaceholderSpecialCharsDoNotBreakStructure(t *testing.T) {
	// Expanded on the parsed nodes rather than by replacing text in the raw bytes -- otherwise this
	// would be an injection path
	t.Setenv("XONE_T_INJECT", "a: b\nEvil: true")
	c := comps(t)
	if err := LoadInto(write(t, "Demo:\n  Addr: \"${XONE_T_INJECT}\"\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "a: b\nEvil: true" {
		t.Errorf("an environment variable's value must reach the field as it is and must not be parsed as YAML, got=%q", c.Addr)
	}

	// Without quotes the scalar's type is decided again, and that step must not let the value
	// escape the scalar either
	c2 := comps(t)
	if err := LoadInto(write(t, "Demo:\n  Addr: ${XONE_T_INJECT}\n"), "Demo", c2); err != nil {
		t.Fatal(err)
	}
	if c2.Addr != "a: b\nEvil: true" {
		t.Errorf("it must still be one scalar after the type is decided again, got=%q", c2.Addr)
	}
}

func TestLoad_EmptyFileUsesAllDefaults(t *testing.T) {
	c := comps(t)
	if err := LoadInto(write(t, ""), "Demo", c); err != nil {
		t.Fatal(err)
	}
	if *c != defaults() {
		t.Errorf("an empty file must keep every default, got=%+v", *c)
	}
}

func TestLoad_MissingBlockKeepsDefaults(t *testing.T) {
	c := comps(t)
	if err := LoadInto(write(t, "Demo:\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	if *c != defaults() {
		t.Errorf("an empty block must keep the defaults, got=%+v", *c)
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	c := comps(t)
	if err := LoadInto(filepath.Join(t.TempDir(), "nope.yml"), "Demo", c); err == nil {
		t.Fatal("a file that does not exist must be an error")
	}
}

func TestLoad_DurationFields(t *testing.T) {
	// Every module's timeouts and periods use time.Duration directly, which rests on yaml.v3
	// recognizing duration strings natively.
	// That is an assumption spanning every module and is pinned here: changing the dependency blows
	// up here first, rather than some module's timeout quietly becoming 0
	type demo struct {
		Timeout time.Duration `yaml:"Timeout"`
		Rotate  time.Duration `yaml:"Rotate"`
	}

	t.Run("duration strings are recognized", func(t *testing.T) {
		c := demo{Rotate: 24 * time.Hour} // the pre-filled default
		if err := LoadInto(write(t, "Demo:\n  Timeout: 1h30m\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if c.Timeout != 90*time.Minute {
			t.Errorf("Timeout must be 90m, got=%v", c.Timeout)
		}
		if c.Rotate != 24*time.Hour {
			t.Errorf("an unconfigured field must keep its default, got=%v", c.Rotate)
		}
	})

	t.Run("a bare number is an error", func(t *testing.T) {
		// Whoever writes Timeout: 30 wants 30 seconds, and Go's semantics would give them 30
		// nanoseconds. Failing at startup beats a timeout that means nothing in production
		c := demo{}
		err := LoadInto(write(t, "Demo:\n  Timeout: 30\n"), "Demo", &c)
		if err == nil {
			t.Fatal("a bare number must be an error asking for a unit")
		}
		if !strings.Contains(err.Error(), "Demo") {
			t.Errorf("the error must name the configuration block, got=%v", err)
		}
	})
}

func TestLoad_UnclaimedTopLevelKeyIsError(t *testing.T) {
	// Most likely it is misspelled, or the matching integration was never imported.
	// Ignored quietly, the caller spends a long time staring at a file that "clearly has it
	// configured"
	type demo struct {
		Addr string `yaml:"Addr"`
	}
	c := demo{}

	if err := LoadInto(write(t, "Demo:\n  Addr: a\nXGrom:\n  DSN: x\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	// Reading is what each integration does in its own start hook, so this is only known once every
	// hook has run, and the framework checks Unclaimed then
	got := Unclaimed()
	if len(got) != 1 || got[0] != "XGrom" {
		t.Errorf("a misspelled top-level key must be reported, got=%v", got)
	}
}

func TestLoad_DefaultOverrideSemantics(t *testing.T) {
	// The defaults are pre-filled in the structs, so "what happens when the file writes it" has to
	// be pinned down. The two kinds of container behave differently, and whoever writes a default
	// has to know that
	type demo struct {
		Buckets []float64         `yaml:"Buckets"`
		Labels  map[string]string `yaml:"Labels"`
	}
	fresh := func() demo {
		return demo{
			Buckets: []float64{1, 2, 3},
			Labels:  map[string]string{"pre": "filled"},
		}
	}

	t.Run("a slice is replaced whole", func(t *testing.T) {
		c := fresh()
		if err := LoadInto(write(t, "Demo:\n  Buckets: [0.1, 1]\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(c.Buckets, []float64{0.1, 1}) {
			t.Errorf("one that is configured must be replaced whole and must not blend into the default, got=%v", c.Buckets)
		}
	})

	t.Run("a map is merged rather than replaced", func(t *testing.T) {
		// Which is why a map field must have no pre-filled default: a caller cannot delete a
		// pre-filled entry.
		// It is pinned here so that nobody gives something like ConstLabels a "sensible default" one
		// day
		c := fresh()
		if err := LoadInto(write(t, "Demo:\n  Labels:\n    a: \"1\"\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if c.Labels["pre"] != "filled" {
			t.Errorf("a map's default is never overridden, which is exactly why a map gets no pre-filled default, got=%v", c.Labels)
		}
	})

	t.Run("an unconfigured field keeps its default", func(t *testing.T) {
		c := fresh()
		if err := LoadInto(write(t, "Demo: {}\n"), "Demo", &c); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(c.Buckets, []float64{1, 2, 3}) {
			t.Errorf("one that is unconfigured must keep its default, got=%v", c.Buckets)
		}
	})
}

func TestLoad_DuplicateTopLevelKeyIsError(t *testing.T) {
	// The moment it becomes a map the duplicate is gone and the strict decoding never sees it again
	// -- so the same block written twice loads fine, quietly going by the second one
	c := comps(t)
	err := LoadInto(write(t, "Demo:\n  Retries: 1\nOther: {}\nDemo:\n  Retries: 2\n"), "Demo", &c)
	if err == nil {
		t.Fatalf("a duplicate top-level key must be an error, it decoded Retries=%d", c.Retries)
	}
	for _, want := range []string{"Demo", "duplicate", "line 4", "line 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must hold %q (the line has to be locatable), got=%v", want, err)
		}
	}
}

// ---- Has: what "initialize it only when it is configured" goes by ----

func TestHas_DistinguishesSetFromUnset(t *testing.T) {
	// Decided wrong once, an optional component either builds a set for nothing or does not come up
	// although it was configured
	Reset()
	t.Cleanup(Reset)
	if err := Load(write(t, "XRedis:\n  Addr: 127.0.0.1:6379\n")); err != nil {
		t.Fatal(err)
	}

	if !Has("XRedis") {
		t.Error("a block that was written must be reported as there")
	}
	if Has("XGorm") {
		t.Error("a block that was not written must not be reported as there")
	}
}

func TestHas_EmptyBlockCountsAsUnset(t *testing.T) {
	// "XCache:" with nothing after it is "I am holding the place", not "build me a set on the
	// defaults"
	Reset()
	t.Cleanup(Reset)
	if err := Load(write(t, "XCache:\nXRedis:\n  Addr: 127.0.0.1:6379\n")); err != nil {
		t.Fatal(err)
	}

	if Has("XCache") {
		t.Error("an empty block must not have an optional component build a set for nothing")
	}
	if !Has("XRedis") {
		t.Error("a block really written in the same file must still be reported as there")
	}
}

func TestHas_LoadsFirstIfNotLoaded(t *testing.T) {
	// An optional component decides whether to build by Has. Reporting none before anything is
	// loaded would have a component that asked once before Run believe it is unconfigured and stay
	// quietly absent from then on
	Reset()
	t.Cleanup(Reset)
	t.Setenv(EnvKey, write(t, "XRedis:\n  Addr: x\n"))

	if !Has("XRedis") {
		t.Error("the first question must load first rather than report none")
	}
}

// ---- Unclaimed's remaining semantics ----

func TestUnclaimed_SortedByName(t *testing.T) {
	// A stable order is what makes the error text reproducible
	c := comps(t)
	if err := LoadInto(write(t, "Demo:\n  Addr: x\nXTypo:\n  Foo: 1\nXAlso:\n  Bar: 2\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	if got := Unclaimed(); len(got) != 2 || got[0] != "XAlso" || got[1] != "XTypo" {
		t.Errorf("want [XAlso XTypo], got=%v", got)
	}
}

func TestUnclaimed_ReadingAbsentBlockCountsAsClaim(t *testing.T) {
	// An optional component always "reads first and looks afterwards", and such a read must not
	// drag another block into counting as unclaimed
	c := comps(t)
	if err := LoadInto(write(t, "Demo:\n  Addr: x\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	if err := Unmarshal("XRedis", &c); err != nil {
		t.Fatal(err)
	}
	if got := Unclaimed(); len(got) != 0 {
		t.Errorf("want empty, got=%v", got)
	}
}

func TestUnclaimed_EmptyBeforeLoad(t *testing.T) {
	Reset()
	if got := Unclaimed(); len(got) != 0 {
		t.Errorf("want empty, got=%v", got)
	}
}

func TestLoad_EachLoadResetsClaims(t *testing.T) {
	// Running a second time in the same process must leave no claims behind from the previous round
	// -- that would miss a block this round really did not read
	c := comps(t)
	if err := LoadInto(write(t, "Demo:\n  Addr: x\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}

	Reset()
	t.Cleanup(Reset)
	if err := Load(write(t, "Demo:\n  Addr: y\n")); err != nil {
		t.Fatal(err)
	}
	if got := Unclaimed(); len(got) != 1 || got[0] != "Demo" {
		t.Errorf("the previous round's claims must not reach this one, got=%v", got)
	}
}

func TestUnmarshal_EmptyBlockKeepsDefaults(t *testing.T) {
	c := comps(t)
	want := c.Addr
	if err := LoadInto(write(t, "Demo:\n"), "Demo", &c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != want {
		t.Errorf("an empty block must not wipe the defaults out, got=%q", c.Addr)
	}
}

func TestHas_AskingCountsAsClaim(t *testing.T) {
	// An empty block such as "XRedis:" used to fail startup outright: Has returned false, the
	// package skipped it and never Unmarshalled, so the key was claimed by nobody. And neither
	// reason Unclaimed reports (a misspelling, a forgotten import) applied, so following them found
	// nothing
	Reset()
	t.Cleanup(Reset)
	if err := Load(write(t, "XRedis:\n")); err != nil {
		t.Fatal(err)
	}

	if Has("XRedis") {
		t.Fatal("an empty block must be reported as unconfigured")
	}
	if got := Unclaimed(); len(got) != 0 {
		t.Errorf("asking counts as wanting it and it must no longer be reported as unclaimed, got=%v", got)
	}
}

func TestHas_AskingNonEmptyBlockCountsAsClaim(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	if err := Load(write(t, "XRedis:\n  Addr: a\n")); err != nil {
		t.Fatal(err)
	}

	if !Has("XRedis") {
		t.Fatal("a block really written must be reported as there")
	}
	if got := Unclaimed(); len(got) != 0 {
		t.Errorf("want empty, got=%v", got)
	}
}

func TestLoad_PlaceholderExpandingToEmptyKeepsDefault(t *testing.T) {
	// Expanding to empty means the field was not written: a non-string field used to fail with
	// cannot unmarshal !!str "" into int, and a string field was cleared to an empty string
	os.Unsetenv("XONE_T_EMPTY")
	t.Setenv("XONE_T_BLANK", "")
	for name, v := range map[string]string{
		"an empty default":              "${XONE_T_EMPTY:}",
		"an empty environment variable": "${XONE_T_BLANK}",
	} {
		t.Run(name, func(t *testing.T) {
			body := "Demo:\n  Addr: " + v + "\n  Timeout: " + v + "\n  Retries: " + v + "\n  Enable: " + v + "\n"
			c := comps(t)
			if err := LoadInto(write(t, body), "Demo", c); err != nil {
				t.Fatal(err)
			}
			if *c != defaults() {
				t.Errorf("a field expanding to empty must keep its default, got=%+v", *c)
			}
		})
	}
}

func TestLoad_QuotedEmptyPlaceholderIsEmptyString(t *testing.T) {
	// Quotes are an explicit "treat it as a string", and an empty string that is really wanted is
	// written that way
	os.Unsetenv("XONE_T_EMPTY")
	c := comps(t)
	if err := LoadInto(write(t, "Demo:\n  Addr: \"${XONE_T_EMPTY:}\"\n"), "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "" {
		t.Errorf("Addr=%q", c.Addr)
	}
}

func TestLoad_AnchorsWorkAcrossTopLevelBlocks(t *testing.T) {
	// Each block is decoded separately, and an alias pointing at an anchor inside another block used
	// to report unknown anchor
	body := "Shared: &timeouts\n  Timeout: 7s\n  Retries: 9\n" +
		"Demo:\n  <<: *timeouts\n  Addr: h:1\n" +
		"Other:\n  Addr: &addr o:1\n" +
		"Third:\n  Addr: *addr\n"
	c := comps(t)
	if err := LoadInto(write(t, body), "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Timeout != 7*time.Second || c.Retries != 9 || c.Addr != "h:1" {
		t.Errorf("got=%+v", *c)
	}
	var third demoConf
	if err := Unmarshal("Third", &third); err != nil {
		t.Fatal(err)
	}
	if third.Addr != "o:1" {
		t.Errorf("a scalar alias must resolve to the anchor's value, got=%q", third.Addr)
	}
}

func TestLoad_AnchorPlaceholdersExpandAtEveryAlias(t *testing.T) {
	t.Setenv("XONE_T_ANCHOR", "env:1")
	body := "Base: &b\n  Addr: ${XONE_T_ANCHOR}\nDemo: *b\n"
	c := comps(t)
	if err := LoadInto(write(t, body), "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "env:1" {
		t.Errorf("Addr=%q", c.Addr)
	}
}

func TestLoad_ProfileFileCanOverrideAliasExpandedFields(t *testing.T) {
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Base: &b\n  Addr: base:1\n  Retries: 5\nDemo: *b\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})
	c := comps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "prod:1" || c.Retries != 5 {
		t.Errorf("got=%+v", *c)
	}
}

func TestLoad_TooManyAliasExpansionsIsError(t *testing.T) {
	// Aliases are expanded into copies while loading, and a deliberately built "billion laughs"
	// would eat the memory up
	var b strings.Builder
	b.WriteString("L0: &l0 [x, x, x, x, x, x, x, x, x, x]\n")
	for i := 1; i <= 8; i++ {
		fmt.Fprintf(&b, "L%d: &l%d [", i, i)
		for j := 0; j < 10; j++ {
			if j > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "*l%d", i-1)
		}
		b.WriteString("]\n")
	}
	err := LoadInto(write(t, b.String()), "Demo", comps(t))
	if err == nil || !strings.Contains(err.Error(), "alias") {
		t.Fatalf("the size after expansion must have an upper bound, got=%v", err)
	}
}

func TestDecodeStrict_MissingTagErrorExplainsFix(t *testing.T) {
	// yaml.v3 recognizes an untagged field only in all lowercase: writing Endpoint as the field is
	// named reports "field Endpoint not found" -- while the field is called exactly that. A nested
	// one has to get the hint as well
	type inner struct{ Host string }
	var c struct {
		Endpoint string
		Items    map[string]inner `yaml:"Items"`
	}
	for _, src := range []string{"Endpoint: a", "Items: {a: {Host: h}}"} {
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(src), &n); err != nil {
			t.Fatal(err)
		}
		err := DecodeStrict(n.Content[0], &c)
		if err == nil {
			t.Fatalf("%s: an untagged field written as the field is named is not recognized by yaml.v3 to begin with", src)
		}
		var te *yaml.TypeError
		if !errors.As(err, &te) {
			t.Errorf("%s: the original yaml error has to be kept with %%w, got=%v", src, err)
		}
		if !strings.Contains(err.Error(), "has no yaml tag") || !strings.Contains(err.Error(), "`yaml:") {
			t.Errorf("%s: the error must say the field has no tag and how to write it, got=%v", src, err)
		}
	}
}

func TestDecodeStrict_KeyOneLevelTooHighPointsToItsBlock(t *testing.T) {
	// Once a field has moved from the top level into a block (xgin's CertFile into TLS:), a caller
	// writing it the old way should see where it moved to rather than only "no such field". With two
	// blocks holding a field of the same name, nothing is guessed at
	type tlsBlock struct {
		CertFile string `yaml:"CertFile"`
		CAFile   string `yaml:"CAFile"`
	}
	type other struct {
		CAFile string `yaml:"CAFile"`
	}
	var c struct {
		Port  int      `yaml:"Port"`
		TLS   tlsBlock `yaml:"TLS"`
		Proxy *other   `yaml:"Proxy"`
	}
	for src, want := range map[string]string{
		"CertFile: a": "field CertFile not found in type struct", // the prefix is unchanged
		"Nope: a":     "field Nope not found in type struct",
		"CAFile: a":   "field CAFile not found in type struct",
	} {
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(src), &n); err != nil {
			t.Fatal(err)
		}
		err := DecodeStrict(n.Content[0], &c)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: a field that is not recognized must be reported, got=%v", src, err)
		}
		hinted := strings.Contains(err.Error(), "did you mean")
		switch src {
		case "CertFile: a":
			if !strings.Contains(err.Error(), "(did you mean TLS.CertFile? move it under TLS:)") {
				t.Errorf("%s: it must point at moving it under TLS:, got=%v", src, err)
			}
		default:
			if hinted {
				t.Errorf("%s: with no block holding it, or more than one, nothing may be guessed at, got=%v", src, err)
			}
		}
	}
}

func TestLoad_PlaceholderNullLiteralIsNotTreatedAsUnset(t *testing.T) {
	// When a variable's value happens to be null / ~, deciding again takes it as "this field was not
	// written": the field quietly stays at its default, startup looks perfectly fine, and the value
	// that was configured never took effect
	for _, v := range []string{"null", "~", "Null", "NULL"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("XONE_T_NULLISH", v)

			c := comps(t)
			if err := LoadInto(write(t, "Demo:\n  Addr: ${XONE_T_NULLISH}\n"), "Demo", c); err != nil {
				t.Fatal(err)
			}
			if c.Addr != v {
				t.Errorf("a string field must get the literal %q, got=%q", v, c.Addr)
			}

			c = comps(t)
			err := LoadInto(write(t, "Demo:\n  Retries: ${XONE_T_NULLISH}\n"), "Demo", c)
			var te *yaml.TypeError
			if !errors.As(err, &te) {
				t.Errorf("a non-string field must report a type error rather than quietly keeping its default, got err=%v Retries=%d", err, c.Retries)
			}
		})
	}
}

// secretElem is a collection element with an UnmarshalYAML of its own: it goes through the nested
// DecodeStrict
type secretElem struct {
	N int `yaml:"N"`
}

func (e *secretElem) UnmarshalYAML(n *yaml.Node) error {
	type raw secretElem
	return DecodeStrict(n, (*raw)(e))
}

func TestUnmarshal_ExpandedPlaceholderValueNotInError(t *testing.T) {
	// A yaml type error carries the first few characters of the value. ${VAR} is the recommended
	// spelling for a credential, and a password written into the wrong field is reported as
	// cannot unmarshal !!str `hunter2...` into int
	t.Setenv("XONE_T_SECRET", "hunter2-very-secret")
	t.Setenv("XONE_T_SHORT", "s3cr3t")
	type conf struct {
		Retries int                   `yaml:"Retries"`
		Enable  bool                  `yaml:"Enable"`
		Items   map[string]secretElem `yaml:"Items"`
	}
	for name, body := range map[string]string{
		"a long value":          "Demo:\n  Retries: ${XONE_T_SECRET}\n",
		"a short value":         "Demo:\n  Enable: ${XONE_T_SHORT}\n",
		"joined onto a literal": "Demo:\n  Retries: pre-${XONE_T_SHORT}\n",
		"a nested element":      "Demo:\n  Items:\n    a:\n      N: ${XONE_T_SECRET}\n",
	} {
		t.Run(name, func(t *testing.T) {
			var c conf
			err := LoadInto(write(t, body), "Demo", &c)
			if err == nil {
				t.Fatal("the wrong type must be an error")
			}
			for _, leak := range []string{"hunter2", "s3cr3t"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("the error must hold no expanded value %q, got=%v", leak, err)
				}
			}
			if !strings.Contains(err.Error(), "${XONE_T_") {
				t.Errorf("it must be replaced with the placeholder as written, so the field is visible, got=%v", err)
			}
			var te *yaml.TypeError
			if !errors.As(err, &te) {
				t.Errorf("errors.As must still reach yaml's type error, got=%v", err)
			}
		})
	}
}

func TestDecodeStrict_NoTagHintOnRealTypo(t *testing.T) {
	var c struct {
		Endpoint string `yaml:"Endpoint"`
	}
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("Endpont: a"), &n); err != nil {
		t.Fatal(err)
	}
	err := DecodeStrict(n.Content[0], &c)
	if err == nil || strings.Contains(err.Error(), "yaml tag") {
		t.Errorf("a misspelled field must be reported as a misspelling with no hint about a tag, got=%v", err)
	}
}

func TestLoad_BadFieldReportsFileAndLine(t *testing.T) {
	// Strict decoding used to serialize the node back to text and decode that, so the line number
	// yaml reported was the line inside that text: a misspelling on line 6 was reported as line 2,
	// with no file named, and one inside a profile file was impossible to find at all
	type conf struct {
		Addr    string                `yaml:"Addr"`
		Retries int                   `yaml:"Retries"`
		Items   map[string]secretElem `yaml:"Items"`
	}
	for _, c := range []struct {
		name    string
		profile string            // the active profile; empty means base alone
		files   map[string]string // application.yml is always there
		file    string            // the file the error must name
		want    string            // the ":<line>: <reason>" the error must hold
	}{
		{"a misspelling in the base file", "", map[string]string{
			"application.yml": "# a comment\nOther:\n  X: 1\nDemo:\n  Addr: a\n  Adrr: b\n",
		}, "application.yml", ":6: field Adrr not found"},
		{"the wrong type", "", map[string]string{
			"application.yml": "Demo:\n\n  Retries: abc\n",
		}, "application.yml", ":3: cannot unmarshal"},
		{"a misspelling in a profile file", "prod", map[string]string{
			"application.yml":      "Demo:\n  Addr: a\n  Retries: 1\n",
			"application-prod.yml": "\n\nDemo:\n  Retries: 2\n  Retires: 3\n",
		}, "application-prod.yml", ":5: field Retires not found"},
		{"the wrong type in a profile file", "prod", map[string]string{
			"application.yml":      "Demo:\n  Addr: a\n",
			"application-prod.yml": "Demo:\n  Retries: x\n",
		}, "application-prod.yml", ":2: cannot unmarshal"},
		{"a block merged from two files is itself the wrong type", "prod", map[string]string{
			"application.yml":      "Demo:\n\n  Addr:\n    a: 1\n",
			"application-prod.yml": "Demo:\n  Addr:\n    b: 2\n",
		}, "application.yml", ":4: cannot unmarshal !!map"},
		{"a misspelling in an imported file", "", map[string]string{
			"application.yml": "XApp:\n  Import: part.yml\nDemo:\n  Addr: a\n",
			"part.yml":        "Demo:\n  Adrr: b\n",
		}, "part.yml", ":2: field Adrr not found"},
		{"the one nested inside an UnmarshalYAML", "", map[string]string{
			"application.yml": "Demo:\n  Items:\n    a:\n      N: 1\n    b:\n      M: 2\n",
		}, "application.yml", ":6: field M not found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.profile != "" {
				withProfileEnv(t, c.profile)
			}
			base := files(t, "application.yml", c.files)
			var got conf
			err := LoadInto(base, "Demo", &got)
			if err == nil {
				t.Fatal("something written wrong must be an error")
			}
			want := filepath.Join(filepath.Dir(base), c.file) + c.want
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error must name the file and the line %q, got=%v", want, err)
			}
			var te *yaml.TypeError
			if !errors.As(err, &te) {
				t.Errorf("errors.As must still reach yaml's type error, got=%v", err)
			}
		})
	}
}

func TestDecodeStrict_NonFileNodeReportsItsOwnLine(t *testing.T) {
	// With no file to name, the line number still has to be that of the node the caller holds
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("a: 1\nDemo:\n  Addr: x\n\n  Adrr: y\n"), &n); err != nil {
		t.Fatal(err)
	}
	var c demoConf
	err := DecodeStrict(n.Content[0].Content[3], &c)
	if err == nil || !strings.Contains(err.Error(), "line 5: field Adrr not found") {
		t.Errorf("the original node's line 5 must be reported, got=%v", err)
	}
}

func TestLoad_CollectionElementErrorsReportedWithOthers(t *testing.T) {
	// An element with an UnmarshalYAML of its own is tried during the checking pass rather than
	// decoded only at the end: otherwise one mistake outside would hide the one inside the element
	// until it is fixed and the process restarted
	type conf struct {
		Addr  string                `yaml:"Addr"`
		Items map[string]secretElem `yaml:"Items"`
	}
	var got conf
	err := LoadInto(write(t, "Demo:\n  Adrr: a\n  Items:\n    a:\n      M: 1\n"), "Demo", &got)
	for _, want := range []string{":2: field Adrr not found", ":5: field M not found"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("both must be reported, %q is missing, got=%v", want, err)
		}
	}
}

func TestDecodeStrict_FieldRulesMatchYAML(t *testing.T) {
	// Unknown fields are looked up by type here, and the rules for recognizing a field have to match
	// yaml.v3's exactly: one too many lets a misspelling through, and one too few keeps a valid
	// configuration from starting
	type base struct {
		Host string `yaml:"Host"`
	}
	type conf struct {
		base  `yaml:",inline"`
		Ptr   *base            `yaml:"Ptr"`
		List  []base           `yaml:"List"`
		Skip  string           `yaml:"-"`
		Any   any              `yaml:"Any"`
		Extra map[string]int   `yaml:",inline"`
		Named map[string]*base `yaml:"Named"`
	}
	ok := "Host: h\nPtr: {Host: p}\nList: [{Host: l}]\nAny: {whatever: 1}\nNamed: {a: {Host: n}}\nother: 7\n"
	bad := map[string]string{
		"a misspelling after an embedded struct is flattened": "Host: h\nHots: x\n",
		"a misspelling inside a pointer":                      "Ptr: {Hots: p}\n",
		"a misspelling inside a slice element":                "List: [{Hots: l}]\n",
		"a misspelling inside a map value":                    "Named: {a: {Hots: n}}\n",
		"a field written with -":                              "Skip: x\n",
		"the wrong value type for an inline map":              "other: abc\n",
	}
	decode := func(src string) (conf, error) {
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(src), &n); err != nil {
			t.Fatal(err)
		}
		var c conf
		return c, DecodeStrict(n.Content[0], &c)
	}
	c, err := decode(ok)
	if err != nil {
		t.Fatalf("a valid configuration must not be an error: %v", err)
	}
	if c.Host != "h" || c.Ptr.Host != "p" || c.List[0].Host != "l" || c.Named["a"].Host != "n" || c.Extra["other"] != 7 {
		t.Errorf("got=%+v", c)
	}
	for name, src := range bad {
		if _, err := decode(src); err == nil {
			t.Errorf("%s: this must be an error", name)
		}
	}
}

func TestLoad_MultipleDocumentsIsError(t *testing.T) {
	// Only the first document used to be read, with everything after the --- dropped quietly and
	// invisible even to Unclaimed
	for _, body := range []string{
		"Demo:\n  Addr: a:1\n---\nDemo:\n  Addr: b:1\n",
		"Demo:\n  Addr: a:1\n---\nOther: 1\n---\n",
		"---\n---\nDemo:\n  Addr: a:1\n---\nDemo:\n  Addr: b:1\n",
	} {
		err := LoadInto(write(t, body), "Demo", comps(t))
		if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") ||
			!strings.Contains(err.Error(), "application-{profile}.yml") {
			t.Errorf("%q: several documents must fail startup, got=%v", body, err)
		}
	}
}

func TestLoad_LeadingOrTrailingDocumentMarkerStillLoads(t *testing.T) {
	// An empty document (nothing after the ---, only comments, a ~) does not count as one: a --- at
	// the start and an extra --- at the end both load as usual
	for _, body := range []string{
		"---\n---\nDemo:\n  Addr: a:1\n",
		"---\nDemo:\n  Addr: a:1\n",
		"# head\n---\nDemo:\n  Addr: a:1\n",
		"Demo:\n  Addr: a:1\n---\n",
		"Demo:\n  Addr: a:1\n---\n# nothing here\n",
		"Demo:\n  Addr: a:1\n--- ~\n",
		"Demo:\n  Addr: a:1\n...\n",
		"---\nDemo:\n  Addr: a:1\n---\n---\n",
	} {
		c := comps(t)
		if err := LoadInto(write(t, body), "Demo", c); err != nil {
			t.Errorf("%q: %v", body, err)
			continue
		}
		if c.Addr != "a:1" {
			t.Errorf("%q: Addr=%q", body, c.Addr)
		}
	}
}

func TestLoad_MissingVariablesListedOnceSorted(t *testing.T) {
	// One variable used in several places used to be reported as ZZMISS_B, ZZMISS_A, ZZMISS_B
	body := "Demo:\n  Addr: ${ZZMISS_B}\n  Retries: ${ZZMISS_A}\nOther:\n  A: ${ZZMISS_B}\n  B: ${ZZMISS_B}-${ZZMISS_A}\n"
	err := LoadInto(write(t, body), "Demo", comps(t))
	if err == nil || !strings.HasSuffix(err.Error(), "environment variables not set: ZZMISS_A, ZZMISS_B]") {
		t.Errorf("the unset variables must be sorted by name and reported once each, got=%v", err)
	}
}
