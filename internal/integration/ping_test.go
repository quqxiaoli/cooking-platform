package integration_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
)

// TestMySQLPing 验证 CI services 中的 MySQL 可达。
// 本地未设置 TEST_MYSQL_DSN 时自动跳过，不影响纯单元测试运行。
func TestMySQLPing(t *testing.T) {
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN not set — skipping MySQL integration test")
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		t.Fatalf("db.Ping: %v", err)
	}
}

// TestRedisPing 验证 CI services 中的 Redis 可达。
func TestRedisPing(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set — skipping Redis integration test")
	}

	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis Ping: %v", err)
	}
}
