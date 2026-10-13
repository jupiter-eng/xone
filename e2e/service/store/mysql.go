package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/jupiter-eng/xone/xgorm"
)

// MySQL 实例的名字，对应 application.yml 里 XGorm.Clients 下的 mysql。
//
// 用户表在 MySQL 上和 PG 上同名（Service.Table）：两个库各是各的，名字撞不上，
// 测试清理时也只用记一个名字
const MySQL = "mysql"

// my 绑了 ctx 的 MySQL 实例
func my(ctx context.Context) *gorm.DB { return xgorm.CWithCtx(ctx, MySQL) }

// migrateMySQL 在 MySQL 上建用户表。和 PG 那张列一样，类型换成 MySQL 的写法
func migrateMySQL(ctx context.Context) error {
	if err := my(ctx).Exec(`CREATE TABLE IF NOT EXISTS ` + users() + ` (
		id         BIGINT AUTO_INCREMENT PRIMARY KEY,
		name       VARCHAR(255) NOT NULL,
		email      VARCHAR(255) NOT NULL DEFAULT '',
		created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
	)`).Error; err != nil {
		return fmt.Errorf("create table %s on %s: %w", users(), MySQL, err)
	}
	return nil
}

// CreateMySQLUser 在 MySQL 上插入一个用户，回填 ID
func CreateMySQLUser(ctx context.Context, u *User) error {
	return my(ctx).Table(users()).Create(u).Error
}

// GetMySQLUser 按 ID 取 MySQL 上的用户，没有时返回 ErrNotFound
func GetMySQLUser(ctx context.Context, id int64) (User, error) {
	var u User
	err := my(ctx).Table(users()).Where("id = ?", id).Take(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return u, ErrNotFound
	}
	return u, err
}

// UpdateMySQLUser 改名字和邮箱，没有这个用户时返回 ErrNotFound。
//
// 不能拿 RowsAffected 判断有没有这一行：MySQL 默认报的是「改动了」的行数，
// 改成和原来一样的值时是 0。所以先查一次在不在
func UpdateMySQLUser(ctx context.Context, u User) error {
	if _, err := GetMySQLUser(ctx, u.ID); err != nil {
		return err
	}
	return my(ctx).Table(users()).Where("id = ?", u.ID).
		Updates(map[string]any{"name": u.Name, "email": u.Email}).Error
}

// DeleteMySQLUser 删掉一个用户，没有时返回 ErrNotFound
func DeleteMySQLUser(ctx context.Context, id int64) error {
	res := my(ctx).Table(users()).Where("id = ?", id).Delete(&User{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// PingMySQL 在 MySQL 上跑一次 SELECT 1
func PingMySQL(ctx context.Context) error {
	return my(ctx).Exec(`SELECT 1`).Error
}

// SleepMySQL 让 MySQL 服务端睡 seconds 秒：SELECT SLEEP(?)。
//
// 用来造一条「真的在服务端执行着」的慢查询，测优雅退出时它能不能做完
func SleepMySQL(ctx context.Context, seconds float64) error {
	var r int64
	return my(ctx).Raw(`SELECT SLEEP(?)`, seconds).Scan(&r).Error
}

// BadInsertMySQL 把 value 当 id 插进 BIGINT 列：严格模式下服务端报 1366，
// 原文是 Incorrect integer value: '<value>' for column 'id' at row 1
func BadInsertMySQL(ctx context.Context, value string) error {
	return my(ctx).Exec(`INSERT INTO `+users()+` (id, name) VALUES (?, ?)`, value, "bad-sql").Error
}

// BadCastPG 在 PG 上把 value 转成 bigint：服务端报 22P02，
// 原文是 invalid input syntax for type bigint: "<value>"
func BadCastPG(ctx context.Context, value string) error {
	return xgorm.CWithCtx(ctx).Exec(`SELECT CAST(CAST(? AS text) AS bigint)`, value).Error
}
