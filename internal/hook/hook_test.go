package hook

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func noop(context.Context) error { return nil }

// board joins a set of registrations into "name name ..." so that the order can be asserted on
func board(es []Entry) string {
	names := make([]string, 0, len(es))
	for _, e := range es {
		names = append(names, e.Name)
	}
	return strings.Join(names, " ")
}

func entry(name string, s Stage) Entry {
	return Entry{Name: name, Pkg: name, Stage: s, Run: noop}
}

func TestStartOrder_AscendingByStage(t *testing.T) {
	got := board(startOrder([]Entry{
		entry("server", StageServer),
		entry("db", StageClient),
		entry("log", StageLog),
		entry("trace", StageTelemetry),
	}))
	if want := "log trace db server"; got != want {
		t.Errorf("startup goes from the deepest layer upwards\nwant %s\ngot  %s", want, got)
	}
}

func TestStartOrder_KeepsRegistrationOrderWithinStage(t *testing.T) {
	// The stable sort is deliberate: within one stage whatever registered first comes up first, and
	// with an unstable sort the order would change with the implementation, while the caller
	// understands it by the order of the imports.
	//
	// The input has to be long enough and out of order to begin with: the standard library's sort
	// takes an insertion sort up to 12 items and lets an already sorted input straight through -- it
	// happens to be stable in both cases, so three or five items in order prove nothing about the
	// difference between slices.SortFunc and slices.SortStableFunc
	stages := []Stage{StageTelemetry, StageClient, StageServer}
	const per = 8

	var in []Entry
	for i := 0; i < per; i++ { // registered round-robin by stage, so that the input is not in order
		for _, s := range stages {
			in = append(in, entry(fmt.Sprintf("s%d-%d", s, i), s))
		}
	}

	var want []string
	for _, s := range stages {
		for i := 0; i < per; i++ {
			want = append(want, fmt.Sprintf("s%d-%d", s, i))
		}
	}

	if got := board(startOrder(in)); got != strings.Join(want, " ") {
		t.Errorf("nothing within one stage may be reordered\nwant %s\ngot  %s", strings.Join(want, " "), got)
	}
}

func TestStopOrder_MirrorsStartOrder(t *testing.T) {
	// The promise that "declaring one stage gives both starting earlier and closing later" rests
	// entirely on this being the whole thing reversed. Sorting by descending stage without reversing
	// the order within one stage would close whatever came up first in that stage first
	in := []Entry{
		entry("log", StageLog),
		entry("trace", StageTelemetry),
		entry("db", StageClient),
		entry("cache", StageClient),
		entry("server", StageServer),
	}
	if got, want := board(stopOrder(in)), "server cache db trace log"; got != want {
		t.Errorf("shutdown goes from the highest layer downwards, and within one stage whatever came up last closes first\nwant %s\ngot  %s", want, got)
	}
}

func TestStopOrder_SymmetricWithStartOrderPerItem(t *testing.T) {
	in := []Entry{
		entry("a", StageClient),
		entry("b", StageLog),
		entry("c", StageServer),
		entry("d", StageClient),
		entry("e", StageTelemetry),
	}
	start, stop := startOrder(in), stopOrder(in)
	if len(start) != len(stop) {
		t.Fatalf("the two orders must be the same length, got %d vs %d", len(start), len(stop))
	}
	for i := range start {
		if j := len(stop) - 1 - i; start[i].Name != stop[j].Name {
			t.Errorf("item %d is not symmetric: start %s, and stop at the matching position is %s", i, start[i].Name, stop[j].Name)
		}
	}
}

func TestStartOrder_DoesNotModifyInput(t *testing.T) {
	// Were the sort to land on the caller's backing array, the global registration board would be
	// quietly reordered by every read -- and every later Start() would return a different order
	in := []Entry{entry("server", StageServer), entry("log", StageLog)}
	startOrder(in)
	stopOrder(in)
	if got, want := board(in), "server log"; got != want {
		t.Errorf("the argument was modified\nwant %s\ngot  %s", want, got)
	}
}

func TestStart_ReturnsCopy(t *testing.T) {
	t.Cleanup(Reset)
	Reset()
	AddStart(entry("a", StageClient))
	AddStart(entry("b", StageClient))

	got := Start()
	got[0], got[1] = got[1], got[0] // the caller casually reorders the copy it was given

	if want := "a b"; board(Start()) != want {
		t.Errorf("the registration board was changed by the caller, want %s, got %s", want, board(Start()))
	}
}

func TestAddStart_RetrievedByStageAfterRegister(t *testing.T) {
	t.Cleanup(Reset)
	Reset()
	AddStart(entry("db", StageClient))
	AddStart(entry("log", StageLog))
	AddStop(entry("db", StageClient))
	AddStop(entry("log", StageLog))

	if got, want := board(Start()), "log db"; got != want {
		t.Errorf("Start want %s, got %s", want, got)
	}
	if got, want := board(Stop()), "db log"; got != want {
		t.Errorf("Stop want %s, got %s", want, got)
	}
}

func TestReset_ClearsBothBoards(t *testing.T) {
	t.Cleanup(Reset)
	AddStart(entry("a", StageClient))
	AddStop(entry("a", StageClient))
	Reset()
	if len(Start()) != 0 || len(Stop()) != 0 {
		t.Errorf("both boards must be empty after Reset, got start=%d stop=%d", len(Start()), len(Stop()))
	}
}

func TestAddStart_ConcurrentRegisterLosesNothing(t *testing.T) {
	// The integrations' init() functions run one after another, but the framework must not count on
	// it: a caller may perfectly well register from a goroutine of their own
	t.Cleanup(Reset)
	Reset()

	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			AddStart(entry("x", StageClient))
			AddStop(entry("x", StageClient))
		}()
	}
	wg.Wait()

	if len(Start()) != n || len(Stop()) != n {
		t.Errorf("registrations were lost, want %d, got start=%d stop=%d", n, len(Start()), len(Stop()))
	}
}

func TestAddStop_PairsWithLatestPriorStartHookInSamePackage(t *testing.T) {
	// Registered together makes a pair: open with close, and the second set each with its own.
	// Another package's, and anything registered after it, do not count
	Reset()
	t.Cleanup(Reset)
	AddStart(Entry{Name: "a.openA", Pkg: "a", Run: noop})
	AddStart(Entry{Name: "b.open", Pkg: "b", Run: noop})
	AddStop(Entry{Name: "a.closeA", Pkg: "a", Run: noop})
	AddStart(Entry{Name: "a.openB", Pkg: "a", Run: noop})
	AddStop(Entry{Name: "a.closeB", Pkg: "a", Run: noop})
	AddStop(Entry{Name: "c.flush", Pkg: "c", Run: noop})

	seqOf := map[string]int{}
	for _, s := range Start() {
		if s.Seq == 0 { // 0 is reserved for "unpaired": were a start hook numbered 0 to fail, its stop hook would be called anyway
			t.Errorf("%s has registration number 0; they are counted from 1", s.Name)
		}
		seqOf[s.Name] = s.Seq
	}
	want := map[string]int{"a.closeA": seqOf["a.openA"], "a.closeB": seqOf["a.openB"], "c.flush": 0}
	for _, e := range Stop() {
		if e.Pair != want[e.Name] {
			t.Errorf("%s must pair with the start hook numbered %d (0 means unpaired), got=%d", e.Name, want[e.Name], e.Pair)
		}
	}
}

func TestStopAfter_SkipsStopHooksWhoseStartHookFailed(t *testing.T) {
	// The ones whose paired start hook did not succeed are skipped and the unpaired ones are always
	// kept; what is kept stays in stop order
	Reset()
	t.Cleanup(Reset)
	AddStart(Entry{Name: "a.open", Pkg: "a", Stage: StageClient, Run: noop})
	AddStop(Entry{Name: "a.close", Pkg: "a", Stage: StageClient, Run: noop})
	AddStart(Entry{Name: "b.open", Pkg: "b", Stage: StageClient, Run: noop})
	AddStop(Entry{Name: "b.close", Pkg: "b", Stage: StageClient, Run: noop})
	AddStop(Entry{Name: "c.flush", Pkg: "c", Stage: StageLog, Run: noop})
	AddStart(Entry{Name: "d.open", Pkg: "d", Stage: StageServer, Run: noop})
	AddStop(Entry{Name: "d.close", Pkg: "d", Stage: StageServer, Run: noop})

	seqOf := map[string]int{}
	for _, s := range Start() {
		seqOf[s.Name] = s.Seq
	}
	started := map[int]bool{seqOf["a.open"]: true, seqOf["d.open"]: true} // b.open failed

	if got, want := board(StopAfter(started)), "d.close a.close c.flush"; got != want {
		t.Errorf("StopAfter want %s, got %s", want, got)
	}
	if got, want := board(StopAfter(nil)), "c.flush"; got != want {
		t.Errorf("with no start hook succeeding, only the unpaired ones are left, want %s, got %s", want, got)
	}
}

func TestAddStop_WithoutExplicitStageFollowsPairedStartHook(t *testing.T) {
	// One pair of hooks minds one resource: with the stage declared on the start hook alone, closing
	// belongs in that stage too
	Reset()
	t.Cleanup(Reset)
	AddStart(Entry{Name: "a.open", Pkg: "a", Stage: StageClient, Run: noop})
	AddStop(Entry{Name: "a.close", Pkg: "a", Stage: StageBusiness, Inherit: true, Run: noop})
	AddStop(Entry{Name: "a.flush", Pkg: "a", Stage: StageServer, Run: noop})
	AddStop(Entry{Name: "c.flush", Pkg: "c", Stage: StageBusiness, Inherit: true, Run: noop})

	want := map[string]Stage{
		"a.close": StageClient,   // inherited
		"a.flush": StageServer,   // one written explicitly is left alone
		"c.flush": StageBusiness, // found no pair and stays as it was
	}
	for _, e := range Stop() {
		if e.Stage != want[e.Name] {
			t.Errorf("%s must be in stage %v, got=%v", e.Name, want[e.Name], e.Stage)
		}
	}
}

func TestStage_BusinessIsAfterClientBeforeServer(t *testing.T) {
	// The relative order of the five stages is part of what is promised outwards and rests on the
	// order the iota is written in. Using xgorm.C() directly inside a business hook rests on
	// StageClient < StageBusiness
	if !(StageLog < StageTelemetry && StageTelemetry < StageClient &&
		StageClient < StageBusiness && StageBusiness < StageServer) {
		t.Errorf("the stage order is wrong: log=%d telemetry=%d client=%d business=%d server=%d",
			StageLog, StageTelemetry, StageClient, StageBusiness, StageServer)
	}
}
