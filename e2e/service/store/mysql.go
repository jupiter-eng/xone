package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/jupiter-eng/xone/xgorm"
)

// MySQL is the name of the MySQL instance, matching mysql under XGorm.Clients in application.yml.
//
// The user table carries the same name on MySQL as on PG (Service.Table): the two databases are
// each their own, the names cannot collide, and a test cleaning up has one name to remember
const MySQL = "mysql"

// my is the MySQL instance bound to ctx
func my(ctx context.Context) *gorm.DB { return xgorm.CWithCtx(ctx, MySQL) }

// migrateMySQL creates the user table on MySQL. The same columns as the PG one, with the types
// written MySQL's way
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

// CreateMySQLUser inserts one user on MySQL and fills the ID back in
func CreateMySQLUser(ctx context.Context, u *User) error {
	return my(ctx).Table(users()).Create(u).Error
}

// GetMySQLUser takes a user on MySQL by ID, and returns ErrNotFound when there is none
func GetMySQLUser(ctx context.Context, id int64) (User, error) {
	var u User
	err := my(ctx).Table(users()).Where("id = ?", id).Take(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return u, ErrNotFound
	}
	return u, err
}

// UpdateMySQLUser updates the name and the email, and returns ErrNotFound when there is no such
// user.
//
// RowsAffected cannot tell whether the row is there: MySQL reports the number of rows it "changed"
// by default, which is 0 when the new values equal the old ones. So it looks the row up first
func UpdateMySQLUser(ctx context.Context, u User) error {
	if _, err := GetMySQLUser(ctx, u.ID); err != nil {
		return err
	}
	return my(ctx).Table(users()).Where("id = ?", u.ID).
		Updates(map[string]any{"name": u.Name, "email": u.Email}).Error
}

// DeleteMySQLUser deletes one user, and returns ErrNotFound when there is none
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

// PingMySQL runs one SELECT 1 on MySQL
func PingMySQL(ctx context.Context) error {
	return my(ctx).Exec(`SELECT 1`).Error
}

// SleepMySQL has the MySQL server sleep seconds seconds: SELECT SLEEP(?).
//
// It makes a slow query that really is running on the server, to test whether it finishes during a
// graceful exit
func SleepMySQL(ctx context.Context, seconds float64) error {
	var r int64
	return my(ctx).Raw(`SELECT SLEEP(?)`, seconds).Scan(&r).Error
}

// BadInsertMySQL inserts value as the id into a BIGINT column: in strict mode the server reports
// 1366, whose text is Incorrect integer value: '<value>' for column 'id' at row 1
func BadInsertMySQL(ctx context.Context, value string) error {
	return my(ctx).Exec(`INSERT INTO `+users()+` (id, name) VALUES (?, ?)`, value, "bad-sql").Error
}

// BadCastPG casts value to bigint on PG: the server reports 22P02, whose text is invalid input
// syntax for type bigint: "<value>"
func BadCastPG(ctx context.Context, value string) error {
	return xgorm.CWithCtx(ctx).Exec(`SELECT CAST(CAST(? AS text) AS bigint)`, value).Error
}
