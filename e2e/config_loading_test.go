package e2e

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jupiter-eng/xone/e2e/harness"
)

// covSeen is GET /probe/config: the Service block main read (before xone.Run) and the Cov block the start hook read
type covSeen struct {
	Service struct {
		Downstream string `json:"downstream"`
		UserTTL    string `json:"user_ttl"`
	} `json:"service"`
	Cov struct {
		Label  string            `json:"Label"`
		Items  []string          `json:"Items"`
		Labels map[string]string `json:"Labels"`
	} `json:"cov"`
}

func covReadConfig(t *testing.T, p *harness.Process) covSeen {
	t.Helper()
	var s covSeen
	p.Get(t, "/probe/config").JSON(t, &s)
	return s
}

// covBase writes one service/application.yml plus a Cov block (and the other top-level blocks in
// extra) into a new temporary directory and returns the path. The profile files and the Imported
// fragments are written relative to the directory it sits in
func covBase(t *testing.T, extra map[string]string) string {
	t.Helper()
	blocks := map[string]string{"Cov": "Cov:\n  Label: base\n  Items: [x, y]\n  Labels: {b: base}\n"}
	for k, v := range extra {
		blocks[k] = v
	}
	return covConfig(t, blocks)
}

// docs/config.md, "Profiles -- one file per environment":
//
//	the precedence: --profile > XONE_PROFILE > XApp.Profiles in the file
//	--profile=prod,eu takes several, comma separated, and a later one wins over an earlier one
//	a profile file that is named and does not exist fails the start outright
//
// "The merge rules": maps merge recursively, a list is replaced whole and a scalar is overridden.
// "The common rules - when it is read": what is read in main before xone.Run is the final value as
// well -- the e2e service's Service block is read in main
func TestCoverage_ProfileThreeWaysToSetAndPrecedence_MergeRules(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	cfg := covBase(t, map[string]string{"XApp": "XApp:\n  Name: xone.e2e.service\n  Version: e2e\n  Profiles: [fromfile]\n"})
	dir := filepath.Dir(cfg)
	covWrite(t, dir, "application-fromfile.yml", "Cov:\n  Label: fromfile\n")
	covWrite(t, dir, "application-envp.yml", "Cov:\n  Label: envp\n  Labels: {e: envp}\n")
	covWrite(t, dir, "application-argp.yml", "Cov:\n  Label: argp\n  Items: [z]\n  Labels: {a: argp}\nService:\n  Downstream: http://from-argp.invalid\n  UserTTL: 7s\n")

	for _, c := range []struct {
		name   string
		args   []string
		env    map[string]string
		label  string
		items  []string
		labels map[string]string
	}{
		{name: "XApp.Profiles alone", label: "fromfile", items: []string{"x", "y"}, labels: map[string]string{"b": "base"}},
		{name: "XONE_PROFILE wins over XApp.Profiles", env: map[string]string{"XONE_PROFILE": "envp"},
			label: "envp", items: []string{"x", "y"}, labels: map[string]string{"b": "base", "e": "envp"}},
		{name: "--profile wins over XONE_PROFILE", args: []string{"--profile=argp"}, env: map[string]string{"XONE_PROFILE": "envp"},
			label: "argp", items: []string{"z"}, labels: map[string]string{"b": "base", "a": "argp"}},
		{name: "--profile=envp,argp: the later wins over the earlier, maps merge level by level and a list is replaced whole", args: []string{"--profile=envp,argp"},
			label: "argp", items: []string{"z"}, labels: map[string]string{"b": "base", "e": "envp", "a": "argp"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := harness.Start(t, harness.Options{Config: cfg, Args: c.args, Env: c.env})
			s := covReadConfig(t, p)
			if s.Cov.Label != c.label || !slices.Equal(s.Cov.Items, c.items) || fmt.Sprint(s.Cov.Labels) != fmt.Sprint(c.labels) {
				t.Errorf("the Cov in force should be Label=%s Items=%v Labels=%v, got %+v", c.label, c.items, c.labels, s.Cov)
			}
			// The Service block is read in main before xone.Run: with argp in force, what it read has to come from argp as well
			wantDS := ""
			if c.label == "argp" {
				wantDS = "http://from-argp.invalid"
			}
			if s.Service.Downstream != wantDS {
				t.Errorf("docs/config.md: what is read in main before xone.Run is the final value as well; Service.Downstream should be %q, got %q", wantDS, s.Service.Downstream)
			}
		})
	}

	t.Run("a profile file that is named and does not exist fails the start", func(t *testing.T) {
		t.Parallel()
		stderr := covStartupError(t, harness.Options{Config: cfg, Args: []string{"--profile=nosuch"}})
		faultMustContain(t, "the start error for --profile=nosuch", stderr, "application-nosuch.yml")
	})
}

// docs/config.md, "Import -- pulling in another configuration file":
//
//	what is pulled in wins over the file pulling it in; a file pulled in has its own profile variants (one that does not exist is no error)
//	a relative path resolves against the directory of the file pulling it in and not the process's working directory
//	the optional: prefix skips a file that does not exist; one without the prefix that does not exist is an error
//
// "The merge rules": application.yml < what it Imports (the fragment's own -prod variant included) < application-prod.yml
func TestCoverage_ImportPrecedence_RelativePath_Optional_FragmentProfiles(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	cfg := covBase(t, map[string]string{"XApp": "XApp:\n  Name: xone.e2e.service\n  Version: e2e\n  Import:\n    - parts/shared.yml\n    - optional:parts/missing.yml\n"})
	dir := filepath.Dir(cfg)
	covWrite(t, dir, "parts/shared.yml", "Cov:\n  Label: imported\n  Items: [imp]\n  Labels: {i: imp}\n")
	covWrite(t, dir, "parts/shared-prod.yml", "Cov:\n  Labels: {iv: variant}\n")
	covWrite(t, dir, "application-prod.yml", "Cov:\n  Label: prod\n")

	t.Run("no profile: the fragment wins over base", func(t *testing.T) {
		t.Parallel()
		// The working directory is Process.Dir, away from the configuration file: parts/ being found shows it resolves against the file pulling it in
		s := covReadConfig(t, harness.Start(t, harness.Options{Config: cfg}))
		if s.Cov.Label != "imported" || !slices.Equal(s.Cov.Items, []string{"imp"}) || fmt.Sprint(s.Cov.Labels) != fmt.Sprint(map[string]string{"b": "base", "i": "imp"}) {
			t.Errorf("what is pulled in wins over the file pulling it in, so it should be Label=imported Items=[imp] Labels={b i}, got %+v", s.Cov)
		}
	})
	t.Run("--profile=prod: the profile wins over the fragment and the fragment's variant is laid on as well", func(t *testing.T) {
		t.Parallel()
		s := covReadConfig(t, harness.Start(t, harness.Options{Config: cfg, Args: []string{"--profile=prod"}}))
		want := map[string]string{"b": "base", "i": "imp", "iv": "variant"}
		if s.Cov.Label != "prod" || !slices.Equal(s.Cov.Items, []string{"imp"}) || fmt.Sprint(s.Cov.Labels) != fmt.Sprint(want) {
			t.Errorf("it should be Label=prod (the profile winning over the fragment) Items=[imp] Labels=%v (the fragment's -prod variant laid on), got %+v", want, s.Cov)
		}
	})
	t.Run("a fragment without the optional: prefix that does not exist fails the start", func(t *testing.T) {
		t.Parallel()
		bad := covBase(t, map[string]string{"XApp": "XApp:\n  Name: xone.e2e.service\n  Version: e2e\n  Import: parts/nowhere.yml\n"})
		faultMustContain(t, "the start error for an Imported file that does not exist", covStartupError(t, harness.Options{Config: bad}), "nowhere.yml")
	})
}

// docs/config.md, "The common rules":
//
//	${VAR:default}  optional, falling back to the default when it is unset
//	${VAR}          required (functional_config_test.go tests that the start fails when it is unset)
//	a placeholder expanding to nothing  ${VAR:} is the same as not writing the setting at all and keeps the struct's default; for a real empty string, quote it: "${VAR:}"
func TestCoverage_PlaceholderDefaults_EmptyExpansionKeepsStructDefault(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	cfg := covBase(t, map[string]string{"Cov": "Cov:\n  Label: ${COV_LABEL:fallback}\n  Items: [\"${COV_ITEM:}\", \"b\"]\n"})
	empty := covBase(t, map[string]string{"Cov": "Cov:\n  Label: ${COV_LABEL:}\n"})
	quoted := covBase(t, map[string]string{"Cov": "Cov:\n  Label: \"${COV_LABEL:}\"\n"})
	for _, c := range []struct {
		name, cfg string
		env       map[string]string
		label     string
	}{
		{"unset: the default after the colon is used", cfg, nil, "fallback"},
		{"set: the variable's value is used", cfg, map[string]string{"COV_LABEL": "from-env"}, "from-env"},
		{"${VAR:} expanding to nothing keeps the struct's default", empty, nil, "default"},
		{"a quoted \"${VAR:}\" is an empty string", quoted, nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := covReadConfig(t, harness.Start(t, harness.Options{Config: c.cfg, Env: c.env}))
			if s.Cov.Label != c.label {
				t.Errorf("Cov.Label should be %q, got %q", c.label, s.Cov.Label)
			}
			// A quoted "${COV_ITEM:}" inside a list is an empty string and not "not written"
			if c.cfg == cfg && !slices.Equal(s.Cov.Items, []string{"", "b"}) {
				t.Errorf("the quoted \"${COV_ITEM:}\" in Items should expand to an empty string, got %q", s.Cov.Items)
			}
		})
	}
}

// The top of docs/config.md: "where the configuration file is: --config=<path> > XONE_CONFIG > the
// conventional paths such as conf/application.yml"; the README: "one that is named explicitly and
// not found is an error"
func TestCoverage_ConfigFileLocation_ConfigFlagBeatsXONE_CONFIG(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	a := covBase(t, map[string]string{"Cov": "Cov:\n  Label: file-a\n"})
	b := covBase(t, map[string]string{"Cov": "Cov:\n  Label: file-b\n"})

	t.Run("XONE_CONFIG alone", func(t *testing.T) {
		t.Parallel()
		p := harness.StartArgs(t, harness.Options{Env: map[string]string{"XONE_CONFIG": a}})
		if got := covReadConfig(t, p).Cov.Label; got != "file-a" {
			t.Errorf("without --config it reads the file XONE_CONFIG names (file-a), got %q", got)
		}
	})
	t.Run("--config wins over XONE_CONFIG", func(t *testing.T) {
		t.Parallel()
		p := harness.StartArgs(t, harness.Options{Env: map[string]string{"XONE_CONFIG": a}}, "--config="+b)
		if got := covReadConfig(t, p).Cov.Label; got != "file-b" {
			t.Errorf("--config comes before XONE_CONFIG and it should read file-b, got %q", got)
		}
	})
	t.Run("a file XONE_CONFIG names that does not exist fails the start", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(t.TempDir(), "nope.yml")
		p := harness.StartArgs(t, harness.Options{NoWait: true, Env: map[string]string{"XONE_CONFIG": missing}})
		exit, ok := p.Wait(30 * time.Second)
		if !ok || exit.Code == 0 {
			t.Fatalf("a configuration file named explicitly and not found should fail the start, got %v (exited=%v)", exit, ok)
		}
		faultMustContain(t, "the start error when XONE_CONFIG points at a file that does not exist", p.Stderr(), missing)
	})
}

// covAppRun runs covapp once (a one-off job that exits as soon as it has read) and returns how it
// exited, the App.Name each read saw, and stderr
func covAppRun(t *testing.T, env map[string]string, args ...string) (harness.Exit, map[string]string, string) {
	t.Helper()
	p := harness.StartCovApp(t, harness.Options{Env: env}, args...)
	exit, ok := p.Wait(30 * time.Second)
	if !ok {
		t.Fatalf("covapp had not exited after 30s\n%s", p.Output())
	}
	seen := map[string]string{}
	for _, l := range p.FindLogs(func(l harness.Log) bool { return l.Msg() == "covapp read" }) {
		seen[l.Str("phase")] = l.Str("name")
	}
	return exit, seen, p.Stderr()
}

// xone.WithConfigPath: "names the configuration file, before --config, XONE_CONFIG and the
// conventional paths. When the configuration has already been read before Run, it has been loaded
// one of those ways, and naming another file here makes Run report an error outright".
// The e2e service uses no WithConfigPath, so this is tested with e2e/covapp
func TestCoverage_WithConfigPathBeatsConfigFlag_OtherFileAfterReadErrors(t *testing.T) {
	harness.Require(t)
	t.Parallel()

	dir := t.TempDir()
	a := covWrite(t, dir, "a.yml", "XApp:\n  Name: from-a\n")
	b := covWrite(t, dir, "b.yml", "XApp:\n  Name: from-b\n")

	t.Run("nothing read beforehand: WithConfigPath wins over --config", func(t *testing.T) {
		t.Parallel()
		exit, seen, stderr := covAppRun(t, map[string]string{"COVAPP_WITH_CONFIG_PATH": a}, "--config="+b)
		if exit.Code != 0 || seen["in_run"] != "from-a" {
			t.Errorf("WithConfigPath comes before --config and should read a (from-a) and exit with 0, got %v having read %q\n%s", exit, seen["in_run"], stderr)
		}
	})
	t.Run("read beforehand (b loaded through --config) and then naming a: Run reports an error", func(t *testing.T) {
		t.Parallel()
		exit, seen, stderr := covAppRun(t, map[string]string{"COVAPP_WITH_CONFIG_PATH": a, "COVAPP_READ_EARLY": "1"}, "--config="+b)
		if exit.Code == 0 {
			t.Errorf("naming another file with WithConfigPath after an earlier read should make Run report an error, and it exited with 0 having read %q in Start", seen["in_run"])
		}
		if seen["early"] != "from-b" {
			t.Errorf("the earlier read loads b through --config and should have read from-b, got %q", seen["early"])
		}
		faultMustContain(t, "the error on a WithConfigPath conflict", stderr, "config was already loaded by an earlier read", b, a, "--config", "XONE_CONFIG")
		if _, ran := seen["in_run"]; ran {
			t.Errorf("Start should not be reached when Run reports an error")
		}
	})
	t.Run("read beforehand, with WithConfigPath naming that same file: it carries on", func(t *testing.T) {
		t.Parallel()
		exit, seen, stderr := covAppRun(t, map[string]string{"COVAPP_WITH_CONFIG_PATH": b, "COVAPP_READ_EARLY": "1"}, "--config="+b)
		if exit.Code != 0 || seen["early"] != "from-b" || seen["in_run"] != "from-b" {
			t.Errorf("naming the file that is already loaded should report no error and both reads should see from-b, got %v early=%q in_run=%q\n%s", exit, seen["early"], seen["in_run"], stderr)
		}
	})
	t.Run("read beforehand, with WithConfigPath spelling the same file another way", func(t *testing.T) {
		t.Parallel()
		// The same file: dir/./b.yml. "an error when the file named is not the same one" -- being the same one, it should report no error
		same := filepath.Join(dir, ".") + string(filepath.Separator) + "." + string(filepath.Separator) + "b.yml"
		exit, seen, stderr := covAppRun(t, map[string]string{"COVAPP_WITH_CONFIG_PATH": same, "COVAPP_READ_EARLY": "1"}, "--config="+b)
		if exit.Code != 0 {
			// Ensure used to compare the strings, so the same file spelled another way (./, a relative path, a symbolic link) counted as another file
			t.Errorf("the same file spelled another way should report no error, got %v\n%s", exit, stderr)
		}
		if seen["in_run"] != "from-b" {
			t.Errorf("Start should have read from-b, got %q", seen["in_run"])
		}
	})
}
