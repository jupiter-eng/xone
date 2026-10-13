package xcron

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/cronexpr"
)

// schedule 给一个时刻，算出它之后的下一个时间点。零值表示再也没有了
type schedule interface {
	Next(time.Time) time.Time
}

// descriptors 收的 @ 写法，就是 cronexpr v1.1.3 的 cronNormalizer 认的那几个，
// 外加本包自己实现的 @every。没有 @midnight、@reboot：cronexpr 不认，报的是 missing field(s)
var descriptors = []string{"@yearly", "@annually", "@monthly", "@weekly", "@daily", "@hourly"}

// parse 把 spec 解析成 schedule。cronexpr 只用来解析和算下一个时间点，调度循环是本包自己的。
//
// 收标准的 5 段（分 时 日 月 周）、上面那几个 @ 写法和 @every <时长>。
// 下面几条是 cronexpr v1.1.3 实测出来、在这里先拦下的，它自己的报错要么没有、要么说不清：
//
//   - 段数：它收 5 到 7 段，6 段时多出来的那段是年（"0 30 2 * * *" 报 syntax error in hour field: '30'，
//     而不是当成秒），7 段时开头多一段秒，8 段及以上把第 7 段之后的悄悄丢掉。
//     这里只收 5 段，其余说清楚要几段
//   - CRON_TZ= / TZ= 前缀：不认，报 syntax error in minute field: 'CRON_TZ=Europe/Berlin'，
//     看不出该怎么改。时区在 WithLocation 里写
//   - 不认识的 @ 写法（@midnight、@reboot、@every）一律报 missing field(s)
//   - 永远不会到的时间点（0 0 30 2 *，2 月 30 日）：解析成功，Next 返回零值，任务一次都不跑
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

// every @every <d>：从传进来的时刻往后数 d，不对齐整点、整分。
// cronexpr 没有这个写法，本包自己实现
type every time.Duration

func (d every) Next(t time.Time) time.Time { return t.Add(time.Duration(d)) }

// next 下一个时间点，按 loc 解释 spec。
//
// cronexpr 按传进来的 t 的时区算（v1.1.3 实测：同一个 "30 2 * * *"，传柏林时间得到柏林的 02:30，
// 传 UTC 得到 UTC 的 02:30），所以把 from 换到 loc 再交给 Next 就是「按 loc 解释」。
//
// from 不早于 prev：墙上时钟被往回拨（NTP 校时）时，按 time.Now() 算会把刚跑过的那个时间点
// 再算一遍、再跑一次。Next 严格大于它的参数（实测从 10:01:00 整算 "* * * * *" 得到 10:02:00），
// 从 prev 算就不会重复
func next(s schedule, loc *time.Location, prev time.Time) time.Time {
	from := time.Now()
	if from.Before(prev) {
		from = prev
	}
	return s.Next(from.In(loc))
}
