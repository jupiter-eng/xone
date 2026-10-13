package xcron

import (
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/cronexpr"
)

// 这一组钉住 cronexpr v1.1.3 的行为：parse / next 和 README「行为与实测」靠的就是这些。
// 升级 cronexpr 之后哪一条红了，对应的注释和文档要跟着改

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
		t.Errorf("传柏林时间该按柏林算：got=%v want=%v", got, want)
	}
	got = e.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if want := time.Date(2026, 1, 1, 2, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("传 UTC 该按 UTC 算：got=%v want=%v", got, want)
	}
}

func TestCronexpr_FieldCounts(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 10, 0, 17, 0, time.UTC)
	// 6 段：多出来的是年，不是秒
	if _, err := cronexpr.Parse("0 30 2 * * *"); err == nil || !strings.Contains(err.Error(), "hour field") {
		t.Errorf("6 段的第 2 段该被当成小时：err=%v", err)
	}
	if got := cronexpr.MustParse("30 2 * * * 2027").Next(t0); got.Year() != 2027 {
		t.Errorf("6 段的最后一段是年：got=%v", got)
	}
	// 7 段：开头多一段秒
	if got := cronexpr.MustParse("15 30 2 * * * *").Next(t0); got.Second() != 15 {
		t.Errorf("7 段的第 1 段是秒：got=%v", got)
	}
	// 8 段：第 7 段之后的悄悄丢掉
	if _, err := cronexpr.Parse("15 30 2 * * * * garbage"); err != nil {
		t.Errorf("8 段时多出来的不报错：err=%v", err)
	}
}

func TestCronexpr_RejectsTimeZonePrefixAndUnknownDescriptors(t *testing.T) {
	for _, spec := range []string{"CRON_TZ=Europe/Berlin 30 2 * * *", "TZ=UTC 0 * * * *", "@midnight", "@reboot", "@every 1m"} {
		if _, err := cronexpr.Parse(spec); err == nil {
			t.Errorf("cronexpr 收下了 %q，parse 的注释和这里一起改", spec)
		}
	}
	for _, d := range descriptors {
		if _, err := cronexpr.Parse(d); err != nil {
			t.Errorf("cronexpr 不认 %s 了：%v", d, err)
		}
	}
}

func TestCronexpr_NeverFiringSpecReturnsZero(t *testing.T) {
	if got := cronexpr.MustParse("0 0 30 2 *").Next(time.Now()); !got.IsZero() {
		t.Errorf("2 月 30 日不存在，Next 该是零值：got=%v", got)
	}
}

func TestCronexpr_NextIsStrictlyAfter(t *testing.T) {
	at := time.Date(2026, 1, 1, 10, 1, 0, 0, time.UTC)
	if got := cronexpr.MustParse("* * * * *").Next(at); !got.Equal(at.Add(time.Minute)) {
		t.Errorf("从一个正好匹配的时刻算，下一个该严格在它之后：got=%v", got)
	}
}

// 夏令时：America/New_York 2026-03-08 02:00 拨快到 03:00，2026-11-01 02:00 拨回 01:00
func TestCronexpr_DSTInNewYork(t *testing.T) {
	ny := mustLoad(t, "America/New_York")
	daily := cronexpr.MustParse("30 2 * * *")

	// 拨快那天 02:30 不存在：那天整天跳过，不挪到 03:30
	got := daily.Next(time.Date(2026, 3, 7, 12, 0, 0, 0, ny))
	if want := time.Date(2026, 3, 9, 2, 30, 0, 0, ny); !got.Equal(want) {
		t.Errorf("拨快那天的 02:30 该跳过：got=%v want=%v", got, want)
	}
	// 拨回那天 02:30 只出现一次（EST）
	got = daily.Next(time.Date(2026, 10, 31, 12, 0, 0, 0, ny))
	if want := time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("拨回那天的 02:30 跑一次：got=%v want=%v", got, want)
	}
	if n := daily.Next(got); n.Day() != 2 {
		t.Errorf("拨回那天的 02:30 不该跑两次：next=%v", n)
	}

	// 拨回那天 01:30 出现两次（EDT 一次、EST 一次），两次都跑
	early := cronexpr.MustParse("30 1 * * *").NextN(time.Date(2026, 10, 31, 12, 0, 0, 0, ny), 2)
	if len(early) != 2 || early[1].Sub(early[0]) != time.Hour {
		t.Errorf("拨回那天的 01:30 该跑两次、相隔一小时：got=%v", early)
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
		t.Errorf("@every 不对齐整分、不截掉毫秒：got=%v", got)
	}
}

func TestNext_UsesLocationAndNeverRepeatsAfterClockStepsBack(t *testing.T) {
	berlin := mustLoad(t, "Europe/Berlin")
	s, _ := parse("30 2 * * *")
	got := next(s, berlin, time.Time{})
	if got.Location() != berlin || got.Hour() != 2 || got.Minute() != 30 {
		t.Errorf("该按柏林时间的 02:30：got=%v", got)
	}
	// prev 在「现在」之后（时钟被往回拨了）：从 prev 往后算，不把 prev 再算一遍
	prev := time.Now().Add(48 * time.Hour).Truncate(time.Minute)
	m, _ := parse("* * * * *")
	if got := next(m, time.UTC, prev); !got.After(prev) {
		t.Errorf("下一个时间点该在 prev 之后：prev=%v got=%v", prev, got)
	}
}
