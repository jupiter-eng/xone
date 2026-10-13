// Package store 是 e2e 服务的数据访问：PG 上的用户表、订单表，MySQL 上的用户表（mysql.go），
// ClickHouse 上的事件表（clickhouse.go），以及启动时建表。
//
// 建表放在一个启动钩子里，不写档位就落在 StageBusiness：那时 xgorm 已经就绪，
// 服务还没开始接流量。放到「第一次访问时再建」的话，并发的头几个请求要么互相等、
// 要么各建一遍；而钩子失败会让进程直接起不来——建不了表本来就该起不来。
//
// 单独一个包而不是写在 main 里：登记钩子的包不能 import 根包（scripts/check.sh 第 6 步），
// 而 main 要调 xone.MustRun。
package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/jupiter-eng/xone/e2e/service/conf"
	"github.com/jupiter-eng/xone/xgorm"
	"github.com/jupiter-eng/xone/xhook"
)

func init() {
	xhook.BeforeStart(migrate)
}

// ErrNotFound 没有这条记录
var ErrNotFound = errors.New("record not found")

// User 用户表的一行
type User struct {
	ID    int64  `json:"id" gorm:"column:id;primaryKey"`
	Name  string `json:"name" gorm:"column:name"`
	Email string `json:"email" gorm:"column:email"`
}

func users() string  { return conf.C().Table }
func orders() string { return conf.C().Table + "_orders" }

// migrate 建两张表。表名已经过 conf.Validate，只含小写字母、数字、下划线
func migrate(ctx context.Context) error {
	db := xgorm.CWithCtx(ctx)
	// 分两条执行：pgx 走扩展协议，一条语句里放不下两个命令
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS ` + users() + ` (
		id         BIGSERIAL PRIMARY KEY,
		name       TEXT NOT NULL,
		email      TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`).Error; err != nil {
		return fmt.Errorf("create table %s: %w", users(), err)
	}
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS ` + orders() + ` (
		id         TEXT PRIMARY KEY,
		user_id    BIGINT NOT NULL,
		amount     BIGINT NOT NULL,
		status     TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`).Error; err != nil {
		return fmt.Errorf("create table %s: %w", orders(), err)
	}
	// MySQL 实例是可选的：换一份没配它的配置文件时（configWithout 之类）照样起得来
	if xgorm.Has(MySQL) {
		if err := migrateMySQL(ctx); err != nil {
			return err
		}
	}
	// ClickHouse 实例只在激活了 ch 那份 profile 时才有，见 clickhouse.go
	if HasCH() {
		return migrateCH(ctx)
	}
	return nil
}

// CreateUser 插入一个用户，回填 ID
func CreateUser(ctx context.Context, u *User) error {
	return xgorm.CWithCtx(ctx).Table(users()).Create(u).Error
}

// GetUser 按 ID 取用户，没有时返回 ErrNotFound
func GetUser(ctx context.Context, id int64) (User, error) {
	var u User
	err := xgorm.CWithCtx(ctx).Table(users()).Where("id = ?", id).Take(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return u, ErrNotFound
	}
	return u, err
}

// UpdateUser 改名字和邮箱，没有这个用户时返回 ErrNotFound
func UpdateUser(ctx context.Context, u User) error {
	res := xgorm.CWithCtx(ctx).Table(users()).Where("id = ?", u.ID).
		Updates(map[string]any{"name": u.Name, "email": u.Email})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// InsertOrder 建一张状态为 created 的订单
func InsertOrder(ctx context.Context, id string, userID, amount int64) error {
	return xgorm.CWithCtx(ctx).Exec(
		`INSERT INTO `+orders()+` (id, user_id, amount, status) VALUES (?, ?, ?, 'created')`,
		id, userID, amount).Error
}

// SetOrderStatus 改订单状态
func SetOrderStatus(ctx context.Context, id, status string) error {
	return xgorm.CWithCtx(ctx).Exec(`UPDATE `+orders()+` SET status = ? WHERE id = ?`, status, id).Error
}

// DeleteOrder 删掉订单，本来就没有也不算错（回滚要幂等）
func DeleteOrder(ctx context.Context, id string) error {
	return xgorm.CWithCtx(ctx).Exec(`DELETE FROM `+orders()+` WHERE id = ?`, id).Error
}

// Ping 跑一次 SELECT 1
func Ping(ctx context.Context) error {
	return xgorm.CWithCtx(ctx).Exec(`SELECT 1`).Error
}
