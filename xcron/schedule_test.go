package xcron

import (
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/cronexpr"
)

// This group pins down the behavior of cronexpr v1.1.3: parse / next and the README's "Behavior (measured)" rest on it.
// Whichever of these goes red after a cronexpr upgrade, the matching comment and documentation change with it.

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("no tzdata for %s: %v", name, err)
	}
	return loc
}

func TestCronexpr_NextFollowsLocationOfInput(t *testing.T) {
	berlin := mustLoad(t, "Europe/Berlin")
	e := cronexpr.MustParse("30 2 * * *")
	got := e.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, berlin))
	if want := time.Date(2026, 1, 1, 2, 30, 0, 0, berlin); !got.Equal(want) {
		t.Errorf("a Berlin time must be computed in Berlin: got=%v want=%v", got, want)
	}
	got = e.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if want := time.Date(2026, 1, 1, 2, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("a UTC time must be computed in UTC: got=%v want=%v", got, want)
	}
}

func TestCronexpr_FieldCounts(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 10, 0, 17, 0, time.UTC)
	// 6 fields: the extra one is the year, not seconds.
	if _, err := cronexpr.Parse("0 30 2 * * *"); err == nil || !strings.Contains(err.Error(), "hour field") {
		t.Errorf("with 6 fields the 2nd must be taken as the hour: err=%v", err)
	}
	if got := cronexpr.MustParse("30 2 * * * 2027").Next(t0); got.Year() != 2027 {
		t.Errorf("with 6 fields the last one is the year: got=%v", got)
	}
	// 7 fields: the extra leading one is seconds.
	if got := cronexpr.MustParse("15 30 2 * * * *").Next(t0); got.Second() != 15 {
		t.Errorf("with 7 fields the 1st is seconds: got=%v", got)
	}
	// 8 fields: everything after the 7th is dropped silently.
	if _, err := cronexpr.Parse("15 30 2 * * * * garbage"); err != nil {
		t.Errorf("with 8 fields the extra ones are not an error: err=%v", err)
	}
}

func TestCronexpr_RejectsTimeZonePrefixAndUnknownDescriptors(t *testing.T) {
	for _, spec := range []string{"CRON_TZ=Europe/Berlin 30 2 * * *", "TZ=UTC 0 * * * *", "@midnight", "@reboot", "@every 1m"} {
		if _, err := cronexpr.Parse(spec); err == nil {
			t.Errorf("cronexpr accepted %q, so parse's comment and this change together", spec)
		}
	}
	for _, d := range descriptors {
		if _, err := cronexpr.Parse(d); err != nil {
			t.Errorf("cronexpr no longer knows %s: %v", d, err)
		}
	}
}

func TestCronexpr_NeverFiringSpecReturnsZero(t *testing.T) {
	if got := cronexpr.MustParse("0 0 30 2 *").Next(time.Now()); !got.IsZero() {
		t.Errorf("30 February does not exist, so Next must be the zero value: got=%v", got)
	}
}

func TestCronexpr_NextIsStrictlyAfter(t *testing.T) {
	at := time.Date(2026, 1, 1, 10, 1, 0, 0, time.UTC)
	if got := cronexpr.MustParse("* * * * *").Next(at); !got.Equal(at.Add(time.Minute)) {
		t.Errorf("computed from an exactly matching moment, the next one must be strictly after it: got=%v", got)
	}
}

// Daylight saving: America/New_York goes forward from 02:00 to 03:00 on 2026-03-08 and back from 02:00 to 01:00 on 2026-11-01.
func TestCronexpr_DSTInNewYork(t *testing.T) {
	ny := mustLoad(t, "America/New_York")
	daily := cronexpr.MustParse("30 2 * * *")

	// 02:30 does not exist on the day the clock goes forward: that whole day is skipped rather than moved to 03:30.
	got := daily.Next(time.Date(2026, 3, 7, 12, 0, 0, 0, ny))
	if want := time.Date(2026, 3, 9, 2, 30, 0, 0, ny); !got.Equal(want) {
		t.Errorf("02:30 on the day the clock goes forward must be skipped: got=%v want=%v", got, want)
	}
	// 02:30 occurs only once on the day the clock goes back (EST).
	got = daily.Next(time.Date(2026, 10, 31, 12, 0, 0, 0, ny))
	if want := time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("02:30 on the day the clock goes back runs once: got=%v want=%v", got, want)
	}
	if n := daily.Next(got); n.Day() != 2 {
		t.Errorf("02:30 on the day the clock goes back must not run twice: next=%v", n)
	}

	// 01:30 occurs twice on the day the clock goes back (once in EDT and once in EST), and both run.
	early := cronexpr.MustParse("30 1 * * *").NextN(time.Date(2026, 10, 31, 12, 0, 0, 0, ny), 2)
	if len(early) != 2 || early[1].Sub(early[0]) != time.Hour {
		t.Errorf("01:30 on the day the clock goes back must run twice, an hour apart: got=%v", early)
	}
}

func TestParse_AcceptsStandardSpecs(t *testing.T) {
	for _, spec := range []string{"*/5 * * * *", "0 3 * * MON-FRI", " 30 2 * * * ", "@daily", "@hourly", "@yearly", "@annually", "@monthly", "@weekly", "@every 90s", "@every 10ms"} {
		if _, err := parse(spec); err != nil {
			t.Errorf("%q: %v", spec, err)
		}
	}
}

func TestParse_RejectsWithClearReason(t *testing.T) {
	cases := map[string]string{
		"":                                 "want 5 fields",
		"* * * *":                          "want 5 fields",
		"0 30 2 * * *":                     "want 5 fields",
		"15 30 2 * * * *":                  "want 5 fields",
		"CRON_TZ=Europe/Berlin 30 2 * * *": "use xcron.WithLocation",
		"TZ=UTC 0 * * * *":                 "use xcron.WithLocation",
		"@midnight":                        "unknown descriptor",
		"@every":                           "unknown descriptor",
		"@every 0s":                        "positive duration",
		"@every -1s":                       "positive duration",
		"@every soon":                      "bad @every duration",
		"61 * * * *":                       "minute field",
		"0 0 30 2 *":                       "never fires",
	}
	for spec, want := range cases {
		if _, err := parse(spec); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want error containing %q, got %v", spec, want, err)
		}
	}
}

func TestEvery_CountsFromGivenTimeNotAligned(t *testing.T) {
	s, err := parse("@every 1m")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 1, 1, 10, 0, 17, 500e6, time.UTC)
	if got := s.Next(from); !got.Equal(from.Add(time.Minute)) {
		t.Errorf("@every does not align to a whole minute and does not truncate milliseconds: got=%v", got)
	}
}

func TestNext_UsesLocationAndNeverRepeatsAfterClockStepsBack(t *testing.T) {
	berlin := mustLoad(t, "Europe/Berlin")
	s, _ := parse("30 2 * * *")
	got := next(s, berlin, time.Time{})
	if got.Location() != berlin || got.Hour() != 2 || got.Minute() != 30 {
		t.Errorf("it must be 02:30 Berlin time: got=%v", got)
	}
	// prev is after "now" (the clock was turned back): compute forward from prev rather than computing prev again.
	prev := time.Now().Add(48 * time.Hour).Truncate(time.Minute)
	m, _ := parse("* * * * *")
	if got := next(m, time.UTC, prev); !got.After(prev) {
		t.Errorf("the next time point must be after prev: prev=%v got=%v", prev, got)
	}
}
