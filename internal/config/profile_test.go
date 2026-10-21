package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// listConf carries a list field, to verify "a list is replaced whole, not merged element by
// element"
type listConf struct {
	Addr    string        `yaml:"Addr"`
	Timeout time.Duration `yaml:"Timeout"`
	Headers []string      `yaml:"Headers"`
	Nested  struct {
		A string `yaml:"A"`
		B string `yaml:"B"`
	} `yaml:"Nested"`
}

func listComps(t *testing.T) *listConf {
	t.Helper()
	c := listConf{Addr: "default:1", Timeout: time.Second}
	return &c
}

// files writes a set of files into one temporary directory and returns base's path
func files(t *testing.T, base string, m map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range m {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, base)
}

// withProfileEnv sets XONE_PROFILE and restores it on the way out
func withProfileEnv(t *testing.T, v string) {
	t.Helper()
	t.Setenv(ProfileEnvKey, v)
}

func TestLoad_ProfileFileOverridesBase(t *testing.T) {
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Addr: base:1\n  Timeout: 1s\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "prod:1" {
		t.Errorf("a profile file must override base, Addr=%q", c.Addr)
	}
	if c.Timeout != time.Second {
		t.Errorf("a field the profile does not set must keep base's value, Timeout=%v", c.Timeout)
	}
}

func TestLoad_LaterProfileOverridesEarlier(t *testing.T) {
	withProfileEnv(t, "a, b")
	base := files(t, "application.yml", map[string]string{
		"application.yml":   "Demo:\n  Addr: base:1\n",
		"application-a.yml": "Demo:\n  Addr: a:1\n  Timeout: 2s\n",
		"application-b.yml": "Demo:\n  Addr: b:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "b:1" {
		t.Errorf("a later profile must override an earlier one, Addr=%q", c.Addr)
	}
	if c.Timeout != 2*time.Second {
		t.Errorf("a field a sets and b does not must keep a's value, Timeout=%v", c.Timeout)
	}
}

func TestLoad_ListsReplacedWholeNotMerged(t *testing.T) {
	// Merged element by element, [A,B] with [C] on top would become [C,B] -- the caller believes
	// they replaced the whole table while only the first entry was replaced and the rest comes from
	// another file. Spring replaces a list whole as well
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Headers: [A, B]\n",
		"application-prod.yml": "Demo:\n  Headers: [C]\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Headers, []string{"C"}) {
		t.Errorf("a list must be replaced whole, got=%v want=[C]", c.Headers)
	}
}

func TestLoad_MapMergedRecursively(t *testing.T) {
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Nested:\n    A: base-a\n    B: base-b\n",
		"application-prod.yml": "Demo:\n  Nested:\n    A: prod-a\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Nested.A != "prod-a" || c.Nested.B != "base-b" {
		t.Errorf("a map must be merged recursively, A=%q B=%q", c.Nested.A, c.Nested.B)
	}
}

func TestLoad_ProfileFileMissingFailsFast(t *testing.T) {
	// Unlike Spring, which skips it quietly. A profile file named and not there is almost always a
	// misspelled name, and skipping it quietly brings up a configuration nobody ever looked at, on
	// the defaults
	withProfileEnv(t, "typo")
	base := files(t, "application.yml", map[string]string{
		"application.yml": "Demo:\n  Addr: base:1\n",
	})

	c := listComps(t)
	err := LoadInto(base, "Demo", c)
	if err == nil {
		t.Fatal("a profile file that does not exist must be an error")
	}
	if !strings.Contains(err.Error(), "application-typo.yml") {
		t.Errorf("the error must point at the file, got=%v", err)
	}
}

func TestLoad_ImportOverridesImporter(t *testing.T) {
	// As in Spring: an import behaves as though inserted directly below the document declaring it,
	// and what is below overrides what is above
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import: shared.yml\nDemo:\n  Addr: base:1\n  Timeout: 1s\n",
		"shared.yml":      "Demo:\n  Addr: shared:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "shared:1" {
		t.Errorf("an imported file must override the one importing it, Addr=%q", c.Addr)
	}
	if c.Timeout != time.Second {
		t.Errorf("a field the import does not set must be kept, Timeout=%v", c.Timeout)
	}
}

func TestLoad_ProfileOverridesBaseImport(t *testing.T) {
	// The order: base < base's imports < the profile file < the profile's imports
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "XApp:\n  Import: shared.yml\nDemo:\n  Addr: base:1\n",
		"shared.yml":           "Demo:\n  Addr: shared:1\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "prod:1" {
		t.Errorf("a profile file must override what base imported, Addr=%q", c.Addr)
	}
}

func TestLoad_ImportMultipleAppliedInOrder(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import:\n    - a.yml\n    - b.yml\nDemo:\n  Addr: base:1\n",
		"a.yml":           "Demo:\n  Addr: a:1\n  Timeout: 3s\n",
		"b.yml":           "Demo:\n  Addr: b:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "b:1" {
		t.Errorf("a later import must override an earlier one, Addr=%q", c.Addr)
	}
	if c.Timeout != 3*time.Second {
		t.Errorf("a field a sets and b does not must keep a's value, Timeout=%v", c.Timeout)
	}
}

func TestLoad_ImportRelativePathResolvedAgainstImporter(t *testing.T) {
	// Resolved against the process's working directory, the references inside a configuration
	// directory would break as soon as it moved elsewhere
	base := files(t, "conf/application.yml", map[string]string{
		"conf/application.yml": "XApp:\n  Import: parts/db.yml\nDemo:\n  Addr: base:1\n",
		"conf/parts/db.yml":    "Demo:\n  Addr: db:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "db:1" {
		t.Errorf("a relative path must resolve against the directory of the file importing it, Addr=%q", c.Addr)
	}
}

func TestLoad_ImportMissingFileIsError(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import: missing.yml\nDemo:\n  Addr: base:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err == nil {
		t.Fatal("an imported file that does not exist must be an error")
	}
}

func TestLoad_OptionalImportMayBeMissing(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import: optional:local.yml\nDemo:\n  Addr: base:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatalf("an optional: file that does not exist must not be an error: %v", err)
	}
	if c.Addr != "base:1" {
		t.Errorf("Addr=%q", c.Addr)
	}
}

func TestLoad_ImportCycleTerminates(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import: a.yml\nDemo:\n  Addr: base:1\n",
		"a.yml":           "XApp:\n  Import: application.yml\nDemo:\n  Addr: a:1\n",
	})

	c := listComps(t)
	// One file counts once, so a cycle breaks by itself rather than recursing forever
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatalf("a cycle must be caught by one file being read once: %v", err)
	}
	if c.Addr != "a:1" {
		t.Errorf("Addr=%q", c.Addr)
	}
}

func TestLoad_ProfilesOnlyAllowedInBase(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import: shared.yml\nDemo:\n  Addr: base:1\n",
		"shared.yml":      "XApp:\n  Profiles: [prod]\nDemo:\n  Addr: shared:1\n",
	})

	c := listComps(t)
	err := LoadInto(base, "Demo", c)
	if err == nil {
		t.Fatal("an imported file activating profiles must be an error")
	}
	if !strings.Contains(err.Error(), ProfilesKey) {
		t.Errorf("the error must name the key, got=%v", err)
	}
}

func TestLoad_BaseProfilesTakeEffect(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "XApp:\n  Profiles: [prod]\nDemo:\n  Addr: base:1\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "prod:1" {
		t.Errorf("a profile declared in the configuration file must take effect, Addr=%q", c.Addr)
	}
}

func TestLoad_EnvVarOverridesProfilesInFile(t *testing.T) {
	withProfileEnv(t, "dev")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "XApp:\n  Profiles: [prod]\nDemo:\n  Addr: base:1\n",
		"application-dev.yml":  "Demo:\n  Addr: dev:1\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "dev:1" {
		t.Errorf("the environment must override the profiles declared in the file, Addr=%q", c.Addr)
	}
}

func TestLoad_ImportAndProfilesAreNotUnclaimed(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Profiles: []\n  Import: optional:x.yml\nDemo:\n  Addr: base:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatalf("these two are reserved keys and must not count as unclaimed: %v", err)
	}
}

func TestLoad_RequiredPlaceholderOverriddenByProfileNotRequired(t *testing.T) {
	// The placeholders are expanded only once everything has been merged. Expanded file by file,
	// the ${SECRET} in base would still have to be set even once prod had replaced it
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Addr: \"${SECRET}\"\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatalf("a placeholder that was overridden must no longer have to be set: %v", err)
	}
	if c.Addr != "prod:1" {
		t.Errorf("Addr=%q", c.Addr)
	}
}

func TestLoad_ImportPathPlaceholdersAreExpanded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shared.yml"), []byte("Demo:\n  Addr: shared:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	basePath := filepath.Join(dir, "application.yml")
	if err := os.WriteFile(basePath, []byte("XApp:\n  Import: \"${CONF_DIR}/shared.yml\"\nDemo:\n  Addr: base:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONF_DIR", dir)

	c := listComps(t)
	if err := LoadInto(basePath, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "shared:1" {
		t.Errorf("a placeholder in an import path must be expanded, Addr=%q", c.Addr)
	}
}

func TestLoad_ImportFilesAlsoGetProfileVariants(t *testing.T) {
	// Once a configuration is split into fragments, what most needs to change per environment is
	// exactly what the fragments hold (connection strings and the like), and with variants for the
	// main file alone, splitting becomes awkward
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "XApp:\n  Import: parts/db.yml\nDemo:\n  Addr: base:1\n",
		"parts/db.yml":         "Demo:\n  Addr: db:1\n  Timeout: 1s\n",
		"parts/db-prod.yml":    "Demo:\n  Addr: db-prod:1\n",
		"application-prod.yml": "Demo:\n  Headers: [x]\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "db-prod:1" {
		t.Errorf("a fragment's profile variant must take effect, Addr=%q", c.Addr)
	}
	if c.Timeout != time.Second {
		t.Errorf("a field the variant does not set must keep the fragment's value, Timeout=%v", c.Timeout)
	}
}

func TestLoad_FragmentProfileVariantMayBeMissing(t *testing.T) {
	// Unlike the main file: split into ten fragments, requiring every fragment to carry a variant
	// for every environment is unusable.
	// A misspelled profile name is caught by the main file's variant already
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "XApp:\n  Import: parts/db.yml\nDemo:\n  Addr: base:1\n",
		"parts/db.yml":         "Demo:\n  Addr: db:1\n",
		"application-prod.yml": "Demo:\n  Timeout: 9s\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatalf("a fragment with no prod variant must not be an error: %v", err)
	}
	if c.Addr != "db:1" || c.Timeout != 9*time.Second {
		t.Errorf("Addr=%q Timeout=%v", c.Addr, c.Timeout)
	}
}

func TestLoad_MainProfileVariantOverridesFragments(t *testing.T) {
	// The order: base, base's imports (their variants included), application-prod, its imports
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "XApp:\n  Import: parts/db.yml\nDemo:\n  Addr: base:1\n",
		"parts/db.yml":         "Demo:\n  Addr: db:1\n",
		"parts/db-prod.yml":    "Demo:\n  Addr: db-prod:1\n",
		"application-prod.yml": "Demo:\n  Addr: app-prod:1\n",
	})

	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "app-prod:1" {
		t.Errorf("the main file's profile variant must override a fragment's, Addr=%q", c.Addr)
	}
}

func TestLoad_ProfilesActiveAcceptsBothForms(t *testing.T) {
	// Both the comments and the documentation say a comma-separated string works, while that
	// spelling used to fail with cannot unmarshal !!str into []string
	for name, active := range map[string]string{
		"a list":                         "[a, b]",
		"a string":                       "\"a,b\"",
		"without quotes":                 "a, b",
		"a list element holding a comma": "[\"a,b\"]",
	} {
		t.Run(name, func(t *testing.T) {
			base := files(t, "application.yml", map[string]string{
				"application.yml":   "XApp:\n  Profiles: " + active + "\nDemo:\n  Addr: base:1\n",
				"application-a.yml": "Demo:\n  Addr: a:1\n  Timeout: 2s\n",
				"application-b.yml": "Demo:\n  Addr: b:1\n",
			})
			c := listComps(t)
			if err := LoadInto(base, "Demo", c); err != nil {
				t.Fatal(err)
			}
			if c.Addr != "b:1" || c.Timeout != 2*time.Second {
				t.Errorf("both profiles a and b must take effect with b overriding a, got Addr=%q Timeout=%v", c.Addr, c.Timeout)
			}
		})
	}
}

func TestLoad_ProfilesPlaceholdersAreExpanded(t *testing.T) {
	// Which profiles are active has to be known before which files to read is, so this block, like
	// Import, is expanded as it is read rather than once everything has been merged
	t.Setenv("XONE_T_ENV", "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "XApp:\n  Profiles: ${XONE_T_ENV:dev}\nDemo:\n  Addr: base:1\n",
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

func TestLoad_ProfilesUnsetPlaceholderIsError(t *testing.T) {
	os.Unsetenv("XONE_T_ENV_MISSING")
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Profiles: ${XONE_T_ENV_MISSING}\n",
	})
	err := LoadInto(base, "Demo", listComps(t))
	if err == nil || !strings.Contains(err.Error(), "XONE_T_ENV_MISSING") {
		t.Fatalf("an unset variable must be named in the error, got=%v", err)
	}
}

func TestLoad_EmptyProfilesInImportedFileIsError(t *testing.T) {
	// It used to be reported only when profiles were really declared, and Active: [] slipped
	// through
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import: shared.yml\n",
		"shared.yml":      "XApp:\n  Profiles: []\n",
	})
	if err := LoadInto(base, "Demo", listComps(t)); err == nil {
		t.Fatal("a Profiles appearing in an imported file must be an error")
	}
}

func TestLoad_ImportNestedTooDeepIsError(t *testing.T) {
	m := map[string]string{}
	for i := 0; i <= maxImportDepth+1; i++ {
		m[fmt.Sprintf("f%d.yml", i)] = fmt.Sprintf("XApp:\n  Import: f%d.yml\n", i+1)
	}
	m[fmt.Sprintf("f%d.yml", maxImportDepth+2)] = "Demo:\n  Addr: deep:1\n"
	err := LoadInto(files(t, "f0.yml", m), "Demo", listComps(t))
	if err == nil || !strings.Contains(err.Error(), "levels deep") {
		t.Fatalf("an import nested more than %d levels deep must be an error, got=%v", maxImportDepth, err)
	}
	// A cycle never reaches here (one file is read once), so the error must not send anybody
	// looking for one
	if strings.Contains(err.Error(), "cycle") {
		t.Errorf("nesting too deep has nothing to do with a cycle and the error must not mention one: %v", err)
	}
}

func TestLoad_XAppWithOnlyProfilesAndImportIsNotUnclaimed(t *testing.T) {
	// A program that never imported xapp: once the loader has taken XApp's two fields out it is an
	// empty block, which must not count as a misspelled key
	t.Cleanup(Reset)
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Profiles: []\n  Import: optional:x.yml\nDemo:\n  Addr: base:1\n",
	})
	if err := Load(base); err != nil {
		t.Fatal(err)
	}
	if err := Unmarshal("Demo", listComps(t)); err != nil {
		t.Fatal(err)
	}
	if got := Unclaimed(); len(got) != 0 {
		t.Errorf("an XApp left with Profiles / Import alone must not count as unread, got=%v", got)
	}
}

func TestLoad_XAppNameLeftToXapp_ProfilesAndImportTakenByLoader(t *testing.T) {
	// xapp decodes XApp strictly: with Profiles and Import still there it would report fields it
	// does not recognize
	t.Cleanup(Reset)
	withProfileEnv(t, "")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "XApp:\n  Name: demo\n  Profiles: prod\n  Import: part.yml\n",
		"part.yml":             "Demo:\n  Addr: part:1\n",
		"application-prod.yml": "XApp:\n  Version: v1\n",
	})
	if err := Load(base); err != nil {
		t.Fatal(err)
	}
	var app struct {
		Name    string `yaml:"Name"`
		Version string `yaml:"Version"`
	}
	if err := Unmarshal(AppKey, &app); err != nil {
		t.Fatalf("XApp must be left with Name / Version alone: %v", err)
	}
	if app.Name != "demo" || app.Version != "v1" {
		t.Errorf("got=%+v, want Name=demo (from base) Version=v1 (from the prod file)", app)
	}
	c := listComps(t)
	if err := Unmarshal("Demo", c); err != nil || c.Addr != "part:1" {
		t.Errorf("XApp.Import must take effect, Addr=%q err=%v", c.Addr, err)
	}
}

// ---- The YAML merge key (<<) across files ----
//
// yaml's rule is "a key written out overrides one merged in with <<". That rule belongs inside each
// file: it used to be applied by the decoder only after several files had been merged, so a value a
// profile file wrote with << lost to a key of the same name written out in base -- the
// higher-priority file was the one that did not take effect

func TestLoad_ProfileMergeKeyBeatsBaseExplicitKeys(t *testing.T) {
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Addr: base:1\n  Timeout: 1s\n",
		"application-prod.yml": "Shared: &s\n  Addr: prod:1\nDemo:\n  <<: *s\n",
	})
	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "prod:1" || c.Timeout != time.Second {
		t.Errorf("a value merged in with << in a profile must override base, got Addr=%q Timeout=%v", c.Addr, c.Timeout)
	}
}

func TestLoad_ImportedMergeKeyBeatsImporterExplicitKeys(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "XApp:\n  Import: extra.yml\nDemo:\n  Addr: base:1\n",
		"extra.yml":       "Demo:\n  <<: {Addr: extra:1}\n",
	})
	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "extra:1" {
		t.Errorf("a value merged in with << in an imported file must override the one importing it, Addr=%q", c.Addr)
	}
}

func TestLoad_ProfileExplicitKeyBeatsBaseMergeKey(t *testing.T) {
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Shared: &s\n  Addr: base:1\n  Timeout: 2s\nDemo:\n  <<: *s\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})
	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "prod:1" || c.Timeout != 2*time.Second {
		t.Errorf("got Addr=%q Timeout=%v", c.Addr, c.Timeout)
	}
}

func TestLoad_ProfileNestedMergeKeyBeatsBase(t *testing.T) {
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Nested:\n    A: base-a\n    B: base-b\n",
		"application-prod.yml": "Demo:\n  Nested:\n    <<: {A: prod-a}\n",
	})
	c := listComps(t)
	if err := LoadInto(base, "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Nested.A != "prod-a" || c.Nested.B != "base-b" {
		t.Errorf("got %+v", c.Nested)
	}
}

func TestLoad_MergeKeyRulesWithinOneFile(t *testing.T) {
	// Inside one file yaml's rules still hold: a key written out overrides one merged in, whether it
	// is written before or after the <<; with a list after the <<, an earlier entry overrides a
	// later one; and a mapping merged in may carry a << of its own
	body := "A: &a {Addr: a:1, Timeout: 1s}\n" +
		"B: &b {Addr: b:1, Timeout: 2s, Headers: [b]}\n" +
		"W: &w {<<: {Nested: {A: deep}}, Addr: w:1}\n" +
		"Demo:\n  Addr: own:1\n  <<: [*a, *b]\n" +
		"Deep:\n  <<: *w\n"
	c := listComps(t)
	if err := LoadInto(write(t, body), "Demo", c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "own:1" || c.Timeout != time.Second || !reflect.DeepEqual(c.Headers, []string{"b"}) {
		t.Errorf("got Addr=%q Timeout=%v Headers=%v", c.Addr, c.Timeout, c.Headers)
	}
	d := listComps(t)
	if err := Unmarshal("Deep", d); err != nil {
		t.Fatal(err)
	}
	if d.Addr != "w:1" || d.Nested.A != "deep" {
		t.Errorf("got Addr=%q Nested=%+v", d.Addr, d.Nested)
	}
}

func TestLoad_MergeKeyWithScalarValueIsError(t *testing.T) {
	err := LoadInto(write(t, "Demo:\n  <<: oops\n"), "Demo", listComps(t))
	if err == nil || !strings.Contains(err.Error(), "<<") {
		t.Errorf("anything after a << that is not a mapping must be an error, got=%v", err)
	}
}

func TestLoad_TypoInMergedFieldsIsError(t *testing.T) {
	// Once the << is flattened, a field merged in is an ordinary key and a misspelling still fails
	// startup
	err := LoadInto(write(t, "Shared: &s\n  Adr: x:1\nDemo:\n  <<: *s\n"), "Demo", listComps(t))
	if err == nil || !strings.Contains(err.Error(), "field Adr not found") {
		t.Errorf("a misspelled field merged in must be an error, got=%v", err)
	}
}

func TestLoad_MultipleDocumentsInProfileFileIsError(t *testing.T) {
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Addr: base:1\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n---\nDemo:\n  Addr: other:1\n",
	})
	err := LoadInto(base, "Demo", listComps(t))
	if err == nil || !strings.Contains(err.Error(), "application-prod.yml") || !strings.Contains(err.Error(), "line 3") {
		t.Errorf("several documents in a profile file must fail startup too, pointing at the file and the line, got=%v", err)
	}
}
