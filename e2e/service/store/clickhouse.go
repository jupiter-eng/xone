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

// CH is the name of the ClickHouse instance, matching ch under XGorm.Clients in
// service/application-ch.yml.
//
// This instance is optional: the harness activates that profile with Options.ClickHouse alone, the
// processes of the other cases do not have it, and the CH endpoints answer 503 there.
// The table carries the same name as the user tables on PG / MySQL (Service.Table): the three
// databases are each their own and cannot collide
const CH = "ch"

// Event is one row of the ClickHouse event table
type Event struct {
	ID    uint64 `json:"id" gorm:"column:id"`
	Name  string `json:"name" gorm:"column:name"`
	Value int64  `json:"value" gorm:"column:value"`
}

// Stats is the result of grouping by name
type Stats struct {
	Count uint64 `json:"count" gorm:"column:n"`
	Sum   int64  `json:"sum" gorm:"column:s"`
}

// ch is the ClickHouse instance bound to ctx
func ch(ctx context.Context) *gorm.DB { return xgorm.CWithCtx(ctx, CH) }

// HasCH says whether this process has a ClickHouse instance configured
func HasCH() bool { return xgorm.Has(CH) }

// migrateCH creates the event table on ClickHouse: MergeTree ordered by id, so that a point lookup
// uses the primary index
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

// nextID: ClickHouse has no auto-increment column, so the service hands out the ids -- starting
// from a nanosecond timestamp and going up by one, which never repeats within one process
var nextID atomic.Uint64

func init() { nextID.Store(uint64(time.Now().UnixNano())) }

// InsertEvents writes a batch of events in one INSERT (the same name, a value each) and returns
// the ids it handed out
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

// GetEvent takes one row by id, and returns ErrNotFound when there is none
func GetEvent(ctx context.Context, id uint64) (Event, error) {
	var e Event
	err := ch(ctx).Table(users()).Where("id = ?", id).Take(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return e, ErrNotFound
	}
	return e, err
}

// EventStats groups by name: the number of rows and the sum of value. It goes through GORM's Scan
func EventStats(ctx context.Context, name string) (Stats, error) {
	var s Stats
	err := ch(ctx).Table(users()).Select("count() AS n, sum(value) AS s").Where("name = ?", name).Scan(&s).Error
	return s, err
}

// PingCH runs one SELECT 1 on ClickHouse
func PingCH(ctx context.Context) error {
	return ch(ctx).Exec(`SELECT 1`).Error
}

// SleepCH has the ClickHouse server sleep seconds seconds: SELECT sleep(?), which the server stops at 3 seconds
func SleepCH(ctx context.Context, seconds float64) error {
	var r uint8
	return ch(ctx).Raw(`SELECT sleep(?)`, seconds).Scan(&r).Error
}

// EchoCH has the server sleep seconds seconds and then answer tag as it is.
//
// The sleep sits in a scalar subquery: the server runs it while analyzing the query, before it
// sends the first data block (the header). The client therefore receives not a byte during that
// time, which to a read timeout is the same thing as "the far end says nothing", the difference
// being that the result does arrive in the end -- which is how it tells whether this connection is
// taken for the next query after a read timeout
func EchoCH(ctx context.Context, tag string, seconds float64) (string, error) {
	var got string
	err := ch(ctx).Raw(`SELECT concat(?, '/', toString((SELECT sleep(?))))`, tag, seconds).Scan(&got).Error
	return got, err
}

// StreamCH emits one data block per row with the server sleeping seconds seconds between them, and
// returns how many rows were read.
//
// The header (the first data block) arrives at once and the rows come one by one afterwards: it is
// how it tells whether the read timeout and the caller's deadline still hold once the far end goes
// silent after the header
func StreamCH(ctx context.Context, rows int, seconds float64) (int, error) {
	var ns []uint64
	err := ch(ctx).Raw(`SELECT number FROM numbers(?) WHERE sleepEachRow(?) = 0 SETTINGS max_block_size = 1`, rows, seconds).Scan(&ns).Error
	return len(ns), err
}

// BadCastCH casts value to Int64: the server reports 6 (CANNOT_PARSE_TEXT), whose text is Cannot
// parse string '<value>' as Int64: ...
func BadCastCH(ctx context.Context, value string) error {
	return ch(ctx).Exec(`SELECT toInt64(?)`, value).Error
}

// CHVersion is the version recorded on the Dialector and the two switches set from it (xgorm/clickhouse's probeVersion)
type CHVersion struct {
	Dialector                  string `json:"dialector_version"`
	Server                     string `json:"server_version"`
	DontSupportRenameColumn    bool   `json:"dont_support_rename_column"`
	DontSupportColumnPrecision bool   `json:"dont_support_column_precision"`
}

// VersionCH reads the version information off the Dialector and asks the server for SELECT
// version() once more to compare
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
