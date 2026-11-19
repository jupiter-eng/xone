package xutil

import (
	"math"
	"runtime/debug"
	"strings"
	"testing"
)

type envMode string

type envInner struct {
	Addr string
	Tags []string
}

type envBase struct {
	Region string
}

type envConfig struct {
	envBase
	Name    string
	Mode    envMode
	Port    int
	Inner   envInner
	Ptr     *envInner
	NilPtr  *envInner
	List    []envInner
	PtrList []*envInner
	Array   [2]string
	Labels  map[string]string
	ByName  map[string]envInner
	Any     any
	AnyPtr  any
	Mixed   map[string]any
	Raw     []byte
	hidden  string
	hiddenM map[string]string
}

func TestExpandEnv_ExpandsStringsAtEveryLevel(t *testing.T) {
	t.Setenv("XUTIL_HOST", "db.internal")
	t.Setenv("XUTIL_REGION", "eu")

	c := envConfig{
		envBase: envBase{Region: "${XUTIL_REGION}"},
		Name:    "${XUTIL_HOST}",
		Mode:    "${XUTIL_REGION}",
		Port:    5432,
		Inner:   envInner{Addr: "${XUTIL_HOST}:5432", Tags: []string{"${XUTIL_REGION}", "plain"}},
		Ptr:     &envInner{Addr: "${XUTIL_HOST}"},
		List:    []envInner{{Addr: "${XUTIL_HOST}"}},
		PtrList: []*envInner{{Tags: []string{"${XUTIL_REGION}"}}, nil},
		Array:   [2]string{"${XUTIL_HOST}", "plain"},
		Labels:  map[string]string{"${XUTIL_REGION}": "${XUTIL_REGION}"},
		ByName:  map[string]envInner{"a": {Addr: "${XUTIL_HOST}"}},
		Any:     envInner{Addr: "${XUTIL_HOST}"},
		AnyPtr:  &envInner{Addr: "${XUTIL_HOST}"},
		Mixed:   map[string]any{"s": "${XUTIL_HOST}", "n": 1, "l": []any{"${XUTIL_REGION}"}},
		Raw:     []byte("${XUTIL_HOST}"),
		hidden:  "${XUTIL_HOST}",
		hiddenM: map[string]string{"k": "${XUTIL_HOST}"},
	}
	if err := ExpandEnv(&c); err != nil {
		t.Fatalf("every variable is set, got err=%v", err)
	}

	got := map[string]string{
		"embedded struct":          c.Region,
		"string field":             c.Name,
		"named string type":        string(c.Mode),
		"nested struct":            c.Inner.Addr,
		"slice in nested struct":   c.Inner.Tags[0],
		"pointer to struct":        c.Ptr.Addr,
		"slice of structs":         c.List[0].Addr,
		"slice of pointers":        c.PtrList[0].Tags[0],
		"array":                    c.Array[0],
		"map value":                c.Labels["${XUTIL_REGION}"],
		"struct as map value":      c.ByName["a"].Addr,
		"struct in an interface":   c.Any.(envInner).Addr,
		"pointer in an interface":  c.AnyPtr.(*envInner).Addr,
		"string in map[string]any": c.Mixed["s"].(string),
		"slice in map[string]any":  c.Mixed["l"].([]any)[0].(string),
	}
	want := map[string]string{
		"embedded struct":          "eu",
		"string field":             "db.internal",
		"named string type":        "eu",
		"nested struct":            "db.internal:5432",
		"slice in nested struct":   "eu",
		"pointer to struct":        "db.internal",
		"slice of structs":         "db.internal",
		"slice of pointers":        "eu",
		"array":                    "db.internal",
		"map value":                "eu",
		"struct as map value":      "db.internal",
		"struct in an interface":   "db.internal",
		"pointer in an interface":  "db.internal",
		"string in map[string]any": "db.internal",
		"slice in map[string]any":  "eu",
	}
	for where, w := range want {
		if got[where] != w {
			t.Errorf("%s: got=%q want=%q", where, got[where], w)
		}
	}

	if c.Port != 5432 || c.Inner.Tags[1] != "plain" || c.Array[1] != "plain" || c.Mixed["n"] != 1 {
		t.Errorf("a value with no placeholder must stay as it is, got=%+v", c)
	}
	if len(c.Labels) != 1 {
		t.Errorf("a map key must not be expanded, got=%v", c.Labels)
	}
	if string(c.Raw) != "${XUTIL_HOST}" {
		t.Errorf("[]byte must be left alone, got=%q", c.Raw)
	}
	if c.hidden != "${XUTIL_HOST}" || c.hiddenM["k"] != "${XUTIL_HOST}" {
		t.Errorf("an unexported field must be left alone, got=%q %v", c.hidden, c.hiddenM)
	}
	if c.NilPtr != nil || c.PtrList[1] != nil {
		t.Error("a nil pointer must stay nil")
	}
}

func TestExpandEnv_PlaceholderForms(t *testing.T) {
	t.Setenv("XUTIL_SET", "value")
	t.Setenv("XUTIL_EMPTY", "")
	t.Setenv("XUTIL_NESTED", "${XUTIL_SET}")

	cases := []struct{ name, in, want string }{
		{"whole value", "${XUTIL_SET}", "value"},
		{"inside a longer string", "a-${XUTIL_SET}-b", "a-value-b"},
		{"several in one string", "${XUTIL_SET}/${XUTIL_SET}", "value/value"},
		{"set variable wins over the default", "${XUTIL_SET:other}", "value"},
		{"unset variable takes the default", "${XUTIL_UNSET:fallback}", "fallback"},
		{"default may hold a colon", "${XUTIL_UNSET:http://localhost:8080}", "http://localhost:8080"},
		{"default may be empty", "[${XUTIL_UNSET:}]", "[]"},
		{"variable set to empty wins over the default", "[${XUTIL_EMPTY:fallback}]", "[]"},
		{"replacement is not expanded again", "${XUTIL_NESTED}", "${XUTIL_SET}"},
		{"dollar without braces is plain text", "$XUTIL_SET", "$XUTIL_SET"},
		{"unclosed brace is plain text", "${XUTIL_SET", "${XUTIL_SET"},
		{"empty name is plain text", "${}", "${}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.in
			if err := ExpandEnv(&s); err != nil {
				t.Fatalf("got err=%v", err)
			}
			if s != tc.want {
				t.Errorf("got=%q want=%q", s, tc.want)
			}
		})
	}
}

func TestExpandEnv_UnsetWithoutDefaultIsAnErrorNamingEveryVariable(t *testing.T) {
	t.Setenv("XUTIL_SET", "value")

	c := envConfig{
		Name:  "${XUTIL_MISSING_B}",
		Inner: envInner{Addr: "${XUTIL_MISSING_A}", Tags: []string{"${XUTIL_MISSING_B}"}},
		Ptr:   &envInner{Addr: "${XUTIL_SET}"},
	}
	err := ExpandEnv(&c)
	if err == nil {
		t.Fatal("an unset variable with no default must be an error")
	}
	if !strings.HasSuffix(err.Error(), ": XUTIL_MISSING_A, XUTIL_MISSING_B") {
		t.Errorf("the error must name every unset variable, sorted and once each, got=%v", err)
	}
	if c.Name != "${XUTIL_MISSING_B}" || c.Inner.Addr != "${XUTIL_MISSING_A}" {
		t.Errorf("an unresolved placeholder must stay as written, got=%q %q", c.Name, c.Inner.Addr)
	}
	if c.Ptr.Addr != "value" {
		t.Errorf("the rest must still be expanded, got=%q", c.Ptr.Addr)
	}
}

func TestExpandEnv_RejectsWhatItCannotWriteTo(t *testing.T) {
	var nilPtr *envConfig
	for name, v := range map[string]any{
		"nil":             nil,
		"struct by value": envConfig{Name: "${XUTIL_HOST}"},
		"nil pointer":     nilPtr,
	} {
		if err := ExpandEnv(v); err == nil {
			t.Errorf("%s: must be an error, nothing could be written back", name)
		}
	}
}

type envNode struct {
	Value string
	Next  *envNode
	Kids  []any
	Index map[string]any
}

func TestExpandEnv_CycleIsWalkedOnce(t *testing.T) {
	t.Setenv("XUTIL_HOST", "db.internal")

	n := &envNode{Value: "${XUTIL_HOST}", Index: map[string]any{}}
	n.Next = n
	n.Kids = []any{n, nil}
	n.Kids[1] = n.Kids
	n.Index["self"] = n.Index
	n.Index["v"] = "${XUTIL_HOST}"

	if err := ExpandEnv(n); err != nil {
		t.Fatalf("got err=%v", err)
	}
	if n.Value != "db.internal" || n.Index["v"] != "db.internal" {
		t.Errorf("got=%q %q", n.Value, n.Index["v"])
	}
}

type envTree struct {
	Value string
	Kids  []envTree
}

type envPeer struct {
	Value string
	Peer  *envPeer
	Self  **envPeer
	Owner map[string]*envPeer
}

func TestExpandEnv_EveryKindOfCycleTerminates(t *testing.T) {
	t.Setenv("XUTIL_HOST", "db.internal")

	t.Run("slice of structs holding itself", func(t *testing.T) {
		s := make([]envTree, 1)
		s[0] = envTree{Value: "${XUTIL_HOST}", Kids: s}
		if err := ExpandEnv(&s); err != nil || s[0].Value != "db.internal" {
			t.Errorf("got=%q err=%v", s[0].Value, err)
		}
	})
	t.Run("two structs pointing at each other", func(t *testing.T) {
		a, b := &envPeer{Value: "${XUTIL_HOST}"}, &envPeer{Value: "${XUTIL_HOST}"}
		a.Peer, b.Peer = b, a
		a.Self = &a
		a.Owner = map[string]*envPeer{"a": a, "b": b}
		b.Owner = a.Owner
		if err := ExpandEnv(a); err != nil || a.Value != "db.internal" || b.Value != "db.internal" {
			t.Errorf("got=%q %q err=%v", a.Value, b.Value, err)
		}
	})
	t.Run("interface holding a pointer to itself", func(t *testing.T) {
		var a any
		a = &a
		if err := ExpandEnv(&a); err != nil {
			t.Errorf("got err=%v", err)
		}
	})
	t.Run("slice of interfaces holding itself by value", func(t *testing.T) {
		s := []any{"${XUTIL_HOST}", nil}
		s[1] = s
		if err := ExpandEnv(&s); err != nil || s[0] != "db.internal" {
			t.Errorf("got=%v err=%v", s[0], err)
		}
	})
	t.Run("map holding itself behind a struct in an interface", func(t *testing.T) {
		m := map[string]any{"v": "${XUTIL_HOST}"}
		m["self"] = struct{ M map[string]any }{m}
		if err := ExpandEnv(&m); err != nil || m["v"] != "db.internal" {
			t.Errorf("got=%v err=%v", m["v"], err)
		}
	})
}

// The stack limit is lowered for the test so that a chain of fifty thousand is enough to tell a walk that
// recurses (about a kilobyte of stack per node, measured: a million nodes ran out of the default 1 GB)
// from one that does not, without building a million nodes on every run.
func TestExpandEnv_LongChainDoesNotOverflowTheStack(t *testing.T) {
	t.Setenv("XUTIL_HOST", "db.internal")
	defer debug.SetMaxStack(debug.SetMaxStack(1 << 20))

	head := &envPeer{Value: "${XUTIL_HOST}"}
	tail := head
	for range 50_000 {
		tail.Peer = &envPeer{Value: "${XUTIL_HOST}"}
		tail = tail.Peer
	}
	if err := ExpandEnv(head); err != nil || tail.Value != "db.internal" {
		t.Errorf("got=%q err=%v", tail.Value, err)
	}
}

type envLabels map[string]string

type envShared struct {
	Name     string
	NamePtr  *string
	Any      any
	AnyPtr   *any
	All      []string
	Head     []string
	Labels   map[string]string
	LabelsBy envLabels
}

// A string reached along two paths must be expanded once: the second pass would expand what the
// first one put there.
func TestExpandEnv_ValueReachedTwiceIsExpandedOnce(t *testing.T) {
	t.Setenv("XUTIL_SET", "value")
	t.Setenv("XUTIL_NESTED", "${XUTIL_SET}")

	c := envShared{
		Name:   "${XUTIL_NESTED}",
		Any:    "${XUTIL_NESTED}",
		All:    []string{"${XUTIL_NESTED}", "${XUTIL_NESTED}"},
		Labels: map[string]string{"k": "${XUTIL_NESTED}"},
	}
	c.NamePtr = &c.Name
	c.AnyPtr = &c.Any
	c.Head = c.All[:1]
	c.LabelsBy = envLabels(c.Labels)

	if err := ExpandEnv(&c); err != nil {
		t.Fatalf("got err=%v", err)
	}
	for where, got := range map[string]any{
		"field and a pointer to it":           c.Name,
		"interface and a pointer to it":       c.Any,
		"two slices over one array":           c.All[0],
		"one map under two types":             c.Labels["k"],
		"the element only one slice reaches":  c.All[1],
		"the same string through the pointer": *c.NamePtr,
	} {
		if got != "${XUTIL_SET}" {
			t.Errorf("%s: got=%q want=%q", where, got, "${XUTIL_SET}")
		}
	}
}

func TestExpandEnv_NaNKeyDoesNotGrowTheMap(t *testing.T) {
	t.Setenv("XUTIL_HOST", "db.internal")

	nan := math.NaN()
	m := map[float64]string{nan: "${XUTIL_HOST}", 1: "${XUTIL_HOST}"}
	if err := ExpandEnv(&m); err != nil {
		t.Fatalf("got err=%v", err)
	}
	if len(m) != 2 || m[1] != "db.internal" {
		t.Errorf("an entry under NaN cannot be written back and must not be added again, got=%v", m)
	}
}

type envHidden struct {
	Zone   string
	Labels map[string]string
}

type envViaPointer struct {
	*envHidden
}

func TestExpandEnv_EmbeddedPointerToUnexportedStruct(t *testing.T) {
	t.Setenv("XUTIL_REGION", "eu")

	c := envViaPointer{&envHidden{Zone: "${XUTIL_REGION}", Labels: map[string]string{"k": "${XUTIL_REGION}"}}}
	if err := ExpandEnv(&c); err != nil {
		t.Fatalf("got err=%v", err)
	}
	if c.Zone != "eu" || c.Labels["k"] != "eu" {
		t.Errorf("promoted fields must be expanded, got=%q %v", c.Zone, c.Labels)
	}
	if err := ExpandEnv(&envViaPointer{}); err != nil {
		t.Errorf("a nil embedded pointer must be skipped, got err=%v", err)
	}
}

// A map with no placeholder in it must not be written to: under -race a write shows up as a data race
// with the reader, and without it the runtime aborts on a concurrent map read and write.
func TestExpandEnv_WritesOnlyWhatChanged(t *testing.T) {
	t.Setenv("XUTIL_HOST", "db.internal")

	type conf struct {
		Addr   string
		Shared map[string]any
		Kept   map[string]any
	}
	shared := map[string]any{"plain": "text", "n": 1, "inner": envInner{Addr: "plain"}}
	c := conf{Addr: "${XUTIL_HOST}", Shared: shared, Kept: map[string]any{"nil": nil, "p": (*envInner)(nil)}}

	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_ = shared["plain"]
			}
		}
	}()
	for range 200 {
		c.Addr = "${XUTIL_HOST}"
		if err := ExpandEnv(&c); err != nil || c.Addr != "db.internal" {
			t.Fatalf("got=%q err=%v", c.Addr, err)
		}
	}
	close(stop)
	<-done

	if v, ok := c.Kept["nil"]; !ok || v != nil {
		t.Errorf("an entry holding nil must stay, got=%v", c.Kept)
	}
}

func TestExpandEnv_NilEntriesSurviveAWriteBack(t *testing.T) {
	t.Setenv("XUTIL_HOST", "db.internal")

	m := map[string]any{"nil": nil, "p": (*envInner)(nil), "l": []any{nil, "${XUTIL_HOST}"}}
	ptrs := map[string]*envInner{"nil": nil, "set": {Addr: "${XUTIL_HOST}"}}
	if err := ExpandEnv(&m); err != nil {
		t.Fatalf("got err=%v", err)
	}
	if err := ExpandEnv(&ptrs); err != nil {
		t.Fatalf("got err=%v", err)
	}
	if v, ok := m["nil"]; !ok || v != nil || len(m) != 3 || m["l"].([]any)[1] != "db.internal" {
		t.Errorf("got=%v", m)
	}
	if v, ok := ptrs["nil"]; !ok || v != nil || ptrs["set"].Addr != "db.internal" {
		t.Errorf("got=%v", ptrs)
	}
}
