package store

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "github.com/lib/pq"
)

// Open 连接 PostgreSQL，服务重启场景下重试等待数据库就绪。
func Open(dsn string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)

	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		lastErr = db.Ping()
		if lastErr == nil {
			return db, nil
		}
		time.Sleep(time.Second)
	}
	return nil, fmt.Errorf("数据库不可达: %w", lastErr)
}

// ApplySQLFile 在一个事务里执行 schema / seed 文件。
func ApplySQLFile(db *sql.DB, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(string(b)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("执行 %s 失败: %w", path, err)
	}
	return tx.Commit()
}
