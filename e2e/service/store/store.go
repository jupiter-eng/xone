// Package store is the e2e service's data access: the user and order tables on PG, the user table
// on MySQL (mysql.go), the event table on ClickHouse (clickhouse.go), and creating them at start.
//
// Creating the tables sits in a start hook, which writes no stage and so lands in StageBusiness:
// by then xgorm is ready and the service has not begun taking traffic. Done as "create it on the
// first access" the first few concurrent requests would either wait for each other or each create
// them once; and a failing hook stops the process from coming up at all -- which is what should
// happen when the tables cannot be created.
//
// A package of its own rather than part of main: a package registering hooks may not import the
// root package (step 6 of scripts/check.sh), while main has to call xone.MustRun.
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

// ErrNotFound: there is no such record
var ErrNotFound = errors.New("record not found")

// User is one row of the user table
type User struct {
	ID    int64  `json:"id" gorm:"column:id;primaryKey"`
	Name  string `json:"name" gorm:"column:name"`
	Email string `json:"email" gorm:"column:email"`
}

func users() string  { return conf.C().Table }
func orders() string { return conf.C().Table + "_orders" }

// migrate creates the two tables. The table name has been through conf.Validate and holds
// lower-case letters, digits and underscores alone
func migrate(ctx context.Context) error {
	db := xgorm.CWithCtx(ctx)
	// Two separate statements: pgx uses the extended protocol, which takes one command per statement
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
	// The MySQL instance is optional: with a configuration file that leaves it out (configWithout and the like) it still comes up
	if xgorm.Has(MySQL) {
		if err := migrateMySQL(ctx); err != nil {
			return err
		}
	}
	// The ClickHouse instance exists only with the ch profile activated, see clickhouse.go
	if HasCH() {
		return migrateCH(ctx)
	}
	return nil
}

// CreateUser inserts one user and fills the ID back in
func CreateUser(ctx context.Context, u *User) error {
	return xgorm.CWithCtx(ctx).Table(users()).Create(u).Error
}

// GetUser takes a user by ID, and returns ErrNotFound when there is none
func GetUser(ctx context.Context, id int64) (User, error) {
	var u User
	err := xgorm.CWithCtx(ctx).Table(users()).Where("id = ?", id).Take(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return u, ErrNotFound
	}
	return u, err
}

// UpdateUser updates the name and the email, and returns ErrNotFound when there is no such user
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

// InsertOrder creates an order in the status created
func InsertOrder(ctx context.Context, id string, userID, amount int64) error {
	return xgorm.CWithCtx(ctx).Exec(
		`INSERT INTO `+orders()+` (id, user_id, amount, status) VALUES (?, ?, ?, 'created')`,
		id, userID, amount).Error
}

// SetOrderStatus changes an order's status
func SetOrderStatus(ctx context.Context, id, status string) error {
	return xgorm.CWithCtx(ctx).Exec(`UPDATE `+orders()+` SET status = ? WHERE id = ?`, status, id).Error
}

// DeleteOrder deletes an order, and one that was not there is no error either (a rollback has to be idempotent)
func DeleteOrder(ctx context.Context, id string) error {
	return xgorm.CWithCtx(ctx).Exec(`DELETE FROM `+orders()+` WHERE id = ?`, id).Error
}

// Ping runs one SELECT 1
func Ping(ctx context.Context) error {
	return xgorm.CWithCtx(ctx).Exec(`SELECT 1`).Error
}
