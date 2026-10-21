package xcron

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/cronexpr"
)

// schedule takes a moment and works out the next time point after it. The zero value means there is no next one.
type schedule interface {
	Next(time.Time) time.Time
}

// descriptors are the @ forms accepted: exactly the ones cronexpr v1.1.3's cronNormalizer recognizes, plus this
// package's own @every. There is no @midnight or @reboot: cronexpr does not know them and reports missing field(s).
var descriptors = []string{"@yearly", "@annually", "@monthly", "@weekly", "@daily", "@hourly"}

// parse turns a spec into a schedule. cronexpr is used only for parsing and for computing the next time point; the scheduling loop is this package's own.
//
// It accepts the standard 5 fields (minute hour day month weekday), the @ forms above and @every <duration>.
// The points below were measured on cronexpr v1.1.3 and are caught here first, because its own error is either absent or unclear:
//
//   - Field count: it accepts 5 to 7 fields; with 6 the extra one is the year ("0 30 2 * * *" reports
//     syntax error in hour field: '30' rather than taking it as seconds), with 7 the extra leading one is seconds,
//     and with 8 or more everything after the 7th is dropped silently. Only 5 are accepted here, and anything else says plainly how many are needed.
//   - A CRON_TZ= / TZ= prefix: not recognized, reported as syntax error in minute field:
//     'CRON_TZ=Europe/Berlin', which gives no hint what to change. The time zone is written in WithLocation.
//   - An unrecognized @ form (@midnight, @reboot, @every) is always reported as missing field(s).
//   - A time point that never arrives (0 0 30 2 *, 30 February): it parses, Next returns the zero value, and the job never runs.
func parse(spec string) (schedule, error) {
	spec = strings.TrimSpace(spec)
	if strings.HasPrefix(spec, "CRON_TZ=") || strings.HasPrefix(spec, "TZ=") {
		return nil, errors.New("time zone prefix is not supported in spec, use xcron.WithLocation instead")
	}
	if rest, ok := strings.CutPrefix(spec, "@every "); ok {
		d, err := time.ParseDuration(strings.TrimSpace(rest))
		if err != nil {
			return nil, fmt.Errorf("bad @every duration: %w", err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("@every needs a positive duration, got %v", d)
		}
		return every(d), nil
	}
	if strings.HasPrefix(spec, "@") {
		if !slices.Contains(descriptors, spec) {
			return nil, fmt.Errorf("unknown descriptor, want one of %v or @every <duration>", descriptors)
		}
	} else if n := len(strings.Fields(spec)); n != 5 {
		return nil, fmt.Errorf("want 5 fields (minute hour day-of-month month day-of-week), got %d", n)
	}
	e, err := cronexpr.Parse(spec)
	if err != nil {
		return nil, err
	}
	if e.Next(time.Now()).IsZero() {
		return nil, errors.New("spec never fires")
	}
	return e, nil
}

// every implements @every <d>: count d forward from the moment passed in, without aligning to a whole hour or minute.
// cronexpr has no such form, so this package implements it.
type every time.Duration

func (d every) Next(t time.Time) time.Time { return t.Add(time.Duration(d)) }

// next is the next time point, with the spec interpreted in loc.
//
// cronexpr computes in the time zone of the t passed in (measured on v1.1.3: the same "30 2 * * *" gives 02:30
// Berlin for a Berlin time and 02:30 UTC for a UTC time), so converting from into loc before handing it to Next is what "interpret in loc" means.
//
// from is never earlier than prev: when the wall clock is turned back (an NTP correction), computing from time.Now()
// would compute the time point that has just run again and run it again. Next is strictly greater than its argument
// (measured: computing "* * * * *" from exactly 10:01:00 gives 10:02:00), so computing from prev never repeats.
func next(s schedule, loc *time.Location, prev time.Time) time.Time {
	from := time.Now()
	if from.Before(prev) {
		from = prev
	}
	return s.Next(from.In(loc))
}
