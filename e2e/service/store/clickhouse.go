package store

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	chdialect "gorm.io/driver/clickhouse"
	"gorm.io/gorm"

	"github.com/jupiter-eng/xone/xgorm"
)

// CH ClickHouse 实例的名字，对应 service/application-ch.yml 里 XGorm.Clients 下的 ch。
//
// 这个实例是可选的：harness 只在 Options.ClickHouse 时激活那份 profile，
// 其余用例的进程里没有它，CH 那一组接口回 503。
// 表名和 PG / MySQL 上的用户表同名（Service.Table）：三个库各是各的，撞不上
const CH = "ch"

// Event ClickHouse 事件表的一行
type Event struct {
	ID    uint64 `json:"id" gorm:"column:id"`
	Name  string `json:"name" gorm:"column:name"`
	Value int64  `json:"value" gorm:"column:value"`
}

// Stats 按 name 聚合出来的结果
type Stats struct {
	Count uint64 `json:"count" gorm:"column:n"`
	Sum   int64  `json:"sum" gorm:"column:s"`
}

// ch 绑了 ctx 的 ClickHouse 实例
func ch(ctx context.Context) *gorm.DB { return xgorm.CWithCtx(ctx, CH) }

// HasCH 这个进程配没配 ClickHouse 实例
func HasCH() bool { return xgorm.Has(CH) }

// migrateCH 在 ClickHouse 上建事件表：MergeTree，按 id 排序，点查走主键索引
func migrateCH(ctx context.Context) error {
	if err := ch(ctx).Exec(`CREATE TABLE IF NOT EXISTS ` + users() + ` (
		id         UInt64,
		name       String,
		value      Int64,
		created_at DateTime64(3) DEFAULT now64(3)
	) ENGINE = MergeTree ORDER BY id`).Error; err != nil {
		return fmt.Errorf("create table %s on %s: %w", users(), CH, err)
	}
	return nil
}

// nextID ClickHouse 没有自增列，id 由服务发：纳秒时间戳起步、逐个加一，同一进程里不重复
var nextID atomic.Uint64

func init() { nextID.Store(uint64(time.Now().UnixNano())) }

// InsertEvents 以一条 INSERT 写进一批事件（同一个 name，各自的 value），返回发出去的 id
func InsertEvents(ctx context.Context, name string, values []int64) ([]uint64, error) {
	rows := make([]Event, len(values))
	ids := make([]uint64, len(values))
	for i, v := range values {
		ids[i] = nextID.Add(1)
		rows[i] = Event{ID: ids[i], Name: name, Value: v}
	}
	if err := ch(ctx).Table(users()).Create(&rows).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

// GetEvent 按 id 取一行，没有时返回 ErrNotFound
func GetEvent(ctx context.Context, id uint64) (Event, error) {
	var e Event
	err := ch(ctx).Table(users()).Where("id = ?", id).Take(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return e, ErrNotFound
	}
	return e, err
}

// EventStats 按 name 聚合：条数与 value 之和。走 GORM 的 Scan
func EventStats(ctx context.Context, name string) (Stats, error) {
	var s Stats
	err := ch(ctx).Table(users()).Select("count() AS n, sum(value) AS s").Where("name = ?", name).Scan(&s).Error
	return s, err
}

// PingCH 在 ClickHouse 上跑一次 SELECT 1
func PingCH(ctx context.Context) error {
	return ch(ctx).Exec(`SELECT 1`).Error
}

// SleepCH 让 ClickHouse 服务端睡 seconds 秒：SELECT sleep(?)，服务端上限 3 秒
func SleepCH(ctx context.Context, seconds float64) error {
	var r uint8
	return ch(ctx).Raw(`SELECT sleep(?)`, seconds).Scan(&r).Error
}

// EchoCH 服务端先睡 seconds 秒再把 tag 原样回来。
//
// 睡在标量子查询里：它在服务端分析查询时就执行，发出第一个数据块（表头）之前。
// 于是客户端在这段时间里一个字节都收不到，和「对端不回话」在读超时看来是一回事，
// 区别是结果最后还是会到——用来看读超时之后这条连接还会不会被拿去跑下一条查询
func EchoCH(ctx context.Context, tag string, seconds float64) (string, error) {
	var got string
	err := ch(ctx).Raw(`SELECT concat(?, '/', toString((SELECT sleep(?))))`, tag, seconds).Scan(&got).Error
	return got, err
}

// StreamCH 一行一个数据块、每行之间服务端睡 seconds 秒，返回读到了几行。
//
// 表头（第一个数据块）立刻就到，之后的行一行一行地来：用来看表头之后对端不回话时，
// 读超时和调用方的截止时间还管不管
func StreamCH(ctx context.Context, rows int, seconds float64) (int, error) {
	var ns []uint64
	err := ch(ctx).Raw(`SELECT number FROM numbers(?) WHERE sleepEachRow(?) = 0 SETTINGS max_block_size = 1`, rows, seconds).Scan(&ns).Error
	return len(ns), err
}

// BadCastCH 把 value 转成 Int64：服务端报 6（CANNOT_PARSE_TEXT），
// 原文是 Cannot parse string '<value>' as Int64: ...
func BadCastCH(ctx context.Context, value string) error {
	return ch(ctx).Exec(`SELECT toInt64(?)`, value).Error
}

// CHVersion Dialector 上记下的版本号和两个按版本设的开关（xgorm/clickhouse 的 probeVersion）
type CHVersion struct {
	Dialector                  string `json:"dialector_version"`
	Server                     string `json:"server_version"`
	DontSupportRenameColumn    bool   `json:"dont_support_rename_column"`
	DontSupportColumnPrecision bool   `json:"dont_support_column_precision"`
}

// VersionCH 读 Dialector 上的版本信息，再问一次服务端 SELECT version() 对照
func VersionCH(ctx context.Context) (CHVersion, error) {
	db := ch(ctx)
	d, ok := db.Dialector.(*chdialect.Dialector)
	if !ok {
		return CHVersion{}, fmt.Errorf("instance %s is not a ClickHouse dialector: %T", CH, db.Dialector)
	}
	v := CHVersion{
		Dialector:                  d.Version,
		DontSupportRenameColumn:    d.DontSupportRenameColumn,
		DontSupportColumnPrecision: d.DontSupportColumnPrecision,
	}
	err := db.Raw(`SELECT version()`).Scan(&v.Server).Error
	return v, err
}
