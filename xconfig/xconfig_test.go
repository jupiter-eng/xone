package xconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/jupiter-eng/xone/internal/config"
	"github.com/jupiter-eng/xone/xonetest"
)

type item struct {
	A string `yaml:"A"`
	B int    `yaml:"B"`
}

func defItem() item { return item{B: 42} }

// UnmarshalYAML is exactly the form this package's doc recommends, so it doubles as a test of that example.
func (i *item) UnmarshalYAML(n *yaml.Node) error {
	*i = defItem()
	type raw item
	return DecodeStrict(n, (*raw)(i))
}

func decode(t *testing.T, src string, v any) error {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(src), &n); err != nil {
		t.Fatal(err)
	}
	return DecodeStrict(n.Content[0], v)
}

func TestDecodeStrict_UnknownFieldIsError(t *testing.T) {
	var got item
	err := decode(t, "A: x\nZZZ: 1\n", &got)
	if err == nil {
		t.Fatal("a misspelled field must be an error")
	}
	if !strings.Contains(err.Error(), "ZZZ") {
		t.Errorf("the error must name it, got=%v", err)
	}
}

func TestDecodeStrict_CollectionElementsStayStrict(t *testing.T) {
	// This is why this package exists: when an element type writes its own UnmarshalYAML to pre-fill defaults,
	// taking the easy route with node.Decode makes the strict check quietly stop holding inside the collection.
	var got map[string]item
	if err := decode(t, "a:\n  A: x\n  ZZZ: 1\n", &got); err == nil {
		t.Fatal("a misspelled field inside a collection element must be an error too")
	}
}

func TestDecodeStrict_DefaultsApplied(t *testing.T) {
	var got map[string]item
	if err := decode(t, "a:\n  A: x\n", &got); err != nil {
		t.Fatal(err)
	}
	if got["a"].A != "x" || got["a"].B != 42 {
		t.Errorf("a field that is not set must keep its default, got=%+v", got["a"])
	}
}

type client struct {
	Addr string `yaml:"Addr"`
	Max  int    `yaml:"Max"`
}

// client deliberately writes no UnmarshalYAML: the defaults of both forms must come from UnmarshalClients.
func defClient() client { return client{Addr: "127.0.0.1", Max: 50} }

// clients installs src as the XMod block in the global configuration and decodes it in both forms.
func clients(t *testing.T, src string) (map[string]client, error) {
	t.Helper()
	body := "XMod:\n"
	for _, line := range strings.SplitAfter(src, "\n") {
		if line != "" {
			body += "  " + line
		}
	}
	xonetest.UseConfigYAML(t, body)
	return UnmarshalClients("XMod", defClient)
}

func TestUnmarshalClients_SingleInstanceForm(t *testing.T) {
	got, err := clients(t, "Addr: 10.0.0.1\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("exactly one instance must be decoded, got=%v", got)
	}
	c, ok := got[DefaultClientName]
	if !ok {
		t.Fatalf("the single-instance form must be normalized to an instance named %s, got=%v", DefaultClientName, got)
	}
	if c.Addr != "10.0.0.1" || c.Max != 50 {
		t.Errorf("what is set takes effect and what is not keeps its default, got=%+v", c)
	}
}

func TestUnmarshalClients_MultiInstanceForm(t *testing.T) {
	got, err := clients(t, "Clients:\n  default: {Addr: a}\n  report: {Addr: b, Max: 5}\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("two instances must be decoded, got=%v", got)
	}
	// A field one instance overrides must not affect the other.
	if got["default"].Max != 50 || got["report"].Max != 5 {
		t.Errorf("the defaults must apply per instance, got=%v", got)
	}
}

func TestUnmarshalClients_NameOnlyInstanceGetsDefaults(t *testing.T) {
	// Nothing written after report: means "this instance uses every default", not "this instance is all zero values".
	got, err := clients(t, "Clients:\n  default: {Addr: a}\n  report:\n")
	if err != nil {
		t.Fatal(err)
	}
	if got["report"] != defClient() {
		t.Errorf("an instance with nothing written must hold the defaults, got=%+v", got["report"])
	}
}

func TestUnmarshalClients_TypoInInstanceNamesTheInstance(t *testing.T) {
	_, err := clients(t, "Clients:\n  a: {Addr: x}\n  b: {Adrr: y}\n")
	if err == nil {
		t.Fatal("a misspelling must be an error")
	}
	if !strings.Contains(err.Error(), "Clients.b") || !strings.Contains(err.Error(), "Adrr") {
		t.Errorf("the error must name the instance and the field, got=%v", err)
	}
}

func TestUnmarshalClients_FormsCannotBeMixed(t *testing.T) {
	// When the two are mixed there is no unsurprising answer to "which one is default".
	_, err := clients(t, "Addr: a\nClients:\n  x: {Addr: b}\n")
	if err == nil {
		t.Fatal("mixing the two forms must be an error")
	}
	if !strings.Contains(err.Error(), "cannot mix") {
		t.Errorf("the error must say plainly why, got=%v", err)
	}
	// It must also name the field left without an owner, otherwise a config block of a few dozen lines gives no hint what to change.
	if !strings.Contains(err.Error(), "Addr") {
		t.Errorf("the error must point at the field, got=%v", err)
	}
}

func TestUnmarshalClients_TypoInInstanceNotReportedAsMixing(t *testing.T) {
	// "the two forms cannot be mixed" used to be attached to any decoding error at all: a misspelled field
	// inside an instance (Clients.x.Adrr) was reported with it too. That configuration mixes nothing, so the
	// caller would follow the message and change something that was fine, while the real typo stayed hidden behind it.
	cases := map[string]string{
		"a misspelled field inside an instance": "Clients:\n  x: {Adrr: a}\n",
		"a wrong type inside an instance":       "Clients:\n  x: {Max: abc}\n",
	}
	for name, src := range cases {
		_, err := clients(t, src)
		if err == nil {
			t.Fatalf("%s: must be an error", name)
		}
		if strings.Contains(err.Error(), "cannot mix") {
			t.Errorf("%s: this is not mixing and must not be reported as such, got=%v", name, err)
		}
	}
}

func TestUnmarshalClients_EmptyClientsIsError(t *testing.T) {
	if _, err := clients(t, "Clients: {}\n"); err == nil {
		t.Fatal("writing Clients and leaving it empty must be an error")
	}
}

func TestUnmarshalClients_TyposCaughtInBothForms(t *testing.T) {
	// A collection element goes through a custom decoder, where the strict check very easily stops holding quietly.
	if _, err := clients(t, "Adrr: a\n"); err == nil {
		t.Error("a misspelling in the single-instance form must be an error")
	}
	if _, err := clients(t, "Clients:\n  x: {Adrr: a}\n"); err == nil {
		t.Error("a misspelling in the multi-instance form must be an error")
	}
}

// ---- Unmarshal / Has: the only two entry points an integration package uses ----

type modCfg struct {
	Addr    string `yaml:"Addr"`
	Retry   int    `yaml:"Retry"`
	badFlag bool
}

func (c *modCfg) Validate() error {
	if c.badFlag {
		return errors.New("validate said no")
	}
	return nil
}

func TestUnmarshal_FieldsNotInFileKeepDefaults(t *testing.T) {
	// "defaults pre-filled in the struct" is the foundation of the whole configuration model: without it, every
	// field would need a pointer type to tell "not configured" from "configured to the zero value".
	xonetest.UseConfigYAML(t, "XMod:\n  Retry: 3\n")

	c := modCfg{Addr: "127.0.0.1", Retry: 1}
	if err := Unmarshal("XMod", &c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "127.0.0.1" {
		t.Errorf("a field that is not set was wiped to its zero value, got=%q", c.Addr)
	}
	if c.Retry != 3 {
		t.Errorf("a field that is set had no effect, got=%d", c.Retry)
	}
}

func TestUnmarshal_AbsentBlockLeavesValueUntouched(t *testing.T) {
	xonetest.UseConfigYAML(t, "XApp:\n  Name: demo\n")

	c := modCfg{Addr: "127.0.0.1"}
	if err := Unmarshal("XMod", &c); err != nil {
		t.Fatalf("not configuring this block must not be an error: %v", err)
	}
	if c.Addr != "127.0.0.1" {
		t.Errorf("the defaults were touched, got=%q", c.Addr)
	}
}

func TestUnmarshal_UnknownFieldIsError(t *testing.T) {
	xonetest.UseConfigYAML(t, "XMod:\n  Adr: 127.0.0.1\n")

	c := modCfg{}
	if err := Unmarshal("XMod", &c); err == nil {
		t.Fatal("a misspelled field must be an error, otherwise the caller keeps believing the setting took")
	}
}

func TestUnmarshal_CallsValidateOnce(t *testing.T) {
	// Some misconfigurations do not fail initialization and only make some behavior unreachable forever;
	// only Validate catches those.
	xonetest.UseConfigYAML(t, "XMod:\n  Retry: 1\n")

	c := modCfg{badFlag: true}
	err := Unmarshal("XMod", &c)
	if err == nil {
		t.Fatal("a Validate error must make Unmarshal fail")
	}
	if !strings.Contains(err.Error(), "validate said no") {
		t.Errorf("Validate's words must be carried up, got=%v", err)
	}
}

// useEnvConf writes a configuration to a file and hands it through XONE_CONFIG to "load on the first read".
func useEnvConf(t *testing.T, yml string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(p, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	config.Reset()
	t.Cleanup(config.Reset)
	t.Setenv(config.EnvKey, p)
}

func TestUnmarshal_LoadsFirstIfNotLoaded(t *testing.T) {
	// Reading early is not an error and does not quietly yield the defaults: the first read loads first,
	// so a read in main or before Run gets the final values from the file.
	useEnvConf(t, "XMod:\n  Retry: 7\n")

	c := modCfg{}
	if err := Unmarshal("XMod", &c); err != nil {
		t.Fatal(err)
	}
	if c.Retry != 7 {
		t.Errorf("the values from the file must be read, got=%d", c.Retry)
	}
}

func TestHas_DistinguishesUnsetFromSet(t *testing.T) {
	xonetest.UseConfigYAML(t, "XMod:\n  Retry: 1\nXOther:\n")

	if !Has("XMod") {
		t.Error("a block that is written must be reported as present")
	}
	if Has("XNope") {
		t.Error("a block that is not written must not be reported as present")
	}
	if Has("XOther") {
		t.Error("an empty block is the same as not configured and must not make an optional component build for nothing")
	}
}

func TestHas_LoadsFirstIfNotLoaded(t *testing.T) {
	useEnvConf(t, "XMod:\n  Retry: 1\n")
	if !Has("XMod") {
		t.Error("the first question must load first rather than report absence")
	}
}

func TestHas_AskingCountsAsClaim(t *testing.T) {
	// The skipped block must not end up on the "nobody claimed it" list: an optional component decides to skip
	// precisely by Has, and counting it as unwanted would fail startup outright.
	xonetest.UseConfigYAML(t, "XMod:\n")

	if Has("XMod") {
		t.Fatal("an empty block must be reported as not configured")
	}
	if got := config.Unclaimed(); len(got) != 0 {
		t.Errorf("want empty, got=%v", got)
	}
}

func TestUnmarshalClients_TypoInInstanceReportsFileAndLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "application.yml")
	body := "XMod:\n  Clients:\n    a: {Addr: x}\n    b:\n      Max: 1\n      Adrr: y\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	config.Reset()
	t.Cleanup(config.Reset)
	if err := config.Load(p); err != nil {
		t.Fatal(err)
	}
	_, err := UnmarshalClients("XMod", defClient)
	for _, want := range []string{"Clients.b: ", p + ":6: field Adrr not found"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name the instance, the file and the line %q, got=%v", want, err)
		}
	}
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		t.Errorf("errors.As must still reach yaml's type error, got=%v", err)
	}
}

func TestUnmarshalClients_AbsentBlockReturnsNilAndClaims(t *testing.T) {
	xonetest.UseConfigYAML(t, "XMod:\n")
	got, err := UnmarshalClients("XMod", defClient)
	if err != nil || got != nil {
		t.Fatalf("not configuring this block must give nil and no error, got=%v err=%v", got, err)
	}
	if left := config.Unclaimed(); len(left) != 0 {
		t.Errorf("asking counts as claiming, want empty, got=%v", left)
	}
}

// checked is an instance type that implements Validate.
type checked struct {
	Max int `yaml:"Max"`
}

func (c *checked) Validate() error {
	if c.Max <= 0 {
		return errors.New("Max must be > 0")
	}
	return nil
}

func TestUnmarshalClients_CallsValidateForEachInstance(t *testing.T) {
	xonetest.UseConfigYAML(t, "XMod:\n  Clients:\n    a: {Max: 1}\n    b: {Max: 0}\n")
	_, err := UnmarshalClients("XMod", func() checked { return checked{Max: 1} })
	// It must also carry which file and line this instance is on: Validate itself can only name the field.
	if err == nil || !strings.Contains(err.Error(), "Clients.b: ") || !strings.Contains(err.Error(), "application.yml:4: Max must be > 0") {
		t.Errorf("Validate's words must be carried up, naming the instance and carrying the file and line, got=%v", err)
	}
}
