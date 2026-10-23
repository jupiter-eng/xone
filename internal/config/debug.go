package config

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// DebugEnvKey, set to 1 / true / yes / on, has the framework print the startup for a person to
// read: which configuration was used and how it was found, which profiles are active, which files
// were read in priority order, the final configuration after merging (with the credentials masked),
// and the order of the start hooks.
//
// What it writes is multi-line text on stderr for a person to read, not JSON -- turn it on while
// tracking something down locally and keep it out of a production logging pipeline.
const DebugEnvKey = "XONE_DEBUG"

// Debug reports whether XONE_DEBUG is on
func Debug() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(DebugEnvKey))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// DebugOut is where the debug output is written. The tests swap in a buffer
var DebugOut io.Writer = os.Stderr

// Debugf writes one line of debug output while XONE_DEBUG is on, prefixed with [xone debug]
func Debugf(format string, args ...any) {
	if Debug() {
		fmt.Fprintf(DebugOut, "[xone debug] "+format+"\n", args...)
	}
}

// report is how one load went, printed while XONE_DEBUG is on
type report struct {
	files        []string              // in priority order, lowest to highest
	profiles     []string              // the active profiles
	profilesFrom string                // where the profiles came from
	root         *yaml.Node            // the whole configuration once merged and expanded; nil means nothing was written
	raw          map[*yaml.Node]string // a node whose placeholder was expanded -> the text written in the configuration, which is what is printed
}

// debugReport writes how one load went into DebugOut
func debugReport(base, from string, r *report) {
	if !Debug() {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[xone debug] config file: %s (%s)\n", base, from)
	if len(r.profiles) == 0 {
		b.WriteString("[xone debug] profiles: none\n")
	} else {
		fmt.Fprintf(&b, "[xone debug] profiles: %s (from %s)\n", strings.Join(r.profiles, ", "), r.profilesFrom)
	}
	b.WriteString("[xone debug] files read, lowest to highest priority:\n")
	for i, f := range r.files {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, f)
	}
	b.WriteString("[xone debug] effective config (secrets redacted):\n")
	if r.root == nil {
		b.WriteString("  (empty)\n")
	} else {
		var out strings.Builder
		enc := yaml.NewEncoder(&out)
		enc.SetIndent(2)
		if err := enc.Encode(redacted(r.root, r.raw)); err != nil {
			fmt.Fprintf(&b, "  (cannot render: %v)\n", err)
		} else {
			for _, l := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
				b.WriteString("  " + l + "\n")
			}
		}
	}
	fmt.Fprint(DebugOut, b.String())
}

// redactedValue is what a masked value is shown as
const redactedValue = "***"

// sensitiveKeys: a value whose key contains one of these words (lowercased and with _ - . and
// spaces removed before the comparison) is masked whole.
// key is not among them: KeyPrefix and KeyFile are not credentials, and masking them only hides
// what was configured
//
// It is deliberately shorter than the access log's word list (internal/web's defaultWords), and the
// difference is what the two are matching against. There a word is matched against a request field
// or a header name, where masking one too many costs a little context; here it is matched against a
// configuration key, and what is hidden is what somebody turned XONE_DEBUG on to read. cookie and
// session are the two that cannot come along for that reason: they would take CookieDomain,
// CookieSameSite, SessionTimeout and beego's SessionOn with them, none of which is a credential.
// authorization and signature have no such everyday spelling and are in.
var sensitiveKeys = []string{
	"password", "passwd", "secret", "token", "credential",
	"apikey", "accesskey", "privatekey", "authorization", "signature",
}

func sensitiveKey(k string) bool {
	k = strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(strings.ToLower(k))
	for _, s := range sensitiveKeys {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// Credentials hiding inside a value: the password in a DSN or a URL, and the credentials in a
// query string or a key=value spelling.
//
// A password holding @ or / (`postgres://u:p@ss@h`, `u:p/ss@h`, `u:p@ss@tcp(h)`) has to be masked
// up to the last @: masked up to the first, the rest of it would be printed along with the host.
// The price is masking the host as well when another @ follows in the same piece (an email address
// in a query string) -- the debug output would rather mask too much
var (
	urlUserinfo = regexp.MustCompile(`(://[^:/?#@\s]*):\S*@`) // postgres://u:p@h, redis://:p@h
	mysqlDSN    = regexp.MustCompile(`^([^:@/\s]+):\S*@`)     // u:p@tcp(h:3306)/db
	// One name=value pair inside a query string (?token=x&page=2) or a PG keyword spelling
	// (host=db password=x).
	// The value stops before a }: with the text shown as written, that is the closing brace of a
	// ${VAR:default} (a placeholder default never holds a }), and masking it along would print
	// ${OPT:https://h/?api_key=*** with the /path behind it gone as well
	kvPair = regexp.MustCompile(`([A-Za-z0-9_.\-]+)=('[^']*'|[^\s&#;}]+)`)
)

func redactString(s string) string {
	s = urlUserinfo.ReplaceAllString(s, "$1:"+redactedValue+"@")
	if !strings.Contains(s, "://") {
		s = mysqlDSN.ReplaceAllString(s, "$1:"+redactedValue+"@")
	}
	return kvPair.ReplaceAllStringFunc(s, func(m string) string {
		name, _, _ := strings.Cut(m, "=")
		if sensitiveKey(name) || strings.EqualFold(name, "pwd") { // pwd is how an ODBC / SQL Server connection string spells it
			return name + "=" + redactedValue
		}
		return m
	})
}

// redacted returns a deep copy of n with the credentials masked and the comments removed; n itself
// is left alone.
// The comments are not kept: after merging there is no telling which file a comment was written in,
// and keeping it would only send somebody to the wrong file.
//
// raw maps a node whose placeholder was expanded to the text written in the configuration (see
// expand). Such a node shows the text `${VAR}` rather than the value it expanded to: ${VAR} is
// exactly the recommended spelling for a credential, and it need not sit under a key called
// password (a DSN, a webhook address). The text goes through the masking all the same:
// `${DSN:postgres://u:p@h}`'s default may hold a password too.
//
// Under a credential key the value is replaced with *** whatever its shape (a scalar, a list, a
// map); an empty one is not masked, so it is visible as unconfigured.
func redacted(n *yaml.Node, raw map[*yaml.Node]string) *yaml.Node {
	c := *n
	c.HeadComment, c.LineComment, c.FootComment = "", "", ""
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, ch := range n.Content {
		c.Content[i] = redacted(ch, raw)
	}
	if c.Kind == yaml.ScalarNode {
		if text, ok := raw[n]; ok {
			c.Value, c.Tag = text, "!!str"
		}
		c.Value = redactString(c.Value)
	}
	if c.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(c.Content); i += 2 {
			if !sensitiveKey(c.Content[i].Value) {
				continue
			}
			// Empty or not goes by the expanded value: `${PW:}` is empty when unset, and showing the
			// text as written would make it look configured
			if v := n.Content[i+1]; isEmptyNode(v) || v.Kind == yaml.ScalarNode && v.Value == "" {
				c.Content[i+1] = redacted(v, nil)
			} else {
				c.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: redactedValue}
			}
		}
	}
	return &c
}
