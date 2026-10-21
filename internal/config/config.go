// Package config reads the configuration file and keeps the result here for whoever comes to take
// it.
//
// Three rules: the defaults are pre-filled in the structs; an unknown field is an error; an unset
// ${VAR} is an error.
//
// It may be taken at any time: the file is found and loaded on the first read, and every read
// afterwards gets the same one. So a caller never has to know when the configuration was loaded --
// in main, before Run, or inside a hook, what is read is the final value from the configuration
// file. xone.Run only makes sure it has been loaded, and hands it back when it ends.
//
// "Who read which block" is only known at run time, and a top-level key nobody read is reported by
// Unclaimed once every hook has run.
package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"go.yaml.in/yaml/v3"

	"github.com/jupiter-eng/xone/xerror"
	"github.com/jupiter-eng/xone/xutil"
)

var placeholder = regexp.MustCompile(`\$\{([^}:]+)(?::([^}]*))?\}`)

var (
	mu       sync.Mutex
	sections map[string]*yaml.Node // a top-level key -> that block's node
	claimed  map[string]bool       // the keys Unmarshal / Has was asked about
	ready    bool                  // loaded: either a file was read, or none was found and everything uses its defaults
	source   string                // which file was loaded; empty when none was found
	failed   error                 // why loading failed, returned unchanged by every read afterwards

	// expanded maps a node whose placeholder was expanded to the text written in the configuration
	// (${DB_PASSWORD}, say), see checker.redact.
	// It is not under mu: DecodeStrict is exported through xconfig and a caller need not hold mu
	expanded atomic.Pointer[map[*yaml.Node]string]

	// origins maps every parsed node to the file it came from, see checker.at. Not under mu, for
	// expanded's reason
	origins atomic.Pointer[map[*yaml.Node]string]
)

// Ensure makes sure the configuration has been loaded, and is called by xone.Run at startup.
//
// path is the file WithConfigPath names, and left empty it is found in locate's order. When
// somebody read the configuration earlier (and it is loaded already), that one is reused: what was
// read early and what takes effect afterwards have to be one and the same. So a named file that is
// not that one is an error rather than a quiet swap.
func Ensure(path string, log *slog.Logger) error {
	mu.Lock()
	defer mu.Unlock()
	if !ready && failed == nil {
		return loadLocked(path, log)
	}
	if path != "" && !sameFile(path, source) {
		earlier := "when no config file was found"
		if source != "" {
			earlier = "from " + source
		}
		return xerror.Newf("xconfig", "config",
			"config was already loaded by an earlier read (%s), so %s cannot take effect: "+
				"pass the path with --%s or %s instead", earlier, path, ArgKey, EnvKey)
	}
	return failed
}

// sameFile reports whether two paths are the same file.
//
// They must not be compared as strings: the same file written as dir/./b.yml, as a relative path or
// through a symlink each used to count as another file, and Run failed startup over "another one
// was loaded already".
// The cleaned absolute paths are compared first; when both exist, os.SameFile decides (which covers
// symlinks and hard links).
func sameFile(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA == nil && errB == nil && absA == absB {
		return true
	}
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}

// ensureLocked finds and loads the file in locate's order on the first read. The caller holds mu
func ensureLocked() error {
	if ready || failed != nil {
		return failed
	}
	return loadLocked("", slog.Default())
}

// loadLocked finds and loads the file; the caller holds mu.
//
// A named file (WithConfigPath, --config, XONE_CONFIG) that cannot be found is an error; none of
// the conventional paths matching only warns and uses the defaults for everything -- which is
// reasonable for a service with no external dependency at all.
// The difference between the two is the difference between "what you asked for" and "what is
// conventional".
//
// A failure is recorded as well: a malformed configuration should report the same error on every
// read afterwards, rather than being re-read every time, or quietly counting as unconfigured on the
// second read.
func loadLocked(path string, log *slog.Logger) error {
	if failed = danglingArg(); failed != nil {
		return failed
	}
	from := "xone.WithConfigPath"
	if path == "" {
		path, from = locate()
	}
	source = path

	switch {
	case path == "":
		log.Warn("no config file found, using defaults for everything",
			"searched", SearchPaths, "or_use", "--"+ArgKey+"=<path> or "+EnvKey)
		Debugf("no config file: searched %s, everything uses its defaults", strings.Join(SearchPaths, ", "))
		sections, claimed, ready = nil, map[string]bool{}, true
		return nil
	case !xutil.FileExist(path):
		// locate returns a conventional path only when the file really exists, so whatever reaches
		// here was named explicitly
		failed = xerror.Newf("xconfig", "config", "config file does not exist: %s", path)
		return failed
	}

	log.Info("loading config", "file", path)
	s, r, err := load(path)
	if err != nil {
		failed = err
		return err
	}
	debugReport(path, from, r)
	sections, claimed, ready = s, map[string]bool{}, true
	return nil
}

// Load reads a configuration file and keeps the result in the package, replacing whatever was
// there. For the tests alone -- the production path goes through Ensure and the automatic load on
// the first read.
func Load(path string) error {
	s, _, err := load(path)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	sections, claimed, ready, source, failed = s, map[string]bool{}, true, path, nil
	return nil
}

// load reads the configuration file, parses, merges and expands the placeholders, and returns the
// node of each top-level key.
//
// What is read need not be one file: the base file may Import others, and an active profile brings
// application-{profile}.yml along.
// The merge rules and the priorities are in loadAll and merge.
//
// It also returns how this load went (which files were read, which profiles are active, the final
// configuration), which XONE_DEBUG prints.
func load(path string) (map[string]*yaml.Node, *report, error) {
	files, active, activeFrom, err := loadAll(path)
	if err != nil {
		return nil, nil, err
	}
	r := &report{profiles: active, profilesFrom: activeFrom}
	for _, f := range files {
		r.files = append(r.files, f.path)
	}
	// After merging, the fields of one block may come from several files, and an error has to say
	// which one
	from := map[*yaml.Node]string{}
	for _, f := range files {
		remember(f.node, f.path, from)
	}
	origins.Store(&from)

	var root *yaml.Node
	for _, f := range files {
		// Profiles is recognized in the base file alone, which loadAll took out already, so whatever
		// is left here was written by an imported file. Were those to activate profiles as well,
		// "who decides which files are loaded" would become a question depending on the loading order
		// that depends on it.
		// Writing it at all counts, even as an empty list -- it most likely came along while copying
		// base, and keeping it would only make it look as though it did something
		if takeFromApp(f.node, ProfilesKey) != nil {
			return nil, nil, xerror.Newf("xconfig", "config",
				"%s.%s may only be set in the base config file, found it in %s", AppKey, ProfilesKey, f.path)
		}
		root = merge(root, f.node)
	}
	if absent(root) {
		return nil, r, nil
	}

	// The placeholders are expanded once, after everything has been merged: a required ${VAR} in
	// base that a profile file overrode must no longer have to be set
	var missing []string
	values := map[*yaml.Node]string{}
	expand(root, &missing, values)
	if len(missing) > 0 {
		return nil, nil, xerror.Newf("xconfig", "config", "environment variables not set: %s", missingNames(missing))
	}
	expanded.Store(&values)
	r.root, r.raw = root, values

	s, err := topLevel(root)
	return s, r, err
}

// Unmarshal decodes the key's block into into and records that the block has been read.
//
// into holds the defaults already and a field the file does not set stays as it is -- which is why
// no pointer field is needed to tell "not configured" from "configured as the zero value". With the
// whole block unconfigured, into is left alone and nil is returned.
//
// It may be called at any time before Start: it loads first when nothing has been loaded, so what
// is read is always the final value from the configuration file, and reading early never quietly
// yields a set of defaults.
func Unmarshal(key string, into any) error {
	if err := checkTarget(into); err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()

	node, err := sectionLocked(key)
	if err != nil {
		return err
	}
	if node == nil {
		return nil // the block is unconfigured, or written empty; either way the defaults stay
	}
	if err := DecodeStrict(node, into); err != nil {
		return xerror.Newf("xconfig", "config", "invalid config %s: %w", key, err)
	}

	// Where a configuration struct can say what counts as valid, it says so -- catching a
	// misconfiguration before anything is built. Some misconfigurations do not fail initialization
	// and merely make a behavior unreachable (an XFlow RollbackTimeout of 0 has every rollback time
	// out the moment it begins and every compensation skipped, while the flow itself looks perfectly
	// fine), and those can only be caught here.
	if v, ok := into.(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return xerror.Newf("xconfig", "config", "invalid config %s: %w", key, err)
		}
	}
	return nil
}

// Has reports whether the configuration file holds this block. It is for "initialize it only when
// it is configured".
//
// It records a claim as Unmarshal does: asking counts as wanting it. Without that, an empty block
// such as "XRedis:" fails startup outright -- Has returns false, the package skips it and never
// Unmarshals, so the key is claimed by nobody, and neither reason Unclaimed reports (a misspelling,
// a forgotten import) applies, so following them finds nothing.
//
// As in Unmarshal, it loads first when nothing has been loaded. On a failed load it reports that
// there is none: that error is reported as it is by xone.Run at startup, and reporting it again
// here would only have every optional component say its own version of it.
func Has(key string) bool {
	mu.Lock()
	defer mu.Unlock()
	node, err := sectionLocked(key)
	return err == nil && node != nil
}

// sectionLocked takes the key's block out; the caller holds mu. Unmarshal, Has and
// UnmarshalClients share it.
//
// The order of the three steps is deliberate: load first when nothing has been loaded (so reading
// early gets the values from the file); record a claim once the load succeeded -- asking counts as
// wanting it, even when the block is unconfigured or empty; and only then look at whether anything
// was written. It returns nil for a block that is unconfigured or written empty.
// On a failed load it returns that error unchanged and records no claim.
func sectionLocked(key string) (*yaml.Node, error) {
	if err := ensureLocked(); err != nil {
		return nil, err
	}
	claimed[key] = true
	if node := sections[key]; !isEmptyNode(node) {
		return node, nil
	}
	return nil, nil
}

// Unclaimed returns the top-level keys of the configuration file that nobody read, sorted by name.
//
// The framework checks it once every BeforeStart hook has run: a key nobody read is most likely
// misspelled, or the matching integration was never imported -- either way somebody configures
// something for a while before finding it has no effect.
func Unclaimed() []string {
	mu.Lock()
	defer mu.Unlock()

	var out []string
	for key := range sections {
		if !claimed[key] {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// LoadInto loads a configuration file and takes the key's block out of it. For the tests alone.
func LoadInto(path, key string, into any) error {
	Reset()
	if err := Load(path); err != nil {
		return err
	}
	return Unmarshal(key, into)
}

// Reset goes back to "nothing has been loaded": the next read finds the file and loads it again.
//
// xone.Run calls it when it ends -- the configuration belongs to one Run, and the next Run in the
// same process (common in the tests) has to read one of its own. The tests use it directly as
// well.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	sections, claimed, ready, source, failed = nil, nil, false, "", nil
	expanded.Store(nil)
	origins.Store(nil)
}

// remember records that n and every node below it came from path
func remember(n *yaml.Node, path string, into map[*yaml.Node]string) {
	into[n] = path
	for _, c := range n.Content {
		remember(c, path, into)
	}
}

func topLevel(root *yaml.Node) (map[string]*yaml.Node, error) {
	out := map[string]*yaml.Node{}
	if len(root.Content) == 0 || isNull(root.Content[0]) {
		return out, nil // a file holding one --- alone
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil, xerror.New("xconfig", "config", errors.New("top level of the config file must be a mapping"))
	}
	// A duplicate key is impossible by now: every file was checked while it was parsed, see parse
	for i := 0; i+1 < len(doc.Content); i += 2 {
		out[doc.Content[i].Value] = doc.Content[i+1]
	}
	return out, nil
}

// isEmptyNode reports whether a configuration block holds nothing at all.
//
// `Demo:` with nothing after it parses into a null scalar, and `Demo: {}` is an empty mapping.
// Both should mean "the block is unconfigured" -- handed to the decoder directly, the former gives
// an EOF error that says nothing, and the latter passes while there was no point going through
// it.
func isEmptyNode(n *yaml.Node) bool {
	if n == nil {
		return true
	}
	if isNull(n) {
		return true
	}
	return (n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode) && len(n.Content) == 0
}

// expand expands the placeholders on the parsed nodes rather than replacing text in the raw bytes:
// an environment variable whose value holds a colon or a line break would have a text replacement
// change the YAML structure.
//
// With values not nil it records every expanded scalar and the text it was written as, so that an
// error masks the expanded value per node.
func expand(n *yaml.Node, missing *[]string, values map[*yaml.Node]string) {
	if n.Kind == yaml.ScalarNode && strings.Contains(n.Value, "${") {
		before := n.Value
		n.Value = placeholder.ReplaceAllStringFunc(n.Value, func(m string) string {
			idx := placeholder.FindStringSubmatchIndex(m)
			name := m[idx[2]:idx[3]]
			if v, ok := os.LookupEnv(name); ok {
				return v
			}
			if idx[4] >= 0 {
				return m[idx[4]:idx[5]]
			}
			*missing = append(*missing, name)
			return m
		})
		if n.Value != before {
			retag(n)
			if values != nil {
				values[n] = before
			}
		}
	}
	for _, c := range n.Content {
		expand(c, missing, values)
	}
}

// missingNames are the unset variables, sorted by name and listed once each: one variable used in
// several places need not be read several times in an error
func missingNames(missing []string) string {
	slices.Sort(missing)
	return strings.Join(slices.Compact(missing), ", ")
}

// retag has a scalar that was replaced decide its type again from its new content.
//
// While parsing, the whole ${PORT:8080} is one piece of text, so the scalar was tagged !!str.
// After the replacement its content is 8080 while the tag is still !!str, so
//
//	Port: ${PORT:8080}
//
// would fail with "cannot unmarshal !!str into int" -- which would leave placeholders usable in
// string fields alone, while the documentation presents them as a general rule. Clearing the tag
// and letting yaml decide again from the replaced content is all it takes.
//
// This does not let an environment variable's value change the YAML structure: what is decided
// again is still this one scalar, and a colon, a line break or an asterisk in the value all stay
// inside it.
//
// There are three cases:
//
// A caller who wrote quotes or a tag explicitly (a Style other than 0) keeps it as it is: that is
// an explicit "treat it as a string", which a numeric-looking password or version depends on.
//
// A replacement that comes out empty (`${PORT:}`, or a variable set to an empty string) is tagged
// null, that is "this field was not written": while decoding, null leaves a field at the default
// pre-filled in the struct, whatever its type. Kept as !!str, a string field would be cleared to an
// empty string and a non-string field would fail with cannot unmarshal !!str "" into int -- the
// same placeholder with two different fates depending on the field. An empty string that is really
// wanted is written with quotes: `"${PW:}"`.
//
// Merging happens before expanding, so the "not written" here falls back to the struct's default
// and not to the value a lower-priority file wrote.
//
// Everything else has its tag cleared and decided again, while an expanded value is never null: a
// variable whose value happens to be one of YAML's null spellings (null, ~, Null) would be taken as
// "not written" when decided again, and the field would quietly stay at its default.
// That case is fixed at !!str -- a string field gets the literal and any other field gets a type
// error that reads sensibly.
func retag(n *yaml.Node) {
	switch {
	case n.Style != 0: // quotes or a tag: kept as it is
	case n.Value == "":
		n.Tag = "!!null"
	default:
		n.Tag = ""
		if n.ShortTag() == "!!null" {
			n.Tag = "!!str"
		}
	}
}

// profilesOf takes XApp.Profiles out of the document, removes it, and returns the profiles it
// declares.
//
// The placeholders are expanded right here, for the reason an Import path is: which profiles are
// active has to be known before which files to read is, and that cannot wait for everything to be
// merged. So `Profiles: ${APP_ENV:dev}` works.
func profilesOf(doc *yaml.Node, path string) ([]string, error) {
	node := takeFromApp(doc, ProfilesKey)
	if node == nil {
		return nil, nil
	}

	var missing []string
	expand(node, &missing, nil)
	if len(missing) > 0 {
		return nil, xerror.Newf("xconfig", "config",
			"environment variables not set in %s.%s of %s: %s", AppKey, ProfilesKey, path, missingNames(missing))
	}

	active, ok := scalarList(node)
	if !ok {
		return nil, xerror.Newf("xconfig", "config",
			"%s.%s in %s must be a profile name, a comma-separated string or a list", AppKey, ProfilesKey, path)
	}

	// A list and a comma-separated string are both accepted, as in Spring:
	// Profiles: "dev,prod" parses into one element, which is cut apart here
	out := make([]string, 0, len(active))
	for _, a := range active {
		out = append(out, splitProfiles(a)...)
	}
	return out, nil
}
