package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/jupiter-eng/xone/xerror"
)

// AppKey is the application-level configuration block: what the application is called (Name and
// Version, which xapp reads) and which environment it runs in and which files its configuration
// comes from (Profiles and Import, which the loader reads itself). It mirrors Spring gathering
// spring.application.name, spring.profiles.active and spring.config.import under spring.
const AppKey = "XApp"

// The two fields of XApp the loader consumes itself. They are taken out of the document once
// read, so what reaches xapp is Name and Version alone
const (
	// ProfilesKey declares which profiles are active, as Spring's spring.profiles.active does
	ProfilesKey = "Profiles"
	// ImportKey imports other configuration files, as Spring's spring.config.import does
	ImportKey = "Import"
)

// optionalPrefix: an imported file carrying this prefix is skipped rather than reported when it
// does not exist
const optionalPrefix = "optional:"

// maxImportDepth is the upper bound on nested imports, keeping a configuration from growing an
// invisible deep chain.
// A cycle never reaches it: one file is read once, and a cycle breaks the second time it is met
const maxImportDepth = 16

// maxResolvedNodes is how many nodes one file may hold once its aliases are expanded.
// An ordinary configuration holds a few hundred; this line guards against the "billion laughs" of
// aliases multiplying layer by layer -- ten layers of ten aliases each expand into ten billion
// nodes
const maxResolvedNodes = 100_000

// loaded is one configuration file once loaded
type loaded struct {
	path string
	node *yaml.Node // the document node
}

// loadAll lists every file to be merged, in priority order from lowest to highest.
//
// The order matches Spring's:
//
//	application.yml  <  what it imports  <  application-prod.yml  <  what prod imports
//
// That is, an imported file overrides the one importing it (Spring puts it as "an import behaves as
// though inserted directly below the document declaring it", where what is below overrides what is
// above), and a profile file overrides the one without a profile.
//
// It also returns the active profiles and where they came from, which XONE_DEBUG prints.
func loadAll(base string) ([]loaded, []string, string, error) {
	seen := map[string]bool{} // one file is imported once, as in Spring
	doc, err := read(base, true, seen)
	if err != nil {
		return nil, nil, "", err
	}

	// The profiles come from three places, in priority order: the command line > the environment >
	// XApp.Profiles in the base file.
	// Consume the file's field even when overridden, but do not expand or validate an inactive
	// value: its placeholders need not exist when the command line or environment replaces it.
	active, from := profiles(nil)
	if from == "--"+ProfileArgKey || from == ProfileEnvKey {
		takeFromApp(doc, ProfilesKey)
	} else {
		active, err = profilesOf(doc, base)
		if err != nil {
			return nil, nil, "", err
		}
	}

	// The main file's profile variant has to exist: a profile file named and not there is almost
	// always a misspelled name, and skipping it quietly brings up a configuration nobody ever looked
	// at, on the defaults.
	// An imported file's variant is optional, see fileSet
	files, err := fileSet(base, doc, true, active, seen, 0)
	return files, active, from, err
}

// fileSet is one file already read along with what it imports and its profile variants, in
// priority order from lowest to highest:
//
//	F, F's imports, F-p1, F-p1's imports, F-p2, ...
//
// variantRequired says whether "F-p1 does not exist" counts as an error. It does for the main file
// and does not for an imported one: split into ten fragments, requiring every fragment to carry a
// variant for every environment is unusable, and a misspelled profile name is caught by the main
// file's variant already.
func fileSet(path string, doc *yaml.Node, variantRequired bool, profiles []string, seen map[string]bool, depth int) ([]loaded, error) {
	out, err := withImports(path, doc, profiles, seen, depth)
	if err != nil {
		return nil, err
	}
	for _, p := range profiles {
		vp := profilePath(path, p)
		v, err := read(vp, variantRequired, seen)
		if err != nil {
			return nil, err
		}
		if v == nil {
			continue
		}
		// A variant may import as well, so it goes through withImports too
		nested, err := withImports(vp, v, profiles, seen, depth)
		if err != nil {
			return nil, err
		}
		out = append(out, nested...)
	}
	return out, nil
}

// withImports is one file already read, followed by the files it imports (which therefore override
// it)
func withImports(path string, doc *yaml.Node, profiles []string, seen map[string]bool, depth int) ([]loaded, error) {
	imports, err := importsOf(doc, path)
	if err != nil {
		return nil, err
	}

	out := []loaded{{path: path, node: doc}}
	for _, spec := range imports {
		target, optional := strings.CutPrefix(spec, optionalPrefix)
		// A relative path resolves against "the directory of the file importing it" rather than the
		// process's working directory: moving the whole configuration directory elsewhere must not
		// break the relative references inside it
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		if depth+1 > maxImportDepth {
			return nil, xerror.Newf("xconfig", "config",
				"import nested more than %d levels deep at %s", maxImportDepth, target)
		}
		d, err := read(target, !optional, seen)
		if err != nil {
			return nil, err
		}
		if d == nil {
			continue
		}
		// An imported file has profile variants too, but a missing variant is not an error
		nested, err := fileSet(target, d, false, profiles, seen, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, nested...)
	}
	return out, nil
}

// read reads and parses one file. It returns nil for one already read, and for one that is not
// required and is not there
func read(path string, required bool, seen map[string]bool) (*yaml.Node, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if seen[abs] {
		// One file counts once, as in Spring. A diamond (two fragments both importing the same shared
		// configuration) and a cycle both break here, so the latter is never an error
		return nil, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if !required && os.IsNotExist(err) {
			return nil, nil
		}
		return nil, xerror.Newf("xconfig", "config", "read config %s: %w", path, err)
	}
	seen[abs] = true
	return parse(path, raw)
}

// importsOf takes XApp.Import out of the document and removes it.
//
// A ${VAR} in a path is expanded right here: which file to read has to be known before anything can
// be merged.
// The other fields are expanded once everything has been merged -- a required ${VAR} in base that a
// profile file overrode must no longer have to be set.
func importsOf(doc *yaml.Node, path string) ([]string, error) {
	node := takeFromApp(doc, ImportKey)
	if node == nil {
		return nil, nil
	}

	var missing []string
	expand(node, &missing, nil)
	if len(missing) > 0 {
		return nil, xerror.Newf("xconfig", "config",
			"environment variables not set in %s.%s of %s: %s", AppKey, ImportKey, path, missingNames(missing))
	}

	out, ok := scalarList(node)
	if !ok {
		return nil, xerror.Newf("xconfig", "config",
			"%s.%s in %s must be a path or a list of paths", AppKey, ImportKey, path)
	}
	return out, nil
}

// scalarList takes a value shaped as "one string, or a list of strings".
// null and an empty string count as none; a shape that does not match gives ok false
func scalarList(n *yaml.Node) (out []string, ok bool) {
	switch n.Kind {
	case 0: // a field decoded into a yaml.Node that was never written at all
		return nil, true
	case yaml.ScalarNode:
		if isNull(n) || n.Value == "" {
			return nil, true
		}
		return []string{n.Value}, true
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return nil, false
			}
			out = append(out, c.Value)
		}
		return out, true
	}
	return nil, false
}

// parse parses one configuration file and does, along the way, the things that can only be done
// before merging.
//
//   - one file may hold one document, see onlyDocument.
//   - a duplicate key is an error, with its file and line. Merging aligns keys by name and a
//     duplicate is swallowed at that step, after which the strict decoding never sees it again --
//     so the same block written twice loads fine, quietly going by the second one, while whoever
//     wrote it most likely believes both took effect. Only the first file used to be caught by the
//     strict decoding, and the profile files and the imported ones were all missed.
//   - an alias is replaced with what it points at. Each top-level block is decoded separately, and
//     an alias pointing at an anchor inside another block would find the anchor gone while that
//     block is decoded (unknown anchor); once expanded, a profile file can also override the fields
//     an alias brought in.
//   - the merge key `<<` is flattened into ordinary keys, see flattenMerges.
func parse(path string, raw []byte) (*yaml.Node, error) {
	doc, err := onlyDocument(raw)
	if err != nil {
		return nil, xerror.Newf("xconfig", "config", "parse config %s: %w", path, err)
	}
	if err := checkDuplicates(&doc); err != nil {
		return nil, xerror.Newf("xconfig", "config", "config %s: %w", path, err)
	}
	budget := maxResolvedNodes
	out, err := resolveAliases(&doc, &budget)
	if err != nil {
		return nil, xerror.Newf("xconfig", "config", "config %s: %w", path, err)
	}
	if err := flattenMerges(out); err != nil {
		return nil, xerror.Newf("xconfig", "config", "config %s: %w", path, err)
	}
	return out, nil
}

// onlyDocument parses the one document in raw; an empty file (holding only comments counts) gives a
// node whose Kind is 0.
//
// Several documents separated by --- inside one file are not supported: yaml.Unmarshal reads the
// first one alone and drops the rest quietly, so not even "a configuration block nobody reads" is
// reported -- while whoever wrote it believes that part took effect.
// Per-environment settings are written as application-{profile}.yml, so this is an error outright,
// pointing there.
//
// An "empty" document does not count as a second one (measured on go.yaml.in/yaml/v3 v3.0.4): a ---
// at the start and an extra --- at the end (with nothing after it, only comments, or a ~) all parse
// into one null, and a trailing ... produces no document at all. Those load as usual.
func onlyDocument(raw []byte) (yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	found := false
	for {
		var d yaml.Node
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			return doc, nil
		}
		if err != nil {
			return yaml.Node{}, err
		}
		if len(d.Content) == 0 || isNull(d.Content[0]) {
			if !found && doc.Kind == 0 {
				doc = d // with every document empty, the first is kept: "a file holding one ---" is still it
			}
			continue
		}
		if found {
			return yaml.Node{}, fmt.Errorf("multiple YAML documents in one file are not supported "+
				"(another document starts at line %d): put per-environment settings in application-{profile}.yml instead", d.Line)
		}
		doc, found = d, true
	}
}

// checkDuplicates finds duplicate keys inside one mapping
func checkDuplicates(n *yaml.Node) error {
	if n.Kind == yaml.MappingNode {
		lines := map[string]int{} // key -> the line it first appeared on
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i]
			if prev, dup := lines[key.Value]; dup {
				return fmt.Errorf("duplicate key %q at line %d (first seen at line %d)", key.Value, key.Line, prev)
			}
			lines[key.Value] = key.Line
		}
	}
	for _, c := range n.Content {
		if err := checkDuplicates(c); err != nil {
			return err
		}
	}
	return nil
}

// resolveAliases returns a copy of n with every alias replaced by what it points at.
//
// Every reference gets a copy of its own rather than sharing one node: placeholders are expanded
// per node, and sharing would expand the same value twice, which goes wrong as soon as an
// environment variable's value happens to hold a ${.
func resolveAliases(n *yaml.Node, budget *int) (*yaml.Node, error) {
	if n.Kind == yaml.AliasNode {
		return resolveAliases(n.Alias, budget)
	}
	if *budget--; *budget < 0 {
		return nil, fmt.Errorf("alias expansion exceeds %d nodes", maxResolvedNodes)
	}
	out := *n
	out.Anchor = "" // the aliases are expanded, and keeping the anchor would only add a definition nobody references when serializing
	out.Content = make([]*yaml.Node, len(n.Content))
	for i, c := range n.Content {
		r, err := resolveAliases(c, budget)
		if err != nil {
			return nil, err
		}
		out.Content[i] = r
	}
	return &out, nil
}

// flattenMerges flattens the merge key `<<` of every mapping into ordinary keys, changing n in
// place.
//
// yaml's rule is "a key written out overrides one merged in with <<", and that rule belongs inside
// one file alone. Left to the decoder until several files have been merged, an Addr a profile
// file's `<<: *prod` brought in would lose to an Addr written out in base -- the higher-priority
// file would be the one that did not take effect.
// Flattened while parsing, merging sees ordinary keys alone and what overrides what goes entirely
// by the files' priority.
//
// The rules match yaml.v3's while decoding (measured on go.yaml.in/yaml/v3 v3.0.4):
//   - a key written out overrides one merged in, whether it is written before or after the <<;
//   - with a list after the <<, an earlier entry overrides a later one;
//   - only that one level is merged in: a Nested written out is used whole and is not merged with
//     the Nested brought in;
//   - a mapping merged in may carry a << of its own, which is flattened first.
//
// The aliases were expanded into copies of their own before this is called (see resolveAliases), so
// changing them in place reaches nowhere else.
func flattenMerges(n *yaml.Node) error {
	for _, c := range n.Content {
		if err := flattenMerges(c); err != nil {
			return err
		}
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}

	own := map[string]bool{} // the keys written out
	merges := false
	for i := 0; i+1 < len(n.Content); i += 2 {
		if isMergeKey(n.Content[i]) {
			merges = true
		} else {
			own[n.Content[i].Value] = true
		}
	}
	if !merges {
		return nil
	}

	// A key merged in takes the << 's own position, so the node order matches what was read from the
	// file
	out := make([]*yaml.Node, 0, len(n.Content))
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if !isMergeKey(k) {
			out = append(out, k, v)
			continue
		}
		sources := []*yaml.Node{v}
		if v.Kind == yaml.SequenceNode {
			sources = v.Content
		}
		for _, src := range sources {
			if src.Kind != yaml.MappingNode {
				return fmt.Errorf("line %d: merge key << needs a mapping or a list of mappings", k.Line)
			}
			for j := 0; j+1 < len(src.Content); j += 2 {
				if key := src.Content[j].Value; !own[key] {
					own[key] = true // an earlier source overrides a later one
					out = append(out, src.Content[j], src.Content[j+1])
				}
			}
		}
	}
	n.Content = out
	return nil
}

// isMergeKey reports whether this is the merge key: a `<<` with no quotes (a quoted "<<" is an
// ordinary string key)
func isMergeKey(k *yaml.Node) bool {
	return k.Kind == yaml.ScalarNode && k.Value == "<<" && k.ShortTag() == "!!merge"
}

// takeFromApp takes one key out from under XApp and removes it from the document, returning nil
// when there is none.
//
// XApp left empty is removed along with it: an XApp holding only Profiles / Import would reach xapp
// as an empty block, and a program that never imported xapp would fail startup over "a
// configuration block nobody reads"
func takeFromApp(doc *yaml.Node, key string) *yaml.Node {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	i := keyIndex(root, AppKey)
	if i < 0 || root.Content[i+1].Kind != yaml.MappingNode {
		return nil
	}
	app := root.Content[i+1]
	val := removeKey(app, key)
	if val != nil && len(app.Content) == 0 {
		root.Content = append(root.Content[:i], root.Content[i+2:]...)
	}
	return val
}

// removeKey takes one key out of the mapping m and removes it, returning nil when there is none
func removeKey(m *yaml.Node, key string) *yaml.Node {
	i := keyIndex(m, key)
	if i < 0 {
		return nil
	}
	val := m.Content[i+1]
	m.Content = append(m.Content[:i], m.Content[i+2:]...)
	return val
}

// keyIndex is the index key sits at inside the mapping m (its value at i+1), or -1 when there is
// none.
// A duplicate key was caught while parsing (see checkDuplicates), so the first one is the only
// one
func keyIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

// profilePath derives application-prod.yml from application.yml, keeping the original file's
// directory and extension
func profilePath(base, profile string) string {
	ext := filepath.Ext(base)
	return strings.TrimSuffix(base, ext) + "-" + profile + ext
}
